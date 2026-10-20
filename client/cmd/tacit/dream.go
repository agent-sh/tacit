package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/agent-sh/tacit/client/internal/platform"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/dream"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/modelrun"
	"github.com/agent-sh/tacit/client/internal/overlay"
	"github.com/agent-sh/tacit/client/internal/store"
)

var dreamRun dream.Runner = dream.RunCLI

var errDreamRuntime = errors.New("dreamer runtime limit")
var dreamBatchContext = func(limit time.Duration) (context.Context, context.CancelFunc) {
	if limit == 0 {
		return context.WithCancel(context.Background())
	}
	return context.WithTimeoutCause(context.Background(), limit, errDreamRuntime)
}

// dreamLockWait is how long the full dream waits for an idle or session
// dream that holds dreamer.lock; tests shorten it.
var dreamLockWait = 45 * time.Minute

// defaultIdle is the quiet time `tacit dream --idle` waits for when the flag
// has no value; the idle job passes it explicitly.
const defaultIdle = 20 * time.Minute

// runDream implements `tacit dream [--model M] [--dry-run] [--max-calls N]
// [--idle D] [--session T]`: digest transcript deltas since the last run,
// extract facts with the dreamer model, write them to inbox/dreamer@<host>/,
// push, and append a metering line. The first run on a machine only records
// the cursors. --idle takes only the transcripts that went quiet or
// compacted (dream.Select); --session only one transcript. One dream runs
// at a time on a machine: an idle or session run that finds one running
// exits 0 with "busy" and touches nothing; a full run waits for it, up to
// dreamLockWait. An idle or session run also passes over a transcript that
// failed within dream.FailureBackoff (the full run retries it), and on a
// machine that cannot write as the dreamer it does nothing, quietly.
func runDream(args []string) int {
	fs := flag.NewFlagSet("dream", flag.ExitOnError)
	model := fs.String("model", dream.DefaultModel, "extraction model")
	dry := fs.Bool("dry-run", false, "extract and print, write nothing, keep cursors")
	maxCalls := fs.Int("max-calls", 40, "cap on model calls per run")
	maxRuntime := fs.String("max-runtime", "45m", "batch runtime before pausing; 0 disables the batch limit")
	envFile := fs.String("env-file", "", "KEY=VALUE lines (the variables claude -p needs, for example the Bedrock ones) set before `claude -p` runs; for launchd, which has no EnvironmentFile")
	idle := fs.Duration("idle", 0, "only transcripts with no write for this long (--idle alone: 20m), or whose new content holds a compaction")
	session := fs.String("session", "", "only this transcript (its path; a Hermes session id)")
	fs.Parse(idleDefault(args))
	if *maxCalls < 0 {
		fmt.Fprintln(os.Stderr, "dream: --max-calls must be nonnegative")
		return 2
	}
	if *idle < 0 {
		fmt.Fprintln(os.Stderr, "dream: --idle must be a positive duration")
		return 2
	}
	home, _ := os.UserHomeDir()
	stateDir := platform.StateDir(home)
	// A selecting run (--idle every 10 minutes, --session from the PreCompact
	// hook) is one of many: it yields to a running dream, it fails quietly,
	// and it does not retry a transcript that just failed.
	selecting := *idle > 0 || *session != ""
	// Before anything reads or moves a cursor. A dry run writes no state and
	// takes no lock.
	if !*dry {
		lockPath := filepath.Join(stateDir, "dreamer.lock")
		var release func()
		var err error
		if selecting {
			release, err = tryJobLock(stateDir, "dreamer")
		} else {
			// The full run comes every 4 hours: it waits for an idle or session
			// dream rather than miss its turn.
			wctx, wcancel := context.WithTimeout(context.Background(), dreamLockWait)
			release, err = waitJobLock(wctx, stateDir, "dreamer", func() {
				fmt.Fprintln(os.Stderr, "dream: waiting for the dream that holds", lockPath)
			})
			wcancel()
			if errors.Is(err, context.DeadlineExceeded) {
				err = platform.ErrBusy
			}
		}
		if errors.Is(err, platform.ErrBusy) {
			fmt.Println("dream: busy (another dream holds " + lockPath + "); nothing done")
			return 0
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "dream: lock:", err)
			return 2
		}
		defer release()
	}
	if *envFile != "" {
		if err := loadEnvFile(*envFile); err != nil {
			fmt.Fprintln(os.Stderr, "dream:", err)
			return 2
		}
		// The flag default was read before the file: a TACIT_DREAM_MODEL
		// line in it (launchd has no other way to set one) wins over that,
		// and an explicit --model wins over both.
		explicit := false
		fs.Visit(func(f *flag.Flag) { explicit = explicit || f.Name == "model" })
		*model = dreamModel(*model, explicit)
		if !explicit {
			*model = modelrun.Model("dream", "opus")
		}
	}
	explicitRuntime := false
	fs.Visit(func(f *flag.Flag) { explicitRuntime = explicitRuntime || f.Name == "max-runtime" })
	if !explicitRuntime {
		if value := os.Getenv("TACIT_DREAM_MAX_RUNTIME"); value != "" {
			*maxRuntime = value
		}
	}
	limit, err := time.ParseDuration(*maxRuntime)
	if err != nil || limit < 0 {
		fmt.Fprintln(os.Stderr, "dream: --max-runtime must be a nonnegative duration")
		return 2
	}

	// Before any model call or cursor move: a read-only clone, or one without
	// the dreamer's key, does nothing (its facts could never be pushed, and
	// the moved cursors would skip them for good). A read-only clone with the
	// local layer dreams into this machine's overlay instead (local_layer.go).
	local := false
	if ws, err := store.Default(); err == nil && ws.Exists() {
		local = localLayer(context.Background(), ws)
		if local && !overlay.Outside(overlay.Dir(), ws.Dir) {
			if selecting {
				return 0
			}
			fmt.Fprintln(os.Stderr, "dream: nothing done:", overlay.ErrInside)
			return 2
		}
	}
	if !*dry && !local {
		ws, err := store.Default()
		if err != nil || !ws.Exists() {
			fmt.Fprintln(os.Stderr, "dream: no store")
			return 2
		}
		if selecting {
			// The idle job runs every 10 minutes and the PreCompact hook at
			// every compaction: on a read-only clone or a machine without the
			// dreamer's key (a Mac that only reads) they end here, with no line.
			if writerCheck(context.Background(), ws, "dreamer") != nil {
				return 0
			}
		} else if refuseWriter(context.Background(), ws, "dreamer", "dream") {
			return 2
		}
	}
	recover := func() int {
		if local {
			return 0 // nothing is pushed from the local layer, so nothing waits for a push
		}
		s, _ := store.Default()
		timeout, err := modelrun.Timeout("dream")
		if err != nil {
			fmt.Fprintln(os.Stderr, "dream:", err)
			return 2
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		n, err := recoverDreamNotes(ctx, s)
		cancel()
		if err != nil {
			fmt.Fprintln(os.Stderr, "dream: recover:", err)
			return 1
		}
		if n > 0 {
			fmt.Printf("dream: recovered %d pending note(s)\n", n)
		}
		return 0
	}
	// A selecting run usually finds nothing to take: it syncs and recovers
	// only when it found something.
	if !*dry && !selecting {
		if code := recover(); code != 0 {
			return code
		}
	}
	statePath := filepath.Join(stateDir, "dreamer.json")
	st, err := dream.LoadState(statePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dream:", err)
		return 2
	}
	fresh := len(st.Offsets) == 0
	if fresh && *session != "" {
		// Recording one transcript's cursor would make every other one look
		// new to the next run, which would replay them from the start.
		fmt.Println("dream: no cursors yet; the first full run records them")
		return 0
	}
	if err := digest.RedactionReady(); err != nil {
		fmt.Fprintln(os.Stderr, "dream: redaction needs scrump with the text profile:", err)
		return 2
	}
	sel := dream.Select{Idle: *idle, Now: time.Now()}
	if *session != "" {
		sel.Only = *session
		if abs, err := filepath.Abs(*session); err == nil && strings.ContainsRune(*session, filepath.Separator) {
			sel.Only = abs
		}
	}
	windows, err := dream.WindowsFor(dream.DefaultSources(home), st, fresh, sel)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dream:", err)
		return 2
	}
	if !*dry {
		recordWindowRepos(home, windows)
	}
	if fresh {
		if !*dry {
			st.LastRun = time.Now().Format(time.RFC3339)
			if err := dream.SaveState(statePath, st); err != nil {
				fmt.Fprintln(os.Stderr, "dream: state:", err)
				return 1
			}
		}
		fmt.Printf("dream: first run, recorded cursors for %d transcript files; facts start next run\n", len(st.Offsets))
		return 0
	}
	// Count the full delta before capping calls. Every chunk shares its file's
	// end offset; advancing after only a prefix would skip the remaining chunks.
	pending := map[string]int{}
	var remaining []dream.Window
	backedOff := map[string]bool{}
	now := time.Now()
	for _, w := range windows {
		if slices.Contains(st.Completed[w.File], dreamWindowKey(w)) {
			continue
		}
		// A transcript whose window failed lately waits for the full run: its
		// cursor is unmoved, so the full run reads the same delta.
		if selecting && st.RecentlyFailed(w.File, now) {
			backedOff[w.File] = true
			continue
		}
		pending[w.File]++
		remaining = append(remaining, w)
	}
	windows = remaining
	if *dry && sel.Idle > 0 {
		// What the idle run took and why, before any cap.
		why := map[string]string{}
		for _, w := range windows {
			why[w.File] = w.Why
		}
		for _, f := range slices.Sorted(maps.Keys(why)) {
			fmt.Printf("dream: %s: %s\n", why[f], f)
		}
	}
	if len(backedOff) > 0 {
		fmt.Printf("dream: %d transcript(s) failed within %s; the full run retries them\n", len(backedOff), dream.FailureBackoff)
	}
	if selecting {
		if len(windows) == 0 {
			// Cursors moved past deltas with nothing to extract; no meter line,
			// so dreamer.log stays one line per run that did work.
			if !*dry {
				if err := dream.SaveState(statePath, st); err != nil {
					fmt.Fprintln(os.Stderr, "dream: state:", err)
					return 1
				}
			}
			if *session != "" {
				fmt.Println("dream: nothing new to dream in", *session)
			} else {
				fmt.Println("dream: no transcript with new content went quiet or compacted")
			}
			return 0
		}
		if !*dry {
			if code := recover(); code != 0 {
				return code
			}
		}
	}
	if len(windows) > *maxCalls {
		windows = windows[:*maxCalls]
	}
	host := store.ShortHost()
	s, err := store.Default()
	if err != nil || (!*dry && !s.Exists()) {
		fmt.Fprintln(os.Stderr, "dream: no store")
		return 2
	}
	ctx, cancel := dreamBatchContext(limit)
	defer cancel()

	meter := dream.Meter{At: time.Now().Format(time.RFC3339), Windows: len(windows), Local: local}
	switch {
	case *session != "":
		meter.Mode = "session"
	case *idle > 0:
		meter.Mode = "idle"
	}
	meter.ModelCalls = &modelrun.Trace{}
	ctx = modelrun.WithTrace(ctx, meter.ModelCalls)
	var written []string
	completed := 0
	paused := func(err error) bool {
		if errors.Is(context.Cause(ctx), errDreamRuntime) && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)) {
			meter.PauseReason = "runtime"
			return true
		}
		return false
	}
	// Cursor per file advances only when every window of that file succeeded.
	failed := map[string]bool{}
	for _, w := range windows {
		if err := ctx.Err(); err != nil {
			if !paused(err) {
				meter.Errors++
				fmt.Fprintln(os.Stderr, "dream:", err)
			}
			break
		}
		// Redact here, one scrump call per window that is actually sent, and
		// never move the cursor past a window whose redaction failed.
		lines, err := digest.Finish(w.Lines)
		if err != nil {
			meter.Errors++
			failed[w.File] = true
			pending[w.File]--
			fmt.Fprintln(os.Stderr, "dream: redaction:", err)
			continue
		}
		if err := ctx.Err(); err != nil {
			if !paused(err) {
				meter.Errors++
				fmt.Fprintln(os.Stderr, "dream:", err)
			}
			break
		}
		prompt := fmt.Sprintf(dream.Prompt, w.Repo, host, strings.Join(lines, "\n"))
		res, cost, in, err := dreamRun(ctx, *model, prompt)
		var extracted []dream.Extracted
		if err == nil {
			extracted, err = dream.ParseChecked(res)
		}
		meter.Calls++
		meter.CostUSD += cost
		meter.InTokens += in
		pending[w.File]--
		if err != nil {
			if paused(err) {
				break
			}
			meter.Errors++
			failed[w.File] = true
			fmt.Fprintln(os.Stderr, "dream:", err)
			continue
		}
		session := w.Session
		var facts []*fact.Fact
		for _, x := range extracted {
			f := dream.ToFact(x, w.Repo, host, session)
			meter.Facts++
			if *dry {
				fmt.Printf("- [%s/%s %s] %s\n", f.Meta.Scope, f.Meta.Type, f.Meta.Source.Kind, f.Meta.Description)
				if q := strings.Join(strings.Fields(string(x.Quote)), " "); f.Meta.Source.Kind == "user" && q != "" {
					fmt.Printf("  quote (%s): %q\n", session, q)
				}
				continue
			}
			facts = append(facts, f)
		}
		if !*dry {
			var paths []string
			var err error
			if local {
				paths, err = persistLocalDreamWindow(s, facts)
			} else {
				paths, err = persistDreamWindow(ctx, s, facts)
			}
			if err != nil {
				if paused(err) {
					break
				}
				meter.Errors++
				failed[w.File] = true
				fmt.Fprintln(os.Stderr, "dream: persist:", err)
			} else {
				completed++
				written = append(written, paths...)
				if st.Completed == nil {
					st.Completed = map[string][]string{}
				}
				st.Completed[w.File] = append(st.Completed[w.File], dreamWindowKey(w))
				if pending[w.File] == 0 && !failed[w.File] {
					st.Offsets[w.File] = w.End
					delete(st.Completed, w.File)
					delete(st.FailedAt, w.File)
				}
				st.LastRun = meter.At
				if err := dream.SaveState(statePath, st); err != nil {
					meter.Errors++
					fmt.Fprintln(os.Stderr, "dream: state:", err)
				}
			}
		} else {
			completed++
		}
	}
	for f := range st.FailedAt {
		if !st.RecentlyFailed(f, now) {
			delete(st.FailedAt, f) // past the backoff: the entry says nothing
		}
	}
	for f := range failed {
		st.Failed(f, now)
	}
	meter.RemainingWindows = len(remaining) - completed
	if meter.PauseReason == "" && len(remaining) > *maxCalls {
		meter.PauseReason = "call_limit"
	}
	if !*dry {
		st.LastRun = meter.At
		if err := dream.SaveState(statePath, st); err != nil {
			meter.Errors++
			fmt.Fprintln(os.Stderr, "dream: state:", err)
		}
		if f, err := os.OpenFile(filepath.Join(stateDir, "dreamer.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			json.NewEncoder(f).Encode(meter)
			f.Close()
		}
	}
	if meter.PauseReason != "" {
		fmt.Printf("dream: paused (%s); %d window(s) remain for the next run\n", meter.PauseReason, meter.RemainingWindows)
	}
	if local && !*dry {
		fmt.Println("dream: local layer: notes go to this machine's overlay, " + filepath.Join(overlay.Dir(), overlay.InboxDir, "dreamer@"+host) + "; nothing is pushed")
	}
	fmt.Printf("dream: %d window(s), %d call(s), %d fact(s), %d written, %d in-tokens, %s, %d error(s)\n", meter.Windows, meter.Calls, meter.Facts, len(written), meter.InTokens, meter.ModelCalls.CostSummary(meter.CostUSD), meter.Errors)
	if meter.Errors > 0 {
		return 1
	}
	return 0
}

