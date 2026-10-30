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

	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/store"
)

// realRedact gives the test the store's own redactor, scrump through
// digest.RedactAllContext (the stand-in of stubScrump where scrump is
// absent), in place of the suite's canaryRedact.
func realRedact(t *testing.T) {
	t.Helper()
	prev := store.Redact
	store.Redact = digest.RedactAllContext
	t.Cleanup(func() { store.Redact = prev })
}

// The 2026-10-26 rebuild's curator wrote an imported memory's loopback
// llama-server key into a curated fact: the memory came into the inbox
// through a path that did not mask it, and the split copied the key into the
// fact's body. Every store write masks what it adds (SPEC.md, "Every write is
// masked"), so the fact the split writes holds no key, however its note came
// in. The key is synthetic; the canary part is the one the stand-in scrump
// masks where the real one is absent.
func TestTheCuratorWritesNoKeyItsNoteBroughtIn(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell CLI fixture")
	}
	home := t.TempDir()
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
	realRedact(t)
	if code := runInit(nil); code != 0 {
		t.Fatalf("init %d", code)
	}
	s := &store.Store{Dir: filepath.Join(home, "store")}
	key := "Zq7Vx9" + "Lm2Kp4Hc3Pz9Rk"
	// An imported memory as the rebuild wrote it: committed into the inbox
	// by hand, past every mask.
	rel := "inbox/codex@" + store.ShortHost() + "/memory/exact-serve-wrapper.md"
	note := "---\nname: exact-serve-wrapper\ndescription: The exact local serve wrapper is the source of truth for argv\ntype: machine_fact\nscope: machines/" + store.ShortHost() + "\nsource:\n  harness: codex\n  host: " + store.ShortHost() + "\n  kind: observed\ncreated: \"2026-06-05\"\n---\n\nPreserve the production flags: -fa on, --parallel 1, --jinja, `--api-key " + key + "`, --metrics.\n"
	p := filepath.Join(s.Dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(note), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "--", rel}, {"-c", "user.name=import", "-c", "user.email=import@example.test", "commit", "-q", "-m", "import: one memory", "--", rel}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = s.Dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	// The curator's model copies the note's flags into the fact, key and all.
	split, _ := json.Marshal([]map[string]any{{"origin": "exact-serve-wrapper", "name": "serve-wrapper-flags", "description": "The exact local serve wrapper is the source of truth for argv",
		"type": "machine_fact", "scope": "machines/" + store.ShortHost(), "kind": "observed",
		"body": "Production flags include -fa on, --parallel 1, --jinja, `--api-key " + key + "`, --metrics, plus the model paths in the wrapper."}})
	response, _ := json.Marshal(map[string]any{"result": string(split), "total_cost_usd": .01})
	fake := filepath.Join(home, "fake-cli")
	t.Setenv("FIXTURE_RESPONSE", string(response))
	if err := os.WriteFile(fake, []byte("#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' \"$FIXTURE_RESPONSE\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TACIT_CLAUDE_COMMAND", fake)
	t.Cleanup(func(f func(string) bool) func() { return func() { hasKey = f } }(hasKey))
	if code := runCurate([]string{"--quick"}); code != 0 {
		t.Fatalf("curate exit %d", code)
	}
	path, ok := findFact(s.Dir, "serve-wrapper-flags")
	if !ok {
		t.Fatal("the split's fact was not written")
	}
	frel, _ := filepath.Rel(s.Dir, path)
	cmd := exec.Command("git", "show", "HEAD:"+filepath.ToSlash(frel))
	cmd.Dir = s.Dir
	committed, err := cmd.Output()
	if err != nil {
		t.Fatalf("the fact is not committed: %v", err)
	}
	if strings.Contains(string(committed), "Zq7Vx9") {
		t.Fatalf("the curated fact was committed with the key:\n%s", committed)
	}
	if !strings.Contains(string(committed), "--metrics, plus the model paths in the wrapper") {
		t.Fatalf("the mask took more than the key:\n%s", committed)
	}
	cmd = exec.Command("git", "show", "HEAD:MEMORY.md")
	cmd.Dir = s.Dir
	if mem, err := cmd.Output(); err == nil && strings.Contains(string(mem), "Zq7Vx9") {
		t.Fatal("MEMORY.md holds the key")
	}
}

// An agent's add masks the note's description and body itself, before it
// writes them, so the store does not mask the note a second time: scrump
// takes about half a second to start, which every add would pay twice.
func TestAnAgentsAddIsNotMaskedTwice(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TACIT_STATE_DIR", "")
	t.Setenv("TACIT_STORE", filepath.Join(home, "store"))
	t.Setenv("TACIT_LOCAL", filepath.Join(home, "overlay"))
	t.Setenv("TACIT_PROJECTS", filepath.Join(home, "projects"))
	stubScrump(t)
	realRedact(t)
	if code := runInit(nil); code != 0 {
		t.Fatalf("init %d", code)
	}
	calls := 0
	prev := store.Redact
	store.Redact = func(ctx context.Context, texts []string) ([]string, error) { calls++; return prev(ctx, texts) }
	t.Cleanup(func() { store.Redact = prev })
	s := &store.Store{Dir: filepath.Join(home, "store")}
	if _, err := addFact(context.Background(), s, "claude", fact.Meta{Name: "walrus-port", Description: "The walrus fixture driver uses port 7781", Type: "machine_fact", Scope: "system", Source: fact.Source{Kind: "observed"}}, "The walrus fixture driver uses port 7781."); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("the store masked the add again (%d redactor calls)", calls)
	}
}
