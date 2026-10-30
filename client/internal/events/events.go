// Package events reads the harnesses' session history as one stream of
// structured events: what the owner typed, what agents and harnesses wrote,
// each tool call with its result, and each compaction. It reads Claude Code
// (top-level and subagent transcripts), Codex (plain and compressed
// rollouts), pi (sessions and their tasks) and the Hermes store
// (internal/harnesses lists where).
//
// Who wrote an item is decided by the rules the dreamer and the owner corpus
// use, never a copy of them: per session digest.SessionWho (Hermes:
// hermes.Session.Owner), per Claude Code line digest.ClaudeTyped and
// digest.ClaudeQueued, per piece of a turn digest.TurnPieces, and for the
// owner's own words ownerwords.TurnReader (Parts, Hermes), with the pastes
// the owner's prompt history records (ownerwords.ClaudePastes).
//
// A read resumes from a cursor, or from a saved State, which keeps what the
// reader needs of the items before its cursor, so that following a growing
// session costs only what was added (ReadFileState, ReadHermesState).
//
// Events hold the text as the harness recorded it, unredacted. A caller that
// stores or sends them redacts them first (Redact), which fails closed.
package events

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/harnesses"
	"github.com/agent-sh/tacit/client/internal/ownerwords"
	"github.com/agent-sh/tacit/client/internal/transcript"
)

// Kind is what an event is.
type Kind string

const (
	// KindOwnerText is the owner's own words in a turn they typed, as the
	// owner corpus reads them: what a harness or another agent wrote in the
	// turn, a paste and agent text they relayed left out.
	KindOwnerText Kind = "owner_text"
	// KindAgentText is text an agent wrote: the session agent's reply, a
	// prompt another agent or a program sent, a note an agent put in a turn
	// of the owner's, a paste or relayed agent text in one.
	KindAgentText Kind = "agent_text"
	// KindHarnessText is text a harness put in: a reminder, a frame, a
	// notice, a command echo, a hook's context, a developer message, an
	// error the harness wrote in the agent's place.
	KindHarnessText Kind = "harness_text"
	// KindToolCall is a tool call, KindToolResult its result; ToolID pairs
	// them.
	KindToolCall   Kind = "tool_call"
	KindToolResult Kind = "tool_result"
	// KindCompaction marks a compaction, with its summary as Text when the
	// harness wrote one.
	KindCompaction Kind = "compaction"
)

// InputKind is how a user turn was put in.
type InputKind string

const (
	// InputTyped is a turn typed into the prompt.
	InputTyped InputKind = "typed"
	// InputQueued is a message typed while the agent worked: Claude Code's
	// queued message, a Codex or pi message steered into a running turn, a
	// Hermes /steer.
	InputQueued InputKind = "queued"
	// InputSlash is a slash command: its echo, and its arguments when they
	// go to the agent.
	InputSlash InputKind = "slash"
	// InputShell is a shell command the person ran from the prompt (Claude
	// Code's and pi's !command) and what it printed.
	InputShell InputKind = "shell"
	// InputAnswer is an answer to an agent's question: a Codex question
	// reply, the result of Claude Code's AskUserQuestion.
	InputAnswer InputKind = "answer"
	// InputSuggestion is a suggestion the model wrote and the person
	// accepted: not their words, though they sent it.
	InputSuggestion InputKind = "suggestion"
)

