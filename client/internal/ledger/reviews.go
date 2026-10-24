package ledger

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
)

// ReviewsPath is the reviewer's ledger (SPEC.md, "The reviewer"): one line
// per verdict on an agent's note, written by code only (the curator on a
// writer clone, the local layer on a read-only one). A verdict on a note
// holds for every curated fact made from it (its origin).
const ReviewsPath = "review/agent-reviews.jsonl"

// ReviewsFormat is the first store format whose curated facts carry the
// reviewer's verdict in their frontmatter (source.review, review_reason):
// an older binary's strict decoder refuses the keys. Below it the verdict
// lives in the ledger alone, which every reader applies the same way.
const ReviewsFormat = 4

// Verdicts. The reviewer's (by tacit or rulebook): correct, uncertain,
// wrong, and nosource for a note with no conversation to judge it against.
// The owner's (by owner): confirmed (yes), rejected (no), corrected (their
// words replace it, as a statement).
const (
	VerdictCorrect   = "correct"
	VerdictUncertain = "uncertain"
	VerdictWrong     = "wrong"
	VerdictNoSource  = "nosource"
	VerdictConfirmed = "confirmed"
	VerdictRejected  = "rejected"
	VerdictCorrected = "corrected"
)

// Reviewers: who gave a verdict.
const (
	ByTacit    = "tacit"
	ByRulebook = "rulebook"
	ByOwner    = "owner"
)

// Verdicts is every verdict a ledger line may hold.
var Verdicts = map[string]bool{VerdictCorrect: true, VerdictUncertain: true, VerdictWrong: true, VerdictNoSource: true,
	VerdictConfirmed: true, VerdictRejected: true, VerdictCorrected: true}

// OwnerVerdict reports a verdict only the owner gives.
func OwnerVerdict(v string) bool {
	return v == VerdictConfirmed || v == VerdictRejected || v == VerdictCorrected
}

// Review is one line of the reviewer's ledger. Note is the note's path in
// the store (inbox/<harness>@<host>/...) or the overlay (local/...), Name its
// name (a curated fact's origin), Hash the note's text as judged
// (NoteHash). Session is the conversation it was judged against
// (<harness>:<id>), From and TurnHash where the owner answered (their turn,
// <harness>:<session>, or rulebook: their pane), Words their correction and
// Statement the statement that holds it.
type Review struct {
	Note      string  `json:"note"`
	Name      string  `json:"name"`
	Hash      string  `json:"hash,omitempty"`
	Verdict   string  `json:"verdict"`
	Reason    string  `json:"reason,omitempty"`
	By        string  `json:"by"`
	Model     string  `json:"model,omitempty"`
	Session   string  `json:"session,omitempty"`
	At        string  `json:"at"`
	From      string  `json:"from,omitempty"`
	TurnHash  string  `json:"turn_hash,omitempty"`
	Words     string  `json:"words,omitempty"`
	Statement string  `json:"statement,omitempty"`
	CostUSD   float64 `json:"cost_usd,omitempty"`
	// Started marks the line that starts the reviewer on a store: notes
	// created before its day are not reviewed unless asked (--backfill).
	Started string `json:"started,omitempty"`
}

// Line encodes a ledger line (with its newline).
func (r Review) Line() []byte {
	b, _ := json.Marshal(r)
	return append(b, '\n')
}

// NoteHash is the hash a verdict records of the note it judged: its
// description and body, spaces collapsed.
func NoteHash(description, body string) string {
	return TurnHash(strings.Join(strings.Fields(description), " ") + "\n" + strings.Join(strings.Fields(body), " "))
}

// Reviews is the ledger read: every verdict line, in file order, and the day
// the reviewer started ("" before its first run).
type Reviews struct {
	Lines   []Review
	Started string
	byNote  map[string][]int
	byName  map[string][]int
}

// ParseReviews reads the ledger. A line that does not parse, or names no
// note, no known verdict or no reviewer, is skipped (lint reports it).
func ParseReviews(jsonl []byte) *Reviews {
	r := &Reviews{byNote: map[string][]int{}, byName: map[string][]int{}}
	for _, l := range bytes.Split(jsonl, []byte("\n")) {
		if len(bytes.TrimSpace(l)) == 0 {
			continue
		}
		var x Review
		if json.Unmarshal(l, &x) != nil {
			continue
		}
		if x.Started != "" {
			if r.Started == "" || x.Started < r.Started {
				r.Started = x.Started
			}
			continue
		}
		if !ValidReview(x) {
			continue
		}
		r.Lines = append(r.Lines, x)
		i := len(r.Lines) - 1
		r.byNote[x.Note] = append(r.byNote[x.Note], i)
		r.byName[x.Name] = append(r.byName[x.Name], i)
	}
	return r
}

// ValidReview is what a line must hold: a note, a name, a known verdict, a
// reviewer, a time; an owner's verdict only by the owner, with where they
// gave it; a correction with their words and its statement.
func ValidReview(x Review) bool {
	if x.Note == "" || x.Name == "" || !Verdicts[x.Verdict] || x.At == "" {
		return false
	}
	switch x.By {
	case ByTacit, ByRulebook:
		return !OwnerVerdict(x.Verdict)
	case ByOwner:
		if !OwnerVerdict(x.Verdict) || x.From == "" {
			return false
		}
		return x.Verdict != VerdictCorrected || (strings.TrimSpace(x.Words) != "" && x.Statement != "")
	}
	return false
}

// pick is the verdict that stands among lines: the owner's newest when they
// answered, else the newest.
func (r *Reviews) pick(idx []int) (Review, bool) {
	if r == nil || len(idx) == 0 {
		return Review{}, false
	}
	best := -1
	for _, i := range idx {
		x := r.Lines[i]
		if best < 0 {
			best = i
			continue
		}
		b := r.Lines[best]
		bo, xo := b.By == ByOwner, x.By == ByOwner
		switch {
		case xo && !bo:
			best = i
		case xo == bo && x.At >= b.At:
			best = i
		}
	}
	return r.Lines[best], true
}

// ForNote is the verdict that stands on the note at path.
func (r *Reviews) ForNote(path string) (Review, bool) {
	if r == nil {
		return Review{}, false
	}
	return r.pick(r.byNote[path])
}

// ForOrigin is the verdict that stands on the note a curated fact was made
// from (its origin, a note name). Two notes of one name (two harnesses' inbox
// folders) give the newest line of either.
func (r *Reviews) ForOrigin(name string) (Review, bool) {
	if r == nil || name == "" {
		return Review{}, false
	}
	return r.pick(r.byName[name])
}

// Notes is every note path the ledger holds a verdict for, sorted.
func (r *Reviews) Notes() []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.byNote))
	for p := range r.byNote {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
