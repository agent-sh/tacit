package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/lint"
	"github.com/agent-sh/tacit/client/internal/record"
	"github.com/agent-sh/tacit/client/internal/store"
)

// migrateHome is a fresh local store with projects/widget and
// projects/widget-old, the second suppressed into the first, and in
// widget-old: a plain fact, one that restates a widget fact, a ruling with
// its quote, a ruling that a widget fact contradicts, and a fact with the
// owner's words.
func migrateHome(t *testing.T) *store.Store {
	t.Helper()
	home, _ := filepath.EvalSymlinks(t.TempDir())
	for k, v := range map[string]string{"HOME": home, "TACIT_STATE_DIR": "", "TACIT_STORE": filepath.Join(home, "store"), "TACIT_INDEX": filepath.Join(home, "index"),
		"TACIT_LOCAL": filepath.Join(home, "overlay"), "TACIT_PROJECTS": filepath.Join(home, "projects")} {
		t.Setenv(k, v)
	}
	if code := runInit(nil); code != 0 {
		t.Fatalf("init %d", code)
	}
	s := &store.Store{Dir: filepath.Join(home, "store")}
	w, o := testRec(record.KindRepo, "projects/widget"), testRec(record.KindRepo, "projects/widget-old")
	o.SuppressedBy, o.Aliases = "projects/widget", []string{"widget-legacy"}
	putRecordFile(t, s.Dir, w)
	putRecordFile(t, s.Dir, o)
	src := fact.Source{Harness: "curator", Host: "box", Kind: "observed", Session: "s1"}
	put := func(f *fact.Fact) {
		f.Path = f.Meta.Scope + "/" + f.Meta.Name + ".md"
		if f.Meta.Source.Harness == "" {
			f.Meta.Source = src
		}
		f.Meta.Created = "2026-10-01"
		writeFactFile(t, s.Dir, f)
	}
	put(&fact.Fact{Meta: fact.Meta{Name: "widget-port", Description: "The widget server listens on port 7781", Type: "machine_fact", Scope: "projects/widget"}, Body: "Port 7781 on every host.\n"})
	put(&fact.Fact{Meta: fact.Meta{Name: "widget-queue", Description: "Widget jobs go through one queue", Type: "design_decision", Scope: "projects/widget", ConflictsWith: []string{"old-tenant-queues"}}, Body: "One queue.\n"})
	put(&fact.Fact{Meta: fact.Meta{Name: "old-build", Description: "The old widget builds with make release", Type: "design_decision", Scope: "projects/widget-old", AppliesTo: fact.AppliesTo{Repos: []string{"widget-old", "widget-legacy", "other"}}}, Body: "make release.\n"})
	put(&fact.Fact{Meta: fact.Meta{Name: "old-port", Description: "The widget server listens on port 7781", Type: "machine_fact", Scope: "projects/widget-old"}, Body: "Port 7781 on every host.\n"})
	quote := "never deploy the widget on a friday afternoon"
	put(&fact.Fact{Meta: fact.Meta{Name: "old-deploy-rule", Description: "Never deploy the widget on a Friday afternoon", Type: "user_standard", Scope: "projects/widget-old",
		Source: fact.Source{Harness: "curator", Host: "box", Kind: "user", OwnerQuote: quote, OwnerQuoteFrom: "claude:s9"}}, Body: "The owner: \"" + quote + "\".\n"})
	tenant := "keep one queue per tenant for the widget jobs"
	put(&fact.Fact{Meta: fact.Meta{Name: "old-tenant-queues", Description: "Keep one queue per tenant for the widget jobs", Type: "user_standard", Scope: "projects/widget-old",
		Source: fact.Source{Harness: "curator", Host: "box", Kind: "user", OwnerQuote: tenant, OwnerQuoteFrom: "claude:s9"}}, Body: "The owner: \"" + tenant + "\".\n"})
	words := "keep the start block short"
	put(&fact.Fact{Meta: fact.Meta{Name: "old-start-block", Description: "The widget start block stays short: keep the start block short", Type: "design_decision", Scope: "projects/widget-old",
		Source: fact.Source{Harness: "curator", Host: "box", Kind: "observed", OwnerWords: words, OwnerWordsFrom: "claude:s9"}}, Body: "Short.\n"})
	ql := ledger.Quote{Name: "old-deploy-rule", Quote: quote, From: "claude:s9", TurnHash: "aa", At: "2026-10-01"}.Line()
	ql = append(ql, ledger.Quote{Name: "old-tenant-queues", Quote: tenant, From: "claude:s9", TurnHash: "bb", At: "2026-10-01"}.Line()...)
	ql = append(ql, ledger.Quote{Name: "old-start-block", Quote: words, From: "claude:s9", TurnHash: "cc", At: "2026-10-01", Words: true}.Line()...)
	os.WriteFile(filepath.Join(s.Dir, filepath.FromSlash(ledger.QuotesPath)), ql, 0o644)
	facts, _, _ := fact.Walk(s.Dir)
	writeIndexesForTest(t, s.Dir, facts)
	gitIn(t, s.Dir, "add", "-A")
	gitIn(t, s.Dir, "commit", "-q", "-m", "facts")
	if fs, err := lint.Run(s.Dir); err != nil || len(fs) != 0 {
		t.Fatalf("fixture lint: %v %v", fs, err)
	}
	return s
}

