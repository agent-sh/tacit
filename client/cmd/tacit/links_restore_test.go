package main

import (
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
