package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/dream"
	"github.com/agent-sh/tacit/client/internal/hermes"
	"github.com/agent-sh/tacit/client/internal/hermes/hermestest"
	"github.com/agent-sh/tacit/client/internal/platform"
	"github.com/agent-sh/tacit/client/internal/store"
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
// does nothing in a subagent or cron context; its prefetch passes the id to
// `tacit hook prompt` (the window trigger), except in a subagent. The provider is the installed
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
p.prefetch("check the fixture read mode")
sub.prefetch("check the fixture read mode")
p.on_session_switch("20261020_def", parent_session_id="20261020_abc")
assert p.on_pre_compress([]) == ""
`), 0o644)
	cmd := exec.Command(py, driver, root)
	cmd.Env = append(os.Environ(), "PYTHONPATH="+filepath.Join(root, "plugins")+string(os.PathListSeparator)+stubs, "TACIT_BIN="+fake)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("driver: %v\n%s", err, out)
	}
	b, _ := os.ReadFile(calls)
	var pre, prompt []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		args, stdin, _ := strings.Cut(l, "|")
		if args != "hook pre-compact" && args != "hook prompt" {
			continue
		}
		var in hookInput
		if err := json.Unmarshal([]byte(stdin), &in); err != nil {
			t.Fatalf("payload %q: %v", stdin, err)
		}
		if args == "hook prompt" {
			prompt = append(prompt, in.HermesSession)
		} else {
			pre = append(pre, in.HermesSession)
		}
	}
	if strings.Join(pre, ",") != "20261020_abc,20261020_def" {
		t.Fatalf("pre-compact calls for %v; all calls:\n%s", pre, b)
	}
	// prefetch passes the Hermes id too (the window trigger), a subagent none.
	if strings.Join(prompt, ",") != "20261020_abc," {
		t.Fatalf("prompt calls for %q; all calls:\n%s", prompt, b)
	}
}

// fakeDreamer makes the hooks start a stand-in that appends its arguments to
// a file; started returns them, waiting up to want lines (0: a short wait,
// for "none started").
func fakeDreamer(t *testing.T) (started func(want int) []string) {
	t.Helper()
	log := filepath.Join(t.TempDir(), "started")
	fake := filepath.Join(t.TempDir(), "tacit")
	os.WriteFile(fake, []byte("#!/bin/sh\necho \"$*\" >> '"+log+"'\n"), 0o700)
	previous := hookExe
	t.Cleanup(func() { hookExe = previous })
	hookExe = func() (string, error) { return fake, nil }
	return func(want int) []string {
		wait := 300 * time.Millisecond
		if want > 0 {
			wait = 10 * time.Second
		}
		var lines []string
		for deadline := time.Now().Add(wait); ; time.Sleep(20 * time.Millisecond) {
			b, _ := os.ReadFile(log)
			lines = strings.Fields(strings.ReplaceAll(strings.TrimSpace(string(b)), " ", "_"))
			if want > 0 && len(lines) >= want || time.Now().After(deadline) {
				return lines
			}
		}
	}
}

// The prompt hook starts the session dream once the transcript's undreamed
// part (its size less its cursor) reaches windowBytes, and not below it; once
// per cursor; never while a dream holds dreamer.lock, for a transcript that
// failed within the backoff, or on a read-only clone. Each rule is checked
// against the same prompt with the rule's condition gone.
func TestPromptHookStartsTheDreamAtAFullWindow(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stand-in")
	}
	home, transcript, statePath := dreamHome(t)
	started := fakeDreamer(t)
	defer func(n int64) { windowBytes[digest.Claude] = n }(windowBytes[digest.Claude])
	fi, _ := os.Stat(transcript)
	prompt := `{"session_id":"s1","transcript_path":"` + transcript + `","cwd":"` + home + `","hook_event_name":"UserPromptSubmit","prompt":""}`
	want := "dream_--session_" + transcript
	setCursor := func(at int64) {
		st, _ := dream.LoadState(statePath)
		st.Offsets[transcript] = at
		dream.SaveState(statePath, st)
	}
	// A started dream is recorded at its cursor in dream-trigger.json when
	// the hook returns; the stand-in's own line may come later (a new
	// executable's first runs can be slow to start), so it is counted with a
	// wait, and once more at the end.
	triggers := filepath.Join(platform.StateDir(home), "dream-trigger.json")
	n := 0
	expect := func(what string, start bool) {
		t.Helper()
		before, _ := os.ReadFile(triggers)
		hookOut(t, prompt, "prompt")
		after, _ := os.ReadFile(triggers)
		if string(after) != string(before) != start {
			t.Fatalf("%s: dream-trigger.json %s -> %s, want a dream started: %v", what, before, after, start)
		}
		if start {
			n++
		}
		if got := started(n); len(got) < n || n > 0 && got[n-1] != want {
			t.Fatalf("%s: started %v, want %d dream(s) of %s", what, got, n, transcript)
		}
	}
	windowBytes[digest.Claude] = fi.Size() + 1
	expect("below a window", false)
	windowBytes[digest.Claude] = 1000
	expect("a full window", true)
	expect("the same cursor again", false)

	setCursor(10)
	release, err := tryJobLock(platform.StateDir(home), "dreamer")
	if err != nil {
		t.Fatal(err)
	}
	expect("a dream holds the lock", false)
	release()
	expect("the lock free", true)

	setCursor(20)
	st, _ := dream.LoadState(statePath)
	st.Failed(transcript, time.Now())
	dream.SaveState(statePath, st)
	expect("failed within the backoff", false)
	st.FailedAt = nil
	dream.SaveState(statePath, st)
	expect("no failure", true)

	setCursor(30)
	git(t, os.Getenv("TACIT_STORE"), "config", "tacit.readonly", "true")
	expect("a read-only clone", false)
	git(t, os.Getenv("TACIT_STORE"), "config", "--unset", "tacit.readonly")
	expect("a writable clone", true)

	time.Sleep(time.Second)
	if got := started(n); len(got) != n {
		t.Fatalf("started %d dream(s), want %d: %v", len(got), n, got)
	}

	// The check's cost on a machine with many cursors: a stat and a state
	// read, no digest, no model, no git when below the threshold.
	st, _ = dream.LoadState(statePath)
	for i := range 5000 {
		st.Offsets[filepath.Join(home, ".claude", "projects", "demo", strconv.Itoa(i)+".jsonl")] = int64(i) * 1000
	}
	dream.SaveState(statePath, st)
	windowBytes[digest.Claude] = fi.Size() + 1
	s, _ := store.Default()
	began := time.Now()
	for range 20 {
		windowDream(s, hookInput{TranscriptPath: transcript})
	}
	took := time.Since(began) / 20
	t.Logf("window check with 5,000 cursors: %s per prompt", took)
	if took > 100*time.Millisecond {
		t.Fatalf("the window check took %s per prompt", took)
	}
}

// A Hermes session (no transcript file) is measured in message content after
// its cursor, read from the store: the prompt hook starts its dream once
// that reaches windowHermesBytes.
func TestPromptHookStartsAHermesDreamAtAFullWindow(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stand-in")
	}
	home, _, statePath := dreamHome(t)
	started := fakeDreamer(t)
	db := filepath.Join(home, ".hermes", "state.db")
	os.MkdirAll(filepath.Dir(db), 0o700)
	t.Setenv("TACIT_HERMES_DB", db)
	h := hermestest.Create(t, db, hermestest.Session{ID: "20261020_abc", Source: "cli", StartedAt: 1})
	h.Add(hermestest.Message{Session: "20261020_abc", Role: "user", Content: strings.Repeat("check the fixture read mode ", 80), Timestamp: 2})
	st, _ := dream.LoadState(statePath)
	st.Offsets[hermes.Key(db, "20261020_abc")] = 0
	dream.SaveState(statePath, st)
	defer func(n int64) { windowHermesBytes = n }(windowHermesBytes)
	prompt := `{"session_id":"hermes-20261020-abc","hermes_session_id":"20261020_abc","cwd":"` + home + `","prompt":""}`
	windowHermesBytes = 5000
	hookOut(t, prompt, "prompt")
	if _, err := os.Stat(filepath.Join(platform.StateDir(home), "dream-trigger.json")); err == nil {
		t.Fatal("below a window, a dream started")
	}
	windowHermesBytes = 1000
	hookOut(t, prompt, "prompt")
	if got := started(1); len(got) != 1 || got[0] != "dream_--session_20261020_abc" {
		t.Fatalf("a full window: %v", got)
	}
}

// After a compaction (source compact) the start hook prints nothing: the
// compaction's summary carries what the session had, the start block
// included. Neither does a compact entry an earlier build of this branch
// installed (`session-start --compact`), nor a resume or a clear.
func TestStartHookPrintsNothingAfterACompaction(t *testing.T) {
	r := newRig(t, rigOpts{repos: []string{"demo"}, filler: 200, rules: 1})
	t.Setenv("TACIT_STORE", r.clone)
	t.Setenv("HOME", filepath.Join(r.root, "home"))
	t.Setenv("TACIT_STATE_DIR", "")
	t.Setenv("TACIT_FOLLOW_STATE", filepath.Join(r.root, "no-follower.json"))
	cwd := filepath.Join(r.projects, "demo")
	payload := func(session, source string) string {
		return `{"session_id":"` + session + `","cwd":"` + cwd + `","hook_event_name":"SessionStart","source":"` + source + `"}`
	}
	if start := hookOut(t, payload("s1", "startup"), "session-start"); !strings.Contains(start, "\n- ") {
		t.Fatalf("no start block:\n%s", start)
	}
	for i, c := range []struct {
		source string
		args   []string
	}{{"compact", nil}, {"compact", []string{"--compact"}}, {"resume", nil}, {"clear", nil}} {
		if out := hookOut(t, payload("s"+strconv.Itoa(i+2), c.source), append([]string{"session-start"}, c.args...)...); out != "" {
			t.Fatalf("source %s %v printed:\n%s", c.source, c.args, out)
		}
	}
}
