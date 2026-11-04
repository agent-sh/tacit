package main

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/dream"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/platform"
	"github.com/agent-sh/tacit/client/internal/repoid"
	"github.com/agent-sh/tacit/client/internal/store"
)

// readRanges is dream-ranges.jsonl's lines.
func readRanges(t *testing.T, home string) []dreamRange {
	t.Helper()
	f, err := os.Open(filepath.Join(platform.StateDir(home), dreamRangesFile))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []dreamRange
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r dreamRange
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("a torn line: %q", sc.Text())
		}
		out = append(out, r)
	}
	return out
}

// A dream run records the range of each note its window wrote: the note's
// name, the transcript, the session and the read's byte range. A window
// whose note was already there records nothing: the earlier range stands.
func TestDreamRecordsEachNotesRange(t *testing.T) {
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
	stubScrump(t)
	if code := runInit(nil); code != 0 {
		t.Fatal(code)
	}
	p := filepath.Join(home, ".claude/projects/demo/a.jsonl")
	os.MkdirAll(filepath.Dir(p), 0o700)
	body := `{"type":"user","message":{"content":"Check the fixture controller read mode and record what fixes its error."}}` + "\n" + `{"type":"assistant","cwd":"/home/u/projects/demo","message":{"content":[{"type":"text","text":"The fixture driver requires asynchronous reads."}]}}` + "\n" + strings.Repeat(" ", 5000) + "\n"
	os.WriteFile(p, []byte(body), 0o600)
	statePath := filepath.Join(platform.StateDir(home), "dreamer.json")
	dream.SaveState(statePath, &dream.State{Offsets: map[string]int64{p: 0}})
	old := dreamRun
	t.Cleanup(func() { dreamRun = old })
	dreamRun = func(context.Context, string, string) (string, float64, int, error) {
		return `[{"description":"The fixture driver requires asynchronous reads.","type":"machine_fact","kind":"observed","applies_to":{}}]`, .01, 10, nil
	}
	if code := runDream(nil); code != 0 {
		t.Fatalf("dream exit %d", code)
	}
	fs, _, err := fact.Walk(dir)
	if err != nil || len(fs) != 1 {
		t.Fatalf("notes: %v %d", err, len(fs))
	}
	got := readRanges(t, home)
	if len(got) != 1 || got[0].Note != fs[0].Meta.Name || got[0].File != p || got[0].Session != fs[0].Meta.Source.Session || got[0].Start != 0 || got[0].End <= 0 {
		t.Fatalf("ranges: %+v (note %s, session %s)", got, fs[0].Meta.Name, fs[0].Meta.Source.Session)
	}
	// The same note again from new content: not written, no new range.
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(body)
	f.Close()
	if code := runDream(nil); code != 0 {
		t.Fatalf("second dream exit %d", code)
	}
	if again := readRanges(t, home); len(again) != 1 {
		t.Fatalf("a note already there recorded again: %+v", again)
	}
}

// The local layer's notes (renamed when redaction changes their
// description) are recorded by the name they were written under.
func TestLocalDreamRecordsTheWrittenName(t *testing.T) {
	r := newLocalRig(t)
	previous := dreamRun
	defer func() { dreamRun = previous }()
	dreamRun = func(context.Context, string, string) (string, float64, int, error) {
		return `[{"description":"The walrus fixture driver listens on port 7790, token Zq7Vx9","type":"machine_fact","kind":"observed","applies_to":{}}]`, .01, 10, nil
	}
	if code := runDream(nil); code != 0 {
		t.Fatalf("local dream exit %d", code)
	}
	got := readRanges(t, r.home)
	if len(got) != 1 || strings.Contains(got[0].Note, "zq7vx9") || got[0].File != r.transcript {
		t.Fatalf("ranges: %+v", got)
	}
	matches, _ := filepath.Glob(filepath.Join(r.local, "inbox", "*", got[0].Note+".md"))
	if len(matches) != 1 {
		t.Fatalf("no overlay note named %s", got[0].Note)
	}
}

