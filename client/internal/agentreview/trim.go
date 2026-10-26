package agentreview

import (
	"fmt"
	"strings"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/ownerwords"
	"github.com/agent-sh/tacit/client/internal/validate"
)

// What is in doubt about an uncertain note (Verdict.Doubt): its main point
// itself, or only claims that go further than the conversation (a true core
// plus an overreach).
const (
	DoubtCore      = "core"
	DoubtOverreach = "overreach"
)

// What a note's main point is (Verdict.Kind), as the reviewer reads it.
const (
	KindRule     = "rule"     // how to work from now on, what the owner wants
	KindDecision = "decision" // a choice made
	KindMistake  = "mistake"  // something that went wrong, and what to do about it
	KindFact     = "fact"     // what exists, where, what was measured
)

var kinds = map[string]bool{KindRule: true, KindDecision: true, KindMistake: true, KindFact: true}

// OneOff is the owner's one remark a trimmed rule or choice rests on: their
// words copied from one OWNER message (its number in the digest), and one
// line of what was being worked on then.
type OneOff struct {
	Words   string `json:"words"`
	Line    int    `json:"line"`
	Context string `json:"context"`
}

// Trim is the reviewer's cut of an uncertain note down to what its session
// supports: the claims kept, each with the messages that support it, the
// claims removed and why, a narrower scope when the kept claims belong to
// one, and the owner's remark when what is kept rests on one.
type Trim struct {
	Claims  []ledger.Claim   `json:"claims"`
	Removed []ledger.Removed `json:"removed"`
	Scope   string           `json:"scope,omitempty"`
	OneOff  *OneOff          `json:"one_off,omitempty"`
}

// Text is the trim as a note's description and body: the first kept claim
// is the description, every kept claim a line of the body.
func (t Trim) Text() (desc, body string) {
	var lines []string
	for _, c := range t.Claims {
		lines = append(lines, strings.Join(strings.Fields(c.Text), " "))
	}
	if len(lines) == 0 {
		return "", ""
	}
	return fact.ClipDescription(lines[0], 200), strings.Join(lines, "\n") + "\n"
}

// maxNarrowing is how many content stems a kept claim may hold that the note
// does not: the words that narrow it ("in the valkey repo", "on 2026-10-20"),
// each found in a message the claim cites.
const maxNarrowing = 3

// CheckTrim is code's check of a trim (SPEC.md, "Trims"): it may only remove
// or narrow. Every kept claim cites messages the reviewer was shown; each of
// its content words is the note's, or one of a few narrowing words found in
// the messages it cites; the trim is no longer than the note, those few
// words aside; a claim that gives the owner's authority (the owner said, wants,
// ruled) cites an OWNER message, and the trim says so only where the note
// did; the scope stays or narrows. A rule or a choice kept on the owner's
// words in this one session (the reviewer's kind, or a claim that cites
// their message and is worded as a standing rule: always, never, must, ...)
// must be their remark said once (one_off): their words verbatim in the
// OWNER message it names. It returns the
// problems; none means the trim stands.
func CheckTrim(n Note, msgs []Msg, shown map[int]bool, kind string, t Trim) []string {
	var ps []string
	if len(t.Claims) == 0 {
		return []string{"no claim is kept"}
	}
	noteText := n.Desc + "\n" + n.Body
	have := stemSet(noteText)
	owned := false    // a kept claim cites one of the owner's messages
	standing := false // such a claim is worded as a standing rule
	for i, c := range t.Claims {
		text := strings.Join(strings.Fields(c.Text), " ")
		if text == "" {
			ps = append(ps, fmt.Sprintf("claim %d is empty", i+1))
			continue
		}
		if len(c.Cites) == 0 {
			ps = append(ps, fmt.Sprintf("claim %d cites no message: %q", i+1, cut(text, 80)))
			continue
		}
		var cited []string
		ownerCited := false
		for _, k := range c.Cites {
			if k < 1 || k > len(msgs) || !shown[k] {
				ps = append(ps, fmt.Sprintf("claim %d cites message %d, which the reviewer was not shown", i+1, k))
				continue
			}
			m := msgs[k-1]
			cited = append(cited, m.Text)
			for _, u := range m.Tools {
				cited = append(cited, u.Input)
			}
			ownerCited = ownerCited || (m.Role == "user" && m.Owner)
		}
		owned = owned || ownerCited
		standing = standing || (ownerCited && ruleWorded(text))
		citedStems := stemSet(strings.Join(cited, "\n"))
		var extra []string
		for _, s := range ownerwords.ContentStems(text) {
			if have[s] {
				continue
			}
			if !citedStems[s] {
				ps = append(ps, fmt.Sprintf("claim %d adds %q, which neither the note nor the messages it cites hold", i+1, s))
				continue
			}
			extra = append(extra, s)
		}
		if len(extra) > maxNarrowing {
			ps = append(ps, fmt.Sprintf("claim %d adds %d words the note does not hold (%s): a new claim, not a narrower one", i+1, len(extra), strings.Join(extra, ", ")))
		}
		if validate.ClaimsOwner(text, "") && !ownerCited {
			ps = append(ps, fmt.Sprintf("claim %d gives the owner's authority but cites no message the owner typed: %q", i+1, cut(text, 80)))
		}
	}
	desc, body := t.Text()
	if words(body) > words(noteText)+2*maxNarrowing {
		ps = append(ps, "the trim is longer than the note: a trim only removes or narrows")
	}
	if validate.ClaimsOwner(desc, body) && !validate.ClaimsOwner(n.Desc, n.Body) {
		ps = append(ps, "the trim says the owner said, wants or ruled something; the note did not")
	}
	if t.Scope != "" && !Narrower(n.Scope, t.Scope) {
		ps = append(ps, fmt.Sprintf("scope %q is not the note's scope %q or narrower", t.Scope, n.Scope))
	}
	if (((kind == KindRule || kind == KindDecision) && owned) || standing) && t.OneOff == nil {
		ps = append(ps, "a rule or choice kept on the owner's words in one session is their remark said once: one_off is required")
	}
	if t.OneOff != nil {
		ps = append(ps, checkOneOff(msgs, shown, *t.OneOff)...)
	}
	return ps
}

