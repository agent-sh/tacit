package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/dream"
	"github.com/agent-sh/tacit/client/internal/platform"
)

// dreamHome is a local store and one Claude transcript with a delta worth
// dreaming, its cursor at 0.
func dreamHome(t *testing.T) (home, transcript, statePath string) {
	t.Helper()
	root := t.TempDir()
	home = filepath.Join(root, "home")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("TACIT_STATE_DIR", "")
	t.Setenv("TACIT_STORE", filepath.Join(root, "store"))
	t.Setenv("TACIT_INDEX", filepath.Join(root, "index"))
	t.Setenv("TACIT_LOCAL", filepath.Join(root, "overlay"))
	t.Setenv("TACIT_PROJECTS", filepath.Join(root, "projects"))
	t.Setenv("TACIT_DREAM_MAX_RUNTIME", "")
	stubScrump(t)
	if code := runInit(nil); code != 0 {
		t.Fatal(code)
	}
	transcript = filepath.Join(home, ".claude/projects/demo/s1.jsonl")
	os.MkdirAll(filepath.Dir(transcript), 0o700)
	os.WriteFile(transcript, []byte(`{"type":"user","message":{"content":"Check the fixture read mode and record its working alternative."}}`+"\n"+`{"type":"assistant","message":{"content":[{"type":"text","text":"The fixture driver requires asynchronous reads."}]}}`+"\n"+strings.Repeat(" ", 5000)+"\n"), 0o600)
	statePath = filepath.Join(home, ".local/state/tacit/dreamer.json")
	dream.SaveState(statePath, &dream.State{Offsets: map[string]int64{transcript: 0}})
	return home, transcript, statePath
}

// A dream that finds another holding dreamer.lock exits 0 with "busy": no
// model call, no note recovered, the cursor file and the meter log as they
// were. Once the lock is free the same run dreams.
func TestDreamLockMakesASecondDreamExitBusy(t *testing.T) {
	home, transcript, statePath := dreamHome(t)
	before, _ := os.ReadFile(statePath)
	previous := dreamRun
	defer func() { dreamRun = previous }()
	calls := 0
	dreamRun = func(context.Context, string, string) (string, float64, int, error) {
		calls++
		return `[{"description":"The fixture driver requires asynchronous reads.","type":"machine_fact","kind":"observed","applies_to":{}}]`, .01, 10, nil
	}
	release, err := tryJobLock(platform.StateDir(home), "dreamer")
	if err != nil {
		t.Fatal(err)
	}
	// The full run waits (here 100ms, not 45 minutes) and then gives up as
	// busy, like the idle and session runs at once.
	defer func(d time.Duration) { dreamLockWait = d }(dreamLockWait)
	dreamLockWait = 100 * time.Millisecond
	for _, args := range [][]string{nil, {"--idle", "1ms"}, {"--session", transcript}} {
		if code := runDream(args); code != 0 {
			t.Fatalf("%v: busy dream exit %d", args, code)
		}
	}
	after, _ := os.ReadFile(statePath)
	if calls != 0 || string(after) != string(before) {
		t.Fatalf("a busy dream ran: %d call(s), state %s", calls, after)
	}
	if _, err := os.Stat(filepath.Join(platform.StateDir(home), "dreamer.log")); err == nil {
		t.Fatal("a busy dream wrote a meter line")
	}
	// The full run waits for the holder and dreams once the lock is free.
	dreamLockWait = time.Minute
	done := make(chan int, 1)
	go func() { done <- runDream(nil) }()
	time.Sleep(time.Second)
	select {
	case code := <-done:
		t.Fatalf("the full dream did not wait for the lock: exit %d", code)
	default:
	}
	release()
	select {
	case code := <-done:
		if code != 0 || calls != 1 {
			t.Fatalf("free lock: exit %d, %d call(s)", code, calls)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the full dream never took the lock")
	}
	if st, _ := dream.LoadState(statePath); st.Offsets[transcript] == 0 {
		t.Fatal("the cursor did not move")
	}
}

// A transcript whose window failed is not sent again by the idle and
// session runs (every 10 minutes, every compaction) for
// dream.FailureBackoff: its cursor stays, the full run retries it, and once
// the backoff has passed the idle run does too. Success clears the record.
func TestFailedTranscriptWaitsForTheFullRun(t *testing.T) {
	_, transcript, statePath := dreamHome(t)
	previous := dreamRun
	defer func() { dreamRun = previous }()
	calls, fail := 0, true
	dreamRun = func(context.Context, string, string) (string, float64, int, error) {
		calls++
		if fail {
			return "", .01, 10, errors.New("fixture model refused")
		}
		return `[]`, .01, 10, nil
	}
	quiet := time.Now().Add(-time.Hour)
	os.Chtimes(transcript, quiet, quiet)
	if code := runDream([]string{"--idle", "20m"}); code != 1 || calls != 1 {
		t.Fatalf("failing idle run: exit %d, %d call(s)", code, calls)
	}
	st, _ := dream.LoadState(statePath)
	if !st.RecentlyFailed(transcript, time.Now()) || st.Offsets[transcript] != 0 {
		t.Fatalf("after the failure: %+v", st)
	}
	for _, args := range [][]string{{"--idle", "20m"}, {"--session", transcript}} {
		if code := runDream(args); code != 0 || calls != 1 {
			t.Fatalf("%v within the backoff: exit %d, %d call(s)", args, code, calls)
		}
	}
	// The full run retries (and fails again).
	if code := runDream(nil); code != 1 || calls != 2 {
		t.Fatalf("full run: exit %d, %d call(s)", code, calls)
	}
	// Past the backoff the idle run takes it again, and success clears it.
	st, _ = dream.LoadState(statePath)
	st.FailedAt[transcript] = time.Now().Add(-dream.FailureBackoff - time.Minute).Format(time.RFC3339)
	dream.SaveState(statePath, st)
	fail = false
	if code := runDream([]string{"--idle", "20m"}); code != 0 || calls != 3 {
		t.Fatalf("past the backoff: exit %d, %d call(s)", code, calls)
	}
	if st, _ := dream.LoadState(statePath); st.Offsets[transcript] == 0 || len(st.FailedAt) != 0 {
		t.Fatalf("after success: %+v", st)
	}
}

// captureStderr runs fn and returns what it wrote to stderr.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	done := make(chan string)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	fn()
	w.Close()
	os.Stderr = old
	return <-done
}

