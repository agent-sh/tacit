package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/fact"
)

// When the server refuses a curator batch, the run says why: the server's
// own lines (which file, which rule) are in the run's stderr, so the journal
// of a refused run can be acted on. v0.9.14 printed only the files it set
// aside, an empty list for a curated fact, run after run.
func TestCurateRefusedBatchSaysWhatTheServerSaid(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell CLI fixture")
	}
	stubScrump(t)
	r := newRig(t, rigOpts{repos: []string{"demo"}})
	hook, err := filepath.Abs("../../../server/priv/hooks/pre-receive")
	if err != nil {
		t.Fatal(err)
	}
	// The server sets TACIT_WRITER from the key that pushed.
	wrap := "#!/bin/sh\nTACIT_WRITER=curator@host-a exec '" + hook + "'\n"
	if err := os.WriteFile(filepath.Join(r.bare, "hooks", "pre-receive"), []byte(wrap), 0o755); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TACIT_STATE_DIR", "")
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("TACIT_STORE", r.clone)
	t.Setenv("TACIT_LOCAL", filepath.Join(home, "overlay"))
	t.Setenv("TACIT_PROJECTS", filepath.Join(home, "projects"))
	t.Setenv("TACIT_CURATE_RUNNER", "claude")
	t.Setenv("TACIT_JUDGE_RUNNER", "claude")
	if _, err := addFact(context.Background(), r.s, "muse", fact.Meta{Name: "n1", Description: "The quokka socket driver uses port 7781", Type: "machine_fact", Scope: "system", Source: fact.Source{Kind: "observed"}}, "The socket driver uses port 7781."); err != nil {
		t.Fatal(err)
	}
	// A curated fact that holds a token shape the hook refuses, built here so
	// no key-shaped string is in the repository.
	token := "ghp_" + strings.Repeat("Q", 36)
	split, _ := json.Marshal([]map[string]any{{"origin": "n1", "name": "socket-fact", "description": "The quokka socket driver uses port 7781",
		"type": "machine_fact", "scope": "system", "kind": "observed", "body": "The socket driver uses port 7781 with " + token + "."}})
	response, _ := json.Marshal(map[string]any{"result": string(split), "total_cost_usd": .01})
	fake := filepath.Join(home, "fake-cli")
	t.Setenv("TACIT_CLAUDE_COMMAND", fake)
	t.Setenv("FIXTURE_RESPONSE", string(response))
	if err := os.WriteFile(fake, []byte("#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' \"$FIXTURE_RESPONSE\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	var code int
	errs := captureStderr(t, func() { code = runCurate([]string{"--quick"}) })
	if code == 0 || strings.Contains(strings.Join(r.serverFiles(), ","), "socket-fact") {
		t.Fatalf("the refused batch landed or the run passed: exit %d\n%s", code, errs)
	}
	if !strings.Contains(errs, "curate: server refused the batch: tacit: system/socket-fact.md looks like it contains a secret") {
		t.Fatalf("the run does not say what the server said:\n%s", errs)
	}
}
