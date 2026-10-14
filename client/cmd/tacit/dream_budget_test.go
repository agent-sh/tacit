package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/agent-sh/tacit/client/internal/dream"
	"github.com/agent-sh/tacit/client/internal/store"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDreamBatchRuntimePausesAndResumes(t *testing.T) {
	for _, budget := range []bool{true, false} {
		t.Run(fmt.Sprint("batch_budget=", budget), func(t *testing.T) {
			root := t.TempDir()
			home := filepath.Join(root, "home")
			dir := filepath.Join(root, "store")
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("TACIT_STORE", dir)
			t.Setenv("TACIT_INDEX", filepath.Join(root, "index"))
			t.Setenv("TACIT_LOCAL", filepath.Join(root, "overlay"))
			t.Setenv("TACIT_PROJECTS", filepath.Join(root, "projects"))
			t.Setenv("TACIT_DREAM_MAX_RUNTIME", "")
			stubScrump(t)
			if code := runInit(nil); code != 0 {
				t.Fatal(code)
			}
			files := []string{filepath.Join(home, ".claude/projects/demo/a.jsonl"), filepath.Join(home, ".claude/projects/demo/b.jsonl")}
			offsets := map[string]int64{}
			for _, p := range files {
				os.MkdirAll(filepath.Dir(p), 0700)
				os.WriteFile(p, []byte(`{"type":"user","message":{"content":"Check the fixture read mode and record its working alternative."}}`+"\n"+`{"type":"assistant","message":{"content":[{"type":"text","text":"The fixture driver requires asynchronous reads."}]}}`+"\n"+strings.Repeat(" ", 5000)+"\n"), 0600)
				offsets[p] = 0
			}
			statePath := filepath.Join(home, ".local/state/tacit/dreamer.json")
			dream.SaveState(statePath, &dream.State{Offsets: offsets})
			originalRun, originalContext := dreamRun, dreamBatchContext
			defer func() { dreamRun = originalRun; dreamBatchContext = originalContext }()
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			dreamBatchContext = func(time.Duration) (context.Context, context.CancelFunc) { return ctx, func() {} }
			calls := 0
			dreamRun = func(context.Context, string, string) (string, float64, int, error) {
				calls++
				if calls == 2 {
					if budget {
						cancel(errDreamRuntime)
					}
					return "", .02, 20, fmt.Errorf("fixture interrupted: %w", context.DeadlineExceeded)
				}
				return `[{"description":"The fixture driver requires asynchronous reads.","type":"machine_fact","kind":"observed","applies_to":{}}]`, .01, 10, nil
			}
			want := 1
			if budget {
				want = 0
			}
			if code := runDream([]string{"--max-calls", "2"}); code != want {
				t.Fatalf("exit %d want %d", code, want)
			}
			state, _ := dream.LoadState(statePath)
			if state.Offsets[files[0]] == 0 || state.Offsets[files[1]] != 0 {
				t.Fatal("runtime lost completed work or skipped interrupted window")
			}
			data, err := os.ReadFile(filepath.Join(home, ".local/state/tacit/dreamer.log"))
			if err != nil {
				t.Fatal(err)
			}
			var meter dream.Meter
			if err = json.Unmarshal(data, &meter); err != nil {
				t.Fatal(err)
			}
			if meter.RemainingWindows != 1 || meter.CostUSD != .03 || meter.InTokens != 30 {
				t.Fatalf("bad accounting: %+v", meter)
			}
			if budget && (meter.PauseReason != "runtime" || meter.Errors != 0) {
				t.Fatalf("budget not a clean pause: %+v", meter)
			}
			if !budget && (meter.PauseReason != "" || meter.Errors != 1) {
				t.Fatalf("attempt timeout masked: %+v", meter)
			}
			// Resume through a fresh context and prove the completed transcript stays skipped.
			dreamBatchContext = originalContext
			dreamRun = func(context.Context, string, string) (string, float64, int, error) { return "[]", 0, 0, nil }
			if code := runDream([]string{"--max-calls", "1"}); code != 0 {
				t.Fatal(code)
			}
			state, _ = dream.LoadState(statePath)
			if state.Offsets[files[1]] == 0 {
				t.Fatal("interrupted transcript did not resume")
			}
		})
	}
}

func TestDreamBatchRuntimeDuringLockedPersistence(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	dir := filepath.Join(root, "store")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("TACIT_STORE", dir)
	t.Setenv("TACIT_INDEX", filepath.Join(root, "index"))
	t.Setenv("TACIT_LOCAL", filepath.Join(root, "overlay"))
	t.Setenv("TACIT_DREAM_MAX_RUNTIME", "")
	stubScrump(t)
	if code := runInit(nil); code != 0 {
		t.Fatal(code)
	}
	file := filepath.Join(home, ".claude/projects/demo/locked.jsonl")
	os.MkdirAll(filepath.Dir(file), 0700)
	os.WriteFile(file, []byte(`{"type":"user","message":{"content":"Check the fixture read mode and record its working alternative."}}`+"\n"+`{"type":"assistant","message":{"content":[{"type":"text","text":"The fixture driver requires asynchronous reads."}]}}`+"\n"+strings.Repeat(" ", 5000)+"\n"), 0600)
	statePath := filepath.Join(home, ".local/state/tacit/dreamer.json")
	dream.SaveState(statePath, &dream.State{Offsets: map[string]int64{file: 0}})
	originalRun, originalContext := dreamRun, dreamBatchContext
	defer func() { dreamRun = originalRun; dreamBatchContext = originalContext }()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	dreamBatchContext = func(time.Duration) (context.Context, context.CancelFunc) { return ctx, func() {} }
	release, held := make(chan struct{}), make(chan struct{})
	defer close(release)
	dreamRun = func(context.Context, string, string) (string, float64, int, error) {
		holder := &store.Store{Dir: dir}
		go holder.WithLock(context.Background(), func() error { close(held); <-release; return nil })
		<-held
		cancel(errDreamRuntime)
		return `[{"description":"The fixture driver requires asynchronous reads.","type":"machine_fact","kind":"observed","applies_to":{}}]`, 0, 0, nil
	}
	if code := runDream([]string{"--max-calls", "1"}); code != 0 {
		t.Fatal("deadline while store locked was not a clean pause", code)
	}
	state, _ := dream.LoadState(statePath)
	if state.Offsets[file] != 0 {
		t.Fatal("locked persistence skipped pending transcript")
	}
}
