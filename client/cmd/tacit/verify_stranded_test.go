package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sandboxVerify points everything runVerify reads at the test: a home, the
// fixture's store, a projects directory that exists (so a real run would get
// as far as a model call) and stand-in model CLIs that record each call and
// fail. It returns the state directory and a check for model calls.
func sandboxVerify(t *testing.T, store string) (state string, called func() bool) {
	t.Helper()
	root := t.TempDir()
	state = filepath.Join(root, "state")
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("TACIT_STATE_DIR", state)
	t.Setenv("TACIT_STORE", store)
	projects := filepath.Join(root, "projects")
	os.MkdirAll(projects, 0o755)
	t.Setenv("TACIT_PROJECTS", projects)
	calls := filepath.Join(root, "calls")
	fake := filepath.Join(root, "fake-cli")
	os.WriteFile(fake, []byte("#!/bin/sh\necho called >> '"+calls+"'\nexit 1\n"), 0o700)
	t.Setenv("TACIT_CLAUDE_COMMAND", fake)
	t.Setenv("TACIT_CODEX_COMMAND", fake)
	return state, func() bool { _, err := os.Stat(calls); return err == nil }
}

// A finding that keeps failing stops being sent after three attempts. The
// run that stops it names it, says what it is about and why its last attempt
// failed, and `tacit verify --requeue` sends it again. v0.9.14 said only how
// many there were. A pair whose fact a third fact replaced, the cause of the
// strandings v0.9.15 named, is no longer sent at all: it moves to the live
// facts (TestVerifyRepointsPairsWhoseFactsWereReplaced), here to b / c.
func TestVerifyNamesStrandedFindingsAndRequeuesThem(t *testing.T) {
	stubScrump(t)
	_, clone := reviewFixture(t)
	rs := clone("one")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	state, called := sandboxVerify(t, rs.s.Dir)
	// a is already retired in favour of c, so "b may replace a" moves to the
	// finding on b and c; d is gone from the store, so its pair takes only
	// "both": a "keep c" on d / c cannot land.
	p := filepath.Join(rs.s.Dir, "system", "a.md")
	b, _ := os.ReadFile(p)
	os.WriteFile(p, []byte(strings.Replace(string(b), "created:", "superseded_by: c\ncreated:", 1)), 0o644)
	git(t, rs.s.Dir, "rm", "-q", "system/d.md")
	git(t, rs.s.Dir, "commit", "-q", "-am", "a retired by c, d gone")
	git(t, rs.s.Dir, "push", "-q", "origin", "HEAD:main")
	ab, dc, bc, ca := itemFor(t, rs, "a", "b"), itemFor(t, rs, "d", "c"), itemFor(t, rs, "b", "c"), itemFor(t, rs, "c", "a")
	// A verdict that keeps a name that is neither fact. It carries a plain
	// marker and a token-shaped part, both built here; neither may reach the
	// failure record or anything the run prints (the real redactor would
	// mask the token, not the marker).
	token := "ghp_" + strings.Repeat("Q", 36)
	marker := "PLAINMARKER7"
	sent := map[string]int{}
	call := func(_ context.Context, _, prompt, _ string, _ float64) ([]verifyVerdict, string, float64, error) {
		var vs []verifyVerdict
		for _, it := range rs.items() {
			if !strings.Contains(prompt, it.ID) {
				continue
			}
			sent[it.ID]++
			// d/c and b/c get a verdict; the others, c/a among them, none.
			switch it.ID {
			case dc.ID:
				vs = append(vs, verifyVerdict{ID: it.ID, Mark: "keep", Keep: "c", Evidence: "c is current"})
			case bc.ID:
				vs = append(vs, verifyVerdict{ID: it.ID, Mark: "keep", Keep: marker + token, Evidence: "x"})
			}
		}
		// and one for a finding the batch does not hold, with a marker id
		vs = append(vs, verifyVerdict{ID: marker + token, Mark: "both", Evidence: "x"})
		return vs, "raw", 0.1, nil
	}
	r := verifyRun{model: "m", max: 50, per: 50, budget: 1, cwd: t.TempDir(), stateDir: filepath.Join(state, "verify")}
	var all, last string
	run := func() {
		last = captureStderr(t, func() { r.run(ctx, rs.s, call) })
		all += last
	}
	for i := 0; i < 4; i++ {
		run()
	}
	if sent[dc.ID] != 3 || sent[ca.ID] != 3 || sent[ab.ID] != 0 {
		t.Fatalf("sent %v, want d/c and c/a 3 times each and a/b never", sent)
	}
	if d := decisionsOf(t, rs)[ab.ID]; d.Action != mootMark || d.Repointed != bc.ID {
		t.Fatalf("a/b: %+v, want it closed and moved to b/c", d)
	}
	for _, want := range []string{
		"verify: 4 finding(s) reached 3 failed attempts and are no longer sent",
		"verify: stranded " + dc.ID + ", contradiction d / c: keep c: that mark is not possible for this finding now (",
		"verify: stranded " + ca.ID + ", supersede-proposed c / a: no verdict came back for it (",
		"verify: stranded " + bc.ID + ", supersede-proposed b / c: the verdict keeps a fact that is neither of the two (",
	} {
		if !strings.Contains(last, want) {
			t.Fatalf("the run does not say %q:\n%s", want, last)
		}
	}
	failures := filepath.Join(state, "verify", "failures.json")
	if st, err := os.Stat(failures); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("failures.json mode: %v %v", st, err)
	}
	// A run holds the counters in memory: a requeue while one runs is
	// refused, not undone by the run's next save; a dry run needs no lock.
	held, err := tryJobLock(state, "verifier")
	if err != nil {
		t.Fatal(err)
	}
	if code := runVerify([]string{"--requeue", dc.ID}); code != 1 || r.attempts()[dc.ID] != 3 {
		t.Fatalf("requeue during a run: exit %d, attempts %v", code, r.attempts())
	}
	if code := runVerify([]string{"--dry-run", "--requeue", "all"}); code != 0 || r.attempts()[dc.ID] != 3 {
		t.Fatalf("dry requeue during a run: exit %d, attempts %v", code, r.attempts())
	}
	held()
	// An empty list is a usage error, never a run; an id with no count is
	// an error; a repeated id is requeued once.
	var code int
	if errs := captureStderr(t, func() { code = runVerify([]string{"--requeue", ""}) }); code != 2 || !strings.Contains(errs, "--requeue needs all or finding ids") || called() {
		t.Fatalf("empty requeue: exit %d, model called %v, %q", code, called(), errs)
	}
	if code := runVerify([]string{"--requeue", "000000000000"}); code != 1 {
		t.Fatalf("unknown id: exit %d", code)
	}
	// A finding the owner settles meanwhile loses its count on disk. Their
	// "both" on c / a also undoes a -> c, so the curator reopens a / b and it
	// is sent; every other finding is at the limit and is not.
	if err := mark(ctx, rs, ca.ID, "both"); err != nil {
		t.Fatal(err)
	}
	before := len(sent)
	run()
	if r.attempts()[ca.ID] != 0 || strings.Contains(last, ca.ID) || len(sent) != before+1 || sent[ab.ID] != 1 {
		t.Fatalf("after the owner settles c / a: attempts %v, sent %v\n%s", r.attempts(), sent, last)
	}
	// Requeue one by id (twice in the list): it is sent again, the others not.
	if code := runVerify([]string{"--requeue", dc.ID + "," + dc.ID}); code != 0 {
		t.Fatalf("requeue exit %d", code)
	}
	if n := r.attempts(); n[dc.ID] != 0 || n[bc.ID] != 3 {
		t.Fatalf("attempts after requeueing d/c: %v", n)
	}
	if f := r.failures(); f[dc.ID].Why != "" || f[bc.ID].Why == "" {
		t.Fatalf("failures after requeueing d/c: %v", f)
	}
	run()
	if sent[dc.ID] != 4 || sent[bc.ID] != 3 {
		t.Fatalf("after requeueing d/c: sent %v", sent)
	}
	// Requeue all: the rest are sent again.
	if code := runVerify([]string{"--requeue", "all"}); code != 0 {
		t.Fatalf("requeue all exit %d", code)
	}
	run()
	if sent[bc.ID] != 4 {
		t.Fatalf("after requeue all: sent %v", sent)
	}
	rec, _ := os.ReadFile(failures)
	for _, s := range []string{marker, token} {
		if strings.Contains(string(rec), s) || strings.Contains(all, s) {
			t.Fatalf("a verdict's text %q reached the failure record or the output:\n%s\n%s", s, rec, all)
		}
	}
	if called() {
		t.Fatal("a model CLI was called")
	}
}

