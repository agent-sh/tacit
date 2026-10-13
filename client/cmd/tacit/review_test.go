package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/store"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func factFile(name string) string {
	return "---\nname: " + name + "\ndescription: \"fact " + name + "\"\ntype: design_decision\nscope: system\napplies_to:\n  paths: []\n  repos: [\"x\"]\nsource:\n  harness: curator\n  host: h\n  kind: observed\ncreated: 2026-10-01\n---\n\nbody of " + name + "\n"
}

// reviewFixture is a bare remote seeded with facts a, b, c and one review
// file asking whether b replaces a and whether d and c disagree.
func reviewFixture(t *testing.T) (bare string, clone func(string) *reviewServer) {
	root := t.TempDir()
	bare = filepath.Join(root, "store.git")
	git(t, root, "init", "-q", "--bare", "-b", "main", bare)
	seed := filepath.Join(root, "seed")
	git(t, root, "clone", "-q", bare, seed)
	os.MkdirAll(filepath.Join(seed, "system"), 0o755)
	os.MkdirAll(filepath.Join(seed, "review"), 0o755)
	for _, n := range []string{"a", "b", "c", "d"} {
		os.WriteFile(filepath.Join(seed, "system", n+".md"), []byte(factFile(n)), 0o644)
	}
	os.WriteFile(filepath.Join(seed, "review", "2026-10-01.md"), []byte("- supersedes? [[b]] may replace [[a]]\n- CONTRADICTION?: [[c]] (system) vs [[d]] (system)\n- supersedes? [[c]] may replace [[b]]\n- supersedes? [[a]] may replace [[c]]\n- CONTRADICTION?: [[a]] (system) vs [[c]] (system)\n"), 0o644)
	markAudited(t, seed)
	git(t, seed, "add", "-A")
	git(t, seed, "commit", "-q", "-m", "seed")
	git(t, seed, "push", "-q", "origin", "HEAD:main")
	clone = func(name string) *reviewServer {
		dir := filepath.Join(root, name)
		git(t, root, "clone", "-q", bare, dir)
		git(t, dir, "config", "user.email", "curator@example.test")
		rs := &reviewServer{s: &store.Store{Dir: dir}}
		rs.loadFacts()
		return rs
	}
	return bare, clone
}

func mark(ctx context.Context, rs *reviewServer, id, m string) error {
	_, err := rs.decide(ctx, id, m, false)
	return err
}

func itemFor(t *testing.T, rs *reviewServer, a, b string) reviewItem {
	t.Helper()
	rs.loadFacts()
	for _, it := range rs.items() {
		if it.A.Name == a && it.B.Name == b {
			return it
		}
	}
	t.Fatalf("no item %s/%s", a, b)
	return reviewItem{}
}

