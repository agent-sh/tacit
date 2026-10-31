package main

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/search"
	"github.com/agent-sh/tacit/client/internal/store"
)

// The prompt hook after a change of this machine's overlay: it answers from
// the index of the older overlay, builds nothing itself, and starts one
// background build; the index after that build holds the new note. Before,
// the prompt hook built the whole store's index for every overlay change
// and timed out.
func TestPromptHookReadsTheOlderIndexAndStartsTheBuild(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "facts")
	local := filepath.Join(root, "overlay")
	index := filepath.Join(root, "index")
	t.Setenv("TACIT_STORE", dir)
	t.Setenv("TACIT_INDEX", index)
	t.Setenv("TACIT_LOCAL", local)
	t.Setenv("TACIT_PROJECTS", filepath.Join(root, "projects"))
	t.Setenv("TACIT_STATE_DIR", filepath.Join(root, "state"))
	stubScrump(t)
	if code := runInit(nil); code != 0 {
		t.Fatalf("init: %d", code)
	}
	s := &store.Store{Dir: dir}
	ctx := context.Background()
	if _, err := addFact(ctx, s, "claude", fact.Meta{Name: "quokka-deploys", Description: "quokka deploys stall when the zebra cache is cold", Type: "machine_fact", Scope: "user"}, "Warm the zebra cache first."); err != nil {
		t.Fatal(err)
	}
	note := func(name, desc string) {
		p := filepath.Join(local, "claude@host-a", name+".md")
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("---\nname: "+name+"\ndescription: "+desc+"\ntype: machine_fact\nscope: user\nsource:\n  harness: claude\n  host: host-a\n  kind: observed\ncreated: 2026-10-21\n---\n\nbody\n"), 0o644)
	}
	note("quokka-first", "quokka zebra cache note one")
	if ix, err := openIndex(ctx, s); err != nil {
		t.Fatal(err)
	} else {
		ix.Close()
	}
	files := func() []string {
		es, _ := os.ReadDir(index)
		var out []string
		for _, e := range es {
			out = append(out, e.Name())
		}
		return out
	}
	before := files()
	note("quokka-second", "quokka zebra cache note two")

	started := 0
	prev := startIndexBuild
	startIndexBuild = func() { started++ }
	defer func() { startIndexBuild = prev }()
	prompt := `{"session_id":"s1","cwd":"` + root + `","prompt":"why do quokka deploys stall on the zebra cache"}`
	hookOut(t, prompt, "prompt")
	if after := files(); started != 1 || strings.Join(after, " ") != strings.Join(before, " ") {
		t.Fatalf("started %d, files %v -> %v: want one start and no build in the hook", started, before, after)
	}
	names := func() (stale bool, got []string) {
		ix, err := openHookIndex(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		defer ix.Close()
		hits, err := ix.Query(ctx, search.Situation{Host: "host-a", Query: "quokka zebra cache"}, 10, false)
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range hits {
			got = append(got, h.Name)
		}
		sort.Strings(got)
		return ix.Stale(), got
	}
	if stale, got := names(); !stale || strings.Join(got, " ") != "quokka-first" || started != 2 {
		t.Fatalf("hook index: stale=%v %v started %d; want the older overlay's index", stale, got, started)
	}

	buildIndexForHooks(s) // what the started `tacit hook index` does
	if stale, got := names(); stale || strings.Join(got, " ") != "quokka-first quokka-second" || started != 2 {
		t.Fatalf("after the build: stale=%v %v started %d", stale, got, started)
	}
}
