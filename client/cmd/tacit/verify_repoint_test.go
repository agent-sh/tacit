package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/store"
)

// repointFixture is a store whose review file holds four pairs, three of them
// with a fact a third fact replaced since:
//
//	b may replace a   a is superseded by c (live): moves to c / b
//	d vs f            f is superseded by g, which is retired: moot
//	h may replace x   x is superseded by y, y by h: both lead to h, moot
//	i vs j            nothing replaced: sent as it is
func repointFixture(t *testing.T) *reviewServer {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, "store.git")
	git(t, root, "init", "-q", "--bare", "-b", "main", bare)
	seed := filepath.Join(root, "seed")
	git(t, root, "clone", "-q", bare, seed)
	os.MkdirAll(filepath.Join(seed, "system"), 0o755)
	os.MkdirAll(filepath.Join(seed, "review"), 0o755)
	extra := map[string]string{"a": "superseded_by: c\n", "f": "superseded_by: g\n", "g": "retired: 2026-10-27\n", "x": "superseded_by: y\n", "y": "superseded_by: h\n"}
	for _, n := range []string{"a", "b", "c", "d", "f", "g", "h", "i", "j", "x", "y"} {
		os.WriteFile(filepath.Join(seed, "system", n+".md"), []byte(strings.Replace(factFile(n), "created:", extra[n]+"created:", 1)), 0o644)
	}
	os.WriteFile(filepath.Join(seed, "review", "2026-10-20.md"), []byte("- supersedes? [[b]] may replace [[a]]\n- CONTRADICTION?: [[f]] (system) vs [[d]] (system)\n- supersedes? [[h]] may replace [[x]]\n- CONTRADICTION?: [[j]] (system) vs [[i]] (system)\n"), 0o644)
	markAudited(t, seed)
	git(t, seed, "add", "-A")
	git(t, seed, "commit", "-q", "-m", "seed")
	git(t, seed, "push", "-q", "origin", "HEAD:main")
	dir := filepath.Join(root, "one")
	git(t, root, "clone", "-q", bare, dir)
	git(t, dir, "config", "user.email", "curator@example.test")
	rs := &reviewServer{s: &store.Store{Dir: dir}}
	rs.loadFacts()
	return rs
}

func decisionsOf(t *testing.T, rs *reviewServer) map[string]decision {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(rs.s.Dir, "review", "decisions.jsonl"))
	out := map[string]decision{}
	for _, l := range strings.Split(string(b), "\n") {
		var d decision
		if json.Unmarshal([]byte(l), &d) == nil && d.ID != "" {
			out[d.ID] = d
		}
	}
	return out
}

