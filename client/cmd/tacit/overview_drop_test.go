package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/curate"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/lint"
	"github.com/agent-sh/tacit/client/internal/modelrun"
	"github.com/agent-sh/tacit/client/internal/overview"
	"github.com/agent-sh/tacit/client/internal/record"
)

// overviewRig is a store at the current format with folders of three facts
// each (four for the first: the fixture's own note), each with a live record,
// pushed behind the server's real pre-receive hook, which sees this
// machine's curator key.
func overviewRig(t *testing.T, repos ...string) *rig {
	t.Helper()
	stubScrump(t)
	r := newRig(t, rigOpts{repos: repos[:1]})
	for _, repo := range repos {
		for _, n := range []string{"port", "build", "deploy"} {
			name := repo + "-" + n
			os.MkdirAll(filepath.Join(r.clone, "projects", repo), 0o755)
			os.WriteFile(filepath.Join(r.clone, "projects", repo, name+".md"), []byte(rotFile(name, "projects/"+repo, "design_decision", "the "+name+" fact", "the "+name+" fact", "2026-09-01", "")), 0o644)
		}
		writeRecordFile(t, r.clone, &record.Record{Folder: "projects/" + repo, Kind: record.KindRepo, Name: repo, Description: "the " + repo + " service", Status: record.StatusActive})
	}
	os.MkdirAll(filepath.Join(r.clone, "review"), 0o755)
	os.WriteFile(filepath.Join(r.clone, filepath.FromSlash(ledger.FormatPath)), ledger.Format{Version: ledger.FormatVersion, At: "x"}.Encode(), 0o644)
	r.commitIndex(r.clone, "facts")
	gitIn(t, r.clone, "push", "-q", "origin", "HEAD:main")
	hook, err := filepath.Abs("../../../server/priv/hooks/pre-receive")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(hook); err != nil {
		t.Fatal(err)
	}
	// The server sets TACIT_WRITER from the key that pushed; here it is the
	// curator key of host-a.
	wrap := "#!/bin/sh\nTACIT_WRITER=curator@host-a exec '" + hook + "'\n"
	if err := os.WriteFile(filepath.Join(r.bare, "hooks", "pre-receive"), []byte(wrap), 0o755); err != nil {
		t.Fatal(err)
	}
	return r
}

// citingModel answers an overview prompt with a body that cites the prompt's
// facts, or, when refuse is set, with one code refuses (an uncited
// paragraph). It counts its calls.
func citingModel(t *testing.T, refuse *bool, calls *int) {
	t.Helper()
	prev := overviewRun
	t.Cleanup(func() { overviewRun = prev })
	overviewRun = func(_ context.Context, _, prompt string) (string, float64, error) {
		*calls++
		if *refuse {
			return "What this folder holds, with no citation.", 0, nil
		}
		var cites []string
		for _, m := range promptFactRx.FindAllStringSubmatch(prompt, -1) {
			cites = append(cites, "[["+m[1]+"]]")
		}
		return "What this folder holds " + strings.Join(cites, " ") + ".\n\n- It builds before it deploys " + cites[0] + ".", 0, nil
	}
}

func runOverviewStep(t *testing.T, r *rig, max int) (int, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	n, dropped, errs := overviewStep(ctx, r.s, "m", max, false, &curate.Report{ModelCalls: &modelrun.Trace{}})
	if errs > 0 {
		t.Fatalf("overview step: %d error(s)", errs)
	}
	return n, dropped
}

