package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/fact"
)

// readDeliveryLog is every entry of the delivery log under state.
func readDeliveryLog(t *testing.T, state string) []logEntry {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(state, "delivery", "*.jsonl"))
	var out []logEntry
	for _, p := range files {
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<24)
		for sc.Scan() {
			var e logEntry
			if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
				t.Fatalf("%s: %v", p, err)
			}
			out = append(out, e)
		}
		f.Close()
	}
	return out
}

func candOf(e logEntry, name string) *logCand {
	for i := range e.Cands {
		if e.Cands[i].Name == name {
			return &e.Cands[i]
		}
	}
	return nil
}

// Each hook block is one entry of the private log: the request, the
// session's repositories and its terms, every candidate with its score,
// rank, share of the best, tier, provenance and age, what chose the lines
// printed and the gate that left the others out, the probability the policy
// had of showing each, and whether the block showed nothing (a block that
// showed nothing is an entry too). Each step an agent takes through the store
// (a tree folder, a search, a fact read whole, by MCP or the CLI) is an
// entry of its own. The log is mode 0600 in a 0700 directory, and with the setting off
// nothing is written.
func TestDeliveryLogRecordsEachBlockAndWhatItLeftOut(t *testing.T) {
	_, cwd := repoFirstRig(t,
		fact.Meta{Name: "pool-lock-order", Description: "Take the shard lock before the slot lock in the scheduler pool", Type: "past_mistake", Scope: "projects/widget", AppliesTo: fact.AppliesTo{Paths: []string{"src/scheduler/pool.go"}}},
		fact.Meta{Name: "scheduler-shard-locks", Description: "The scheduler pool keeps one shard lock per tenant and a slot lock per job", Type: "design_decision", Scope: "projects/widget"},
		fact.Meta{Name: "lint-before-push", Description: "Run the linter with the strict profile", Type: "user_standard", Scope: "projects/widget", AppliesTo: fact.AppliesTo{Commands: []string{"make lint"}}},
	)
	state := filepath.Join(t.TempDir(), "state")
	t.Setenv("TACIT_STATE_DIR", state)
	t.Setenv("TACIT_DELIVERY_REPO_START", "off")
	t.Setenv("TACIT_DELIVERY_PROMPT_ACTION", "off")

	prompt := "Fix the scheduler pool shard lock and slot lock race"
	out := hookOut(t, promptPayload(cwd, "s1", prompt), "prompt")
	if !strings.Contains(out, "scheduler-shard-locks") {
		t.Fatalf("the prompt block:\n%s", out)
	}
	hookOut(t, promptPayload(cwd, "s1", prompt), "prompt") // the same request again: every line was seen
	hookOut(t, toolPayload(cwd, "s1", "Bash", map[string]any{"command": "make lint"}), "pre-tool")
	hookOut(t, toolPayload(cwd, "s1", "Bash", map[string]any{"command": "ls -la"}), "pre-tool") // matches nothing
	hookOut(t, toolPayload(cwd, "s1", "mcp__tacit__tacit_fact", map[string]any{"name": "scheduler-shard-locks"}), "pre-tool")
	hookOut(t, toolPayload(cwd, "s1", "mcp__tacit__tacit_tree", map[string]any{"path": "projects/widget", "depth": 0}), "pre-tool")
	hookOut(t, toolPayload(cwd, "s1", "Bash", map[string]any{"command": "cd /x && tacit search --scope projects/widget lock order | head; ~/bin/tacit fact pool-lock-order"}), "pre-tool")

	log := readDeliveryLog(t, state)
	var prompts, pushes, navs []logEntry
	for _, e := range log {
		switch e.Hook {
		case "prompt":
			prompts = append(prompts, e)
		case "push":
			pushes = append(pushes, e)
		case "nav":
			navs = append(navs, e)
		}
	}
	// The pushes: make lint, ls (nothing), the two MCP calls' and the
	// shell's search (nothing).
	if len(prompts) != 2 || len(pushes) != 5 || len(navs) != 4 {
		t.Fatalf("entries: %d prompt, %d push, %d nav, want 2, 5, 4: %+v", len(prompts), len(pushes), len(navs), log)
	}
	first := prompts[0]
	if first.V != deliveryLogVersion || first.ID == "" || first.Session != "s1" || first.Request != prompt || first.Floor != promptFloor || first.Relative != promptRelative ||
		len(first.Terms) == 0 || !strings.Contains(strings.Join(first.Repos, ","), "widget") || first.Policy != "gates" || first.Nothing || first.PNothing != 0 {
		t.Fatalf("the prompt entry: %+v", first)
	}
	c := candOf(first, "scheduler-shard-locks")
	if c == nil || !c.Delivered || c.Via != "ranked" || c.Dropped != "" || c.Rank < 0 || c.Share <= 0 || c.Share > 1 || c.Score <= 0 ||
		c.Tier != 1 || c.Prov != "agent" || !c.Tied || c.AgeDays < 0 || c.Created == "" || c.Matched < 2 || c.Desc == "" || c.P != 1 {
		t.Fatalf("the delivered line: %+v", c)
	}
	again := candOf(prompts[1], "scheduler-shard-locks")
	if again == nil || again.Delivered || again.Dropped != "seen" || again.P != 0 || !prompts[1].Nothing || prompts[1].PNothing != 1 {
		t.Fatalf("the line the session had: %+v, nothing %v %v", again, prompts[1].Nothing, prompts[1].PNothing)
	}
	if p := candOf(pushes[0], "lint-before-push"); p == nil || !p.Delivered || p.Via != "meta" || p.P != 1 || pushes[0].Request != "Bash make lint" || pushes[0].Nothing {
		t.Fatalf("the push: %+v", pushes[0])
	}
	if p := pushes[1]; p.Request != "Bash ls -la" || !p.Nothing || p.PNothing != 1 || len(p.Cands) != 0 {
		t.Fatalf("the push that showed nothing: %+v", p)
	}
	want := []logNav{
		{Tool: "fact", Names: []string{"scheduler-shard-locks"}, Levels: -1},
		{Tool: "tree", Path: "projects/widget", Depth: 2, Levels: 0},
		{Tool: "search", Path: "projects/widget", Query: "lock order", Depth: 2, Levels: -1},
		{Tool: "fact", Names: []string{"pool-lock-order"}, Levels: -1},
	}
	for i, w := range want {
		got, _ := json.Marshal(navs[i].Nav)
		exp, _ := json.Marshal(w)
		if string(got) != string(exp) || navs[i].Session != "s1" {
			t.Fatalf("nav %d: %s, want %s", i, got, exp)
		}
	}
	if runtime.GOOS != "windows" {
		dir, _ := os.Stat(filepath.Join(state, "delivery"))
		files, _ := filepath.Glob(filepath.Join(state, "delivery", "*.jsonl"))
		fi, _ := os.Stat(files[0])
		if dir.Mode().Perm() != 0o700 || fi.Mode().Perm() != 0o600 {
			t.Fatalf("modes: dir %v, file %v", dir.Mode().Perm(), fi.Mode().Perm())
		}
	}

	t.Setenv("TACIT_DELIVERY_LOG", "off")
	n := len(readDeliveryLog(t, state))
	hookOut(t, promptPayload(cwd, "s2", prompt), "prompt")
	hookOut(t, toolPayload(cwd, "s2", "mcp__tacit__tacit_fact", map[string]any{"name": "pool-lock-order"}), "pre-tool")
	if got := len(readDeliveryLog(t, state)); got != n {
		t.Fatalf("the log off wrote %d entries", got-n)
	}
}

