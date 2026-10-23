package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/assess"
	"github.com/agent-sh/tacit/client/internal/curate"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/lint"
	"github.com/agent-sh/tacit/client/internal/record"
	"github.com/agent-sh/tacit/client/internal/store"
)

func daysAgo(n int) string { return time.Now().AddDate(0, 0, -n).Format("2006-01-02") }

// datedRepo makes a repository in the projects directory with one commit
// per day under the owner's identity, each changing the given file.
func datedRepo(t *testing.T, home, name string, commits ...[2]string) {
	t.Helper()
	dir := filepath.Join(home, "projects", name)
	os.MkdirAll(dir, 0o755)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		gitIn(t, dir, "init", "-q", "-b", "main")
	}
	for i, c := range commits {
		p := filepath.Join(dir, filepath.FromSlash(c[1]))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(name+c[0]+string(rune('a'+i))), 0o644)
		gitIn(t, dir, "add", "-A")
		cmd := exec.Command("git", "-c", "user.email=owner@example.com", "-c", "user.name=Owner", "commit", "-q", "-m", "work")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE="+c[0]+"T12:00:00", "GIT_COMMITTER_DATE="+c[0]+"T12:00:00")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("commit: %v %s", err, out)
		}
	}
}

// wordsModel answers every folder active, and oldtool with the owner's word
// on its status from the note code shows for it.
type wordsModel struct {
	prompts []string
	words   assess.OwnerWords
}

func (m *wordsModel) run(_ context.Context, _, prompt string) (string, float64, error) {
	m.prompts = append(m.prompts, prompt)
	var out []assess.Decision
	for _, b := range blockRx.FindAllStringSubmatch(prompt, -1) {
		d := assess.Decision{Folder: b[1], Kind: "repo", Status: "active", Why: "the owner's repository " + citeInRx.FindString(b[2]), Description: "what " + b[1] + " is"}
		if b[1] == "projects/oldtool" && strings.Contains(b[2], "[[oldtool-obsolete]]") {
			d.OwnerStatus = assess.OwnerWordsList{m.words}
		}
		out = append(out, d)
	}
	bs, _ := json.Marshal(out)
	return string(bs), 0.05, nil
}

// wordsHome is assessHome with oldtool (the owner's commits until three days
// ago: active by its dates), tower (worked today) and the owner's decision,
// a day old, that oldtool is obsolete and tower replaces it.
func wordsHome(t *testing.T) (string, *store.Store) {
	home, s := assessHome(t)
	datedRepo(t, home, "oldtool", [2]string{daysAgo(9), "main.go"}, [2]string{daysAgo(6), "main.go"}, [2]string{daysAgo(3), "cmd/run.go"})
	datedRepo(t, home, "tower", [2]string{daysAgo(0), "main.go"})
	writeFactFile(t, s.Dir, &fact.Fact{Path: "user/oldtool-obsolete.md", Meta: fact.Meta{Name: "oldtool-obsolete", Type: "design_decision", Scope: "user",
		Description: "Owner decided on " + daysAgo(1) + " that oldtool is obsolete: tower replaces it.",
		Source:      fact.Source{Harness: "curator", Host: "box", Session: "s-one", Kind: "observed"}, Created: daysAgo(0)}, Body: "Stop polishing oldtool.\n"})
	for _, f := range []*fact.Fact{
		{Path: "projects/tower/tower-signs-releases.md", Meta: fact.Meta{Name: "tower-signs-releases", Type: "user_standard", Scope: "projects/tower", Description: "Every tower release is a signed tag",
			Source: fact.Source{Harness: "curator", Host: "box", Session: "s-two", Kind: "observed"}, Created: daysAgo(0)}, Body: "Signed tags.\n"},
		{Path: "projects/oldtool/oldtool-flag-order.md", Meta: fact.Meta{Name: "oldtool-flag-order", Type: "user_standard", Scope: "projects/oldtool", Description: "oldtool flags go before the subcommand",
			Source: fact.Source{Harness: "curator", Host: "box", Session: "s-two", Kind: "observed"}, Created: daysAgo(5)}, Body: "Flags first.\n"},
	} {
		writeFactFile(t, s.Dir, f)
	}
	facts, _, _ := fact.Walk(s.Dir)
	writeIndexesForTest(t, s.Dir, facts)
	gitIn(t, s.Dir, "add", "-A")
	gitIn(t, s.Dir, "commit", "-q", "-m", "the owner's decision")
	return home, s
}

