package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The PreCompact hook starts `tacit dream --session <transcript>` detached
// and returns at once: the harness waits for it before it compacts. The
// stand-in binary takes two seconds; the hook must not.
func TestPreCompactHookStartsTheDreamAndReturns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stand-in")
	}
	home, transcript, _ := dreamHome(t)
	out := filepath.Join(t.TempDir(), "args")
	fake := filepath.Join(t.TempDir(), "tacit")
	os.WriteFile(fake, []byte("#!/bin/sh\nsleep 2\nprintf '%s\\n' \"$*\" > '"+out+"'\n"), 0o700)
	previous := hookExe
	defer func() { hookExe = previous }()
	hookExe = func() (string, error) { return fake, nil }
	env := filepath.Join(home, ".config", "tacit", "env")
	os.MkdirAll(filepath.Dir(env), 0o700)
	os.WriteFile(env, nil, 0o600)

	began := time.Now()
	printed := hookOut(t, `{"session_id":"s1","transcript_path":"`+transcript+`","cwd":"`+home+`","hook_event_name":"PreCompact","trigger":"auto"}`, "pre-compact")
	if took := time.Since(began); took > 500*time.Millisecond {
		t.Fatalf("the hook waited for the dream: %s", took)
	}
	if printed != "" {
		t.Fatalf("the hook printed %q", printed)
	}
	var got []byte
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if got, _ = os.ReadFile(out); len(got) > 0 {
			break
		}
	}
	if want := "dream --session " + transcript + " --env-file " + env + "\n"; string(got) != want {
		t.Fatalf("started %q, want %q", got, want)
	}
	// No transcript: nothing to start, still exit 0.
	os.Remove(out)
	hookOut(t, `{"session_id":"s1","hook_event_name":"PreCompact"}`, "pre-compact")
	time.Sleep(2500 * time.Millisecond)
	if _, err := os.Stat(out); err == nil {
		t.Fatal("a dream started with no transcript")
	}
}

// The Hermes provider's on_pre_compress runs `tacit hook pre-compact` with
// the session's Hermes id (the dreamer reads the session from state.db by
// it), adds nothing to the summary prompt, follows a session switch, and
// does nothing in a subagent or cron context. The provider is the installed
// file, run by python3 against stand-ins for Hermes' modules.
func TestHermesPreCompressRunsThePreCompactHook(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stand-in")
	}
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	root := t.TempDir()
	calls := filepath.Join(root, "calls")
	fake := filepath.Join(root, "tacit")
	os.WriteFile(fake, []byte("#!/bin/sh\n{ printf '%s|' \"$*\"; cat; echo; } >> '"+calls+"'\n"), 0o700)
	t.Setenv("TACIT_SETTINGS", "")
	if err := installHermes(filepath.Join(root, "plugins", "tacit"), fake); err != nil {
		t.Fatal(err)
	}
	stubs := filepath.Join(root, "stubs")
	os.MkdirAll(filepath.Join(stubs, "agent"), 0o755)
	os.WriteFile(filepath.Join(stubs, "agent", "__init__.py"), nil, 0o644)
	os.WriteFile(filepath.Join(stubs, "agent", "memory_provider.py"), []byte(`class MemoryProvider:
    pass
class RecallStatus:
    def __init__(self, **kw):
        pass
def is_trivial_prompt(q):
    return False
`), 0o644)
	driver := filepath.Join(root, "driver.py")
	os.WriteFile(driver, []byte(`import sys
import tacit
cwd = sys.argv[1]
sub = tacit.TacitProvider()
sub.initialize("20261020_sub", cwd=cwd, agent_context="subagent")
assert sub.on_pre_compress([{"role": "user", "content": "x"}]) == ""
p = tacit.TacitProvider()
p.initialize("20261020_abc", cwd=cwd)
p.system_prompt_block()
assert p.on_pre_compress([{"role": "user", "content": "x"}]) == ""
p.on_session_switch("20261020_def", parent_session_id="20261020_abc")
assert p.on_pre_compress([]) == ""
`), 0o644)
	cmd := exec.Command(py, driver, root)
	cmd.Env = append(os.Environ(), "PYTHONPATH="+filepath.Join(root, "plugins")+string(os.PathListSeparator)+stubs, "TACIT_BIN="+fake)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("driver: %v\n%s", err, out)
	}
	b, _ := os.ReadFile(calls)
	var pre []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if args, stdin, _ := strings.Cut(l, "|"); args == "hook pre-compact" {
			var in hookInput
			if err := json.Unmarshal([]byte(stdin), &in); err != nil {
				t.Fatalf("payload %q: %v", stdin, err)
			}
			pre = append(pre, in.HermesSession)
		}
	}
	if strings.Join(pre, ",") != "20261020_abc,20261020_def" {
		t.Fatalf("pre-compact calls for %v; all calls:\n%s", pre, b)
	}
}
