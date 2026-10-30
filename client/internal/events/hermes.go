package events

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/hermes"
	"github.com/agent-sh/tacit/client/internal/ownerwords"
)

// hermesPage is how much content one query of a session's rows reads, as a
// dream run reads per session: a session is read in pages of it, so its
// rows are never all held at once.
const hermesPage = 4 << 20

// ReadHermes reads the events of a Hermes session after message id after (0:
// from its start), calling emit for each in order. s must be a row of
// db.Sessions, which decides whether it is the owner's (hermes.Session.Owner).
// It returns the id of the last message read, where the next read resumes;
// the Offset and Next of a row's events are its message id. As with ReadFile,
// a read after an id gives exactly the events a read from the start gives
// after it: the rows before it are read for what later rows depend on (the
// call a result names, the reply before a turn), their events not emitted.
// When emit returns an error ReadHermes stops and returns it, with the id
// before that row's. Rows Hermes wrote again when it compacted a conversation
// (hermes.Message.Copy) are passed over, as the dreamer passes them.
//
// Who wrote a row: a session of the owner's holds their words in its user
// rows, cut out of the gateway's frames as the corpus reads them
// (ownerwords.TurnReader.Hermes); every other session's user rows are an
// agent's or the gateway's.
func ReadHermes(ctx context.Context, db *hermes.DB, s hermes.Session, after int64, emit func(Event) error) (int64, error) {
	r := &hermesReader{base: newBase("hermes", s.ID)}
	r.parent, r.cwd, r.model = s.Parent, s.Cwd, s.Model
	if r.cwd == "" {
		r.cwd = s.RepoRoot
	}
	if s.Delegated || s.Source == "subagent" || s.CreatedSource == "subagent" {
		r.subagent = s.ID
	}
	r.verdict = digest.Verdict{Who: digest.Agent, Why: "hermes session from " + s.Source}
	if s.Owner {
		r.verdict = digest.Verdict{Who: digest.Owner, Why: "hermes owner session"}
		ps, err := ownerPastes()
		if err != nil {
			return after, err
		}
		r.turns.Pastes = ps
	}
	last, read := after, int64(0)
	for {
		rows, err := db.Messages(ctx, s.ID, read, hermesPage)
		if err != nil {
			return last, err
		}
		if len(rows) == 0 {
			return last, nil
		}
		for _, m := range rows {
			evs := r.row(m)
			read = m.ID
			if m.ID <= after {
				continue
			}
			for i := range evs {
				e := &evs[i]
				e.Path, e.Offset, e.Next, e.Index = db.Path, m.ID, m.ID, i
				if err := emit(*e); err != nil {
					return last, err
				}
			}
			last = m.ID
		}
	}
}

type hermesReader struct {
	base
	turns ownerwords.TurnReader
}

func (r *hermesReader) row(m hermes.Message) []Event {
	if m.Copy {
		return nil
	}
	var at time.Time
	if m.Timestamp > 0 {
		sec, frac := math.Modf(m.Timestamp)
		at = time.Unix(int64(sec), int64(frac*1e9))
	}
	if m.Summary && (m.Role == "user" || m.Role == "assistant") {
		r.prior = ""
		return []Event{r.text(KindCompaction, m.Role, digest.Harness, "compaction summary", m.Content, at)}
	}
	var out []Event
	switch m.Role {
	case "user":
		if strings.TrimSpace(m.Content) != "" {
			out = r.user(m, at)
		}
		r.prior = ""
	case "assistant":
		if strings.TrimSpace(m.Content) != "" {
			e := r.text(KindAgentText, "assistant", digest.Agent, whyAssistant, m.Content, at)
			e.IsError = m.DisplayKind == "failed_turn"
			out = append(out, e)
		}
		var calls []struct {
			ID       string `json:"id"`
			CallID   string `json:"call_id"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		}
		if json.Unmarshal([]byte(m.ToolCalls), &calls) == nil {
			for _, c := range calls {
				id := c.ID
				if id == "" {
					id = c.CallID
				}
				r.calls[id] = c.Function.Name
				e := r.event(KindToolCall, "assistant", digest.Agent, whyToolCall, at)
				e.ToolID, e.ToolName, e.ToolInput = id, c.Function.Name, c.Function.Arguments
				out = append(out, e)
			}
		}
		r.prior = m.Finish
	case "tool":
		name := m.ToolName
		if name == "" {
			name = r.calls[m.ToolCallID]
		}
		e := r.event(KindToolResult, "tool", digest.Harness, whyToolResult, at)
		e.ToolID, e.ToolName, e.ToolOutput = m.ToolCallID, name, m.Content
		_, e.IsError = digest.HermesToolError(m.Content)
		out = append(out, e)
		r.prior = ""
	}
	return out
}

func (r *hermesReader) user(m hermes.Message, at time.Time) []Event {
	text, injected := digest.HermesOwnerText(m.Content, m.DisplayKind, m.Summary)
	if injected {
		why := "hermes notice"
		if m.DisplayKind != "" {
			why += " " + m.DisplayKind
		}
		e := r.text(KindHarnessText, "user", digest.Harness, why, m.Content, at)
		e.Prior = r.prior
		return []Event{e}
	}
	if !r.verdict.Owner() {
		return r.untyped(digest.Hermes, []string{m.Content}, digest.Agent, r.verdict.Why, at, "")
	}
	in := InputTyped
	if m.DisplayKind == "steer" {
		in = InputQueued
	}
	// Their words as the corpus reads the row, out of the gateway's frames (a
	// quoted reply, the origin data, attached context, a paste's reference),
	// which are in no event: the row holds them.
	var own []string
	t, cut, ok := r.turns.HermesCut(m.Content, m.DisplayKind, m.Summary)
	if ok {
		own = []string{t}
	}
	return r.typed([]string{text}, own, cut, nil, at, in)
}
