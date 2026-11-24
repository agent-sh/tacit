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
//
// The hooks that read the outcomes are on the clock (a deny has 400 ms),
// so one reconcile reads at most markReadBudget bytes of the log, the
// forward scan and the search for a mark's block together, and leaves
// where it stopped for the next: the cursor, and a mark whose block is
// still being searched for with how far back the search has come.

// markGrace: a mark this young may still be having its verdicts recorded
// by the process that took it; the cursor waits for it.
const markGrace = time.Minute

var (
	// markReadBudget bounds the log bytes one reconcile reads.
	markReadBudget int64 = 8 << 20
	// markChunk is the step of the backward search for a block.
	markChunk int64 = 1 << 20
)

// markCursor is where the last reconcile stopped: a day file of the log
// and the offset of the first line it did not read, and the mark whose
// block it was searching for, if any.
type markCursor struct {
	File    string       `json:"file"`
	Offset  int64        `json:"offset"`
	Pending *markPending `json:"pending,omitempty"`
}

// markPending is a mark whose verdicts the outcomes lack, and the search
// for its block: backward from the mark through its day's file and the
// day before's (markedBlock's window), SearchFile read down to SearchPos.
type markPending struct {
	Mark       logEntry `json:"mark"`
	SearchFile string   `json:"search_file"`
	SearchPos  int64    `json:"search_pos"`
}

var markEntryNeedle = []byte(`"hook":"mark"`)

// reconciler is one reconcile's state.
type reconciler struct {
	dir    string
	now    time.Time
	budget int64
	have   map[string]bool // block and fact of the mark verdicts held; nil: not read yet
}

// reconcileMarks appends the verdicts of logged marks the outcomes do not
// hold. One process at a time (a lock it does not wait for); a failure
// leaves the cursor where it was, so nothing is skipped.
func reconcileMarks(now time.Time) {
	if !deliveryLogOn() {
		return
	}
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
	was, _ := os.ReadFile(curPath)
	if was != nil {
		json.Unmarshal(was, &cur)
	}
	r := &reconciler{dir: deliveryLogDir(), now: now, budget: markReadBudget}
	next, ok := r.run(cur)
	if !ok {
		return
	}
	if b, err := json.Marshal(next); err == nil && !bytes.Equal(b, was) {
		tmp := curPath + ".tmp" + shortHash(now.String())
		if os.WriteFile(tmp, b, 0o600) == nil {
			os.Rename(tmp, curPath)
		}
	}
}

// run goes on from cur: the pending search first, then the log's lines
// past the cursor. It returns where to go on from, false on a failure.
func (r *reconciler) run(cur markCursor) (markCursor, bool) {
	if cur.Pending != nil {
		done, ok := r.search(cur.Pending)
		if !ok {
			return cur, false
		}
		if !done {
			return cur, true
		}
		cur.Pending = nil
	}
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return cur, false
	}
	var days []string
	for _, x := range entries {
		if logDay.MatchString(x.Name()) && x.Name() >= cur.File {
			days = append(days, x.Name())
		}
	}
	sort.Strings(days)
	for k, name := range days {
		if name != cur.File {
			cur = markCursor{File: name}
		}
		for {
			mark, end, stop, ok := r.nextMark(filepath.Join(r.dir, name), cur.Offset, k == len(days)-1)
			if !ok {
				return cur, false
			}
			cur.Offset = end
			if mark == nil {
				if stop {
					return cur, true
				}
				break // the file's end: the next day's
			}
			missing, ok := r.missing(mark)
			if !ok {
				return cur, false
			}
			if !missing {
				continue
			}
			// end is the line after the mark: its block is before it.
			p := &markPending{Mark: *mark, SearchFile: name, SearchPos: end}
			done, ok := r.search(p)
			if !ok {
				return cur, false
			}
			if !done {
				cur.Pending = p
				return cur, true
			}
		}
	}
	return cur, true
}

// nextMark reads whole lines of a day file from off up to its next mark
// entry: the mark (nil at the file's end, or when it stops), the offset
// after the last line read, and whether to stop there (the budget is
// spent, a mark is younger than markGrace, or the newest file's last line
// is still being written; an older file's, which a crash cut short, is
// passed over).
func (r *reconciler) nextMark(path string, off int64, newest bool) (mark *logEntry, end int64, stop, ok bool) {
	end = off
	if fi, err := os.Lstat(path); err != nil || !fi.Mode().IsRegular() {
		return nil, end, false, true // only this log's own regular files
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, end, false, false
	}
	defer f.Close()
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, end, false, false
	}
	br := bufio.NewReaderSize(f, 1<<16)
	for {
		if r.budget <= 0 {
			return nil, end, true, true
		}
		line, err := br.ReadBytes('\n')
		if err != nil {
			if err != io.EOF {
				return nil, end, false, false
			}
			if len(line) == 0 || !newest {
				return nil, end + int64(len(line)), false, true
			}
			return nil, end, true, true // still being written: the next reconcile's
		}
		r.budget -= int64(len(line))
		if bytes.Contains(line, markEntryNeedle) {
			var e logEntry
			if json.Unmarshal(line, &e) == nil && e.Hook == "mark" && e.Mark != nil && logIDRx.MatchString(e.Mark.Block) {
				if t, err := time.Parse(time.RFC3339Nano, e.TS); err == nil {
					if r.now.Sub(t) < markGrace {
						return nil, end, true, true
					}
					return &e, end + int64(len(line)), false, true
				}
			}
		}
		end += int64(len(line))
	}
}

