package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/fact"
)

// tacit expire's plan and writes: a machine fact whose path moved under the
// projects registry gets an in-place note, one whose path is gone with no
// new place is retired, a fact of a folder the registry marks dead is
// retired, the owner's ruling with a gone path gets a question, and facts
// whose paths are there, another machine's and those the probe's rules
// pass over are left alone.
func TestExpireRetiresGoneFixesMovedAndAsksTheOwner(t *testing.T) {
	home := t.TempDir()
	projects := filepath.Join(home, "projects")
	os.MkdirAll(filepath.Join(projects, "apps", "widget", "src"), 0o755)
	os.MkdirAll(filepath.Join(home, "notes"), 0o755)
	os.WriteFile(filepath.Join(projects, "projects.json"), []byte(`{"entries": {"apps/widget": {"essence": "x"}, "archive/oldtool": {"status": "dead"}}}`), 0o644)
	regs := readRegistries([]string{filepath.Join(projects, "projects.json")})
	mf := func(scope, name, desc string) *fact.Fact {
		return &fact.Fact{Path: scope + "/" + name + ".md", Meta: fact.Meta{Name: name, Scope: scope, Type: "machine_fact", Description: desc, Created: "2026-10-01",
			Source: fact.Source{Harness: "curator", Host: "host-a", Kind: "observed"}}, Body: desc + "\n"}
	}
	const ms = "machines/host-a"
	moved := mf(ms, "widget-src", "The widget sources live in ~/projects/widget/src on this box")
	gone := mf(ms, "scratch-runner", "The nightly runner script is ~/.cache/task-x/run.sh")
	here := mf(ms, "notes-dir", "Notes are kept in ~/notes")
	other := mf("machines/host-b", "other-box", "The cache on host-b is ~/.cache/task-y")
	said := mf(ms, "old-dir-said-gone", "The old ~/.cache/task-z directory was removed")
	dead := mf("projects/oldtool", "oldtool-build", "oldtool builds with make release")
	rule := mf(ms, "keep-runner", "Run the nightly job with ~/.cache/task-w/job.sh only")
	rule.Audited, rule.QuoteBacked = true, true
	rule.Meta.Source.Kind, rule.Meta.Source.OwnerQuote, rule.Meta.Source.OwnerQuoteFrom = "user", "run the nightly job with that script only", "claude:s1"
	project := mf("projects/widget", "widget-fixture-dir", "Widget test fixtures are in ~/.cache/widget-fixtures")
	facts := []*fact.Fact{moved, gone, here, other, said, dead, rule, project}
	g := newGoneProbe("host-a", home, machineNames(facts))
	g.writer = true
	items := expirePlan(facts, g, regs, home, "2026-11-20", nil)
	got := map[string]string{}
	for _, it := range items {
		got[it.Name] = it.Action + ":" + it.Why
	}
	want := map[string]string{
		"widget-src":         "fix:moved",
		"scratch-runner":     "retire:gone",
		"oldtool-build":      "retire:dead",
		"keep-runner":        "ask:gone",
		"widget-fixture-dir": "retire:gone",
	}
	if len(got) != len(want) {
		t.Fatalf("plan = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %q, want %q", k, got[k], v)
		}
	}
	// The writes: the note, the retirements, the question.
	dir := t.TempDir()
	for _, f := range facts {
		b, err := f.Encode()
		if err != nil {
			t.Fatal(err)
		}
		os.MkdirAll(filepath.Join(dir, filepath.Dir(f.Path)), 0o755)
		os.WriteFile(filepath.Join(dir, f.Path), b, 0o644)
	}
	files, msg, err := expireFiles(dir, items, "2026-11-20")
	if err != nil {
		t.Fatal(err)
	}
	if b := string(files[moved.Path]); !strings.Contains(b, "> superseded 2026-11-20: ~/projects/widget/src -> ~/projects/apps/widget/src") || strings.Contains(b, "retired:") {
		t.Errorf("moved fact: %s", b)
	}
	for _, f := range []*fact.Fact{gone, dead, project} {
		if p, err := fact.Parse(f.Path, files[f.Path]); err != nil || p.Meta.Retired != "2026-11-20" {
			t.Errorf("%s not retired: %v", f.Meta.Name, err)
		}
	}
	if _, ok := files[rule.Path]; ok {
		t.Error("the owner's ruling was written")
	}
	if q := string(files["review/2026-11-20-expire.md"]); !strings.Contains(q, "- check? [[keep-runner]] ("+ms+") outcome=stale on=2026-11-20") {
		t.Errorf("question: %q", q)
	}
	if !strings.HasPrefix(msg, "curator: expire: 3 retired (2 paths gone, 1 project dead), 1 fixed (paths moved), 1 question(s)") {
		t.Errorf("message: %s", msg)
	}
	// Idempotent: with the note written, the moved fact is no item again.
	moved2 := *moved
	moved2.Body = moved.Body + "\n" + strings.Join(items[indexOf(items, "widget-src")].Notes, "\n") + "\n"
	for _, it := range expirePlan([]*fact.Fact{&moved2}, newGoneProbe("host-a", home, machineNames(facts)), regs, home, "2026-11-21", nil) {
		t.Errorf("the moved fact came back after its note: %+v", it)
	}
	// Asked once: a second plan with the question raised leaves the ruling.
	for _, it := range expirePlan(facts, g, regs, home, "2026-11-21", map[string]bool{"keep-runner": true}) {
		if it.Name == "keep-runner" {
			t.Error("the ruling was asked again")
		}
	}
}

func indexOf(items []expireItem, name string) int {
	for i, it := range items {
		if it.Name == name {
			return i
		}
	}
	return -1
}

