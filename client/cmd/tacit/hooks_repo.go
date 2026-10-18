package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/agent-sh/tacit/client/internal/repoinfer"
)

// transcriptTail is how much of the session transcript the prompt hook reads
// for the repo the session works in: the recent tool calls, not the history.
const transcriptTail = 512 << 10

// activityRepo is the repo a session works in when its cwd is in none: an
// agent started in the home directory or a folder of checkouts reaches its
// repo by path. Paths in the prompt count three times (the owner typed
// them), paths in the transcript's recent tool calls once; the repo must
// hold most of them. "" when nothing points at one repo.
func activityRepo(in hookInput, home string) string {
	counts := repoinfer.Counts([]string{in.Prompt}, home, 3, nil)
	if tail := readTail(in.TranscriptPath, home); tail != "" {
		repoinfer.Counts([]string{tail}, home, 1, counts)
	}
	return repoinfer.Dominant(counts, 2)
}

// readTail is the last transcriptTail bytes of the transcript, when it is a
// regular file under home (the harness's own transcript, nothing else).
func readTail(path, home string) string {
	if path == "" || home == "" {
		return ""
	}
	p := filepath.Clean(path)
	if !strings.HasPrefix(p, home+string(filepath.Separator)) {
		return ""
	}
	fh, err := os.Open(p)
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
