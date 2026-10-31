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
	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/ownerwords"
	"github.com/agent-sh/tacit/client/internal/platform"
	"github.com/agent-sh/tacit/client/internal/store"
)

// The guards of Closing questions (SPEC.md): what no closure and no moot
// pass may do.

// questionsHome is a home for the questions command against rs's clone: no
// Hermes database, the agent's own Claude Code session.
func questionsHome(t *testing.T, rs *reviewServer) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TACIT_STATE_DIR", "")
	t.Setenv("TACIT_HERMES_DB", "off")
	t.Setenv("TACIT_STORE", rs.s.Dir)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "agent-session")
	return home
}

// queueNoteQuestion queues the reviewer's question about the note the-note,
// which the curated fact made was made from, in home's queue.
func queueNoteQuestion(t *testing.T, home, made string, at time.Time) agentreview.Item {
	t.Helper()
	q := &agentreview.Queue{}
	if !q.Add(agentreview.Item{Note: "inbox/claude@h/the-note.md", Name: "the-note", Fact: made, Desc: "the walrus driver uses port 7781", Harness: "claude", Day: "2026-10-24"}, at) {
		t.Fatal("queue")
	}
	if err := q.Save(agentreview.Path(platform.StateDir(home))); err != nil {
		t.Fatal(err)
	}
	return q.Items[0]
}

func wrongOp(id string) *op {
	return &op{ID: id, Mark: "wrong", By: "agent", Proof: "config", Evidence: "the unit file sets port 7790", Reason: "the note's port is not the one the unit runs"}
}

// The moot pass closes nothing it could not read: on a store whose marker
// does not read every fact would look gone, and a fact whose file does not
// parse is not gone either.
func TestTheMootPassClosesNothingItCouldNotRead(t *testing.T) {
	stubScrump(t)
	ab, stale := pairID(kindContradiction, "a", "b"), pairID(kindStale, "j", "abc1234")
	rs := questionStore(t, map[string]string{"a": "", "b": "", "j": ""},
		"- CONTRADICTION?: [[b]] (system) vs [[a]] (system)\n- stale? [[j]] (system) @abc1234 n=1 since=2026-10-01\n  2026-10-09 abc1234 demo: drop FOO - FOO (gone at HEAD)\n",
		askOn(ab, "a", "b", "Which holds?"), askOn(stale, "j", "abc1234", "Does it still hold?"))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	head := gitIn(t, rs.s.Dir, "rev-parse", "HEAD")
	var lines []string
	out := func(l string) { lines = append(lines, l) }
	marker := filepath.Join(rs.s.Dir, filepath.FromSlash(ledger.MarkerPath))
	good, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(marker, []byte("{not json"), 0o644)
	rs.loadFacts()
	if n, err := closeMootQuestions(ctx, rs, rs.ownerTouched(), nil, false, out); err == nil || n != 0 {
		t.Fatalf("a store whose facts do not read: closed %d (%v)\n%s", n, err, strings.Join(lines, "\n"))
	}
	os.WriteFile(marker, good, 0o644)
	for _, name := range []string{"b", "j"} {
		p := filepath.Join(rs.s.Dir, "system", name+".md")
		b, _ := os.ReadFile(p)
		os.WriteFile(p, []byte(strings.Replace(string(b), "created:", "a_field_of_a_newer_tacit: 1\ncreated:", 1)), 0o644)
	}
	rs.loadFacts()
	if !rs.unread["b"] || !rs.unread["j"] {
		t.Fatalf("fixture: the files should not parse: %v", rs.unread)
	}
	lines = nil
	if n, err := closeMootQuestions(ctx, rs, rs.ownerTouched(), nil, false, out); err != nil || n != 0 {
		t.Fatalf("facts whose files do not parse: closed %d (%v)\n%s", n, err, strings.Join(lines, "\n"))
	}
	if gitIn(t, rs.s.Dir, "rev-parse", "HEAD") != head {
		t.Fatal("the moot pass wrote")
	}
	if ds := decisionsOf(t, rs); ds[ab].Action != "ask" || ds[stale].Action != "ask" {
		t.Fatalf("a question was closed: %+v / %+v", ds[ab], ds[stale])
	}
}

// ownerTurns writes the owner's typed turns into a Claude Code history under
// home: each {session, time, text}, the session's transcript and the prompt
// history.
func ownerTurns(t *testing.T, home string, turns ...[3]string) {
	t.Helper()
	proj := filepath.Join(home, ".claude", "projects", "-home-u-projects")
	os.MkdirAll(proj, 0o755)
	bySession := map[string][]byte{}
	var hist []byte
	for _, tr := range turns {
		b, _ := json.Marshal(map[string]any{"type": "user", "sessionId": tr[0], "timestamp": tr[1], "origin": map[string]any{"kind": "human"}, "promptSource": "typed", "message": map[string]any{"role": "user", "content": tr[2]}})
		bySession[tr[0]] = append(append(bySession[tr[0]], b...), '\n')
		h, _ := json.Marshal(map[string]any{"display": tr[2], "pastedContents": map[string]any{}, "project": "/home/u/projects", "sessionId": tr[0]})
		hist = append(append(hist, h...), '\n')
	}
	for sid, b := range bySession {
		os.WriteFile(filepath.Join(proj, sid+".jsonl"), b, 0o644)
	}
	os.WriteFile(filepath.Join(home, ".claude", "history.jsonl"), hist, 0o644)
}

// The fixture of the words proofs: w holds the owner's words "keep the
// walrus driver on port 7781" (said on 2026-10-01), x is the other side.
const (
	walrusWords   = "keep the walrus driver on port 7781"
	walrusMoved   = "the walrus driver moved to port 7790 last week"
	releaseNotes  = "lets ship the release notes for the parser today"
	supportPortal = "the support portal drives me crazy"
)

