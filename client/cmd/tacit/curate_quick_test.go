package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/curate"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/platform"
	"github.com/agent-sh/tacit/client/internal/store"
)

// quickHome is a local store with two pending notes: fresh-note, added now,
// and old-note, added three days ago. The curator CLI is a stand-in that
// splits whichever note it is sent into one fact and logs each prompt.
func quickHome(t *testing.T) (home string, prompts func() []string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell CLI fixture")
	}
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TACIT_STATE_DIR", "")
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("TACIT_STORE", filepath.Join(home, "store"))
	t.Setenv("TACIT_INDEX", filepath.Join(home, "index"))
	t.Setenv("TACIT_LOCAL", filepath.Join(home, "overlay"))
	t.Setenv("TACIT_PROJECTS", filepath.Join(home, "projects"))
	t.Setenv("TACIT_CURATE_RUNNER", "claude")
	t.Setenv("TACIT_JUDGE_RUNNER", "claude")
	stubScrump(t)
	if code := runInit(nil); code != 0 {
		t.Fatalf("init %d", code)
	}
	s := &store.Store{Dir: filepath.Join(home, "store")}
	add := func(name, desc string) {
		if _, err := addFact(context.Background(), s, "muse", fact.Meta{Name: name, Description: desc, Type: "machine_fact", Scope: "system", Source: fact.Source{Kind: "observed"}}, desc); err != nil {
			t.Fatal(err)
		}
	}
	add("old-note", "The walrus fixture driver uses port 7781")
	// Back-date the commit that added old-note.
	amend := exec.Command("git", "commit", "-q", "--amend", "--no-edit", "--no-verify")
	amend.Dir = s.Dir
	old := time.Now().Add(-72 * time.Hour).Format(time.RFC3339)
	amend.Env = append(os.Environ(), "GIT_COMMITTER_DATE="+old, "GIT_AUTHOR_DATE="+old)
	if out, err := amend.CombinedOutput(); err != nil {
		t.Fatalf("amend: %v\n%s", err, out)
	}
	add("fresh-note", "The quokka fixture driver uses port 7790")
	answer := func(origin, desc string) string {
		b, _ := json.Marshal([]map[string]any{{"origin": origin, "name": origin + "-fact", "description": desc, "type": "machine_fact", "scope": "system", "kind": "observed", "body": desc}})
		r, _ := json.Marshal(map[string]any{"result": string(b), "total_cost_usd": .01})
		return string(r)
	}
	t.Setenv("FIXTURE_OLD", answer("old-note", "The walrus fixture driver uses port 7781"))
	t.Setenv("FIXTURE_FRESH", answer("fresh-note", "The quokka fixture driver uses port 7790"))
	log := filepath.Join(home, "prompts")
	t.Setenv("FIXTURE_PROMPTS", log)
	fake := filepath.Join(home, "fake-cli")
	script := `#!/bin/sh
in=$(cat)
printf '%s\n\x1e\n' "$in" >> "$FIXTURE_PROMPTS"
case "$in" in
*"name=old-note"*) printf '%s\n' "$FIXTURE_OLD" ;;
*) printf '%s\n' "$FIXTURE_FRESH" ;;
esac
`
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TACIT_CLAUDE_COMMAND", fake)
	return home, func() []string {
		b, _ := os.ReadFile(log)
		var out []string
		for _, p := range strings.Split(string(b), "\n\x1e\n") {
			if strings.TrimSpace(p) != "" {
				out = append(out, p)
			}
		}
		return out
	}
}

// countSteps replaces the full curate's no-model steps with counters.
func countSteps(t *testing.T) (upkeep, stale *int) {
	t.Helper()
	u, s := 0, 0
	prevUpkeep, prevStale := curateUpkeep, curateStaleStep
	t.Cleanup(func() { curateUpkeep, curateStaleStep = prevUpkeep, prevStale })
	curateUpkeep = func(context.Context, *store.Store, string, bool, *curate.Report) { u++ }
	curateStaleStep = func(context.Context, *store.Store, bool) int { s++; return 0 }
	return &u, &s
}