func l19(t *testing.T, dir string) []string {
	t.Helper()
	fs, err := lint.Run(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, f := range fs {
		if f.Rule == "L19" {
			out = append(out, f.String())
		}
	}
	return out
}

// serverHas reports whether the server's main has rel.
func (r *rig) serverHas(rel string) bool {
	for _, f := range r.serverFiles() {
		if f == rel {
			return true
		}
	}
	return false
}

// An overview whose folder has no live facts left, or fewer than the
// overview minimum, is removed in one curator commit the server's hook
// accepts, with no model call; every fact stays, and lint L19 holds nothing
// against the store. A folder still due one keeps its overview.
func TestOverviewStepRemovesOverviewsNoLongerDue(t *testing.T) {
	r := overviewRig(t, "widget", "gadget", "gizmo")
	refuse, calls := false, 0
	citingModel(t, &refuse, &calls)
	if n, d := runOverviewStep(t, r, 10); n != 3 || d != 0 {
		t.Fatalf("first run: %d written, %d removed", n, d)
	}
	// widget: every fact retired. gadget: one of three retired, two left.
	retire := func(repo string, names ...string) {
		for _, n := range names {
			p := filepath.Join(r.clone, "projects", repo, repo+"-"+n+".md")
			if n == "note" {
				p = filepath.Join(r.clone, "projects", repo, repo+"-note.md")
			}
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			os.WriteFile(p, []byte(strings.Replace(string(data), "\n---\n", "\nretired: 2026-10-27\nupdated: 2026-10-27\n---\n", 1)), 0o644)
		}
	}
	retire("widget", "note", "port", "build", "deploy")
	retire("gadget", "port")
	r.commitAs("retire")
	before := r.serverFiles()
	calls = 0
	if _, d := runOverviewStep(t, r, 10); d != 2 {
		t.Fatalf("removed %d, want 2", d)
	}
	if calls != 0 {
		t.Fatalf("removing took %d model call(s)", calls)
	}
	for _, rel := range []string{"projects/widget/_overview.md", "projects/gadget/_overview.md"} {
		if r.serverHas(rel) {
			t.Fatalf("%s is still on the server", rel)
		}
	}
	if !r.serverHas("projects/gizmo/_overview.md") {
		t.Fatal("gizmo, still due an overview, lost it")
	}
	// Only the two overviews (and indexes the commit regenerates) left the tree.
	after := map[string]bool{}
	for _, f := range r.serverFiles() {
		after[f] = true
	}
	for _, f := range before {
		if !after[f] && filepath.Base(f) != overview.File {
			t.Fatalf("%s left the store", f)
		}
	}
	subj := strings.TrimSpace(gitIn(t, r.clone, "log", "-1", "--format=%an %s"))
	if subj != "curator curator: overview: removed 2 stale overview(s): projects/gadget (2 live fact(s), fewer than 3), projects/widget (no live facts)" {
		t.Fatalf("commit %q", subj)
	}
	if n := strings.TrimSpace(gitIn(t, r.clone, "rev-list", "--count", "HEAD", "^HEAD~1")); n != "1" {
		t.Fatalf("commits %s", n)
	}
	if fs := l19(t, r.clone); len(fs) > 0 {
		t.Fatalf("lint: %v", fs)
	}
	// Nothing left to remove: no commit.
	head := r.serverHead()
	if _, d := runOverviewStep(t, r, 10); d != 0 || r.serverHead() != head {
		t.Fatalf("a second run removed %d", d)
	}
}

// An overview citing a fact the store no longer has (a removed derived fact)
// is lint L19 red: it is rewritten before any other candidate, and when its
// rewrite is refused twice it is removed rather than left red.
func TestOverviewStepRewritesOrRemovesRedOverviews(t *testing.T) {
	r := overviewRig(t, "widget", "gadget")
	refuse, calls := false, 0
	citingModel(t, &refuse, &calls)
	if n, _ := runOverviewStep(t, r, 1); n != 1 {
		t.Fatalf("first run wrote %d", n)
	}
	written := "projects/gadget/_overview.md" // a folder with none: the larger first
	if !r.serverHas(written) {
		written = "projects/widget/_overview.md"
	}
	red := func() {
		p := filepath.Join(r.clone, filepath.FromSlash(written))
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(p, []byte(strings.TrimRight(string(b), "\n")+"\n- Where the code lives [[gone-module-map]].\n"), 0o644)
		r.commitAs("an overview citing a removed fact")
		if fs := l19(t, r.clone); len(fs) != 1 || !strings.Contains(fs[0], "[[gone-module-map]] names no fact") {
			t.Fatalf("lint before: %v", fs)
		}
	}
	red()
	// One rewrite a run: the red overview goes before the folder with none.
	if n, d := runOverviewStep(t, r, 1); n != 1 || d != 0 {
		t.Fatalf("written %d, removed %d", n, d)
	}
	if subj := strings.TrimSpace(gitIn(t, r.clone, "log", "-1", "--format=%s")); !strings.HasPrefix(subj, "curator: overview: "+strings.TrimSuffix(written, "/_overview.md")+" ") {
		t.Fatalf("rewrote %q, not the red one", subj)
	}
	if fs := l19(t, r.clone); len(fs) > 0 {
		t.Fatalf("lint after the rewrite: %v", fs)
	}
	// Red again, and the model's rewrite refused twice: removed.
	red()
	refuse, calls = true, 0
	n, d := runOverviewStep(t, r, 1)
	if n != 0 || d != 1 || calls != 2 {
		t.Fatalf("written %d, removed %d, %d call(s)", n, d, calls)
	}
	if r.serverHas(written) {
		t.Fatalf("%s is still on the server", written)
	}
	if subj := strings.TrimSpace(gitIn(t, r.clone, "log", "-1", "--format=%s")); !strings.Contains(subj, "[[gone-module-map]], which the store no longer has, and the rewrite was refused twice") {
		t.Fatalf("subject %q", subj)
	}
	if fs := l19(t, r.clone); len(fs) > 0 {
		t.Fatalf("lint after the removal: %v", fs)
	}
}

// A curator step may remove only a folder's generated overview or index:
// a fact, a record or a file outside a scope root is refused before
// anything is written, and the store does not move.
func TestCuratorChangeRemovesOnlyGeneratedFiles(t *testing.T) {
	for rel, ok := range map[string]bool{
		"projects/widget/_overview.md":     true,
		"projects/widget/_index.md":        true,
		"areas/build/sub/_overview.md":     true,
		"projects/widget/widget-port.md":   false,
		"projects/widget/_record.md":       false,
		"derived/widget/_overview.md":      false,
		"MEMORY.md":                        false,
		"inbox/claude@host/_overview.md":   false,
		"projects/../keys/_overview.md":    false,
		"projects/widget/_overview.md.bak": false,
	} {
		if removableRel(rel) != ok {
			t.Errorf("removableRel(%q) = %v", rel, !ok)
		}
	}
	r := overviewRig(t, "widget")
	head := r.serverHead()
	for _, rel := range []string{"projects/widget/widget-port.md", "projects/widget/_record.md"} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		_, err := curatorChange(ctx, r.s, func() (map[string][]byte, []string, string, error) {
			return nil, []string{"projects/widget/_index.md", rel}, "curator: remove " + rel, nil
		})
		cancel()
		if err == nil || !strings.Contains(err.Error(), "refusing to remove "+rel) {
			t.Fatalf("removing %s: %v", rel, err)
		}
		if _, err := os.Stat(filepath.Join(r.clone, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
	}
	if _, err := os.Stat(filepath.Join(r.clone, "projects", "widget", "_index.md")); err != nil {
		t.Fatalf("the index went with the refused plan: %v", err)
	}
	if r.serverHead() != head {
		t.Fatal("a refused removal moved the store")
	}
	if out := strings.TrimSpace(gitIn(t, r.clone, "status", "--porcelain")); out != "" {
		t.Fatalf("the clone is dirty:\n%s", out)
	}
}

// The order is by tier, and the tier decides alone: a red overview goes
// before a forced change whose share passes 1 (a red overview with share 0
// that sorted behind many forced folders was never reached), a forced
// change before a folder with none, and that before an unforced share.
func TestOverviewCandidatesOrderByTier(t *testing.T) {
	old := &overview.Overview{}
	none := []*fact.Fact{{}, {}, {}}
	cs := []overviewCand{
		{folder: "a-share", old: old, change: overview.Change{Share: 1.6}},
		{folder: "b-new", facts: none},
		{folder: "c-forced", old: old, change: overview.Change{Share: 1.52, Forced: []string{"x"}}},
		{folder: "d-red", old: old, red: true, change: overview.Change{Forced: []string{"x"}}},
		{folder: "e-forced", old: old, change: overview.Change{Share: 0.1, Forced: []string{"x"}}},
		{folder: "f-new", facts: append(none, &fact.Fact{})},
	}
	sortCands(cs)
	var got []string
	for _, c := range cs {
		got = append(got, c.folder)
	}
	if want := "d-red c-forced e-forced f-new b-new a-share"; strings.Join(got, " ") != want {
		t.Fatalf("order %v, want %s", got, want)
	}
}
