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
