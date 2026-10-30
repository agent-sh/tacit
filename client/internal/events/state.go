package events

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"

	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/hermes"
)

// State is a saved place in a session's history: the cursor a read resumes
// from (Offset) and what the reader kept from the items before it. For a
// transcript that is the session's ids, cwd and model, the calls a later
// result names, the record before the next turn, a Codex turn under way and
// the calls that ended in an error before their output, pi's agent mid-turn,
// and the lines that changed a local command's caveat or a Codex question's
// options; for a Hermes session the record before the next turn and the
// calls (its other fields are its row's).
//
// A read from a State that kept them gives exactly the events a read from
// the start gives from its cursor, without reading the items before the
// cursor again, as long as those items are the ones it was kept from: a read
// checks the transcript's bytes the State names (its first 256 KiB, or all
// of it up to the cursor when shorter) and the line before the cursor, or
// every field of the Hermes row at the cursor, and reads as from a bare
// cursor when they changed. A change further inside the transcript, or to a
// Hermes row before the one at the cursor, is not seen.
//
// The zero State is the start, and a read that ends there (a first line
// still being written) returns it: what the readers take from a transcript's
// first line comes from its head. A read that ends past the head it began
// with (the transcript grew during the read) returns a bare cursor, for the
// same reason. State{Offset: n} is a bare cursor: a read from it passes over
// the items before n to rebuild what they leave, as ReadFile does.
// MarshalBinary and UnmarshalBinary save a State and load it back, as do
// MarshalJSON and UnmarshalJSON inside a caller's JSON.
//
// A State a read returns counts the lines before its cursor that did not
// parse (Unparsed), from the session's start, however the read came there.
//
// A State is as private as its transcript: it holds whole lines of it
// (Replay), the working directory and tool names, unredacted. Keep it where
// the transcript may be kept, and never send it.
type State struct {
	// Offset is where the next read resumes: a byte offset in a transcript's
	// JSONL bytes (a compressed copy's decompressed stream), or the id of the
	// last Hermes message read.
	Offset   int64
	kept     *kept
	unparsed int
}

// Kept reports whether s holds what the reader kept: a read from it starts
// at its cursor when the format, and the transcript's bytes or the Hermes
// row it names, are still the ones it was kept from.
func (s State) Kept() bool { return s.kept != nil }

// Unparsed is how many lines before the cursor did not parse: lines whose
// record the reader could not decode, or whose message held no text it could
// take, so it gave none of the events they hold. A line is one when it is not
// JSON (a line a crash cut short, which the next write joined), when what the
// reader reads of its kind does not decode (not an object, a field its kind
// reads of another type: a Claude Code attachment's by its type, a Codex
// payload's by its type, an end event's word on its call's failure among
// them, a pi message's by its role), or when a message's content (a queued
// message's prompt, a hook's output) is neither a string nor a list. A field
// of a kind the reader passes over, or one the line's kind does not read,
// counts nothing. What a counted line left still holds: a Codex compacted
// entry marks its compaction, what decoded of a Codex event_msg is taken (a
// failure only under a call id that decoded), a Codex question's options are
// noted (digest.CodexAsked), a line whose content is of another shape gives
// its session's fields, and a pi reply its provider error. A line that holds
// nothing (blank, or null) is not one, nor a last line still being written,
// nor a part inside a message's content (a block, a tool's output). For a
// Hermes session it is how many of the rows up to the cursor are assistant
// rows the reader reads (not a compaction copy or a summary) whose tool calls
// did not parse: their text was read, their calls were not.
//
// The count is from the session's start however the read came there: a read
// that starts at a State's cursor (Kept, its items unchanged) adds the lines
// it reads to its count, and any other read counts the lines before the
// cursor in its pass over them. So a session read in parts ends with the
// count one read from the start gives, unless a harness wrote its next record
// on the same line as one a read took whole before its newline: that read
// gave the line's events, and a read from the start counts the joined line
// instead. A State a read returned is returned as it is at the end, with its
// count; a bare cursor at the end is read with the pass, so the State
// returned holds the count. A bare cursor the caller makes counts none, and
// one a read returns after an I/O or store error that came before the read
// reached from's cursor holds from's count.
func (s State) Unparsed() int { return s.unparsed }

// keptVersion is the format of what a State keeps. Bump it when what a
// reader keeps or replays, or what a State counts, changes
// (TestStateCoversEveryField lists them): a State saved in another format
// loads as a bare cursor, and a read from it rebuilds what it kept and counts
// again. Version 2 added the count of lines that did not parse.
const keptVersion = 2

