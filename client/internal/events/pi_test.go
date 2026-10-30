package events

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/agent-sh/tacit/client/internal/digest"
)

// piMsg is a pi message entry at second s past 08:00.
func piMsg(s int, id string, message map[string]any) string {
	b, _ := json.Marshal(map[string]any{"type": "message", "id": id, "parentId": nil, "timestamp": fmt.Sprintf("2026-10-22T08:%02d:%02d.000Z", s/60, s%60), "message": message})
	return string(b)
}

func piEntry(s int, fields string) string {
	return fmt.Sprintf(`{%s,"parentId":null,"timestamp":"2026-10-22T08:%02d:%02d.000Z"}`, fields, s/60, s%60)
}

func piText(text string) []map[string]any {
	return []map[string]any{{"type": "text", "text": text}}
}

func TestPiSessionEvents(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".pi", "agent", "sessions", "--home-u-projects-shop--")
	lines := []string{
		/* 0 */ `{"type":"session","version":3,"id":"01pi","timestamp":"2026-10-22T08:00:00.000Z","cwd":"/home/u/projects/shop"}`,
		/* 1 */ piEntry(0, `"type":"model_change","id":"m1","provider":"p","modelId":"qwen3-coder"`),
		/* 2 */ piEntry(0, `"type":"thinking_level_change","id":"m2","thinkingLevel":"high"`),
		/* 3 */ piMsg(5, "u3", map[string]any{"role": "user", "content": piText("keep the cache warm before a release"), "timestamp": 1}),
		/* 4 */ piMsg(8, "a4", map[string]any{"role": "assistant", "model": "qwen3-coder", "stopReason": "toolUse", "content": []map[string]any{{"type": "thinking", "thinking": "hm"}, {"type": "text", "text": "Looking."}, {"type": "toolCall", "id": "call_1", "name": "bash", "arguments": map[string]string{"command": "make test"}}}}),
		/* 5 */ piMsg(9, "r5", map[string]any{"role": "toolResult", "toolCallId": "call_1", "toolName": "bash", "content": piText("FAIL cache"), "isError": true}),
		/* 6 */ piMsg(10, "u6", map[string]any{"role": "user", "content": piText("and use -race")}),
		/* 7 */ piMsg(14, "a7", map[string]any{"role": "assistant", "model": "qwen3-coder", "stopReason": "stop", "content": piText("Running with -race.")}),
		/* 8 */ piMsg(30, "u8", map[string]any{"role": "user", "content": "now the docs"}),
		/* 9 */ piMsg(31, "a9", map[string]any{"role": "assistant", "model": "qwen3-coder", "stopReason": "error", "errorMessage": "provider overloaded", "content": []any{}}),
		/* 10 */ piMsg(40, "b10", map[string]any{"role": "bashExecution", "command": "git status", "output": "fatal: not a git repository", "exitCode": 128, "cancelled": false, "truncated": false}),
		/* 11 */ piEntry(50, `"type":"compaction","id":"c11","summary":"The cache work so far.","firstKeptEntryId":"u8","tokensBefore":50000`),
		/* 12 */ piMsg(55, "u12", map[string]any{"role": "user", "content": piText("continue")}),
		/* 13 */ piEntry(56, `"type":"custom_message","id":"x13","customType":"plan","content":"Plan: 1. fix the key","display":true`),
		/* 14 */ piEntry(57, `"type":"branch_summary","id":"x14","fromId":"u12","summary":"Branch tried approach A."`),
		/* 15 */ piMsg(58, "u15", map[string]any{"role": "user", "content": piText(`<file name="notes.md">the cache notes</file>`)}),
	}
	path := write(t, filepath.Join(dir, "2026-10-22T08-00-00-000Z_01pi.jsonl"), lines...)
	evs, _ := readAll(t, path, digest.Pi, 0)
	got := byLine(t, lines, evs)
	for _, i := range []int{0, 1, 2} {
		check(t, i, got[i])
	}
	check(t, 3, got[3], want{Kind: KindOwnerText, Who: digest.Owner, Why: "pi interactive", Text: "keep the cache warm before a release", InputKind: InputTyped})
	check(t, 4, got[4],
		want{Kind: KindAgentText, Who: digest.Agent, Text: "Looking."},
		want{Kind: KindToolCall, Who: digest.Agent, ToolID: "call_1", ToolName: "bash"})
	check(t, 5, got[5], want{Kind: KindToolResult, Who: digest.Harness, ToolID: "call_1", ToolName: "bash", IsError: true})
	// Sent while the agent's tools ran: steered into its turn.
	check(t, 6, got[6], want{Kind: KindOwnerText, Who: digest.Owner, Text: "and use -race", InputKind: InputQueued})
	check(t, 7, got[7], want{Kind: KindAgentText, Who: digest.Agent, Text: "Running with -race."})
	check(t, 8, got[8], want{Kind: KindOwnerText, Who: digest.Owner, Text: "now the docs", InputKind: InputTyped, Prior: "stop"})
	check(t, 9, got[9], want{Kind: KindHarnessText, Who: digest.Harness, Why: "provider error", Text: "provider overloaded", IsError: true})
	// A !command the person ran: their call, and its failure.
	check(t, 10, got[10],
		want{Kind: KindToolCall, Who: digest.Owner, ToolID: "b10", ToolName: "bash", InputKind: InputShell, Prior: "error"},
		want{Kind: KindToolResult, Who: digest.Harness, ToolID: "b10", ToolName: "bash", InputKind: InputShell, IsError: true})
	check(t, 11, got[11], want{Kind: KindCompaction, Who: digest.Harness, Why: "compaction", Text: "The cache work so far."})
	check(t, 12, got[12], want{Kind: KindOwnerText, Who: digest.Owner, Text: "continue", InputKind: InputTyped, Prior: "compaction"})
	check(t, 13, got[13], want{Kind: KindHarnessText, Who: digest.Harness, Why: "extension message", Text: "Plan: 1. fix the key"})
	check(t, 14, got[14], want{Kind: KindHarnessText, Who: digest.Harness, Why: "branch summary", Text: "Branch tried approach A."})
	check(t, 15, got[15], want{Kind: KindHarnessText, Who: digest.Harness, Text: `<file name="notes.md">the cache notes</file>`, InputKind: InputTyped, Prior: "branch_summary"})

	if in := got[10][0].ToolInput; in != `{"command":"git status"}` {
		t.Errorf("!command input: %q", in)
	}
	if out := got[5][0].ToolOutput; out != "FAIL cache" {
		t.Errorf("tool result: %q", out)
	}
	for _, e := range evs {
		if e.Session != "01pi" || e.Subagent != "" || e.Parent != "" || e.Cwd != "/home/u/projects/shop" || e.Model != "qwen3-coder" {
			t.Fatalf("session fields: %+v", e)
		}
	}

	// A task, under the parent's folder: an agent's session, its parent
	// named by its header or, without one, by the folder.
	tasks := filepath.Join(dir, "2026-10-22T08-00-00-000Z_01pi", "tasks")
	for _, header := range []string{
		`{"type":"session","version":3,"id":"02task","timestamp":"2026-10-22T08:10:00.000Z","cwd":"/home/u/projects/shop","parentSession":"` + filepath.ToSlash(path) + `"}`,
		`{"type":"session","version":3,"id":"02task","timestamp":"2026-10-22T08:10:00.000Z","cwd":"/home/u/projects/shop"}`,
	} {
		task := write(t, filepath.Join(tasks, "2026-10-22T08-10-00-000Z_02task.jsonl"),
			header,
			piEntry(600, `"type":"model_change","id":"m1","provider":"p","modelId":"qwen3-coder"`),
			piMsg(605, "u1", map[string]any{"role": "user", "content": piText("Review cache/key.go.")}),
			piMsg(609, "a1", map[string]any{"role": "assistant", "model": "qwen3-coder", "stopReason": "stop", "content": piText("Looks fine.")}),
		)
		evs, _ := readAll(t, task, digest.Pi, 0)
		if len(evs) != 2 {
			t.Fatalf("task events: %s", dump(evs))
		}
		check(t, 2, evs[:1], want{Kind: KindAgentText, Who: digest.Agent, Why: "pi subagent", Text: "Review cache/key.go."})
		for _, e := range evs {
			if e.Session != "02task" || e.Subagent != "02task" || e.Parent != "01pi" {
				t.Fatalf("task fields: %+v", e)
			}
		}
	}

	// A program's session (its first prompt right after pi started): an agent's.
	headless := write(t, filepath.Join(dir, "2026-10-22T09-00-00-000Z_03hl.jsonl"),
		`{"type":"session","version":3,"id":"03hl","timestamp":"2026-10-22T09:00:00.000Z","cwd":"/home/u/projects/shop"}`,
		`{"type":"model_change","id":"m1","parentId":null,"timestamp":"2026-10-22T09:00:00.000Z","provider":"p","modelId":"qwen3-coder"}`,
		`{"type":"message","id":"u1","parentId":"m1","timestamp":"2026-10-22T09:00:00.100Z","message":{"role":"user","content":"summarize the cache docs"}}`,
	)
	evs, _ = readAll(t, headless, digest.Pi, 0)
	check(t, 2, evs, want{Kind: KindAgentText, Who: digest.Agent, Why: "pi headless", Text: "summarize the cache docs"})
}
