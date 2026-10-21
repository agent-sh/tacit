package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/agent-sh/tacit/client/internal/dream"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/store"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestDreamCommitsWindowAndCursorBeforeNextCall(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	dir := filepath.Join(root, "store")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("TACIT_STORE", dir)
	t.Setenv("TACIT_INDEX", filepath.Join(root, "index"))
	t.Setenv("TACIT_LOCAL", filepath.Join(root, "overlay"))
	t.Setenv("TACIT_PROJECTS", filepath.Join(root, "projects"))
	stubScrump(t)
	if code := runInit(nil); code != 0 {
		t.Fatal(code)
	}
	files := []string{filepath.Join(home, ".claude/projects/demo/a.jsonl"), filepath.Join(home, ".claude/projects/demo/b.jsonl")}
	offsets := map[string]int64{}
	for _, p := range files {
		os.MkdirAll(filepath.Dir(p), 0700)
		os.WriteFile(p, []byte(`{"type":"user","message":{"content":"Check the fixture controller read mode and record what fixes its error."}}`+"\n"+`{"type":"assistant","cwd":"/home/u/projects/demo","message":{"content":[{"type":"text","text":"The fixture driver requires asynchronous reads."}]}}`+"\n"+strings.Repeat(" ", 5000)+"\n"), 0600)
		offsets[p] = 0
	}
	statePath := filepath.Join(home, ".local/state/tacit/dreamer.json")
	dream.SaveState(statePath, &dream.State{Offsets: offsets})
	old := dreamRun
	defer func() { dreamRun = old }()
	calls := 0
	dreamRun = func(context.Context, string, string) (string, float64, int, error) {
		calls++
		if calls == 1 {
			return `[{"description":"The fixture driver requires asynchronous reads.","type":"machine_fact","kind":"observed","applies_to":{}}]`, .01, 10, nil
		}
		fs, _, err := fact.Walk(dir)
		if err != nil || len(fs) != 1 {
			t.Fatalf("first window missing: %v %d", err, len(fs))
		}
		cmd := exec.Command("git", "ls-tree", "--name-only", "HEAD", "--", fs[0].Path)
		cmd.Dir = dir
		out, err := cmd.Output()
		if err != nil || strings.TrimSpace(string(out)) != fs[0].Path {
			t.Fatal("first window not committed before second model call")
		}
		state, _ := dream.LoadState(statePath)
		if state.Offsets[files[0]] == 0 {
			t.Fatal("successful cursor not saved before second call")
		}
		return "", 0, 0, errors.New("second model call failed")
	}
	if code := runDream([]string{"--max-calls", "2"}); code != 1 {
		t.Fatal(code)
	}
	state, _ := dream.LoadState(statePath)
	if state.Offsets[files[0]] == 0 || state.Offsets[files[1]] != 0 {
		t.Fatal("failed window cursor advanced", state.Offsets)
	}
}

func TestRecoverDreamNotesPreservesBytesAndPushesOnlyOwnInbox(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "store")
	t.Setenv("TACIT_STORE", dir)
	t.Setenv("TACIT_LOCAL", filepath.Join(root, "overlay"))
	if code := runInit(nil); code != 0 {
		t.Fatal(code)
	}
	s := &store.Store{Dir: dir}
	host := store.ShortHost()
	f := dream.ToFact(dream.Extracted{Description: "A pending fixture note requires asynchronous reads", Type: "machine_fact", Kind: "observed"}, "demo", host, "fixture-session")
	f.Path = filepath.ToSlash(filepath.Join("inbox", "dreamer@"+host, f.Meta.Name+".md"))
	b, err := f.Encode()
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(dir, filepath.Dir(f.Path)), 0755)
	os.WriteFile(filepath.Join(dir, f.Path), b, 0644)
	unrelated := filepath.Join(dir, "unrelated.txt")
	os.WriteFile(unrelated, []byte("private unrelated content"), 0644)
	cmd := exec.Command("git", "add", "unrelated.txt")
	cmd.Dir = dir
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	n, err := recoverDreamNotes(context.Background(), s)
	if err != nil || n != 1 {
		t.Fatal(n, err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, f.Path))
	if string(after) != string(b) {
		t.Fatal("recovery rewrote note")
	}
	cmd = exec.Command("git", "ls-tree", "--name-only", "HEAD", "--", "unrelated.txt")
	cmd.Dir = dir
	out, _ := cmd.Output()
	if len(out) != 0 {
		t.Fatal("unrelated staged file committed")
	}
	n, err = recoverDreamNotes(context.Background(), s)
	if err != nil || n != 0 {
		t.Fatal(n, err)
	}
}

