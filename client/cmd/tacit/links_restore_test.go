package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/links"
)

// Runs of the step before v0.9.17 unwrapped a fact's links to what it
// replaced (the statements a preference was made of) as dead. `tacit links
// --restore` reads the step's own commits and puts those links back where the
// line the run wrote is still there, through the step's locked write, and
// nothing else: a link the run unwrapped because nothing had the name stays
// plain text, and a line edited since is left alone. The next run of the
// step leaves the links as they are, and a second restore does nothing.
func TestLinksRestorePutsBackWhatOlderRunsUnwrapped(t *testing.T) {
	stubScrump(t)
	r := newRig(t, rigOpts{})
	w := func(name string, m fact.Meta, body string) {
		m.Name, m.Scope, m.Description, m.Type = name, "user", "fact "+name, "user_standard"
		writeFact(t, r.clone, m, body)
	}
	w("stmt-a", fact.Meta{SupersededBy: "pref"}, "You said once.")
	w("stmt-b", fact.Meta{SupersededBy: "pref"}, "You said again.")
	w("pref", fact.Meta{}, "Made of [[stmt-a]] and [[stmt-b|the second]]; see [[gone-x]].\nAlso from [[stmt-a]].")
	r.commitIndex(r.clone, "facts")
	// What an older run of the step wrote, under its own subject.
	p := filepath.Join(r.clone, "user", "pref.md")
	b, _ := os.ReadFile(p)
	unwrapped := strings.NewReplacer("[[stmt-a]]", "stmt-a", "[[stmt-b|the second]]", "the second", "[[gone-x]]", "gone-x").Replace(string(b))
	os.WriteFile(p, []byte(unwrapped), 0o644)
	gitIn(t, r.clone, "add", "-A")
	gitIn(t, r.clone, "-c", "user.name=curator", "commit", "-q", "-m", links.CommitPrefix+" 4 link(s) in 1 fact(s) repaired (0 rewritten, 4 unwrapped)")
	// The second line was edited since: its link is not put back.
	edited := strings.Replace(unwrapped, "Also from stmt-a.", "Also from stmt-a, as edited later.", 1)
	os.WriteFile(p, []byte(edited), 0o644)
	gitIn(t, r.clone, "commit", "-q", "-am", "curator: an edit")
	gitIn(t, r.clone, "push", "-q", "origin", "HEAD:main")

	dry, err := linksStepFor(ctxMinute(t), r.s, true, linksNow, true)
	if err != nil || len(dry.ToFix) != 1 || len(dry.ToFix[0].Edits) != 2 {
		t.Fatalf("the dry run plans: %+v %v", dry, err)
	}
	rep, err := linksStepFor(ctxMinute(t), r.s, false, linksNow, true)
	if err != nil || !rep.Pushed {
		t.Fatalf("restore: %+v %v", rep, err)
	}
	got := r.onServer("user/pref.md")
	if !strings.Contains(got, "Made of [[stmt-a]] and [[stmt-b|the second]]; see gone-x.") || !strings.Contains(got, "Also from stmt-a, as edited later.") {
		t.Fatalf("on the server:\n%s", got)
	}
	if subj := gitIn(t, r.clone, "log", "-1", "--format=%s"); !strings.HasPrefix(subj, links.CommitPrefix) || !strings.Contains(subj, "2 link(s) to what the fact replaced put back in 1 fact(s)") {
		t.Fatalf("commit: %s", subj)
	}
	// The step leaves them; a second restore finds nothing.
	if rep, err := r.links(false); err != nil || len(rep.Repaired) != 0 {
		t.Fatalf("the step after the restore: %+v %v", rep, err)
	}
	if rep, err := linksStepFor(ctxMinute(t), r.s, false, linksNow, true); err != nil || rep.Pushed || len(rep.ToFix) != 0 {
		t.Fatalf("a second restore: %+v %v", rep, err)
	}
}