func walrusStore(t *testing.T) (*reviewServer, string, string) {
	t.Helper()
	wx := pairID(kindContradiction, "w", "x")
	rs := questionStore(t, map[string]string{"w": "words\n", "x": ""}, "- CONTRADICTION?: [[x]] (system) vs [[w]] (system)\n", askOn(wx, "w", "x", "Do your words still hold?"))
	return rs, questionsHome(t, rs), wx
}

func closeOnWords(id, words string) int {
	return runQuestionsClose([]string{id, "--harness", "claude", "--answer", "keep:x", "--proof", "words", "--words", words, "--evidence", "their turn of 2026-10-29", "--reason", "they moved the driver"})
}

// A words proof retires a fact that holds the owner's older words only on
// their own later words on its subject: a later sentence of theirs about
// something else leaves it, one that only shares word starts with them
// ("portal" and "port", "drives" and "driver") too. The record keeps the
// words, where and when they said them.
func TestAWordsProofRestsOnTheOwnersLaterWordsOnItsSubject(t *testing.T) {
	stubScrump(t)
	rs, home, wx := walrusStore(t)
	sid := strings.TrimPrefix(testOwnerSession, "claude:")
	ownerTurns(t, home, [3]string{sid, "2026-10-01T09:00:00Z", walrusWords},
		[3]string{"owner-later", "2026-10-29T08:00:00Z", supportPortal},
		[3]string{"owner-later", "2026-10-29T09:00:00Z", releaseNotes},
		[3]string{"owner-later", "2026-10-29T10:00:00Z", walrusMoved})
	// Each is a statement of theirs, said after the fact's words.
	corpus, err := loadOwnerCorpus(home, ownerwords.DefaultDigests(home), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{supportPortal, releaseNotes, walrusMoved} {
		if m, ok := corpus.Find(w, "", true); !ok || !m.Clean() || corpus.SaidOn(w, m.Ref, m.TurnHash) != "2026-10-29" {
			t.Fatalf("fixture: %q is no clean statement of 2026-10-29 (%v %+v)", w, ok, m)
		}
	}
	for _, off := range []string{releaseNotes, supportPortal} {
		if c := closeOnWords(wx, off); c != 1 {
			t.Fatalf("their later words on another subject (%q): exit %d", off, c)
		}
		rs.loadFacts()
		if m := rs.facts["w"].Meta; m.SupersededBy != "" || m.Retired != "" {
			t.Fatalf("their words on another subject (%q) retired the fact with their words: %+v", off, m)
		}
	}
	if c := closeOnWords(wx, walrusMoved); c != 0 {
		t.Fatalf("their later words on the driver: exit %d", c)
	}
	rs.loadFacts()
	if m := rs.facts["w"].Meta; m.SupersededBy == "" && m.Retired == "" {
		t.Fatalf("the fact with their older words stands: %+v", m)
	}
	if d := decisionsOf(t, rs)[wx]; d.Proof != "words" || d.Words != walrusMoved || d.WordsFrom != "claude:owner-later" || d.WordsOn != "2026-10-29" {
		t.Fatalf("the record: %+v", d)
	}
}

// Words the owner said again after the ones a proof rests on still stand:
// the proof's day is checked against the last day they said the fact's
// words, in any session.
func TestAWordsProofDoesNotRetireWordsTheySaidAgainLater(t *testing.T) {
	stubScrump(t)
	rs, home, wx := walrusStore(t)
	sid := strings.TrimPrefix(testOwnerSession, "claude:")
	ownerTurns(t, home, [3]string{sid, "2026-10-01T09:00:00Z", walrusWords},
		[3]string{"owner-later", "2026-10-29T10:00:00Z", walrusMoved},
		[3]string{"owner-again", "2026-10-30T10:00:00Z", walrusWords})
	if c := closeOnWords(wx, walrusMoved); c != 1 {
		t.Fatalf("words older than their last saying of the fact's: exit %d", c)
	}
	rs.loadFacts()
	if m := rs.facts["w"].Meta; m.SupersededBy != "" || m.Retired != "" {
		t.Fatalf("the fact with words they said again was retired: %+v", m)
	}
}

// The subject is the fact's too: later words on what its description says
// count, when they share fewer words with its older words.
func TestAWordsProofReadsTheFactsDescription(t *testing.T) {
	stubScrump(t)
	rs, home, wx := walrusStore(t)
	p := filepath.Join(rs.s.Dir, "system", "w.md")
	b, _ := os.ReadFile(p)
	os.WriteFile(p, []byte(strings.Replace(string(b), `description: "fact w"`, `description: "The lab's walrus driver listens on port 7781"`, 1)), 0o644)
	git(t, rs.s.Dir, "commit", "-qam", "w's description")
	git(t, rs.s.Dir, "push", "-q", "origin", "HEAD:main")
	const later = "our walrus driver listens elsewhere now"
	sid := strings.TrimPrefix(testOwnerSession, "claude:")
	ownerTurns(t, home, [3]string{sid, "2026-10-01T09:00:00Z", walrusWords}, [3]string{"owner-later", "2026-10-29T10:00:00Z", later})
	if agentreview.SharesWords(walrusWords, later) {
		t.Fatal("fixture: the later words should share fewer than three words with the fact's words")
	}
	if c := closeOnWords(wx, later); c != 0 {
		t.Fatalf("later words on the fact's description: exit %d", c)
	}
}

// A reviewer question's wrong never retires a fact made from the note that
// the owner kept in a pair or left a note on: the question stays theirs.
func TestAReviewerClosureLeavesWhatTheOwnerKeptOrNoted(t *testing.T) {
	stubScrump(t)
	pair := pairID(kindContradiction, "made-from-note", "other")
	for name, d := range map[string]decision{
		"kept in a pair": {ID: pair, Action: "left", A: "made-from-note", B: "other", At: "2026-10-21T10:00:00Z"},
		"a note on it":   {ID: pair, Action: "note", A: "made-from-note", B: "other", At: "2026-10-21T10:00:00Z", Comment: "this one is right"},
	} {
		rs := questionStore(t, map[string]string{"made-from-note": "origin: the-note\n", "other": ""}, "- CONTRADICTION?: [[other]] (system) vs [[made-from-note]] (system)\n", d)
		home := questionsHome(t, rs)
		item := queueNoteQuestion(t, home, "made-from-note", time.Now())
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		head := gitIn(t, rs.s.Dir, "rev-parse", "HEAD")
		if _, err := closeReviewerQuestion(ctx, rs, wrongOp(item.ID), item); err == nil {
			t.Errorf("%s: the closure landed", name)
		}
		cancel()
		rs.loadFacts()
		if rs.facts["made-from-note"].Meta.Retired != "" || gitIn(t, rs.s.Dir, "rev-parse", "HEAD") != head {
			t.Errorf("%s: the fact was retired: %+v", name, rs.facts["made-from-note"].Meta)
		}
	}
}

// questions close runs the redactor's canary first, as every write path
// does: with a scrump that runs but masks nothing, nothing is closed.
func TestQuestionsCloseRefusesARedactorThatMasksNothing(t *testing.T) {
	kl := pairID(kindSupersedeAsk, "k", "l")
	rs := questionStore(t, map[string]string{"k": "", "l": ""}, "- supersedes? [[l]] may replace [[k]]\n", askOn(kl, "k", "l", "Does l replace k?"))
	questionsHome(t, rs)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "scrump"), []byte("#!/bin/sh\nexec cat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	head := gitIn(t, rs.s.Dir, "rev-parse", "HEAD")
	if c := runQuestionsClose([]string{kl, "--harness", "claude", "--answer", "keep:l", "--proof", "git", "--evidence", "the unit sets password=Zq7Vx9Lm2Kp4", "--reason", "l names what the repo holds now"}); c != 2 {
		t.Fatalf("a redactor that masks nothing: exit %d", c)
	}
	if gitIn(t, rs.s.Dir, "rev-parse", "HEAD") != head || decisionsOf(t, rs)[kl].Action != "ask" {
		t.Fatal("a closure was written with no redaction")
	}
}

// The owner's reopen of a reviewer question is exact: the fact comes back as
// it was, its updated day too, and the question is asked afresh, so the
// queue's 30 days and 3 sessions count from the reopen.
func TestTheOwnersReopenOfAReviewerQuestionIsExact(t *testing.T) {
	stubScrump(t)
	rs := questionStore(t, map[string]string{"made-from-note": "origin: the-note\nupdated: 2026-10-05\n"}, "")
	home := questionsHome(t, rs)
	item := queueNoteQuestion(t, home, "made-from-note", time.Now())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := closeReviewerQuestion(ctx, rs, wrongOp(item.ID), item); err != nil {
		t.Fatal(err)
	}
	// Long queued and asked in 3 sessions before the closure.
	qpath := agentreview.Path(platform.StateDir(home))
	q, err := agentreview.Load(qpath)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-40 * 24 * time.Hour).UTC().Format(time.RFC3339)
	it := q.ByID(item.ID)
	it.Queued = old
	for _, s := range []string{"s1", "s2", "s3"} {
		it.Asks = append(it.Asks, agentreview.Ask{Harness: "claude", Session: s, At: old, Passed: true})
	}
	if err := q.Save(qpath); err != nil {
		t.Fatal(err)
	}
	if _, err := rs.decide(ctx, item.ID, "reopen", false); err != nil {
		t.Fatal(err)
	}
	rs.loadFacts()
	if m := rs.facts["made-from-note"].Meta; m.Retired != "" || m.Updated != "2026-10-05" {
		t.Fatalf("the fact is not back as it was: retired %q updated %q", m.Retired, m.Updated)
	}
	q, _ = agentreview.Load(qpath)
	q.Tidy(time.Now())
	if it := q.ByID(item.ID); it.State != agentreview.StateOpen || len(it.Asks) != 0 {
		t.Fatalf("the reopened question is not asked afresh: %+v", it)
	}
}