// grades are a mark's verdicts by name.
func grades(m *logMarkStep) [][2]string {
	var g [][2]string
	for _, n := range m.Helped {
		g = append(g, [2]string{n, verdictUsed})
	}
	for _, n := range m.NotHelped {
		g = append(g, [2]string{n, verdictNotRelevant})
	}
	for _, n := range m.Wrong {
		g = append(g, [2]string{n, verdictWrong})
	}
	return g
}

// missing is whether the outcomes lack one of the mark's verdicts.
func (r *reconciler) missing(mark *logEntry) (bool, bool) {
	if r.have == nil {
		r.have = map[string]bool{}
		f, err := os.Open(outcomesPath())
		if err == nil {
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 1<<16), 1<<20)
			for sc.Scan() {
				var l outcomeLine
				if json.Unmarshal(sc.Bytes(), &l) == nil && l.Src == "mark" {
					r.have[l.Block+"\x00"+l.Fact] = true
				}
			}
			err = sc.Err()
			f.Close()
		} else if os.IsNotExist(err) {
			err = nil
		}
		if err != nil {
			r.have = nil
			return false, false
		}
	}
	for _, g := range grades(mark.Mark) {
		if !r.have[mark.Mark.Block+"\x00"+g[0]] {
			return true, true
		}
	}
	return false, true
}

// search goes on looking for a pending mark's block, backward from where
// it stopped, within the budget. Done: the block was found and the
// missing verdicts appended, or the window holds no such block (the mark
// is passed over). Not ok: a read or the append failed.
func (r *reconciler) search(p *markPending) (done, ok bool) {
	t, err := time.Parse(time.RFC3339Nano, p.Mark.TS)
	if err != nil || p.Mark.Mark == nil {
		return true, true
	}
	last := t.UTC().AddDate(0, 0, -1).Format("2006-01-02") + ".jsonl"
	needle := []byte(`"id":"` + p.Mark.Mark.Block + `"`)
	for {
		asked, pos, more, ok := r.searchFile(filepath.Join(r.dir, p.SearchFile), p.SearchPos, needle, p.Mark.Mark.Block)
		if !ok {
			return false, false
		}
		p.SearchPos = pos
		if asked != nil {
			if _, ok := r.missing(&p.Mark); !ok {
				return false, false
			}
			return true, r.appendMissing(&p.Mark, asked)
		}
		if more {
			return false, true // the budget is spent
		}
		if p.SearchFile <= last {
			return true, true // no such block in the window
		}
		p.SearchFile, p.SearchPos = last, -1 // -1: from the file's end
	}
}

// searchFile reads a day file backward from pos (-1: its end) for the
// entry of this id, a chunk at a time within the budget: the entry, where
// the search stopped, and whether the file has more to read.
func (r *reconciler) searchFile(path string, pos int64, needle []byte, id string) (asked *logEntry, at int64, more, ok bool) {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return nil, 0, false, true
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, pos, true, false
	}
	defer f.Close()
	if pos < 0 || pos > fi.Size() {
		pos = fi.Size()
	}
	chunk := markChunk
	for pos > 0 {
		if r.budget <= 0 {
			return nil, pos, true, true
		}
		start := max(pos-chunk, 0)
		buf := make([]byte, pos-start)
		if _, err := f.ReadAt(buf, start); err != nil {
			return nil, pos, true, false
		}
		r.budget -= int64(len(buf))
		whole, from := buf, start
		if start > 0 {
			i := bytes.IndexByte(buf, '\n')
			if i < 0 {
				chunk *= 2 // one line longer than the chunk
				continue
			}
			whole, from = buf[i+1:], start+int64(i)+1
		}
		for rest := whole; ; {
			i := bytes.LastIndex(rest, needle)
			if i < 0 {
				break
			}
			ls := bytes.LastIndexByte(rest[:i], '\n') + 1
			le := i + bytes.IndexByte(rest[i:], '\n')
			if le < i {
				le = len(rest)
			}
			var e logEntry
			if json.Unmarshal(rest[ls:le], &e) == nil && e.ID == id && e.Hook != "mark" {
				return &e, from, true, true
			}
			rest = rest[:ls]
		}
		pos = from
	}
	return nil, 0, false, true
}

// appendMissing appends the mark's verdicts the outcomes lack, each kept
// under the asked block's scope and hook as logMark keeps it.
func (r *reconciler) appendMissing(mark, asked *logEntry) bool {
	lines := askedLines(asked) // the lines logMark lets a mark grade
	var outs []outcomeLine
	for _, g := range grades(mark.Mark) {
		key := mark.Mark.Block + "\x00" + g[0]
		if r.have[key] || lines[g[0]] == nil {
			continue
		}
		in, hook := markScope(asked, lines[g[0]])
		outs = append(outs, outcomeLine{TS: mark.TS, In: in, Fact: g[0], Verdict: g[1], Src: "mark", Block: mark.Mark.Block, Hook: hook})
	}
	if appendOutcomes(outs) != nil {
		return false
	}
	for _, o := range outs {
		r.have[o.Block+"\x00"+o.Fact] = true
	}
	return true
}