// idleDefault lets --idle stand alone: with no duration after it, it is
// --idle=20m. Go's flag package gives a flag a value or none, not either.
func idleDefault(args []string) []string {
	out := slices.Clone(args)
	for i, a := range out {
		if a != "--idle" && a != "-idle" {
			continue
		}
		if i+1 < len(out) {
			if _, err := time.ParseDuration(out[i+1]); err == nil {
				continue
			}
		}
		out[i] = "--idle=" + defaultIdle.String()
	}
	return out
}

// Content and chunk position identify durable progress even when a transcript
// grows between runs. Changed chunks are processed again instead of skipped.
func dreamWindowKey(w dream.Window) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%d:%d:%s\n%s", w.Start, w.Part, w.Repo, strings.Join(w.Lines, "\n")))))
}

// loadEnvFile sets the variables of a systemd-style environment file in this
// process, so `claude -p` inherits them: KEY=VALUE lines, an optional
// leading `export `, blank lines and # comments skipped, one pair of
// matching quotes around a value removed. Values are never printed; an
// error names the file and line only.
// dreamModel is the model after an env file was loaded: an explicit --model,
// else $TACIT_DREAM_MODEL as it is now, else the flag's default.
func dreamModel(flagModel string, explicit bool) string {
	if v := strings.TrimSpace(os.Getenv("TACIT_DREAM_MODEL")); v != "" && !explicit {
		return v
	}
	return flagModel
}

