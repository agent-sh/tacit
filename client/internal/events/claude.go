package events

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/ownerwords"
	"github.com/agent-sh/tacit/client/internal/transcript"
)

// Claude Code writes one JSONL file per session,
// ~/.claude/projects/<project>/<session>.jsonl, and one per subagent the
// session ran, <session>/subagents/agent-<id>.jsonl, whose lines carry the
// parent's sessionId and the subagent's agentId. A line is a user turn (text,
// or tool results), an assistant reply (text, tool calls, thinking), a
// system record (its subtype) or an attachment (a message queued while the
// agent worked, a hook's output, reminders).
type claudeReader struct {
	base
	turns ownerwords.TurnReader
	cmds  digest.ClaudeCommands
	// replay are the lines that changed cmds, in order: read again, they
	// leave cmds, and the owner's words' own copy, as they were (State).
	replay []string
}

func newClaude(plain string, v digest.Verdict, pastes []string) *claudeReader {
	r := &claudeReader{base: newBase("claude", transcript.Name(plain))}
	r.verdict = v
	r.turns.Pastes = pastes
	if strings.Contains(filepath.ToSlash(plain), "/subagents/") {
		r.subagent = strings.TrimPrefix(transcript.Name(plain), "agent-")
	}
	return r
}

// synthetic is the model Claude Code names on a reply it wrote itself (an
// API error, an interruption), not a model's.
const synthetic = "<synthetic>"

type claudeLine struct {
	Type         string          `json:"type"`
	Subtype      string          `json:"subtype"`
	SessionID    string          `json:"sessionId"`
	AgentID      string          `json:"agentId"`
	Cwd          string          `json:"cwd"`
	Timestamp    json.RawMessage `json:"timestamp"`
	IsMeta       bool            `json:"isMeta"`
	IsCompact    bool            `json:"isCompactSummary"`
	IsAPIError   bool            `json:"isApiErrorMessage"`
	PromptSource string          `json:"promptSource"`
	Content      json.RawMessage `json:"content"`
	Message      struct {
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	Attachment struct {
		Type        string          `json:"type"`
		HookEvent   string          `json:"hookEvent"`
		ToolUseID   string          `json:"toolUseID"`
		CommandMode string          `json:"commandMode"`
		Content     json.RawMessage `json:"content"`
	} `json:"attachment"`
}

type claudeBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// claudeBlocks are a message's content blocks; a string content is one text
// block.
func claudeBlocks(raw json.RawMessage) []claudeBlock {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []claudeBlock{{Type: "text", Text: s}}
	}
	var bs []claudeBlock
	_ = json.Unmarshal(raw, &bs)
	return bs
}

// note keeps what a line says about its session: its id, the subagent, the
// working directory.
func (r *claudeReader) note(l claudeLine) {
	if l.SessionID != "" {
		r.session = l.SessionID
	}
	if l.AgentID != "" {
		r.subagent = l.AgentID
	}
	if l.Cwd != "" {
		r.cwd = l.Cwd
	}
}

// line is a line's events, and false when the line does not parse: it is not
// JSON, or not a JSON object whose fields have the types Claude Code writes.
func (r *claudeReader) line(raw []byte) ([]Event, bool) {
	var l claudeLine
	if json.Unmarshal(raw, &l) != nil {
		return nil, false
	}
	r.note(l)
	at := ownerwords.JSONTime(l.Timestamp)
	switch l.Type {
	case "assistant":
		return r.assistant(l, at), true
	case "user":
		return r.user(raw, l, at), true
	case "attachment":
		return r.attachment(raw, l, at), true
	case "system":
		// A system line is a record of the harness's (a compaction's boundary
		// among them; its summary is the compaction event): what came before
		// the next turn.
		r.prior = l.Subtype
	}
	return nil, true
}

func (r *claudeReader) assistant(l claudeLine, at time.Time) []Event {
	model := l.Message.Model
	if model != "" && model != synthetic {
		r.model = model
	}
	var out []Event
	for _, b := range claudeBlocks(l.Message.Content) {
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) == "" {
				continue
			}
			if model == synthetic {
				// Claude Code wrote it in the model's place: an API error or
				// an interruption.
				why := "synthetic message"
				if l.IsAPIError {
					why = "api error"
				}
				e := r.text(KindHarnessText, "system", digest.Harness, why, b.Text, at)
				e.IsError = l.IsAPIError
				out = append(out, e)
				continue
			}
			out = append(out, r.text(KindAgentText, "assistant", digest.Agent, whyAssistant, b.Text, at))
		case "tool_use":
			r.calls[b.ID] = b.Name
			e := r.event(KindToolCall, "assistant", digest.Agent, whyToolCall, at)
			e.ToolID, e.ToolName, e.ToolInput = b.ID, b.Name, string(b.Input)
			out = append(out, e)
		}
	}
	r.prior = ""
	return out
}

// askUser is Claude Code's tool for asking the person questions: its result
// holds their answers.
const askUser = "AskUserQuestion"