var wordsOnly = map[string]bool{"projects/oldtool": true, "projects/tower": true}

// The owner's word beats the dates: oldtool is active by its commits, but
// the owner's decision a day old, with no work on it since, makes it dead,
// replaced by tower (decided later in the same batch: the decision waits for
// its record). Its facts stay; the tree and search say what replaced it.
func TestOwnerWordsBeatDates(t *testing.T) {
	home, s := wordsHome(t)
	m := &wordsModel{words: assess.OwnerWords{Says: "dead", Words: "oldtool is obsolete: tower replaces it", From: "note:oldtool-obsolete", Successor: "tower"}}
	prev := assessRun
	t.Cleanup(func() { assessRun = prev })
	assessRun = m.run
	rep := &curate.Report{}
	if errs := assessStep(context.Background(), s, assessOpts{model: "opus", only: wordsOnly}, rep); errs != 0 || rep.AssessRecords != 2 || rep.AssessRefused != 0 {
		t.Fatalf("errs %d report %+v", errs, rep)
	}
	p := m.prompts[0]
	if !strings.Contains(p, "notes in other folders that name it") || !strings.Contains(p, "[[oldtool-obsolete]] (user, design_decision, "+daysAgo(0)+"; a decision): Owner decided on") {
		t.Fatalf("the note is not shown up front:\n%s", p)
	}
	recs, _ := record.Walk(s.Dir)
	r := recs["projects/oldtool"]
	if r == nil || r.Status != record.StatusDead || r.Successor != "projects/tower" || r.SuppressedBy != "" || r.Was[0].WordsOn != daysAgo(1) || r.Was[0].WordsFrom != "note:oldtool-obsolete" {
		t.Fatalf("oldtool %+v", r)
	}
	if !strings.HasPrefix(r.Was[0].Why, "code: dead on the owner's words of "+daysAgo(1)) || !strings.Contains(r.Was[0].Why, "replaced by projects/tower") || !strings.Contains(r.Was[0].Why, "its dates alone say active") {
		t.Fatalf("why %s", r.Was[0].Why)
	}
	if fs, err := lint.Run(s.Dir); err != nil || len(fs) != 0 {
		t.Fatalf("lint %v %v", fs, err)
	}
	idx, _ := os.ReadFile(filepath.Join(s.Dir, "projects", "_index.md"))
	if !strings.Contains(string(idx), "repo, dead, replaced by projects/tower") {
		t.Errorf("projects index:\n%s", idx)
	}
	// A session in oldtool's checkout: oldtool is quiet (dead); tower's rule
	// comes lightly, and the title says what replaced it. Search says so too.
	out := hookOut(t, startPayload(filepath.Join(home, "projects", "oldtool"), "r1"), "session-start")
	if !strings.Contains(out, "tower-signs-releases") || strings.Contains(out, "oldtool-flag-order") || !strings.Contains(out, "oldtool (dead, replaced by tower)") {
		t.Fatalf("session start in the dead repository:\n%s", out)
	}
	if out := hookOut(t, startPayload(filepath.Join(home, "projects", "widget"), "r2"), "session-start"); strings.Contains(out, "tower-signs-releases") {
		t.Fatalf("the successor's rule reached another repository:\n%s", out)
	}
	res, err := runSearchText(t, s, browseArgs{Query: "oldtool flags subcommand", Scope: "projects/oldtool"})
	if err != nil || !strings.Contains(res, "oldtool-flag-order") || !strings.Contains(res, "# projects/oldtool is dead, replaced by projects/tower: its facts are its history") {
		t.Fatalf("search: %v\n%s", err, res)
	}
	// A second run asks nothing: the words hold while nobody works on it.
	n := len(m.prompts)
	if errs := assessStep(context.Background(), s, assessOpts{model: "opus", only: wordsOnly}, &curate.Report{}); errs != 0 || len(m.prompts) != n {
		t.Fatalf("second run: %d errors, %d prompts", errs, len(m.prompts)-n)
	}
}

