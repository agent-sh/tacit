package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/lint"
	"github.com/agent-sh/tacit/client/internal/store"
)

// treeHome is a fresh local store on a machine with one checkout, widget
// (in the projects directory), and a stand-in curator CLI that answers each
// note from FIXTURE_<NOTE> and a prompt that sends a refused answer back
// from FIXTURE_FIXED. Every prompt is logged.
func treeHome(t *testing.T) (home string, s *store.Store, prompts func() []string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell CLI fixture")
	}
	home, _ = filepath.EvalSymlinks(t.TempDir())
	t.Setenv("HOME", home)
	t.Setenv("TACIT_STATE_DIR", "")
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("TACIT_STORE", filepath.Join(home, "store"))
	t.Setenv("TACIT_INDEX", filepath.Join(home, "index"))
	t.Setenv("TACIT_LOCAL", filepath.Join(home, "overlay"))
	t.Setenv("TACIT_PROJECTS", filepath.Join(home, "projects"))
	t.Setenv("TACIT_CURATE_RUNNER", "claude")
	t.Setenv("TACIT_JUDGE_RUNNER", "claude")
	stubScrump(t)
	widget := filepath.Join(home, "projects", "widget")
	if out, err := exec.Command("git", "init", "-q", widget).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	if code := runInit(nil); code != 0 {
		t.Fatalf("init %d", code)
	}
	s = &store.Store{Dir: filepath.Join(home, "store")}
	log := filepath.Join(home, "prompts")
	t.Setenv("FIXTURE_PROMPTS", log)
	fake := filepath.Join(home, "fake-cli")
	script := `#!/bin/sh
in=$(cat)
printf '%s\n\x1e\n' "$in" >> "$FIXTURE_PROMPTS"
case "$in" in
*"CODE REFUSED THAT ANSWER"*) printf '%s\n' "$FIXTURE_FIXED" ;;
*"name=widget-note"*) printf '%s\n' "$FIXTURE_WIDGET" ;;
*"name=box-note"*) printf '%s\n' "$FIXTURE_BOX" ;;
*) printf '%s\n' "$FIXTURE_USER" ;;
esac
`
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TACIT_CLAUDE_COMMAND", fake)
	t.Cleanup(func(f func(string) bool) func() { return func() { hasKey = f } }(hasKey))
	return home, s, func() []string {
		b, _ := os.ReadFile(log)
		var out []string
		for _, p := range strings.Split(string(b), "\n\x1e\n") {
			if strings.TrimSpace(p) != "" {
				out = append(out, p)
			}
		}
		return out
	}
}

// answer is the CLI's JSON for a split answer of entries.
func answer(t *testing.T, entries ...map[string]any) string {
	t.Helper()
	b, _ := json.Marshal(entries)
	r, _ := json.Marshal(map[string]any{"result": string(b), "total_cost_usd": .01})
	return string(r)
}

func entry(origin, name, desc, typ, scope string, repos ...string) map[string]any {
	if repos == nil {
		repos = []string{}
	}
	return map[string]any{"origin": origin, "name": name, "description": desc, "type": typ, "scope": scope, "kind": "observed", "applies_to": map[string]any{"paths": []string{}, "repos": repos, "hosts": []string{}, "commands": []string{}}, "body": desc}
}

