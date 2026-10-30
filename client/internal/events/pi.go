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

// pi writes one JSONL file per session, <sessions>/--<cwd>--/<time>_<id>.jsonl
// (digest.PiSessionGlob): a header ({"type":"session","id","cwd",
// "parentSession"}), then entries. A "message" entry holds a message by
// role: user, assistant (text, thinking and toolCall blocks, its model and
// stop reason, errorMessage when the provider failed), toolResult
// (toolCallId, isError), bashExecution (a !command the person ran, with its
// exit code), custom and summaries. Other entries: model_change,
// compaction (a summary), branch_summary, custom_message (an extension's
// text in the context). A subagent's session is a task, one level down:
// <session>/tasks/<time>_<id>.jsonl, its header naming the parent.
type piReader struct {
	base
	turns ownerwords.TurnReader
	// working is the agent mid-turn: its last reply asked for tools, or a
	// tool result came after it. A user message then was steered in.
	working bool
}

func newPi(plain string, head []byte, v digest.Verdict, pastes []string) *piReader {
	r := &piReader{base: newBase("pi", transcript.Name(plain))}
	r.verdict = v
	r.turns.Pastes = pastes
	h, ok := digest.ReadPiHeader(head)
	if ok {
		if h.ID != "" {
			r.session = h.ID
		}
		r.cwd = h.Cwd
		r.parent = piSessionID(h.ParentSession)
	}
	if digest.PiTask(plain) {
		if r.parent == "" {
			// <session>/tasks/<task>.jsonl: the folder is named after the
			// parent's file.
			r.parent = piSessionID(filepath.Base(filepath.Dir(filepath.Dir(plain))))
		}
		r.subagent = r.session
	} else if ok && h.ParentSession != "" {
		r.subagent = r.session
	}
	return r
}

// piSessionID is a session's id from its file's path or name
// (<time>_<id>.jsonl), or the value as it is when it names neither.
func piSessionID(p string) string {
	if p == "" {
		return ""
	}
	name := transcript.Name(p)
	if _, id, ok := strings.Cut(name, "_"); ok && id != "" {
		return id
	}
	return name
}

type piLine struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Timestamp json.RawMessage `json:"timestamp"`
	Summary   string          `json:"summary"`
	Content   json.RawMessage `json:"content"`
	ModelID   string          `json:"modelId"`
	Message   json.RawMessage `json:"message"`
}

type piMessage struct {
	Role         string          `json:"role"`
	Content      json.RawMessage `json:"content"`
	Model        string          `json:"model"`
	StopReason   string          `json:"stopReason"`
	ErrorMessage string          `json:"errorMessage"`
	ToolCallID   string          `json:"toolCallId"`
	ToolName     string          `json:"toolName"`
	IsError      bool            `json:"isError"`
	Command      string          `json:"command"`
	Output       string          `json:"output"`
	ExitCode     *float64        `json:"exitCode"`
	Summary      string          `json:"summary"`
}

type piBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// piTexts are a content's text blocks; a string content is one.
func piTexts(raw json.RawMessage) []string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []string{s}
	}
	var bs []piBlock
	_ = json.Unmarshal(raw, &bs)
	var out []string
	for _, b := range bs {
		if b.Type == "text" && b.Text != "" {
			out = append(out, b.Text)
		}
	}
	return out
}