func loadEnvFile(path string) error {
	defer setExplicitRunner()
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("env file: %w (it holds the variables claude -p needs, for example the Bedrock ones; it may be empty)", err)
	}
	// PowerShell Set-Content -Encoding UTF8 and older Notepad write a BOM.
	for i, line := range strings.Split(strings.TrimPrefix(string(b), "\ufeff"), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" || strings.ContainsAny(k, " \t") {
			return fmt.Errorf("env file %s: line %d is not KEY=VALUE", path, i+1)
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		if err := os.Setenv(k, v); err != nil {
			return fmt.Errorf("env file %s: line %d: %v", path, i+1, err)
		}
	}
	return nil
}

// Existing files from an interrupted write are retried, while a fact already
// committed at HEAD is left alone. Never include unrelated staged paths.
func dreamUncommitted(ctx context.Context, s *store.Store, paths []string) ([]string, error) {
	paths = dedupe(paths)
	if len(paths) == 0 {
		return nil, nil
	}
	args := append([]string{"ls-tree", "-rz", "--name-only", "HEAD", "--"}, paths...)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = s.Dir
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return nil, fmt.Errorf("read committed dreamer paths: %w", err)
	}
	committed := map[string]bool{}
	for _, p := range strings.Split(string(out), "\x00") {
		committed[p] = true
	}
	var pending []string
	for _, p := range paths {
		if !committed[p] {
			pending = append(pending, p)
		}
	}
	return pending, nil
}