// A window whose commit failed still records its note's range: recovery
// publishes the note later, and the next window that finds it there records
// nothing.
func TestDreamRecordsTheRangeOfANoteWhoseCommitFailed(t *testing.T) {
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
	stubScrump(t)
	if code := runInit(nil); code != 0 {
		t.Fatal(code)
	}
	hook := filepath.Join(dir, ".git", "hooks", "pre-commit")
	os.MkdirAll(filepath.Dir(hook), 0o755)
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(home, ".claude/projects/demo/a.jsonl")
	os.MkdirAll(filepath.Dir(p), 0o700)
	os.WriteFile(p, []byte(`{"type":"user","message":{"content":"Check the fixture controller read mode and record what fixes its error."}}`+"\n"+`{"type":"assistant","cwd":"/home/u/projects/demo","message":{"content":[{"type":"text","text":"The fixture driver requires asynchronous reads."}]}}`+"\n"+strings.Repeat(" ", 5000)+"\n"), 0o600)
	statePath := filepath.Join(platform.StateDir(home), "dreamer.json")
	dream.SaveState(statePath, &dream.State{Offsets: map[string]int64{p: 0}})
	old := dreamRun
	t.Cleanup(func() { dreamRun = old })
	dreamRun = func(context.Context, string, string) (string, float64, int, error) {
		return `[{"description":"The fixture driver requires asynchronous reads.","type":"machine_fact","kind":"observed","applies_to":{}}]`, .01, 10, nil
	}
	runDream(nil) // the commit fails
	if got := readRanges(t, home); len(got) != 1 || got[0].File != p {
		t.Fatalf("after a failed commit: %+v", got)
	}
	os.Remove(hook)
	if code := runDream(nil); code != 0 {
		t.Fatalf("second dream exit %d", code)
	}
	if got := readRanges(t, home); len(got) != 1 {
		t.Fatalf("after recovery: %+v", got)
	}
}

// The local layer records the notes it wrote though persisting then failed
// (here: the catalog beside them could not be written).
func TestLocalDreamRecordsNotesWrittenBeforeAnError(t *testing.T) {
	r := newLocalRig(t)
	previous := dreamRun
	defer func() { dreamRun = previous }()
	dreamRun = func(context.Context, string, string) (string, float64, int, error) {
		return `[{"description":"The walrus fixture driver listens on port 7790","type":"machine_fact","kind":"observed","applies_to":{}}]`, .01, 10, nil
	}
	// A directory where the catalog goes: its write fails after the note's.
	os.MkdirAll(filepath.Join(r.local, "inbox", "dreamer@"+store.ShortHost(), repoid.CatalogFile), 0o755)
	runDream(nil)
	notes, _ := filepath.Glob(filepath.Join(r.local, "inbox", "*", "*.md"))
	got := readRanges(t, r.home)
	if len(notes) != 1 || len(got) != 1 || got[0].Note+".md" != filepath.Base(notes[0]) {
		t.Fatalf("notes %v, ranges %+v", notes, got)
	}
}

// A line a crash tore takes no record with it: the next is found.
func TestDreamRangesAfterATornLine(t *testing.T) {
	state := t.TempDir()
	if err := os.WriteFile(filepath.Join(state, dreamRangesFile), []byte(`{"note":"torn","file":"/x/a.jsonl","ses`), 0o600); err != nil {
		t.Fatal(err)
	}
	n := &fact.Fact{Meta: fact.Meta{Name: "after-the-tear"}}
	recordDreamRanges(state, dream.Window{File: "/x/b.jsonl", Session: "s1", Start: 0, End: 9}, []*fact.Fact{n}, []string{"after-the-tear.md"})
	if _, ok := dreamRangeOf(state, "after-the-tear", "s1", "b"); !ok {
		t.Fatal("the record after a torn line is lost")
	}
}
