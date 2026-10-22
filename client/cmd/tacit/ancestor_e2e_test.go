package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/assess"
	"github.com/agent-sh/tacit/client/internal/curate"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/platform"
	"github.com/agent-sh/tacit/client/internal/record"
	"github.com/agent-sh/tacit/client/internal/repoid"
	"github.com/agent-sh/tacit/client/internal/store"
)

// ancestorModel answers like a model that judges each folder alone: every
// folder stands on its own (active, citing a session or commit; one with
// independence evidence cites it only when the "cite" set says so). Sent
// back, it takes the fold code's refusal names.
type ancestorModel struct {
	prompts []string
	cite    map[string]bool
}

var (
	keyRx     = regexp.MustCompile(`independence \[([^\]]+)\]`)
	foldToRx  = regexp.MustCompile(`set suppressed_by to (projects/[a-z0-9-]+)`)
	folderLRx = regexp.MustCompile(`(?m)^- (projects/[a-z0-9-]+): (.*)$`)
)

func (m *ancestorModel) run(_ context.Context, _, prompt string) (string, float64, error) {
	m.prompts = append(m.prompts, prompt)
	blocks := map[string]string{}
	var order []string
	for _, b := range blockRx.FindAllStringSubmatch(prompt, -1) {
		blocks[b[1]] = b[2]
		order = append(order, b[1])
	}
	decide := func(folder string) assess.Decision {
		body := blocks[folder]
		cite := commitCiteRx.FindString(body)
		if cite == "" {
			cite = citeInRx.FindString(body)
		}
		d := assess.Decision{Folder: folder, Kind: "repo", Status: "active", Description: "what " + folder + " is", Why: "worked on lately, " + cite}
		if m.cite[folder] {
			if k := keyRx.FindStringSubmatch(body); k != nil {
				d.Independent = assess.Keys{k[1]}
			}
		}
		return d
	}
	var out []assess.Decision
	if i := strings.Index(prompt, "CODE REFUSED THESE DECISIONS"); i >= 0 {
		for _, l := range folderLRx.FindAllStringSubmatch(prompt[i:], -1) {
			d := decide(l[1])
			if f := foldToRx.FindStringSubmatch(l[2]); f != nil {
				d.SuppressedBy = f[1]
			}
			out = append(out, d)
		}
	} else {
		for _, f := range order {
			out = append(out, decide(f))
		}
	}
	b, _ := json.Marshal(out)
	return string(b), 0.01, nil
}