// Retry only validated notes in this machine's dreamer inbox. Old failed
// pushes must not leave their notes stranded after a cursor was saved.
func recoverDreamNotes(ctx context.Context, s *store.Store) (n int, err error) {
	err = s.WithLock(ctx, func() error { var inner error; n, inner = recoverDreamNotesLocked(ctx, s); return inner })
	return n, err
}

func recoverDreamNotesLocked(ctx context.Context, s *store.Store) (int, error) {
	if err := s.SyncLocked(ctx); err != nil {
		return 0, err
	}
	if err := s.PushPendingLocked(ctx); err != nil {
		return 0, err
	}
	prefix := filepath.ToSlash(filepath.Join("inbox", "dreamer@"+store.ShortHost()))
	cmd := exec.CommandContext(ctx, "git", "ls-files", "--others", "--exclude-standard", "-z", "--", prefix)
	cmd.Dir = s.Dir
	out, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	var paths []string
	for _, rel := range strings.Split(string(out), "\x00") {
		if strings.HasSuffix(rel, ".md") {
			paths = append(paths, rel)
		}
	}
	if len(paths) == 0 {
		return 0, nil
	}
	fs, perrs, err := fact.Walk(s.Dir)
	if err != nil {
		return 0, err
	}
	byPath := map[string]*fact.Fact{}
	for _, f := range fs {
		byPath[f.Path] = f
	}
	for _, rel := range paths {
		f := byPath[rel]
		if f == nil || !f.InInbox() || f.Meta.Source.Harness != "dreamer" || f.Meta.Source.Host != store.ShortHost() || fact.ClaimsEvidence(f) {
			return 0, fmt.Errorf("pending dreamer note is invalid or protected: %s (%d parse errors)", rel, len(perrs))
		}
	}
	err = s.CommitPushNamedLocked(ctx, paths, fmt.Sprintf("dreamer: recover %d note(s) from interrupted runs", len(paths)), "dreamer")
	if err != nil {
		return 0, err
	}
	return len(paths), nil
}