// kept is what a reader keeps across a session's items, as of a cursor.
type kept struct {
	Format digest.Format `json:"format"`
	// Head names what the reader was made from. A transcript: a hash of its
	// first HeadLen bytes, the cursor's offset or headBytes when that is
	// less, so a State of a session shorter than headBytes names every byte
	// before its cursor (a read that ends past the head it began with keeps
	// nothing). Hermes: the session's cursor key (hermes.Key).
	Head    string `json:"head"`
	HeadLen int64  `json:"head_len,omitempty"`
	// Last names the item before the cursor: a hash of the transcript's line
	// from LastAt to the cursor, or of the Hermes row at the cursor. A read
	// checks it, so a transcript rewritten past its head, or a session whose
	// rows Hermes wrote again, reads as from a bare cursor.
	Last   string `json:"last,omitempty"`
	LastAt int64  `json:"last_at,omitempty"`

	Session  string            `json:"session,omitempty"`
	Subagent string            `json:"subagent,omitempty"`
	Parent   string            `json:"parent,omitempty"`
	Cwd      string            `json:"cwd,omitempty"`
	Model    string            `json:"model,omitempty"`
	Prior    string            `json:"prior,omitempty"`
	Calls    map[string]string `json:"calls,omitempty"`

	// Replay are the lines that changed what a rule of another package
	// follows across a transcript's lines, in order: Claude Code's local
	// commands (digest.ClaudeCommands, in the reader and in the owner's
	// words' own TurnReader) and the options a Codex question offered
	// (digest.CodexAsked, in both). A new reader reads them again, which
	// leaves those rules as the lines before the cursor did, and then takes
	// the rest of what is kept here.
	Replay []string `json:"replay,omitempty"`

	// Codex: the calls whose end event reported a failure before their
	// output, and a turn under way.
	Failed map[string]bool `json:"failed,omitempty"`
	Open   bool            `json:"open,omitempty"`
	Active bool            `json:"active,omitempty"`
	// pi: the agent mid-turn.
	Working bool `json:"working,omitempty"`
}

// stateWire is a saved State. Unparsed is read only in keptVersion, so a State
// of another format that holds something else there loads as a bare cursor.
type stateWire struct {
	V        int             `json:"v"`
	Offset   int64           `json:"offset"`
	Unparsed json.RawMessage `json:"unparsed,omitempty"`
	Kept     json.RawMessage `json:"kept,omitempty"`
}

// MarshalBinary is the State as bytes to save (JSON).
func (s State) MarshalBinary() ([]byte, error) {
	w := stateWire{V: keptVersion, Offset: s.Offset}
	if s.unparsed != 0 {
		w.Unparsed = json.RawMessage(strconv.Itoa(s.unparsed))
	}
	if s.kept != nil {
		k, err := json.Marshal(s.kept)
		if err != nil {
			return nil, err
		}
		w.Kept = k
	}
	return json.Marshal(w)
}

// UnmarshalBinary loads a State MarshalBinary saved, its count with it. A
// State saved in another format, or whose count is not a whole number of 0
// or more (a damaged one), loads as a bare cursor (State{Offset: n}) with no
// count, which a read counts again; bytes that are no State are an error.
func (s *State) UnmarshalBinary(b []byte) error {
	var w stateWire
	if err := json.Unmarshal(b, &w); err != nil {
		return fmt.Errorf("events state: %w", err)
	}
	*s = State{Offset: w.Offset}
	if w.V != keptVersion {
		return nil
	}
	if len(w.Unparsed) > 0 {
		// MarshalBinary writes a whole number or nothing: null is damage too,
		// though it decodes as 0.
		var n int
		if string(w.Unparsed) == "null" || json.Unmarshal(w.Unparsed, &n) != nil || n < 0 {
			return nil
		}
		s.unparsed = n
	}
	if len(w.Kept) == 0 || string(w.Kept) == "null" {
		return nil
	}
	var k kept
	if err := json.Unmarshal(w.Kept, &k); err != nil {
		return fmt.Errorf("events state: %w", err)
	}
	s.kept = &k
	return nil
}

// MarshalJSON is MarshalBinary, so a State inside a caller's JSON keeps what
// it kept.
func (s State) MarshalJSON() ([]byte, error) { return s.MarshalBinary() }

// UnmarshalJSON is UnmarshalBinary.
func (s *State) UnmarshalJSON(b []byte) error { return s.UnmarshalBinary(b) }

// hash names bytes (kept.Head, kept.Last).
func hash(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:16])
}

// rowID names a Hermes row by every field the reader reads of it (kept.Last):
// what it holds, and what it leaves for the rows after it (the finish reason
// a turn's prior is, a summary's mark). A row that does not encode names
// nothing, and no State is used after it.
func rowID(m hermes.Message) string {
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return hash(b)
}

// keep is what every reader keeps. The calls are the reader's own map: a
// reader is not used after the read that returns its State.
func (b *base) keep() *kept {
	return &kept{Session: b.session, Subagent: b.subagent, Parent: b.parent, Cwd: b.cwd, Model: b.model, Prior: b.prior, Calls: b.calls}
}

// restore takes back what keep kept, copying it: a State can be read from
// more than once.
func (b *base) restore(k *kept) {
	b.session, b.subagent, b.parent, b.cwd, b.model, b.prior = k.Session, k.Subagent, k.Parent, k.Cwd, k.Model, k.Prior
	b.calls = make(map[string]string, len(k.Calls))
	maps.Copy(b.calls, k.Calls)
}

func (r *claudeReader) keep() *kept {
	k := r.base.keep()
	k.Replay = r.replay
	return k
}

func (r *claudeReader) restore(k *kept) {
	r.base.restore(k)
	r.replay = slices.Clone(k.Replay)
}

func (r *codexReader) keep() *kept {
	k := r.base.keep()
	k.Replay, k.Failed, k.Open, k.Active = r.replay, r.failed, r.open, r.active
	return k
}

func (r *codexReader) restore(k *kept) {
	r.base.restore(k)
	r.replay = slices.Clone(k.Replay)
	r.failed = make(map[string]bool, len(k.Failed))
	maps.Copy(r.failed, k.Failed)
	r.open, r.active = k.Open, k.Active
}

func (r *piReader) keep() *kept {
	k := r.base.keep()
	k.Working = r.working
	return k
}

func (r *piReader) restore(k *kept) {
	r.base.restore(k)
	r.working = k.Working
}
