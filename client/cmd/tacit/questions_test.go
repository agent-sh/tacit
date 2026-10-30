package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/agentreview"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/platform"
	"github.com/agent-sh/tacit/client/internal/store"
)

// questionStore is a store of the named facts in system/ (each with the
// frontmatter lines extra gives it; "ruling\n" first makes it the owner's
// ruling, "words\n" first gives it words of theirs the quote ledger backs),
// the review file's lines and the decision log's records; it returns the
// clone.
func questionStore(t *testing.T, facts map[string]string, review string, ds ...decision) *reviewServer {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, "store.git")
	git(t, root, "init", "-q", "--bare", "-b", "main", bare)
	seed := filepath.Join(root, "seed")
	git(t, root, "clone", "-q", bare, seed)
	os.MkdirAll(filepath.Join(seed, "system"), 0o755)
	os.MkdirAll(filepath.Join(seed, "review"), 0o755)
	for name, extra := range facts {
		body := factFile(name)
		switch {
		case strings.HasPrefix(extra, "ruling\n"):
			body = strings.Replace(strings.Replace(body, "harness: curator", "harness: human", 1), "kind: observed", "kind: user", 1)
			extra = strings.TrimPrefix(extra, "ruling\n")
		case strings.HasPrefix(extra, "words\n"):
			body = strings.Replace(body, "kind: observed\n", "kind: observed\n  owner_words: keep the walrus driver on port 7781\n  owner_words_from: "+testOwnerSession+"\n", 1)
			body = strings.Replace(body, "body of "+name, "The owner said: keep the walrus driver on port 7781.", 1)
			extra = strings.TrimPrefix(extra, "words\n")
		}
		os.WriteFile(filepath.Join(seed, "system", name+".md"), []byte(strings.Replace(body, "created:", extra+"created:", 1)), 0o644)
	}
	os.WriteFile(filepath.Join(seed, "review", "2026-10-20.md"), []byte(review), 0o644)
	var log []byte
	for _, d := range ds {
		b, _ := json.Marshal(d)
		log = append(append(log, b...), '\n')
	}
	os.WriteFile(filepath.Join(seed, "review", "decisions.jsonl"), log, 0o644)
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

func askOn(id, a, b, q string) decision {
	return decision{ID: id, Action: "ask", A: a, B: b, At: "2026-10-20T10:00:00Z", By: "agent", Question: q, Evidence: "the sources did not settle it"}
}