// Event is one item of a session's history.
type Event struct {
	// Harness is claude, codex, pi or hermes.
	Harness string
	// Path is the transcript's plain path (transcript.Plain: the name it has
	// whichever file holds it), or the Hermes store.
	Path string
	// Offset is the first byte of the item's line in the transcript's JSONL
	// bytes (a compressed copy's decompressed stream), or the Hermes message
	// id. Next is where a read resumes after that line: the next line's
	// offset, or the same message id. Index is the event's place among its
	// line's events: (Path, Offset, Index) names an event.
	Offset int64
	Next   int64
	Index  int
	// Session is the session id the harness gives the item: Claude Code's
	// sessionId (a subagent's lines carry their parent session's), the Codex
	// thread, the pi session, the Hermes session. Subagent is the subagent
	// it came from, empty in a session of its own: Claude Code's agentId, a
	// Codex subagent thread's or a pi task's own id. Parent is the session
	// that started this one when the harness records it: a Codex
	// parent_thread_id, a pi parentSession, a Hermes parent session.
	Session  string
	Subagent string
	Parent   string
	// At is when the harness recorded the item; zero when it does not say.
	At time.Time
	// Cwd is the session's working directory as last recorded, Model the
	// model of the agent's last reply (Hermes: the session's model).
	Cwd   string
	Model string

	Kind Kind
	// Role is the side the item is on: user (a turn, whoever wrote it),
	// assistant (the agent's reply and calls), tool (a result), developer
	// (a Codex developer message) or system (what the harness recorded on
	// its own: a hook's context, a compaction, an error message).
	Role string
	// Who wrote it (digest.Owner, Agent, Harness, Unknown), and Why: the
	// reason the rule gave (a session's verdict, a line's mark, a piece).
	Who digest.Who
	Why string
	// InputKind is how the user turn the event came from was put in; empty
	// for an event of no turn a person put in.
	InputKind InputKind
	// Prior is the harness's record of what happened just before a user
	// turn, since the item before it: Claude Code's system line (its
	// subtype: turn_duration, api_error, away_summary, local_command,
	// compact_boundary, ...), the Codex event that ended or broke the turn
	// before (task_complete, turn_aborted, error, stream_error,
	// context_compacted, thread_rolled_back, compacted), pi's stop reason of
	// the agent's reply before it (stop, toolUse, error, aborted, length) or
	// a compaction or branch_summary after it, the finish reason of the
	// Hermes reply before it. Empty when nothing was recorded.
	Prior string
	// Text is a text event's text or a compaction's summary.
	Text string

	// ToolID pairs a tool call with its result. ToolName is the tool (a
	// Codex tool in a namespace <namespace>__<name>, as Claude Code names an
	// MCP server's); ToolInput the call's arguments as recorded (JSON, or a
	// custom tool's raw input), ToolOutput the result as the agent got it.
	ToolID     string
	ToolName   string
	ToolInput  string
	ToolOutput string
	// IsError marks a result the harness recorded as failed (Claude Code's
	// is_error; pi's isError and a !command's exit code; Codex's end of the
	// call before its output, the exit code it writes above a command's
	// output and the first line of its script tool's; a Hermes tool row's
	// error), and an error message the harness wrote in the agent's place.
	IsError bool
}

// ErrPastEnd is a cursor past the end of its transcript: the file was cut
// or replaced since the cursor was saved.
var ErrPastEnd = errors.New("cursor past the end of the transcript")

// headBytes is how much of a transcript decides who its turns come from:
// digest.SessionWho reads the first 256 KiB.
const headBytes = 256 << 10

// lineReader turns one transcript line into events, keeping what it needs
// across lines; keep is what it kept as of the last line it read, restore
// takes that back into a new reader (State).
type lineReader interface {
	line(raw []byte) []Event
	keep() *kept
	restore(k *kept)
}

// ReadFile reads the events of a Claude Code, Codex or pi transcript from
// byte offset from (0: its start; an offset is in the JSONL bytes, a
// compressed copy's decompressed stream), calling emit for each in order. It
// returns the offset after the last line read, where the next read resumes.
// A last line without its newline is read once it holds whole JSON; until
// then it is left for the next read. A transcript the harness compressed
// since its path was recorded is read from its compressed copy
// (transcript.Resolve).
//
// A read from a cursor gives exactly the events a read from the start gives
// from there: what a line's events depend on in the lines before it (the
// call a result names, the options a Codex question offered, a local
// command's caveat, a turn under way, the record before a turn, the model
// and cwd) is rebuilt from those lines, whose events are not emitted. So
// (Path, Offset, Index) names one event however the transcript is read. When
// emit returns an error ReadFile stops and returns it, with the offset of
// the line it stopped in, from which a read gives that line's events again.
// ReadFileState with a saved State starts at its cursor instead, without the
// pass over the lines before it.
func ReadFile(path string, f digest.Format, from int64, emit func(Event) error) (int64, error) {
	st, err := ReadFileState(path, f, State{Offset: from}, 0, emit)
	return st.Offset, err
}

