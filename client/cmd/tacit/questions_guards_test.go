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

// A words proof retires a fact that holds the owner's older words only on
// their own later words on its subject; a later sentence of theirs about
// something else leaves it. The record keeps the words, where and when they
// said them.
func TestAWordsProofRestsOnTheOwnersLaterWordsOnItsSubject(t *testing.T) {
	stubScrump(t)
	wx := pairID(kindContradiction, "w", "x")
	rs := questionStore(t, map[string]string{"w": "words\n", "x": ""}, "- CONTRADICTION?: [[x]] (system) vs [[w]] (system)\n", askOn(wx, "w", "x", "Do your words still hold?"))
	home := questionsHome(t, rs)
	proj := filepath.Join(home, ".claude", "projects", "-home-u-projects")
	os.MkdirAll(proj, 0o755)
	turn := func(session, at, text string) string {
		b, _ := json.Marshal(map[string]any{"type": "user", "sessionId": session, "timestamp": at, "origin": map[string]any{"kind": "human"}, "promptSource": "typed", "message": map[string]any{"role": "user", "content": text}})
		return string(b)
	}
	const offSubject, onSubject = "lets ship the release notes for the parser today", "the walrus driver moved to port 7790 last week"
	sid := strings.TrimPrefix(testOwnerSession, "claude:")
	os.WriteFile(filepath.Join(proj, sid+".jsonl"), []byte(turn(sid, "2026-10-01T09:00:00Z", "keep the walrus driver on port 7781")+"\n"), 0o644)
	os.WriteFile(filepath.Join(proj, "owner-later.jsonl"), []byte(turn("owner-later", "2026-10-29T09:00:00Z", offSubject)+"\n"+turn("owner-later", "2026-10-29T10:00:00Z", onSubject)+"\n"), 0o644)
	var hist []byte
	for _, h := range [][2]string{{sid, "keep the walrus driver on port 7781"}, {"owner-later", offSubject}, {"owner-later", onSubject}} {
		b, _ := json.Marshal(map[string]any{"display": h[1], "pastedContents": map[string]any{}, "project": "/home/u/projects", "sessionId": h[0]})
		hist = append(append(hist, b...), '\n')
	}
	os.WriteFile(filepath.Join(home, ".claude", "history.jsonl"), hist, 0o644)
	// Both are statements of theirs, said after the fact's words.
	corpus, err := loadOwnerCorpus(home, ownerwords.DefaultDigests(home), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{offSubject, onSubject} {
		if m, ok := corpus.Find(w, "", true); !ok || !m.Clean() || corpus.SaidOn(w, m.Ref, m.TurnHash) != "2026-10-29" {
			t.Fatalf("fixture: %q is no clean statement of 2026-10-29 (%v %+v)", w, ok, m)
		}
	}
	closeWith := func(words string) int {
		return runQuestionsClose([]string{wx, "--harness", "claude", "--answer", "keep:x", "--proof", "words", "--words", words, "--evidence", "their turn of 2026-10-29", "--reason", "they moved the driver"})
	}
	if c := closeWith(offSubject); c != 1 {
		t.Fatalf("their later words on another subject: exit %d", c)
	}
	rs.loadFacts()
	if m := rs.facts["w"].Meta; m.SupersededBy != "" || m.Retired != "" {
		t.Fatalf("their words on another subject retired the fact with their words: %+v", m)
	}
	if c := closeWith(onSubject); c != 0 {
		t.Fatalf("their later words on the driver: exit %d", c)
	}
	rs.loadFacts()
	if m := rs.facts["w"].Meta; m.SupersededBy == "" && m.Retired == "" {
		t.Fatalf("the fact with their older words stands: %+v", m)
	}
	if d := decisionsOf(t, rs)[wx]; d.Proof != "words" || d.Words != onSubject || d.WordsFrom != "claude:owner-later" || d.WordsOn != "2026-10-29" {
		t.Fatalf("the record: %+v", d)
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

// tacit questions says which questions no agent closes: the owner's (a note
// of theirs on it) and a moot one, which code closes.
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
	if r := byID[uv]; !strings.Contains(asString(r["owners"]), "note") {
		t.Errorf("a question with the owner's note: %v", r)
	}
	out = captureStdout(t, func() { code = runQuestions(nil) })
	if code != 0 || !strings.Contains(out, "moot, `tacit questions moot` closes it") || !strings.Contains(out, "the owner's, no agent closes it") || !strings.Contains(out, "answers: keep:k, keep:l, both, neither") {
		t.Fatalf("exit %d, the list:\n%s", code, out)
	}
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}
