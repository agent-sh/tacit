package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/statement"
)

// tacit rulings one-time: a ruling made of a list of chores and a decision
// the statements step made of a go-ahead are retired in one curator
// commit, with a line in the statements log; an instruction of the task at
// hand in the user scope is asked about and left as it is; a standing rule
// and the same task instruction in its project are not touched; a second
// run writes nothing.
func TestOneTimeRetiresChoresAndAsksAboutTaskWords(t *testing.T) {
	r := newRig(t, rigOpts{})
	chores := "send the invoice, already called the bank. remind me at 5 to renew the domain"
	writeFact(t, r.clone, fact.Meta{Name: "chores-decision", Description: "Clearing items from the morning list: the invoice, the bank and the domain", Type: "design_decision", Scope: "user", Source: ownerSource(chores)},
		"You decided (2026-10-24): \""+chores+"\"\n")
	goAhead := "go on with it, publish it once done"
	writeFact(t, r.clone, fact.Meta{Name: "release-when-done", Description: "Continue the export work and publish it once it is done", Type: "user_standard", Scope: "projects/storefront", Source: ownerSource(goAhead)},
		"You decided (2026-10-25): \""+goAhead+"\"\n")
	task := "you can close the stale pull requests, they are not needed"
	writeFact(t, r.clone, fact.Meta{Name: "close-stale-prs", Description: "Close the stale pull requests that are not needed", Type: "user_standard", Scope: "user", Source: ownerSource(task)},
		"The owner said \""+task+"\".\n")
	writeFact(t, r.clone, fact.Meta{Name: "close-stale-prs-here", Description: "Close the stale pull requests of the storefront that are not needed", Type: "user_standard", Scope: "projects/storefront", Source: ownerSource(task)},
		"The owner said \""+task+"\".\n")
	rule := "never push to main without a review of the diff"
	writeFact(t, r.clone, fact.Meta{Name: "review-before-push", Description: "Never push to main without a review of the diff", Type: "user_standard", Scope: "user", Source: ownerSource(rule)},
		"The owner said \""+rule+"\".\n")
	os.MkdirAll(filepath.Join(r.clone, "review"), 0o755)
	os.WriteFile(filepath.Join(r.clone, filepath.FromSlash(ledger.StatementsPath)),
		statement.Entry{Kind: statement.LogPromoted, At: "2026-10-25", Fact: "release-when-done", Level: statement.LevelPreference, By: statement.BySessions}.Line(), 0o644)
	r.commitIndex(r.clone, "one-time fixture")
	gitIn(t, r.clone, "push", "-q", "origin", "HEAD:main")
	t.Setenv("TACIT_STORE", r.clone)

	head := r.serverHead()
	if code := runOneTime([]string{"--dry-run"}); code != 0 || r.serverHead() != head {
		t.Fatalf("one-time --dry-run exited %d or moved the server", code)
	}
	if code := runOneTime(nil); code != 0 {
		t.Fatalf("one-time exited %d", code)
	}
	retired := func(rel string) string {
		f, err := fact.Parse(rel, []byte(r.onServer(rel)))
		if err != nil {
			t.Fatal(err)
		}
		return f.Meta.Retired
	}
	for rel, want := range map[string]bool{
		"user/chores-decision.md":                     true,
		"projects/storefront/release-when-done.md":    true,
		"user/close-stale-prs.md":                     false,
		"projects/storefront/close-stale-prs-here.md": false,
		"user/review-before-push.md":                  false,
	} {
		if got := retired(rel) != ""; got != want {
			t.Errorf("%s: retired %v, want %v", rel, got, want)
		}
	}
	log := r.onServer(ledger.StatementsPath)
	if !strings.Contains(log, `"kind":"one-time"`) || !strings.Contains(log, `"name":"chores-decision"`) {
		t.Errorf("statements log: %s", log)
	}
	refs := parseCheckFiles(r.clone)
	if len(refs) != 1 || refs[0].name != "close-stale-prs" || refs[0].outcome != outcomeOneTime {
		t.Fatalf("questions: %+v", refs)
	}
	r.assertClean("after one-time")
	head = r.serverHead()
	if code := runOneTime(nil); code != 0 || r.serverHead() != head {
		t.Fatalf("second run exited %d or pushed", code)
	}
}