func TestDreamFailedCommitDoesNotAdvanceCursor(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell pre-commit fixture")
	}
	root := t.TempDir()
	home := filepath.Join(root, "home")
	dir := filepath.Join(root, "store")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("TACIT_STORE", dir)
	t.Setenv("TACIT_INDEX", filepath.Join(root, "index"))
	t.Setenv("TACIT_LOCAL", filepath.Join(root, "overlay"))
	t.Setenv("TACIT_PROJECTS", filepath.Join(root, "projects"))
	stubScrump(t)
	if code := runInit(nil); code != 0 {
		t.Fatal(code)
	}
	file := filepath.Join(home, ".claude/projects/demo/failed.jsonl")
	os.MkdirAll(filepath.Dir(file), 0700)
	os.WriteFile(file, []byte(`{"type":"user","message":{"content":"Check the fixture read mode and record its working alternative."}}`+"\n"+`{"type":"assistant","message":{"content":[{"type":"text","text":"The fixture driver requires asynchronous reads."}]}}`+"\n"+strings.Repeat(" ", 5000)+"\n"), 0600)
	statePath := filepath.Join(home, ".local/state/tacit/dreamer.json")
	dream.SaveState(statePath, &dream.State{Offsets: map[string]int64{file: 0}})
	os.WriteFile(filepath.Join(dir, ".git/hooks/pre-commit"), []byte("#!/bin/sh\nexit 1\n"), 0700)
	old := dreamRun
	defer func() { dreamRun = old }()
	dreamRun = func(context.Context, string, string) (string, float64, int, error) {
		return `[{"description":"The fixture driver requires asynchronous reads.","type":"machine_fact","kind":"observed","applies_to":{}}]`, .01, 10, nil
	}
	if code := runDream([]string{"--max-calls", "1"}); code != 1 {
		t.Fatal(code)
	}
	state, _ := dream.LoadState(statePath)
	if state.Offsets[file] != 0 {
		t.Fatal("failed commit advanced cursor")
	}
}

func TestDreamRecoveryFlushesRetainedRemoteCommit(t *testing.T) {
	root := t.TempDir()
	dir, _ := newPair(t, root)
	s := &store.Store{Dir: dir}
	host := store.ShortHost()
	f := dream.ToFact(dream.Extracted{Description: "Pending remote fixture note", Type: "machine_fact", Kind: "observed"}, "demo", host, "fixture")
	f.Path = filepath.ToSlash(filepath.Join("inbox", "dreamer@"+host, f.Meta.Name+".md"))
	b, _ := f.Encode()
	os.MkdirAll(filepath.Join(dir, filepath.Dir(f.Path)), 0755)
	os.WriteFile(filepath.Join(dir, f.Path), b, 0644)
	gitIn(t, dir, "add", f.Path)
	gitIn(t, dir, "-c", "user.name=dreamer", "commit", "-q", "-m", "retained pending dreamer commit")
	local := gitIn(t, dir, "rev-parse", "HEAD")
	bare := filepath.Join(root, "store.git")
	if gitIn(t, bare, "rev-parse", "main") == local {
		t.Fatal("fixture was already pushed")
	}
	original := gitIn(t, dir, "remote", "get-url", "origin")
	gitIn(t, dir, "remote", "set-url", "origin", filepath.Join(root, "missing.git"))
	if _, err := recoverDreamNotes(context.Background(), s); err == nil {
		t.Fatal("unavailable remote reported success")
	}
	gitIn(t, dir, "remote", "set-url", "origin", strings.TrimSpace(original))
	if n, err := recoverDreamNotes(context.Background(), s); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	if gitIn(t, bare, "rev-parse", "main") != local {
		t.Fatal("retained commit never reached remote")
	}
}

func TestDreamRecoverySyncsRemoteAdvanceAndIgnoresQuarantine(t *testing.T) {
	root := t.TempDir()
	dir, push := newPair(t, root)
	s := &store.Store{Dir: dir}
	host := store.ShortHost()
	f := dream.ToFact(dream.Extracted{Description: "Pending race fixture note", Type: "machine_fact", Kind: "observed"}, "demo", host, "fixture")
	f.Path = filepath.ToSlash(filepath.Join("inbox", "dreamer@"+host, f.Meta.Name+".md"))
	b, _ := f.Encode()
	os.MkdirAll(filepath.Join(dir, filepath.Dir(f.Path)), 0755)
	os.WriteFile(filepath.Join(dir, f.Path), b, 0644)
	os.WriteFile(filepath.Join(dir, filepath.Dir(f.Path), "bad.md.rejected"), []byte("quarantined"), 0600)
	push("remote-advance.txt")
	if n, err := recoverDreamNotes(context.Background(), s); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	remote := filepath.Join(root, "store.git")
	if gitIn(t, dir, "rev-parse", "HEAD") != gitIn(t, remote, "rev-parse", "main") {
		t.Fatal("recovery was not pushed")
	}
	if n, err := recoverDreamNotes(context.Background(), s); err != nil || n != 0 {
		t.Fatal(n, err)
	}
}

