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

// activityRepo is the repo a session works in when its cwd is in none: an
// agent started in the home directory or a folder of checkouts reaches its
// repo by path. "" when nothing points at one repo, or the time ran out.
func activityRepo(ctx context.Context, in hookInput, home string) string {
	ctx, cancel := context.WithTimeout(ctx, activityBudget)
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
func readTail(path, home string) string {
	if path == "" || home == "" {
		return ""
	}
	realHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		return ""
	}
	p, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil || !strings.HasPrefix(p, realHome+string(filepath.Separator)) {
		return ""
	}
	if fi, err := os.Lstat(p); err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	fh, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return ""
	}
	defer fh.Close()
	fi, err := fh.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return ""
	}
	if fi.Size() > transcriptTail {
		if _, err := fh.Seek(fi.Size()-transcriptTail, io.SeekStart); err != nil {
			return ""
		}
	}
	b, _ := io.ReadAll(io.LimitReader(fh, transcriptTail))
	return string(b)
}
