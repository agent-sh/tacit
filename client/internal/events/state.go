package events

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/agent-sh/tacit/client/internal/digest"
)

// State is a saved place in a session's history: the cursor a read resumes
// from (Offset) and what the reader kept from the items before it (the
// session's ids, cwd and model, the calls a later result names, the record
// before the next turn, a turn under way, the calls that ended in an error
// before their output, the local commands Claude Code ran, the options a
// Codex question offered). A read from a State gives exactly the events a
// read from the start gives from its cursor, without reading the items
// before the cursor again, so following a growing session costs only what
// was added to it.
//
// The zero State is the start. State{Offset: n} is a bare cursor: a read from
// it passes over the items before n to rebuild what they leave, as ReadFile
// does. MarshalBinary and UnmarshalBinary save a State and load it back.
type State struct {
	// Offset is where the next read resumes: a byte offset in a transcript's
	// JSONL bytes (a compressed copy's decompressed stream), or the id of the
	// last Hermes message read.
	Offset int64
	kept   *kept
}

// Kept reports whether s holds what the reader kept, so that a read from it
// starts at its cursor.
func (s State) Kept() bool { return s.kept != nil }

// keptVersion is the format of what a State keeps. A State saved in another
// format loads as a bare cursor, and a read from it rebuilds what it kept.
const keptVersion = 1

// kept is what a reader keeps across a session's items, as of a cursor.
type kept struct {
	Format digest.Format `json:"format"`
	// Head names what the reader was made from. A transcript: a hash of its
	// first headBytes, which decide who wrote its turns (digest.SessionWho)
	// and hold its first line (Codex's session_meta, pi's header). A State
	// whose transcript has another head now (a session shorter than that
	// when it was saved, a file replaced) is read as a bare cursor. Hermes:
	// the store's cursor key for the session (hermes.Key).
	Head string `json:"head"`

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

type stateWire struct {
	V      int             `json:"v"`
	Offset int64           `json:"offset"`
	Kept   json.RawMessage `json:"kept,omitempty"`
}

// MarshalBinary is the State as bytes to save (JSON).
func (s State) MarshalBinary() ([]byte, error) {
	w := stateWire{V: keptVersion, Offset: s.Offset}
	if s.kept != nil {
		k, err := json.Marshal(s.kept)
		if err != nil {
			return nil, err
		}
		w.Kept = k
	}
	return json.Marshal(w)
}

// UnmarshalBinary loads a State MarshalBinary saved. A State saved in another
// format loads as a bare cursor (State{Offset: n}); bytes that are no State
// are an error.
func (s *State) UnmarshalBinary(b []byte) error {
	var w stateWire
	if err := json.Unmarshal(b, &w); err != nil {
		return fmt.Errorf("events state: %w", err)
	}
	*s = State{Offset: w.Offset}
	if w.V != keptVersion || len(w.Kept) == 0 || string(w.Kept) == "null" {
		return nil
	}
	var k kept
	if err := json.Unmarshal(w.Kept, &k); err != nil {
		return fmt.Errorf("events state: %w", err)
	}
	s.kept = &k
	return nil
}

// headID names a transcript's head (kept.Head).
func headID(head []byte) string {
	h := sha256.Sum256(head)
	return hex.EncodeToString(h[:16])
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
