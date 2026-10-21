package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/record"
)

// writeRecordFile writes a folder's record into a store directory.
func writeRecordFile(t *testing.T, root string, r *record.Record) {
	t.Helper()
	if r.Created == "" {
		r.Created, r.CreatedBy = "2026-10-20", record.ByOwner
	}
	if len(r.Was) == 0 {
		r.Was = []record.Entry{{Status: r.Status, Date: r.Created, By: record.ByOwner, Why: "made in the test"}}
	}
	b, err := r.Encode()
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, filepath.FromSlash(record.Rel(r.Folder)))
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func preToolContext(t *testing.T, out string) string {
	t.Helper()
	if strings.TrimSpace(out) == "" {
		return ""
	}
	var v struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil || v.HookSpecificOutput.HookEventName != "PreToolUse" {
		t.Fatalf("not PreToolUse JSON: %v %q", err, out)
	}
	return v.HookSpecificOutput.AdditionalContext
}

// areaRig is a store with an active area for a ticketing tool family, a
// dormant area for another, a shipped area, an active topic with an alias
// and a dead project folder.
func areaRig(t *testing.T) (r *rig, cwd, outside string) {
	t.Helper()
	r, cwd = repoFirstRig(t,
		fact.Meta{Name: "tickets-cite-the-cr", Description: "Every ticket comment cites the code review it rests on", Type: "user_standard", Scope: "areas/ticketing"},
		fact.Meta{Name: "tickets-sev2-pages", Description: "A sev2 ticket pages the on-call within five minutes", Type: "design_decision", Scope: "areas/ticketing"},
		fact.Meta{Name: "tickets-resolver-mistake", Description: "Resolving with the wrong resolver group bounced a ticket for a day", Type: "past_mistake", Scope: "areas/ticketing"},
		fact.Meta{Name: "tickets-tool-history", Description: "The ticketing tool moved to a new API in 2025", Type: "history", Scope: "areas/ticketing"},
		fact.Meta{Name: "tickets-cli-dry-run", Description: "Run the ticket CLI with --dry-run first", Type: "user_standard", Scope: "areas/ticketing", AppliesTo: fact.AppliesTo{Commands: []string{"tix update"}}},
		fact.Meta{Name: "tickets-q4-cutover", Description: "The zebra cutover moves every queue on one weekend", Type: "design_decision", Scope: "areas/ticketing/topics/zebra-cutover"},
		fact.Meta{Name: "wiki-sections", Description: "Wiki pages keep one section per service", Type: "user_standard", Scope: "areas/wiki"},
		fact.Meta{Name: "pager-old-rotation", Description: "The old pager rotation is weekly", Type: "user_standard", Scope: "areas/pager"},
		fact.Meta{Name: "widget-dead-standard", Description: "Widget commits are squashed before merge", Type: "user_standard", Scope: "projects/widget"},
	)
	for _, rec := range []*record.Record{
		{Folder: "areas/ticketing", Kind: record.KindArea, Name: "ticketing", Description: "the ticketing system", Status: record.StatusActive, Tools: []string{"mcp__*__Ticketing*"}, Commands: []string{"tix"}},
		{Folder: "areas/wiki", Kind: record.KindArea, Name: "wiki", Description: "the wiki", Status: record.StatusDormant, Tools: []string{"mcp__*__Wiki*"}},
		{Folder: "areas/pager", Kind: record.KindArea, Name: "pager", Description: "the pager", Status: record.StatusShipped, Tools: []string{"mcp__*__Pager*"}},
		{Folder: "areas/ticketing/topics/zebra-cutover", Kind: record.KindTopic, Name: "zebra-cutover", Description: "the cutover", Status: record.StatusActive, Aliases: []string{"queue-move"}},
		{Folder: "projects/widget", Kind: record.KindRepo, Name: "widget", Description: "the widget", Status: record.StatusDead},
	} {
		writeRecordFile(t, r.clone, rec)
	}
	r.commitIndex(r.clone, "records")
	outside = filepath.Join(r.root, "home", "notes")
	os.MkdirAll(outside, 0o755)
	return r, cwd, outside
}

// An area's tool pushes its rules, standards, decisions and past mistakes
// before the call, outside any repository, once per session; a dormant or
// shipped area's tool pushes nothing; the area's topics are not pushed.
func TestPreToolPushesAnAreaOutsideARepoOncePerSession(t *testing.T) {
	_, _, outside := areaRig(t)
	call := func(session, tool string, input map[string]any) string {
		return preToolContext(t, hookOut(t, toolPayload(outside, session, tool, input), "pre-tool"))
	}
	got := call("a1", "mcp__builder__TicketingReadActions", map[string]any{"path": "/x/y.md", "command": "make lint"})
	// The two standards, then the first decision or past mistake.
	for _, want := range []string{"tickets-cite-the-cr", "tickets-cli-dry-run", "tickets-resolver-mistake", "tacit_tree areas/ticketing"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %s:\n%s", want, got)
		}
	}
	for _, not := range []string{"tickets-tool-history", "tickets-q4-cutover", "wiki-sections"} {
		if strings.Contains(got, not) {
			t.Fatalf("%s pushed:\n%s", not, got)
		}
	}
	if strings.Count(got, "\n- ") != areaCeiling {
		t.Fatalf("not %d lines:\n%s", areaCeiling, got)
	}
	if again := call("a1", "mcp__builder__TicketingWriteActions", nil); again != "" {
		t.Fatalf("pushed twice in a session:\n%s", again)
	}
	if got := call("a2", "mcp__builder__WikiRead", nil); got != "" {
		t.Fatalf("a dormant area pushed:\n%s", got)
	}
	if got := call("a2", "mcp__builder__PagerSend", nil); got != "" {
		t.Fatalf("a shipped area pushed:\n%s", got)
	}
	if got := call("a2", "mcp__other__Search", nil); got != "" {
		t.Fatalf("an unmatched tool pushed:\n%s", got)
	}
	// The area's command: the fact whose own command the call runs first.
	got = call("a3", "Bash", map[string]any{"command": "tix update 42 --status done"})
	if !strings.HasPrefix(strings.SplitN(got, "\n", 3)[1], "- tickets-cli-dry-run") {
		t.Fatalf("the command's own fact is not first:\n%s", got)
	}
	t.Setenv("TACIT_DELIVERY_AREA_PUSH", "off")
	if got := call("a4", "mcp__builder__TicketingReadActions", nil); got != "" {
		t.Fatalf("setting off:\n%s", got)
	}
}