// On a read-only clone, and on a clone of the server without the dreamer's
// key (a Mac that only reads), the idle job's dream and the session dream
// exit 0 with no model call and no stderr line (they run every 10 minutes
// and at every compaction); the full dream still refuses out loud. The
// PreCompact hook starts no dream there at all.
func TestIdleJobAndHooksDoNothingWhereTheDreamerCannotWrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stand-in")
	}
	home, transcript, statePath := dreamHome(t)
	before, _ := os.ReadFile(statePath)
	quiet := time.Now().Add(-time.Hour)
	os.Chtimes(transcript, quiet, quiet)
	previous := dreamRun
	defer func() { dreamRun = previous }()
	calls := 0
	dreamRun = func(context.Context, string, string) (string, float64, int, error) {
		calls++
		return `[]`, 0, 0, nil
	}
	started := filepath.Join(t.TempDir(), "started")
	fake := filepath.Join(t.TempDir(), "tacit")
	os.WriteFile(fake, []byte("#!/bin/sh\necho \"$*\" >> '"+started+"'\n"), 0o700)
	prevExe := hookExe
	defer func() { hookExe = prevExe }()
	hookExe = func() (string, error) { return fake, nil }
	defer func(f func(string) bool) { hasKey = f }(hasKey)
	hasKey = func(string) bool { return true }

	storeDir := os.Getenv("TACIT_STORE")
	setups := []struct {
		name  string
		setup func()
	}{
		{"read-only clone", func() { git(t, storeDir, "config", "tacit.readonly", "true") }},
		{"no dreamer key", func() {
			git(t, storeDir, "config", "--unset", "tacit.readonly")
			git(t, storeDir, "remote", "add", "origin", "https://tacit.invalid/git")
			hasKey = func(string) bool { return false }
		}},
	}
	for _, c := range setups {
		c.setup()
		for _, args := range [][]string{{"--idle", "20m"}, {"--session", transcript}} {
			code := 0
			if errs := captureStderr(t, func() { code = runDream(args) }); code != 0 || errs != "" {
				t.Fatalf("%s: %v: exit %d, stderr %q", c.name, args, code, errs)
			}
		}
		code := 0
		if errs := captureStderr(t, func() { code = runDream(nil) }); code != 2 || !strings.Contains(errs, "nothing done") {
			t.Fatalf("%s: full dream: exit %d, stderr %q", c.name, code, errs)
		}
		hookOut(t, `{"session_id":"s1","transcript_path":"`+transcript+`","cwd":"`+home+`","hook_event_name":"PreCompact"}`, "pre-compact")
	}
	time.Sleep(500 * time.Millisecond)
	if b, err := os.ReadFile(started); err == nil {
		t.Fatalf("the hook started a dream: %s", b)
	}
	if after, _ := os.ReadFile(statePath); calls != 0 || string(after) != string(before) {
		t.Fatalf("%d call(s), state %s", calls, after)
	}
	// The control: with the key, the same hook starts the dream.
	hasKey = func(string) bool { return true }
	hookOut(t, `{"session_id":"s1","transcript_path":"`+transcript+`","cwd":"`+home+`","hook_event_name":"PreCompact"}`, "pre-compact")
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if b, _ := os.ReadFile(started); len(b) > 0 {
			return
		}
	}
	t.Fatal("with the key, the hook started no dream")
}

