package main

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
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
	if first.V != deliveryLogVersion || first.ID == "" || first.Session != "s1" || strings.Join(first.Terms, " ") != "fix scheduler pool shard lock slot race" || first.Floor != promptFloor || first.Relative != promptRelative ||
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
	if p := candOf(pushes[0], "lint-before-push"); p == nil || !p.Delivered || p.Via != "meta" || p.P != 1 || pushes[0].Tool != "Bash" || strings.Join(pushes[0].Terms, " ") != "make lint" || pushes[0].Nothing {
		t.Fatalf("the push: %+v", pushes[0])
	}
	if p := pushes[1]; p.Tool != "Bash" || len(p.Terms) != 0 || !p.Nothing || p.PNothing != 1 || len(p.Cands) != 0 {
		t.Fatalf("the push that showed nothing: %+v", p)
	}
	want := []logNav{
		{Tool: "fact", Names: []string{"scheduler-shard-locks"}, Levels: -1},
		{Tool: "tree", Path: "projects/widget", Depth: 2, Levels: 0},
		{Tool: "search", Path: "projects/widget", Terms: []string{"lock", "order"}, Depth: 2, Levels: -1},
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
	beginLog("prompt", hookInput{SessionID: "s1"}, prompt, nil)
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
		"tacit search --repo widget --limit 5 --offset 10 lock order":     `[{"tool":"search","terms":["lock","order"],"depth":0,"levels":-1,"offset":10}]`,
		"FOO=1 tacit search -scope=projects/widget -json lock":            `[{"tool":"search","path":"projects/widget","terms":["lock"],"depth":2,"levels":-1}]`,
		"tacit tree --depth=1 projects/widget":                            `[{"tool":"tree","path":"projects/widget","depth":2,"levels":1}]`,
		`C:\tools\tacit.exe fact pool-lock-order`:                         `[{"tool":"fact","names":["pool-lock-order"],"depth":0,"levels":-1}]`,
		`git commit -m "use tacit fact to read one" && echo tacit tree x`: `null`,
		"tacit search lock order --scope projects/widget":                 `[{"tool":"search","terms":["lock","order","scope","projects","widget"],"depth":0,"levels":-1}]`,
		"tacit search -- --scope":                                         `[{"tool":"search","terms":["scope"],"depth":0,"levels":-1}]`,
		"tacit fact one two; tacit fact Not_A_Slug; tacit tree a b":       `null`,
	} {
		b, _ := json.Marshal(navOf(hookInput{ToolName: "Bash"}, cmd))
		if string(b) != want {
			t.Errorf("%s:\n got %s\nwant %s", cmd, b, want)
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
	score := func(v float64) *float64 { return &v }
	status := "ok"
	utilitySelectFn = func(ctx context.Context, p string, hits, derived []search.Hit, seen map[string]bool, floor float64) ([]search.Hit, utilityAudit) {
		if status != "ok" {
			return promptSelectionFor(p, hits, derived, seen, floor), utilityAudit{Status: status}
		}
		return []search.Hit{useful}, utilityAudit{Status: "ok", Threshold: 0.7, QueryHash: "abc", Candidates: []utilityAuditHit{
			{ID: useful.Path, Score: score(0.95)}, {ID: topical.Path, Score: score(0.1)}}}
	}
	run := func() *logEntry {
		t.Helper()
		beginLog("prompt", hookInput{SessionID: "s1"}, "endpoint api release", nil)
		e := dlog
		dlog.pool([]search.Hit{useful, topical}, nil)
		endLog(utilityLogged(context.Background(), "endpoint api release", []search.Hit{useful, topical}, nil, nil, promptFloor))
		return e
	}
	e := run()
	u, tp := candOf(*e, "endpoint-useful"), candOf(*e, "endpoint-topical")
	if e.Policy != "utility" || e.UtilityThreshold != 0.7 || e.UtilityQuery != "abc" || u == nil || u.Via != "utility" || !u.Delivered || u.Utility == nil || *u.Utility != 0.95 ||
		tp == nil || tp.Delivered || tp.Via != "" || tp.Dropped != "utility" || tp.Utility == nil || *tp.Utility != 0.1 {
		t.Fatalf("the provider chose: %+v, %+v, %+v", e, u, tp)
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

// The log keeps a request's terms, never its text, and leaves out every
// secret before it takes them: by shape (the server's key shapes, provider
// prefixes with a key's tail, long hex runs, random-looking runs) and by
// place (the value after a password, token or key name, a URL's user and
// password, a password glued onto -p). Paths, identifiers and names that
// only start like a key stay.
func TestDeliveryLogTermsLeaveOutSecrets(t *testing.T) {
	for _, s := range []string{
		"sk-Ab3dEfGh1jKlMnOpQrStUvWx", "ghp_0123456789abcdefABCDEF0123456789abcd", "AKIAABCDEFGHIJKLMNOP",
		"xoxb-123456789012-abcdefABCDEF", "0123456789abcdef0123456789abcdef01234567",
		"token=Zm9vYmFyYmF6cXV4MTIzNDU2Nzg5MEFCQ0RFRg", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2ln",
		"Q8vX2mZp7LkR4tNw9YbH3sJd", "--password=hunter2", "PGPASSWORD=hunter2", "-pS3cretPass",
		"--api-key Kq2Jd8Lw", "password is Tr0ub4dor&3", "Authorization: Bearer abcdef123456",
		"https://alice:Sup3rS3cret@example.com/x",
	} {
		got := strings.Join(logTerms("deploy the widget with "+s+" tonight"), " ")
		for _, leak := range []string{"hunter2", "s3cretpass", "kq2jd8lw", "tr0ub4dor", "abcdef123456", "sup3rs3cret", "alice", "ab3d", "zm9v", "q8vx", "akia", "0123456789abcdef"} {
			if strings.Contains(got, leak) {
				t.Errorf("%s: terms %q hold %q", s, got, leak)
			}
		}
		if !strings.HasPrefix(got, "deploy widget") || !strings.HasSuffix(got, "tonight") {
			t.Errorf("%s: terms %q lost the words around it", s, got)
		}
	}
	for text, want := range map[string]string{
		"edit client/cmd/tacit/hooks_pretool.go and run make lint":   "edit client cmd tacit hooks_pretool run make lint",
		"why did the otter retry loop stop":                          "otter retry loop stop",
		"open src/components/UserProfile2.tsx":                       "open src components userprofile2 tsx",
		"read pkg/apis/v1beta1/DeploymentSpec.go":                    "read pkg apis v1beta1 deploymentspec",
		"rename configureHTTPClientWithRetries":                      "rename configurehttpclientwithretries",
		"call hf_hub_download with npm_config_cache in pypi-publish": "call hf_hub_download npm_config_cache pypi publish",
		"find . -print and fix the token refresh":                    "find print fix token refresh",
	} {
		if got := strings.Join(logTerms(text), " "); got != want {
			t.Errorf("%q: terms %q, want %q", text, got, want)
		}
	}
	// The entry holds no text field, and a nav step's folder only when it is
	// shaped as one.
	b, _ := json.Marshal(logEntry{V: 1, Terms: logTerms("fix it with sk-Ab3dEfGh1jKlMnOpQrStUvWx now")})
	if strings.Contains(string(b), "Ab3d") || strings.Contains(string(b), "request") {
		t.Errorf("entry: %s", b)
	}
	for in, want := range map[string]string{"projects/widget": "projects/widget", "/user/": "user", "free text here": "", "projects/Q8vX2mZp7LkR4tNw9YbH3sJd": ""} {
		if got := scopePath(in); got != want {
			t.Errorf("scopePath(%q) = %q, want %q", in, got, want)
		}
	}
	if n := navOf(hookInput{ToolName: "mcp__tacit__tacit_fact", ToolInput: json.RawMessage(`{"name":"not a slug"}`)}, ""); len(n) != 0 {
		t.Errorf("a fact name that is no slug: %+v", n)
	}
}