func read(t *testing.T, rs *reviewServer, name string) string {
	b, err := os.ReadFile(filepath.Join(rs.s.Dir, "system", name+".md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Marks set a state, a decided item refuses a second mark, and reopen puts
// both facts back byte for byte, so no sequence can leave a and b retiring
// each other (the delta-based version could).
func TestReviewMarksAreStatesAndReopenIsExact(t *testing.T) {
	_, clone := reviewFixture(t)
	rs := clone("one")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	origA, origB := read(t, rs, "a"), read(t, rs, "b")
	it := itemFor(t, rs, "a", "b")

	if err := mark(ctx, rs, it.ID, "right"); err != nil {
		t.Fatal(err)
	}
	if got := itemFor(t, rs, "a", "b"); got.State != "right" {
		t.Fatalf("state after right = %s", got.State)
	}
	diff := git(t, rs.s.Dir, "diff", "HEAD~1", "--unified=0", "--", "system/a.md")
	var changed []string
	for _, l := range strings.Split(diff, "\n") {
		if (strings.HasPrefix(l, "+") || strings.HasPrefix(l, "-")) && !strings.HasPrefix(l, "+++") && !strings.HasPrefix(l, "---") {
			changed = append(changed, l)
		}
	}
	if len(changed) != 2 || !strings.HasPrefix(changed[0], "+updated: ") || changed[1] != "+superseded_by: b" {
		t.Fatalf("a mark should add two lines to a.md, got %q", changed)
	}
	if err := mark(ctx, rs, it.ID, "left"); err == nil || !strings.Contains(err.Error(), "already marked") {
		t.Fatalf("second mark on a decided item: %v", err)
	}
	if err := mark(ctx, rs, it.ID, "reopen"); err != nil {
		t.Fatal(err)
	}
	if read(t, rs, "a") != origA || read(t, rs, "b") != origB {
		t.Fatal("reopen did not restore the facts exactly")
	}
	if err := mark(ctx, rs, it.ID, "left"); err != nil {
		t.Fatal(err)
	}
	if got := itemFor(t, rs, "a", "b"); got.State != "left" || got.A.SupersededBy != "" {
		t.Fatalf("after left: state %s, a superseded_by %q", got.State, got.A.SupersededBy)
	}
	if err := mark(ctx, rs, it.ID, "reopen"); err != nil {
		t.Fatal(err)
	}
	if read(t, rs, "a") != origA || read(t, rs, "b") != origB {
		t.Fatal("second reopen did not restore the facts exactly")
	}
	if out := git(t, rs.s.Dir, "status", "--porcelain"); out != "" {
		t.Fatalf("clone left dirty:\n%s", out)
	}
}

// A mark that would retire a fact another fact already retired is refused.
func TestReviewRefusesToOverwriteAThirdPartySupersede(t *testing.T) {
	_, clone := reviewFixture(t)
	rs := clone("one")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	p := filepath.Join(rs.s.Dir, "system", "a.md")
	b, _ := os.ReadFile(p)
	os.WriteFile(p, []byte(strings.Replace(string(b), "created:", "superseded_by: c\ncreated:", 1)), 0o644)
	git(t, rs.s.Dir, "commit", "-q", "-am", "a retired by c")
	git(t, rs.s.Dir, "push", "-q", "origin", "HEAD:main")
	it := itemFor(t, rs, "a", "b")
	if contains(it.Allowed, "right") {
		t.Fatalf("right offered while a is retired by c: %v", it.Allowed)
	}
	if err := mark(ctx, rs, it.ID, "right"); err == nil {
		t.Fatal("right accepted while a is retired by c")
	}
}

// Two machines marking different items at once both land; the decisions
// log merges by union instead of wedging a clone mid-rebase.
func TestReviewConcurrentDecisionsFromTwoClonesBothLand(t *testing.T) {
	_, clone := reviewFixture(t)
	one, two := clone("one"), clone("two")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ab, cd := itemFor(t, one, "a", "b"), itemFor(t, two, "d", "c")
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); errs[0] = mark(ctx, one, ab.ID, "right") }()
	go func() { defer wg.Done(); errs[1] = mark(ctx, two, cd.ID, "query") }()
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("clone %d: %v", i, err)
		}
	}
	for _, rs := range []*reviewServer{one, two} {
		if err := rs.s.Sync(ctx); err != nil {
			t.Fatalf("sync %s: %v", rs.s.Dir, err)
		}
		ds, _ := rs.decisions()
		if ds[ab.ID] == nil || ds[cd.ID] == nil {
			t.Fatalf("%s sees %d decisions, want both", rs.s.Dir, len(ds))
		}
	}
	if st := itemFor(t, two, "a", "b").State; st != "right" {
		t.Fatalf("clone two sees a/b as %s", st)
	}
	if st := itemFor(t, one, "d", "c").State; st != "query" {
		t.Fatalf("clone one sees d/c as %s", st)
	}
}

