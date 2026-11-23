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
	// Asked once: a second plan with the question raised leaves the ruling.
	for _, it := range expirePlan(facts, g, regs, home, "2026-11-21", map[string]bool{"keep-runner": true}) {
		if it.Name == "keep-runner" {
			t.Error("the ruling was asked again")
		}
	}
}
