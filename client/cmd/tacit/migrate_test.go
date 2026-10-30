package main

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
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
	// Two facts of one name (a store can hold them): the suppressed folder's moves too.
	put(&fact.Fact{Meta: fact.Meta{Name: "twin-note", Description: "The twin note lives in the gadget folder", Type: "history", Scope: "projects/gadget"}, Body: "Gadget twin.\n"})
	put(&fact.Fact{Meta: fact.Meta{Name: "twin-note", Description: "The twin note of the old widget is about its release", Type: "history", Scope: "projects/widget-old"}, Body: "Old widget twin.\n"})
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
	if rep.Moved != 4 || rep.Absorbed != 1 || rep.Held != 1 || rep.Commits != 1 {
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
	twin, _ := fact.Parse("projects/widget-old/twin-note.md", []byte(readFile(t, filepath.Join(s.Dir, "projects", "widget-old", "twin-note.md"))))
	if twin == nil || twin.Meta.SupersededBy == "" {
		t.Errorf("the suppressed folder's fact of a shared name did not move: %+v", twin)
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
	if !strings.HasPrefix(msg, "curator: migrate: projects/widget-old -> projects/widget (6 fact(s): 4 moved, 1 absorbed, 1 held)") || !strings.Contains(msg, "old-port -> absorbed by widget-port") {
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

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A held fact is tried again on every run, not left for the owner: once
// the contradiction that held it is gone (the verifier settled it) it moves
// and its question is closed; a ruling whose quote the ledger holds under
// another fact's name is backed by that line and moves on the first run.
func TestMigrateKeepsTryingHeldFacts(t *testing.T) {
	s := migrateHome(t)
	ctx := context.Background()
	q := "cache the widget builds for a day"
	writeFactFile(t, s.Dir, &fact.Fact{Path: "projects/widget-old/old-cache-rule.md", Meta: fact.Meta{Name: "old-cache-rule", Description: "Cache the widget builds for a day",
		Type: "user_standard", Scope: "projects/widget-old", Created: "2026-10-01",
		Source: fact.Source{Harness: "curator", Host: "box", Kind: "user", OwnerQuote: q, OwnerQuoteFrom: "claude:s9"}}, Body: "The owner: \"" + q + "\".\n"})
	lp := filepath.Join(s.Dir, filepath.FromSlash(ledger.QuotesPath))
	cur, _ := os.ReadFile(lp)
	os.WriteFile(lp, append(cur, ledger.Quote{Name: "cache-rule-earlier-name", Quote: q, From: "claude:s9", TurnHash: "dd", At: "2026-10-01"}.Line()...), 0o644)
	facts, _, _ := fact.Walk(s.Dir)
	writeIndexesForTest(t, s.Dir, facts)
	gitIn(t, s.Dir, "add", "-A")
	gitIn(t, s.Dir, "commit", "-q", "-m", "a ruling whose ledger line has another name")

	rep, err := migrateStep(ctx, s, migrateBatch, migrateCommits, false, nil)
	if err != nil || rep.Held != 1 {
		t.Fatalf("run 1: %+v %v", rep, err)
	}
	fs := factsByName(t, s.Dir)
	rule := fs[fs["old-cache-rule"].Meta.SupersededBy]
	if rule == nil || !fact.IsRuling(rule) || !rule.QuoteBacked {
		t.Fatalf("the rescued ruling's copy: %+v", rule)
	}
	// The verifier settles the contradiction; the next run moves the held
	// ruling and closes its question.
	wq := filepath.Join(s.Dir, "projects", "widget", "widget-queue.md")
	b, _ := os.ReadFile(wq)
	settled := *fs["widget-queue"]
	settled.Meta.ConflictsWith = nil
	nb, err := fact.PatchRelations(b, &settled)
	if err != nil || strings.Contains(string(nb), "old-tenant-queues") {
		t.Fatalf("fixture: %v\n%s", err, nb)
	}
	os.WriteFile(wq, nb, 0o644)
	gitIn(t, s.Dir, "commit", "-q", "-am", "verifier: no contradiction")
	rep, err = migrateStep(ctx, s, migrateBatch, migrateCommits, false, nil)
	if err != nil || rep.Moved != 1 || rep.Held != 0 {
		t.Fatalf("run 2: %+v %v", rep, err)
	}
	if asks := openFolderAsks(s.Dir); len(asks) != 0 {
		t.Fatalf("the question is still open: %+v", asks)
	}
	if !strings.Contains(readFile(t, filepath.Join(s.Dir, filepath.FromSlash(ledger.FoldersPath))), "moved: the reason it was held no longer holds") {
		t.Fatal("no close line")
	}
	// The fixture's own finding (its ledger line has another name) is all.
	fl, err := lint.Run(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fl {
		if !strings.Contains(fmt.Sprint(f), "projects/widget-old/old-cache-rule.md") {
			t.Errorf("lint: %v", f)
		}
	}
}

// Routing reads every ~ and absolute path a fact names, since only a git
// root here makes a repository of one, but none in a kernel's view or
// under /net, each once, the places first, so routes never crowd a place
// out of the cap; rotation and the ancestor index read places on a machine
// only, never a route.
func TestPathReaders(t *testing.T) {
	text := "Checkouts: /tmp/wt/tacit, /root/projects/tacit/main.go, (/workspace/app) and `~/projects/memra`; the API is /v1/messages, the forge /repos/agent-sh/tacit/pulls; logs in /var/log/x.log; " +
		"its cwd /proc/501543/cwd, /dev/fd/3, /sys/class/net, /dev and /net/buildbox/src/tacit; again /v1/messages; once ~//projects/old/a.md."
	if got, want := strings.Join(repoPaths(text, -1), " "), "~/projects/memra /var/log/x.log ~//projects/old/a.md /tmp/wt/tacit /root/projects/tacit/main.go /workspace/app /v1/messages /repos/agent-sh/tacit/pulls"; got != want {
		t.Errorf("repoPaths read %q, want %q", got, want)
	}
	if got, want := strings.Join(repoPaths(text, 4), " "), "~/projects/memra /var/log/x.log ~//projects/old/a.md /tmp/wt/tacit"; got != want {
		t.Errorf("repoPaths(4) read %q, want %q", got, want)
	}
	var places []string
	for _, m := range machinePathRx.FindAllStringSubmatch(text, -1) {
		places = append(places, strings.TrimRight(m[1], ".,;:)"))
	}
	if got, want := strings.Join(places, " "), "~/projects/memra /var/log/x.log ~//projects/old/a.md"; got != want {
		t.Errorf("machinePathRx read %q, want %q", got, want)
	}
}

// A fact's paths name repositories as SPEC's Orgs says: a checkout here
// names its repository when the catalog takes it and nothing otherwise, and
// a path with no checkout here names one only under a home's projects
// directory.
func TestPointedReposReadsCheckoutsAsTheCatalogTakesThem(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX paths")
	}
	root := t.TempDir()
	home := filepath.Join(root, "home")
	for k, v := range map[string]string{"HOME": home, "USERPROFILE": home, "TACIT_PROJECTS": filepath.Join(home, "projects"), "TACIT_STATE_DIR": "", "TMPDIR": filepath.Join(home, "tmp")} {
		t.Setenv(k, v)
	}
	checkout := func(dir, origin string) string {
		os.MkdirAll(dir, 0o755)
		gitIn(t, dir, "init", "-q", "-b", "main")
		gitIn(t, dir, "remote", "add", "origin", origin)
		return filepath.ToSlash(dir)
	}
	const billingURL = "https://github.com/workco/billing.git"
	checkout(filepath.Join(home, "projects", "billing"), billingURL)
	// A second main checkout of that origin: repoid names a clone for
	// neither (Mains.Canonical), so only the catalog's name and URLs say
	// the checkout outside home is billing.
	checkout(filepath.Join(home, "projects", "workco", "billing"), billingURL)
	mounted := checkout(filepath.Join(root, "mnt", "billing"), billingURL)
	other := checkout(filepath.Join(root, "scratch", "billing"), "https://github.com/vendor/billing.git")
	checkout(filepath.Join(home, "tmp", "projects", "billing", "vendor_lib"), "https://github.com/vendor/lib.git")
	storeDir := filepath.Join(root, "store")
	os.MkdirAll(storeDir, 0o755)
	recs := map[string]*record.Record{}
	catalog := knownCatalog(home, storeDir, false)
	rf := newRefiler(home, catalog, recs)
	for _, c := range []struct{ text, want string }{
		{"reads ~/projects/billing/config.yaml", "billing"},
		{"the same repository at " + mounted + "/config.yaml", "billing"},
		{"another repository under billing's name at " + other + "/main.go", ""},
		{"a scratch clone with no slug name at ~/tmp/projects/billing/vendor_lib/lib.go", ""},
		{"a scratch clone that is gone, ~/tmp/projects/billing/gone/lib.go", ""},
		{"on the Mac, /Users/owner/projects/billing/config.yaml", "billing"},
		{"the store's folder ~/.local/share/tacit/store/projects/billing/rule.md", ""},
		{"Claude's memory, ~/.claude/projects/-home-owner-projects-billing/memory/a.md", ""},
		{"the dashboard calls /api/projects/billing/settings", ""},
	} {
		got := pointedRepos(rf.cache, catalog, recs, &fact.Fact{Meta: fact.Meta{Description: c.text}})
		if g := strings.Join(slices.Sorted(maps.Keys(got)), ","); g != c.want {
			t.Errorf("%q points at %q, want %q", c.text, g, c.want)
		}
	}
}