// Real work after the owner's words is a changed mind: a commit today that
// changes code keeps oldtool's dates (active), the words cited, and leaves
// an optional review item. A later dependency bump alone would not.
func TestOwnerWordsAndLaterWork(t *testing.T) {
	home, s := wordsHome(t)
	datedRepo(t, home, "oldtool", [2]string{daysAgo(0), "go.mod"})
	m := &wordsModel{words: assess.OwnerWords{Says: "dead", Words: "oldtool is obsolete", From: "note:oldtool-obsolete"}}
	prev := assessRun
	t.Cleanup(func() { assessRun = prev })
	assessRun = m.run
	if errs := assessStep(context.Background(), s, assessOpts{model: "opus", only: wordsOnly}, &curate.Report{}); errs != 0 {
		t.Fatalf("errs %d", errs)
	}
	recs, _ := record.Walk(s.Dir)
	if r := recs["projects/oldtool"]; r.Status != record.StatusDead || !strings.Contains(r.Was[0].Why, "maintenance only: "+daysAgo(0)+" commit") {
		t.Fatalf("a dependency bump changed the owner's mind: %+v", r.Was[0])
	}
	// Real work today: the record is asked again, and the dates stand.
	datedRepo(t, home, "oldtool", [2]string{daysAgo(0), "internal/new.go"})
	if errs := assessStep(context.Background(), s, assessOpts{model: "opus", only: wordsOnly}, &curate.Report{}); errs != 0 {
		t.Fatalf("errs %d", errs)
	}
	recs, _ = record.Walk(s.Dir)
	r := recs["projects/oldtool"]
	if r.Status != record.StatusActive || r.Successor != "" || r.Was[0].Words != "" || !strings.Contains(r.Was[0].Why, "Owner activity after the "+daysAgo(1)+" statement (note:oldtool-obsolete") || !strings.Contains(r.Was[0].Why, "changes internal/new.go") {
		t.Fatalf("oldtool %+v", r.Was[0])
	}
	rv, _ := os.ReadFile(filepath.Join(s.Dir, "review", today()+"-assess.md"))
	if !strings.Contains(string(rv), "- check? [[oldtool-obsolete]] (user) outcome=activity on="+today()) {
		t.Fatalf("review item:\n%s", rv)
	}
	if fs, err := lint.Run(s.Dir); err != nil || len(fs) != 0 {
		t.Fatalf("lint %v %v", fs, err)
	}
}

// runSearchText is tacit_search on the store's index.
func runSearchText(t *testing.T, s *store.Store, a browseArgs) (string, error) {
	t.Helper()
	ctx := context.Background()
	ix, err := openIndex(ctx, s)
	if err != nil {
		return "", err
	}
	defer ix.Close()
	return treeSearch(ctx, ix, a)
}

// A successor with no record yet that this run does not decide: the folder
// is dead on the owner's words now, and code links the successor once it
// has a record (no model call for it).
func TestSuccessorLinkedLater(t *testing.T) {
	_, s := wordsHome(t)
	m := &wordsModel{words: assess.OwnerWords{Says: "dead", Words: "oldtool is obsolete: tower replaces it", From: "note:oldtool-obsolete", Successor: "tower"}}
	prev := assessRun
	t.Cleanup(func() { assessRun = prev })
	assessRun = m.run
	ctx := context.Background()
	if errs := assessStep(ctx, s, assessOpts{model: "opus", only: map[string]bool{"projects/oldtool": true}}, &curate.Report{}); errs != 0 {
		t.Fatalf("errs %d", errs)
	}
	recs, _ := record.Walk(s.Dir)
	if r := recs["projects/oldtool"]; r == nil || r.Status != record.StatusDead || r.Successor != "" || !strings.Contains(r.Was[0].Why, "replaced by projects/tower (linked once it has a record)") {
		t.Fatalf("oldtool %+v", r)
	}
	if errs := assessStep(ctx, s, assessOpts{model: "opus", only: wordsOnly}, &curate.Report{}); errs != 0 {
		t.Fatalf("errs %d", errs)
	}
	recs, _ = record.Walk(s.Dir)
	if r := recs["projects/oldtool"]; r.Successor != "projects/tower" || len(r.Was) != 1 || recs["projects/tower"] == nil {
		t.Fatalf("oldtool %+v", r)
	}
	if fs, err := lint.Run(s.Dir); err != nil || len(fs) != 0 {
		t.Fatalf("lint %v %v", fs, err)
	}
}
