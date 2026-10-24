package agentreview

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/agent-sh/tacit/client/internal/ledger"
)

// RulebookLog is the rulebook mod's review log under home
// (~/.local/state/rulebook/reviews.jsonl), or $TACIT_RULEBOOK_LOG ("off":
// none). The mod reviews a Claude Code session's memory writes and tacit adds
// before they run, with the same reviewer: tacit honours its verdicts instead
// of reviewing the note again.
func RulebookLog(home string) string {
	if v := strings.TrimSpace(os.Getenv("TACIT_RULEBOOK_LOG")); v != "" {
		if strings.EqualFold(v, "off") {
			return ""
		}
		return v
	}
	return filepath.Join(home, ".local", "state", "rulebook", "reviews.jsonl")
}

// RulebookRow is one line of the mod's log: the session and its cwd, the
// save (kind memory: the file's path; kind tacit: the fact's name), its
// title (a memory's description, a fact's --desc), the verdict and why. A
// mod with the owner-decision patch also logs the owner's decisions in its
// pane (by owner: owner-approved, owner-discarded, owner-edited with the
// text), and the answers to tacit's questions it shows there (tacit: the
// question's id).
type RulebookRow struct {
	TS      string `json:"ts"`
	Session string `json:"session"`
	Cwd     string `json:"cwd"`
	Model   string `json:"model"`
	Kind    string `json:"kind"`
	Title   string `json:"title"`
	Path    string `json:"path"`
	Name    string `json:"name"`
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
	By      string `json:"by"`
	Text    string `json:"text"`
	Tacit   string `json:"tacit"`
}

// Rulebook verdicts of the owner (the patched mod).
const (
	RulebookApproved  = "owner-approved"
	RulebookDiscarded = "owner-discarded"
	RulebookEdited    = "owner-edited"
)

// ReadRulebook reads the mod's log; a missing file is none.
func ReadRulebook(path string) []RulebookRow {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []RulebookRow
	for _, l := range bytes.Split(b, []byte("\n")) {
		var r RulebookRow
		if json.Unmarshal(l, &r) == nil && r.Verdict != "" {
			out = append(out, r)
		}
	}
	return out
}

// Note is what the reviewer knows of an agent's note.
type Note struct {
	Path, Name, Desc, Body, Type, Scope string
	Harness                             string // the harness that saved it (claude, codex, pi, hermes; import's from where it was imported)
	Session                             string // the harness's session id
	ImportedFrom                        string // a memory file's path, ~ expanded
	Created                             string
}

func same(a, b string) bool {
	return strings.Join(strings.Fields(a), " ") == strings.Join(strings.Fields(b), " ")
}

// sameClipped: one text is the other cut to tacit's 200-character
// description (with an ellipsis), or they are the same.
func sameClipped(a, b string) bool {
	a, b = strings.Join(strings.Fields(a), " "), strings.Join(strings.Fields(b), " ")
	if a == b {
		return true
	}
	trim := func(s string) string { return strings.TrimSuffix(strings.TrimSuffix(s, "…"), "...") }
	ta, tb := trim(a), trim(b)
	return len(ta) >= 60 && strings.HasPrefix(b, ta) || len(tb) >= 60 && strings.HasPrefix(a, tb)
}

// matches reports the row that reviewed this note: a memory file by its
// path and description, a tacit add by its name (tacit may add a suffix to
// a name taken) and its description or session.
func (r RulebookRow) matches(n Note) bool {
	switch r.Kind {
	case "memory":
		return n.ImportedFrom != "" && r.Path == n.ImportedFrom && sameClipped(r.Title, n.Desc)
	case "tacit":
		if n.ImportedFrom != "" || r.Name == "" {
			return false
		}
		name := n.Name == r.Name || strings.HasPrefix(n.Name, r.Name+"-") && len(n.Name) == len(r.Name)+7
		if !name {
			return false
		}
		return sameClipped(r.Title, n.Desc) || (n.Session != "" && r.Session == n.Session)
	}
	return false
}

// RulebookVerdict is the mod's verdict on the note, as a ledger line: its
// newest review of the note (a failed review is none), or the owner's
// decision in its pane when the patched mod logged one after it.
func RulebookVerdict(rows []RulebookRow, n Note, at string) (ledger.Review, bool) {
	var best, owner *RulebookRow
	for i := range rows {
		r := &rows[i]
		if r.Tacit != "" || !r.matches(n) {
			continue
		}
		switch {
		case r.By == "owner" && (r.Verdict == RulebookApproved || r.Verdict == RulebookDiscarded):
			if owner == nil || r.TS >= owner.TS {
				owner = r
			}
		case r.Verdict == ledger.VerdictCorrect || r.Verdict == ledger.VerdictUncertain || r.Verdict == ledger.VerdictWrong:
			if best == nil || r.TS >= best.TS {
				best = r
			}
		}
	}
	// The owner's decision stands over the review it answered; a review made
	// after it (the note changed again) stands over it.
	if owner != nil && (best == nil || owner.TS >= best.TS) {
		best = owner
	}
	if best == nil {
		return ledger.Review{}, false
	}
	l := ledger.Review{Note: n.Path, Name: n.Name, Hash: ledger.NoteHash(n.Desc, n.Body), Reason: best.Reason, By: ledger.ByRulebook,
		Model: best.Model, Session: refOf("claude", best.Session), At: at}
	switch best.Verdict {
	case RulebookApproved:
		l.By, l.Verdict, l.From, l.Reason = ledger.ByOwner, ledger.VerdictConfirmed, refOf("rulebook", best.Session), "approved in the rulebook pane"
	case RulebookDiscarded:
		l.By, l.Verdict, l.From, l.Reason = ledger.ByOwner, ledger.VerdictRejected, refOf("rulebook", best.Session), "discarded in the rulebook pane"
	default:
		l.Verdict = best.Verdict
	}
	return l, true
}

func refOf(harness, session string) string {
	if session == "" {
		return harness
	}
	return harness + ":" + session
}

// RulebookAnswers is the owner's answers to tacit's questions given in the
// rulebook pane (the patched mod): question id -> the answer.
func RulebookAnswers(rows []RulebookRow) map[string]Answer {
	out := map[string]Answer{}
	for _, r := range rows {
		if r.Tacit == "" || r.By != "owner" {
			continue
		}
		a := Answer{From: refOf("rulebook", r.Session), At: r.TS}
		switch r.Verdict {
		case RulebookApproved:
			a.Kind = AnswerYes
		case RulebookDiscarded:
			a.Kind, a.Words = AnswerNo, clean(r.Text)
		case RulebookEdited:
			if clean(r.Text) == "" {
				continue
			}
			a.Kind, a.Words = AnswerCorrect, clean(r.Text)
		default:
			continue
		}
		a.TurnHash = ledger.TurnHash(r.TS + "\x00" + r.Verdict + "\x00" + r.Text)
		if prev, ok := out[r.Tacit]; !ok || a.At >= prev.At {
			out[r.Tacit] = a
		}
	}
	return out
}
