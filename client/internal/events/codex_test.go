package events

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/agent-sh/tacit/client/internal/digest"
)

// cx is a line of a Codex rollout at minute m.
func cx(m int, typ, payload string) string {
	return fmt.Sprintf(`{"timestamp":"2026-10-21T09:%02d:00.000Z","type":%q,"payload":%s}`, m, typ, payload)
}

// cxUser is a user message of a rollout.
func cxUser(m int, texts ...string) string {
	var blocks []map[string]string
	for _, t := range texts {
		blocks = append(blocks, map[string]string{"type": "input_text", "text": t})
	}
	b, _ := json.Marshal(map[string]any{"type": "message", "role": "user", "content": blocks})
	return cx(m, "response_item", string(b))
}

// cxCall is a function call with its arguments, as Codex records them (a
// string of JSON).
func cxCall(m int, name, call, args string) string {
	a, _ := json.Marshal(args)
	return cx(m, "response_item", `{"type":"function_call","id":"fc_`+call+`","name":"`+name+`","arguments":`+string(a)+`,"call_id":"`+call+`"}`)
}

// cxOutput is a function call's output, a string.
func cxOutput(m int, call, output string) string {
	o, _ := json.Marshal(output)
	return cx(m, "response_item", `{"type":"function_call_output","call_id":"`+call+`","output":`+string(o)+`}`)
}

func codexMeta(id, extra string) string {
	return cx(0, "session_meta", `{"session_id":"`+id+`","id":"`+id+`","timestamp":"2026-10-21T09:00:00.000Z","cwd":"/home/u/projects/shop","originator":"codex-tui","cli_version":"0.162.1",`+extra+`"model_provider":"p","base_instructions":{"text":"You are Codex."},"history_mode":"full"}`)
}