// The owner's own answer comes first: one they typed in a session the
// question was put in, that no step has read yet, keeps an agent from
// closing it, and the queue keeps it for the reviewer step.
func TestAnAgentDoesNotCloseAQuestionTheOwnerAnswered(t *testing.T) {
	stubScrump(t)
	rs := questionStore(t, map[string]string{"made-from-note": "origin: the-note\n"}, "")
	home := questionsHome(t, rs)
	item := queueNoteQuestion(t, home, "made-from-note", time.Now())
	tr := filepath.Join(home, "s-ask.jsonl")
	os.WriteFile(tr, []byte(agentTurn("s-ask", "Before we go on, ["+item.Tag()+"]: an agent saved that the walrus driver uses port 7781. Right?")+"\n"+ownerTurn("s-ask", "yes")+"\n"), 0o644)
	qpath := agentreview.Path(platform.StateDir(home))
	q, _ := agentreview.Load(qpath)
	q.ByID(item.ID).Asks = []agentreview.Ask{{Harness: "claude", Session: "s-ask", Transcript: tr, At: time.Now().UTC().Format(time.RFC3339)}}
	if err := q.Save(qpath); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := closeReviewerQuestion(ctx, rs, wrongOp(item.ID), *q.ByID(item.ID)); err == nil {
		t.Fatal("an agent closed a question the owner answered")
	}
	rs.loadFacts()
	if rs.facts["made-from-note"].Meta.Retired != "" {
		t.Fatal("the closure retired the fact")
	}
	q, _ = agentreview.Load(qpath)
	if it := q.ByID(item.ID); it.State != agentreview.StateAnswered || it.Answer == nil || it.Answer.Kind != agentreview.AnswerYes {
		t.Fatalf("the owner's answer was not kept for the reviewer step: %+v", it)
	}
}