// Reopening one proof undoes only that pair: a mark another proof made on
// the same fact stays, and that proof does not go stale.
func TestReviewReopenLeavesOtherProofsAlone(t *testing.T) {
	_, clone := reviewFixture(t)
	rs := clone("one")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ab, ca := itemFor(t, rs, "a", "b"), itemFor(t, rs, "c", "a")
	if err := mark(ctx, rs, ab.ID, "query"); err != nil {
		t.Fatal(err)
	}
	if err := mark(ctx, rs, ca.ID, "left"); err != nil { // a retired by c: the same fact the reopen touches
		t.Fatal(err)
	}
	if err := mark(ctx, rs, ab.ID, "reopen"); err != nil {
		t.Fatal(err)
	}
	got := itemFor(t, rs, "c", "a")
	if got.State != "left" || got.Stale {
		t.Fatalf("other proof after reopen: state %s stale %v", got.State, got.Stale)
	}
	if st := itemFor(t, rs, "a", "b").State; st != "both" {
		t.Fatalf("reopened pair reads %s", st)
	}
}

// Marks never close a loop of retirements.
func TestReviewRefusesARetirementLoop(t *testing.T) {
	_, clone := reviewFixture(t)
	rs := clone("one")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, p := range [][2]string{{"a", "b"}, {"b", "c"}} { // a by b, b by c
		if err := mark(ctx, rs, itemFor(t, rs, p[0], p[1]).ID, "right"); err != nil {
			t.Fatal(err)
		}
	}
	it := itemFor(t, rs, "c", "a") // right would retire c by a: a -> b -> c
	if contains(it.Allowed, "right") {
		t.Fatalf("loop offered: %v", it.Allowed)
	}
	if err := mark(ctx, rs, it.ID, "right"); err == nil {
		t.Fatal("loop accepted")
	}
}

// Two machines marking proofs that touch the same fact both land, and
// neither clone is left diverged.
func TestReviewTwoClonesEditingOneFact(t *testing.T) {
	_, clone := reviewFixture(t)
	one, two := clone("one"), clone("two")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ab, ca := itemFor(t, one, "a", "b"), itemFor(t, two, "c", "a")
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); errs[0] = mark(ctx, one, ab.ID, "query") }()
	go func() { defer wg.Done(); errs[1] = mark(ctx, two, ca.ID, "query") }()
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("clone %d: %v", i, err)
		}
	}
	for _, rs := range []*reviewServer{one, two} {
		if err := rs.s.Sync(ctx); err != nil {
			t.Fatalf("sync %s: %v", rs.s.Dir, err)
		}
		if a, b := itemFor(t, rs, "a", "b").State, itemFor(t, rs, "c", "a").State; a != "query" || b != "query" {
			t.Fatalf("%s reads %s and %s", rs.s.Dir, a, b)
		}
		if out := git(t, rs.s.Dir, "status", "--porcelain", "--branch"); strings.Contains(out, "ahead") || strings.Contains(out, "behind") || strings.Count(out, "\n") > 1 {
			t.Fatalf("clone not clean:\n%s", out)
		}
	}
}

