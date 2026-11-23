package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/search"
)

func stopPayload(cwd, session string, active bool) string {
	b, _ := json.Marshal(map[string]any{"session_id": session, "cwd": cwd, "hook_event_name": "Stop", "stop_hook_active": active})
	return string(b)
}

// stopReason is the Stop hook's block reason, "" when it let the stop go.
func stopReason(t *testing.T, out string) string {
	t.Helper()
	if strings.TrimSpace(out) == "" {
		return ""
	}
	var v struct{ Decision, Reason string }
	if err := json.Unmarshal([]byte(out), &v); err != nil || v.Decision != "block" {
		t.Fatalf("not a Stop block: %v %q", err, out)
	}
	return v.Reason
}

var askID = func(reason string) string {
	i := strings.Index(reason, `block "`)
	if i < 0 {
		return ""
	}
	return reason[i+7 : i+7+16]
}

// A turn given facts cannot end silently: its first Stop is blocked once
// with the facts' names and the one call that marks them; the second Stop
// of the turn (stop_hook_active) and a turn given nothing go through. One
// tacit_mark call takes the list; the verdicts land in the log, in the
// format a mark always had (helped, not_helped) plus wrong, and in the
// outcomes under the session's repository.
func TestStopAsksOnceForTheTurnsMarks(t *testing.T) {
	r, cwd := repoFirstRig(t,
		fact.Meta{Name: "scheduler-shard-locks", Description: "The scheduler pool keeps one shard lock per tenant and a slot lock per job", Type: "design_decision", Scope: "projects/widget", AppliesTo: widgetRepo},
		fact.Meta{Name: "slot-lock-order", Description: "Take the shard lock before the slot lock in the scheduler pool, never the other way", Type: "past_mistake", Scope: "projects/widget", AppliesTo: widgetRepo},
	)
	state := filepath.Join(t.TempDir(), "state")
	t.Setenv("TACIT_STATE_DIR", state)
	if out := hookOut(t, stopPayload(cwd, "s1", false), "stop", "--mcp"); out != "" {
		t.Fatalf("nothing given, nothing asked: %q", out)
	}
	start, _ := json.Marshal(map[string]any{"session_id": "s1", "cwd": cwd, "source": "startup"})
	if out := hookOut(t, string(start), "session-start", "--mcp"); !strings.Contains(out, "slot-lock-order") {
		t.Fatalf("start: %s", out)
	}
	reason := stopReason(t, hookOut(t, stopPayload(cwd, "s1", false), "stop", "--mcp"))
	if !strings.HasPrefix(reason, stopAskTitle) || !strings.Contains(reason, "scheduler-shard-locks") || !strings.Contains(reason, "slot-lock-order") ||
		!strings.Contains(reason, "mcp__tacit__tacit_mark") || !strings.Contains(reason, "not_relevant") {
		t.Fatalf("ask: %q", reason)
	}
	id := askID(reason)
	if !logIDRx.MatchString(id) {
		t.Fatalf("ask id: %q in %q", id, reason)
	}
	if out := hookOut(t, stopPayload(cwd, "s1", true), "stop", "--mcp"); out != "" {
		t.Fatalf("the turn's second stop: %q", out)
	}
	if out := hookOut(t, stopPayload(cwd, "s1", false), "stop", "--mcp"); out != "" {
		t.Fatalf("asked again for the same lines: %q", out)
	}
	// A resume's block is owed under the repository too (the restart paths
	// log before they remember).
	beginLog("resume", hookInput{SessionID: "s9"}, "", []string{"widget"}, nil)
	endLog(nil)
	owe("s9", []string{"slot-lock-order"})
	if b, _ := os.ReadFile(owedFile("s9")); string(b) != "slot-lock-order\twidget\n" {
		t.Fatalf("owed after its entry ended: %q", b)
	}
	os.Remove(owedFile("s1"))
	// A headless session's stop asks nothing and drops what it owes.
	os.WriteFile(owedFile("h1"), []byte("slot-lock-order\twidget\n"), 0o600)
	t.Setenv("TACIT_HEADLESS", "1")
	if out := hookOut(t, stopPayload(cwd, "h1", false), "stop", "--mcp"); out != "" {
		t.Fatalf("a headless stop: %q", out)
	}
	t.Setenv("TACIT_HEADLESS", "")
	if _, err := os.Stat(owedFile("h1")); err == nil {
		t.Fatal("a headless session kept its owed lines")
	}
	// Codex sends the reason as the next prompt: it is no request.
	if out := hookOut(t, promptPayload(cwd, "s1", reason), "prompt", "--mcp"); out != "" {
		t.Fatalf("the ask as a prompt: %q", out)
	}
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
	call := func(args map[string]any) (string, bool) {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "tacit_mark", Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		return res.Content[0].(*mcp.TextContent).Text, res.IsError
	}
	if msg, isErr := call(map[string]any{"block": id, "marks": []map[string]string{{"name": "no-such-fact", "verdict": "used"}}}); !isErr || !strings.Contains(msg, "name at least one") {
		t.Fatalf("a name the ask did not list: %q", msg)
	}
	msg, isErr := call(map[string]any{"block": id, "marks": []map[string]string{
		{"name": "scheduler-shard-locks", "verdict": "used"}, {"name": "slot-lock-order", "verdict": "wrong"}, {"name": "other", "verdict": "used"}}})
	if isErr || !strings.Contains(msg, "noted") || !strings.Contains(msg, "other") {
		t.Fatalf("mark: %q", msg)
	}
	if msg, isErr := call(map[string]any{"block": id, "marks": []map[string]string{{"name": "slot-lock-order", "verdict": "used"}}}); !isErr || !strings.Contains(msg, "marked already") {
		t.Fatalf("a second mark: %q", msg)
	}
	var mark *logMarkStep
	for _, e := range readDeliveryLog(t, state) {
		if e.Hook == "mark" {
			mark = e.Mark
		}
	}
	if mark == nil || mark.Block != id || strings.Join(mark.Helped, ",") != "scheduler-shard-locks" || strings.Join(mark.Wrong, ",") != "slot-lock-order" || len(mark.NotHelped) != 0 {
		t.Fatalf("logged mark: %+v", mark)
	}
	o := loadOutcomes(time.Now())["widget"]
	if o["scheduler-shard-locks"] != (outcomeTally{Used: 1}) || o["slot-lock-order"] != (outcomeTally{Bad: 1, Wrong: 1}) {
		t.Fatalf("outcomes: %+v", o)
	}
}