// A question that went moot is closed by the curator before anything is
// sent, with its reason: a fact of the pair retired, a fact replaced by a
// third (moved to the live facts), the fact of a stale finding retired. One
// that still asks something stays, and so does the owner's: one they left a
// note on, or reopened. A second run closes nothing.
func TestAMootQuestionIsClosedWithItsReason(t *testing.T) {
	stubScrump(t)
	ab, cd, fg, hi := pairID(kindContradiction, "a", "b"), pairID(kindSupersedeAsk, "c", "d"), pairID(kindContradiction, "f", "g"), pairID(kindContradiction, "h", "i")
	stale := pairID(kindStale, "j", "abc1234")
	rs := questionStore(t, map[string]string{"a": "retired: 2026-10-27\n", "b": "", "c": "superseded_by: e\n", "d": "", "e": "", "f": "", "g": "", "h": "retired: 2026-10-27\n", "i": "", "j": "retired: 2026-10-27\n"},
		"- CONTRADICTION?: [[b]] (system) vs [[a]] (system)\n- supersedes? [[d]] may replace [[c]]\n- CONTRADICTION?: [[g]] (system) vs [[f]] (system)\n- CONTRADICTION?: [[i]] (system) vs [[h]] (system)\n- stale? [[j]] (system) @abc1234 n=1 since=2026-10-01\n  2026-10-09 abc1234 demo: drop FOO - FOO (gone at HEAD)\n",
		askOn(ab, "a", "b", "Retire both?"),
		decision{ID: cd, Action: "query", A: "c", B: "d", At: "2026-10-20T10:00:00Z", By: "agent", Evidence: "they conflict"},
		askOn(fg, "f", "g", "Which holds?"),
		askOn(hi, "h", "i", "Which holds?"),
		decision{ID: hi, Action: "note", A: "h", B: "i", At: "2026-10-21T10:00:00Z", Comment: "my note on these"},
		askOn(stale, "j", "abc1234", "Does it still hold?"),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var lines []string
	out := func(l string) { lines = append(lines, l) }
	head := gitIn(t, rs.s.Dir, "rev-parse", "HEAD")
	if n, err := closeMootQuestions(ctx, rs, rs.ownerTouched(), nil, true, out); err != nil || n != 0 || gitIn(t, rs.s.Dir, "rev-parse", "HEAD") != head {
		t.Fatalf("a dry run wrote: %d %v", n, err)
	}
	if got := strings.Join(lines, "\n"); !strings.Contains(got, "would close "+ab) || !strings.Contains(got, "would close "+cd) || !strings.Contains(got, "would close "+stale) || strings.Contains(got, fg) || strings.Contains(got, hi) {
		t.Fatalf("the dry run would close:\n%s", got)
	}
	lines = nil
	n, err := closeMootQuestions(ctx, rs, rs.ownerTouched(), nil, false, out)
	if err != nil || n != 3 {
		t.Fatalf("closed %d (%v):\n%s", n, err, strings.Join(lines, "\n"))
	}
	ds := decisionsOf(t, rs)
	for id, want := range map[string]string{ab: "moot: a is retired (2026-10-27) and no fact replaces it; b stands", cd: "re-pointed to e / d", stale: "moot: j is retired (2026-10-27)"} {
		d := ds[id]
		if d.Action != mootMark || d.By != "curator" || d.Closes == "" || !strings.Contains(d.Evidence, want) {
			t.Errorf("%s: %+v, want a moot closure saying %q", id, d, want)
		}
	}
	if ds[cd].Closes != closesQuery || ds[ab].Closes != closesAsk || ds[cd].Repointed != pairID(kindSupersedeAsk, "e", "d") {
		t.Errorf("what the closures say they closed: %+v / %+v", ds[ab], ds[cd])
	}
	if ds[fg].Action != "ask" || ds[hi].Action != "note" {
		t.Errorf("an open question, or the owner's, was closed: %+v / %+v", ds[fg], ds[hi])
	}
	if b, _ := os.ReadFile(filepath.Join(rs.s.Dir, repointedFile(time.Now()))); !strings.Contains(string(b), "[[d]] may replace [[e]]") {
		t.Errorf("the replaced fact's question was not moved to the live facts:\n%s", b)
	}
	rs.loadFacts()
	for _, it := range rs.items() {
		if (it.ID == ab || it.ID == cd) && it.NeedsOwner {
			t.Errorf("%s still waits on the owner", it.ID)
		}
	}
	lines = nil
	if n, err := closeMootQuestions(ctx, rs, rs.ownerTouched(), nil, false, out); err != nil || n != 0 {
		t.Fatalf("a second run closed %d (%v)", n, err)
	}
	// The owner reopens one: it waits for them again and is never closed by
	// code after that.
	if _, err := rs.decide(ctx, ab, "reopen", false); err != nil {
		t.Fatal(err)
	}
	rs.loadFacts()
	if it := itemFor(t, rs, "a", "b"); !it.NeedsOwner || it.Decision != nil {
		t.Fatalf("reopened: %+v", it)
	}
	if n, _ := closeMootQuestions(ctx, rs, rs.ownerTouched(), nil, false, out); n != 0 {
		t.Fatal("the owner's reopened question was closed again")
	}
}

// An agent closes a question it can prove, with its evidence and reason:
// keep one fact, both, or neither. The owner sees the closure and reopens
// it, which puts the facts back as they were; after that it is theirs.
func TestAnAgentClosesAQuestionItProves(t *testing.T) {
	stubScrump(t)
	kl, mn, pq := pairID(kindSupersedeAsk, "k", "l"), pairID(kindContradiction, "m", "n"), pairID(kindContradiction, "p", "q")
	rs := questionStore(t, map[string]string{"k": "", "l": "", "m": "conflicts_with: [n]\n", "n": "conflicts_with: [m]\n", "p": "", "q": ""},
		"- supersedes? [[l]] may replace [[k]]\n- CONTRADICTION?: [[n]] (system) vs [[m]] (system)\n- CONTRADICTION?: [[q]] (system) vs [[p]] (system)\n",
		askOn(kl, "k", "l", "Does l replace k?"),
		decision{ID: mn, Action: "query", A: "m", B: "n", At: "2026-10-20T10:00:00Z", By: "agent", Evidence: "they conflict"},
		askOn(pq, "p", "q", "Retire both?"),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	closure := func(id, a, b, mark string) op {
		return op{ID: id, A: a, B: b, Mark: mark, By: "agent", Proof: "git", Evidence: "demo@abc1234 removed the old path", Reason: "the newer fact matches the repository now", Session: "claude:s1"}
	}
	// What every closure must carry.
	for _, bad := range []op{{ID: kl, A: "k", B: "l", Mark: "right", By: "agent", Proof: "git", Reason: "r"}, {ID: kl, A: "k", B: "l", Mark: "right", By: "agent", Proof: "hunch", Evidence: "e", Reason: "r"}} {
		_, errs, err := rs.applyOps(ctx, []op{bad})
		if err == nil && errs[0] == nil {
			t.Fatalf("a closure without its evidence, reason or a known proof landed: %+v", bad)
		}
	}
	_, errs, err := rs.applyOps(ctx, []op{closure(kl, "k", "l", "right"), closure(mn, "m", "n", "both"), closure(pq, "p", "q", "wrong")})
	if err != nil || errs[0] != nil || errs[1] != nil || errs[2] != nil {
		t.Fatalf("closures: %v %v", err, errs)
	}
	rs.loadFacts()
	if rs.facts["k"].Meta.SupersededBy != "l" || len(rs.facts["m"].Meta.ConflictsWith) != 0 || rs.facts["p"].Meta.Retired == "" || rs.facts["q"].Meta.Retired == "" {
		t.Fatalf("the closures' answers are not in the store: k %+v m %+v p %+v q %+v", rs.facts["k"].Meta, rs.facts["m"].Meta, rs.facts["p"].Meta, rs.facts["q"].Meta)
	}
	ds := decisionsOf(t, rs)
	if d := ds[kl]; d.By != "agent" || d.Proof != "git" || d.Reason == "" || d.Evidence == "" || d.Closes != closesAsk || d.Session != "claude:s1" || d.Before == nil {
		t.Fatalf("the closure's record: %+v", d)
	}
	if ds[mn].Closes != closesQuery || ds[pq].Action != "wrong" {
		t.Fatalf("records: %+v %+v", ds[mn], ds[pq])
	}
	for _, it := range rs.items() {
		if it.NeedsOwner {
			t.Errorf("%s still waits on the owner", it.ID)
		}
	}
	// The owner reopens the neither: both facts come back; the question waits
	// for them again, and an agent may not close it any more.
	if _, err := rs.decide(ctx, pq, "reopen", false); err != nil {
		t.Fatal(err)
	}
	rs.loadFacts()
	if rs.facts["p"].Meta.Retired != "" || rs.facts["q"].Meta.Retired != "" {
		t.Fatalf("reopen did not put the facts back: %+v %+v", rs.facts["p"].Meta, rs.facts["q"].Meta)
	}
	if it := itemFor(t, rs, "p", "q"); !it.NeedsOwner {
		t.Fatal("the reopened question does not wait on the owner")
	}
	_, errs, err = rs.applyOps(ctx, []op{closure(pq, "p", "q", "wrong")})
	if err == nil && errs[0] == nil {
		t.Fatal("an agent closed a question the owner reopened")
	}
}

// A closure never changes a ruling and never retires a fact that holds the
// owner's words but on their own later words; it does not touch a finding
// with no question, and the owner's note keeps a question theirs.
func TestAClosureLeavesRulingsTheOwnersWordsAndNotes(t *testing.T) {
	stubScrump(t)
	ro, wx, yz, uv := pairID(kindRulingChange, "r", "o"), pairID(kindContradiction, "w", "x"), pairID(kindContradiction, "y", "z"), pairID(kindContradiction, "u", "v")
	rs := questionStore(t, map[string]string{"r": "ruling\n", "o": "", "w": "words\n", "x": "", "y": "", "z": "", "u": "", "v": ""},
		"- RULING CHANGE? new [[o]] may supersede owner ruling [[r]]\n- CONTRADICTION?: [[x]] (system) vs [[w]] (system)\n- CONTRADICTION?: [[z]] (system) vs [[y]] (system)\n- CONTRADICTION?: [[v]] (system) vs [[u]] (system)\n",
		askOn(ro, "r", "o", "Does your ruling r still hold?"),
		askOn(wx, "w", "x", "Do your words still hold?"),
		askOn(uv, "u", "v", "Which holds?"),
		decision{ID: uv, Action: "note", A: "u", B: "v", At: "2026-10-21T10:00:00Z", Comment: "u is right, I think"},
	)
	if !isRuling(rs.facts["r"]) || fact.OwnerWordsOf(rs.facts["w"]) == "" {
		t.Fatalf("fixture: r a ruling %v, w with words %q", isRuling(rs.facts["r"]), fact.OwnerWordsOf(rs.facts["w"]))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	try := func(o op) error {
		o.By, o.Proof, o.Evidence, o.Reason = "agent", "file", "README.md:3 says so", "the file says the newer one"
		_, errs, err := rs.applyOps(ctx, []op{o})
		if err != nil {
			return err
		}
		return errs[0]
	}
	head := gitIn(t, rs.s.Dir, "rev-parse", "HEAD")
	for name, o := range map[string]op{
		"retire the ruling":    {ID: ro, A: "r", B: "o", Mark: "right"},
		"neither on a ruling":  {ID: ro, A: "r", B: "o", Mark: "wrong"},
		"retire their words":   {ID: wx, A: "w", B: "x", Mark: "right"},
		"no question on it":    {ID: yz, A: "y", B: "z", Mark: "both"},
		"the owner's own note": {ID: uv, A: "u", B: "v", Mark: "left"},
	} {
		if err := try(o); err == nil {
			t.Errorf("%s: the closure landed", name)
		}
	}
	if gitIn(t, rs.s.Dir, "rev-parse", "HEAD") != head {
		t.Fatal("a refused closure wrote")
	}
	// Keeping the ruling over the agent's fact changes nothing in the ruling:
	// that closure lands.
	if err := try(op{ID: ro, A: "r", B: "o", Mark: "left"}); err != nil {
		t.Fatalf("keeping the ruling: %v", err)
	}
	rs.loadFacts()
	if rs.facts["o"].Meta.SupersededBy != "r" || rs.facts["r"].Meta.SupersededBy != "" {
		t.Fatalf("r %+v o %+v", rs.facts["r"].Meta, rs.facts["o"].Meta)
	}
	// Their own later words let a closure retire the fact with their older words.
	o := op{ID: wx, A: "w", B: "x", Mark: "right", Words: "the walrus driver moved to port 7790", WordsFrom: testOwnerSession, WordsOn: "2026-10-29", wordsRetire: func(f *fact.Fact) bool { return f.Meta.Name == "w" }}
	o.By, o.Proof, o.Evidence, o.Reason = "agent", "words", "their turn of 2026-10-29", "they moved it later"
	if _, errs, err := rs.applyOps(ctx, []op{o}); err != nil || errs[0] != nil {
		t.Fatalf("a words proof: %v %v", err, errs)
	}
	if d := decisionsOf(t, rs)[wx]; d.Words == "" || d.WordsFrom != testOwnerSession || d.Proof != "words" {
		t.Fatalf("the words it rests on are not recorded: %+v", d)
	}
}

// A one-fact question: an agent closes its own ask on a stale finding as
// still true or outdated, the rotation's check? as still true only, and
// never retires a ruling.
func TestAnAgentClosesAOneFactQuestion(t *testing.T) {
	stubScrump(t)
	sj, sr, cc := pairID(kindStale, "j", "abc1234"), pairID(kindStale, "rr", "abc1234"), pairID(kindCheck, "cc", "2026-10-21")
	rs := questionStore(t, map[string]string{"j": "", "rr": "ruling\n", "cc": "ruling\n"},
		"- stale? [[j]] (system) @abc1234 n=1 since=2026-10-01\n  2026-10-09 abc1234 demo: drop FOO - FOO (gone at HEAD)\n- stale? [[rr]] (system) @abc1234 n=1 since=2026-10-01\n  2026-10-09 abc1234 demo: drop FOO - FOO (gone at HEAD)\n- check? [[cc]] (system) outcome=stale on=2026-10-21\n  The curator reads it as: the path it names moved\n",
		askOn(sj, "j", "abc1234", "Is it still true?"), askOn(sr, "rr", "abc1234", "Does your ruling still hold?"))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c := func(id, mark string) error {
		_, errs, err := rs.applyOps(ctx, []op{{ID: id, Mark: mark, By: "agent", Proof: "file", Evidence: "demo/README.md:4 still names FOO", Reason: "the name it holds is still there"}})
		if err != nil {
			return err
		}
		return errs[0]
	}
	if err := c(sr, "retire"); err == nil {
		t.Fatal("a closure retired a ruling")
	}
	if err := c(sj, "retire"); err != nil {
		t.Fatal(err)
	}
	if err := c(sr, "current"); err != nil {
		t.Fatalf("still true on a ruling: %v", err)
	}
	// The rotation's check? on the owner's fact: still true only.
	if err := c(cc, "retire"); err == nil {
		t.Fatal("a closure retired the owner's fact on the rotation's question")
	}
	if err := c(cc, "current"); err != nil {
		t.Fatalf("still true on the rotation's question: %v", err)
	}
	rs.loadFacts()
	if rs.facts["j"].Meta.Retired == "" || rs.facts["rr"].Meta.Retired != "" || rs.facts["cc"].Meta.Retired != "" {
		t.Fatalf("j %+v rr %+v cc %+v", rs.facts["j"].Meta, rs.facts["rr"].Meta, rs.facts["cc"].Meta)
	}
	checks := rs.checkItems()
	if len(checks) != 1 || checks[0].NeedsOwner || checks[0].Decision == nil || checks[0].Decision.Closes != closesCheck || checks[0].Decision.Action != "current" {
		t.Fatalf("the rotation's question: %+v", checks)
	}
	for _, it := range rs.staleItems() {
		if it.NeedsOwner {
			t.Errorf("%s still waits on the owner", it.ID)
		}
		if it.Decision == nil || it.Decision.Closes != closesAsk {
			t.Errorf("%s: %+v", it.ID, it.Decision)
		}
	}
}

// The reviewer's question about an agent's note: an agent closes it with its
// proof (wrong retires the facts made from the note, never a ruling), the
// owner's page lists it with the closure, and the owner's reopen brings the
// facts back and puts it in the queue at their prompt again, for good.
func TestAnAgentClosesAReviewerQuestionAndTheOwnerReopensIt(t *testing.T) {
	stubScrump(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TACIT_STATE_DIR", "")
	rs := questionStore(t, map[string]string{"made-from-note": "origin: the-note\n", "other": ""}, "")
	note := "inbox/claude@h/the-note.md"
	os.MkdirAll(filepath.Join(rs.s.Dir, "inbox", "claude@h"), 0o755)
	os.WriteFile(filepath.Join(rs.s.Dir, filepath.FromSlash(note)), []byte("---\nname: the-note\ndescription: the walrus driver uses port 7781\ntype: machine_fact\nscope: system\nsource:\n  harness: claude\n  host: h\n  kind: observed\ncreated: 2026-10-24\n---\n\nThe walrus driver uses port 7781.\n"), 0o644)
	git(t, rs.s.Dir, "add", "-A")
	git(t, rs.s.Dir, "commit", "-q", "-m", "note")
	git(t, rs.s.Dir, "push", "-q", "origin", "HEAD:main")
	rs.loadFacts()
	qpath := agentreview.Path(platform.StateDir(home))
	q := &agentreview.Queue{}
	it := agentreview.Item{Note: note, Name: "the-note", Fact: "made-from-note", Desc: "the walrus driver uses port 7781", Harness: "claude", Day: "2026-10-24"}
	if !q.Add(it, time.Now()) {
		t.Fatal("queue")
	}
	if err := q.Save(qpath); err != nil {
		t.Fatal(err)
	}
	item := q.Items[0]
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	o := &op{ID: item.ID, Mark: "wrong", By: "agent", Proof: "config", Evidence: "the unit file sets port 7790", Reason: "the note's port is not the one the unit runs", Session: "claude:s2"}
	if _, err := closeReviewerQuestion(ctx, rs, o, item); err != nil {
		t.Fatal(err)
	}
	rs.loadFacts()
	if rs.facts["made-from-note"].Meta.Retired == "" || rs.facts["other"].Meta.Retired != "" {
		t.Fatalf("wrong retires the facts made from the note only: %+v / %+v", rs.facts["made-from-note"].Meta, rs.facts["other"].Meta)
	}
	if got, _ := agentreview.Load(qpath); got.ByID(item.ID).State != agentreview.StateClosed {
		t.Fatalf("the queue: %+v", got.ByID(item.ID))
	}
	if gitIn(t, rs.s.Dir, "log", "-1", "--format=%s") != "review: close "+item.ID+" (wrong, config proof) by agent" {
		t.Fatalf("commit: %s", gitIn(t, rs.s.Dir, "log", "-1", "--format=%s"))
	}
	qs := rs.questionItems()
	if len(qs) != 1 || qs[0].ID != item.ID || qs[0].NeedsOwner || qs[0].Decision.Proof != "config" || !strings.Contains(qs[0].Question, "the walrus driver uses port 7781") {
		t.Fatalf("the owner's page lists: %+v", qs)
	}
	if _, err := closeReviewerQuestion(ctx, rs, o, item); err == nil {
		t.Fatal("a closed question was closed twice")
	}
	// The owner reopens it.
	if _, err := rs.decide(ctx, item.ID, "reopen", false); err != nil {
		t.Fatal(err)
	}
	rs.loadFacts()
	if rs.facts["made-from-note"].Meta.Retired != "" {
		t.Fatal("reopen did not bring the fact back")
	}
	if got, _ := agentreview.Load(qpath); got.ByID(item.ID).State != agentreview.StateOpen {
		t.Fatalf("the reopened question is not open: %+v", got.ByID(item.ID))
	}
	if len(rs.questionItems()) != 0 {
		t.Fatal("a reopened question is still listed as closed")
	}
	if _, err := closeReviewerQuestion(ctx, rs, o, item); err == nil {
		t.Fatal("an agent closed a question the owner reopened")
	}
	// A ruling made from a note is never retired by a closure.
	rs2 := questionStore(t, map[string]string{"ruled": "ruling\norigin: the-note\n"}, "")
	if _, err := closeReviewerQuestion(ctx, rs2, &op{ID: item.ID, Mark: "wrong", By: "agent", Proof: "file", Evidence: "e", Reason: "r"}, item); err == nil {
		t.Fatal("a closure retired a ruling made from the note")
	}
}

// The reviewer step keeps this machine's queue in step with the store: a
// question an agent closed elsewhere closes here, one the owner reopened
// opens again.
func TestTheQueueFollowsTheClosuresInTheStore(t *testing.T) {
	q := &agentreview.Queue{}
	now := time.Now()
	q.Add(agentreview.Item{Note: "inbox/a@h/one.md", Name: "one"}, now)
	q.Add(agentreview.Item{Note: "inbox/a@h/two.md", Name: "two"}, now)
	one, two := q.Items[0].ID, q.Items[1].ID
	q.Items[1].Close(agentreview.StateClosed, "holds (git): x", now)
	if c, r := syncQuestionClosures(q, nil, nil, now); c+r != 0 {
		t.Fatal("nothing read, something changed")
	}
	c, r := syncQuestionClosures(q, map[string]string{one: "moot (gone): the tree is gone"}, map[string]bool{two: true}, now)
	if c != 1 || r != 1 || q.ByID(one).State != agentreview.StateClosed || q.ByID(two).State != agentreview.StateOpen {
		t.Fatalf("closed %d reopened %d: %+v", c, r, q.Items)
	}
}

// tacit verify closes the questions that went moot before it sends
// anything, and sends no question to the model.
func TestVerifyClosesMootQuestionsBeforeItSends(t *testing.T) {
	stubScrump(t)
	ab := pairID(kindContradiction, "a", "b")
	rs := questionStore(t, map[string]string{"a": "retired: 2026-10-27\n", "b": "", "f": "", "g": ""},
		"- CONTRADICTION?: [[b]] (system) vs [[a]] (system)\n- CONTRADICTION?: [[g]] (system) vs [[f]] (system)\n",
		askOn(ab, "a", "b", "Retire both?"))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	state, _ := sandboxVerify(t, rs.s.Dir)
	sent := map[string]bool{}
	call := func(_ context.Context, _, prompt, _ string, _ float64) ([]verifyVerdict, string, float64, error) {
		for _, it := range rs.items() {
			if strings.Contains(prompt, `"ID":"`+it.ID+`"`) {
				sent[it.ID] = true
			}
		}
		return nil, "raw", 0.01, nil
	}
	r := verifyRun{model: "m", max: 50, per: 50, budget: 1, cwd: t.TempDir(), stateDir: filepath.Join(state, "verify")}
	if _, _, err := r.run(ctx, rs.s, call); err != nil {
		t.Fatal(err)
	}
	if d := decisionsOf(t, rs)[ab]; d.Action != mootMark || d.Closes != closesAsk {
		t.Fatalf("the moot question: %+v", d)
	}
	if sent[ab] {
		t.Fatal("a question went to the model")
	}
	if !sent[pairID(kindContradiction, "f", "g")] {
		t.Fatal("the open finding with no question was not sent")
	}
}

// `tacit questions` lists what waits for the owner, and `tacit questions
// close` takes an agent's proof: the answer named by what it keeps, the
// session it ran in; a words proof with no owner history here fails closed.
func TestTheQuestionsCommand(t *testing.T) {
	stubScrump(t)
	kl := pairID(kindSupersedeAsk, "k", "l")
	rs := questionStore(t, map[string]string{"k": "", "l": ""}, "- supersedes? [[l]] may replace [[k]]\n", askOn(kl, "k", "l", "Does l replace k?"))
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TACIT_STATE_DIR", "")
	t.Setenv("TACIT_STORE", rs.s.Dir)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "abc123")
	var code int
	out := captureStdout(t, func() { code = runQuestions([]string{"--json"}) })
	var rows []questionRow
	if code != 0 || json.Unmarshal([]byte(out), &rows) != nil || len(rows) != 1 || rows[0].ID != kl || rows[0].Closes != closesAsk || rows[0].Question != "Does l replace k?" {
		t.Fatalf("exit %d, listed:\n%s", code, out)
	}
	if c := runQuestionsClose([]string{kl, "--harness", "claude", "--answer", "keep:nope", "--proof", "git", "--evidence", "e", "--reason", "r"}); c != 2 {
		t.Fatalf("an answer naming neither fact: exit %d", c)
	}
	if c := runQuestionsClose([]string{kl, "--harness", "claude", "--answer", "keep:l", "--proof", "words", "--words", "l is the current one now", "--evidence", "e", "--reason", "r"}); c != 1 {
		t.Fatalf("a words proof with no owner history: exit %d", c)
	}
	if c := runQuestionsClose([]string{kl, "--harness", "claude", "--answer", "keep:l", "--proof", "git", "--evidence", "demo@abc1234 moved it", "--reason", "l names what the repo holds now"}); c != 0 {
		t.Fatalf("close: exit %d", c)
	}
	d := decisionsOf(t, rs)[kl]
	if d.Action != "right" || d.By != "agent" || d.Session != "claude:abc123" || d.Proof != "git" || d.Closes != closesAsk {
		t.Fatalf("the closure: %+v", d)
	}
	out = captureStdout(t, func() { code = runQuestions(nil) })
	if code != 0 || !strings.Contains(out, "no questions waiting") {
		t.Fatalf("after the closure: exit %d\n%s", code, out)
	}
}