// A prompt candidate the floor, the share of the best or the ceiling left
// out is in the entry with that gate.
func TestDeliveryLogNamesThePromptGate(t *testing.T) {
	_, cwd := repoFirstRig(t,
		fact.Meta{Name: "queue-retry-backoff", Description: "Queue retries back off exponentially with jitter in the queue worker", Type: "design_decision", Scope: "projects/widget"},
		fact.Meta{Name: "queue-retry-cap", Description: "Queue retries stop after five attempts in the queue worker", Type: "design_decision", Scope: "projects/widget"},
		fact.Meta{Name: "queue-worker-name", Description: "The queue worker binary is called qw", Type: "reference", Scope: "projects/widget"},
	)
	state := filepath.Join(t.TempDir(), "state")
	t.Setenv("TACIT_STATE_DIR", state)
	t.Setenv("TACIT_DELIVERY_REPO_START", "off")
	t.Setenv("TACIT_DELIVERY_PROMPT_ACTION", "off")
	t.Setenv("TACIT_DELIVERY_PROMPT_CEILING", "1")
	hookOut(t, promptPayload(cwd, "s1", "queue worker retries: backoff jitter and attempts"), "prompt")
	log := readDeliveryLog(t, state)
	if len(log) != 1 {
		t.Fatalf("entries: %+v", log)
	}
	delivered, gates := 0, map[string]int{}
	for _, c := range log[0].Cands {
		if c.Delivered {
			delivered++
		} else {
			gates[c.Dropped]++
		}
	}
	if delivered != 1 || gates["ceiling"]+gates["relative"]+gates["floor"] != len(log[0].Cands)-1 || gates[""] != 0 {
		t.Fatalf("ceiling 1: %d delivered, gates %v: %+v", delivered, gates, log[0].Cands)
	}
}

// A day's file is made 0600; making it removes the days older than the
// window, and a day's file stops growing at its cap.
func TestDeliveryLogKeepsItsWindowAndCap(t *testing.T) {
	state := t.TempDir()
	t.Setenv("TACIT_STATE_DIR", state)
	t.Setenv("TACIT_DELIVERY_LOG_DAYS", "3")
	dir := filepath.Join(state, "delivery")
	os.MkdirAll(dir, 0o700)
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	for _, d := range []string{"2026-03-01", "2026-03-06", "2026-03-07", "2026-03-09"} {
		os.WriteFile(filepath.Join(dir, d+".jsonl"), []byte("{}\n"), 0o600)
	}
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600)
	writeLogEntry(&logEntry{V: 1, Hook: "prompt"}, now)
	var left []string
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if strings.Join(left, " ") != "2026-03-07.jsonl 2026-03-09.jsonl 2026-03-10.jsonl notes.txt" {
		t.Fatalf("left: %v", left)
	}
	today := filepath.Join(dir, "2026-03-10.jsonl")
	os.Truncate(today, deliveryLogDayCap-10)
	writeLogEntry(&logEntry{V: 1, Hook: "prompt", Request: "past the cap"}, now)
	if fi, _ := os.Stat(today); fi.Size() != deliveryLogDayCap-10 {
		t.Fatalf("grew past the cap: %d", fi.Size())
	}
	// A link in a day's place is never followed.
	os.Remove(today)
	target := filepath.Join(t.TempDir(), "elsewhere")
	os.WriteFile(target, nil, 0o600)
	if os.Symlink(target, today) == nil {
		writeLogEntry(&logEntry{V: 1, Hook: "prompt"}, now)
		if b, _ := os.ReadFile(target); len(b) != 0 {
			t.Fatalf("wrote through a link: %q", b)
		}
	}
}