// codexOwnerSession is a Codex session a person types into, with what each
// line should give.
func codexOwnerSession() []string {
	reply := `[{"answer":"memory","question":"Which backend?","questionItemId":"[\"request_user_input\",\"call_q1\",0]"},{"answer":"use 42 and keep it fixed","question":"Which seed?","questionItemId":"[\"request_user_input\",\"call_q1\",1]"}]`
	return []string{
		/* 0 */ codexMeta("019a-own", `"source":"cli","thread_source":"user",`),
		/* 1 */ cx(0, "response_item", `{"type":"message","role":"developer","content":[{"type":"input_text","text":"<permissions instructions>sandbox: workspace-write</permissions instructions>"}]}`),
		/* 2 */ cx(1, "event_msg", `{"type":"task_started","turn_id":"t1"}`),
		/* 3 */ cx(1, "turn_context", `{"turn_id":"t1","cwd":"/home/u/projects/shop","model":"gpt-6.1-sol","effort":"high","approval_policy":"never","summary":"auto"}`),
		/* 4 */ cxUser(1, "<environment_context>\n  <cwd>/home/u/projects/shop</cwd>\n</environment_context>", "make the cache tests pass"),
		/* 5 */ cxCall(2, "exec_command", "call_e1", `{"cmd":"go test ./cache"}`),
		/* 6 */ cxOutput(3, "call_e1", "Chunk ID: 1a2b\nWall time: 1.2 seconds\nProcess exited with code 1\nOriginal token count: 30\nOutput:\n--- FAIL: TestKey\nExit code: 0\n"),
		/* 7 */ cxUser(4, "and run it with -race"),
		/* 8 */ cx(5, "response_item", `{"type":"custom_tool_call","status":"completed","call_id":"call_p1","name":"apply_patch","input":"*** Begin Patch\n*** Update File: cache/key.go\n@@\n-old\n+new\n*** End Patch\n"}`),
		/* 9 */ cx(5, "event_msg", `{"type":"patch_apply_end","call_id":"call_p1","turn_id":"t1","stdout":"","stderr":"context mismatch","success":false,"changes":{}}`),
		/* 10 */ cx(5, "response_item", `{"type":"custom_tool_call_output","call_id":"call_p1","output":[{"type":"input_text","text":"Patch failed: context mismatch"}]}`),
		/* 11 */ cx(6, "response_item", `{"type":"function_call","id":"fc_m1","name":"tacit_search","namespace":"mcp__tacit","arguments":"{\"query\":\"cache\"}","call_id":"call_m1"}`),
		/* 12 */ cx(6, "event_msg", `{"type":"mcp_tool_call_end","call_id":"call_m1","invocation":{"server":"tacit","tool":"tacit_search"},"duration":{"secs":0,"nanos":1000},"result":{"Ok":{"content":[{"type":"text","text":"store unreachable"}],"isError":true}}}`),
		/* 13 */ cxOutput(6, "call_m1", "store unreachable"),
		/* 14 */ cxCall(7, "exec_command", "call_e2", `{"cmd":"go test -race ./cache"}`),
		/* 15 */ cxOutput(8, "call_e2", "Exit code: 0\nWall time: 0.4 seconds\nOutput:\nok  \tcache\n"),
		/* 16 */ cx(9, "response_item", `{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"Fixed: the key now holds the lockfile hash."}]}`),
		/* 17 */ cx(9, "event_msg", `{"type":"task_complete","turn_id":"t1","last_agent_message":"Fixed."}`),
		/* 18 */ cx(10, "turn_context", `{"turn_id":"t2","cwd":"/home/u/projects/other","model":"gpt-6.1-luna","effort":"low","approval_policy":"never","summary":"auto"}`),
		/* 19 */ cxUser(10, "pick the backend"),
		/* 20 */ cxCall(11, "request_user_input", "call_q1", `{"questions":[{"title":"Which backend?","options":["redis","memory"]},{"title":"Which seed?","options":["7","42"]}]}`),
		/* 21 */ cxUser(12, "<send_user_message_question_reply>\n"+reply+"\n</send_user_message_question_reply>"),
		/* 22 */ cx(13, "event_msg", `{"type":"turn_aborted","turn_id":"t2","reason":"interrupted"}`),
		/* 23 */ cx(14, "compacted", `{"message":"Summary of the cache work so far.","replacement_history":[{"type":"message","role":"user","content":[{"type":"input_text","text":"make the cache tests pass"}]}]}`),
		/* 24 */ cxUser(15, "continue with the seed"),
		/* 25 */ cxUser(16, "Wake nonce=5c1d: report the queue depth"),
		/* 26 */ cx(17, "response_item", `{"type":"agent_message","author":"/root/ada","recipient":"/root","content":[{"type":"input_text","text":"subagent report: key.go updated"},{"type":"encrypted_content","encrypted_content":"x"}]}`),
		/* 27 */ cx(18, "response_item", `{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"zstd frame format"}}`),
		/* 28 */ cxUser(19, "| host | disk |\n| a | 91% |\n| b | 40% |\n| c | 12% |\n| d | 77% |\n| e | 63% |"),
	}
}