// A window's check, writes and push form one locked operation. Another
// dreamer cannot overwrite the first run's already committed slug.
func persistDreamWindow(ctx context.Context, s *store.Store, facts []*fact.Fact) (written []string, err error) {
	err = s.WithLock(ctx, func() error {
		if err := s.SyncLocked(ctx); err != nil {
			return err
		}
		if err := s.PushPendingLocked(ctx); err != nil {
			return err
		}
		var paths []string
		for _, f := range facts {
			rel := filepath.ToSlash(filepath.Join("inbox", "dreamer@"+store.ShortHost(), f.Meta.Name+".md"))
			full := filepath.Join(s.Dir, rel)
			if _, err := os.Stat(full); err == nil {
				paths = append(paths, rel)
				continue
			} else if !os.IsNotExist(err) {
				return err
			}
			b, err := f.Encode()
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
				return err
			}
			if err := writeDreamNote(full, bytes.NewReader(b)); err != nil {
				return err
			}
			paths = append(paths, rel)
		}
		pending, err := dreamUncommitted(ctx, s, paths)
		if err != nil {
			return err
		}
		if len(pending) > 0 {
			if err := s.CommitPushNamedLocked(ctx, pending, fmt.Sprintf("dreamer: %d fact(s) from 1 window(s)", len(pending)), "dreamer"); err != nil {
				return err
			}
		}
		written = pending
		return nil
	})
	return written, err
}

// Publish only a complete, flushed note. Interrupted writes stay outside the
// .md namespace, so recovery cannot mistake them for extracted facts.
func writeDreamNote(path string, content io.Reader) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".dream-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err := io.Copy(f, content); err != nil {
		return err
	}
	if err := f.Chmod(0644); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
