package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
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

// runDream implements `tacit dream [--model M] [--dry-run] [--max-calls N]`:
// digest transcript deltas since the last run, extract facts with the
// dreamer model, write them to inbox/dreamer@<host>/, push, and append a
// metering line. The first run on a machine only records the cursors.
func runDream(args []string) int {
	fs := flag.NewFlagSet("dream", flag.ExitOnError)
	model := fs.String("model", dream.DefaultModel, "extraction model")
	dry := fs.Bool("dry-run", false, "extract and print, write nothing, keep cursors")
	maxCalls := fs.Int("max-calls", 40, "cap on model calls per run")
	maxRuntime := fs.String("max-runtime", "45m", "batch runtime before pausing; 0 disables the batch limit")
	envFile := fs.String("env-file", "", "KEY=VALUE lines (the variables claude -p needs, for example the Bedrock ones) set before `claude -p` runs; for launchd, which has no EnvironmentFile")
	fs.Parse(args)
	if *maxCalls < 0 {
		fmt.Fprintln(os.Stderr, "dream: --max-calls must be nonnegative")
		return 2
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
			*model = modelrun.Model("dream", "haiku")
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
	// the moved cursors would skip them for good).
	if !*dry {
		ws, err := store.Default()
		if err != nil || !ws.Exists() {
			fmt.Fprintln(os.Stderr, "dream: no store")
			return 2
		}
		if refuseWriter(context.Background(), ws, "dreamer", "dream") {
			return 2
		}
	}
	if !*dry {
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
	}
	home, _ := os.UserHomeDir()
	stateDir := filepath.Join(home, ".local", "state", "tacit")
	statePath := filepath.Join(stateDir, "dreamer.json")
	st, err := dream.LoadState(statePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dream:", err)
		return 2
	}
	fresh := len(st.Offsets) == 0
	if err := digest.RedactionReady(); err != nil {
		fmt.Fprintln(os.Stderr, "dream: redaction needs scrump with the text profile:", err)
		return 2
	}
	windows, err := dream.Windows(dream.DefaultSources(home), st, fresh)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dream:", err)
		return 2
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
	for _, w := range windows {
		if slices.Contains(st.Completed[w.File], dreamWindowKey(w)) {
			continue
		}
		pending[w.File]++
		remaining = append(remaining, w)
	}
	windows = remaining
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

	meter := dream.Meter{At: time.Now().Format(time.RFC3339), Windows: len(windows)}
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
		session := strings.TrimSuffix(filepath.Base(w.File), ".jsonl")
		var facts []*fact.Fact
		for _, x := range extracted {
			f := dream.ToFact(x, w.Repo, host, session)
			meter.Facts++
			if *dry {
				fmt.Printf("- [%s/%s] %s\n", f.Meta.Scope, f.Meta.Type, f.Meta.Description)
				continue
			}
			facts = append(facts, f)
		}
		if !*dry {
			paths, err := persistDreamWindow(ctx, s, facts)
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
	fmt.Printf("dream: %d window(s), %d call(s), %d fact(s), %d written, %d in-tokens, %s, %d error(s)\n", meter.Windows, meter.Calls, meter.Facts, len(written), meter.InTokens, meter.ModelCalls.CostSummary(meter.CostUSD), meter.Errors)
	if meter.Errors > 0 {
		return 1
	}
	return 0
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