func (r *claudeReader) user(raw []byte, l claudeLine, at time.Time) []Event {
	var out []Event
	var texts []string
	for _, b := range claudeBlocks(l.Message.Content) {
		switch b.Type {
		case "tool_result":
			e := r.event(KindToolResult, "tool", digest.Harness, whyToolResult, at)
			e.ToolID, e.ToolName, e.ToolOutput, e.IsError = b.ToolUseID, r.calls[b.ToolUseID], claudeResult(b.Content), b.IsError
			if e.ToolName == askUser {
				e.InputKind = InputAnswer
			}
			out = append(out, e)
		case "text":
			texts = append(texts, b.Text)
		}
	}
	if len(texts) > 0 {
		out = append(out, r.turn(raw, l, texts, at)...)
	}
	if !l.IsMeta && !l.IsCompact {
		// A meta line and a compaction's summary are the harness's record,
		// not an item of the conversation: what came before them still is.
		r.prior = ""
	}
	return out
}

// turn is the events of a user line's texts, by who wrote them: the session
// (digest.SessionWho), the line's marks (digest.ClaudeTyped) and its pieces
// (typed).
func (r *claudeReader) turn(raw []byte, l claudeLine, texts []string, at time.Time) []Event {
	// Both follow every user line in order: a local command's caveat comes
	// on the line before its echo.
	was := r.cmds
	local := r.cmds.LocalLine(raw)
	if r.cmds != was {
		r.replay = append(r.replay, string(raw))
	}
	var own, cut []string
	if r.verdict.Owner() {
		own, _, cut = r.turns.PartsCut(digest.Claude, raw)
	}
	switch {
	case l.IsCompact:
		return []Event{r.text(KindCompaction, "user", digest.Harness, "compaction summary", strings.Join(texts, "\n"), at)}
	case local:
		return r.untyped(digest.Claude, texts, digest.Harness, "local command", at, InputSlash)
	case l.IsMeta:
		return r.untyped(digest.Claude, texts, digest.Harness, "meta line", at, "")
	case !r.verdict.Owner():
		return r.untyped(digest.Claude, texts, digest.Agent, r.verdict.Why, at, "")
	}
	var o map[string]any
	_ = json.Unmarshal(raw, &o)
	marks := digest.ClaudeMarksOf(o)
	if typed, why := digest.ClaudeTyped(marks); !typed {
		in := InputKind("")
		if marks.PromptSource == "suggestion_accepted" {
			in = InputSuggestion
		}
		return r.untyped(digest.Claude, texts, digest.Agent, why, at, in)
	}
	return r.typed(texts, own, cut, nil, at, claudeInput(l.PromptSource, texts))
}

// claudeInput is how a turn a person typed was put in: queued while the agent
// worked, a slash command's echo, a !command and what it printed, or typed.
func claudeInput(promptSource string, texts []string) InputKind {
	if promptSource == "queued" {
		return InputQueued
	}
	for _, t := range texts {
		t = strings.TrimSpace(t)
		switch {
		case strings.Contains(t, "<command-name>"):
			return InputSlash
		case strings.HasPrefix(t, "<bash-input>"), strings.HasPrefix(t, "<bash-stdout>"), strings.HasPrefix(t, "<bash-stderr>"):
			return InputShell
		}
	}
	return InputTyped
}

func (r *claudeReader) attachment(raw []byte, l claudeLine, at time.Time) []Event {
	if texts, typed, why, ok := digest.ClaudeQueuedLine(raw); ok {
		// A message delivered while the agent worked, in its place in the
		// turn (digest.ClaudeQueued).
		var own, cut []string
		if r.verdict.Owner() {
			own, _, cut = r.turns.PartsCut(digest.Claude, raw)
		}
		var out []Event
		switch {
		case !r.verdict.Owner():
			out = r.untyped(digest.Claude, texts, digest.Agent, r.verdict.Why, at, "")
		case !typed && l.Attachment.CommandMode != "" && l.Attachment.CommandMode != "prompt":
			// Not a message at all: the harness delivering its own (a task
			// notification), as digest.ClaudeQueued holds.
			out = r.untyped(digest.Claude, texts, digest.Harness, why, at, "")
		case !typed:
			out = r.untyped(digest.Claude, texts, digest.Agent, why, at, "")
		default:
			out = r.typed(texts, own, cut, nil, at, InputQueued)
		}
		r.prior = ""
		return out
	}
	// A hook's output that reached the agent: Claude Code records what it
	// added to the context as the attachment's content.
	if strings.HasPrefix(l.Attachment.Type, "hook_") {
		if txt := rawText(l.Attachment.Content); strings.TrimSpace(txt) != "" {
			e := r.text(KindHarnessText, "system", digest.Harness, "hook "+l.Attachment.HookEvent, txt, at)
			e.ToolID = l.Attachment.ToolUseID
			return []Event{e}
		}
	}
	return nil
}

// claudeResult is a tool result's content as the agent got it: a string, or
// its text blocks ("[image]" for an image).
func claudeResult(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var bs []claudeBlock
	_ = json.Unmarshal(raw, &bs)
	var parts []string
	for _, b := range bs {
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		case "image":
			parts = append(parts, "[image]")
		}
	}
	return strings.Join(parts, "\n")
}

// rawText is a JSON string, or the strings and text blocks of a list, joined
// by newlines.
func rawText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return ""
	}
	var parts []string
	for _, it := range items {
		var t string
		var b struct {
			Text string `json:"text"`
		}
		switch {
		case json.Unmarshal(it, &t) == nil:
		case json.Unmarshal(it, &b) == nil:
			t = b.Text
		}
		if t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n")
}
