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
	rs, _ := repointStore(t, nil, nil, "- supersedes? [[b]] may replace [[a]]\n- CONTRADICTION?: [[f]] (system) vs [[d]] (system)\n- supersedes? [[h]] may replace [[x]]\n- CONTRADICTION?: [[j]] (system) vs [[i]] (system)\n")
	return rs
}

// repointStore is a store of facts a b c d f g h i j x y (with the relations
// of repointFixture, more in extra, and names more facts), review the review
// file's lines; it returns the clone and its bare remote.
func repointStore(t *testing.T, names []string, more map[string]string, review string) (*reviewServer, string) {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, "store.git")
	git(t, root, "init", "-q", "--bare", "-b", "main", bare)
	seed := filepath.Join(root, "seed")
	git(t, root, "clone", "-q", bare, seed)
	os.MkdirAll(filepath.Join(seed, "system"), 0o755)
	os.MkdirAll(filepath.Join(seed, "review"), 0o755)
	extra := map[string]string{"a": "superseded_by: c\n", "f": "superseded_by: g\n", "g": "retired: 2026-10-27\n", "x": "superseded_by: y\n", "y": "superseded_by: h\n"}
	for k, v := range more {
		extra[k] = v
	}
	for _, n := range append([]string{"a", "b", "c", "d", "f", "g", "h", "i", "j", "x", "y"}, names...) {
		body := factFile(n)
		if x := extra[n]; strings.HasPrefix(x, "ruling\n") {
			// An owner ruling: kind user, and the owner wrote it (fact.IsRuling).
			body = strings.Replace(strings.Replace(body, "harness: curator", "harness: human", 1), "kind: observed", "kind: user", 1)
			extra[n] = strings.TrimPrefix(x, "ruling\n")
		}
		os.WriteFile(filepath.Join(seed, "system", n+".md"), []byte(strings.Replace(body, "created:", extra[n]+"created:", 1)), 0o644)
	}
	os.WriteFile(filepath.Join(seed, "review", "2026-10-20.md"), []byte(review), 0o644)
	markAudited(t, seed)
	git(t, seed, "add", "-A")
	git(t, seed, "commit", "-q", "-m", "seed")
	git(t, seed, "push", "-q", "origin", "HEAD:main")
	dir := filepath.Join(root, "one")
	git(t, root, "clone", "-q", bare, dir)
	git(t, dir, "config", "user.email", "curator@example.test")
	rs := &reviewServer{s: &store.Store{Dir: dir}}
	rs.loadFacts()
	return rs, bare
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
	today, _ := os.ReadFile(filepath.Join(rs.s.Dir, filepath.FromSlash(repointedFile(time.Now()))))
	if string(today) != "- supersedes? [[b]] may replace [[c]]; owner decides\n  re-pointed "+time.Now().Format("2006-01-02")+" from finding "+ab.ID+" (a / b): a is replaced by c; b stands\n" {
		t.Fatalf("today's re-pointed findings:\n%s", today)
	}
	if _, err := os.Stat(filepath.Join(rs.s.Dir, "review", time.Now().Format("2006-01-02")+".md")); err == nil {
		t.Fatal("the step wrote the curator's review file")
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

// Every kind of pair moves to a finding its parser reads back, on the live
// facts, of the kind it maps to, and the record names that finding's id; two
// findings that move to one pair raise it once.
func TestRepointRaisesEveryKindAsAFindingItsParserReads(t *testing.T) {
	stubScrump(t)
	var names []string
	for _, n := range []string{"1", "2", "3", "4", "5", "6"} {
		names = append(names, "a"+n, "b"+n, "c"+n)
	}
	names = append(names, "a7", "b7", "d7", "x7", "e7")
	more := map[string]string{
		"a1": "superseded_by: c1\n", "a2": "superseded_by: c2\n", "a3": "superseded_by: c3\n",
		"a4": "ruling\nsuperseded_by: c4\n", "c4": "ruling\n", "a5": "ruling\nsuperseded_by: c5\n",
		"a6": "superseded_by: c6\n", "b7": "superseded_by: d7\n", "x7": "superseded_by: e7\n", "e7": "superseded_by: a7\n",
	}
	review := "- CONTRADICTION?: [[b1]] (system) vs [[a1]] (system)\n" +
		"- duplicate in one run: [[a2]] and [[b2]]\n" +
		"- supersedes: [[b3]] replaces [[a3]]\n" +
		"- RULING CHANGE? new [[b4]] may supersede owner ruling [[a4]]\n" +
		"- RULING CHANGE? new [[b5]] may supersede owner ruling [[a5]]\n" +
		"- result-superseded? [[b6]] may replace [[a6]]\n" +
		"- supersedes? [[b7]] may replace [[a7]]\n" +
		"- supersedes? [[b7]] may replace [[x7]]\n"
	rs, _ := repointStore(t, names, more, review)
	sandboxVerify(t, rs.s.Dir)
	cases := []struct{ a, b, kind, newA, newB, newKind string }{
		{"a1", "b1", kindContradiction, "c1", "b1", kindContradiction},
		{"a2", "b2", kindDuplicate, "c2", "b2", kindContradiction},
		{"a3", "b3", kindSupersedeDone, "c3", "b3", kindSupersedeAsk},
		{"a4", "b4", kindRulingChange, "c4", "b4", kindRulingChange},
		{"a5", "b5", kindRulingChange, "c5", "b5", kindSupersedeAsk},
		{"a6", "b6", kindResultSup, "c6", "b6", kindResultSup},
		{"a7", "b7", kindSupersedeAsk, "a7", "d7", kindSupersedeAsk},
		// x7 is replaced by a7 through e7: it moves to the same pair.
		{"x7", "b7", kindSupersedeAsk, "a7", "d7", kindSupersedeAsk},
	}
	ids := map[string]string{}
	for _, c := range cases {
		it := itemFor(t, rs, c.a, c.b)
		if it.Kind != c.kind {
			t.Fatalf("%s / %s is a %s, want %s", c.a, c.b, it.Kind, c.kind)
		}
		ids[c.a] = it.ID
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	r := verifyRun{model: "m", max: 50, per: 50, budget: 1, cwd: t.TempDir(), stateDir: filepath.Join(t.TempDir(), "verify")}
	if err := r.repoint(ctx, rs, rs.ownerTouched()); err != nil {
		t.Fatal(err)
	}
	rs.loadFacts()
	byID := map[string]reviewItem{}
	for _, it := range rs.items() {
		byID[it.ID] = it
	}
	ds := decisionsOf(t, rs)
	for _, c := range cases {
		d := ds[ids[c.a]]
		x, ok := byID[d.Repointed]
		if d.Action != mootMark || !ok || x.Kind != c.newKind || x.A.Name != c.newA || x.B.Name != c.newB || x.Decision != nil {
			t.Errorf("%s / %s (%s): record %+v, its finding %+v", c.a, c.b, c.kind, d, x)
		}
	}
	if ds[ids["a7"]].Repointed != ds[ids["x7"]].Repointed {
		t.Errorf("the two findings on a7 / d7 name %s and %s", ds[ids["a7"]].Repointed, ds[ids["x7"]].Repointed)
	}
	b, _ := os.ReadFile(filepath.Join(rs.s.Dir, filepath.FromSlash(repointedFile(time.Now()))))
	if n := strings.Count(string(b), "\n- "); n+1 != 7 || !strings.HasPrefix(string(b), "- ") {
		t.Errorf("%d findings raised, want 7 (one for the two that move to a7 / d7):\n%s", n+1, b)
	}
}

// A replay (--ids) moves only what it names; --max 0 moves and sends
// nothing; a write the server refuses closes nothing, and the verifier still
// sends none of the findings it would have closed; a finding the owner left a
// note on stays, and a verdict on it becomes their question.
func TestVerifyRepointKeepsToItsBounds(t *testing.T) {
	stubScrump(t)
	rs, bare := repointStore(t, nil, nil, "- supersedes? [[b]] may replace [[a]]\n- CONTRADICTION?: [[f]] (system) vs [[d]] (system)\n- supersedes? [[h]] may replace [[x]]\n- CONTRADICTION?: [[j]] (system) vs [[i]] (system)\n")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	state, called := sandboxVerify(t, rs.s.Dir)
	ab, df, xh, ij := itemFor(t, rs, "a", "b"), itemFor(t, rs, "d", "f"), itemFor(t, rs, "x", "h"), itemFor(t, rs, "i", "j")
	sent := map[string]int{}
	call := func(_ context.Context, _, prompt, _ string, _ float64) ([]verifyVerdict, string, float64, error) {
		var vs []verifyVerdict
		for _, it := range rs.items() {
			if strings.Contains(prompt, `"ID":"`+it.ID+`"`) {
				sent[it.ID]++
				vs = append(vs, verifyVerdict{ID: it.ID, Mark: "keep", Keep: it.B.Name, Evidence: "b is current"})
			}
		}
		return vs, "raw", 0.1, nil
	}
	newRun := func() verifyRun {
		return verifyRun{model: "m", max: 50, per: 50, budget: 1, cwd: t.TempDir(), stateDir: filepath.Join(state, "verify")}
	}
	closed := func() map[string]bool {
		out := map[string]bool{}
		for id, d := range decisionsOf(t, rs) {
			out[id] = d.Action == mootMark
		}
		return out
	}

	// The server refuses every push: nothing closes, and nothing it would
	// close is sent.
	hook := filepath.Join(bare, "hooks", "pre-receive")
	os.WriteFile(hook, []byte("#!/bin/sh\necho refused >&2\nexit 1\n"), 0o755)
	r := newRun()
	errs := captureStderr(t, func() { r.run(ctx, rs.s, call) })
	if c := closed(); c[ab.ID] || c[df.ID] || c[xh.ID] || sent[ab.ID]+sent[df.ID]+sent[xh.ID] != 0 || sent[ij.ID] != 1 {
		t.Fatalf("with the push refused: closed %v, sent %v\n%s", c, sent, errs)
	}
	if gitIn(t, rs.s.Dir, "status", "--porcelain") != "" {
		t.Fatal("a refused write left changes in the clone")
	}
	os.Remove(hook)

	// A replay of x / h closes only it, and sends nothing (it is closed).
	r = newRun()
	r.only = []string{xh.ID}
	captureStderr(t, func() { r.run(ctx, rs.s, call) })
	if c := closed(); !c[xh.ID] || c[ab.ID] || c[df.ID] || sent[xh.ID] != 0 {
		t.Fatalf("replay of x / h: closed %v, sent %v", c, sent)
	}

	// The owner leaves a note on a / b: it stays, is sent, and the verdict
	// that would retire a becomes a question for them.
	if _, err := rs.answer(ctx, ab.ID, "note", "a still holds on the old boxes", false); err != nil {
		t.Fatal(err)
	}
	git(t, rs.s.Dir, "pull", "-q", "--rebase")
	before := sent[ij.ID]
	r = newRun()
	r.max = 0
	captureStderr(t, func() { r.run(ctx, rs.s, call) })
	if c := closed(); !c[df.ID] || c[ab.ID] || len(sent) != 1 || sent[ij.ID] != before {
		t.Fatalf("--max 0: closed %v, sent %v", c, sent)
	}
	r = newRun()
	captureStderr(t, func() { r.run(ctx, rs.s, call) })
	if it := itemFor(t, rs, "a", "b"); sent[ab.ID] != 1 || it.Ask == nil || it.Decision != nil {
		t.Fatalf("the noted finding: sent %d, ask %+v, decision %+v", sent[ab.ID], it.Ask, it.Decision)
	}
	if called() {
		t.Fatal("a model CLI was called")
	}
}

// A replay (--ids) of a finding that moves verifies the finding in its place
// in the same run; a pair whose replacement is itself under review (a supersede
// with an open finding of its own) waits, neither moved nor sent, until that
// review is answered; and a moot closure whose supersede was undone since is
// reopened by the curator and sent.
func TestVerifyRepointFollowsReplaysWaitsAndReopens(t *testing.T) {
	stubScrump(t)
	rs, _ := repointStore(t, []string{"k", "m", "n"}, map[string]string{"k": "superseded_by: m\n"},
		"- supersedes? [[b]] may replace [[a]]\n- CONTRADICTION?: [[n]] (system) vs [[k]] (system)\n- supersedes: [[m]] replaces [[k]]\n")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	state, called := sandboxVerify(t, rs.s.Dir)
	ab, kn, km := itemFor(t, rs, "a", "b"), itemFor(t, rs, "k", "n"), itemFor(t, rs, "k", "m")
	cb := pairID(kindSupersedeAsk, "c", "b")
	sent := map[string]int{}
	verdicts := map[string]verifyVerdict{}
	call := func(_ context.Context, _, prompt, _ string, _ float64) ([]verifyVerdict, string, float64, error) {
		var vs []verifyVerdict
		for _, it := range rs.items() {
			if strings.Contains(prompt, `"ID":"`+it.ID+`"`) {
				sent[it.ID]++
				if v, ok := verdicts[it.ID]; ok {
					vs = append(vs, v)
				}
			}
		}
		return vs, "raw", 0.1, nil
	}
	newRun := func() verifyRun {
		return verifyRun{model: "m", max: 50, per: 50, budget: 1, cwd: t.TempDir(), stateDir: filepath.Join(state, "verify")}
	}

	// A replay of a / b moves it to c / b and verifies c / b.
	verdicts[cb] = verifyVerdict{ID: cb, Mark: "both", Evidence: "different subjects"}
	r := newRun()
	r.only = []string{ab.ID}
	captureStderr(t, func() { r.run(ctx, rs.s, call) })
	if d := decisionsOf(t, rs); d[ab.ID].Repointed != cb || sent[cb] != 1 || d[cb].Action != "both" {
		t.Fatalf("replay of a / b: records %+v %+v, sent %v", d[ab.ID], d[cb], sent)
	}

	// k / n rests on k -> m, which "m replaces k" still asks about: it waits.
	verdicts[km.ID] = verifyVerdict{ID: km.ID, Mark: "both", Evidence: "k still holds"}
	r = newRun()
	captureStderr(t, func() { r.run(ctx, rs.s, call) })
	if d := decisionsOf(t, rs); d[kn.ID].Action != "" || sent[kn.ID] != 0 || sent[km.ID] != 1 {
		t.Fatalf("k / n under review: record %+v, sent %v", d[kn.ID], sent)
	}
	// The verifier undid k -> m ("both"): k / n is a plain pair now, sent.
	if strings.Contains(read(t, rs, "k"), "superseded_by") {
		t.Fatalf("the verdict on m / k did not undo the supersede:\n%s", read(t, rs, "k"))
	}
	r = newRun()
	captureStderr(t, func() { r.run(ctx, rs.s, call) })
	if sent[kn.ID] != 1 {
		t.Fatalf("k / n after its supersede was undone: sent %v", sent)
	}

	// Undo the supersede a / b moved along: the curator reopens a / b and it
	// is sent.
	p := filepath.Join(rs.s.Dir, "system", "a.md")
	b, _ := os.ReadFile(p)
	os.WriteFile(p, []byte(strings.Replace(string(b), "superseded_by: c\n", "", 1)), 0o644)
	git(t, rs.s.Dir, "commit", "-q", "-am", "a stands again")
	git(t, rs.s.Dir, "push", "-q", "origin", "HEAD:main")
	r = newRun()
	errs := captureStderr(t, func() { r.run(ctx, rs.s, call) })
	if it := itemFor(t, rs, "a", "b"); it.Decision != nil || sent[ab.ID] != 1 || !strings.Contains(errs, "verify: reopened "+ab.ID) {
		t.Fatalf("a / b after a stands again: decision %+v, sent %v\n%s", it.Decision, sent, errs)
	}
	if rs.ownerTouched()[ab.ID] {
		t.Fatal("the curator's reopen counts as the owner's")
	}
	if called() {
		t.Fatal("a model CLI was called")
	}
}