// A closure never keeps a retired or replaced fact: the question is moot,
// and the curator's pass closes it.
func TestAClosureRefusesAMootPair(t *testing.T) {
	stubScrump(t)
	ab, cd := pairID(kindContradiction, "a", "b"), pairID(kindContradiction, "c", "d")
	rs := questionStore(t, map[string]string{"a": "retired: 2026-10-27\n", "b": "", "c": "superseded_by: e\n", "d": "", "e": ""},
		"- CONTRADICTION?: [[b]] (system) vs [[a]] (system)\n- CONTRADICTION?: [[d]] (system) vs [[c]] (system)\n",
		askOn(ab, "a", "b", "Which holds?"), askOn(cd, "c", "d", "Which holds?"))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	head := gitIn(t, rs.s.Dir, "rev-parse", "HEAD")
	for _, o := range []op{{ID: ab, A: "a", B: "b", Mark: "left"}, {ID: ab, A: "a", B: "b", Mark: "right"}, {ID: cd, A: "c", B: "d", Mark: "left"}} {
		o.By, o.Proof, o.Evidence, o.Reason = "agent", "file", "README.md:3 says so", "the file says it"
		_, errs, err := rs.applyOps(ctx, []op{o})
		if err == nil && errs[0] == nil {
			t.Errorf("%s %s: a closure landed on a moot pair", o.ID, o.Mark)
		}
	}
	if gitIn(t, rs.s.Dir, "rev-parse", "HEAD") != head {
		t.Fatal("a refused closure wrote")
	}
}

// A stale ask the curator closed as moot asks again once its fact is live
// again: the supersede that made it moot was undone.
func TestAStaleAskComesBackWhenItsFactIsLiveAgain(t *testing.T) {
	stubScrump(t)
	stale := pairID(kindStale, "j", "abc1234")
	rs := questionStore(t, map[string]string{"j": "superseded_by: k\n", "k": ""},
		"- stale? [[j]] (system) @abc1234 n=1 since=2026-10-01\n  2026-10-09 abc1234 demo: drop FOO - FOO (gone at HEAD)\n",
		askOn(stale, "j", "abc1234", "Does it still hold?"))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var lines []string
	out := func(l string) { lines = append(lines, l) }
	if n, err := closeMootQuestions(ctx, rs, rs.ownerTouched(), nil, false, out); err != nil || n != 1 {
		t.Fatalf("closed %d (%v)\n%s", n, err, strings.Join(lines, "\n"))
	}
	p := filepath.Join(rs.s.Dir, "system", "j.md")
	b, _ := os.ReadFile(p)
	os.WriteFile(p, []byte(strings.Replace(string(b), "superseded_by: k\n", "", 1)), 0o644)
	git(t, rs.s.Dir, "commit", "-qam", "the supersede is undone")
	git(t, rs.s.Dir, "push", "-q", "origin", "HEAD:main")
	rs.loadFacts()
	lines = nil
	if _, err := closeMootQuestions(ctx, rs, rs.ownerTouched(), nil, false, out); err != nil {
		t.Fatal(err)
	}
	rs.loadFacts()
	found := false
	for _, it := range rs.staleItems() {
		if it.ID != stale {
			continue
		}
		found = true
		if it.Decision != nil || it.Ask == nil || staleQuestionOf(it) != closesAsk {
			t.Fatalf("the ask did not come back: decision %+v ask %+v\n%s", it.Decision, it.Ask, strings.Join(lines, "\n"))
		}
	}
	if !found {
		t.Fatal("the stale finding is gone")
	}
}

// A dry run of a closure runs every guard on the clone as it is: one that
// would retire a ruling is refused, one that may land says so, and neither
// writes.
func TestADryRunClosureRunsTheGuards(t *testing.T) {
	stubScrump(t)
	ro, kl := pairID(kindRulingChange, "r", "o"), pairID(kindSupersedeAsk, "k", "l")
	rs := questionStore(t, map[string]string{"r": "ruling\n", "o": "", "k": "", "l": ""},
		"- RULING CHANGE? new [[o]] may supersede owner ruling [[r]]\n- supersedes? [[l]] may replace [[k]]\n",
		askOn(ro, "r", "o", "Does your ruling r still hold?"), askOn(kl, "k", "l", "Does l replace k?"))
	questionsHome(t, rs)
	head := gitIn(t, rs.s.Dir, "rev-parse", "HEAD")
	if c := runQuestionsClose([]string{ro, "--harness", "claude", "--answer", "keep:o", "--proof", "file", "--evidence", "README.md:3 names o", "--reason", "the file names the newer one", "--dry-run"}); c != 1 {
		t.Fatalf("a dry run of a closure that retires a ruling: exit %d", c)
	}
	var code int
	out := captureStdout(t, func() {
		code = runQuestionsClose([]string{kl, "--harness", "claude", "--answer", "keep:l", "--proof", "git", "--evidence", "demo@abc1234 moved it", "--reason", "l names what the repo holds now", "--dry-run"})
	})
	if code != 0 || !strings.Contains(out, `"action": "right"`) {
		t.Fatalf("a dry run of a closure that may land: exit %d\n%s", code, out)
	}
	if gitIn(t, rs.s.Dir, "rev-parse", "HEAD") != head || strings.TrimSpace(gitIn(t, rs.s.Dir, "status", "--porcelain")) != "" {
		t.Fatal("a dry run wrote")
	}
	if d := decisionsOf(t, rs); d[kl].Action != "ask" || d[ro].Action != "ask" {
		t.Fatalf("a dry run closed: %+v / %+v", d[kl], d[ro])
	}
}