// `tacit mark` is the same call from a shell: <name>=<verdict>.
func TestMarkCommand(t *testing.T) {
	state := t.TempDir()
	t.Setenv("TACIT_STATE_DIR", state)
	ask := &logEntry{V: 1, ID: "00000000000000ab", Hook: "stop", MarkAsked: true, Cands: []logCand{{Name: "owner-rule", Delivered: true, Prov: "rule", In: "widget"}, {Name: "a-note", Delivered: true, In: "widget"}}}
	writeLogEntry(ask, time.Now())
	if code := runMark([]string{ask.ID, "owner-rule=not relevant", "a-note=used"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	o := loadOutcomes(time.Now())["widget"]
	if o["owner-rule"] != (outcomeTally{Bad: 1}) || o["a-note"] != (outcomeTally{Used: 1}) {
		t.Fatalf("outcomes: %+v", o)
	}
	if code := runMark([]string{ask.ID, "a-note"}); code != 2 {
		t.Fatalf("a name with no verdict: exit %d", code)
	}
}

// A headless session gets no start or prompt block (and so owes no mark);
// the MCP tools stay. Kinds: TACIT_HEADLESS, a subagent's agent_id, an
// SDK entrypoint, a Codex exec run, a Keel subagent's brief, a transcript
// under the cache directory. A person's Claude Code and Codex sessions
// are not headless, nor is a session whose transcript has no prompt yet.
func TestHeadlessSessionsGetNoBlocks(t *testing.T) {
	_, cwd := repoFirstRig(t,
		fact.Meta{Name: "slot-lock-order", Description: "Take the shard lock before the slot lock in the scheduler pool, never the other way", Type: "past_mistake", Scope: "projects/widget", AppliesTo: widgetRepo},
	)
	t.Setenv("TACIT_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	os.MkdirAll(filepath.Join(cache, "run"), 0o700)
	scratch := filepath.Join(cache, "run", "t.jsonl")
	os.WriteFile(scratch, []byte(`{"type":"user","message":{"role":"user","content":"hi"}}`+"\n"), 0o600)
	kinds := []struct {
		name, transcript, agent, want string
	}{
		{"cli", write("cli.jsonl", `{"type":"user","isSidechain":false,"entrypoint":"cli","message":{"role":"user","content":"fix the slot lock"}}`+"\n"), "", ""},
		{"sdk", write("sdk.jsonl", `{"type":"user","isSidechain":false,"entrypoint":"sdk-cli","message":{"role":"user","content":"fix"}}`+"\n"), "", "sdk"},
		{"sub", write("sub.jsonl", `{"type":"user","entrypoint":"cli","message":{"role":"user","content":"fix"}}`+"\n"), "agent-1", "subagent"},
		{"side", write("side.jsonl", `{"type":"user","isSidechain":true,"message":{"role":"user","content":"fix"}}`+"\n"), "", "subagent"},
		{"codex", write("rollout-a.jsonl", `{"type":"session_meta","payload":{"originator":"codex-tui","source":"cli"}}`+"\n"+`{"type":"response_item","payload":{"role":"user"}}`+"\n"), "", ""},
		{"exec", write("rollout-b.jsonl", `{"type":"session_meta","payload":{"originator":"codex_exec","source":"exec"}}`+"\n"), "", "exec"},
		{"keel", write("keel.jsonl", `{"type":"user","isSidechain":false,"message":{"role":"user","content":"`+keelSubagentBrief+`. Work on the task below."}}`+"\n"), "", "subagent"},
		{"scratch", scratch, "", "scratch"},
	}
	for _, k := range kinds {
		in := hookInput{SessionID: "kind-" + k.name, TranscriptPath: k.transcript, AgentID: k.agent}
		if got := sessionKind(in); got != k.want {
			t.Errorf("%s: kind %q, want %q", k.name, got, k.want)
		}
		p, _ := json.Marshal(map[string]any{"session_id": "p-" + k.name, "cwd": cwd, "prompt": "Fix the shard lock and slot lock order in the scheduler pool", "transcript_path": k.transcript, "agent_id": k.agent})
		out := hookOut(t, string(p), "prompt", "--mcp")
		if (out == "") != (k.want != "") {
			t.Errorf("%s: prompt block %q", k.name, out)
		}
	}
	t.Setenv("TACIT_HEADLESS", "1")
	if sessionKind(hookInput{SessionID: "env"}) != "env" {
		t.Error("TACIT_HEADLESS=1")
	}
	t.Setenv("TACIT_HEADLESS", "")
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "sdk-cli")
	if sessionKind(hookInput{SessionID: "noscript"}) != "sdk" {
		t.Error("an SDK run with no transcript")
	}
	for _, ep := range []string{"cli", "sdk-ts"} {
		t.Setenv("CLAUDE_CODE_ENTRYPOINT", ep)
		if sessionKind(hookInput{SessionID: "noscript-" + ep}) != "" {
			t.Errorf("%s with no transcript: headless", ep)
		}
	}
}

// Outcomes move delivery in their scope: a line with more bad marks than
// used ones is demoted, one with outcomeStop bad and none used is stopped,
// one with more used is promoted; another scope is untouched.
func TestOutcomesDemoteStopAndPromote(t *testing.T) {
	t.Setenv("TACIT_STATE_DIR", t.TempDir())
	now := time.Now().UTC().Format(time.RFC3339Nano)
	var lines []outcomeLine
	add := func(in, f, v string, n int) {
		for i := 0; i < n; i++ {
			lines = append(lines, outcomeLine{TS: now, In: in, Fact: f, Verdict: v, Src: "mark"})
		}
	}
	add("widget", "noisy", verdictNotRelevant, 3)
	add("widget", "mixed", verdictWrong, 1)
	add("widget", "good", verdictUsed, 2)
	add("widget", "saved", verdictNotRelevant, 3)
	add("widget", "saved", verdictUsed, 1)
	add("gadget", "good", verdictNotRelevant, 3)
	old := outcomeLine{TS: time.Now().Add(-outcomeWindow - time.Hour).UTC().Format(time.RFC3339Nano), In: "widget", Fact: "good", Verdict: verdictNotRelevant, Src: "judge"}
	lines = append(lines, old, old, old, old)
	recordOutcomes(lines)
	o := outcomesFor("widget")
	for name, want := range map[string]int{"noisy": outcomeStopped, "mixed": outcomeDemoted, "good": outcomePromoted, "saved": outcomeDemoted, "fresh": outcomeNeutral} {
		if got := o.class(name); got != want {
			t.Errorf("%s: class %d, want %d", name, got, want)
		}
	}
	if outcomesFor("gadget").class("good") != outcomeStopped {
		t.Error("another scope's verdicts")
	}
	// The owner's own lines are never moved.
	if o.classOf(search.Hit{Name: "noisy", Ruling: true}) != outcomeNeutral || o.classOf(search.Hit{Name: "noisy"}) != outcomeStopped {
		t.Error("an owner's line was moved, or an agent's was not")
	}
	hits := []search.Hit{{Name: "noisy", Score: 30}, {Name: "mixed", Score: 28}, {Name: "fresh", Score: 20}, {Name: "good", Score: 16}}
	var names []string
	for _, h := range o.rescored(hits) {
		names = append(names, h.Name)
	}
	if strings.Join(names, ",") != "good,fresh,mixed" {
		t.Fatalf("rescored: %v", names)
	}
	if hits[1].Score != 28 {
		t.Fatal("rescored changed the caller's hits")
	}
	// A new verdict is read: the summary is rebuilt when the file grows.
	recordOutcomes([]outcomeLine{{TS: now, In: "widget", Fact: "fresh", Verdict: verdictUsed, Src: "judge"}})
	if outcomesFor("widget").class("fresh") != outcomePromoted {
		t.Fatal("a verdict after the summary")
	}
}
