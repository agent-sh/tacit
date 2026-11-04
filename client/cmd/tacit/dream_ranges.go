package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/agent-sh/tacit/client/internal/dream"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/transcript"
)

// The transcript range each dreamer note was dreamed from (SPEC.md, "Session
// events", Recall): the dreamer keeps it on this machine, in the state
// directory, one JSON line per note of a window it completed. Nothing in the
// store changes and nothing is synced: the transcript is on this machine too.
// `tacit episode --fact` reads it to take a fact to its source window.

// dreamRangesFile is the file in the state directory.
const dreamRangesFile = "dream-ranges.jsonl"

// dreamRange is one note's window: the transcript's plain path, the session
// id the note names, and the byte range [Start, End) of the read its window
// was cut from (a read takes up to 4 MiB of a transcript, a window at most
// 60,000 characters of its digest, so one read can hold several windows).
type dreamRange struct {
	Note    string `json:"note"`
	File    string `json:"file"`
	Session string `json:"session,omitempty"`
	Start   int64  `json:"start"`
	End     int64  `json:"end"`
}

// recordDreamRanges appends the window's range for each note it wrote
// (written: the paths persisting returned; a note already there was written
// by an earlier window, whose range stands). It fails quietly: the range is
// a convenience for recall, never a reason for a window to fail. A Hermes
// window records none.
func recordDreamRanges(stateDir string, w dream.Window, facts []*fact.Fact, written []string) {
	if len(facts) == 0 || w.File == "" || strings.HasPrefix(w.File, "hermes:") || w.End <= w.Start {
		return
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(stateDir, dreamRangesFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	wrote := map[string]bool{}
	for _, p := range written {
		wrote[strings.TrimSuffix(filepath.Base(filepath.FromSlash(p)), ".md")] = true
	}
	// One write for the window's lines, so a crash tears at most this one.
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	for _, x := range facts {
		if x == nil || x.Meta.Name == "" || !wrote[x.Meta.Name] {
			continue
		}
		_ = enc.Encode(dreamRange{Note: x.Meta.Name, File: transcript.Plain(w.File), Session: w.Session, Start: w.Start, End: w.End})
	}
	if buf.Len() > 0 {
		_, _ = f.WriteString(buf.String())
	}
}

// dreamRangeOf is the first range recorded for a note in this session's
// transcript (named name): the window that wrote it.
func dreamRangeOf(stateDir, note, session, name string) (dreamRange, bool) {
	f, err := os.Open(filepath.Join(stateDir, dreamRangesFile))
	if err != nil {
		return dreamRange{}, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if !strings.Contains(string(line), note) {
			continue
		}
		var r dreamRange
		if json.Unmarshal(line, &r) == nil && r.Note == note && r.End > r.Start && r.Session == session && transcript.Name(r.File) == name {
			return r, true
		}
	}
	return dreamRange{}, false
}
