package events

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/ownerwords"
	"github.com/agent-sh/tacit/client/internal/transcript"
)

// Codex writes one rollout per thread (a session, or a subagent's thread):
// session_meta first, then response items (messages, tool calls and their
// outputs, by call_id), event messages (a turn's start and end, the end of a
// patch or an MCP call, errors), turn_context at each turn's start (its model
// and cwd) and compacted entries. It rolls an old rollout into a .jsonl.zst
// copy, read the same (transcript).
type codexReader struct {
	base
	turns ownerwords.TurnReader
	// asked are the options a request_user_input call offered, for the
	// answers that follow (digest.CodexAsked).
	asked digest.CodexAsked
	// replay are the lines that changed asked, and the owner's words' own
	// copy of it, in order: those the rule notes a question's options on
	// (noted, emptied for each line). Read again, they leave both as they
	// were (State).
	replay []string
	noted  digest.CodexAsked
	// failed are the calls an event reported failed before their output came
	// (a patch, an MCP call, a command's exit code): Codex writes the end
	// event first.
	failed map[string]bool
	// open is a turn under way (task_started, until task_complete or
	// turn_aborted), active one in which the agent has done something: a
	// user message then was steered into it.
	open, active bool
}

func newCodex(plain string, head []byte, v digest.Verdict, pastes []string) *codexReader {
	r := &codexReader{base: newBase("codex", transcript.Name(plain)), asked: digest.CodexAsked{}, noted: digest.CodexAsked{}, failed: map[string]bool{}}
	r.verdict = v
	r.turns.Pastes = pastes
	if m, ok := digest.ReadCodexMeta(head); ok {
		if m.ID != "" {
			r.session = m.ID
		}
		r.cwd, r.parent = m.Cwd, m.Parent
		if m.Parent != "" || m.Source == "subagent" || m.ThreadSource == "subagent" {
			r.subagent = r.session
		}
	}
	return r
}

type codexLine struct {
	Timestamp json.RawMessage `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

// codexPayload is the fields read of a payload; ones whose type varies are
// kept raw.
type codexPayload struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Name      string          `json:"name"`
	Namespace string          `json:"namespace"`
	CallID    string          `json:"call_id"`
	ID        string          `json:"id"`
	Arguments json.RawMessage `json:"arguments"`
	Input     json.RawMessage `json:"input"`
	Output    json.RawMessage `json:"output"`
	Action    json.RawMessage `json:"action"`
	Tools     json.RawMessage `json:"tools"`
	Content   json.RawMessage `json:"content"`
	Message   json.RawMessage `json:"message"`
	Success   json.RawMessage `json:"success"`
	ExitCode  json.RawMessage `json:"exit_code"`
	Result    json.RawMessage `json:"result"`
	Item      json.RawMessage `json:"item"`
}

func (r *codexReader) context(raw json.RawMessage) {
	var c struct {
		Cwd   string `json:"cwd"`
		Model string `json:"model"`
	}
	if json.Unmarshal(raw, &c) == nil {
		if c.Cwd != "" {
			r.cwd = c.Cwd
		}
		if c.Model != "" {
			r.model = c.Model
		}
	}
}

func (r *codexReader) line(raw []byte) []Event {
	var l codexLine
	if json.Unmarshal(raw, &l) != nil {
		return nil
	}
	// A line the rule notes a question's options on is one to read again
	// (State).
	clear(r.noted)
	if r.noted.NoteLine(raw); len(r.noted) > 0 {
		r.replay = append(r.replay, string(raw))
	}
	r.asked.NoteLine(raw)
	at := ownerwords.JSONTime(l.Timestamp)
	switch l.Type {
	case "turn_context":
		r.context(l.Payload)
	case "compacted":
		var p codexPayload
		_ = json.Unmarshal(l.Payload, &p)
		r.prior = "compacted"
		return []Event{r.text(KindCompaction, "system", digest.Harness, "compacted", rawText(p.Message), at)}
	case "event_msg":
		var p codexPayload
		_ = json.Unmarshal(l.Payload, &p)
		r.eventMsg(p)
	case "response_item":
		var p codexPayload
		if json.Unmarshal(l.Payload, &p) != nil {
			return nil
		}
		return r.item(raw, p, at)
	}
	return nil
}

func (r *codexReader) eventMsg(p codexPayload) {
	switch p.Type {
	case "task_started":
		r.open, r.active = true, false
	case "task_complete", "turn_aborted":
		r.open = false
		r.prior = p.Type
	case "error", "stream_error", "context_compacted", "thread_rolled_back":
		r.prior = p.Type
	case "patch_apply_end":
		if string(p.Success) == "false" {
			r.failed[p.CallID] = true
		}
	case "exec_command_end":
		if n, err := strconv.ParseFloat(string(p.ExitCode), 64); err == nil && n != 0 {
			r.failed[p.CallID] = true
		}
	case "mcp_tool_call_end":
		if mcpFailed(p.Result) {
			r.failed[p.CallID] = true
		}
	case "item_completed":
		// Current Codex reports a call's end as the item it completed: a
		// command, an MCP call or a file change with its status.
		if id, ok := itemFailed(p.Item); ok {
			r.failed[id] = true
		}
	}
}

// itemFailed is the id of a completed item that failed: its status is
// "failed", its exit code is not 0, or it holds an error.
func itemFailed(raw json.RawMessage) (string, bool) {
	var it struct {
		ID       string          `json:"id"`
		Status   string          `json:"status"`
		ExitCode json.RawMessage `json:"exit_code"`
		Error    json.RawMessage `json:"error"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &it) != nil || it.ID == "" {
		return "", false
	}
	code, _ := strconv.ParseFloat(string(it.ExitCode), 64)
	errSet := len(it.Error) > 0 && string(it.Error) != "null" && string(it.Error) != `""`
	return it.ID, it.Status == "failed" || code != 0 || errSet
}