// A pair whose fact a third fact replaced cannot take "keep the other" (the
// page refuses it, so the finding stranded after three attempts). The run
// moves it to the live facts and the verifier judges those, or closes it as
// moot with the reason when a side has no live successor or both lead to one
// fact; a pair with nothing replaced is sent as before. A second run moves
// nothing; the owner can reopen a moot finding, which changes no fact, and
// it is theirs after that.
func TestVerifyRepointsPairsWhoseFactsWereReplaced(t *testing.T) {
	stubScrump(t)
	rs := repointFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	state, called := sandboxVerify(t, rs.s.Dir)
	ab, df, xh, ij := itemFor(t, rs, "a", "b"), itemFor(t, rs, "d", "f"), itemFor(t, rs, "x", "h"), itemFor(t, rs, "i", "j")
	cb := pairID(kindSupersedeAsk, "c", "b")
	sent := map[string]int{}
	call := func(_ context.Context, _, prompt, _ string, _ float64) ([]verifyVerdict, string, float64, error) {
		var vs []verifyVerdict
		for _, it := range rs.items() {
			if !strings.Contains(prompt, `"ID":"`+it.ID+`"`) {
				continue
			}
			sent[it.ID]++
			switch it.ID {
			case cb:
				vs = append(vs, verifyVerdict{ID: it.ID, Mark: "keep", Keep: "b", Evidence: "b is current"})
			case ij.ID:
				vs = append(vs, verifyVerdict{ID: it.ID, Mark: "both", Evidence: "different subjects"})
			}
		}
		return vs, "raw", 0.1, nil
	}
	r := verifyRun{model: "m", max: 50, per: 50, budget: 1, cwd: t.TempDir(), stateDir: filepath.Join(state, "verify")}

	// A dry run says what it would close and writes nothing.
	head := gitIn(t, rs.s.Dir, "rev-parse", "HEAD")
	r.dry = true
	out := captureStdout(t, func() { r.run(ctx, rs.s, call) })
	for _, want := range []string{"verify: would close " + ab.ID + " (a / b): re-pointed to c / b", "verify: would close " + df.ID + " (d / f): moot", "verify: would close " + xh.ID + " (x / h): moot"} {
		if !strings.Contains(out, want) {
			t.Fatalf("the dry run does not say %q:\n%s", want, out)
		}
	}
	if got := gitIn(t, rs.s.Dir, "rev-parse", "HEAD"); got != head || gitIn(t, rs.s.Dir, "status", "--porcelain") != "" {
		t.Fatal("the dry run changed the store")
	}
	r.dry = false
	for id := range sent {
		delete(sent, id)
	}

	facts := map[string]string{}
	for _, n := range []string{"a", "b", "d", "f", "g", "h", "x", "y"} {
		facts[n] = read(t, rs, n)
	}
	errs := captureStderr(t, func() { r.run(ctx, rs.s, call) })
	if sent[cb] != 1 || sent[ij.ID] != 1 || sent[ab.ID]+sent[df.ID]+sent[xh.ID] != 0 {
		t.Fatalf("sent %v: want the live pair c / b and i / j, none of the closed ones\n%s", sent, errs)
	}
	ds := decisionsOf(t, rs)
	for id, want := range map[string]string{
		ab.ID: "re-pointed to c / b, finding " + cb + ": a is replaced by c; b stands",
		df.ID: "moot: f is replaced by g, which is retired (2026-10-27), and no live fact replaces it",
		xh.ID: "moot: both facts now lead to h (x is replaced by h; h stands)",
	} {
		d := ds[id]
		if d.Action != mootMark || d.By != "curator" || d.Evidence != want || d.Before != nil {
			t.Fatalf("%s: %+v, want a moot record by the curator saying %q", id, d, want)
		}
	}
	if ds[ab.ID].Repointed != cb || ds[df.ID].Repointed != "" {
		t.Fatalf("repointed: %q %q", ds[ab.ID].Repointed, ds[df.ID].Repointed)
	}
	// The new finding is in today's review file, and the verifier's mark on
	// it landed: b replaces c.
	today, _ := os.ReadFile(filepath.Join(rs.s.Dir, "review", time.Now().Format("2006-01-02")+".md"))
	if !strings.Contains(string(today), "- supersedes? [[b]] may replace [[c]]; owner decides\n  re-pointed "+time.Now().Format("2006-01-02")+" from finding "+ab.ID+" (a / b): a is replaced by c; b stands\n") {
		t.Fatalf("today's review file:\n%s", today)
	}
	if !strings.Contains(read(t, rs, "c"), "superseded_by: b") || ds[cb].Action != "right" {
		t.Fatalf("the verdict on c / b did not land: %+v\n%s", ds[cb], read(t, rs, "c"))
	}
	for n, b := range facts {
		if read(t, rs, n) != b {
			t.Fatalf("closing the findings changed %s:\n%s", n, read(t, rs, n))
		}
	}
	if !strings.Contains(errs, "verify: 3 finding(s) with a fact replaced by a third one: 1 re-pointed to the live facts, 2 moot") {
		t.Fatalf("the run does not count what it closed:\n%s", errs)
	}

	// Nothing more to move.
	before := len(strings.Split(strings.TrimSpace(gitIn(t, rs.s.Dir, "log", "--format=%H")), "\n"))
	errs = captureStderr(t, func() { r.run(ctx, rs.s, call) })
	if after := len(strings.Split(strings.TrimSpace(gitIn(t, rs.s.Dir, "log", "--format=%H")), "\n")); after != before || strings.Contains(errs, "closed") {
		t.Fatalf("a second run moved something (%d -> %d commits):\n%s", before, after, errs)
	}

	// The page shows the record and takes only reopen; reopening puts
	// nothing back, and the finding is the owner's from then on.
	it := itemFor(t, rs, "a", "b")
	if it.Decision == nil || it.Decision.Action != mootMark || it.NeedsOwner || strings.Join(it.Allowed, ",") != "reopen" {
		t.Fatalf("the closed finding on the page: %+v allowed %v", it.Decision, it.Allowed)
	}
	aBefore := read(t, rs, "a")
	note, err := rs.decide(ctx, ab.ID, "reopen", false)
	if err != nil || !strings.Contains(note, "Closing it changed no fact") || read(t, rs, "a") != aBefore {
		t.Fatalf("reopen: %q %v", note, err)
	}
	errs = captureStderr(t, func() { r.run(ctx, rs.s, call) })
	if d := decisionsOf(t, rs)[ab.ID]; d.Action != "reopen" || sent[ab.ID] != 0 {
		t.Fatalf("after the owner's reopen: %+v, sent %d\n%s", d, sent[ab.ID], errs)
	}
	if called() {
		t.Fatal("a model CLI was called")
	}
}
