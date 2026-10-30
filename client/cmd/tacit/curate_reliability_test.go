package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/curate"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/search"
	"github.com/agent-sh/tacit/client/internal/store"
)

func TestJudgeFailureDoesNotApplyEarlierChunks(t *testing.T) {
	dir := t.TempDir()
	oldFact := curatedFact(t, dir, "old-quokka", "quokka scheduler runs on port 7781 behind the proxy", "2026-10-01", "note")
	replacement := curatedFact(t, dir, "new-quokka", "quokka scheduler runs on port 7790 behind the proxy", "2026-10-14", "note")
	before, _ := os.ReadFile(filepath.Join(dir, oldFact.Path))
	var pairs []curate.Pair
	var refs []judgedPair
	for i := 0; i < 26; i++ {
		pairs = append(pairs, curate.Pair{})
		refs = append(refs, judgedPair{newIdx: 0, existing: &search.Hit{Name: oldFact.Meta.Name, Path: oldFact.Path, Description: oldFact.Meta.Description}})
	}
	previous := judgeRun
	defer func() { judgeRun = previous }()
	calls := 0
	judgeRun = func(context.Context, string, string) (string, float64, error) {
		calls++
		if calls == 2 {
			// A model or transport error stops the check. (An invalid answer is
			// split and asked again: TestJudgeInvalidChunkIsSplitAndAskedAgain.)
			return "", .1, errors.New("judge unavailable")
		}
		verdicts := make([]string, 25)
		for i := range verdicts {
			verdicts[i] = "supersedes"
		}
		b, _ := json.Marshal(verdicts)
		return string(b), .1, nil
	}
	report := curate.Report{}
	review, written, err := judgePairs(context.Background(), &store.Store{Dir: dir}, pairs, refs, []*fact.Fact{replacement}, false, &report)
	after, _ := os.ReadFile(filepath.Join(dir, oldFact.Path))
	if err == nil || len(review) != 0 || len(written) != 0 || string(after) != string(before) || report.Supersedes != 0 || report.CostUSD != .2 {
		t.Fatalf("partial judgment applied: %v %v %v %+v", review, written, err, report)
	}
}

func TestCuratorFreshInstallAndMalformedRetry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell CLI fixture")
	}
	for _, answer := range []string{"malformed", "[]", `[{"origin":"n1","name":"socket-fact","description":"The quokka socket driver uses port 7781","type":"machine_fact","scope":"system","kind":"observed","body":"The socket driver uses port 7781."}]`} {
		t.Run(answer[:min(len(answer), 12)], func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
			t.Setenv("TACIT_STORE", filepath.Join(home, "store"))
			t.Setenv("TACIT_INDEX", filepath.Join(home, "index"))
			t.Setenv("TACIT_LOCAL", filepath.Join(home, "overlay"))
			t.Setenv("TACIT_PROJECTS", filepath.Join(home, "projects"))
			t.Setenv("TACIT_CURATE_RUNNER", "claude")
			stubScrump(t)
			if code := runInit(nil); code != 0 {
				t.Fatalf("init %d", code)
			}
			s := &store.Store{Dir: filepath.Join(home, "store")}
			if _, err := addFact(context.Background(), s, "muse", fact.Meta{Name: "n1", Description: "The quokka socket driver uses port 7781", Type: "machine_fact", Scope: "system", Source: fact.Source{Kind: "observed"}}, "The socket driver uses port 7781."); err != nil {
				t.Fatal(err)
			}
			response, _ := json.Marshal(map[string]any{"result": answer, "total_cost_usd": .01})
			fake := filepath.Join(home, "fake-cli")
			t.Setenv("TACIT_CLAUDE_COMMAND", fake)
			t.Setenv("FIXTURE_RESPONSE", string(response))
			t.Setenv("FIXTURE_CALLS", filepath.Join(home, "calls"))
			script := "#!/bin/sh\ncat >/dev/null\nprintf 'call\\n' >> \"$FIXTURE_CALLS\"\nprintf '%s\\n' \"$FIXTURE_RESPONSE\"\n"
			if err := os.WriteFile(fake, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			code := runCurate([]string{"--max-batches", "1"})
			if answer == "malformed" {
				if code != 1 {
					t.Fatalf("malformed exit %d", code)
				}
				state := loadCuratorState(filepath.Join(home, ".local/state/tacit/curator.json"))
				if state.Empty["n1"] != "" {
					t.Fatal("malformed marked empty")
				}
				if code := runCurate([]string{"--max-batches", "1"}); code != 1 {
					t.Fatalf("retry exit %d", code)
				}
				calls, _ := os.ReadFile(filepath.Join(home, "calls"))
				if strings.Count(string(calls), "call") != 2 {
					t.Fatal("malformed note was not retried")
				}
			} else {
				if code != 0 {
					t.Fatalf("fresh observation exit %d", code)
				}
				if answer == "[]" {
					state := loadCuratorState(filepath.Join(home, ".local/state/tacit/curator.json"))
					if state.Empty["n1"] == "" {
						t.Fatal("valid empty extraction was not remembered")
					}
				} else {
					all, _, err := fact.Walk(s.Dir)
					if err != nil || len(all) != 2 {
						t.Fatalf("fresh curation failed: %d %v", len(all), err)
					}
				}
			}
		})
	}
}