// tacit questions says which questions no agent closes on its own proof: the
// owner's (a note of theirs on it), which only their own answer closes, and a
// moot one, which code closes.
func TestTheQuestionsListSaysWhoClosesEach(t *testing.T) {
	stubScrump(t)
	kl, ab, uv := pairID(kindSupersedeAsk, "k", "l"), pairID(kindContradiction, "a", "b"), pairID(kindContradiction, "u", "v")
	rs := questionStore(t, map[string]string{"k": "", "l": "", "a": "retired: 2026-10-27\n", "b": "", "u": "", "v": ""},
		"- supersedes? [[l]] may replace [[k]]\n- CONTRADICTION?: [[b]] (system) vs [[a]] (system)\n- CONTRADICTION?: [[v]] (system) vs [[u]] (system)\n",
		askOn(kl, "k", "l", "Does l replace k?"), askOn(ab, "a", "b", "Which holds?"), askOn(uv, "u", "v", "Which holds?"),
		decision{ID: uv, Action: "note", A: "u", B: "v", At: "2026-10-21T10:00:00Z", Comment: "u is right, I think"})
	questionsHome(t, rs)
	var code int
	out := captureStdout(t, func() { code = runQuestions([]string{"--json"}) })
	var rows []map[string]any
	if code != 0 || json.Unmarshal([]byte(out), &rows) != nil || len(rows) != 3 {
		t.Fatalf("exit %d, listed:\n%s", code, out)
	}
	byID := map[string]map[string]any{}
	for _, r := range rows {
		byID[r["id"].(string)] = r
	}
	if r := byID[kl]; r["owners"] != nil || r["moot"] != nil {
		t.Errorf("an open question an agent may close: %v", r)
	}
	if r := byID[ab]; !strings.Contains(asString(r["moot"]), "a is retired") {
		t.Errorf("a moot question: %v", r)
	}
	if r := byID[uv]; !strings.Contains(asString(r["owners"]), "note") || r["words_only"] != true {
		t.Errorf("a question with the owner's note: %v", r)
	}
	out = captureStdout(t, func() { code = runQuestions(nil) })
	if code != 0 || !strings.Contains(out, "moot, `tacit questions moot` closes it") || !strings.Contains(out, "an agent closes it only with their own answer (--proof words)") || !strings.Contains(out, "answers: keep:k, keep:l, both, neither") {
		t.Fatalf("exit %d, the list:\n%s", code, out)
	}
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

// The owner's answer comes first, also once a step read or applied it: a
// question no longer open in the queue takes no closure, and wrong retires
// no fact that carries their verdict on the note.
func TestAReviewerClosureWaitsOnTheOwnersAnswer(t *testing.T) {
	stubScrump(t)
	rs := questionStore(t, map[string]string{"made-from-note": "origin: the-note\n"}, "")
	home := questionsHome(t, rs)
	item := queueNoteQuestion(t, home, "made-from-note", time.Now())
	qpath := agentreview.Path(platform.StateDir(home))
	q, _ := agentreview.Load(qpath)
	q.ByID(item.ID).State = agentreview.StateApplied
	q.ByID(item.ID).Answer = &agentreview.Answer{Kind: agentreview.AnswerYes, From: "claude:s-ask", At: time.Now().UTC().Format(time.RFC3339)}
	if err := q.Save(qpath); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := closeReviewerQuestion(ctx, rs, wrongOp(item.ID), item); err == nil {
		t.Fatal("an agent closed a question whose answer the reviewer step applied")
	}
	// The owner's verdict in the fact (source.review), with the question open.
	q.ByID(item.ID).State, q.ByID(item.ID).Answer = agentreview.StateOpen, nil
	if err := q.Save(qpath); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(rs.s.Dir, "system", "made-from-note.md")
	b, _ := os.ReadFile(p)
	os.WriteFile(p, []byte(strings.Replace(string(b), "  kind: observed\n", "  kind: observed\n  review: confirmed\n", 1)), 0o644)
	git(t, rs.s.Dir, "commit", "-qam", "the owner confirmed the note")
	git(t, rs.s.Dir, "push", "-q", "origin", "HEAD:main")
	if _, err := closeReviewerQuestion(ctx, rs, wrongOp(item.ID), item); err == nil || !strings.Contains(err.Error(), "source.review") {
		t.Fatalf("wrong on a fact the owner confirmed: %v", err)
	}
	rs.loadFacts()
	if rs.facts["made-from-note"].Meta.Retired != "" {
		t.Fatal("the closure retired the fact the owner confirmed")
	}
	if got, _ := agentreview.Load(qpath); got.ByID(item.ID).State != agentreview.StateOpen {
		t.Fatalf("a refused closure left the question closed in the queue: %+v", got.ByID(item.ID))
	}
}

// The curator reopens a stale ask it closed as moot only while its closure
// still stands, under the lock: the owner's answer that landed since stays,
// a retirement of theirs included.
func TestTheCuratorsStaleReopenLeavesTheOwnersAnswer(t *testing.T) {
	stubScrump(t)
	stale := pairID(kindStale, "j", "abc1234")
	rs := questionStore(t, map[string]string{"j": "superseded_by: k\n", "k": ""},
		"- stale? [[j]] (system) @abc1234 n=1 since=2026-10-01\n  2026-10-09 abc1234 demo: drop FOO - FOO (gone at HEAD)\n",
		askOn(stale, "j", "abc1234", "Does it still hold?"))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if n, err := closeMootQuestions(ctx, rs, rs.ownerTouched(), nil, false, func(string) {}); err != nil || n != 1 {
		t.Fatalf("closed %d (%v)", n, err)
	}
	p := filepath.Join(rs.s.Dir, "system", "j.md")
	b, _ := os.ReadFile(p)
	os.WriteFile(p, []byte(strings.Replace(string(b), "superseded_by: k\n", "", 1)), 0o644)
	git(t, rs.s.Dir, "commit", "-qam", "the supersede is undone")
	git(t, rs.s.Dir, "push", "-q", "origin", "HEAD:main")
	rs.loadFacts()
	// The owner answers before the curator's planned reopen lands.
	if _, err := rs.decide(ctx, stale, "reopen", false); err != nil {
		t.Fatal(err)
	}
	if _, err := rs.decide(ctx, stale, "retire", false); err != nil {
		t.Fatal(err)
	}
	rs.loadFacts()
	_, errs, err := rs.applyOps(ctx, []op{{ID: stale, Mark: "reopen", By: "curator"}})
	if err == nil && errs[0] == nil {
		t.Fatal("the curator's reopen undid the owner's answer")
	}
	rs.loadFacts()
	if rs.facts["j"].Meta.Retired == "" {
		t.Fatal("the owner's retirement was undone")
	}
}

// A reviewer question through the command: a dry run checks it and writes
// nothing (no queue saved, no sync, no file touched), and the close lands
// with its record and closes the question in the queue.
func TestAReviewerQuestionThroughTheCommand(t *testing.T) {
	stubScrump(t)
	rs := questionStore(t, map[string]string{"made-from-note": "origin: the-note\n"}, "")
	home := questionsHome(t, rs)
	qpath := agentreview.Path(platform.StateDir(home))
	q := &agentreview.Queue{}
	now := time.Now()
	q.Add(agentreview.Item{Note: "inbox/claude@h/the-note.md", Name: "the-note", Fact: "made-from-note", Desc: "the walrus driver uses port 7781", Harness: "claude", Day: "2026-10-24"}, now)
	q.Add(agentreview.Item{Note: "inbox/claude@h/other-note.md", Name: "other-note", Desc: "the parser runs nightly", Harness: "claude", Day: "2026-10-24"}, now)
	one, two := q.Items[0], q.Items[1]
	// The owner answered the other question; no step has read it yet.
	tr := filepath.Join(home, "s-two.jsonl")
	os.WriteFile(tr, []byte(agentTurn("s-two", "Before we go on, ["+two.Tag()+"]: an agent saved that the parser runs nightly. Right?")+"\n"+ownerTurn("s-two", "yes")+"\n"), 0o644)
	q.Items[1].Asks = []agentreview.Ask{{Harness: "claude", Session: "s-two", Transcript: tr, At: now.UTC().Format(time.RFC3339)}}
	if err := q.Save(qpath); err != nil {
		t.Fatal(err)
	}
	qBefore, _ := os.ReadFile(qpath)
	// Another machine pushed since this clone last synced.
	root := filepath.Dir(rs.s.Dir)
	other := filepath.Join(root, "other")
	git(t, root, "clone", "-q", filepath.Join(root, "store.git"), other)
	os.WriteFile(filepath.Join(other, "system", "zz.md"), []byte(factFile("zz")), 0o644)
	git(t, other, "add", "-A")
	git(t, other, "-c", "user.email=o@example.test", "-c", "user.name=o", "commit", "-q", "-m", "elsewhere")
	git(t, other, "push", "-q", "origin", "HEAD:main")
	head := gitIn(t, rs.s.Dir, "rev-parse", "HEAD")
	fp := filepath.Join(rs.s.Dir, "system", "made-from-note.md")
	old := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	os.Chtimes(fp, old, old)
	args := []string{one.ID, "--harness", "claude", "--answer", "wrong", "--proof", "config", "--evidence", "the unit file sets port 7790", "--reason", "the note's port is not the one the unit runs"}
	var code int
	out := captureStdout(t, func() { code = runQuestionsClose(append(args, "--dry-run")) })
	if code != 0 || !strings.Contains(out, `"action": "wrong"`) {
		t.Fatalf("the dry run: exit %d\n%s", code, out)
	}
	if qAfter, _ := os.ReadFile(qpath); string(qAfter) != string(qBefore) {
		t.Fatal("the dry run wrote the queue")
	}
	if gitIn(t, rs.s.Dir, "rev-parse", "HEAD") != head {
		t.Fatal("the dry run synced the clone")
	}
	if st, _ := os.Stat(fp); !st.ModTime().Equal(old) {
		t.Fatal("the dry run wrote the fact file")
	}
	if code := runQuestionsClose(args); code != 0 {
		t.Fatalf("close: exit %d", code)
	}
	rs.loadFacts()
	if rs.facts["made-from-note"].Meta.Retired == "" {
		t.Fatal("wrong did not retire the fact made from the note")
	}
	if d := decisionsOf(t, rs)[one.ID]; d.Action != "wrong" || d.Closes != closesQuestion || d.Proof != "config" || d.Session != "claude:agent-session" {
		t.Fatalf("the record: %+v", d)
	}
	if got, _ := agentreview.Load(qpath); got.ByID(one.ID).State != agentreview.StateClosed {
		t.Fatalf("the queue: %+v", got.ByID(one.ID))
	}
}

// A reviewer question's wrong retires nothing while a fact file that names
// the note as its origin does not parse: the question stays the owner's.
func TestAReviewerClosureRefusesAFactItCannotRead(t *testing.T) {
	stubScrump(t)
	rs := questionStore(t, map[string]string{"m1": "origin: the-note\n", "m2": "origin: the-note\n"}, "")
	p := filepath.Join(rs.s.Dir, "system", "m2.md")
	b, _ := os.ReadFile(p)
	os.WriteFile(p, []byte(strings.Replace(string(b), "created:", "a_field_of_a_newer_tacit: 1\ncreated:", 1)), 0o644)
	git(t, rs.s.Dir, "commit", "-qam", "m2 from a newer tacit")
	git(t, rs.s.Dir, "push", "-q", "origin", "HEAD:main")
	home := questionsHome(t, rs)
	item := queueNoteQuestion(t, home, "m1", time.Now())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := closeReviewerQuestion(ctx, rs, wrongOp(item.ID), item); err == nil || !strings.Contains(err.Error(), "does not parse") {
		t.Fatalf("wrong with a fact made from the note that does not parse: %v", err)
	}
	rs.loadFacts()
	if rs.facts["m1"].Meta.Retired != "" {
		t.Fatal("m1 was retired")
	}
}

// The owner's reopen puts back only the closure's own retirement: a fact
// retired again since keeps that retirement.
func TestTheOwnersReopenLeavesALaterRetirement(t *testing.T) {
	stubScrump(t)
	rs := questionStore(t, map[string]string{"made-from-note": "origin: the-note\n"}, "")
	home := questionsHome(t, rs)
	item := queueNoteQuestion(t, home, "made-from-note", time.Now())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := closeReviewerQuestion(ctx, rs, wrongOp(item.ID), item); err != nil {
		t.Fatal(err)
	}
	rs.loadFacts()
	p := filepath.Join(rs.s.Dir, rs.facts["made-from-note"].Path)
	b, _ := os.ReadFile(p)
	day := rs.facts["made-from-note"].Meta.Retired
	os.WriteFile(p, []byte(strings.Replace(string(b), "retired: "+day, "retired: 2001-01-02", 1)), 0o644)
	git(t, rs.s.Dir, "commit", "-qam", "retired again, by another")
	git(t, rs.s.Dir, "push", "-q", "origin", "HEAD:main")
	rs.loadFacts()
	if _, err := rs.decide(ctx, item.ID, "reopen", false); err != nil {
		t.Fatal(err)
	}
	rs.loadFacts()
	if got := rs.facts["made-from-note"].Meta.Retired; got != "2001-01-02" {
		t.Fatalf("the later retirement was undone: retired %q", got)
	}
}

// A closed query keeps what it asked, for the owner's page.
func TestAQueryClosureKeepsWhatItAsked(t *testing.T) {
	stubScrump(t)
	mn := pairID(kindContradiction, "m", "n")
	rs := questionStore(t, map[string]string{"m": "conflicts_with: [n]\n", "n": "conflicts_with: [m]\n"}, "- CONTRADICTION?: [[n]] (system) vs [[m]] (system)\n",
		decision{ID: mn, Action: "query", A: "m", B: "n", At: "2026-10-20T10:00:00Z", By: "agent", Evidence: "they name two ports"})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_, errs, err := rs.applyOps(ctx, []op{{ID: mn, A: "m", B: "n", Mark: "both", By: "agent", Proof: "file", Evidence: "hosts.md:4 lists both ports", Reason: "both hold, for two hosts"}})
	if err != nil || errs[0] != nil {
		t.Fatalf("the closure: %v %v", err, errs)
	}
	if d := decisionsOf(t, rs)[mn]; d.Question != "An agent marked these as conflicting and left the call to you: they name two ports" || d.Closes != closesQuery {
		t.Fatalf("the record: %+v", d)
	}
}

// A question this machine closed for a closure the store's log never got (a
// run stopped before its commit) waits for the owner again once that is long
// past any closure's deadline, its asks kept; one closed a minute ago may
// still be on its way and stays. A log that does not read whole changes
// nothing.
func TestAClosureTheStoreNeverGotWaitsAgain(t *testing.T) {
	q := &agentreview.Queue{}
	now := time.Now()
	q.Add(agentreview.Item{Note: "inbox/a@h/one.md", Name: "one"}, now)
	q.Add(agentreview.Item{Note: "inbox/a@h/two.md", Name: "two"}, now)
	old, recent := q.Items[0].ID, q.Items[1].ID
	q.Items[0].Asks = []agentreview.Ask{{Harness: "claude", Session: "s1", At: now.UTC().Format(time.RFC3339)}}
	q.Items[0].Close(agentreview.StateClosed, "wrong (config): r", now.Add(-20*time.Minute))
	q.Items[1].Close(agentreview.StateClosed, "holds (file): r", now.Add(-time.Minute))
	if c, r := syncQuestionClosures(q, map[string]string{}, map[string]bool{}, now); c != 0 || r != 1 {
		t.Fatalf("closed %d reopened %d", c, r)
	}
	if it := q.ByID(old); it.State != agentreview.StateOpen || len(it.Asks) != 1 {
		t.Fatalf("the old close: %+v", it)
	}
	if it := q.ByID(recent); it.State != agentreview.StateClosed {
		t.Fatalf("a closure on its way was undone: %+v", it)
	}
	// A log with a line cut short reads as nothing.
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "review"), 0o755)
	os.WriteFile(filepath.Join(dir, "review", "decisions.jsonl"), []byte(`{"id":"q-aaaaaa","action":"holds","by":"agent","closes":"question"}`+"\n"+`{"id":"q-bbbbbb","act`), 0o644)
	if closed, reopened := questionClosures(dir); closed != nil || reopened != nil {
		t.Fatalf("a cut log read as %v %v", closed, reopened)
	}
	if closed, _ := questionClosures(t.TempDir()); closed != nil {
		t.Fatalf("a missing log read as %v", closed)
	}
	// An empty log, as one read while a write rewrites it, too.
	os.WriteFile(filepath.Join(dir, "review", "decisions.jsonl"), nil, 0o644)
	if closed, reopened := questionClosures(dir); closed != nil || reopened != nil {
		t.Fatalf("an empty log read as %v %v", closed, reopened)
	}
}