// In a repository an edit's own match still comes first, and the area's
// facts follow when the same call is an area's.
func TestPreToolRepoMatchRanksAboveTheArea(t *testing.T) {
	r, cwd, _ := areaRig(t)
	writeFact(t, r.clone, fact.Meta{Name: "gadget-tix-rule", Description: "In gadget run tix with the gadget queue", Type: "user_standard", Scope: "projects/gadget", AppliesTo: fact.AppliesTo{Commands: []string{"tix"}}}, "x\n")
	writeRecordFile(t, r.clone, &record.Record{Folder: "projects/gadget", Kind: record.KindRepo, Name: "gadget", Description: "gadget", Status: record.StatusActive})
	r.commitIndex(r.clone, "gadget")
	gadget := r.checkout("gadget")
	got := preToolContext(t, hookOut(t, toolPayload(gadget, "b1", "Bash", map[string]any{"command": "tix update 7"}), "pre-tool"))
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) < 3 || !strings.HasPrefix(lines[1], "- gadget-tix-rule") || !strings.Contains(got, "tickets-cli-dry-run") {
		t.Fatalf("order:\n%s", got)
	}
	_ = cwd
}

// The prompt hook adds a topic's facts when the prompt names the topic or
// an alias, by stem; another prompt does not reach them.
func TestPromptReachesANamedTopic(t *testing.T) {
	_, _, outside := areaRig(t)
	named := hookOut(t, promptPayload(outside, "p1", "what is the plan for the zebra cutover of every queue weekend"), "prompt")
	if !strings.Contains(named, "tickets-q4-cutover") {
		t.Fatalf("the named topic's fact is missing:\n%s", named)
	}
	alias := hookOut(t, promptPayload(outside, "p2", "how does the queue move go on that weekend for every queue"), "prompt")
	if !strings.Contains(alias, "tickets-q4-cutover") {
		t.Fatalf("the alias did not reach the topic:\n%s", alias)
	}
	other := hookOut(t, promptPayload(outside, "p3", "zebra weekend plans for every queue"), "prompt")
	if strings.Contains(other, "tickets-q4-cutover") {
		t.Fatalf("an unnamed topic was searched:\n%s", other)
	}
	t.Setenv("TACIT_DELIVERY_TOPICS", "off")
	if off := hookOut(t, promptPayload(outside, "p4", "what is the plan for the zebra cutover of every queue weekend"), "prompt"); strings.Contains(off, "tickets-q4-cutover") {
		t.Fatalf("setting off:\n%s", off)
	}
}

// A dead project folder is quiet: its facts leave the start block.
func TestSessionStartLeavesADeadFolderQuiet(t *testing.T) {
	_, cwd, _ := areaRig(t)
	if out := hookOut(t, startPayload(cwd, "q1"), "session-start"); strings.Contains(out, "widget-dead-standard") {
		t.Fatalf("a dead folder's fact at session start:\n%s", out)
	}
}

// A session in a checkout whose folder is suppressed into another reads
// the winner's facts at session start, by the records in the index.
func TestSessionStartFollowsASuppressedRepo(t *testing.T) {
	r, _ := repoFirstRig(t,
		fact.Meta{Name: "gizmo-signoff", Description: "Every gizmo commit is signed off", Type: "user_standard", Scope: "projects/gizmo"},
	)
	writeRecordFile(t, r.clone, &record.Record{Folder: "projects/gizmo", Kind: record.KindRepo, Name: "gizmo", Description: "gizmo", Status: record.StatusActive})
	writeRecordFile(t, r.clone, &record.Record{Folder: "projects/widget", Kind: record.KindRepo, Name: "widget", Description: "old name", Status: record.StatusActive, SuppressedBy: "projects/gizmo"})
	r.commitIndex(r.clone, "records")
	cwd := filepath.Join(r.projects, "widget")
	if out := hookOut(t, startPayload(cwd, "s1"), "session-start"); !strings.Contains(out, "gizmo-signoff") {
		t.Fatalf("the winner's fact is missing:\n%s", out)
	}
}

// The PreToolUse hook on MCP calls: its latency over many calls, logged
// for eval/VALUE.md; the hook keeps its budget.
func TestPreToolMCPLatency(t *testing.T) {
	_, _, outside := areaRig(t)
	var d []time.Duration
	for i := 0; i < 60; i++ {
		tool := "mcp__other__Search"
		if i%3 == 0 {
			tool = "mcp__builder__TicketingReadActions"
		}
		start := time.Now()
		hookOut(t, toolPayload(outside, "lat"+string(rune('a'+i%26)), tool, map[string]any{"q": "x"}), "pre-tool")
		d = append(d, time.Since(start))
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	p50, p95 := d[len(d)/2], d[len(d)*95/100]
	t.Logf("pre-tool on MCP calls, in-process: p50 %v p95 %v over %d calls", p50, p95, len(d))
	if p95 > toolBudget {
		t.Fatalf("p95 %v over the budget %v", p95, toolBudget)
	}
}