// --session dreams only that transcript, and on a machine with no cursors it
// records none (a full first run records them all).
func TestDreamSessionOneTranscript(t *testing.T) {
	home, transcript, statePath := dreamHome(t)
	other := filepath.Join(home, ".claude/projects/demo/s2.jsonl")
	b, _ := os.ReadFile(transcript)
	os.WriteFile(other, b, 0o600)
	dream.SaveState(statePath, &dream.State{Offsets: map[string]int64{transcript: 0, other: 0}})
	previous := dreamRun
	defer func() { dreamRun = previous }()
	calls := 0
	dreamRun = func(context.Context, string, string) (string, float64, int, error) {
		calls++
		return `[]`, 0, 0, nil
	}
	if code := runDream([]string{"--session", transcript}); code != 0 || calls != 1 {
		t.Fatalf("exit %d, %d call(s)", code, calls)
	}
	st, _ := dream.LoadState(statePath)
	if st.Offsets[transcript] == 0 || st.Offsets[other] != 0 {
		t.Fatalf("cursors %v", st.Offsets)
	}
	os.Remove(statePath)
	if code := runDream([]string{"--session", transcript}); code != 0 || calls != 1 {
		t.Fatalf("fresh machine: exit %d, %d call(s)", code, calls)
	}
	if _, err := os.Stat(statePath); err == nil {
		t.Fatal("a session dream on a fresh machine recorded cursors")
	}
}

func TestIdleFlagStandsAlone(t *testing.T) {
	for _, c := range []struct{ in, want []string }{
		{[]string{"--idle"}, []string{"--idle=20m0s"}},
		{[]string{"--idle", "--dry-run"}, []string{"--idle=20m0s", "--dry-run"}},
		{[]string{"--idle", "5m", "--dry-run"}, []string{"--idle", "5m", "--dry-run"}},
		{[]string{"--idle=5m"}, []string{"--idle=5m"}},
	} {
		if got := idleDefault(c.in); !slices.Equal(got, c.want) {
			t.Errorf("%v: %v, want %v", c.in, got, c.want)
		}
	}
}

// The idle run end to end: a transcript written a minute ago waits (no model
// call, no meter line, its cursor unmoved); once it has been quiet for the
// idle time it is dreamed, and the meter line says how the run chose it.
func TestDreamIdleRun(t *testing.T) {
	home, transcript, statePath := dreamHome(t)
	previous := dreamRun
	defer func() { dreamRun = previous }()
	calls := 0
	dreamRun = func(context.Context, string, string) (string, float64, int, error) {
		calls++
		return `[]`, .01, 10, nil
	}
	now := time.Now()
	os.Chtimes(transcript, now.Add(-time.Minute), now.Add(-time.Minute))
	if code := runDream([]string{"--idle", "20m"}); code != 0 || calls != 0 {
		t.Fatalf("busy transcript: exit %d, %d call(s)", code, calls)
	}
	logPath := filepath.Join(platform.StateDir(home), "dreamer.log")
	if _, err := os.Stat(logPath); err == nil {
		t.Fatal("an idle run with nothing to take wrote a meter line")
	}
	if st, _ := dream.LoadState(statePath); st.Offsets[transcript] != 0 {
		t.Fatal("the busy transcript's cursor moved")
	}
	os.Chtimes(transcript, now.Add(-21*time.Minute), now.Add(-21*time.Minute))
	if code := runDream([]string{"--idle", "20m"}); code != 0 || calls != 1 {
		t.Fatalf("quiet transcript: exit %d, %d call(s)", code, calls)
	}
	var meter dream.Meter
	if b, err := os.ReadFile(logPath); err != nil || json.Unmarshal(b, &meter) != nil || meter.Mode != "idle" || meter.Calls != 1 {
		t.Fatalf("meter %+v %v", meter, err)
	}
}