// The reviewer step reads the closures in its queue section, after its model
// calls, not when it sets its options: a closure that lands meanwhile is in
// what it reads.
func TestTheReviewerStepReadsClosuresInItsQueueSection(t *testing.T) {
	stubScrump(t)
	rs := questionStore(t, map[string]string{"a": ""}, "")
	var o reviewOpts
	gateOpts(context.Background(), &o, rs.s.Dir, nil, false)
	if o.closuresFrom != rs.s.Dir || o.closed != nil || o.reopened != nil {
		t.Fatalf("the options carry closures read before the model calls: from %q, %v %v", o.closuresFrom, o.closed, o.reopened)
	}
}

// A closure whose deadline runs out before its commit made none: nothing it
// wrote stays in the clone, and the question waits in the queue again. Here
// the deadline runs out in the store's mask, before anything is staged.
func TestAClosureWhoseDeadlineRunsOutKeepsNothing(t *testing.T) {
	stubScrump(t)
	rs := questionStore(t, map[string]string{"made-from-note": "origin: the-note\n"}, "")
	home := questionsHome(t, rs)
	item := queueNoteQuestion(t, home, "made-from-note", time.Now())
	prev := store.Redact
	store.Redact = func(ctx context.Context, _ []string) ([]string, error) { <-ctx.Done(); return nil, ctx.Err() }
	t.Cleanup(func() { store.Redact = prev })
	head := gitIn(t, rs.s.Dir, "rev-parse", "HEAD")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := closeReviewerQuestion(ctx, rs, wrongOp(item.ID), item); err == nil || strings.Contains(err.Error(), "next push") {
		t.Fatalf("a closure past its deadline: %v", err)
	}
	if gitIn(t, rs.s.Dir, "rev-parse", "HEAD") != head {
		t.Fatal("a commit was made")
	}
	if st := strings.TrimSpace(gitIn(t, rs.s.Dir, "status", "--porcelain")); st != "" {
		t.Fatalf("the closure's files stayed in the clone: %q", st)
	}
	if got, _ := agentreview.Load(agentreview.Path(platform.StateDir(home))); got.ByID(item.ID).State != agentreview.StateOpen {
		t.Fatalf("the question did not wait again: %+v", got.ByID(item.ID))
	}
}

