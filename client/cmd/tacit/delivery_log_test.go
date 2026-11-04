package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/fact"
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
