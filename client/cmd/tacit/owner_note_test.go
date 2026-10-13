package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func needRedaction(t *testing.T) {
	t.Helper()
	requireScrump(t)
}

// pushReview adds review lines to the store through a clone, as the curator would.
func pushReview(t *testing.T, rs *reviewServer, lines string) {
	t.Helper()
	p := filepath.Join(rs.s.Dir, "review", "2026-10-11.md")
	if err := os.WriteFile(p, []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, rs.s.Dir, "add", "review/2026-10-11.md")
	git(t, rs.s.Dir, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "review")
	git(t, rs.s.Dir, "push", "-q", "origin", "HEAD:main")
	rs.loadFacts()
}

func lastDecision(t *testing.T, rs *reviewServer) decision {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(rs.s.Dir, "review", "decisions.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	ls := strings.Split(strings.TrimSpace(string(b)), "\n")
	var d decision
	if err := json.Unmarshal([]byte(ls[len(ls)-1]), &d); err != nil {
		t.Fatal(err)
	}
	return d
}

// Neither is true retires both facts in one named owner commit, records both
// as they were, and reopen puts both back byte for byte.
func TestNeitherIsTrueRetiresBothAndReopenIsExact(t *testing.T) {
	_, clone := reviewFixture(t)
	rs := clone("one")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	origA, origB := read(t, rs, "a"), read(t, rs, "b")
	it := itemFor(t, rs, "a", "b")
	if !contains(it.Allowed, "wrong") {
		t.Fatalf("neither is true is not offered: %v", it.Allowed)
	}
	if err := mark(ctx, rs, it.ID, "wrong"); err != nil {
		t.Fatal(err)
	}
	day := time.Now().Format("2006-01-02")
	for _, n := range []string{"a", "b"} {
		if !strings.Contains(read(t, rs, n), "retired: "+day) {
			t.Fatalf("%s not retired:\n%s", n, read(t, rs, n))
		}
	}
	if subj := strings.TrimSpace(git(t, rs.s.Dir, "log", "-1", "--format=%s")); subj != "owner: neither is true: retire a and b" {
		t.Errorf("commit subject %q", subj)
	}
	files := git(t, rs.s.Dir, "show", "--name-only", "--format=", "HEAD")
	for _, f := range []string{"system/a.md", "system/b.md", "review/decisions.jsonl"} {
		if !strings.Contains(files, f) {
			t.Errorf("the commit lacks %s:\n%s", f, files)
		}
	}
	d := lastDecision(t, rs)
	if d.Action != "wrong" || d.By != "" || len(d.Before) != 2 {
		t.Fatalf("decision %+v", d)
	}
	got := itemFor(t, rs, "a", "b")
	if got.State != "wrong" || got.Mark != "wrong" || got.Stale || got.NeedsOwner || !contains(got.Allowed, "reopen") {
		t.Fatalf("after: state %q mark %q stale %v needs %v allowed %v", got.State, got.Mark, got.Stale, got.NeedsOwner, got.Allowed)
	}
	// The facts are theirs now: a verifier keeps off both.
	if owned := rs.ownerTouched(); !owned["a"] || !owned["b"] {
		t.Errorf("owner-touched %v", owned)
	}
	if err := mark(ctx, rs, it.ID, "reopen"); err != nil {
		t.Fatal(err)
	}
	if read(t, rs, "a") != origA || read(t, rs, "b") != origB {
		t.Fatalf("reopen is not exact:\n%s\n%s", read(t, rs, "a"), read(t, rs, "b"))
	}
	if again := itemFor(t, rs, "a", "b"); again.Decision != nil || !again.NeedsOwner {
		t.Fatalf("reopened item %+v", again)
	}
}

// The owner may say neither is true on a pair holding their own ruling; an agent
// may not say it at all, nor leave a note.
func TestNeitherIsTrueIsTheOwnersOnly(t *testing.T) {
	_, clone := ownerFixture(t)
	rs := clone("one")
	ctx := ctxT(t)
	pushReview(t, rs, "- CONTRADICTION?: [[rule]] (projects/x) vs [[engine-retries]] (projects/x)\n")
	it := itemFor(t, rs, "engine-retries", "rule")
	if _, errs, _ := rs.applyOps(ctx, []op{{ID: it.ID, A: "engine-retries", B: "rule", Mark: "wrong", By: "agent", Evidence: "README.md:1"}}); errs[0] == nil {
		t.Fatal("an agent said neither is true")
	}
	if _, errs, _ := rs.applyOps(ctx, []op{{ID: it.ID, A: "engine-retries", B: "rule", Mark: "both", By: "agent", Evidence: "README.md:1", Comment: "mine"}}); errs[0] == nil {
		t.Fatal("an agent left a note")
	}
	if it = itemFor(t, rs, "engine-retries", "rule"); it.Decision != nil {
		t.Fatalf("an agent's refused mark was recorded: %+v", it.Decision)
	}
	if err := mark(ctx, rs, it.ID, "wrong"); err != nil {
		t.Fatal(err)
	}
	day := time.Now().Format("2006-01-02")
	if !strings.Contains(readRel(t, rs, "projects/x/rule.md"), "retired: "+day) || !strings.Contains(readRel(t, rs, "projects/x/engine-retries.md"), "retired: "+day) {
		t.Fatal("the owner's answer did not retire their ruling and the other fact")
	}
}

// A note sent with an answer is kept with the answer (redacted), and written as
// their inbox note quoting them, dated, linking both facts by name, in the same
// commit.
func TestOwnerNoteWithAnAnswer(t *testing.T) {
	needRedaction(t)
	_, clone := ownerFixture(t)
	rs := clone("one")
	ctx := ctxT(t)
	it := itemFor(t, rs, "a", "b")
	secret := "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	if _, err := rs.answer(ctx, it.ID, "both", "Both hold: b is the exception for x.\nThe key was "+secret, false); err != nil {
		t.Fatal(err)
	}
	d := lastDecision(t, rs)
	if d.Action != "both" || !strings.HasPrefix(d.Comment, "Both hold: b is the exception for x.") || strings.Contains(d.Comment, secret) {
		t.Fatalf("decision %+v", d)
	}
	if got := itemFor(t, rs, "a", "b"); got.Decision == nil || got.Decision.Comment != d.Comment {
		t.Fatalf("past answer lacks the note: %+v", got.Decision)
	}
	files := strings.Fields(git(t, rs.s.Dir, "show", "--name-only", "--format=", "HEAD"))
	var note string
	for _, f := range files {
		if strings.HasPrefix(f, "inbox/human@") {
			note = f
		}
	}
	if note == "" {
		t.Fatalf("no inbox note in the commit: %v", files)
	}
	body := readRel(t, rs, note)
	day := time.Now().Format("2006-01-02")
	for _, want := range []string{"kind: user", "harness: human", "[[a]]", "[[b]]", "> Both hold: b is the exception for x.", "The owner's answer: both are true.", "on " + day} {
		if !strings.Contains(body, want) {
			t.Errorf("inbox note lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, secret) {
		t.Errorf("the note kept a secret:\n%s", body)
	}
}

// A note on its own leaves the finding open, shows on it, reaches the verifier
// as the owner's note, and keeps an agent from retiring a fact of it.
func TestOwnerNoteWithoutAnswering(t *testing.T) {
	needRedaction(t)
	_, clone := reviewFixture(t)
	rs := clone("one")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	it := itemFor(t, rs, "a", "b")
	head := git(t, rs.s.Dir, "rev-parse", "HEAD")
	if _, err := rs.answer(ctx, it.ID, "note", "   ", false); err == nil {
		t.Fatal("an empty note was accepted")
	}
	if now := git(t, rs.s.Dir, "rev-parse", "HEAD"); now != head || git(t, rs.s.Dir, "status", "--porcelain", "--untracked-files=all") != "" {
		t.Fatal("an empty note wrote something")
	}
	if _, err := rs.answer(ctx, it.ID, "note", "a still holds on the old boxes", false); err != nil {
		t.Fatal(err)
	}
	got := itemFor(t, rs, "a", "b")
	if got.Decision != nil || !got.NeedsOwner || len(got.Notes) != 1 || got.Notes[0].Text != "a still holds on the old boxes" {
		t.Fatalf("item after a note: decision %+v needs %v notes %+v", got.Decision, got.NeedsOwner, got.Notes)
	}
	if owned := rs.ownerTouched(); owned[it.ID] || owned["a"] || owned["b"] {
		t.Fatalf("a note claimed the finding: %v", owned)
	}
	if !strings.HasPrefix(strings.TrimSpace(git(t, rs.s.Dir, "log", "-1", "--format=%s")), "owner: note on a") {
		t.Error("the note is not an owner commit")
	}
	// The verifier reads it with the open finding.
	var prompt string
	r := &verifyRun{model: "m", per: 5, max: 10, budget: 1, cwd: t.TempDir(), stateDir: t.TempDir()}
	_, _, _ = r.run(ctx, rs.s, func(ctx context.Context, model, p, cwd string, budget float64) ([]verifyVerdict, string, float64, error) {
		prompt = p
		return []verifyVerdict{{ID: it.ID, Mark: "keep", Keep: "b", Evidence: "README.md:3 says b"}}, "", 0.01, nil
	})
	if !strings.Contains(prompt, `"OwnerNote":["Owner's note, `) || !strings.Contains(prompt, "a still holds on the old boxes") || !strings.Contains(prompt, "Never mark against them") {
		t.Fatalf("the verifier was not given the owner's note:\n%s", prompt)
	}
	// Its "keep b" would retire a: with their note on it, it became a question.
	after := itemFor(t, rs, "a", "b")
	if after.Decision != nil || after.Ask == nil || after.Ask.Proposed != "b" || strings.Contains(read(t, rs, "a"), "superseded_by") {
		t.Fatalf("the agent retired against their note: decision %+v ask %+v", after.Decision, after.Ask)
	}
}

// Redaction fails closed: no scrump, nothing saved.
func TestOwnerNoteFailsClosedWithoutRedaction(t *testing.T) {
	_, clone := reviewFixture(t)
	rs := clone("one")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "scrump"), []byte("#!/bin/sh\necho broken >&2\nexit 3\n"), 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	it := itemFor(t, rs, "a", "b")
	head := git(t, rs.s.Dir, "rev-parse", "HEAD")
	for _, m := range []string{"both", "note", "wrong"} {
		if _, err := rs.answer(ctx, it.ID, m, "a note", false); err == nil || !strings.Contains(err.Error(), "redaction") {
			t.Fatalf("%s: err %v", m, err)
		}
	}
	if now := git(t, rs.s.Dir, "rev-parse", "HEAD"); now != head || git(t, rs.s.Dir, "status", "--porcelain", "--untracked-files=all") != "" {
		t.Fatal("a note without redaction wrote something")
	}
}

// A note survives a lost push race: the retry finds its own note on disk with
// the same bytes and lands it; the same note sent again the same day lands
// too. For a note alone and for Neither is true with a note.
func TestOwnerNoteSurvivesALostRace(t *testing.T) {
	needRedaction(t)
	bare, clone := ownerFixture(t)
	one, two := clone("one"), clone("two")
	ctx := ctxT(t)
	pushReview(t, two, "- CONTRADICTION?: [[engine-retries]] (projects/x) vs [[rule]] (projects/x)\n")
	one.loadFacts()
	ab := itemFor(t, one, "a", "b")
	var otherErr error
	race := func() {
		db := draftOf(t, two, "a")
		db.Body = "a from two " + time.Now().Format(time.RFC3339Nano)
		_, otherErr = two.editFact(ctx, "a", db)
	}
	one.beforePush = race
	if _, err := one.answer(ctx, ab.ID, "note", "a still holds on the old boxes", false); err != nil || otherErr != nil {
		t.Fatalf("note after a lost race: %v; other: %v", err, otherErr)
	}
	one.beforePush = race
	pair := itemFor(t, one, "rule", "engine-retries")
	if _, err := one.answer(ctx, pair.ID, "wrong", "both moved to the new engine", false); err != nil || otherErr != nil {
		t.Fatalf("neither with a note after a lost race: %v; other: %v", err, otherErr)
	}
	log := git(t, bare, "log", "--format=%s", "main")
	if n := strings.Count(log, "owner: edit a (body)"); n != 2 {
		t.Fatalf("the other writer should have won both races, landed %d times:\n%s", n, log)
	}
	for _, want := range []string{"owner: note on a", "owner: neither is true: retire rule and engine-retries"} {
		if !strings.Contains(log, want) {
			t.Fatalf("server log lacks %q:\n%s", want, log)
		}
	}
	if st := git(t, one.s.Dir, "status", "--porcelain", "--untracked-files=all"); st != "" {
		t.Fatalf("a note was left behind:\n%s", st)
	}
	// The same words again, the same day: a second record, the same note file.
	if _, err := one.answer(ctx, ab.ID, "note", "a still holds on the old boxes", false); err != nil {
		t.Fatalf("the same note again: %v", err)
	}
	if got := itemFor(t, one, "a", "b"); len(got.Notes) != 2 {
		t.Fatalf("notes %+v", got.Notes)
	}
}

// A note follows the fact: a newer finding on the same fact carries it, the
// verifier reads it, and an agent's retirement of the fact becomes a question.
func TestOwnerNoteFollowsTheFact(t *testing.T) {
	needRedaction(t)
	_, clone := ownerFixture(t)
	rs := clone("one")
	ctx := ctxT(t)
	old := rs.staleItems()[0]
	if _, err := rs.answer(ctx, old.ID, "note", "still true: the retry limit moved, the rule did not", false); err != nil {
		t.Fatal(err)
	}
	pushReview(t, rs, "- stale? [[engine-retries]] (projects/x) @def5678 n=1 since=2026-10-03\n  2026-10-04 def5678 x: drop - MAX_RETRIES (gone at HEAD)\n")
	var st staleItem
	for _, it := range rs.staleItems() {
		if it.A.Name == "engine-retries" {
			st = it
		}
	}
	if st.ID == old.ID || len(st.Notes) != 1 || !strings.HasPrefix(st.Notes[0].Text, "still true") {
		t.Fatalf("the newer finding %s (old %s) carries %+v", st.ID, old.ID, st.Notes)
	}
	r := &verifyRun{}
	if line := r.staleLine(ctx, rs, st); !strings.Contains(line, `"OwnerNote":["Owner's note, `) {
		t.Fatalf("the verifier input lacks their note: %s", line)
	}
	_, errs, err := rs.applyOps(ctx, []op{{ID: st.ID, A: "engine-retries", Mark: "retire", By: "agent", Evidence: "def5678 removed MAX_RETRIES"}})
	if err != nil || errs[0] != nil {
		t.Fatalf("%v %v", err, errs)
	}
	var after *staleItem
	for _, it := range rs.staleItems() {
		if it.ID == st.ID {
			c := it
			after = &c
		}
	}
	if after == nil || after.Decision != nil || after.Ask == nil || strings.Contains(readRel(t, rs, "projects/x/engine-retries.md"), "retired:") {
		t.Fatalf("an agent retired a fact the owner left a note on: %+v", after)
	}
}

// A scrump that runs but masks nothing fails the canary: no note is saved.
func TestOwnerNoteRunsTheRedactionCanary(t *testing.T) {
	_, clone := reviewFixture(t)
	rs := clone("one")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "scrump"), []byte("#!/bin/sh\ncat\n"), 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	it := itemFor(t, rs, "a", "b")
	head := git(t, rs.s.Dir, "rev-parse", "HEAD")
	if _, err := rs.answer(ctx, it.ID, "note", "a note", false); err == nil || !strings.Contains(err.Error(), "redaction") {
		t.Fatalf("err %v", err)
	}
	if git(t, rs.s.Dir, "rev-parse", "HEAD") != head || git(t, rs.s.Dir, "status", "--porcelain", "--untracked-files=all") != "" {
		t.Fatal("a note passed a redactor that masks nothing")
	}
}
