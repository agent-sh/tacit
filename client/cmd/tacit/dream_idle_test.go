package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
	release()
	if code := runDream(nil); code != 0 || calls != 1 {
		t.Fatalf("free lock: exit %d, %d call(s)", code, calls)
	}
	if st, _ := dream.LoadState(statePath); st.Offsets[transcript] == 0 {
		t.Fatal("the cursor did not move")
	}
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