// ReadFileState is ReadFile from a State, returning the State after the last
// line read. When from holds what the reader kept (State.Kept) and the
// transcript still holds the bytes it was kept from (the ones its head hash
// names, and the line before its cursor), the read starts at from's cursor:
// it costs only the lines after it, and gives the events ReadFile gives from
// there. A bare cursor, or a transcript whose bytes changed (a file replaced
// or rewritten), is read as ReadFile reads it, with a pass over the lines
// before the cursor. A State at the transcript's end is returned as it is,
// with no read.
//
// limit > 0 stops the read at the end of the first line that ends at least
// limit bytes past from's cursor, with what the reader kept, so a long
// transcript can be read and saved in parts; 0 reads to the end. Each part
// of a compressed copy is decompressed from its start again. When emit
// returns an error the read stops and returns it with the State a read from
// gives that line's events again: from itself when the error came at the
// first event from it, else a bare cursor at the line.
func ReadFileState(path string, f digest.Format, from State, limit int64, emit func(Event) error) (State, error) {
	plain := transcript.Plain(path)
	if from.Offset < 0 {
		return from, fmt.Errorf("%s: negative offset %d", plain, from.Offset)
	}
	if f != digest.Claude && f != digest.Codex && f != digest.Pi {
		return from, fmt.Errorf("%s: not a transcript format (%d)", plain, f)
	}
	_, size, err := transcript.Stat(path)
	if err != nil {
		return from, err
	}
	if from.Offset > size {
		return from, fmt.Errorf("%s: %w (%d > %d)", plain, ErrPastEnd, from.Offset, size)
	}
	if from.Offset == size {
		return from, nil
	}
	head := transcript.Head(path, headBytes)
	v := digest.SessionWho(plain, head, f)
	var pastes []string
	if v.Owner() {
		if pastes, err = ownerPastes(); err != nil {
			return from, err
		}
	}
	var rd lineReader
	switch f {
	case digest.Claude:
		rd = newClaude(plain, v, pastes)
	case digest.Codex:
		rd = newCodex(plain, head, v, pastes)
	default:
		rd = newPi(plain, head, v, pastes)
	}
	var start int64
	var fh io.ReadCloser
	var br *bufio.Reader
	used := false
	if k := from.kept; k != nil && k.Format == f && k.HeadLen <= int64(len(head)) && hash(head[:k.HeadLen]) == k.Head {
		if fh, br, err = openAfter(path, k, from.Offset); err != nil {
			return from, err
		}
		if br != nil {
			// The lines that left another package's rules where they were,
			// then the rest of what was kept over what they left.
			for _, l := range k.Replay {
				rd.line([]byte(l))
			}
			rd.restore(k)
			start, used = from.Offset, true
		}
	}
	if br == nil {
		if fh, err = transcript.OpenAt(path, 0); err != nil {
			return from, err
		}
		br = bufio.NewReaderSize(fh, 1<<20)
	}
	defer fh.Close()
	// last is the last whole line read and where it starts.
	var last []byte
	lastAt := int64(-1)
	// at is the State after the lines read up to off: what the reader kept,
	// unless the read ended before from's cursor (one inside a line).
	at := func(off int64) State {
		if off < from.Offset {
			return State{Offset: from.Offset}
		}
		k := rd.keep()
		k.Format = f
		k.HeadLen = min(off, int64(len(head)))
		k.Head = hash(head[:k.HeadLen])
		switch {
		case lastAt >= 0 && lastAt+int64(len(last)) == off:
			k.Last, k.LastAt = hash(last), lastAt
		case used && off == from.Offset:
			k.Last, k.LastAt = from.kept.Last, from.kept.LastAt
		}
		return State{Offset: off, kept: k}
	}
	off, emitted := start, 0
	for {
		line, rerr := br.ReadBytes('\n')
		if rerr != nil && rerr != io.EOF {
			return State{Offset: max(off, from.Offset)}, rerr
		}
		if len(line) == 0 {
			return at(off), nil
		}
		body := bytes.TrimRight(line, "\r\n")
		if rerr == io.EOF && !json.Valid(body) {
			return at(off), nil // still being written
		}
		next := off + int64(len(line))
		evs := rd.line(body)
		if off >= from.Offset {
			for i := range evs {
				e := &evs[i]
				e.Path, e.Offset, e.Next, e.Index = plain, off, next, i
				if err := emit(*e); err != nil {
					if used && emitted == 0 {
						return from, err
					}
					return State{Offset: off}, err
				}
				emitted++
			}
		}
		last, lastAt, off = line, off, next
		if rerr == io.EOF || limit > 0 && off-from.Offset >= limit {
			return at(off), nil
		}
	}
}

