package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/platform"
	"github.com/agent-sh/tacit/client/internal/search"
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
	if first.V != deliveryLogVersion || first.ID == "" || first.Session != "s1" || !subset(first.Terms, "scheduler", "pool", "shard", "lock", "slot") || first.NTerms != 7 || first.Floor != promptFloor || first.Relative != promptRelative ||
		len(first.Terms) == 0 || !strings.Contains(strings.Join(first.Repos, ","), "widget") || first.Policy != "gates" || first.Nothing || first.PNothing != 0 {
		t.Fatalf("the prompt entry: %+v", first)
	}
	c := candOf(first, "scheduler-shard-locks")
	if c == nil || !c.Delivered || c.Via != "ranked" || c.Dropped != "" || c.Rank < 0 || c.Share <= 0 || c.Share > 1 || c.Score <= 0 ||
		c.Tier != 1 || c.Prov != "agent" || !c.Tied || c.AgeDays < 0 || c.Created == "" || c.Matched < 2 || c.P != 1 {
		t.Fatalf("the delivered line: %+v", c)
	}
	again := candOf(prompts[1], "scheduler-shard-locks")
	if again == nil || again.Delivered || again.Dropped != "seen" || again.P != 0 || !prompts[1].Nothing || prompts[1].PNothing != 1 {
		t.Fatalf("the line the session had: %+v, nothing %v %v", again, prompts[1].Nothing, prompts[1].PNothing)
	}
	if p := candOf(pushes[0], "lint-before-push"); p == nil || !p.Delivered || p.Via != "meta" || p.P != 1 || p.Echo != 0 || pushes[0].Tool != "Bash" || pushes[0].NTerms != 2 || pushes[0].Nothing {
		t.Fatalf("the push: %+v", pushes[0])
	}
	if p := pushes[1]; p.Tool != "Bash" || len(p.Terms) != 0 || !p.Nothing || p.PNothing != 1 || len(p.Cands) != 0 {
		t.Fatalf("the push that showed nothing: %+v", p)
	}
	want := []logNav{
		{Tool: "fact", Names: []string{"scheduler-shard-locks"}, Levels: -1},
		{Tool: "tree", Path: "projects/widget", Depth: 2, Levels: 0},
		{Tool: "search", Path: "projects/widget", Terms: []string{"lock", "order"}, NTerms: 2, Depth: 2, Levels: -1},
		{Tool: "fact", Names: []string{"pool-lock-order"}, Levels: -1},
	}
	if navs[2].ID == navs[3].ID {
		t.Fatalf("two steps of one call share an id: %s", navs[2].ID)
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

	// What the store does not hold never reaches the log: a word no fact
	// has, in a prompt, a push's command and a search, a fact name and a
	// folder the store lacks.
	before := len(log)
	hookOut(t, promptPayload(cwd, "s9", "Fix the scheduler pool shard lock with zqxjvword tonight"), "prompt")
	hookOut(t, toolPayload(cwd, "s9", "Bash", map[string]any{"command": "make lint zqxjvword"}), "pre-tool")
	hookOut(t, toolPayload(cwd, "s9", "mcp__tacit__tacit_search", map[string]any{"query": "shard lock zqxjvword"}), "pre-tool")
	hookOut(t, toolPayload(cwd, "s9", "mcp__tacit__tacit_fact", map[string]any{"name": "zqxjv-not-a-fact"}), "pre-tool")
	hookOut(t, toolPayload(cwd, "s9", "mcp__tacit__tacit_tree", map[string]any{"path": "projects/zqxjvnowhere"}), "pre-tool")
	added := readDeliveryLog(t, state)[before:]
	b, _ := json.Marshal(added)
	if strings.Contains(string(b), "zqxjv") {
		t.Fatalf("a word the store lacks is in the log: %s", b)
	}
	kinds := map[string]int{}
	for _, e := range added {
		kinds[e.Hook]++
		if e.Hook == "nav" && e.Nav.Tool == "search" && (!subset(e.Nav.Terms, "shard", "lock") || e.Nav.NTerms != 3) {
			t.Fatalf("the search's indexed terms: %+v", e.Nav)
		}
	}
	if kinds["prompt"] != 1 || kinds["push"] < 2 || kinds["nav"] != 3 {
		t.Fatalf("entries: %v", kinds)
	}

	t.Setenv("TACIT_DELIVERY_LOG", "off")
	n := len(readDeliveryLog(t, state))
	hookOut(t, promptPayload(cwd, "s2", prompt), "prompt")
	hookOut(t, toolPayload(cwd, "s2", "mcp__tacit__tacit_fact", map[string]any{"name": "pool-lock-order"}), "pre-tool")
	if got := len(readDeliveryLog(t, state)); got != n {
		t.Fatalf("the log off wrote %d entries", got-n)
	}
}