// mcpFailed reports an MCP call's result that failed: an Err, or an Ok the
// server marked isError.
func mcpFailed(raw json.RawMessage) bool {
	var res struct {
		Ok *struct {
			IsError bool `json:"isError"`
		} `json:"Ok"`
		Err json.RawMessage `json:"Err"`
	}
	if json.Unmarshal(raw, &res) != nil {
		return false
	}
	return len(res.Err) > 0 && string(res.Err) != "null" || res.Ok != nil && res.Ok.IsError
}

func (r *codexReader) item(raw []byte, p codexPayload, at time.Time) []Event {
	var out []Event
	switch p.Type {
	case "message":
		texts := codexTexts(p.Content)
		switch p.Role {
		case "user":
			out = r.user(raw, texts, at)
		case "assistant":
			for _, t := range texts {
				if strings.TrimSpace(t) == "" {
					continue
				}
				if strings.HasPrefix(t, "[Recorded tool history]") {
					out = append(out, r.text(KindHarnessText, "assistant", digest.Harness, "recorded tool history", t, at))
					continue
				}
				out = append(out, r.text(KindAgentText, "assistant", digest.Agent, whyAssistant, t, at))
			}
			r.active = r.open
		case "developer":
			if t := strings.TrimSpace(strings.Join(texts, "\n")); t != "" {
				out = append(out, r.text(KindHarnessText, "developer", digest.Harness, "developer message", strings.Join(texts, "\n"), at))
			}
			return out // Codex's own instructions; not an item of the turn
		}
	case "agent_message":
		// A message another agent sent this thread (a subagent's report, a
		// parent's instruction).
		if t := strings.Join(codexTexts(p.Content), "\n"); strings.TrimSpace(t) != "" {
			out = append(out, r.text(KindAgentText, "user", digest.Agent, "message from another agent", t, at))
		}
	case "function_call", "custom_tool_call", "tool_search_call", "local_shell_call", "web_search_call":
		out = append(out, r.call(raw, p, at))
	case "function_call_output", "custom_tool_call_output", "tool_search_output":
		out = append(out, r.output(p, at))
	default:
		return nil // reasoning, and items that hold no text of the turn
	}
	r.prior = ""
	return out
}

func (r *codexReader) user(raw []byte, texts []string, at time.Time) []Event {
	if !r.verdict.Owner() {
		who := digest.Agent
		if r.verdict.Who == digest.Unknown {
			who = digest.Unknown
		}
		return r.untyped(digest.Codex, texts, who, r.verdict.Why, at, "")
	}
	own, _, cut := r.turns.PartsCut(digest.Codex, raw)
	in := InputTyped
	if r.open && r.active {
		in = InputQueued // steered into the running turn
	}
	return r.typed(texts, own, cut, r.asked, at, in)
}