// openAfter opens a transcript at the line before a kept cursor, reads that
// line and checks it is the one the State was kept after. It returns the
// reader at the cursor, or a nil reader when the line is another.
func openAfter(path string, k *kept, cursor int64) (io.ReadCloser, *bufio.Reader, error) {
	if cursor > 0 && (k.Last == "" || k.LastAt < 0 || k.LastAt >= cursor) {
		return nil, nil, nil
	}
	fh, err := transcript.OpenAt(path, k.LastAt)
	if err != nil {
		return nil, nil, err
	}
	br := bufio.NewReaderSize(fh, 1<<20)
	if cursor == 0 {
		return fh, br, nil
	}
	line := make([]byte, cursor-k.LastAt)
	if _, err := io.ReadFull(br, line); err != nil || hash(line) != k.Last {
		fh.Close()
		if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
			return nil, nil, err
		}
		return nil, nil, nil
	}
	return fh, br, nil
}

// ownerPastes are the texts the owner pasted into their prompts
// (ownerwords.ClaudePastes, the prompt history under the home directory),
// which the corpus cuts from their words and so does the reader.
func ownerPastes() ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return ownerwords.ClaudePastes(home)
}

// Files lists a source's transcripts by their plain paths
// (transcript.Plain), one per session whichever file holds it, sorted: a
// session Codex compresses between two reads keeps its path, which is
// Event.Path, and ReadFile reads its compressed copy. Only regular files
// count. The Hermes store has none: its sessions are rows (ReadHermes).
func Files(src harnesses.Source) ([]string, error) {
	if src.Glob == "" {
		return nil, nil
	}
	files, err := transcript.Glob(src.Glob)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, f := range files {
		if fi, err := os.Lstat(f); err == nil && fi.Mode().IsRegular() {
			out = append(out, transcript.Plain(f))
		}
	}
	sort.Strings(out)
	return out, nil
}

// base is what every reader keeps across a session's items.
type base struct {
	harness, session, subagent, parent, cwd, model string
	verdict                                        digest.Verdict
	// prior is the harness's last record since the last item (Event.Prior).
	prior string
	// calls are the tool calls seen, by id: a result names its tool by them.
	calls map[string]string
}

func newBase(harness, session string) base {
	return base{harness: harness, session: session, calls: map[string]string{}}
}

// event is an event of kind k with the session's fields.
func (b *base) event(k Kind, role string, who digest.Who, why string, at time.Time) Event {
	return Event{Harness: b.harness, Session: b.session, Subagent: b.subagent, Parent: b.parent, At: at, Cwd: b.cwd, Model: b.model,
		Kind: k, Role: role, Who: who, Why: why}
}

// text is a text event.
func (b *base) text(k Kind, role string, who digest.Who, why, text string, at time.Time) Event {
	e := b.event(k, role, who, why, at)
	e.Text = text
	return e
}