func TestConcurrentDreamWindowsKeepCommittedFactUntouched(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "store")
	t.Setenv("TACIT_STORE", dir)
	if code := runInit(nil); code != 0 {
		t.Fatal(code)
	}
	s := &store.Store{Dir: dir}
	host := store.ShortHost()
	var wg sync.WaitGroup
	failures := make(chan error, 2)
	for _, description := range []string{"First fixture wording", "Second fixture wording"} {
		f := dream.ToFact(dream.Extracted{Description: description, Type: "machine_fact", Kind: "observed"}, "demo", host, "fixture")
		f.Meta.Name = "shared-slug"
		wg.Add(1)
		go func(f *fact.Fact) {
			defer wg.Done()
			_, err := persistDreamWindow(context.Background(), s, []*fact.Fact{f}, nil)
			failures <- err
		}(f)
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("git", "diff", "--exit-code", "HEAD", "--", "inbox/dreamer@"+host+"/shared-slug.md")
	cmd.Dir = dir
	if err := cmd.Run(); err != nil {
		t.Fatal("committed fact was overwritten", err)
	}
}

// A source interrupted after valid metadata must never publish a partial fact.
type interruptedDreamNote struct{ sent bool }

func (r *interruptedDreamNote) Read(p []byte) (int, error) {
	if r.sent {
		return 0, errors.New("fixture disk write interrupted")
	}
	r.sent = true
	return copy(p, "---\nname: partial\n---\ntruncated body"), nil
}
func TestDreamInterruptedWriteNeverPublishesNote(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "partial.md")
	if err := writeDreamNote(path, &interruptedDreamNote{}); err == nil {
		t.Fatal("interrupted write succeeded")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("partial fact published: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed write left files: %v %v", entries, err)
	}
	body := "complete retry body"
	if err := writeDreamNote(path, strings.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != body {
		t.Fatalf("retry content: %q %v", got, err)
	}
}

func TestDreamCappedTranscriptResumesNextWindow(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	dir := filepath.Join(root, "store")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("TACIT_STORE", dir)
	t.Setenv("TACIT_INDEX", filepath.Join(root, "index"))
	t.Setenv("TACIT_LOCAL", filepath.Join(root, "overlay"))
	t.Setenv("TACIT_PROJECTS", filepath.Join(root, "projects"))
	stubScrump(t)
	if code := runInit(nil); code != 0 {
		t.Fatal(code)
	}
	file := filepath.Join(home, ".claude/projects/demo/capped.jsonl")
	os.MkdirAll(filepath.Dir(file), 0700)
	var transcript strings.Builder
	for i := 0; i < 100; i++ {
		row, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": fmt.Sprintf("Fixture turn %03d requires asynchronous reads. %s", i, strings.Repeat("fixture ", 80))}})
		transcript.Write(row)
		transcript.WriteByte('\n')
	}
	transcript.WriteString(`{"type":"assistant","message":{"content":[{"type":"text","text":"The fixture requires asynchronous reads."}]}}` + "\n")
	os.WriteFile(file, []byte(transcript.String()), 0600)
	statePath := filepath.Join(home, ".local/state/tacit/dreamer.json")
	dream.SaveState(statePath, &dream.State{Offsets: map[string]int64{file: 0}})
	initial, _ := dream.LoadState(statePath)
	windows, err := dream.Windows(dream.DefaultSources(home), initial, false)
	if err != nil || len(windows) != 2 {
		t.Fatalf("fixture windows: %d %v", len(windows), err)
	}
	old := dreamRun
	defer func() { dreamRun = old }()
	var prompts []string
	dreamRun = func(_ context.Context, _, prompt string) (string, float64, int, error) {
		prompts = append(prompts, prompt)
		return `[{"description":"The fixture driver requires asynchronous reads.","type":"machine_fact","kind":"observed","applies_to":{}}]`, .01, 10, nil
	}
	if code := runDream([]string{"--max-calls", "1"}); code != 0 {
		t.Fatal(code)
	}
	state, _ := dream.LoadState(statePath)
	if state.Offsets[file] != 0 {
		t.Fatal("capped prefix advanced full transcript cursor")
	}
	if code := runDream([]string{"--max-calls", "1"}); code != 0 {
		t.Fatal(code)
	}
	state, _ = dream.LoadState(statePath)
	if len(prompts) != 2 || prompts[0] == prompts[1] {
		t.Fatal("capped run repeated first chunk")
	}
	if state.Offsets[file] != int64(transcript.Len()) {
		t.Fatal("completed transcript cursor not advanced", state.Offsets[file])
	}
	if code := runDream([]string{"--max-calls", "1"}); code != 0 {
		t.Fatal(code)
	}
	if len(prompts) != 2 {
		t.Fatal("completed windows replayed")
	}
}