func TestCodexSessionEvents(t *testing.T) {
	home := t.TempDir()
	lines := codexOwnerSession()
	path := write(t, filepath.Join(home, ".codex", "sessions", "2026", "10", "21", "rollout-2026-10-21T09-00-00-019a-own.jsonl"), lines...)
	evs, _ := readAll(t, path, digest.Codex, 0)
	got := byLine(t, lines, evs)
	const owner = "originator codex-tui"
	check(t, 0, got[0])
	check(t, 1, got[1], want{Kind: KindHarnessText, Who: digest.Harness, Why: "developer message", Text: "<permissions instructions>sandbox: workspace-write</permissions instructions>"})
	check(t, 4, got[4],
		want{Kind: KindHarnessText, Who: digest.Harness, Text: "<environment_context>\n  <cwd>/home/u/projects/shop</cwd>\n</environment_context>", InputKind: InputTyped},
		want{Kind: KindOwnerText, Who: digest.Owner, Why: owner, Text: "make the cache tests pass", InputKind: InputTyped})
	check(t, 5, got[5], want{Kind: KindToolCall, Who: digest.Agent, ToolID: "call_e1", ToolName: "exec_command"})
	// The exit code in Codex's header decides, not text the command printed.
	check(t, 6, got[6], want{Kind: KindToolResult, Who: digest.Harness, ToolID: "call_e1", ToolName: "exec_command", IsError: true})
	// Typed while the turn ran: steered into it.
	check(t, 7, got[7], want{Kind: KindOwnerText, Who: digest.Owner, Text: "and run it with -race", InputKind: InputQueued})
	check(t, 8, got[8], want{Kind: KindToolCall, Who: digest.Agent, ToolID: "call_p1", ToolName: "apply_patch"})
	check(t, 9, got[9])
	// The patch's end event, written before the output, says it failed.
	check(t, 10, got[10], want{Kind: KindToolResult, Who: digest.Harness, ToolID: "call_p1", ToolName: "apply_patch", IsError: true})
	// A namespaced tool is named as Claude Code names an MCP server's.
	check(t, 11, got[11], want{Kind: KindToolCall, Who: digest.Agent, ToolID: "call_m1", ToolName: "mcp__tacit__tacit_search"})
	check(t, 13, got[13], want{Kind: KindToolResult, Who: digest.Harness, ToolID: "call_m1", ToolName: "mcp__tacit__tacit_search", IsError: true})
	check(t, 14, got[14], want{Kind: KindToolCall, Who: digest.Agent, ToolID: "call_e2", ToolName: "exec_command"})
	for _, i := range []int{2, 3, 12, 17, 18, 22} {
		// (18 is the second turn's context: no event, a new model and cwd)
		check(t, i, got[i]) // the turn's records: no event of their own
	}
	check(t, 15, got[15], want{Kind: KindToolResult, Who: digest.Harness, ToolID: "call_e2", ToolName: "exec_command"})
	check(t, 16, got[16], want{Kind: KindAgentText, Who: digest.Agent, Why: whyAssistant, Text: "Fixed: the key now holds the lockfile hash."})
	check(t, 19, got[19], want{Kind: KindOwnerText, Who: digest.Owner, Text: "pick the backend", InputKind: InputTyped, Prior: "task_complete"})
	check(t, 20, got[20], want{Kind: KindToolCall, Who: digest.Agent, ToolID: "call_q1", ToolName: "request_user_input"})
	// Their answers: an option the question offered is the model's words, a
	// typed answer theirs.
	check(t, 21, got[21],
		want{Kind: KindHarnessText, Who: digest.Harness, Text: "Which backend?", InputKind: InputAnswer},
		want{Kind: KindHarnessText, Who: digest.Harness, Why: whyOffered, Text: "memory", InputKind: InputAnswer},
		want{Kind: KindHarnessText, Who: digest.Harness, Text: "Which seed?", InputKind: InputAnswer},
		want{Kind: KindOwnerText, Who: digest.Owner, Text: "use 42 and keep it fixed", InputKind: InputAnswer})
	check(t, 23, got[23], want{Kind: KindCompaction, Who: digest.Harness, Why: "compacted", Text: "Summary of the cache work so far."})
	check(t, 24, got[24], want{Kind: KindOwnerText, Who: digest.Owner, Text: "continue with the seed", InputKind: InputTyped, Prior: "compacted"})
	// A wake loop's prompt in a person's session is not their words.
	check(t, 25, got[25], want{Kind: KindHarnessText, Who: digest.Harness, Text: "Wake nonce=5c1d: report the queue depth", InputKind: InputTyped})
	check(t, 26, got[26], want{Kind: KindAgentText, Who: digest.Agent, Why: "message from another agent", Text: "subagent report: key.go updated"})
	check(t, 27, got[27], want{Kind: KindToolCall, Who: digest.Agent, ToolID: "ws_1", ToolName: "web_search"})
	// A pasted table is no words of theirs (ownerwords.PastedBlob): an agent's.
	check(t, 28, got[28], want{Kind: KindAgentText, Who: digest.Agent, Why: whyLeftOut, Text: "| host | disk |\n| a | 91% |\n| b | 40% |\n| c | 12% |\n| d | 77% |\n| e | 63% |", InputKind: InputTyped})

	if in := got[5][0].ToolInput; in != `{"cmd":"go test ./cache"}` {
		t.Errorf("arguments: %q", in)
	}
	if in := got[8][0].ToolInput; in != "*** Begin Patch\n*** Update File: cache/key.go\n@@\n-old\n+new\n*** End Patch\n" {
		t.Errorf("custom tool input: %q", in)
	}
	if out := got[10][0].ToolOutput; out != "Patch failed: context mismatch" {
		t.Errorf("output blocks: %q", out)
	}
	for _, e := range evs {
		if e.Session != "019a-own" || e.Subagent != "" || e.Parent != "" {
			t.Fatalf("session fields: %+v", e)
		}
	}
	if e := got[4][1]; e.Model != "gpt-6.1-sol" || e.Cwd != "/home/u/projects/shop" {
		t.Errorf("model and cwd from the first turn's context: %q %q", e.Model, e.Cwd)
	}
	if e := got[19][0]; e.Model != "gpt-6.1-luna" || e.Cwd != "/home/u/projects/other" {
		t.Errorf("model and cwd from the second turn's context: %q %q", e.Model, e.Cwd)
	}
}

