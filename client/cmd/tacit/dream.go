package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/dream"
	"github.com/agent-sh/tacit/client/internal/modelrun"
	"github.com/agent-sh/tacit/client/internal/store"
)

// runDream implements `tacit dream [--model M] [--dry-run] [--max-calls N]`:
// digest transcript deltas since the last run, extract facts with the
// dreamer model, write them to inbox/dreamer@<host>/, push, and append a
// metering line. The first run on a machine only records the cursors.
func runDream(args []string) int {
	fs := flag.NewFlagSet("dream", flag.ExitOnError)
	model := fs.String("model", dream.DefaultModel, "extraction model")
	dry := fs.Bool("dry-run", false, "extract and print, write nothing, keep cursors")
	maxCalls := fs.Int("max-calls", 40, "cap on model calls per run")
	envFile := fs.String("env-file", "", "KEY=VALUE lines (the variables claude -p needs, for example the Bedrock ones) set before `claude -p` runs; for launchd, which has no EnvironmentFile")
	fs.Parse(args)
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
			dream.SaveState(statePath, st)
		}
		fmt.Printf("dream: first run, recorded cursors for %d transcript files; facts start next run\n", len(st.Offsets))
		return 0
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
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	meter := dream.Meter{At: time.Now().Format(time.RFC3339), Windows: len(windows)}
	meter.ModelCalls = &modelrun.Trace{}
	ctx = modelrun.WithTrace(ctx, meter.ModelCalls)
	var written []string
	// Cursor per file advances only when every window of that file succeeded.
	pending := map[string]int{}
	failed := map[string]bool{}
	for _, w := range windows {
		pending[w.File]++
	}
	for _, w := range windows {
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
		prompt := fmt.Sprintf(dream.Prompt, w.Repo, host, strings.Join(lines, "\n"))
		res, cost, in, err := dream.RunCLI(ctx, *model, prompt)
		var extracted []dream.Extracted
		if err == nil {
			extracted, err = dream.ParseChecked(res)
		}
		meter.Calls++
		meter.CostUSD += cost
		meter.InTokens += in
		pending[w.File]--
		if err != nil {
			meter.Errors++
			failed[w.File] = true
			fmt.Fprintln(os.Stderr, "dream:", err)
			continue
		}
		if pending[w.File] == 0 && !failed[w.File] && !*dry {
			st.Offsets[w.File] = w.End
		}
		session := strings.TrimSuffix(filepath.Base(w.File), ".jsonl")
		for _, x := range extracted {
			f := dream.ToFact(x, w.Repo, host, session)
			meter.Facts++
			if *dry {
				fmt.Printf("- [%s/%s] %s\n", f.Meta.Scope, f.Meta.Type, f.Meta.Description)
				continue
			}
			rel := filepath.ToSlash(filepath.Join("inbox", "dreamer@"+host, f.Meta.Name+".md"))
			full := filepath.Join(s.Dir, rel)
			if _, err := os.Stat(full); err == nil {
				// Same slug already filed; keep the earlier one (the curator merges).
				continue
			}
			b, err := f.Encode()
			if err != nil {
				continue
			}
			os.MkdirAll(filepath.Dir(full), 0o755)
			if err := os.WriteFile(full, b, 0o644); err == nil {
				written = append(written, rel)
			}
		}
	}
	if !*dry {
		if len(written) > 0 {
			if err := s.CommitPush(ctx, written, fmt.Sprintf("dreamer: %d fact(s) from %d window(s)", len(written), meter.Windows), "dreamer"); err != nil {
				fmt.Fprintln(os.Stderr, "dream: push:", err)
				meter.Errors++
			}
		}
		st.LastRun = meter.At
		if err := dream.SaveState(statePath, st); err != nil {
			fmt.Fprintln(os.Stderr, "dream: state:", err)
		}
		if f, err := os.OpenFile(filepath.Join(stateDir, "dreamer.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			json.NewEncoder(f).Encode(meter)
			f.Close()
		}
	}
	fmt.Printf("dream: %d window(s), %d call(s), %d fact(s), %d written, %d in-tokens, %s, %d error(s)\n", meter.Windows, meter.Calls, meter.Facts, len(written), meter.InTokens, meter.ModelCalls.CostSummary(meter.CostUSD), meter.Errors)
	if meter.Errors > 0 {
		return 1
	}
	return 0
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