// The quick curate splits only the note of the last day and runs none of the
// full run's later steps (derive, results, disowned, links, staleness); a
// second quick run inside --min-interval does nothing at all; the full run
// takes the older note and runs every step.
func TestQuickCurateTakesFreshNotesOnlyAndSkipsTheLaterSteps(t *testing.T) {
	home, prompts := quickHome(t)
	upkeep, stale := countSteps(t)
	if code := runCurate([]string{"--quick"}); code != 0 {
		t.Fatalf("quick exit %d", code)
	}
	ps := prompts()
	if len(ps) != 1 || !strings.Contains(ps[0], "name=fresh-note") || strings.Contains(ps[0], "old-note") {
		t.Fatalf("quick split %d prompt(s):\n%s", len(ps), strings.Join(ps, "\n---\n"))
	}
	if *upkeep != 0 || *stale != 0 {
		t.Fatalf("quick ran the later steps: upkeep %d, stale %d", *upkeep, *stale)
	}
	s := &store.Store{Dir: filepath.Join(home, "store")}
	if _, ok := findFact(s.Dir, "fresh-note-fact"); !ok {
		t.Fatal("the fresh note's fact was not written")
	}
	var logged curate.Report
	if b, err := os.ReadFile(filepath.Join(platform.StateDir(home), "curator.log")); err != nil || json.Unmarshal(b, &logged) != nil || !logged.Quick {
		t.Fatalf("curator.log: %+v %v", logged, err)
	}

	// Within --min-interval: nothing.
	if code := runCurate([]string{"--quick", "--min-interval", "1h"}); code != 0 || len(prompts()) != 1 {
		t.Fatalf("within the interval: exit %d, %d prompt(s)", code, len(prompts()))
	}
	// Past it, with nothing fresh pending: no model call, still no later step.
	if code := runCurate([]string{"--quick", "--min-interval", "1ns"}); code != 0 || len(prompts()) != 1 || *upkeep != 0 || *stale != 0 {
		t.Fatalf("nothing fresh: exit %d, %d prompt(s), upkeep %d, stale %d", code, len(prompts()), *upkeep, *stale)
	}

	if code := runCurate(nil); code != 0 {
		t.Fatalf("full exit %d", code)
	}
	if ps := prompts(); len(ps) != 2 || !strings.Contains(ps[1], "name=old-note") {
		t.Fatalf("full run prompts: %d", len(ps))
	}
	if *upkeep != 1 || *stale != 1 {
		t.Fatalf("full run steps: upkeep %d, stale %d", *upkeep, *stale)
	}
}

// One curate at a time (curator.lock): a quick run that finds one running
// exits 0 doing nothing, and stamps no run; a full run waits for it.
func TestQuickCurateSharesTheCurateLock(t *testing.T) {
	home, prompts := quickHome(t)
	countSteps(t)
	release, err := tryJobLock(platform.StateDir(home), "curator")
	if err != nil {
		t.Fatal(err)
	}
	if code := runCurate([]string{"--quick"}); code != 0 || len(prompts()) != 0 {
		t.Fatalf("busy quick: exit %d, %d prompt(s)", code, len(prompts()))
	}
	if _, ok := lastQuickRun(filepath.Join(platform.StateDir(home), "curate-quick.json")); ok {
		t.Fatal("a busy quick run stamped the interval")
	}
	done := make(chan int, 1)
	began := time.Now()
	go func() { done <- runCurate(nil) }()
	time.Sleep(time.Second)
	select {
	case code := <-done:
		t.Fatalf("the full run did not wait for the lock: exit %d", code)
	default:
	}
	release()
	select {
	case code := <-done:
		if code != 0 || len(prompts()) != 1 || time.Since(began) < time.Second {
			t.Fatalf("full run after the lock: exit %d, %d prompt(s)", code, len(prompts()))
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the full run never took the lock")
	}
}
