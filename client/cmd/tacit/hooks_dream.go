package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/dream"
	"github.com/agent-sh/tacit/client/internal/hermes"
	"github.com/agent-sh/tacit/client/internal/platform"
	"github.com/agent-sh/tacit/client/internal/store"
)

// The hooks start the dreamer at three moments of a session, each the same
// detached `tacit dream --session <transcript>` (startSessionDream):
//
//   - a full window (windowDream, from the prompt hook): the session's
//     undreamed part has grown to about one dream window, so dreaming does
//     not wait for a compaction, a quiet spell or the 4-hour run;
//   - before a compaction (preCompact, the PreCompact hook), so what the
//     compaction is about to drop reaches the store for other and later
//     sessions. Nothing is given back to the same session: the compaction's
//     summary carries its content.
//
// The idle job (a session quiet for 20 minutes, which catches Ctrl-C and
// crashes) and the 4-hour run are the other triggers. Each dream reads from
// the transcript's saved cursor, never from its start.

// hookExe is the binary the hooks start; tests substitute it.
var hookExe = os.Executable

// dreamSessionLog caps the log of the dreams the hooks start: past it the
// file moves to .1 (replacing the older one) before the next run.
const dreamSessionLog = 1 << 20

// windowBytes is the undreamed part of a transcript (its size less its
// dream cursor) at which the prompt hook starts a dream, by the transcript's
// format. A transcript holds far more bytes than its digest (tool output,
// metadata, images), and how many depends on the harness. Measured over
// consecutive deltas of real transcripts (eval/VALUE.md, "Dreaming at a full
// window"): a 4 MiB Claude Code delta typically digests to less than one
// window of 60,000 characters, and 4 MiB is also what a dream reads of a file
// per run (maxReadBytes in internal/dream), so a larger threshold would only
// read a part of it; a 1 MiB Codex delta typically digests to more than one
// window (sessions of mostly tool output to far less).
var windowBytes = map[digest.Format]int64{digest.Claude: 4 << 20, digest.Codex: 1 << 20}

// windowHermesBytes is the same for a Hermes session: message content bytes
// after its cursor (hermes.NewBytes), which are much closer to the digest's
// characters (no metadata; long tool output is clipped in the digest). Not
// measured: no Hermes store on the machine the others were measured on.
var windowHermesBytes int64 = 120_000

// windowDream is the prompt hook's dream trigger: when the transcript's
// undreamed part has reached a full window, it starts a session dream and
// returns. The check is a stat of the transcript (a Hermes session: one
// query) and a read of dreamer.json, no digest and no model; the writer
// check runs only once the threshold is met. It starts nothing while a
// dream holds dreamer.lock, for a transcript that failed within
// dream.FailureBackoff (the full run retries it), for a cursor it already
// started a dream at (dream-trigger.json), or on a machine that cannot write
// as the dreamer and has no local layer (canDream). Every failure is silent.
func windowDream(s *store.Store, in hookInput) {
	home, _ := os.UserHomeDir()
	dir := platform.StateDir(home)
	st, err := dream.LoadState(filepath.Join(dir, "dreamer.json"))
	if err != nil || len(st.Offsets) == 0 {
		return // no cursors yet: the first full run records them
	}
	key, target, undreamed, threshold := "", "", int64(0), int64(0)
	switch {
	case in.TranscriptPath != "":
		key, target = filepath.Clean(in.TranscriptPath), in.TranscriptPath
		// The dreamer's own sources say the format; a transcript none of them
		// holds is one no dream reads.
		for _, src := range dream.DefaultSources(home) {
			if ok, _ := filepath.Match(src.Glob, key); ok && src.Glob != "" {
				threshold = windowBytes[src.Format]
				break
			}
		}
		if threshold == 0 {
			return
		}
		fi, err := os.Stat(key)
		if err != nil || !fi.Mode().IsRegular() {
			return
		}
		// A transcript with no cursor is a session begun since the last run:
		// the dream reads it from its start.
		undreamed = fi.Size() - st.Offsets[key]
	case in.HermesSession != "":
		var db string
		for _, src := range dream.DefaultSources(home) {
			if src.Format == digest.Hermes {
				db = src.DB
			}
		}
		if db == "" {
			return
		}
		key, target, threshold = hermes.Key(db, in.HermesSession), in.HermesSession, windowHermesBytes
		h, err := hermes.Open(db)
		if err != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		undreamed, err = h.NewBytes(ctx, in.HermesSession, st.Offsets[key])
		cancel()
		h.Close()
		if err != nil {
			return
		}
	default:
		return
	}
	if undreamed < threshold || st.RecentlyFailed(key, time.Now()) {
		return
	}
	cursor := st.Offsets[key]
	triggers := filepath.Join(dir, "dream-trigger.json")
	started := loadTriggers(triggers)
	if at, ok := started[key]; ok && at == cursor {
		return // a dream was started at this cursor; it ran or is running
	}
	if jobLockHeld(dir, "dreamer") || !canDream(context.Background(), s) {
		return
	}
	if !startSessionDream(home, target) {
		return
	}
	// Only the entries whose cursor has not moved say anything.
	for k, at := range started {
		if st.Offsets[k] != at {
			delete(started, k)
		}
	}
	started[key] = cursor
	saveTriggers(triggers, started)
}

