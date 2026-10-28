package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/dream"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/platform"
)

// With TACIT_DREAM_HELD_CHARS on, the dreamer's prompt lists the curated
// facts the window is about, so it does not write them again; unset, the
// list is off.
func TestDreamPromptListsTheFactsAlreadyHeld(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	dir := filepath.Join(root, "store")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("TACIT_STATE_DIR", "")
	t.Setenv("TACIT_STORE", dir)
	t.Setenv("TACIT_INDEX", filepath.Join(root, "index"))
	t.Setenv("TACIT_LOCAL", filepath.Join(root, "overlay"))
	t.Setenv("TACIT_PROJECTS", filepath.Join(root, "projects"))
	t.Setenv("TACIT_DREAM_HELD_CHARS", "on")
	stubScrump(t)
	if code := runInit(nil); code != 0 {
		t.Fatal(code)
	}
	held := &fact.Fact{Path: "system/fixture-driver-async.md", Meta: fact.Meta{Name: "fixture-driver-async", Description: "The fixture driver requires asynchronous reads.", Type: "machine_fact", Scope: "system", Source: fact.Source{Kind: "observed", Harness: "curator"}}, Body: "The fixture driver requires asynchronous reads.\n"}
	b, _ := held.Encode()
	os.MkdirAll(filepath.Join(dir, "system"), 0o755)
	os.WriteFile(filepath.Join(dir, held.Path), b, 0o644)
	write := func(name string) string {
		p := filepath.Join(home, ".claude/projects/demo/"+name+".jsonl")
		os.MkdirAll(filepath.Dir(p), 0o700)
		os.WriteFile(p, []byte(`{"type":"user","message":{"content":"Check the fixture controller read mode."}}`+"\n"+`{"type":"assistant","cwd":"/home/u/projects/demo","message":{"content":[{"type":"text","text":"The fixture driver requires asynchronous reads."}]}}`+"\n"+strings.Repeat(" ", 5000)+"\n"), 0o600)
		return p
	}
	a := write("a")
	statePath := filepath.Join(home, ".local/state/tacit/dreamer.json")
	dream.SaveState(statePath, &dream.State{Offsets: map[string]int64{a: 0}})
	var prompts []string
	old := dreamRun
	t.Cleanup(func() { dreamRun = old })
	dreamRun = func(_ context.Context, _, prompt string) (string, float64, int, error) {
		prompts = append(prompts, prompt)
		return `[]`, .01, 10, nil
	}
	if code := runDream(nil); code != 0 {
		t.Fatalf("dream exit %d", code)
	}
	if len(prompts) != 1 || !strings.Contains(prompts[0], "ALREADY HELD") || !strings.Contains(prompts[0], "- The fixture driver requires asynchronous reads.") {
		t.Fatalf("prompts: %q", prompts)
	}
	var m dream.Meter
	lb, _ := os.ReadFile(filepath.Join(platform.StateDir(home), "dreamer.log"))
	if json.Unmarshal([]byte(strings.TrimSpace(string(lb))), &m) != nil || m.Held != 1 || m.HeldChars == 0 {
		t.Fatalf("meter: %+v", m)
	}

	// Off: the default.
	t.Setenv("TACIT_DREAM_HELD_CHARS", "")
	b2 := write("b")
	st, _ := dream.LoadState(statePath)
	st.Offsets[b2] = 0
	dream.SaveState(statePath, st)
	if code := runDream(nil); code != 0 {
		t.Fatalf("dream exit %d", code)
	}
	if len(prompts) != 2 || strings.Contains(prompts[1], "ALREADY HELD") {
		t.Fatalf("off: %d prompt(s)", len(prompts))
	}
}