// Each prompt candidate the selection leaves out is logged with the gate
// that did it: the floor, the share of the best (which raised the floor),
// the ceiling, the session having it; a rule the slot adds is "rule_slot",
// a derived fact under its floor "floor". The worker that runs the gates
// for the utility provider passes no entry and logs nothing.
func TestDeliveryLogNamesEachPromptGate(t *testing.T) {
	t.Setenv("TACIT_DELIVERY_PROMPT_CEILING", "2")
	rule := search.Hit{Name: "rule", Path: "user/rule.md", Score: 12, Ruling: true}
	hits := []search.Hit{
		{Name: "best", Path: "user/best.md", Score: 40},
		{Name: "seen", Path: "user/seen.md", Score: 39},
		{Name: "second", Path: "user/second.md", Score: 36},
		{Name: "third", Path: "user/third.md", Score: 33},
		{Name: "under-share", Path: "user/under-share.md", Score: 25},
		rule,
	}
	derived := []search.Hit{{Name: "hot", Path: "derived/demo/hot.md", Scope: "derived/demo", Score: derivedPromptFloor - 1}}
	rec := &logEntry{}
	rec.pool(hits, nil)
	got := promptSelectionLogged("fix the widget queue retries now", hits, derived, map[string]bool{"seen": true}, promptFloor, rec)
	if names(got) != "best,rule" {
		t.Fatalf("selected %s", names(got))
	}
	want := map[string][2]string{
		"best": {"ranked", ""}, "seen": {"", "seen"}, "second": {"", "ceiling"}, "third": {"", "ceiling"},
		"under-share": {"", "relative"}, "rule": {"rule_slot", ""}, "hot": {"", "floor"},
	}
	for name, w := range want {
		c := candOf(*rec, name)
		if c == nil || c.Via != w[0] || c.Dropped != w[1] {
			t.Errorf("%s: %+v, want via %q dropped %q", name, c, w[0], w[1])
		}
	}
	if c := candOf(*rec, "best"); c.Rank != 0 || c.Share != 1 {
		t.Errorf("best: rank %d share %v", c.Rank, c.Share)
	}
	t.Setenv("TACIT_DELIVERY_PROMPT_RELATIVE", "0")
	rec = &logEntry{}
	promptSelectionLogged("fix the widget queue retries now", []search.Hit{{Name: "low", Path: "user/low.md", Score: promptFloor - 1}}, nil, nil, promptFloor, rec)
	if c := candOf(*rec, "low"); c == nil || c.Dropped != "floor" {
		t.Errorf("under the floor: %+v", c)
	}
	// No entry, nothing logged and no panic: the provider's worker.
	if names(promptSelectionLogged("fix the widget queue retries now", hits, nil, nil, promptFloor, nil)) == "" {
		t.Error("nothing selected without an entry")
	}
}

// A gate after the selection (the echo filter, withoutClaims) takes a line
// back from what chose it: its via is cleared and the gate named, and a
// line no gate had named yet gets it too.
func TestDeliveryLogDroppedAfterTheSelection(t *testing.T) {
	a, b, c := search.Hit{Name: "a", Path: "user/a.md"}, search.Hit{Name: "b", Path: "user/b.md"}, search.Hit{Name: "c", Path: "user/c.md"}
	rec := &logEntry{}
	rec.chose([]search.Hit{a, b}, "ranked")
	rec.cand(c)
	rec.dropped([]search.Hit{a, b, c}, []search.Hit{a}, "withheld")
	if x := candOf(*rec, "a"); x.Via != "ranked" || x.Dropped != "" {
		t.Errorf("kept: %+v", x)
	}
	for _, n := range []string{"b", "c"} {
		if x := candOf(*rec, n); x.Via != "" || x.Dropped != "withheld" {
			t.Errorf("%s: %+v", n, x)
		}
	}
	// Two files of one name: only the one printed is delivered.
	t.Cleanup(func() { dlog, logPending = nil, nil })
	dlog = &logEntry{V: deliveryLogVersion, ID: "x"}
	t.Setenv("TACIT_STATE_DIR", t.TempDir())
	store, overlay := search.Hit{Name: "twin", Path: "user/twin.md"}, search.Hit{Name: "twin", Path: "local/twin.md", Local: true}
	logChose([]search.Hit{store, overlay}, "ranked")
	e := dlog
	endLog([]search.Hit{store})
	for _, x := range e.Cands {
		if x.Delivered != (x.Path == store.Path) {
			t.Errorf("%s delivered %v", x.Path, x.Delivered)
		}
	}
}