// checkOneOff checks the owner's remark: their words, verbatim and long
// enough to say something (ownerwords.Usable), in the OWNER message it
// names, which the reviewer was shown; a line of context.
func checkOneOff(msgs []Msg, shown map[int]bool, o OneOff) []string {
	var ps []string
	if o.Line < 1 || o.Line > len(msgs) || !shown[o.Line] {
		return []string{fmt.Sprintf("one_off names message %d, which the reviewer was not shown", o.Line)}
	}
	m := msgs[o.Line-1]
	if m.Role != "user" || !m.Owner {
		return []string{fmt.Sprintf("one_off names message %d, which the owner did not type", o.Line)}
	}
	words := strings.Join(strings.Fields(o.Words), " ")
	if !ownerwords.Usable(words) || !ownerwords.In(m.Text, words) {
		ps = append(ps, fmt.Sprintf("one_off's words are not the owner's words in message %d, verbatim", o.Line))
	}
	if c := strings.TrimSpace(o.Context); c == "" || len([]rune(c)) > 300 {
		ps = append(ps, "one_off needs one line of context")
	}
	return ps
}

// Narrower reports whether scope to is from or narrower: the same, a folder
// (project, machine, org, area, topic) for a note of the owner's or the
// system's scope, or a topic under an area for that area.
func Narrower(from, to string) bool {
	if to == from {
		return true
	}
	if !fact.ValidScope(to) {
		return false
	}
	root, _, _ := strings.Cut(to, "/")
	switch from {
	case "", "user", "system":
		return root != "user" && root != "system"
	}
	return strings.HasPrefix(to, from+"/topics/")
}

// standingWords word a standing rule.
var standingWords = map[string]bool{"always": true, "never": true, "must": true, "should": true, "dont": true, "every": true,
	"whenever": true, "prefers": true, "prefer": true, "wants": true, "rule": true, "standing": true, "default": true}

// ruleWorded reports text worded as a standing rule.
func ruleWorded(text string) bool {
	for _, w := range strings.Fields(ownerwords.Normalize(text)) {
		if standingWords[w] {
			return true
		}
	}
	return false
}

func words(text string) int { return len(strings.Fields(ownerwords.Normalize(text))) }

func stemSet(text string) map[string]bool {
	out := map[string]bool{}
	for _, s := range ownerwords.ContentStems(text) {
		out[s] = true
	}
	return out
}

// As is what the trim becomes (ledger.TrimNote, ledger.TrimStatement): a
// trimmed agent's note, or the owner's statement when it rests on their one
// remark.
func (t Trim) As() string {
	if t.OneOff != nil {
		return ledger.TrimStatement
	}
	return ledger.TrimNote
}