// A compressed rollout reads as the plain one did, and a cursor taken from
// an event (its Next) resumes in the decompressed stream with the events
// after it, the model and cwd of the session's first turn known.
func TestCodexCompressedResume(t *testing.T) {
	home := t.TempDir()
	lines := codexOwnerSession()
	plain := write(t, filepath.Join(home, ".codex", "sessions", "2026", "10", "21", "rollout-2026-10-21T09-00-00-019a-own.jsonl"), lines...)
	whole, end := readAll(t, plain, digest.Codex, 0)
	packed := compress(t, plain)
	for _, p := range []string{plain, packed} {
		again, next := readAll(t, p, digest.Codex, 0)
		if next != end || !reflect.DeepEqual(again, whole) {
			t.Fatalf("%s: compressed copy reads differently (%d events, cursor %d; want %d, %d)", p, len(again), next, len(whole), end)
		}
	}
	var at int
	for i, e := range whole {
		if e.Kind == KindToolResult && e.ToolID == "call_e2" {
			at = i
		}
	}
	cursor := whole[at].Next
	rest, next := readAll(t, packed, digest.Codex, cursor)
	if next != end || len(rest) != len(whole)-at-1 {
		t.Fatalf("resumed at %d: %d events, cursor %d; want %d, %d", cursor, len(rest), next, len(whole)-at-1, end)
	}
	if !reflect.DeepEqual(rest, whole[at+1:]) {
		t.Fatalf("resumed in the compressed copy:\n%s\nwant\n%s", dump(rest), dump(whole[at+1:]))
	}
	if _, err := ReadFile(packed, digest.Codex, end+1, func(Event) error { return nil }); err == nil {
		t.Fatal("a cursor past the end was read")
	}
}

// A subagent's thread is an agent's, with its parent named.
func TestCodexSubagentThread(t *testing.T) {
	home := t.TempDir()
	spawn := `{"subagent":{"thread_spawn":{"parent_thread_id":"019a-own","depth":1,"agent_path":"/root/ada","agent_nickname":"Ada","agent_role":"worker"}}}`
	lines := []string{
		codexMeta("019a-sub", `"source":`+spawn+`,"thread_source":"subagent","parent_thread_id":"019a-own","agent_nickname":"Ada","agent_role":"worker","agent_path":"/root/ada","multi_agent_version":2,`),
		cxUser(1, "Update cache/key.go to hash the lockfile and report back."),
		cx(2, "response_item", `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Updated."}]}`),
	}
	path := write(t, filepath.Join(home, ".codex", "sessions", "2026", "10", "21", "rollout-2026-10-21T09-01-00-019a-sub.jsonl"), lines...)
	evs, _ := readAll(t, path, digest.Codex, 0)
	got := byLine(t, lines, evs)
	check(t, 1, got[1], want{Kind: KindAgentText, Who: digest.Agent, Why: "codex subagent", Text: "Update cache/key.go to hash the lockfile and report back."})
	check(t, 2, got[2], want{Kind: KindAgentText, Who: digest.Agent, Text: "Updated."})
	for _, e := range evs {
		if e.Session != "019a-sub" || e.Subagent != "019a-sub" || e.Parent != "019a-own" {
			t.Fatalf("subagent thread fields: %+v", e)
		}
	}
}

