// Package ledger reads the two records a ruling's evidence must match in the
// store itself, so a forged field fails lint even after the transcripts it
// names are gone: the owner's standing decisions (review/decisions.jsonl) for
// source.endorsed, and the curator's quote ledger (review/owner-quotes.jsonl)
// for source.owner_quote. It also reads the audit marker
// (review/rulings-audit.json) that switches a store to the definition of a
// ruling as the owner's own words (SPEC.md, Rulings).
package ledger

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Store-relative paths.
const (
	DecisionsPath = "review/decisions.jsonl"
	QuotesPath    = "review/owner-quotes.jsonl"
	MarkerPath    = "review/rulings-audit.json"
	// FormatPath is the store's format marker (Format).
	FormatPath = "review/format.json"
	// FoldersPath is the curator's folder log: proposals of areas and
	// topics, and the questions for the owner a folder step left open.
	FoldersPath = "review/folders.jsonl"
)

// Decision actions that only the owner records in the app.
const (
	ActionMine    = "mine"     // This is mine: the fact is their rule
	ActionNotMine = "not-mine" // An agent's: the fact is not their rule
)

// Decision is one record of review/decisions.jsonl, the fields this package
// reads.
type Decision struct {
	ID     string `json:"id"`
	Action string `json:"action"`
	A      string `json:"a"`
	B      string `json:"b"`
	At     string `json:"at"`
	By     string `json:"by,omitempty"`
}

// Endorsement is the owner's standing decision that kept a fact.
type Endorsement struct {
	ID   string
	Mine bool   // This is mine, not a pair answer
	At   string // when the owner recorded it (RFC 3339)
}

// legacy marks of the first review page: what each keeps.
var keepMarks = map[string]string{"left": "a", "right": "b", "a-replaces-b": "a", "b-replaces-a": "b", "keep": "b"}

// Standing reads the decision log: the last record per id, with a reopen
// clearing it; notes and agents' questions are not decisions.
func Standing(jsonl []byte) ([]Decision, map[string]Decision) {
	standing := map[string]Decision{}
	var order []string
	for _, l := range bytes.Split(jsonl, []byte("\n")) {
		var d Decision
		if json.Unmarshal(l, &d) != nil || d.ID == "" {
			continue
		}
		switch d.Action {
		case "reopen":
			delete(standing, d.ID)
			continue
		case "note", "ask":
			continue
		}
		if _, ok := standing[d.ID]; !ok {
			order = append(order, d.ID)
		}
		standing[d.ID] = d
	}
	var out []Decision
	for _, id := range order {
		if d, ok := standing[id]; ok {
			out = append(out, d)
		}
	}
	return out, standing
}

// Endorsements maps a fact to the owner's standing decision that kept it: a
// pair the owner answered keep this one, or This is mine. An agent's record (by set)
// is not theirs.
func Endorsements(jsonl []byte) map[string]Endorsement {
	ds, _ := Standing(jsonl)
	out := map[string]Endorsement{}
	for _, d := range ds {
		if d.By != "" {
			continue
		}
		kept := ""
		if d.Action == ActionMine {
			kept = d.A
		} else if side, ok := keepMarks[d.Action]; ok {
			kept = map[string]string{"a": d.A, "b": d.B}[side]
		}
		if kept != "" {
			out[kept] = Endorsement{ID: d.ID, Mine: d.Action == ActionMine, At: d.At}
		}
	}
	return out
}

// NotMine lists the facts the owner said are an agent's, not their rule, with
// when the owner said it.
func NotMine(jsonl []byte) map[string]string {
	ds, _ := Standing(jsonl)
	out := map[string]string{}
	for _, d := range ds {
		if d.By == "" && d.Action == ActionNotMine {
			out[d.A] = d.At
		}
	}
	return out
}

// Quote is one line of the quote ledger: the curator found this phrase of a
// fact in one of the owner's turns (or notes) and recorded where, with a hash
// of that turn as the owner wrote it. Words marks a phrase too short to prove a
// ruling, recorded as evidence for source.owner_words only: it never backs an
// owner_quote.
type Quote struct {
	Name     string `json:"name"`
	Quote    string `json:"quote"`
	From     string `json:"from"`
	TurnHash string `json:"turn_hash"`
	At       string `json:"at"`
	Words    bool   `json:"words,omitempty"`
}

// Key is what a fact's evidence must match: its name, quote and where, and
// whether it is a ruling's quote or a fact's words of theirs.
func (q Quote) Key() string {
	k := q.Name + "\x00" + strings.Join(strings.Fields(q.Quote), " ") + "\x00" + q.From
	if q.Words {
		k += "\x00words"
	}
	return k
}

// Quotes reads the quote ledger, keyed by Key.
func Quotes(jsonl []byte) map[string]Quote {
	out := map[string]Quote{}
	for _, l := range bytes.Split(jsonl, []byte("\n")) {
		var q Quote
		if json.Unmarshal(l, &q) == nil && q.Name != "" {
			out[q.Key()] = q
		}
	}
	return out
}

