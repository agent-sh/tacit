package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A finding that keeps failing stops being sent after three attempts. The
// run that stops it names it, says what it is about and why its last attempt
// failed, and `tacit verify --requeue` sends it again. v0.9.14 said only how
// many there were.
func TestVerifyNamesStrandedFindingsAndRequeuesThem(t *testing.T) {
	stubScrump(t)
	_, clone := reviewFixture(t)
	rs := clone("one")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// a is already retired in favour of c, so "keep b" on "b may replace a"
	// cannot land: a has another successor.
	p := filepath.Join(rs.s.Dir, "system", "a.md")
	b, _ := os.ReadFile(p)
	os.WriteFile(p, []byte(strings.Replace(string(b), "created:", "superseded_by: c\ncreated:", 1)), 0o644)
	git(t, rs.s.Dir, "commit", "-q", "-am", "a retired by c")
	git(t, rs.s.Dir, "push", "-q", "origin", "HEAD:main")
	ab, dc, bc := itemFor(t, rs, "a", "b"), itemFor(t, rs, "d", "c"), itemFor(t, rs, "b", "c")
	// A verdict that keeps a name that is neither fact; the name is shaped
	// like a token, built here, and must not reach the failure record.
	token := "ghp_" + strings.Repeat("Q", 36)
	sent := map[string]int{}
	call := func(_ context.Context, _, prompt, _ string, _ float64) ([]verifyVerdict, string, float64, error) {
		var vs []verifyVerdict
		for _, it := range rs.items() {
			if !strings.Contains(prompt, it.ID) {
				continue
			}
			sent[it.ID]++
			// a/b and b/c get a verdict; the others, d/c among them, none.
			switch it.ID {
			case ab.ID:
				vs = append(vs, verifyVerdict{ID: it.ID, Mark: "keep", Keep: "b", Evidence: "b is current"})
			case bc.ID:
				vs = append(vs, verifyVerdict{ID: it.ID, Mark: "keep", Keep: token, Evidence: "x"})
			}
		}
		return vs, "raw", 0.1, nil
	}
	state := t.TempDir()
	t.Setenv("TACIT_STATE_DIR", state)
	r := verifyRun{model: "m", max: 50, per: 50, budget: 1, cwd: t.TempDir(), stateDir: filepath.Join(state, "verify")}
	var last string
	for i := 0; i < 4; i++ {
		last = captureStderr(t, func() { r.run(ctx, rs.s, call) })
	}
	if sent[ab.ID] != 3 || sent[dc.ID] != 3 {
		t.Fatalf("sent %v, want a/b and d/c 3 times each", sent)
	}
	for _, want := range []string{
		"verify: 5 finding(s) reached 3 failed attempts and are no longer sent",
		"verify: stranded " + ab.ID + ", supersede-proposed a / b: keep b: that mark is not possible for this finding now (",
		"verify: stranded " + dc.ID + ", contradiction d / c: no verdict came back for it (",
		"verify: stranded " + bc.ID + ", supersede-proposed b / c: the verdict keeps a fact that is neither of the two (",
	} {
		if !strings.Contains(last, want) {
			t.Fatalf("the run does not say %q:\n%s", want, last)
		}
	}
	rec, err := os.ReadFile(filepath.Join(state, "verify", "failures.json"))
	if err != nil || strings.Contains(string(rec), token) || strings.Contains(last, token) {
		t.Fatalf("a verdict's text reached the failure record: %v\n%s", err, rec)
	}
	if st, err := os.Stat(filepath.Join(state, "verify", "failures.json")); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("failures.json mode: %v %v", st.Mode(), err)
	}
	// A run holds the counters in memory: a requeue while one runs is
	// refused, not undone by the run's next save.
	held, err := tryJobLock(state, "verifier")
	if err != nil {
		t.Fatal(err)
	}
	if code := runVerify([]string{"--requeue", ab.ID}); code != 1 || r.attempts()[ab.ID] != 3 {
		t.Fatalf("requeue during a run: exit %d, attempts %v", code, r.attempts())
	}
	held()
	// Requeue one by id, then the rest with all: each is sent again once.
	if code := runVerify([]string{"--requeue", ab.ID}); code != 0 {
		t.Fatalf("requeue exit %d", code)
	}
	if n := r.attempts(); n[ab.ID] != 0 || n[dc.ID] != 3 {
		t.Fatalf("attempts after requeueing a/b: %v", n)
	}
	if f := r.failures(); f[ab.ID].Why != "" || f[dc.ID].Why == "" {
		t.Fatalf("failures after requeueing a/b: %v", f)
	}
	r.run(ctx, rs.s, call)
	if sent[ab.ID] != 4 || sent[dc.ID] != 3 {
		t.Fatalf("after requeueing a/b: sent %v", sent)
	}
	if code := runVerify([]string{"--requeue", "all"}); code != 0 {
		t.Fatalf("requeue all exit %d", code)
	}
	r.run(ctx, rs.s, call)
	if sent[dc.ID] != 4 {
		t.Fatalf("after requeue all: sent %v", sent)
	}
}