// A fresh store from nothing: agents add notes, the curator splits them
// with a model whose first answer names a folder that is no repository and
// a type code generates; code refuses it, the model fixes it on the one
// retry, and the batch lands with every folder index committed with it. The
// tree tool then answers from those index files alone.
func TestFreshStoreCuratesIntoAnIndexedTree(t *testing.T) {
	noAssess(t)
	home, s, prompts := treeHome(t)
	host := store.ShortHost()
	ctx := context.Background()
	add := func(name, desc, typ, scope string) {
		t.Helper()
		if _, err := addFact(ctx, s, "muse", fact.Meta{Name: name, Description: desc, Type: typ, Scope: scope, Source: fact.Source{Kind: "observed"}}, desc); err != nil {
			t.Fatalf("add %s: %v", name, err)
		}
	}
	add("widget-note", "widget's server listens on port 7781 and its scheduler test is flaky", "machine_fact", "projects/widget")
	add("box-note", "this box has two disks and no swap", "machine_fact", "machines/"+host)
	add("user-note", "release notes are written in the past tense", "user_standard", "user")

	// One batch holds the three notes. What the audit found models write: a
	// task folder's name as the project, hot_spot on a model's fact, a folder
	// name in applies_to.repos.
	box := entry("box-note", "box-disks", "This box has two disks and no swap", "machine_fact", "machines/"+host)
	user := entry("user-note", "release-notes-past-tense", "Release notes are written in the past tense", "user_standard", "user")
	t.Setenv("FIXTURE_WIDGET", answer(t,
		entry("widget-note", "widget-port", "The widget server listens on port 7781", "machine_fact", "projects/tk-013-w1"),
		entry("widget-note", "widget-flaky-scheduler", "The widget scheduler test is flaky under load; rerun it alone", "hot_spot", "projects/widget", "client"),
		box, user))
	// The fixed answer gives the scheduler test as this machine's absolute
	// path inside the widget checkout: code makes it repo-relative.
	flaky := entry("widget-note", "widget-flaky-scheduler", "The widget scheduler test is flaky under load; rerun it alone", "past_mistake", "projects/widget")
	flaky["applies_to"].(map[string]any)["paths"] = []string{filepath.Join(home, "projects", "widget", "sched", "sched_test.go")}
	t.Setenv("FIXTURE_FIXED", answer(t,
		entry("widget-note", "widget-port", "The widget server listens on port 7781", "machine_fact", "projects/widget", "widget"),
		flaky, box, user))
	countSteps(t)
	if code := runCurate(nil); code != 0 {
		t.Fatalf("curate exit %d", code)
	}

	ps := prompts()
	retries := 0
	for _, p := range ps {
		if strings.Contains(p, "CODE REFUSED THAT ANSWER") {
			retries++
			for _, want := range []string{"names no known repository", "projects/tk-013-w1", "generated by code", `"client"`} {
				if !strings.Contains(p, want) {
					t.Errorf("the retry prompt does not say %q", want)
				}
			}
		}
	}
	if retries != 1 {
		t.Fatalf("%d retries in %d prompts", retries, len(ps))
	}

	for _, rel := range []string{"projects/widget/widget-port.md", "projects/widget/widget-flaky-scheduler.md", "machines/" + host + "/box-disks.md", "user/release-notes-past-tense.md"} {
		if _, err := os.Stat(filepath.Join(s.Dir, rel)); err != nil {
			t.Errorf("%s not written: %v", rel, err)
		}
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "projects", "tk-013-w1")); err == nil {
		t.Error("the refused scope was written")
	}
	b, _ := os.ReadFile(filepath.Join(s.Dir, "projects/widget/widget-flaky-scheduler.md"))
	if f, err := fact.Parse("x.md", b); err != nil || f.Meta.Type != "past_mistake" || len(f.Meta.AppliesTo.Repos) != 0 || strings.Join(f.Meta.AppliesTo.Paths, ",") != "sched/sched_test.go" {
		t.Errorf("the model's fixed entry is not what was written: %+v %v", f, err)
	}

	// Every folder has its index, committed (nothing left in the working tree).
	want := []string{"_index.md", "machines/_index.md", "machines/" + host + "/_index.md", "projects/_index.md", "projects/widget/_index.md", "user/_index.md"}
	tracked, _ := store.Git(ctx, s.Dir, "ls-files", "--", "*_index.md")
	for _, rel := range want {
		if !strings.Contains(tracked, rel) {
			t.Errorf("%s is not committed; tracked:\n%s", rel, tracked)
		}
	}
	if st, _ := store.Git(ctx, s.Dir, "status", "--porcelain"); strings.TrimSpace(st) != "" {
		t.Errorf("the clone is dirty after the curate:\n%s", st)
	}
	if findings, err := lint.Run(s.Dir); err != nil || len(findings) != 0 {
		t.Errorf("lint: %v %v", findings, err)
	}
	// tacit index --check agrees; a hand edit is caught and tacit index fixes it.
	if code := runIndex([]string{"--store", s.Dir, "--check"}); code != 0 {
		t.Errorf("index --check after the curate: exit %d", code)
	}
	os.WriteFile(filepath.Join(s.Dir, "user", "_index.md"), []byte("edited\n"), 0o644)
	if code := runIndex([]string{"--store", s.Dir, "--check"}); code != 1 {
		t.Errorf("index --check missed a hand edit: exit %d", code)
	}
	if code := runIndex([]string{"--store", s.Dir}); code != 0 {
		t.Errorf("index: exit %d", code)
	}
	if st, _ := store.Git(ctx, s.Dir, "status", "--porcelain"); strings.TrimSpace(st) != "" {
		t.Errorf("tacit index did not put the index back:\n%s", st)
	}
	idx, _ := os.ReadFile(filepath.Join(s.Dir, "projects/widget/_index.md"))
	for _, l := range []string{"# projects/widget", "2 facts", "`widget-port` (machine_fact): The widget server listens on port 7781", "`widget-flaky-scheduler` (past_mistake)"} {
		if !strings.Contains(string(idx), l) {
			t.Errorf("projects/widget index lacks %q:\n%s", l, idx)
		}
	}

	// The tree tool, for several folders and depths, from the index files.
	view := func(p string, depth int) string {
		t.Helper()
		out, err := treeView(s, p, depth, "widget", host)
		if err != nil {
			t.Fatalf("tree %q %d: %v", p, depth, err)
		}
		return out
	}
	heads := func(out string) []string {
		var hs []string
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, "=== ") {
				hs = append(hs, l)
			}
		}
		return hs
	}
	if got := heads(view("", 0)); len(got) != 1 || got[0] != "=== store root: _index.md ===" {
		t.Errorf("root at depth 0: %v", got)
	}
	full := view("", 3)
	if got := heads(full); len(got) != 6 {
		t.Errorf("root at depth 3: %v", got)
	}
	for _, s := range []string{"`widget-port`", "`box-disks`", "`release-notes-past-tense`"} {
		if !strings.Contains(full, s) {
			t.Errorf("root at depth 3 lacks %s", s)
		}
	}
	if got := heads(view("projects", 1)); len(got) != 2 {
		t.Errorf("projects at depth 1: %v", got)
	}
	if got := heads(view("user", 2)); len(got) != 1 {
		t.Errorf("user at depth 2: %v", got)
	}
	// Another session's repository is not listed, but read when named;
	// paths out of the store are refused.
	if out, err := treeView(s, "", 3, "gadget", host); err != nil || strings.Contains(out, "widget-port") {
		t.Errorf("another repository's folder was listed: %v\n%s", err, out)
	}
	if out, err := treeView(s, "projects/widget", 0, "gadget", host); err != nil || !strings.Contains(out, "`widget-port`") {
		t.Errorf("a named repository's folder: %v\n%s", err, out)
	}
	for _, p := range []string{"../", "inbox", "/etc"} {
		if out := treeToolText(s, p, nil); !strings.HasPrefix(out, "error: ") {
			t.Errorf("tacit_tree %q answered:\n%s", p, out)
		}
	}
	// The MCP answer with the defaults (root, depth 2: the scope folders'
	// own indexes) from inside widget; a folder's default is 3; an explicit
	// depth wins.
	t.Chdir(filepath.Join(home, "projects", "widget"))
	if out := treeToolText(s, "", nil); !strings.Contains(out, "`widget-port`") || len(heads(out)) != 6 {
		t.Errorf("tacit_tree defaults:\n%s", out)
	}
	if out := treeToolText(s, "projects", nil); len(heads(out)) != 2 {
		t.Errorf("tacit_tree projects default:\n%s", out)
	}
	zero := 0
	if out := treeToolText(s, "", &zero); len(heads(out)) != 1 {
		t.Errorf("tacit_tree root depth 0:\n%s", out)
	}
}