// codexNowSession is a session as Codex 0.165 writes it: commands run in the
// script tool exec (its output opens with "Script completed" or "Script
// failed") or exec_command, and a call's end comes as the item it completed
// (item_completed, its status), before the output.
func codexNowSession() []string {
	return []string{
		/* 0 */ codexMeta("019b-now", `"source":"cli","thread_source":"user",`),
		/* 1 */ cx(1, "event_msg", `{"type":"task_started","turn_id":"t1"}`),
		/* 2 */ cx(1, "turn_context", `{"turn_id":"t1","cwd":"/home/u/projects/shop","model":"gpt-6.1-sol","effort":"high"}`),
		/* 3 */ cxUser(1, "run the cache tests"),
		/* 4 */ cx(2, "response_item", `{"type":"custom_tool_call","status":"completed","call_id":"call_x1","name":"exec","input":"await tools.exec_command({cmd: 'go test ./cache'})"}`),
		/* 5 */ cx(3, "event_msg", `{"type":"item_completed","thread_id":"019b-now","turn_id":"t1","item":{"type":"CommandExecution","id":"item_9","command":"go test ./cache","status":"failed","exit_code":1,"aggregated_output":"FAIL"}}`),
		/* 6 */ cx(3, "response_item", `{"type":"custom_tool_call_output","call_id":"call_x1","output":[{"type":"input_text","text":"Script failed\nWall time 0.4 seconds\nOutput:\nError: go test exited with code 1\n"}]}`),
		/* 7 */ cx(4, "response_item", `{"type":"custom_tool_call","status":"completed","call_id":"call_x2","name":"exec","input":"await tools.exec_command({cmd: 'go vet ./cache'})"}`),
		/* 8 */ cx(4, "response_item", `{"type":"custom_tool_call_output","call_id":"call_x2","output":[{"type":"input_text","text":"Script completed\nWall time 0.2 seconds\nOutput:\nScript failed earlier, fixed now\n"}]}`),
		/* 9 */ cxCall(5, "exec_command", "call_e3", `{"cmd":"make lint"}`),
		/* 10 */ cx(5, "event_msg", `{"type":"item_completed","thread_id":"019b-now","turn_id":"t1","item":{"type":"CommandExecution","id":"call_e3","command":"make lint","status":"failed","exit_code":2,"aggregated_output":"lint: 3 issues"}}`),
		/* 11 */ cxOutput(5, "call_e3", "lint: 3 issues"),
		/* 12 */ cx(6, "response_item", `{"type":"function_call","id":"fc_j1","name":"js","namespace":"mcp__cua_repl","arguments":"{\"code\":\"1+1\"}","call_id":"call_j1"}`),
		/* 13 */ cx(6, "event_msg", `{"type":"item_completed","thread_id":"019b-now","turn_id":"t1","item":{"type":"McpToolCall","id":"call_j1","server":"cua_repl","tool":"js","status":"completed","result":{"content":[]}}}`),
		/* 14 */ cxOutput(6, "call_j1", "2"),
		/* 15 */ cx(7, "response_item", `{"type":"custom_tool_call","status":"completed","call_id":"call_x3","name":"exec","input":"await tools.exec_command({cmd: 'sleep 999'})"}`),
		/* 16 */ cx(8, "response_item", `{"type":"custom_tool_call_output","call_id":"call_x3","output":[{"type":"input_text","text":"aborted by user after 4s"}]}`),
		/* 17 */ cx(9, "event_msg", `{"type":"turn_aborted","turn_id":"t1","reason":"interrupted"}`),
		/* 18 */ cxUser(10, "skip the slow test"),
	}
}

