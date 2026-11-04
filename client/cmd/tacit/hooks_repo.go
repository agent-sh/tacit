package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/agent-sh/tacit/client/internal/repoinfer"
)

const (
	// transcriptTail is how much of the session transcript the prompt hook
	// reads for the repo the session works in: the recent tool calls, not
	// the history.
	transcriptTail = 512 << 10
	// activityBudget bounds the inference inside the prompt hook's budget
	// (queryBudget), and activityDirs the directories it may stat, so a slow
	// or network file system costs a bounded share of a prompt.
	activityBudget = 300 * time.Millisecond
	activityDirs   = 2000
	// A path the owner typed counts twice and one in the transcript once,
	// and the repo needs three: one typed path alone ("compare with
	// ~/w/other/x.go") does not move the session, a typed path with one
	// recent tool call there does.
	promptPathWeight = 2
	activityMin      = 3
)

// activityDeadline is what activityRepo runs under: activityBudget, except in
// tests that check the repo it finds, which widen it so a loaded machine or
// the race detector cannot turn their answer into the empty one.
var activityDeadline = activityBudget

// activityRepo is the repo a session works in when its cwd is in none: an
// agent started in the home directory or a folder of checkouts reaches its
// repo by path. "" when nothing points at one repo, or the time ran out.
func activityRepo(ctx context.Context, in hookInput, home string) string {
	ctx, cancel := context.WithTimeout(ctx, activityDeadline)
	defer cancel()
	r := repoinfer.NewResolver(home)
	r.MaxDirs = activityDirs
	r.Stop = func() bool { return ctx.Err() != nil }
	counts := r.Counts([]string{in.Prompt}, promptPathWeight, nil)
	if tail := readTail(in.TranscriptPath, home); tail != "" {
		r.Counts([]string{tail}, 1, counts)
	}
	if ctx.Err() != nil {
		return ""
	}
	return repoinfer.Dominant(counts, activityMin)
}

// readTail is the last transcriptTail bytes of the transcript, when it is a
// regular file under home with symlinks resolved on both sides. The file is
// checked before it is opened and opened without blocking, so a FIFO or a
// device in its place cannot hang the hook.
func readTail(path, home string) string { return readTailN(path, home, transcriptTail) }

// readTailN is readTail with the size it reads: the start after a
// compaction reads more of the end (compactTail) for the summary.
func readTailN(path, home string, size int64) string {
	b, _ := readRange(path, home, -1, size)
	return b
}

// readRange is at most size bytes of the transcript from offset from, or its
// last size bytes when from is below 0, with readTail's checks, and the
// file's size; "" and -1 when a check fails.
func readRange(path, home string, from, size int64) (string, int64) {
	if path == "" || home == "" {
		return "", -1
	}
	realHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		return "", -1
	}
	p, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil || !strings.HasPrefix(p, realHome+string(filepath.Separator)) {
		return "", -1
	}
	if fi, err := os.Lstat(p); err != nil || !fi.Mode().IsRegular() {
		return "", -1
	}
	fh, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", -1
	}
	defer fh.Close()
	fi, err := fh.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return "", -1
	}
	start := from
	if start < 0 {
		start = max(0, fi.Size()-size)
	}
	if start > fi.Size() {
		return "", fi.Size()
	}
	if _, err := fh.Seek(start, io.SeekStart); err != nil {
		return "", -1
	}
	b, _ := io.ReadAll(io.LimitReader(fh, size))
	return string(b), fi.Size()
}