// Find the ancestor, end to end on a fresh store: a removed worktree's
// folder is folded by code from the relation the registry kept; a dated task
// folder whose name holds a repository's, and a folder whose paths sit in
// that repository's tree, are presumed part of it, so standing alone is
// refused and they fold; a repository of its own (own commits) whose name
// holds another's stands alone only by citing that; and the folders of one
// ancestor are asked in one batch with it.
func TestAncestorsAreFoundAndDecidedTogether(t *testing.T) {
	home, _, _ := checkedHome(t)
	s := &store.Store{Dir: filepath.Join(home, "store")}
	p := filepath.Join(home, "projects")
	widget := filepath.Join(p, "widget")
	// A worktree of widget a session worked in, then removed.
	wt := filepath.Join(home, "work", "widget-hotfix-3")
	gitIn(t, widget, "worktree", "add", "-q", "-b", "hotfix-3", wt)
	reg := filepath.Join(platform.StateDir(home), repoid.RegistryFile)
	r, ok := repoid.OfMachine(wt, home)
	if !ok || r.Name != "widget" {
		t.Fatalf("worktree: %+v", r)
	}
	if err := repoid.RecordCheckouts(reg, repoid.LoadMains(home, p), r); err != nil {
		t.Fatal(err)
	}
	gitIn(t, widget, "worktree", "remove", "--force", wt)
	// widget-cli: a repository of its own, own commits.
	cli := filepath.Join(p, "widget-cli")
	os.MkdirAll(cli, 0o755)
	gitIn(t, cli, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(cli, "cli.go"), []byte("package main\n"), 0o644)
	gitIn(t, cli, "add", "cli.go")
	cmd := exec.Command("git", "commit", "-q", "-m", "start widget-cli")
	cmd.Dir = cli
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("commit: %v %s", err, out)
	}
	add := func(scope, name, session, desc string) {
		writeFactFile(t, s.Dir, &fact.Fact{Path: scope + "/" + name + ".md", Meta: fact.Meta{Name: name, Description: desc, Type: "history", Scope: scope,
			Source: fact.Source{Harness: "curator", Host: "box", Session: session, Kind: "observed"}, Created: "2026-10-11"}, Body: desc + ".\n"})
	}
	add("projects/widget-hotfix-3", "hotfix-3-note", "s-h1", "The hotfix branch drops the retry on timeouts")
	add("projects/wt-widget-rework-20261004", "rework-note", "s-r1", "The rework moves the settings page to one column")
	add("projects/harness", "harness-runs", "s-hr", "The benchmark harness writes its runs as one JSON line each")
	add("projects/widget", "widget-harness-location", "s-w9", "The benchmark harness lives at ~/projects/widget/harness/run.py")
	add("projects/widget-cli", "cli-flags", "s-c1", "The widget CLI reads its flags from the environment first")
	repoid.RecordWorks(reg, []repoid.WorkItem{{Root: widget, Session: "s-w9", By: "edit"}, {Root: cli, Session: "s-c1", By: "commit"}})
	facts, _, _ := fact.Walk(s.Dir)
	writeIndexesForTest(t, s.Dir, facts)
	gitIn(t, s.Dir, "add", "-A")
	gitIn(t, s.Dir, "commit", "-q", "-m", "ancestors")

	m := &ancestorModel{cite: map[string]bool{"projects/widget-cli": true}}
	prev := assessRun
	assessRun = m.run
	t.Cleanup(func() { assessRun = prev })
	rep := &curate.Report{}
	if errs := assessStep(context.Background(), s, assessOpts{model: "opus"}, rep); errs != 0 {
		t.Fatalf("%d errors", errs)
	}
	recs, errs := record.Walk(s.Dir)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	all := strings.Join(m.prompts, "\n=====\n")
	// The removed worktree: code's, from git's metadata the registry kept.
	if r := recs["projects/widget-hotfix-3"]; r == nil || r.SuppressedBy != "projects/widget" || !strings.HasPrefix(r.Was[0].Why, "code: its name is the directory of a worktree of widget") {
		t.Fatalf("worktree folder: %+v", r)
	}
	if strings.Contains(all, "### folder=projects/widget-hotfix-3\n") {
		t.Error("the model was asked about a folder git's metadata settles")
	}
	// Presumed part of widget: standing alone refused, folded.
	for _, f := range []string{"projects/wt-widget-rework-20261004", "projects/harness"} {
		if r := recs[f]; r == nil || r.SuppressedBy != "projects/widget" {
			t.Errorf("%s: %+v", f, r)
		}
	}
	if !strings.Contains(all, "wt-widget-rework-20261004 is related to projects/widget: it is presumed part of one of") {
		t.Errorf("no refusal of the related folder standing alone:\n%s", all[strings.Index(all, "CODE REFUSED"):])
	}
	if !strings.Contains(all, "1. projects/widget [path] (1 fact(s) about it or widget name paths with a harness directory inside widget's tree") {
		t.Errorf("no path ancestor for harness:\n%s", all)
	}
	// Its own repository, cited: stands alone.
	if r := recs["projects/widget-cli"]; r == nil || !r.Live() || r.Status != "active" {
		t.Errorf("widget-cli: %+v", r)
	}
	if !strings.Contains(all, "independence [commits]: 1 own commit(s) in its own checkout") {
		t.Error("widget-cli's own commits not shown as independence")
	}
	// One batch: widget and the folders presumed part of it.
	for _, p := range m.prompts {
		if strings.Contains(p, "### folder=projects/wt-widget-rework-20261004\n") {
			for _, f := range []string{"projects/widget", "projects/harness", "projects/widget-cli"} {
				if !strings.Contains(p, "### folder="+f+"\n") {
					t.Errorf("%s not in the batch of its ancestor's folders", f)
				}
			}
			break
		}
	}
}