func TestCodexCurrentFormat(t *testing.T) {
	home := t.TempDir()
	lines := codexNowSession()
	path := write(t, filepath.Join(home, ".codex", "sessions", "2026", "10", "29", "rollout-2026-10-29T09-00-00-019b-now.jsonl"), lines...)
	evs, _ := readAll(t, path, digest.Codex, 0)
	got := byLine(t, lines, evs)
	check(t, 4, got[4], want{Kind: KindToolCall, Who: digest.Agent, ToolID: "call_x1", ToolName: "exec"})
	// The script failed, so its result is an error; a command inside it
	// (item_9, no call of the agent's) flags nothing else.
	check(t, 6, got[6], want{Kind: KindToolResult, Who: digest.Harness, ToolID: "call_x1", ToolName: "exec", IsError: true})
	// "Script failed" in the command's own output is no failure of the script.
	check(t, 8, got[8], want{Kind: KindToolResult, Who: digest.Harness, ToolID: "call_x2", ToolName: "exec"})
	// The completed item of the call, before its output, says it failed.
	check(t, 11, got[11], want{Kind: KindToolResult, Who: digest.Harness, ToolID: "call_e3", ToolName: "exec_command", IsError: true})
	check(t, 12, got[12], want{Kind: KindToolCall, Who: digest.Agent, ToolID: "call_j1", ToolName: "mcp__cua_repl__js"})
	check(t, 14, got[14], want{Kind: KindToolResult, Who: digest.Harness, ToolID: "call_j1", ToolName: "mcp__cua_repl__js"})
	// Interrupted by the person: the harness records the script as aborted.
	check(t, 16, got[16], want{Kind: KindToolResult, Who: digest.Harness, ToolID: "call_x3", ToolName: "exec", IsError: true})
	check(t, 18, got[18], want{Kind: KindOwnerText, Who: digest.Owner, Text: "skip the slow test", InputKind: InputTyped, Prior: "turn_aborted"})
	for _, i := range []int{0, 1, 2, 5, 10, 13, 17} {
		check(t, i, got[i])
	}
}

// An answer the corpus's own split finds not the person's (it opens with a
// harness notice) is cut from their words, and shown, not lost; a question
// asked by a namespaced call still decides which answers are an offered
// option.
// codexAskSession is a Codex session whose agent asks the person questions
// with a namespaced call, and their reply (TestCodexAnswersCutAndNamespacedQuestions).
func codexAskSession() []string {
	reply := `[{"answer":"memory","question":"Which backend?","questionItemId":"[\"request_user_input\",\"call_q2\",0]"},{"answer":"use 42 and keep it fixed","question":"Which seed?","questionItemId":"[\"request_user_input\",\"call_q2\",1]"},{"answer":"Continue the current task first, then the key","question":"Anything else?","questionItemId":"[\"request_user_input\",\"call_q2\",2]"}]`
	ask, _ := json.Marshal(`{"questions":[{"title":"Which backend?","options":["redis","memory"]},{"title":"Which seed?","options":["7","42"]},{"title":"Anything else?","options":["no"]}]}`)
	return []string{
		codexMeta("019c-ask", `"source":"cli","thread_source":"user",`),
		cx(1, "response_item", `{"type":"function_call","id":"fc_q2","name":"request_user_input","namespace":"collaboration","arguments":`+string(ask)+`,"call_id":"call_q2"}`),
		cxUser(2, "<send_user_message_question_reply>\n"+reply+"\n</send_user_message_question_reply>"),
	}
}

func TestCodexAnswersCutAndNamespacedQuestions(t *testing.T) {
	home := t.TempDir()
	lines := codexAskSession()
	path := write(t, filepath.Join(home, ".codex", "sessions", "2026", "10", "29", "rollout-2026-10-29T10-00-00-019c-ask.jsonl"), lines...)
	evs, _ := readAll(t, path, digest.Codex, 0)
	got := byLine(t, lines, evs)
	check(t, 1, got[1], want{Kind: KindToolCall, Who: digest.Agent, ToolID: "call_q2", ToolName: "collaboration__request_user_input"})
	check(t, 2, got[2],
		want{Kind: KindHarnessText, Who: digest.Harness, Text: "Which backend?", InputKind: InputAnswer},
		want{Kind: KindHarnessText, Who: digest.Harness, Why: whyOffered, Text: "memory", InputKind: InputAnswer},
		want{Kind: KindHarnessText, Who: digest.Harness, Text: "Which seed?", InputKind: InputAnswer},
		want{Kind: KindOwnerText, Who: digest.Owner, Text: "use 42 and keep it fixed", InputKind: InputAnswer},
		want{Kind: KindAgentText, Who: digest.Agent, Why: whyLeftOut, Text: "Continue the current task first, then the key", InputKind: InputAnswer},
		want{Kind: KindHarnessText, Who: digest.Harness, Text: "Anything else?", InputKind: InputAnswer})
}