// The registry's moves and dead folders hold only where they must: a path
// gone inside a live entry is not "moved" to an entry of the same name
// elsewhere; a move needs its old place gone; a name one registry marks
// dead and another holds live is no dead folder; a fact the owner endorsed,
// one with a quote of theirs and a statement are never retired.
func TestExpireMovesAndDeadFoldersHoldOnlyWhereTheyMust(t *testing.T) {
	home := t.TempDir()
	projects := filepath.Join(home, "projects")
	for _, d := range []string{"apps/widget", "tools/build/out", "apps/gizmo/src", "gizmo"} {
		os.MkdirAll(filepath.Join(projects, filepath.FromSlash(d)), 0o755)
	}
	os.WriteFile(filepath.Join(projects, "projects.json"), []byte(`{"entries": {"apps/widget": {}, "tools/build": {}, "apps/gizmo": {}, "archive/sprocket": {"status": "dead"}}}`), 0o644)
	data := filepath.Join(home, "data")
	os.MkdirAll(data, 0o755)
	os.WriteFile(filepath.Join(data, "data.json"), []byte(`{"entries": {"sets/sprocket": {}}}`), 0o644)
	regs := readRegistries([]string{filepath.Join(projects, "projects.json"), filepath.Join(data, "data.json")})
	if _, ok := moved(regs, filepath.Join(projects, "apps", "widget", "build", "out")); ok {
		t.Error("a path gone inside a live entry was moved to another entry's name")
	}
	if _, ok := moved(regs, filepath.Join(projects, "gizmo", "src")); ok {
		t.Error("a move whose old place is still there")
	}
	if np, ok := moved(regs, filepath.Join(projects, "widget", "x")); ok {
		t.Errorf("a move to a path that is not there: %s", np)
	}
	if d := deadFolders(regs); len(d) != 0 {
		t.Errorf("a name another registry holds live is dead: %v", d)
	}
	mf := func(name, desc string, src fact.Source) *fact.Fact {
		src.Harness, src.Host = "curator", "host-a"
		if src.Kind == "" {
			src.Kind = "observed"
		}
		return &fact.Fact{Path: "machines/host-a/" + name + ".md", Meta: fact.Meta{Name: name, Scope: "machines/host-a", Type: "machine_fact", Description: desc, Created: "2026-10-01", Source: src}, Body: desc + "\n"}
	}
	endorsed := mf("endorsed-dir", "The job dir is ~/.cache/job-a", fact.Source{Endorsed: "d1"})
	endorsed.EndorsedBacked = true
	quoted := mf("quoted-dir", "The job dir is ~/.cache/job-b", fact.Source{OwnerQuote: "the job dir is that cache dir", OwnerQuoteFrom: "claude:s1"})
	st := mf("said-dir", "The job dir is ~/.cache/job-c", fact.Source{OwnerWords: "the job dir", OwnerWordsFrom: "claude:s1"})
	st.Meta.Type = fact.StatementType
	// A gone path does not settle a record of what happened, a fact about a
	// place elsewhere too, or a fact that names a path still here.
	record := mf("job-dir-record", "The nightly job wrote its report to ~/.cache/job-d", fact.Source{})
	record.Meta.Type = "history"
	elsewhere := mf("job-dir-mirror", "The job report is mirrored from ~/.cache/job-e to https://example.org/reports", fact.Source{})
	partly := mf("job-dirs-two", "Job inputs are in ~/projects/apps and outputs in ~/.cache/job-f", fact.Source{})
	facts := []*fact.Fact{endorsed, quoted, st, record, elsewhere, partly}
	g := newGoneProbe("host-a", home, machineNames(facts))
	for _, it := range expirePlan(facts, g, regs, home, "2026-11-20", nil) {
		if it.Action != expireAsk {
			t.Errorf("%s: %s, want a question or nothing", it.Name, it.Action)
		}
		switch it.Name {
		case "said-dir", "job-dir-record", "job-dir-mirror", "job-dirs-two":
			t.Errorf("%s is an item: %+v", it.Name, it)
		}
	}
}

// A home whose projects directory is a symlink: the registry read through
// either path finds the move, and the note names the new path in the
// fact's own ~ form.
func TestExpireMoveThroughASymlinkedRoot(t *testing.T) {
	home := t.TempDir()
	real := filepath.Join(home, "disk", "projects")
	os.MkdirAll(filepath.Join(real, "apps", "widget", "src"), 0o755)
	if err := os.Symlink(real, filepath.Join(home, "projects")); err != nil {
		t.Skip("no symlinks here")
	}
	os.WriteFile(filepath.Join(real, "projects.json"), []byte(`{"entries": {"apps/widget": {}}}`), 0o644)
	f := &fact.Fact{Path: "machines/host-a/widget-src.md", Meta: fact.Meta{Name: "widget-src", Scope: "machines/host-a", Type: "machine_fact",
		Description: "The widget sources live in ~/projects/widget/src on this box", Created: "2026-10-01", Source: fact.Source{Harness: "curator", Host: "host-a", Kind: "observed"}}, Body: "x\n"}
	for _, reg := range []string{filepath.Join(home, "projects", "projects.json"), filepath.Join(real, "projects.json")} {
		regs := readRegistries([]string{reg})
		items := expirePlan([]*fact.Fact{f}, newGoneProbe("host-a", home, nil), regs, home, "2026-11-20", nil)
		if len(items) != 1 || items[0].Action != expireFix || len(items[0].Notes) != 1 {
			t.Fatalf("registry %s: %+v", reg, items)
		}
		if want := "> superseded 2026-11-20: ~/projects/widget/src -> ~/disk/projects/apps/widget/src"; items[0].Notes[0] != want {
			t.Errorf("registry %s: note %q, want %q", reg, items[0].Notes[0], want)
		}
	}
}
