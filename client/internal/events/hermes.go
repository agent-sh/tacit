package events

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
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
// (hermes.Message.Copy) are passed over, as the dreamer passes them. An
// assistant row whose tool calls do not parse gives its text and no calls;
// ReadHermesState counts such rows (State.Unparsed).
//
// Who wrote a row: a session of the owner's holds their words in its user
// rows, cut out of the gateway's frames as the corpus reads them
// (ownerwords.TurnReader.Hermes); every other session's user rows are an
// agent's or the gateway's. ReadHermesState is ReadHermes without the pass
// over the rows before the cursor.
func ReadHermes(ctx context.Context, db *hermes.DB, s hermes.Session, after int64, emit func(Event) error) (int64, error) {
	st, err := ReadHermesState(ctx, db, s, State{Offset: after}, emit)
	return st.Offset, err
}

// ReadHermesState is ReadHermes from a State, returning the State after the
// last row read. When from holds what the reader kept for this store's
// session (State.Kept) and the row at its cursor is still the one it was kept
// after, the read starts after from's message id: it reads only the rows
// after it, and gives the events ReadHermes gives after it. A bare cursor, or
// a session whose rows Hermes wrote again (its /retry, /undo and /compress
// replace them), is read as ReadHermes reads it, with a pass over the rows
// before the cursor. A State at the session's last row is returned as it is.
// The State returned counts the rows up to its cursor whose tool calls did
// not parse (State.Unparsed), as ReadFileState counts a transcript's lines.
// When emit returns an error the read stops and returns it with the State a
// read from gives that row's events again: from itself when the read started
// after its cursor and the error came at the first event, else a bare cursor
// at the id before the row's.
func ReadHermesState(ctx context.Context, db *hermes.DB, s hermes.Session, from State, emit func(Event) error) (State, error) {
	after := from.Offset
	r := &hermesReader{base: newBase("hermes", s.ID)}
	r.parent, r.cwd, r.model = s.Parent, s.Cwd, s.Model
	if r.cwd == "" {
		r.cwd = s.RepoRoot
	}
	if s.Delegated || s.Source == "subagent" || s.CreatedSource == "subagent" {
		r.subagent = s.ID
	}
	switch last, err := db.LastID(ctx, s.ID); {
	case err != nil:
		return from, err
	case last < after:
		// The store went back (restored from a backup): the cursor names a
		// row it no longer has.
		return from, fmt.Errorf("hermes session %s: %w (%d > %d)", s.ID, ErrPastEnd, after, last)
	case last == after:
		return from, nil // nothing after the cursor: no pass over the session
	}
	r.verdict = digest.Verdict{Who: digest.Agent, Why: "hermes session from " + s.Source}
	if s.Owner {
		r.verdict = digest.Verdict{Who: digest.Owner, Why: "hermes owner session"}
		ps, err := ownerPastes()
		if err != nil {
			return from, err
		}
		r.turns.Pastes = ps
	}
	key := hermes.Key(db.Path, s.ID)
	var read int64
	used := false
	// unparsed is how many rows up to the last one read had tool calls that
	// did not parse: from's count when the read starts after its cursor, else
	// counted again in the pass.
	unparsed := 0
	if k := from.kept; k != nil && after > 0 && from.unparsed >= 0 && k.Format == digest.Hermes && k.Head == key {
		rows, err := db.Messages(ctx, s.ID, after-1, 1)
		if err != nil {
			return from, err
		}
		if len(rows) > 0 && rows[0].ID == after && k.Last != "" && rowID(rows[0]) == k.Last {
			// Only what a row leaves for the rows after it is kept: the
			// session's own fields are its row's, read now as a read from the
			// start reads them.
			r.prior = k.Prior
			r.calls = make(map[string]string, len(k.Calls))
			maps.Copy(r.calls, k.Calls)
			read, used, unparsed = after, true, from.unparsed
		}
	}
	last, emitted := after, 0
	var lastRow hermes.Message
	at := func() State {
		k := &kept{Format: digest.Hermes, Head: key, Prior: r.prior, Calls: r.calls}
		if lastRow.ID == last && last > 0 {
			k.Last, k.LastAt = rowID(lastRow), last
		}
		return State{Offset: last, kept: k, unparsed: unparsed}
	}
	for {
		rows, err := db.Messages(ctx, s.ID, read, hermesPage)
		if err != nil {
			if read < after {
				// The pass did not reach the cursor: the rows it did not read
				// are from's to count.
				return State{Offset: last, unparsed: from.unparsed}, err
			}
			return State{Offset: last, unparsed: unparsed}, err
		}
		if len(rows) == 0 {
			return at(), nil
		}
		for _, m := range rows {
			evs, ok := r.row(m)
			read = m.ID
			if m.ID > after {
				for i := range evs {
					e := &evs[i]
					e.Path, e.Offset, e.Next, e.Index = db.Path, m.ID, m.ID, i
					if err := emit(*e); err != nil {
						if used && emitted == 0 {
							return from, err
						}
						return State{Offset: last, unparsed: unparsed}, err
					}
					emitted++
				}
				last, lastRow = m.ID, m
			}
			if !ok {
				unparsed++
			}
		}
	}
}

type hermesReader struct {
	base
	turns ownerwords.TurnReader
}

// row is a row's events, and false for an assistant row whose tool calls do
// not parse (not JSON, or calls of another shape): its text is read, its
// calls are not. A row with no calls holds none to parse.
func (r *hermesReader) row(m hermes.Message) ([]Event, bool) {
	if m.Copy {
		return nil, true
	}
	var at time.Time
	if m.Timestamp > 0 {
		sec, frac := math.Modf(m.Timestamp)
		at = time.Unix(int64(sec), int64(frac*1e9))
	}
	if m.Summary && (m.Role == "user" || m.Role == "assistant") {
		r.prior = ""
		return []Event{r.text(KindCompaction, m.Role, digest.Harness, "compaction summary", m.Content, at)}, true
	}
	var out []Event
	ok := true
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
		switch err := json.Unmarshal([]byte(m.ToolCalls), &calls); {
		case err == nil:
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
		case strings.TrimSpace(m.ToolCalls) != "":
			ok = false // calls the row holds, and none read
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
	return out, ok
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