// A stale finding on a fact the owner kept in a pair is theirs: the page
// refuses an agent's mark on it, so the verifier never sends it.
func TestVerifyLeavesAStaleFindingOnAFactTheOwnerKept(t *testing.T) {
	stubScrump(t)
	_, clone := reviewFixture(t)
	rs := clone("one")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	state, _ := sandboxVerify(t, rs.s.Dir)
	if err := mark(ctx, rs, itemFor(t, rs, "d", "c").ID, "right"); err != nil { // the owner keeps c
		t.Fatal(err)
	}
	git(t, rs.s.Dir, "pull", "-q", "--rebase")
	os.WriteFile(filepath.Join(rs.s.Dir, "review", "2026-10-02.md"), []byte("- stale? [[c]] (system) @abc1234 n=1 since=2026-10-01\n  2026-10-09 abc1234 x: subject - FOO (gone at HEAD)\n"), 0o644)
	git(t, rs.s.Dir, "add", "-A")
	git(t, rs.s.Dir, "commit", "-q", "-m", "stale c")
	git(t, rs.s.Dir, "push", "-q", "origin", "HEAD:main")
	rs.loadFacts()
	var sid string
	for _, it := range rs.staleItems() {
		sid = it.ID
	}
	if sid == "" {
		t.Fatal("no stale finding")
	}
	sent := 0
	call := func(_ context.Context, _, prompt, _ string, _ float64) ([]verifyVerdict, string, float64, error) {
		if strings.Contains(prompt, sid) {
			sent++
			return []verifyVerdict{{ID: sid, Mark: "keep", Evidence: "still true at HEAD"}}, "raw", 0.1, nil
		}
		return nil, "raw", 0.1, nil
	}
	r := verifyRun{model: "m", max: 50, per: 50, budget: 1, cwd: t.TempDir(), stateDir: filepath.Join(state, "verify")}
	for i := 0; i < 2; i++ {
		r.run(ctx, rs.s, call)
	}
	if sent != 0 || r.attempts()[sid] != 0 {
		t.Fatalf("a stale finding on a fact the owner kept was sent %d time(s), count %d", sent, r.attempts()[sid])
	}
}

// One verify runs at a time: a second run while one holds verifier.lock
// does nothing and says so, and calls no model; the same run without the
// lock held does call one.
func TestVerifyRunIsBusyWhileAnotherHoldsTheLock(t *testing.T) {
	stubScrump(t)
	_, clone := reviewFixture(t)
	rs := clone("one")
	state, called := sandboxVerify(t, rs.s.Dir)
	held, err := tryJobLock(state, "verifier")
	if err != nil {
		t.Fatal(err)
	}
	var code int
	out := captureStdout(t, func() { code = runVerify(nil) })
	if code != 0 || !strings.Contains(out, "verify: busy") || called() {
		t.Fatalf("exit %d, model called %v, output %q", code, called(), out)
	}
	held()
	captureStdout(t, func() { runVerify(nil) })
	if !called() {
		t.Fatal("without the lock held the run calls no model either, so this test cannot fail")
	}
}
