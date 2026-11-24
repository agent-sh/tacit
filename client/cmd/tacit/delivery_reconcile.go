package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/agent-sh/tacit/client/internal/platform"
)

// Reconciling marks (SPEC.md, "Learning from outcomes"): a mark is a log
// entry, and its verdicts reach the outcomes only when the process that
// took it records them. An MCP server started on a binary that did not
// left marks in the log with no verdict. Before the outcomes are read,
// the log's mark entries past a cursor are matched against the verdicts
// (by block and fact, src mark) and the missing ones appended, so every
// logged mark counts whichever binary took it.

const (
	// markGrace: a mark this young may still be having its verdicts
	// recorded by the process that took it; the cursor waits for it.
	markGrace = time.Minute
	// markReadBudget bounds the log bytes one reconcile reads, so the
	// first one, with no cursor, does not hold a hook up; the next goes on.
	markReadBudget = 32 << 20
)

// markCursor is where the last reconcile stopped: a day file of the log
// and the offset of the first line it did not read.
type markCursor struct {
	File   string `json:"file"`
	Offset int64  `json:"offset"`
}

var markEntryNeedle = []byte(`"hook":"mark"`)

// reconcileMarks appends the verdicts of logged marks the outcomes do not
// hold. One process at a time (a lock it does not wait for); any failure
// leaves the outcomes as they are.
func reconcileMarks(now time.Time) {
	if !deliveryLogOn() {
		return
	}
	dir := deliveryLogDir()
	if os.MkdirAll(outcomesDir(), 0o700) != nil {
		return
	}
	lf, err := os.OpenFile(filepath.Join(outcomesDir(), "reconcile.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return
	}
	defer lf.Close()
	if platform.TryLock(lf) != nil {
		return // another process reconciles
	}
	defer platform.Unlock(lf)

	curPath := filepath.Join(outcomesDir(), "reconcile.json")
	var cur markCursor
	if b, err := os.ReadFile(curPath); err == nil {
		json.Unmarshal(b, &cur)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var days []string
	for _, x := range entries {
		if logDay.MatchString(x.Name()) && x.Name() >= cur.File {
			days = append(days, x.Name())
		}
	}
	sort.Strings(days)

	next := cur
	var marks []*logEntry
	budget := int64(markReadBudget)
	for _, name := range days {
		off := int64(0)
		if name == cur.File {
			off = cur.Offset
		}
		got, end, stop := readMarks(filepath.Join(dir, name), off, &budget, now)
		marks = append(marks, got...)
		next = markCursor{File: name, Offset: end}
		if stop {
			break
		}
	}
	if len(marks) > 0 && !appendMissingMarks(marks) {
		return // the cursor stays: the next reconcile tries again
	}
	if next != cur {
		if b, err := json.Marshal(next); err == nil {
			tmp := curPath + ".tmp" + shortHash(now.String())
			if os.WriteFile(tmp, b, 0o600) == nil {
				os.Rename(tmp, curPath)
			}
		}
	}
}

// readMarks reads a day file's whole lines from off: its mark entries, the
// offset after the last line it read, and whether to stop there (the
// budget is spent, or a mark is younger than markGrace).
func readMarks(path string, off int64, budget *int64, now time.Time) (marks []*logEntry, end int64, stop bool) {
	end = off
	if fi, err := os.Lstat(path); err != nil || !fi.Mode().IsRegular() {
		return nil, end, false // only this log's own regular files
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, end, true
	}
	defer f.Close()
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, end, true
	}
	r := bufio.NewReaderSize(f, 1<<16)
	for {
		if *budget <= 0 {
			return marks, end, true
		}
		line, err := r.ReadBytes('\n')
		if err != nil {
			// The end, or a line still being written: what is read so far
			// stands, the rest is the next reconcile's.
			return marks, end, err != io.EOF || len(line) > 0
		}
		if bytes.Contains(line, markEntryNeedle) {
			var e logEntry
			if json.Unmarshal(line, &e) == nil && e.Hook == "mark" && e.Mark != nil && e.Mark.Block != "" {
				t, err := time.Parse(time.RFC3339Nano, e.TS)
				if err == nil && now.Sub(t) < markGrace {
					return marks, end, true
				}
				if err == nil {
					marks = append(marks, &e)
				}
			}
		}
		end += int64(len(line))
		*budget -= int64(len(line))
	}
}

// appendMissingMarks appends the verdicts of these marks the outcomes do
// not hold, each kept under the asked block's scope and hook as logMark
// keeps it; a mark whose block the log no longer holds is passed over.
// False when the outcomes could not be read.
func appendMissingMarks(marks []*logEntry) bool {
	have := map[string]bool{}
	if f, err := os.Open(outcomesPath()); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<16), 1<<20)
		for sc.Scan() {
			var l outcomeLine
			if json.Unmarshal(sc.Bytes(), &l) == nil && l.Src == "mark" {
				have[l.Block+"\x00"+l.Fact] = true
			}
		}
		err := sc.Err()
		f.Close()
		if err != nil {
			return false
		}
	} else if !os.IsNotExist(err) {
		return false
	}
	type graded struct{ name, verdict string }
	gradesOf := func(m *logMarkStep) []graded {
		var g []graded
		for _, n := range m.Helped {
			g = append(g, graded{n, verdictUsed})
		}
		for _, n := range m.NotHelped {
			g = append(g, graded{n, verdictNotRelevant})
		}
		for _, n := range m.Wrong {
			g = append(g, graded{n, verdictWrong})
		}
		return g
	}
	// The blocks whose asks are needed, by the days a mark can follow
	// its block in (markedBlock's window).
	need := map[string]bool{}
	days := map[string]bool{}
	for _, e := range marks {
		for _, g := range gradesOf(e.Mark) {
			if !have[e.Mark.Block+"\x00"+g.name] {
				need[e.Mark.Block] = true
				t, _ := time.Parse(time.RFC3339Nano, e.TS)
				days[t.UTC().Format("2006-01-02")] = true
				days[t.UTC().AddDate(0, 0, -1).Format("2006-01-02")] = true
			}
		}
	}
	if len(need) == 0 {
		return true
	}
	asks := askedBlocks(need, days)
	var outs []outcomeLine
	for _, e := range marks {
		asked := asks[e.Mark.Block]
		if asked == nil {
			continue
		}
		lines := map[string]*logCand{}
		for i, c := range asked.Cands {
			if lines[c.Name] == nil || c.Delivered {
				lines[c.Name] = &asked.Cands[i]
			}
		}
		for _, g := range gradesOf(e.Mark) {
			key := e.Mark.Block + "\x00" + g.name
			if have[key] || lines[g.name] == nil {
				continue
			}
			have[key] = true
			in, hook := markScope(asked, lines[g.name])
			outs = append(outs, outcomeLine{TS: e.TS, In: in, Fact: g.name, Verdict: g.verdict, Src: "mark", Block: e.Mark.Block, Hook: hook})
		}
	}
	recordOutcomes(outs)
	return true
}

// askedBlocks finds the entries of these ids in these days' files: the
// blocks the marks graded (a mark entry is never one).
func askedBlocks(ids map[string]bool, days map[string]bool) map[string]*logEntry {
	out := map[string]*logEntry{}
	var names []string
	for d := range days {
		names = append(names, d)
	}
	sort.Strings(names)
	for _, d := range names {
		path := filepath.Join(deliveryLogDir(), d+".jsonl")
		if fi, err := os.Lstat(path); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<26)
		for sc.Scan() {
			b := sc.Bytes()
			i := bytes.Index(b, []byte(`"id":"`))
			if i < 0 || i+6+16 > len(b) || !ids[string(b[i+6:i+6+16])] {
				continue // most lines are other blocks
			}
			var e logEntry
			if json.Unmarshal(b, &e) != nil || e.Hook == "mark" || !ids[e.ID] {
				continue
			}
			if prev := out[e.ID]; prev == nil || (e.MarkAsked && !prev.MarkAsked) {
				ee := e
				out[e.ID] = &ee
			}
		}
		f.Close()
	}
	return out
}