// line is a line's events, and false when the line does not parse: it is not
// JSON, not a JSON object of an entry's fields, or a message entry whose
// message does not decode.
func (r *piReader) line(raw []byte) ([]Event, bool) {
	var l piLine
	if json.Unmarshal(raw, &l) != nil {
		return nil, false
	}
	at := ownerwords.JSONTime(l.Timestamp)
	switch l.Type {
	case "model_change":
		if l.ModelID != "" {
			r.model = l.ModelID
		}
	case "compaction":
		r.prior = "compaction"
		return []Event{r.text(KindCompaction, "system", digest.Harness, "compaction", l.Summary, at)}, true
	case "branch_summary":
		r.prior = "branch_summary"
		if strings.TrimSpace(l.Summary) != "" {
			return []Event{r.text(KindHarnessText, "system", digest.Harness, "branch summary", l.Summary, at)}, true
		}
	case "custom_message":
		if t := strings.Join(piTexts(l.Content), "\n"); strings.TrimSpace(t) != "" {
			return []Event{r.text(KindHarnessText, "user", digest.Harness, "extension message", t, at)}, true
		}
	case "message":
		var m piMessage
		if json.Unmarshal(l.Message, &m) != nil {
			return nil, false
		}
		return r.message(raw, l.ID, m, at), true
	}
	return nil, true
}

func (r *piReader) message(raw []byte, entry string, m piMessage, at time.Time) []Event {
	var out []Event
	switch m.Role {
	case "user":
		texts := piTexts(m.Content)
		if r.verdict.Owner() {
			own, _, cut := r.turns.PartsCut(digest.Pi, raw)
			in := InputTyped
			if r.working {
				in = InputQueued // steered into the agent's turn
			}
			out = r.typed(texts, own, cut, nil, at, in)
		} else {
			out = r.untyped(digest.Pi, texts, digest.Agent, r.verdict.Why, at, "")
		}
		r.working = false
		r.prior = ""
	case "assistant":
		if m.Model != "" {
			r.model = m.Model
		}
		var bs []piBlock
		_ = json.Unmarshal(m.Content, &bs)
		for _, b := range bs {
			switch b.Type {
			case "text":
				if strings.TrimSpace(b.Text) != "" {
					out = append(out, r.text(KindAgentText, "assistant", digest.Agent, whyAssistant, b.Text, at))
				}
			case "toolCall":
				r.calls[b.ID] = b.Name
				e := r.event(KindToolCall, "assistant", digest.Agent, whyToolCall, at)
				e.ToolID, e.ToolName, e.ToolInput = b.ID, b.Name, string(b.Arguments)
				out = append(out, e)
			}
		}
		if strings.TrimSpace(m.ErrorMessage) != "" {
			e := r.text(KindHarnessText, "system", digest.Harness, "provider error", m.ErrorMessage, at)
			e.IsError = true
			out = append(out, e)
		}
		r.working = m.StopReason == "toolUse"
		r.prior = m.StopReason
	case "toolResult":
		name := m.ToolName
		if name == "" {
			name = r.calls[m.ToolCallID]
		}
		e := r.event(KindToolResult, "tool", digest.Harness, whyToolResult, at)
		e.ToolID, e.ToolName, e.ToolOutput, e.IsError = m.ToolCallID, name, strings.Join(piTexts(m.Content), "\n"), m.IsError
		out = append(out, e)
		r.working = true
		r.prior = ""
	case "bashExecution":
		// A !command the person ran: their call in their session, and what
		// it printed.
		who, why := digest.Agent, r.verdict.Why
		if r.verdict.Owner() {
			who, why = digest.Owner, "shell command the person ran"
		}
		input, _ := json.Marshal(map[string]string{"command": m.Command})
		call := r.event(KindToolCall, "user", who, why, at)
		call.ToolID, call.ToolName, call.ToolInput, call.InputKind, call.Prior = entry, "bash", string(input), InputShell, r.prior
		res := r.event(KindToolResult, "tool", digest.Harness, whyToolResult, at)
		res.ToolID, res.ToolName, res.ToolOutput, res.InputKind = entry, "bash", m.Output, InputShell
		res.IsError = m.ExitCode != nil && *m.ExitCode != 0
		out = append(out, call, res)
		r.prior = ""
	case "custom", "compactionSummary", "branchSummary":
		t := m.Summary
		if t == "" {
			t = strings.Join(piTexts(m.Content), "\n")
		}
		if strings.TrimSpace(t) != "" {
			out = append(out, r.text(KindHarnessText, "user", digest.Harness, m.Role, t, at))
		}
	}
	return out
}