// The echo share is measured on the whole request, as the echo filter
// reads it; the echo filter's lines
// are logged "echo".
func TestDeliveryLogEchoOnTheWholeRequest(t *testing.T) {
	t.Setenv("TACIT_STATE_DIR", t.TempDir())
	ix := relevanceIndex(t, []fact.Meta{
		{Name: "review-tool-failures-weekly", Description: "Review the tool failures weekly and fix their root cause"},
		{Name: "tool-failures-fix-at-source", Description: "Review tool failures weekly: fix the root cause in the mods, rules or scripts that let them happen, and log what changed"},
	})
	prompt := strings.Repeat("preamble words of a long scheduled prompt ", 60) + "Review the tool failures weekly and fix their root cause, then report"
	t.Cleanup(func() { dlog, logPending = nil, nil })
	beginLog("prompt", hookInput{SessionID: "s1"}, prompt, nil, nil)
	e := dlog
	got, err := promptHits(context.Background(), ix, search.Situation{}, prompt, nil)
	if err != nil {
		t.Fatal(err)
	}
	endLog(got)
	if len(e.Terms) > 24 {
		t.Fatalf("terms past the query's: %d", len(e.Terms))
	}
	if c := candOf(*e, "review-tool-failures-weekly"); c == nil || c.Delivered || c.Dropped != "echo" || c.Echo < echoShare {
		t.Fatalf("the echo: %+v", c)
	}
}