func loadTriggers(path string) map[string]int64 {
	m := map[string]int64{}
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, &m)
	}
	return m
}

func saveTriggers(path string, m map[string]int64) {
	b, _ := json.Marshal(m)
	tmp := path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, path)
	}
}

// jobLockHeld reports whether another process holds <stateDir>/<name>.lock,
// without taking it for longer than the check (and without writing the
// holder's pid, as tryJobLock does).
func jobLockHeld(stateDir, name string) bool {
	f, err := os.OpenFile(filepath.Join(stateDir, name+".lock"), os.O_RDWR, 0)
	if err != nil {
		return false // no lock file: no dream has run yet
	}
	defer f.Close()
	if platform.TryLock(f) != nil {
		return true
	}
	platform.Unlock(f)
	return false
}

// preCompact is `tacit hook pre-compact` (Claude Code's and Codex's
// PreCompact, the Hermes provider's on_pre_compress): it starts the session
// dream for the session about to compact and returns. The harness waits for
// the hook before it compacts, and a dream takes minutes. Every failure is
// silent (fail open); the idle dream finds the compaction marker later
// anyway (dream.Select). On a read-only clone, or a machine without the
// dreamer's key (a Mac that only reads), it starts nothing: the dream could
// never push what it found. A read-only clone with the local layer dreams
// into its overlay (canDream).
func preCompact(s *store.Store, in hookInput) {
	target := in.TranscriptPath
	if target == "" {
		target = in.HermesSession
	}
	if target == "" {
		return
	}
	if !canDream(context.Background(), s) {
		return
	}
	home, _ := os.UserHomeDir()
	startSessionDream(home, target)
}

// startSessionDream starts `tacit dream --session <target>` detached
// (platform.StartDetached), with no stdin and its output in
// dream-session.log in the state dir: nothing holds a pipe the harness
// reads. The provider variables come from the env file the dream job uses,
// when it exists. It reports whether the dream started.
func startSessionDream(home, target string) bool {
	if target == "" || strings.ContainsAny(target, "\n\r") {
		return false
	}
	exe, err := hookExe()
	if err != nil {
		return false
	}
	args := []string{"dream", "--session", target}
	if env := defaultDreamEnv(runtime.GOOS, home); fileExists(env) {
		args = append(args, "--env-file", env)
	}
	dir := platform.StateDir(home)
	if os.MkdirAll(dir, 0o700) != nil {
		return false
	}
	path := filepath.Join(dir, "dream-session.log")
	if fi, err := os.Stat(path); err == nil && fi.Size() > dreamSessionLog {
		os.Rename(path, path+".1")
	}
	log, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return false
	}
	defer log.Close()
	cmd := exec.Command(exe, args...)
	cmd.Stdout, cmd.Stderr = log, log
	p, err := platform.StartDetached(cmd)
	if err != nil {
		return false
	}
	p.Release()
	return true
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}