// A verifier's batch lands in one commit: marks carry who made them and
// the evidence, a verdict naming the facts in the wrong order is refused
// rather than guessed at, and an owner question changes no fact.
func TestReviewAgentBatch(t *testing.T) {
	_, clone := reviewFixture(t)
	rs := clone("one")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ab, dc := itemFor(t, rs, "a", "b"), itemFor(t, rs, "d", "c")
	before := git(t, rs.s.Dir, "rev-parse", "HEAD")
	origC := read(t, rs, "c")
	_, errs, err := rs.applyOps(ctx, []op{
		{ID: ab.ID, A: "a", B: "b", Mark: "right", By: "agent", Evidence: "README.md:3 says b is current"},
		{ID: dc.ID, A: "d", B: "c", Mark: "ask", By: "agent", Question: "Which order do you want?", Evidence: "docs disagree"},
		{ID: dc.ID, A: "c", B: "d", Mark: "both", By: "agent"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if errs[0] != nil || errs[1] != nil || errs[2] == nil {
		t.Fatalf("errs %v", errs)
	}
	log := git(t, rs.s.Dir, "log", "--format=%s", strings.TrimSpace(before)+"..HEAD")
	if strings.Count(log, "\n") != 1 || !strings.Contains(log, "right a / b") || !strings.Contains(log, "by agent") {
		t.Fatalf("commits:\n%s", log)
	}
	got := itemFor(t, rs, "a", "b")
	if got.State != "right" || got.Decision == nil || got.Decision.By != "agent" || got.Decision.Evidence == "" {
		t.Fatalf("agent mark: state %s decision %+v", got.State, got.Decision)
	}
	asked := itemFor(t, rs, "d", "c")
	if asked.Decision != nil || asked.Ask == nil || asked.Ask.Question != "Which order do you want?" || read(t, rs, "c") != origC {
		t.Fatalf("ask: decision %+v ask %+v", asked.Decision, asked.Ask)
	}
	// An agent may not touch a finding that carries a mark or a question.
	_, errs, _ = rs.applyOps(ctx, []op{{ID: dc.ID, A: "d", B: "c", Mark: "both", By: "agent"}})
	if errs[0] == nil {
		t.Fatal("agent overrode an open question")
	}
}

// verify sends only open findings the owner never touched, maps "keep"
// by fact name, refuses ids outside the batch, and stops re-sending a
// finding after three attempts.
func TestVerifySendsOnlyOpenUntouchedFindings(t *testing.T) {
	stubScrump(t)
	_, clone := reviewFixture(t)
	rs := clone("one")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ab, dc := itemFor(t, rs, "a", "b"), itemFor(t, rs, "d", "c")
	// The owner marked a/b and reopened it: it is theirs now.
	if err := mark(ctx, rs, ab.ID, "right"); err != nil {
		t.Fatal(err)
	}
	if err := mark(ctx, rs, ab.ID, "reopen"); err != nil {
		t.Fatal(err)
	}
	var sent []string
	call := func(_ context.Context, _, prompt, _ string, _ float64) ([]verifyVerdict, string, float64, error) {
		var vs []verifyVerdict
		for _, it := range rs.items() {
			if !strings.Contains(prompt, it.ID) {
				continue
			}
			sent = append(sent, it.ID)
			v := verifyVerdict{ID: it.ID, Mark: "both", Evidence: "checked"}
			if it.ID == dc.ID {
				v = verifyVerdict{ID: it.ID, Mark: "keep", Keep: "c", Evidence: "c is current"}
			}
			vs = append(vs, v)
		}
		vs = append(vs, verifyVerdict{ID: "000000000000", Mark: "both", Evidence: "invented"})
		return vs, "raw", 0.5, nil
	}
	r := verifyRun{model: "m", max: 50, per: 3, budget: 1, cwd: t.TempDir(), stateDir: t.TempDir()}
	n, cost, err := r.run(ctx, rs.s, call)
	if err != nil || cost == 0 {
		t.Fatalf("run: n %d cost %v err %v", n, cost, err)
	}
	// The owner touched facts a and b, so every finding about them is theirs;
	// only d/c is left for the verifier.
	if len(sent) != 1 || sent[0] != dc.ID || n != 1 {
		t.Fatalf("sent %v (want only %s), settled %d", sent, dc.ID, n)
	}
	if got := itemFor(t, rs, "d", "c"); got.State != "right" || got.Decision.By != "agent" {
		t.Fatalf("keep c should retire d: %+v", got)
	}
	// Nothing left to send.
	sent = nil
	if n, _, err := r.run(ctx, rs.s, call); err != nil || n != 0 || len(sent) != 0 {
		t.Fatalf("second run: n %d sent %v err %v", n, sent, err)
	}
}

func TestVerifyGivesUpAfterThreeFailedAttempts(t *testing.T) {
	_, clone := reviewFixture(t)
	rs := clone("one")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	calls := 0
	call := func(context.Context, string, string, string, float64) ([]verifyVerdict, string, float64, error) {
		calls++
		return nil, "", 0.1, errors.New("model fell over")
	}
	r := verifyRun{model: "m", max: 50, per: 50, budget: 1, cwd: t.TempDir(), stateDir: t.TempDir()}
	for i := 0; i < 5; i++ {
		r.run(ctx, rs.s, call)
	}
	if calls != 3 {
		t.Fatalf("verifier called %d times, want 3", calls)
	}
}

// An agent never retires an owner ruling: the mark becomes a question. It
// also needs evidence, and it keeps off findings the owner touched, even
// through review apply.
func TestAgentMarksRespectOwnerRulings(t *testing.T) {
	_, clone := reviewFixture(t)
	rs := clone("one")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Make fact a an owner ruling: their words, on record.
	p := filepath.Join(rs.s.Dir, "system", "a.md")
	b, _ := os.ReadFile(p)
	a := strings.Replace(string(b), "kind: observed", "kind: user\n  owner_quote: keep a as the rule here\n  owner_quote_from: "+testOwnerSession, 1)
	os.WriteFile(p, []byte(strings.Replace(a, "body of a", "Owner: \"keep a as the rule here\"", 1)), 0o644)
	markAudited(t, rs.s.Dir) // their words on record: the quote ledger
	git(t, rs.s.Dir, "add", "-A")
	git(t, rs.s.Dir, "commit", "-q", "-m", "a is a ruling")
	git(t, rs.s.Dir, "push", "-q", "origin", "HEAD:main")
	ab, dc := itemFor(t, rs, "a", "b"), itemFor(t, rs, "d", "c")
	_, errs, err := rs.applyOps(ctx, []op{
		{ID: ab.ID, A: "a", B: "b", Mark: "right", By: "agent", Evidence: "b is newer"},
		{ID: dc.ID, A: "d", B: "c", Mark: "both", By: "agent"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if errs[0] != nil {
		t.Fatalf("ruling retirement should turn into a question, got %v", errs[0])
	}
	if got := itemFor(t, rs, "a", "b"); got.Decision != nil || got.Ask == nil || got.A.SupersededBy != "" {
		t.Fatalf("ruling was retired or no question left: %+v", got)
	}
	if errs[1] == nil {
		t.Fatal("agent mark without evidence accepted")
	}
	// A finding with a missing fact and a ruling does not crash and asks nothing
	// of the store: "both" is the only mark left and it changes nothing.
	os.WriteFile(filepath.Join(rs.s.Dir, "review", "2026-10-02.md"), []byte("- supersedes? [[zz]] may replace [[a]]\n"), 0o644)
	git(t, rs.s.Dir, "add", "review")
	git(t, rs.s.Dir, "commit", "-q", "-m", "missing fact finding")
	git(t, rs.s.Dir, "push", "-q", "origin", "HEAD:main")
	za := itemFor(t, rs, "a", "zz")
	if _, errs, err := rs.applyOps(ctx, []op{{ID: za.ID, A: "a", B: "zz", Mark: "both", By: "agent", Evidence: "zz is gone"}}); err != nil || errs[0] != nil {
		t.Fatalf("missing-fact finding: %v %v", err, errs)
	}
	// Two findings name c and a (a supersede and a contradiction). A flag on
	// a ruling is a question, not a write into the ruling.
	var ca []reviewItem
	rs.loadFacts()
	for _, it := range rs.items() {
		if it.A.Name == "c" && it.B.Name == "a" {
			ca = append(ca, it)
		}
	}
	if len(ca) != 2 {
		t.Fatalf("want two c/a findings, got %d", len(ca))
	}
	_, errs, _ = rs.applyOps(ctx, []op{{ID: ca[0].ID, A: "c", B: "a", Mark: "query", By: "agent", Evidence: "they clash"}})
	if got := itemFor(t, rs, "c", "a"); errs[0] != nil || got.Ask == nil || len(got.B.ConflictsWith) != 0 {
		t.Fatalf("agent flag wrote into a ruling: %v %+v", errs[0], got)
	}
	// A "both" that would clear a relation the owner set on the ruling is a question too.
	ap := filepath.Join(rs.s.Dir, "system", "a.md")
	ab2, _ := os.ReadFile(ap)
	os.WriteFile(ap, []byte(strings.Replace(string(ab2), "created:", "conflicts_with: [\"c\"]\ncreated:", 1)), 0o644)
	git(t, rs.s.Dir, "commit", "-q", "-am", "owner flagged a and c")
	git(t, rs.s.Dir, "push", "-q", "origin", "HEAD:main")
	// "right" keeps the ruling a over c, but would clear a's own relation: a question.
	_, errs, _ = rs.applyOps(ctx, []op{{ID: ca[1].ID, A: "c", B: "a", Mark: "right", By: "agent", Evidence: "a is current"}})
	rs.loadFacts()
	var got reviewItem
	for _, it := range rs.items() {
		if it.ID == ca[1].ID {
			got = it
		}
	}
	if errs[0] != nil || got.Ask == nil || len(got.B.ConflictsWith) != 1 || got.A.SupersededBy != "" {
		t.Fatalf("agent right cleared an owner relation on a ruling: %v %+v", errs[0], got)
	}
	// The owner marks and reopens d/c: an agent may not touch it after that.
	if err := mark(ctx, rs, dc.ID, "both"); err != nil {
		t.Fatal(err)
	}
	if err := mark(ctx, rs, dc.ID, "reopen"); err != nil {
		t.Fatal(err)
	}
	_, errs, _ = rs.applyOps(ctx, []op{{ID: dc.ID, A: "d", B: "c", Mark: "both", By: "agent", Evidence: "x"}})
	if errs[0] == nil {
		t.Fatal("agent marked a finding the owner reopened")
	}
}

// A dry run changes nothing, including the attempt counters.
func TestVerifyDryRunBurnsNoAttempts(t *testing.T) {
	_, clone := reviewFixture(t)
	rs := clone("one")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	call := func(context.Context, string, string, string, float64) ([]verifyVerdict, string, float64, error) {
		return nil, "", 0.1, errors.New("down")
	}
	r := verifyRun{model: "m", max: 50, per: 50, budget: 1, cwd: t.TempDir(), stateDir: t.TempDir(), dry: true}
	for i := 0; i < 4; i++ {
		r.run(ctx, rs.s, call)
	}
	if len(r.attempts()) != 0 {
		t.Fatalf("dry runs recorded attempts: %v", r.attempts())
	}
	head := git(t, rs.s.Dir, "rev-parse", "HEAD")
	r.dry = false
	calls := 0
	ok := func(_ context.Context, _, prompt, _ string, _ float64) ([]verifyVerdict, string, float64, error) {
		calls++
		return nil, "", 0.1, errors.New("down")
	}
	r.run(ctx, rs.s, ok)
	if calls != 1 || git(t, rs.s.Dir, "rev-parse", "HEAD") != head {
		t.Fatalf("real run after dry runs: calls %d", calls)
	}
}

// A standard the curator filed as observed is the agents' reading of their
// preference, not their ruling: an agent with evidence may retire it like any
// fact (before 2026-10-11 the type alone made it a ruling and the mark a
// question).
func TestAgentMayRetireAnAgentsStandard(t *testing.T) {
	_, clone := reviewFixture(t)
	rs := clone("one")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	p := filepath.Join(rs.s.Dir, "system", "d.md")
	b, _ := os.ReadFile(p)
	os.WriteFile(p, []byte(strings.Replace(string(b), "type: design_decision", "type: user_standard", 1)), 0o644)
	git(t, rs.s.Dir, "commit", "-q", "-am", "d is a standard")
	git(t, rs.s.Dir, "push", "-q", "origin", "HEAD:main")
	dc := itemFor(t, rs, "d", "c")
	_, errs, err := rs.applyOps(ctx, []op{{ID: dc.ID, A: "d", B: "c", Mark: "right", By: "agent", Evidence: "c is newer"}})
	if err != nil || errs[0] != nil {
		t.Fatalf("apply: %v %v", err, errs)
	}
	if got := itemFor(t, rs, "d", "c"); got.Decision == nil || got.Ask != nil || got.A.SupersededBy != "c" {
		t.Fatalf("the agents' standard was not retired, or became a question: %+v", got)
	}
}

// A label can say only the owner knows the answer. The flag goes through the
// page's request (the same JSON keys app.js sends) into the label file, never
// into the store, and a reopen clears it.
func TestLabelRecordsOwnerOnly(t *testing.T) {
	_, clone := reviewFixture(t)
	rs := clone("one")
	all := rs.items()
	ab, cd := all[0], all[1]
	rs.token = "tok"
	rs.labelFile = filepath.Join(t.TempDir(), "labels.jsonl")
	rs.sampleIDs = map[string]bool{ab.ID: true, cd.ID: true}
	post := func(id, action string, only bool) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"ID": id, "Action": action, "OwnerOnly": only})
		req := httptest.NewRequest(http.MethodPost, "/api/decide", bytes.NewReader(body))
		req.Header.Set("X-Tacit-Token", "tok")
		w := httptest.NewRecorder()
		rs.handleDecide(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s %s: %d %s", id, action, w.Code, w.Body.String())
		}
	}
	post(ab.ID, "left", true)
	post(cd.ID, "skip", true)
	marks, _ := rs.decisions()
	if !marks[ab.ID].OwnerOnly || marks[cd.ID].OwnerOnly {
		t.Fatalf("owner_only: marked %v (want true), skipped %v (want false)", marks[ab.ID].OwnerOnly, marks[cd.ID].OwnerOnly)
	}
	if st := git(t, rs.s.Dir, "status", "--porcelain", "--untracked-files=all"); st != "" {
		t.Fatalf("a label changed the store:\n%s", st)
	}
	post(ab.ID, "reopen", true)
	post(ab.ID, "both", false)
	if marks, _ = rs.decisions(); marks[ab.ID].OwnerOnly {
		t.Fatal("the flag survived a reopen and a relabel without it")
	}
}

// In label mode "neither is true" is a label: it records and leaves the
// store alone (on the store it retires both facts: owner_note_test.go).
func TestBothWrongInLabelModeLeavesTheStore(t *testing.T) {
	_, clone := reviewFixture(t)
	rs := clone("one")
	it := rs.items()[0]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rs.labelFile = filepath.Join(t.TempDir(), "labels.jsonl")
	rs.sampleIDs = map[string]bool{it.ID: true}
	if got := rs.items()[0]; !contains(got.Allowed, "wrong") {
		t.Fatalf("label mode does not offer both wrong: %v", got.Allowed)
	}
	if err := mark(ctx, rs, it.ID, "wrong"); err != nil {
		t.Fatal(err)
	}
	marks, _ := rs.decisions()
	if marks[it.ID] == nil || marks[it.ID].Action != "wrong" {
		t.Fatalf("label = %+v", marks[it.ID])
	}
	if got := rs.items()[0]; got.Mark != "wrong" || got.Stale {
		t.Fatalf("item mark %q stale %v", got.Mark, got.Stale)
	}
	if st := git(t, rs.s.Dir, "status", "--porcelain", "--untracked-files=all"); st != "" {
		t.Fatalf("a label changed the store:\n%s", st)
	}
}