// The owner's answer in the rulebook pane comes first too; a store whose
// facts do not read takes no closure, dry or not; and a closure whose commit
// was kept after a failed push leaves the question closed, since the commit
// goes up with the next push.
func TestAReviewerClosureReadsThePaneAndKeepsAKeptCommit(t *testing.T) {
	stubScrump(t)
	rs := questionStore(t, map[string]string{"made-from-note": "origin: the-note\n"}, "")
	home := questionsHome(t, rs)
	item := queueNoteQuestion(t, home, "made-from-note", time.Now())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	qpath := agentreview.Path(platform.StateDir(home))
	// The pane.
	pane := filepath.Join(home, "rulebook.jsonl")
	b, _ := json.Marshal(agentreview.RulebookRow{TS: time.Now().UTC().Format(time.RFC3339), Session: "s-pane", By: "owner", Verdict: agentreview.RulebookDiscarded, Text: "the port moved", Tacit: item.ID})
	os.WriteFile(pane, append(b, '\n'), 0o644)
	t.Setenv("TACIT_RULEBOOK_LOG", pane)
	if _, err := closeReviewerQuestion(ctx, rs, wrongOp(item.ID), item); err == nil {
		t.Fatal("an agent closed a question the owner answered in the rulebook pane")
	}
	if got, _ := agentreview.Load(qpath); got.ByID(item.ID).State != agentreview.StateAnswered || got.ByID(item.ID).Answer.Kind != agentreview.AnswerNo {
		t.Fatalf("the pane's answer was not kept: %+v", got.ByID(item.ID))
	}
	t.Setenv("TACIT_RULEBOOK_LOG", "off")
	q, _ := agentreview.Load(qpath)
	q.ByID(item.ID).State, q.ByID(item.ID).Answer = agentreview.StateOpen, nil
	if err := q.Save(qpath); err != nil {
		t.Fatal(err)
	}
	// A store whose facts do not read.
	marker := filepath.Join(rs.s.Dir, filepath.FromSlash(ledger.MarkerPath))
	good, _ := os.ReadFile(marker)
	os.WriteFile(marker, []byte("{not json"), 0o644)
	for _, dry := range []bool{true, false} {
		o := wrongOp(item.ID)
		o.dry = dry
		if _, err := closeReviewerQuestion(ctx, rs, o, item); err == nil {
			t.Fatalf("a closure on a store whose facts do not read (dry %v)", dry)
		}
	}
	os.WriteFile(marker, good, 0o644)
	git(t, rs.s.Dir, "checkout", "--", ".")
	if got, _ := agentreview.Load(qpath); got.ByID(item.ID).State != agentreview.StateOpen {
		t.Fatalf("a refused closure left the question closed: %+v", got.ByID(item.ID))
	}
	// A push that fails for the machine's key keeps the commit.
	hook := filepath.Join(rs.s.Dir, ".git", "hooks", "pre-push")
	os.WriteFile(hook, []byte("#!/bin/sh\necho \"fatal: Authentication failed for 'https://example.test/store.git/'\" >&2\nexit 1\n"), 0o755)
	head := gitIn(t, rs.s.Dir, "rev-parse", "HEAD")
	if _, err := closeReviewerQuestion(ctx, rs, wrongOp(item.ID), item); err == nil || !strings.Contains(err.Error(), "next push") {
		t.Fatalf("a closure whose push failed: %v", err)
	}
	if gitIn(t, rs.s.Dir, "rev-parse", "HEAD") == head {
		t.Fatal("the closure's commit was not kept")
	}
	if got, _ := agentreview.Load(qpath); got.ByID(item.ID).State != agentreview.StateClosed {
		t.Fatalf("a kept closure put the question back: %+v", got.ByID(item.ID))
	}
}