func (r *codexReader) call(raw []byte, p codexPayload, at time.Time) Event {
	id, name := p.CallID, p.Name
	input := unquote(p.Arguments)
	if input == "" {
		input = unquote(p.Input)
	}
	switch p.Type {
	case "web_search_call":
		if id == "" {
			id = p.ID
		}
		name, input = "web_search", string(p.Action)
	case "local_shell_call":
		name, input = "local_shell", string(p.Action)
	case "tool_search_call":
		name = "tool_search"
	}
	if p.Namespace != "" {
		// A namespaced tool (an MCP server's, mcp__<server>) is named as
		// Claude Code names one: mcp__<server>__<tool>.
		name = p.Namespace + "__" + name
	}
	if strings.HasPrefix(p.Name, "request_user_input") && r.verdict.Owner() {
		// The options it offers decide the answers that follow, for the
		// owner's words as for the pieces (TurnReader keeps its own).
		r.turns.Parts(digest.Codex, raw)
	}
	r.calls[id] = name
	r.active = r.open
	e := r.event(KindToolCall, "assistant", digest.Agent, whyToolCall, at)
	e.ToolID, e.ToolName, e.ToolInput = id, name, input
	return e
}

func (r *codexReader) output(p codexPayload, at time.Time) Event {
	id := p.CallID
	text, code := codexOutput(p.Output)
	if p.Type == "tool_search_output" {
		text = string(p.Tools)
	}
	e := r.event(KindToolResult, "tool", digest.Harness, whyToolResult, at)
	e.ToolID, e.ToolName, e.ToolOutput = id, r.calls[id], text
	if id == "" {
		e.ToolName = p.Name
	}
	e.IsError = r.failed[id] || code != 0 || scriptFailed(e.ToolName, text)
	delete(r.failed, id)
	r.active = r.open
	return e
}

// codexTexts are the texts of a message's content blocks.
func codexTexts(raw json.RawMessage) []string {
	var bs []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &bs) != nil {
		if s := rawText(raw); s != "" {
			return []string{s}
		}
		return nil
	}
	var out []string
	for _, b := range bs {
		if b.Text != "" {
			out = append(out, b.Text)
		}
	}
	return out
}

// unquote is a JSON string's value (Codex records a call's arguments as a
// string of JSON, a custom tool's input as its raw text), or the raw JSON
// when it is not a string.
func unquote(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// codexOutput is an output as the agent got it and the exit code Codex
// recorded with it (0 when it records none): the header Codex writes before
// a command's output ("Exit code: N", "Process exited with code N", above the
// line "Output:"), or the exit_code in the metadata of an older tool's JSON
// output. A list of content blocks is their texts ("[image]" for an image).
func codexOutput(raw json.RawMessage) (string, int) {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		var bs []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &bs) != nil {
			return string(raw), 0
		}
		var parts []string
		for _, b := range bs {
			switch {
			case b.Text != "":
				parts = append(parts, b.Text)
			case strings.Contains(b.Type, "image"):
				parts = append(parts, "[image]")
			}
		}
		s = strings.Join(parts, "\n")
		return s, headerExit(s)
	}
	if strings.HasPrefix(s, "{") {
		var o struct {
			Output   *string `json:"output"`
			Metadata struct {
				ExitCode json.RawMessage `json:"exit_code"`
			} `json:"metadata"`
		}
		if json.Unmarshal([]byte(s), &o) == nil && o.Output != nil && len(o.Metadata.ExitCode) > 0 {
			code, _ := strconv.ParseFloat(string(o.Metadata.ExitCode), 64)
			return *o.Output, int(code)
		}
	}
	return s, headerExit(s)
}

// scriptFailed reports the output of Codex's script tool (exec, and wait for
// a script left running) whose first line Codex wrote as a failure: "Script
// failed", the person's interruption ("aborted by user after ..."), or a call
// Codex refused ("unsupported custom tool call"). "Script completed" and
// "Script running" are not.
func scriptFailed(tool, out string) bool {
	if tool != "exec" && tool != "wait" {
		return false
	}
	first, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	first = strings.TrimSpace(first)
	return first == "Script failed" || strings.HasPrefix(first, "aborted by user") || strings.HasPrefix(first, "unsupported custom tool call")
}

var exitLine = regexp.MustCompile(`^(?:Exit code: |Process exited with code )(-?\d+)\s*$`)

// headerExit is the exit code in the header Codex writes before a command's
// output: the lines above the line "Output:", which is among the first
// eight; 0 when there is none. Text past the header is the command's own and
// never read for it.
func headerExit(s string) int {
	var header []string
	for range 8 {
		line, rest, more := strings.Cut(s, "\n")
		if strings.TrimSpace(line) == "Output:" {
			for _, h := range header {
				if m := exitLine.FindStringSubmatch(strings.TrimSpace(h)); m != nil {
					n, _ := strconv.Atoi(m[1])
					return n
				}
			}
			return 0
		}
		header = append(header, line)
		if !more {
			break
		}
		s = rest
	}
	return 0
}