// Two runs that repaired one line come back whole (the commits are read
// newest first); a commit that only names the step in its body is not the
// step's; what a killed daily run left in the clone (an unwrap, a review
// line) is not committed by the restore, which skips that fact; and a fact a
// killed restore wrote exactly as the plan has it is committed.
func TestLinksRestoreReadsTheStepsCommitsAndLeavesForeignDirt(t *testing.T) {
	stubScrump(t)
	r := newRig(t, rigOpts{})
	w := func(name string, m fact.Meta, body string) {
		m.Name, m.Scope, m.Description, m.Type = name, "user", "fact "+name, "user_standard"
		writeFact(t, r.clone, m, body)
	}
	for _, n := range []string{"stmt-a", "stmt-b", "stmt-c", "stmt-d"} {
		w(n, fact.Meta{SupersededBy: map[string]string{"stmt-a": "pref", "stmt-b": "pref", "stmt-c": "pref-two", "stmt-d": "pref-three"}[n]}, "You said once.")
	}
	w("pref", fact.Meta{}, "Made of [[stmt-a]] and [[stmt-b]].")
	w("pref-two", fact.Meta{}, "Made of [[stmt-c]].")
	w("pref-three", fact.Meta{}, "Made of [[stmt-d]].")
	r.commitIndex(r.clone, "facts")
	edit := func(rel, from, to, subject string) {
		p := filepath.Join(r.clone, filepath.FromSlash(rel))
		b, _ := os.ReadFile(p)
		os.WriteFile(p, []byte(strings.Replace(string(b), from, to, 1)), 0o644)
		gitIn(t, r.clone, "add", "-A")
		gitIn(t, r.clone, "-c", "user.name=curator", "commit", "-q", "-m", subject)
	}
	edit("user/pref.md", "[[stmt-a]]", "stmt-a", links.CommitPrefix+" 1 link(s) in 1 fact(s) repaired (0 rewritten, 1 unwrapped)")
	edit("user/pref.md", "[[stmt-b]]", "stmt-b", links.CommitPrefix+" 1 link(s) in 1 fact(s) repaired (0 rewritten, 1 unwrapped)")
	edit("user/pref-two.md", "[[stmt-c]]", "stmt-c", "curator: an edit\n\n"+links.CommitPrefix+" in the body is not the step's commit")
	edit("user/pref-three.md", "[[stmt-d]]", "stmt-d", links.CommitPrefix+" 1 link(s) in 1 fact(s) repaired (0 rewritten, 1 unwrapped)")
	gitIn(t, r.clone, "push", "-q", "origin", "HEAD:main")
	// A killed daily run's leftovers: a change to pref-three, a review line.
	p3 := filepath.Join(r.clone, "user", "pref-three.md")
	b3, _ := os.ReadFile(p3)
	os.WriteFile(p3, []byte(strings.Replace(string(b3), "Made of stmt-d.", "Made of stmt-d, and more.", 1)), 0o644)
	os.MkdirAll(filepath.Join(r.clone, "review"), 0o755)
	os.WriteFile(filepath.Join(r.clone, "review", "2026-10-10.md"), []byte("## link check\n- LINK? [[x]] (user) links to [[y]], and y is gone; this is an owner ruling, so the curator left it; it would write plain text\n"), 0o644)

	rep, err := linksStepFor(ctxMinute(t), r.s, false, linksNow, true)
	if err != nil || !rep.Pushed {
		t.Fatalf("restore: %+v %v", rep, err)
	}
	if got := r.onServer("user/pref.md"); !strings.Contains(got, "Made of [[stmt-a]] and [[stmt-b]].") {
		t.Fatalf("two runs on one line did not both come back:\n%s", got)
	}
	if got := r.onServer("user/pref-two.md"); strings.Contains(got, "[[stmt-c]]") {
		t.Fatalf("a commit naming the step only in its body was read as the step's:\n%s", got)
	}
	if got := r.onServer("user/pref-three.md"); strings.Contains(got, "[[stmt-d]]") || strings.Contains(got, "and more") {
		t.Fatalf("a fact with another writer's changes was restored or committed:\n%s", got)
	}
	if files := gitIn(t, r.clone, "show", "--name-only", "--format=", "HEAD"); strings.Contains(files, "review/") {
		t.Fatalf("the restore committed a killed run's review lines: %s", files)
	}
	found := false
	for _, n := range rep.NotDone {
		found = found || n.Path == "user/pref-three.md"
	}
	if !found {
		t.Fatalf("the skipped fact is not reported: %+v", rep.NotDone)
	}
}

// A restore that has nothing to put back says so in its own words.
func TestLinksRestoreWithNothingToPutBackSaysSo(t *testing.T) {
	stubScrump(t)
	r := newRig(t, rigOpts{})
	writeFact(t, r.clone, fact.Meta{Name: "plain", Scope: "user", Description: "fact plain", Type: "user_standard"}, "Nothing linked.")
	r.commitIndex(r.clone, "facts")
	gitIn(t, r.clone, "push", "-q", "origin", "HEAD:main")
	rep, err := linksStepFor(ctxMinute(t), r.s, true, linksNow, true)
	if err != nil {
		t.Fatal(err)
	}
	var out, errw bytes.Buffer
	writeLinks(&out, &errw, rep, true)
	if !strings.Contains(out.String(), "links: 0 link(s) to what the fact replaced would put back in 0 fact(s)") || strings.Contains(out.String(), "would repair") {
		t.Fatalf("the restore's report:\n%s", out.String())
	}
}