// tacit_add and `tacit add` refuse metadata that names nothing real, with a
// reason the agent can act on, and write nothing.
func TestAddRefusesWhatItCannotCheck(t *testing.T) {
	_, s, _ := treeHome(t)
	ctx := context.Background()
	for _, c := range []struct {
		m    fact.Meta
		want string
	}{
		{fact.Meta{Name: "a", Description: "x", Type: "machine_fact", Scope: "projects/tune"}, "names no known repository"},
		{fact.Meta{Name: "b", Description: "x", Type: "hot_spot", Scope: "projects/widget"}, "generated by code"},
		{fact.Meta{Name: "c", Description: "x", Type: "feedback", Scope: "user"}, "not a known type"},
		{fact.Meta{Name: "d", Description: "x", Type: "machine_fact", Scope: "machines/nowhere"}, "names no known machine"},
		{fact.Meta{Name: "e", Description: "Owner ruling: tags are signed", Type: "user_standard", Scope: "user"}, "says the owner ruled"},
		{fact.Meta{Name: "f", Description: "x", Type: "past_mistake", Scope: "projects/widget", AppliesTo: fact.AppliesTo{Paths: []string{"/abs/x"}}}, "is absolute"},
	} {
		c.m.Source.Kind = "observed"
		_, err := addFact(ctx, s, "muse", c.m, "body")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("add %s: %v, want %q", c.m.Name, err, c.want)
		}
	}
	if out, _ := store.Git(ctx, s.Dir, "ls-files", "inbox"); strings.TrimSpace(out) != "" {
		t.Errorf("a refused note was written:\n%s", out)
	}
	// What it can check passes; Widget is the same scope in lower case.
	if _, err := addFact(ctx, s, "muse", fact.Meta{Name: "ok", Description: "widget uses make", Type: "design_decision", Scope: "projects/Widget", Source: fact.Source{Kind: "observed"}}, "body"); err != nil {
		t.Fatalf("a valid note refused: %v", err)
	}
	// A path inside this machine's widget checkout is made relative to it;
	// a machine's fact keeps its absolute path.
	home, _ := os.UserHomeDir()
	rel, err := addFact(ctx, s, "muse", fact.Meta{Name: "paths", Description: "the widget server reads its port from config", Type: "design_decision", Scope: "projects/widget", AppliesTo: fact.AppliesTo{Paths: []string{filepath.Join(home, "projects", "widget", "cmd", "server", "main.go"), "~/projects/widget/config/*.yaml"}}, Source: fact.Source{Kind: "observed"}}, "body")
	if err != nil {
		t.Fatalf("a path inside the checkout refused: %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(s.Dir, rel))
	if f, err := fact.Parse(rel, b); err != nil || strings.Join(f.Meta.AppliesTo.Paths, ",") != "cmd/server/main.go,config/*.yaml" {
		t.Errorf("written paths: %+v %v", f, err)
	}
	if _, err := addFact(ctx, s, "muse", fact.Meta{Name: "box-paths", Description: "the box keeps its models in /srv/models", Type: "machine_fact", Scope: "machines/" + store.ShortHost(), AppliesTo: fact.AppliesTo{Paths: []string{"/srv/models/*"}}, Source: fact.Source{Kind: "observed"}}, "body"); err != nil {
		t.Errorf("a machine's absolute path refused: %v", err)
	}
}

// With memory_md off a curated write stops generating MEMORY.md and its
// commit removes the store's copy once; later writes name it no more, lint
// stays clean, tacit index --check agrees, and turning it back on brings it
// back. A fresh store with it off starts without one.
func TestMemoryMDOffStopsAndRemovesIt(t *testing.T) {
	home, s, _ := treeHome(t)
	ctx := context.Background()
	if _, err := os.Stat(filepath.Join(s.Dir, "MEMORY.md")); err != nil {
		t.Fatalf("the default store has no MEMORY.md: %v", err)
	}
	t.Setenv("TACIT_MEMORY_MD", "off")
	write := func(name string) {
		t.Helper()
		f := &fact.Fact{Path: "user/" + name + ".md", Meta: fact.Meta{Name: name, Description: "standard " + name, Type: "user_standard", Scope: "user", Source: fact.Source{Harness: "curator", Host: "box", Kind: "observed"}, Created: "2026-10-21"}, Body: "Standard " + name + ".\n"}
		writeFactFile(t, s.Dir, f)
		all, _, _ := fact.Walk(s.Dir)
		rels, err := regenIndexes(ctx, s.Dir, all, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.CommitPush(ctx, append(rels, f.Path), "curator: "+name, "curator"); err != nil {
			t.Fatalf("commit %s: %v (paths %v)", name, err, rels)
		}
	}
	write("first")
	if tracked, _ := store.Git(ctx, s.Dir, "ls-files", "--", "MEMORY.md"); strings.TrimSpace(tracked) != "" {
		t.Fatal("MEMORY.md is still tracked after a write with memory_md off")
	}
	write("second") // nothing to remove: the commit names no MEMORY.md
	if _, err := os.Stat(filepath.Join(s.Dir, "MEMORY.md")); err == nil {
		t.Fatal("MEMORY.md came back with memory_md off")
	}
	if findings, err := lint.Run(s.Dir); err != nil || len(findings) != 0 {
		t.Errorf("lint: %v %v", findings, err)
	}
	if code := runIndex([]string{"--store", s.Dir, "--check"}); code != 0 {
		t.Errorf("index --check with memory_md off: %d", code)
	}
	os.WriteFile(filepath.Join(s.Dir, "MEMORY.md"), []byte("stale\n"), 0o644)
	if code := runIndex([]string{"--store", s.Dir, "--check"}); code != 1 {
		t.Errorf("index --check missed a MEMORY.md with memory_md off: %d", code)
	}
	if code := runIndex([]string{"--store", s.Dir}); code != 0 {
		t.Fatal(code)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "MEMORY.md")); err == nil {
		t.Fatal("tacit index kept MEMORY.md with memory_md off")
	}
	t.Setenv("TACIT_MEMORY_MD", "on")
	write("third")
	if tracked, _ := store.Git(ctx, s.Dir, "ls-files", "--", "MEMORY.md"); strings.TrimSpace(tracked) == "" {
		t.Fatal("MEMORY.md did not come back with memory_md on")
	}
	// A fresh store with it off starts without one.
	t.Setenv("TACIT_MEMORY_MD", "off")
	fresh := &store.Store{Dir: filepath.Join(home, "fresh")}
	if err := fresh.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(fresh.Dir, "MEMORY.md")); err == nil {
		t.Fatal("a fresh store with memory_md off has a MEMORY.md")
	}
}