// Why an item is the harness's or an agent's, where no rule's reason says.
const (
	whyAssistant   = "assistant"
	whyToolCall    = "tool call"
	whyToolResult  = "tool result"
	whyHarnessText = "harness text"
	whyAgentInTurn = "agent text in the owner's turn"
	whyLeftOut     = "cut from the owner's turn"
)

// untyped is the events of the texts of a user turn no person typed (an
// agent's session, or a line its marks say a person did not type), whole, by
// the digest's rule for such a turn (digest.UntypedWho): text a harness
// injected is the harness's, the rest who's (an agent's, or unknown in a
// session no rule decides). who Harness makes all of it the harness's.
func (b *base) untyped(f digest.Format, texts []string, who digest.Who, why string, at time.Time, in InputKind) []Event {
	var out []Event
	for _, t := range texts {
		if strings.TrimSpace(t) == "" {
			continue
		}
		k, w := KindAgentText, who
		if w == digest.Harness || digest.UntypedWho(f, t) == digest.Harness {
			k, w = KindHarnessText, digest.Harness
		}
		e := b.text(k, "user", w, why, t, at)
		e.InputKind, e.Prior = in, b.prior
		out = append(out, e)
	}
	return out
}

// whyOffered is why an answer to a Codex question is the harness's: it is one
// of the options the question offered (digest.CodexAsked.Offered), the
// model's words the person chose.
const whyOffered = "an option the question offered"

// typed is the events of the texts of a user turn a person typed: each piece
// as digest.TurnPieces splits it (an answer to a Codex question that is one
// of the options it offered is the harness's, its text as recorded:
// digest.CodexAsked.Offered), and the owner's words as the owner corpus
// reads the turn (own, ownerwords.TurnReader) in the place of the first piece
// of theirs, with what the corpus cut from their pieces (cut: a paste, agent
// text they relayed, each as it was) as an agent's after it. A piece that
// answers a Codex question makes the turn an answer.
func (b *base) typed(texts, own, cut []string, asked digest.CodexAsked, at time.Time, in InputKind) []Event {
	var out []Event
	var mine []string
	slot := -1
	for _, t := range texts {
		for _, p := range digest.TurnPieces(t) {
			if strings.TrimSpace(p.Text) == "" {
				continue
			}
			if p.Item != "" {
				in = InputAnswer
			}
			switch {
			case asked.OfferedPiece(p):
				out = append(out, b.text(KindHarnessText, "user", digest.Harness, whyOffered, p.Text, at))
			case p.Who == digest.Owner:
				mine = append(mine, p.Text)
				if slot < 0 {
					slot = len(out)
					out = append(out, Event{})
				}
			case p.Who == digest.Agent:
				out = append(out, b.text(KindAgentText, "user", digest.Agent, whyAgentInTurn, p.Text, at))
			default:
				out = append(out, b.text(KindHarnessText, "user", digest.Harness, whyHarnessText, p.Text, at))
			}
		}
	}
	var place []Event
	if len(own) > 0 {
		place = append(place, b.text(KindOwnerText, "user", digest.Owner, b.verdict.Why, strings.Join(own, "\n"), at))
	}
	if left := strings.TrimSpace(strings.Join(cut, "\n")); left != "" {
		place = append(place, b.text(KindAgentText, "user", digest.Agent, whyLeftOut, strings.Join(cut, "\n"), at))
	}
	switch {
	case slot < 0:
		// A paste the corpus cut out first made the turn open with their words.
		out = append(out, place...)
	case len(place) == 0:
		// Pieces the corpus finds nothing of theirs in.
		out[slot] = b.text(KindAgentText, "user", digest.Agent, whyLeftOut, strings.Join(mine, "\n"), at)
	default:
		out = append(out[:slot], append(place, out[slot+1:]...)...)
	}
	for i := range out {
		out[i].InputKind, out[i].Prior = in, b.prior
	}
	return out
}
