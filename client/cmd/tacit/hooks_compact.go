package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/agent-sh/tacit/client/internal/platform"
	"github.com/agent-sh/tacit/client/internal/store"
)

// hookExe is the binary the PreCompact hook starts; tests substitute it.
var hookExe = os.Executable

// dreamSessionLog caps the log of the dreams the PreCompact hook starts: past
// it the file moves to .1 (replacing the older one) before the next run.
const dreamSessionLog = 1 << 20

// preCompact is `tacit hook pre-compact` (Claude Code's and Codex's
// PreCompact, the Hermes provider's on_pre_compress): it starts `tacit dream
// --session <transcript>` for the session about to compact and returns. The
// harness waits for the hook before it compacts, and a dream takes minutes,
// so the dream runs detached (platform.Detach), with no stdin and its output
// in dream-session.log in the state dir: nothing holds a pipe the harness
// reads. The provider variables come from the env file the dream job uses,
// when it exists. Every failure is silent (fail open); the idle dream finds
// the compaction marker later anyway (dream.Select). On a read-only clone, or
// a machine without the dreamer's key (a Mac that only reads), it starts
// nothing: the dream could never push what it found.
func preCompact(s *store.Store, in hookInput) {
	target := in.TranscriptPath
	if target == "" {
		target = in.HermesSession
	}
	if target == "" || strings.ContainsAny(target, "\n\r") {
		return
	}
	if writerCheck(context.Background(), s, "dreamer") != nil {
		return
	}
	exe, err := hookExe()
	if err != nil {
		return
	}
	home, _ := os.UserHomeDir()
	args := []string{"dream", "--session", target}
	if env := defaultDreamEnv(runtime.GOOS, home); fileExists(env) {
		args = append(args, "--env-file", env)
	}
	dir := platform.StateDir(home)
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	path := filepath.Join(dir, "dream-session.log")
	if fi, err := os.Stat(path); err == nil && fi.Size() > dreamSessionLog {
		os.Rename(path, path+".1")
	}
	log, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer log.Close()
	cmd := exec.Command(exe, args...)
	cmd.Stdout, cmd.Stderr = log, log
	platform.Detach(cmd)
	if cmd.Start() == nil {
		cmd.Process.Release()
	}
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}
