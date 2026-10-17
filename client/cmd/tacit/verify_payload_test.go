package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/modelrun"
)

func TestVerifyPayloadBudget(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		body, per, wantCalls int
		oversized            bool
	}{
		{"characters", 250000, 12, 5, false},
		{"count", 10, 2, 3, false},
		{"oversized-single", 500000, 12, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubScrump(t)
			_, clone := reviewFixture(t)
			rs := clone("payload")
			for _, name := range []string{"a", "b", "c", "d"} {
				path := filepath.Join(rs.s.Dir, "system", name+".md")
				text := strings.Replace(factFile(name), "body of "+name, strings.Repeat("x", tc.body), 1)
				if err := os.WriteFile(path, []byte(text), 0600); err != nil {
					t.Fatal(err)
				}
			}
			git(t, rs.s.Dir, "commit", "-qam", "large facts")
			git(t, rs.s.Dir, "push", "-q", "origin", "HEAD:main")
			calls, sent := 0, map[string]bool{}
			call := func(_ context.Context, _, prompt, _ string, _ float64) ([]verifyVerdict, string, float64, error) {
				calls++
				if len(prompt) > 900000 {
					t.Fatalf("payload %d exceeds 900000-byte budget", len(prompt))
				}
				for _, it := range rs.items() {
					if strings.Contains(prompt, it.ID) {
						if sent[it.ID] {
							t.Fatalf("duplicate finding %s", it.ID)
						}
						sent[it.ID] = true
					}
				}
				return nil, "", 0, nil
			}
			r := verifyRun{model: "m", max: 60, per: tc.per, budget: 1, dry: !tc.oversized, cwd: t.TempDir(), stateDir: t.TempDir()}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			_, _, err := r.run(ctx, rs.s, call)
			if (err != nil) != tc.oversized {
				t.Fatalf("oversized=%v err=%v", tc.oversized, err)
			}
			if calls != tc.wantCalls {
				t.Fatalf("calls=%d want=%d", calls, tc.wantCalls)
			}
			if !tc.oversized && len(sent) != 5 {
				t.Fatalf("sent %d of 5 findings", len(sent))
			}
			if len(r.attempts()) != 0 {
				t.Fatalf("attempts burned: %v", r.attempts())
			}
		})
	}
}

func TestVerifyStalePayloadBudget(t *testing.T) {
	fx := newStaleFixture(t)
	fx.change(t, "2026-10-06")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := staleStep(ctx, fx.rs.s, fx.opts()); err != nil {
		t.Fatal(err)
	}
	fx.rs.loadFacts()
	// Keep the existing stale flag and owner guards, but make its fact too
	// large for a singleton. No model call or retry may be charged.
	fx.rs.facts["engine-retries"].Body = strings.Repeat("x", 900000)
	r := defaultRun(t)
	r.tl = fx.b
	r.attemptsBefore = 3
	tries := map[string]int{}
	calls := 0
	_, _, err := r.runStale(ctx, fx.rs.s, fx.rs, func(context.Context, string, string, string, float64) ([]verifyVerdict, string, float64, error) {
		calls++
		return nil, "", 0, nil
	}, 60, fx.rs.ownerTouched(), tries)
	if err == nil || calls != 0 || len(tries) != 0 {
		t.Fatalf("calls=%d tries=%v err=%v", calls, tries, err)
	}
}

func TestVerifyBatchExactBoundary(t *testing.T) {
	// JSON escaping and multibyte text must be counted after serialization,
	// including the template and the newline joining findings.
	text, _ := json.Marshal(strings.Repeat("\"שלום\\", 200))
	prefix := 900000 - 2*len(text) - 1
	end, lines, err := nextVerifyBatch(0, 3, 12, prefix, func(int) string { return string(text) })
	if err != nil || end != 2 || len(lines) != 2 || prefix+len(strings.Join(lines, "\n")) != 900000 {
		t.Fatalf("end=%d lines=%d err=%v", end, len(lines), err)
	}
}

func TestPortableVerifierPayloadBudget(t *testing.T) {
	stubScrump(t)
	root := t.TempDir()
	t.Setenv("TACIT_PROJECTS", root)
	for i := 1; i <= 20; i++ {
		source := filepath.Join(root, fmt.Sprintf("round-%02d.txt", i))
		if err := os.WriteFile(source, []byte(strings.Repeat("x", 64000)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	calls, omitted := 0, false
	call := func(_ context.Context, prompt string) (modelrun.Result, error) {
		calls++
		if len(prompt) > 1000000 {
			t.Fatalf("turn %d payload %d exceeds 1000000-byte request budget", calls, len(prompt))
		}
		if !strings.HasPrefix(prompt, "Original findings") {
			t.Fatal("original findings lost")
		}
		omitted = omitted || strings.Contains(prompt, "Earlier evidence omitted")
		if calls <= 20 {
			source := filepath.Join(root, fmt.Sprintf("round-%02d.txt", calls))
			b, _ := json.Marshal(map[string]any{"probes": [][]string{{"read", source}}})
			return modelrun.Result{Text: string(b)}, nil
		}
		if strings.Contains(prompt, "round-01.txt") || !strings.Contains(prompt, "round-20.txt") {
			t.Fatal("did not evict oldest evidence and preserve newest round")
		}
		return modelrun.Result{Text: `{"verdicts":[]}`}, nil
	}
	_, _, _, err := portableVerifier(context.Background(), "m", "Original findings", 4, call)
	if err != nil || calls != 21 || !omitted {
		t.Fatalf("calls=%d omitted=%v err=%v", calls, omitted, err)
	}
}

func TestPortableVerifierRejectsOversizedInput(t *testing.T) {
	stubScrump(t)
	root := t.TempDir()
	t.Setenv("TACIT_PROJECTS", root)
	source := filepath.Join(root, "large.txt")
	if err := os.WriteFile(source, []byte(strings.Repeat("x", 64000)), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, prompt string
		wantCalls    int
	}{
		{"initial", strings.Repeat("x", 1000000), 0},
		{"evidence-round", "original findings", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			_, _, _, err := portableVerifier(context.Background(), "m", tc.prompt, 4, func(context.Context, string) (modelrun.Result, error) {
				calls++
				probes := make([][]string, 17)
				for i := range probes {
					probes[i] = []string{"read", source}
				}
				b, _ := json.Marshal(map[string]any{"probes": probes})
				return modelrun.Result{Text: string(b)}, nil
			})
			if err == nil || calls != tc.wantCalls {
				t.Fatalf("calls=%d want=%d err=%v", calls, tc.wantCalls, err)
			}
		})
	}
}