func factsByName(t *testing.T, dir string) map[string]*fact.Fact {
	t.Helper()
	facts, _, err := fact.Walk(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*fact.Fact{}
	for _, f := range facts {
		out[f.Meta.Name] = f
	}
	return out
}

// The suppressed folder empties: each old fact points at a live fact of the
// winner (a copy, or the fact it restates), rulings and words move with
// their ledger lines, a ruling the winner contradicts is held for the owner
// with the reason, and a second run writes nothing.
func TestMigrateEmptiesTheSuppressedFolder(t *testing.T) {
	s := migrateHome(t)
	ctx := context.Background()
	head := gitIn(t, s.Dir, "rev-parse", "HEAD")
	rep, err := migrateStep(ctx, s, migrateBatch, migrateCommits, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Moved != 3 || rep.Absorbed != 1 || rep.Held != 1 || rep.Commits != 1 {
		t.Fatalf("report %+v", rep)
	}
	fs := factsByName(t, s.Dir)
	for _, old := range []string{"old-build", "old-port", "old-deploy-rule", "old-start-block"} {
		f := fs[old]
		w := fs[f.Meta.SupersededBy]
		if w == nil || w.Meta.Scope != "projects/widget" || w.Meta.SupersededBy != "" || w.Meta.Retired != "" {
			t.Errorf("%s points at %q, not a live widget fact", old, f.Meta.SupersededBy)
		}
		if f.Meta.Updated == "" {
			t.Errorf("%s: updated not set", old)
		}
	}
	if fs["old-port"].Meta.SupersededBy != "widget-port" {
		t.Errorf("the restating fact was copied: %s", fs["old-port"].Meta.SupersededBy)
	}
	c := fs[fs["old-build"].Meta.SupersededBy]
	if c.Meta.Origin != "old-build" || c.Meta.Created != "2026-10-01" || strings.Join(c.Meta.AppliesTo.Repos, ",") != "widget,other" || strings.TrimSpace(c.Body) != "make release." {
		t.Errorf("the copy %+v", c.Meta)
	}
	rule := fs[fs["old-deploy-rule"].Meta.SupersededBy]
	if !fact.IsRuling(rule) || rule.Meta.Source.OwnerQuote == "" {
		t.Errorf("the ruling's copy is no ruling: %+v", rule.Meta.Source)
	}
	if w := fs[fs["old-start-block"].Meta.SupersededBy]; fact.OwnerWordsOf(w) == "" {
		t.Errorf("the words were lost: %+v", w.Meta.Source)
	}
	if held := fs["old-tenant-queues"]; held.Meta.SupersededBy != "" {
		t.Errorf("the contradicted ruling moved: %+v", held.Meta)
	}
	asks := openFolderAsks(s.Dir)
	if len(asks) != 1 || asks[0].Fact != "old-tenant-queues" || !strings.Contains(asks[0].Reason, "[[widget-queue]] in the folder it would move to contradicts it") {
		t.Fatalf("asks %+v", asks)
	}
	if n := gitIn(t, s.Dir, "rev-list", "--count", head+"..HEAD"); n != "1" {
		t.Fatalf("%s commits", n)
	}
	msg := gitIn(t, s.Dir, "log", "-1", "--format=%B")
	if !strings.HasPrefix(msg, "curator: migrate: projects/widget-old -> projects/widget (5 fact(s): 3 moved, 1 absorbed, 1 held)") || !strings.Contains(msg, "old-port -> absorbed by widget-port") {
		t.Fatalf("message %q", msg)
	}
	if fs, err := lint.Run(s.Dir); err != nil || len(fs) != 0 {
		t.Fatalf("lint %v %v", fs, err)
	}
	again := gitIn(t, s.Dir, "rev-parse", "HEAD")
	if rep, err := migrateStep(ctx, s, migrateBatch, migrateCommits, false, nil); err != nil || rep.Moved+rep.Absorbed+rep.Held != 0 || gitIn(t, s.Dir, "rev-parse", "HEAD") != again {
		t.Fatalf("second run: %+v %v", rep, err)
	}
}

// tacit rescope moves what a plan names into a scope the checks accept.
func TestRescopeMovesAPlansFacts(t *testing.T) {
	s := migrateHome(t)
	put := &fact.Fact{Path: "user/widget-tip.md", Meta: fact.Meta{Name: "widget-tip", Description: "Restart the widget with make restart", Type: "user_standard", Scope: "user",
		Source: fact.Source{Harness: "curator", Host: "box", Kind: "observed"}, Created: "2026-10-02"}, Body: "make restart.\n"}
	writeFactFile(t, s.Dir, put)
	facts, _, _ := fact.Walk(s.Dir)
	writeIndexesForTest(t, s.Dir, facts)
	gitIn(t, s.Dir, "add", "-A")
	gitIn(t, s.Dir, "commit", "-q", "-m", "tip")
	plan := filepath.Join(t.TempDir(), "plan.jsonl")
	os.WriteFile(plan, []byte(`{"path": "user/widget-tip.md", "scope": ["evidence", "projects/widget"]}
{"name": "widget-port", "to": "projects/nowhere"}
{"name": "widget-port", "to": "user or machines/<host> (no repository)"}
`), 0o644)
	if code := runRescope([]string{"--plan", plan}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	fs := factsByName(t, s.Dir)
	if w := fs[fs["widget-tip"].Meta.SupersededBy]; w == nil || w.Meta.Scope != "projects/widget" {
		t.Fatalf("not moved: %+v", fs["widget-tip"].Meta)
	}
	if fs["widget-port"].Meta.SupersededBy != "" {
		t.Fatal("a refused target moved a fact")
	}
	if subj := gitIn(t, s.Dir, "log", "-1", "--format=%s"); !strings.HasPrefix(subj, "curator: rescope: 1 fact(s) moved") {
		t.Fatalf("subject %q", subj)
	}
	if fs, err := lint.Run(s.Dir); err != nil || len(fs) != 0 {
		t.Fatalf("lint %v %v", fs, err)
	}
}