// Backing reads the quote ledger as a lookup of the owner's words by what
// they said and where, whatever fact the line was recorded for: a folder
// record's status that rests on their words (record.Entry.Quote) is backed
// by a line the curator recorded after finding the words in their turn.
// A line of words (too short to prove a ruling) backs nothing.
func Backing(jsonl []byte) func(quote, from string) bool {
	set := map[string]bool{}
	for _, q := range Quotes(jsonl) {
		if !q.Words {
			set[strings.Join(strings.Fields(q.Quote), " ")+"\x00"+q.From] = true
		}
	}
	return func(quote, from string) bool {
		return set[strings.Join(strings.Fields(quote), " ")+"\x00"+from]
	}
}

// RecordQuoteName is the ledger line's name for a quote a folder record
// rests on: the record's path, which no fact name can be.
func RecordQuoteName(folder string) string { return folder + "/_record" }

// Line encodes a ledger line (with its newline).
func (q Quote) Line() []byte {
	b, _ := json.Marshal(q)
	return append(b, '\n')
}

// TurnHash is the hash recorded for the turn a quote was found in.
func TurnHash(turn string) string {
	h := sha256.Sum256([]byte(turn))
	return hex.EncodeToString(h[:8])
}

// Marker is review/rulings-audit.json, written in the audit's own commit. Its
// presence switches the store to the definition of a ruling as their own words.
// The audit puts nothing to the owner, so the marker it writes lists no facts.
type Marker struct {
	Version     int    `json:"version"`
	AuditedHead string `json:"audited_head"`
	At          string `json:"at"`
	// Pending is read only: the facts an older audit (before owner_words) put
	// to the owner, a short quote of theirs with where it was found
	// ("short-quote"), or a fact taken off kind user with no words of theirs
	// ("no-words"). The next audit carries them forward (owner_words, a
	// candidate again) and writes the marker without them.
	Pending []LegacyPending `json:"pending,omitempty"`
}

// LegacyPending is one entry of an older audit's pending list.
type LegacyPending struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
	Quote  string `json:"quote,omitempty"`
	From   string `json:"from,omitempty"`
}

// ReadMarker reads the marker of the store at root; ok is false without one
// or when it cannot be read (fact.LoadState tells the two apart).
func ReadMarker(root string) (Marker, bool) {
	b, err := os.ReadFile(filepath.Join(root, MarkerPath))
	if err != nil {
		return Marker{}, false
	}
	m, err := ParseMarker(b)
	return m, err == nil
}

// ParseMarker reads marker bytes.
func ParseMarker(b []byte) (Marker, error) {
	var m Marker
	if err := json.Unmarshal(b, &m); err != nil {
		return Marker{}, err
	}
	if m.Version < 1 {
		return Marker{}, errors.New("no version")
	}
	return m, nil
}

// Encode renders the marker.
func (m Marker) Encode() []byte {
	b, _ := json.MarshalIndent(m, "", " ")
	return append(b, '\n')
}

// FormatVersion is the newest store format this binary writes. Version 1
// is the folder records format (SPEC.md, "Store format"): folder records
// (_record.md), the areas/ and topics/ roots, last_checked on facts. A
// binary older than the format reads such a store wrong (its strict
// decoder refuses last_checked, it reads _record.md as a broken fact), so
// no writer of this binary writes any of them until the store holds the
// marker, which the owner sets once every machine runs a binary that knows
// the format.
const FormatVersion = 1

// Format is review/format.json, the store's format marker: the format
// version every machine that writes the store can read, set by the owner's
// one-time command (`tacit format --set`). RecordsComplete is set once
// every project, area and topic folder that holds facts has a record (the
// migration's last step): from then on lint requires a record for every
// such folder, and the metadata checks no longer accept a project folder
// only because it holds facts.
type Format struct {
	Version         int    `json:"version"`
	At              string `json:"at"`
	RecordsComplete bool   `json:"records_complete,omitempty"`
}

// ParseFormat reads marker bytes.
func ParseFormat(b []byte) (Format, error) {
	var f Format
	if err := json.Unmarshal(b, &f); err != nil {
		return Format{}, err
	}
	if f.Version < 1 {
		return Format{}, errors.New("no version")
	}
	return f, nil
}

// ReadFormat reads the format marker of the store at root. ok is false
// without one; err is a marker that exists and cannot be read.
func ReadFormat(root string) (f Format, ok bool, err error) {
	b, rerr := os.ReadFile(filepath.Join(root, filepath.FromSlash(FormatPath)))
	if errors.Is(rerr, os.ErrNotExist) {
		return Format{}, false, nil
	}
	if rerr != nil {
		return Format{}, false, rerr
	}
	f, err = ParseFormat(b)
	if err != nil {
		return Format{}, false, err
	}
	return f, true, nil
}

// Writes reports whether this binary may write the folder records format
// into a store with this marker: the marker is set and names a version
// this binary knows (a newer one is a newer binary's store).
func (f Format) Writes() bool { return f.Version >= 1 && f.Version <= FormatVersion }

// Encode renders the marker.
func (f Format) Encode() []byte {
	b, _ := json.MarshalIndent(f, "", " ")
	return append(b, '\n')
}