// The CLI's store steps are read as the command reads its flags: a flag's
// value is no search word, --k=v and -k v both count, a Windows path runs
// tacit too, and words that only mention tacit are no step.
func TestDeliveryLogReadsTheCLIStep(t *testing.T) {
	for cmd, want := range map[string]string{
		"tacit search --repo widget --limit 5 --offset 10 lock order":     "search||[]|-1|10|lock order",
		"FOO=1 tacit search -scope=projects/widget -json lock":            "search|projects/widget|[]|-1|0|lock",
		"tacit tree --depth=1 projects/widget":                            "tree|projects/widget|[]|1|0|",
		`C:\tools\tacit.exe fact pool-lock-order`:                         "fact||[pool-lock-order]|-1|0|",
		`git commit -m "use tacit fact to read one" && echo tacit tree x`: "",
		"tacit search lock order --scope projects/widget":                 "search||[]|-1|0|lock order --scope projects/widget",
		"tacit search -- --scope":                                         "search||[]|-1|0|--scope",
		"tacit fact one two; tacit fact Not_A_Slug; tacit tree a b":       "",
		`tacit search --command "make lint" lock`:                         "search||[]|-1|0|lock",
		`tacit search 'shard lock' --scope x`:                             "search||[]|-1|0|shard lock --scope x",
	} {
		var got []string
		for _, n := range navOf(hookInput{ToolName: "Bash"}, cmd) {
			got = append(got, fmt.Sprintf("%s|%s|%v|%d|%d|%s", n.Tool, n.Path, n.Names, n.Levels, n.Offset, n.query))
		}
		if strings.Join(got, ";") != want {
			t.Errorf("%s:\n got %q\nwant %q", cmd, strings.Join(got, ";"), want)
		}
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
	writeLogEntry(&logEntry{V: 1, Hook: "prompt", Terms: []string{"past", "cap"}}, now)
	if fi, _ := os.Stat(today); fi.Size() != deliveryLogDayCap-10 {
		t.Fatalf("grew past the cap: %d", fi.Size())
	}
	// A second hard link in a day's place is not written (Unix).
	if runtime.GOOS != "windows" {
		os.Remove(today)
		other := filepath.Join(t.TempDir(), "other")
		os.WriteFile(other, nil, 0o600)
		if os.Link(other, today) == nil {
			writeLogEntry(&logEntry{V: 1, Hook: "prompt"}, now)
			if b, _ := os.ReadFile(other); len(b) != 0 {
				t.Fatalf("wrote through a hard link: %q", b)
			}
		}
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

// With a utility provider the log says which policy chose the block: the
// provider's scores ("utility": its lines chosen by it with their scores and
// threshold, the pool's others left out by it), or the gates when the
// provider failed and their selection stands.
func TestDeliveryLogNamesTheUtilityPolicy(t *testing.T) {
	t.Setenv("TACIT_STATE_DIR", t.TempDir())
	t.Cleanup(func() { dlog, logPending, utilitySelectFn = nil, nil, utilitySelect })
	useful := search.Hit{Name: "endpoint-useful", Path: "user/endpoint-useful.md", Scope: "user", Score: 30}
	topical := search.Hit{Name: "endpoint-topical", Path: "user/endpoint-topical.md", Scope: "user", Score: 29}
	capped := search.Hit{Name: "endpoint-capped", Path: "user/endpoint-capped.md", Scope: "user", Score: 28}
	score := func(v float64) *float64 { return &v }
	status := "ok"
	utilitySelectFn = func(ctx context.Context, p string, hits, derived []search.Hit, seen map[string]bool, floor float64) ([]search.Hit, utilityAudit) {
		if status != "ok" {
			return promptSelectionFor(p, hits, derived, seen, floor), utilityAudit{Status: status}
		}
		return []search.Hit{useful}, utilityAudit{Status: "ok", Threshold: 0.7, QueryHash: "abc", Candidates: []utilityAuditHit{
			{ID: useful.Path, Score: score(0.95)}, {ID: topical.Path, Score: score(0.1)}, {ID: capped.Path, Score: score(0.9)}}}
	}
	run := func() *logEntry {
		t.Helper()
		beginLog("prompt", hookInput{SessionID: "s1"}, "endpoint api release", nil, nil)
		e := dlog
		dlog.pool([]search.Hit{useful, topical, capped}, nil)
		endLog(utilityLogged(context.Background(), "endpoint api release", []search.Hit{useful, topical, capped}, nil, nil, promptFloor))
		return e
	}
	e := run()
	u, tp := candOf(*e, "endpoint-useful"), candOf(*e, "endpoint-topical")
	if e.Policy != "utility" || e.UtilityThreshold == nil || *e.UtilityThreshold != 0.7 || u == nil || u.Via != "utility" || !u.Delivered || u.Utility == nil || *u.Utility != 0.95 ||
		tp == nil || tp.Delivered || tp.Via != "" || tp.Dropped != "utility" || tp.Utility == nil || *tp.Utility != 0.1 {
		t.Fatalf("the provider chose: %+v, %+v, %+v", e, u, tp)
	}
	if c := candOf(*e, "endpoint-capped"); c == nil || c.Delivered || c.Dropped != "ceiling" {
		t.Fatalf("over the threshold, past the cap: %+v", c)
	}
	status = "deadline"
	e = run()
	u = candOf(*e, "endpoint-useful")
	if e.Policy != "gates" || u == nil || u.Via != "ranked" || !u.Delivered || u.Utility != nil {
		t.Fatalf("the provider failed: policy %q, %+v", e.Policy, u)
	}
}

// The hook waits for its log lines at most logWriteBudget, all of them
// together: a write that stalls (a hung state directory) leaves the hook to
// answer.
func TestDeliveryLogWriteIsBounded(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	wait, write := logWriteWait, logWrite
	t.Cleanup(func() { logWriteWait, logWrite = wait, write })
	logWriteWait = logWriteBudget
	logWrite = func(*logEntry, time.Time) { <-release }
	start := time.Now()
	writeBounded(&logEntry{V: 1}, &logEntry{V: 1}, &logEntry{V: 1})
	if d := time.Since(start); d > logWriteBudget+time.Second {
		t.Fatalf("the hook waited %v for a stalled write", d)
	}
}

// The log keeps a request's terms only when the store's vocabulary holds
// them: an unindexed secret-shaped word and an unindexed ordinary word
// both stay out, and an indexed term is kept. Without an index none is
// kept, only how many there were. A nav step keeps a fact name the index has and a folder
// the store has, nothing else.
func TestDeliveryLogTermsAreTheStoresVocabulary(t *testing.T) {
	ix := relevanceIndex(t, []fact.Meta{
		{Name: "scheduler-shard-locks", Description: "The scheduler pool keeps one shard lock per tenant"},
	})
	key := "sk-" + "Ab3dEfGh1jKlMnOpQrStUvWx" // split so a scanner sees no key in the source
	terms, n := logTerms(ix, "fix the scheduler shard with "+key+" and frobnicate the quux")
	if strings.Join(terms, " ") != "scheduler shard" || n != 6 {
		t.Fatalf("terms %v of %d", terms, n)
	}
	if terms, n := logTerms(nil, "fix the scheduler shard"); terms != nil || n != 3 {
		t.Fatalf("without an index: %v of %d", terms, n)
	}
	b, _ := json.Marshal(logEntry{V: 1, Terms: terms})
	if strings.Contains(string(b), "ab3d") || strings.Contains(string(b), "frobnicate") {
		t.Fatalf("entry: %s", b)
	}
	store := t.TempDir()
	os.MkdirAll(filepath.Join(store, "projects", "widget"), 0o755)
	for in, want := range map[string]string{"projects/widget": "projects/widget", "/projects/widget/": "projects/widget", "projects/nothere": "", "../etc": "", "free text here": ""} {
		if got := storeFolder(store, in); got != want {
			t.Errorf("storeFolder(%q) = %q, want %q", in, got, want)
		}
	}
	if !ix.HasFact(context.Background(), "scheduler-shard-locks") || ix.HasFact(context.Background(), "not-a-fact") {
		t.Error("HasFact")
	}
}

// subset reports whether xs holds every one of want.
func subset(xs []string, want ...string) bool {
	for _, w := range want {
		if !slices.Contains(xs, w) {
			return false
		}
	}
	return true
}

// With the session's repositories the prompt ranks a wider pool and cuts
// it to twice the ceiling; the log holds the whole pool, its tail past the
// cut "ceiling" with its rank.
func TestDeliveryLogHoldsTheRepoRankedTail(t *testing.T) {
	var facts []fact.Meta
	for i := 0; i < 14; i++ {
		facts = append(facts, fact.Meta{Name: fmt.Sprintf("queue-retry-note-%02d", i), Description: fmt.Sprintf("queue retry backoff note %d for the worker pool jitter", i), Type: "design_decision", Scope: "projects/widget"})
	}
	_, cwd := repoFirstRig(t, facts...)
	state := filepath.Join(t.TempDir(), "state")
	t.Setenv("TACIT_STATE_DIR", state)
	t.Setenv("TACIT_DELIVERY_REPO_START", "off")
	t.Setenv("TACIT_DELIVERY_PROMPT_ACTION", "off")
	t.Setenv("TACIT_DELIVERY_PROMPT_CEILING", "2")
	hookOut(t, promptPayload(cwd, "s1", "queue retry backoff jitter for the worker pool"), "prompt")
	log := readDeliveryLog(t, state)
	if len(log) != 1 {
		t.Fatalf("entries: %d", len(log))
	}
	tail := 0
	for _, c := range log[0].Cands {
		if c.Rank >= 4 {
			if c.Dropped != "ceiling" || c.Delivered {
				t.Errorf("past the cut: %+v", c)
			}
			tail++
		}
	}
	if tail == 0 {
		t.Fatalf("no candidate past the cut of 4: %+v", log[0].Cands)
	}
}

// BenchmarkDeliveryLogWrite is one block's line of 50 candidates written
// (eval/DELIVERY.md, "Scoring each delivery": its cost).
func BenchmarkDeliveryLogWrite(b *testing.B) {
	b.Setenv("TACIT_STATE_DIR", b.TempDir())
	e := &logEntry{V: 1, Hook: "prompt", Terms: []string{"scheduler", "pool", "shard", "lock"}}
	for i := 0; i < 50; i++ {
		e.Cands = append(e.Cands, logCand{Name: fmt.Sprintf("fact-name-%d", i), Path: "projects/widget/x.md", Prov: "agent", Score: 12.3, Rank: i})
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		writeLogEntry(e, time.Now())
	}
}

// A repository can tie thousands of facts to itself: the start entry holds
// the pool's head (what was shown and as many again past the ceiling) and
// counts the rest, and no entry holds more than logCandCap candidates, the
// delivered lines always among them.
func TestDeliveryLogHoldsAPoolsHeadOnly(t *testing.T) {
	var facts []fact.Meta
	for i := 0; i < 30; i++ {
		facts = append(facts, fact.Meta{Name: fmt.Sprintf("widget-rule-%02d", i), Description: fmt.Sprintf("widget decision number %d about the build", i), Type: "design_decision", Scope: "projects/widget"})
	}
	_, cwd := repoFirstRig(t, facts...)
	state := filepath.Join(t.TempDir(), "state")
	t.Setenv("TACIT_STATE_DIR", state)
	t.Setenv("TACIT_DELIVERY_START_CEILING", "3")
	hookOut(t, startPayload(cwd, "s1"), "session-start", "--mcp")
	var start *logEntry
	for _, e := range readDeliveryLog(t, state) {
		if e.Hook == "start" {
			start = &e
		}
	}
	if start == nil {
		t.Fatal("no start entry")
	}
	if len(start.Cands) != 6 || start.Omitted < 24 {
		t.Fatalf("start entry: %d candidates, %d omitted", len(start.Cands), start.Omitted)
	}
	delivered := 0
	for _, c := range start.Cands {
		if c.Delivered {
			delivered++
		} else if c.Dropped != "ceiling" {
			t.Errorf("past the ceiling: %+v", c)
		}
	}
	if delivered != 3 {
		t.Fatalf("delivered %d", delivered)
	}

	// A prompt's whole pool with its derived, metadata and action lines
	// fits; a runaway one is cut by rank, every delivered line kept.
	e := &logEntry{}
	for i := 0; i < promptRankPool+3; i++ {
		e.Cands = append(e.Cands, logCand{Name: fmt.Sprintf("p%03d", i), Rank: i})
	}
	if e.capCands(); len(e.Cands) != promptRankPool+3 || e.Omitted != 0 {
		t.Fatalf("a full prompt pool was cut: %d, omitted %d", len(e.Cands), e.Omitted)
	}
	e = &logEntry{}
	n := logCandCap + 30
	for i := 0; i < n; i++ {
		e.Cands = append(e.Cands, logCand{Name: fmt.Sprintf("c%03d", i), Rank: i, Delivered: i == n-1})
	}
	e.capCands()
	last := fmt.Sprintf("c%03d", n-1)
	cut := fmt.Sprintf("c%03d", logCandCap-1)
	if len(e.Cands) != logCandCap || e.Omitted != 30 || !slices.ContainsFunc(e.Cands, func(c logCand) bool { return c.Name == last }) || slices.ContainsFunc(e.Cands, func(c logCand) bool { return c.Name == cut }) {
		t.Fatalf("capped: %d, omitted %d", len(e.Cands), e.Omitted)
	}
}

// With the titles setting on, a hook's agent notes are titles, the block
// says how to open one, and the log marks each printed line's form; off,
// the block is as before.
func TestHookTitlesWithTheSettingOn(t *testing.T) {
	_, cwd := repoFirstRig(t,
		fact.Meta{Name: "scheduler-shard-locks", Description: "The scheduler pool keeps one shard lock per tenant and a slot lock per job", Type: "design_decision", Scope: "projects/widget"},
	)
	state := filepath.Join(t.TempDir(), "state")
	t.Setenv("TACIT_STATE_DIR", state)
	t.Setenv("TACIT_DELIVERY_REPO_START", "off")
	t.Setenv("TACIT_DELIVERY_PROMPT_ACTION", "off")
	prompt := "Fix the scheduler pool shard lock and slot lock race"
	t.Setenv("TACIT_DELIVERY_TITLES", "off")
	if out := hookOut(t, promptPayload(cwd, "s1", prompt), "prompt"); !strings.Contains(out, "one shard lock per tenant") || strings.Contains(out, "titles:") {
		t.Fatalf("off: %s", out)
	}
	t.Setenv("TACIT_DELIVERY_TITLES", "on")
	out := hookOut(t, promptPayload(cwd, "s2", prompt), "prompt", "--mcp")
	if strings.Contains(out, "one shard lock per tenant") || !strings.Contains(out, "- scheduler-shard-locks: agent note (") || !strings.Contains(out, "), decision\n") || !strings.Contains(out, titlesNote) {
		t.Fatalf("on: %s", out)
	}
	var forms []string
	for _, e := range readDeliveryLog(t, state) {
		if c := candOf(e, "scheduler-shard-locks"); c != nil && c.Delivered {
			forms = append(forms, e.Session+":"+c.Form)
		}
	}
	// A harness without tacit's MCP server (no --mcp) keeps whole lines.
	if out := hookOut(t, promptPayload(cwd, "s3", prompt), "prompt"); !strings.Contains(out, "one shard lock per tenant") || strings.Contains(out, "titles:") {
		t.Fatalf("no MCP server: %s", out)
	}
	forms = forms[:0]
	for _, e := range readDeliveryLog(t, state) {
		if c := candOf(e, "scheduler-shard-locks"); c != nil && c.Delivered {
			forms = append(forms, e.Session+":"+c.Form)
		}
	}
	if strings.Join(forms, " ") != "s1:full s2:title s3:full" {
		t.Fatalf("forms: %v", forms)
	}
}

// Titles are the default: with the setting unset, a hook prints titles.
func TestTitlesAreTheDefault(t *testing.T) {
	t.Setenv("TACIT_DELIVERY_TITLES", "")
	if !deliveryTitles() {
		t.Fatal("titles are not the default")
	}
	t.Setenv("TACIT_DELIVERY_TITLES", "off")
	if deliveryTitles() {
		t.Fatal("titles off is not off")
	}
}

// Titles reach only the blocks the A/B measured: the start (its first line,
// which names its own way to open a fact, ends with "(titles)"), the prompt
// and a shell command's push. A resume, the start again after a compaction
// and the summary's selection keep whole lines, as does an edit's push. The
// log's form says which.
func TestTitlesOnlyWhereMeasured(t *testing.T) {
	r, home, payload := restartRig(t)
	t.Setenv("TACIT_DELIVERY_TITLES", "on")
	start := hookOut(t, payload("t1", "startup", ""), "session-start", "--mcp")
	first, _, _ := strings.Cut(start, "\n")
	if !strings.HasSuffix(first, startTitlesNote) || !strings.Contains(start, "- demo-note: agent note (") || strings.Contains(start, "a note about demo") {
		t.Fatalf("start:\n%s", start)
	}
	if !strings.Contains(start, "RULE demo-quokka-scheduler-0") {
		t.Fatalf("the owner's rule not whole:\n%s", start)
	}
	writeFact(t, r.clone, fact.Meta{Name: "demo-note-v3", Description: "a third curated note about demo", Type: "design_decision", Scope: "projects/demo"}, "body")
	r.commitIndex(r.clone, "a note since")
	gitIn(t, r.clone, "push", "-q", "origin", "HEAD:main")
	buildIndexForHooks(r.s)
	if out := hookOut(t, payload("t1", "resume", ""), "session-start", "--mcp"); !strings.Contains(out, "third curated note") || strings.Contains(out, "titles") {
		t.Fatalf("resume:\n%s", out)
	}
	turns := []string{`{"type":"response_item","payload":{"type":"message","role":"user"}}`}
	tr := writeTranscript(t, filepath.Join(home, ".codex", "sessions", "2026", "11", "04", "rollout-2026-11-04T10-00-00-t2.jsonl"),
		append(turns, `{"timestamp":"2026-11-04T10:00:00Z","type":"compacted","payload":{"message":`+jsonString(compactSummaryText)+`,"replacement_history":[]}}`)...)
	hookOut(t, payload("t2", "startup", ""), "session-start", "--mcp")
	out := hookOut(t, payload("t2", "compact", tr), "session-start", "--mcp")
	if !strings.Contains(out, "drops stale wombat entries") || strings.Contains(out, "(titles") {
		t.Fatalf("after the compaction:\n%s", out)
	}
	forms := map[string]string{}
	for _, e := range readDeliveryLog(t, platform.StateDir(home)) {
		for _, c := range e.Cands {
			if c.Delivered {
				forms[e.Hook+":"+c.Name] = c.Form
			}
		}
	}
	for k, want := range map[string]string{"start:demo-note": "title", "start:demo-quokka-scheduler-0": "full", "resume:demo-note-v3": "full", "summary:demo-wombat-flusher": "full"} {
		if forms[k] != want {
			t.Errorf("form of %s: %q, want %q (%v)", k, forms[k], want, forms)
		}
	}
}

// A shell command's push is titled, as measured; an edit's keeps whole
// lines. The forms say so.
func TestPushTitlesForCommandsOnly(t *testing.T) {
	_, cwd := repoFirstRig(t,
		fact.Meta{Name: "lint-before-push", Description: "Run the linter with the strict profile", Type: "user_standard", Scope: "projects/widget", AppliesTo: fact.AppliesTo{Commands: []string{"make lint"}}},
		fact.Meta{Name: "worker-no-globals", Description: "Worker state lives in the pool, never in package globals", Type: "design_decision", Scope: "projects/widget", AppliesTo: fact.AppliesTo{Paths: []string{"src/scheduler/**"}}},
	)
	state := filepath.Join(t.TempDir(), "state")
	t.Setenv("TACIT_STATE_DIR", state)
	t.Setenv("TACIT_DELIVERY_TITLES", "on")
	if out := hookOut(t, toolPayload(cwd, "s1", "Bash", map[string]any{"command": "make lint"}), "pre-tool", "--mcp"); strings.Contains(out, "strict profile") || !strings.Contains(out, "lint-before-push: agent note (") || !strings.Contains(out, "titles:") {
		t.Fatalf("command push: %s", out)
	}
	edit := toolPayload(cwd, "s1", "Edit", map[string]any{"file_path": filepath.Join(cwd, "src", "scheduler", "worker.go"), "old_string": "a", "new_string": "b"})
	if out := hookOut(t, edit, "pre-tool", "--mcp"); !strings.Contains(out, "package globals") || strings.Contains(out, "titles:") {
		t.Fatalf("edit push: %s", out)
	}
	forms := map[string]string{}
	for _, e := range readDeliveryLog(t, state) {
		for _, c := range e.Cands {
			if c.Delivered {
				forms[c.Name] = c.Form
			}
		}
	}
	if forms["lint-before-push"] != "title" || forms["worker-no-globals"] != "full" {
		t.Fatalf("forms: %v", forms)
	}
}

// A block of only the owner's own lines has nothing to title: whole lines,
// no note, and the form full.
func TestTitlesLeaveAnOwnersBlockWhole(t *testing.T) {
	t.Setenv("TACIT_DELIVERY_TITLES", "on")
	t.Setenv("TACIT_STATE_DIR", t.TempDir())
	hookMCP = true
	t.Cleanup(func() { dlog, logPending, hookMCP = nil, nil, false })
	rule := search.Hit{Name: "signoff", Path: "user/signoff.md", Type: "user_standard", Ruling: true, Created: "2026-10-01", Description: "Every commit carries a Signed-off-by trailer"}
	beginLog("prompt", hookInput{SessionID: "s1"}, "commit the change", nil, nil)
	e := dlog
	logChose([]search.Hit{rule}, "ranked")
	out := hookBlock([]search.Hit{rule}, "# tacit: facts matching this request", titlesNote)
	endLog([]search.Hit{rule})
	if strings.Contains(out, "titles") || !strings.Contains(out, "Signed-off-by trailer") {
		t.Fatalf("an owner's block: %s", out)
	}
	if c := candOf(*e, "signoff"); c == nil || c.Form != "full" {
		t.Fatalf("form: %+v", c)
	}
}

// A sampled prompt block asks for a self-mark (its probability logged on
// every prompt entry, 0 where no ask could be made); the ask names the
// block, and tacit_mark records the marks, only for a block that asked and
// only names of the store's facts. No ask without the MCP server, for a
// block of only the owner's lines, or at the rate 0.
func TestSelfMarksAreSampledAndLogged(t *testing.T) {
	r, cwd := repoFirstRig(t,
		fact.Meta{Name: "scheduler-shard-locks", Description: "The scheduler pool keeps one shard lock per tenant and a slot lock per job", Type: "design_decision", Scope: "projects/widget"},
	)
	state := filepath.Join(t.TempDir(), "state")
	t.Setenv("TACIT_STATE_DIR", state)
	t.Setenv("TACIT_DELIVERY_REPO_START", "off")
	t.Setenv("TACIT_DELIVERY_PROMPT_ACTION", "off")
	t.Setenv("TACIT_DELIVERY_MARK_RATE", "0.05")
	draw := markDraw
	t.Cleanup(func() { markDraw, hookMCP = draw, false })
	prompt := "Fix the scheduler pool shard lock and slot lock race"
	markDraw = func() float64 { return 0.99 }
	if out := hookOut(t, promptPayload(cwd, "s1", prompt), "prompt", "--mcp"); strings.Contains(out, "tacit_mark") {
		t.Fatalf("not drawn: %s", out)
	}
	markDraw = func() float64 { return 0.01 }
	out := hookOut(t, promptPayload(cwd, "s2", prompt), "prompt", "--mcp")
	if !strings.Contains(out, "`tacit_mark` (block ") {
		t.Fatalf("drawn: %s", out)
	}
	if out := hookOut(t, promptPayload(cwd, "s3", prompt), "prompt"); strings.Contains(out, "tacit_mark") {
		t.Fatalf("no MCP server: %s", out)
	}
	markDraw = func() float64 { return 0 }
	t.Setenv("TACIT_DELIVERY_MARK_RATE", "0")
	if out := hookOut(t, promptPayload(cwd, "s4", prompt), "prompt", "--mcp"); strings.Contains(out, "tacit_mark") {
		t.Fatalf("rate 0: %s", out)
	}
	t.Setenv("TACIT_DELIVERY_MARK_RATE", "0.05")
	pm := map[string]float64{}
	var asked logEntry
	for _, e := range readDeliveryLog(t, state) {
		if e.Hook != "prompt" {
			continue
		}
		if e.PMark == nil {
			t.Fatalf("a prompt entry without p_mark: %+v", e)
		}
		pm[e.Session] = *e.PMark
		if e.Session == "s2" {
			asked = e
		}
		if e.Session != "s2" && e.MarkAsked {
			t.Fatalf("asked unasked: %+v", e)
		}
	}
	if pm["s1"] != markRate || pm["s2"] != markRate || pm["s3"] != 0 || pm["s4"] != 0 || len(pm) != 4 {
		t.Fatalf("p_mark: %v", pm)
	}
	if !asked.MarkAsked || !strings.Contains(out, asked.ID) {
		t.Fatalf("asked: %+v", asked)
	}
	// A block of only the owner's lines asks nothing.
	beginLog("prompt", hookInput{SessionID: "s5"}, "commit", nil, nil)
	hookMCP = true
	if ask := markAsk([]search.Hit{{Name: "signoff", Path: "user/signoff.md", Ruling: true}}); ask != "" || *dlog.PMark != 0 {
		t.Fatalf("owner only: %q", ask)
	}
	endLog(nil)
	hookMCP = false
	// The tool: a block that asked, names the store holds; else an error.
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := storeServer(r.s, "claude").Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	call := func(args map[string]any) *mcp.CallToolResult {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "tacit_mark", Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := call(map[string]any{"block": asked.ID, "helped": []string{"scheduler-shard-locks", "no-such-fact", "Not A Slug"}}); res.IsError {
		t.Fatalf("tacit_mark: %+v", res)
	}
	var s1 logEntry
	for _, e := range readDeliveryLog(t, state) {
		if e.Session == "s1" && e.Hook == "prompt" {
			s1 = e
		}
	}
	if res := call(map[string]any{"block": s1.ID, "helped": []string{"scheduler-shard-locks"}}); !res.IsError {
		t.Fatal("a mark for a block that did not ask was taken")
	}
	if res := call(map[string]any{"block": asked.ID, "helped": []string{"scheduler-shard-locks"}, "not_helped": []string{"scheduler-shard-locks"}}); !res.IsError {
		t.Fatal("a mark with no name left was taken")
	}
	if res := call(map[string]any{"block": "not-an-id"}); !res.IsError {
		t.Fatal("a bad id was taken")
	}
	var marks []*logMarkStep
	for _, e := range readDeliveryLog(t, state) {
		if e.Hook == "mark" {
			marks = append(marks, e.Mark)
		}
	}
	if len(marks) != 1 || marks[0].Block != asked.ID || strings.Join(marks[0].Helped, ",") != "scheduler-shard-locks" {
		t.Fatalf("marks: %+v", marks)
	}
}
