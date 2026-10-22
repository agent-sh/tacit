// Package statement is the statements step's logic (SPEC.md, "Statements"):
// the owner's words said once, kept verbatim, and how they become binding.
// Code counts the evidence and checks the dry claims; the curator's model
// judges meaning: what kind of point a statement makes (a choice, or a
// standing instruction), whether an earlier statement or decision makes the
// same point, and which dry claims its words make. From those code decides,
// by these rules alone:
//
//   - The same point said again in a separate session: a choice becomes a
//     decision; a standing instruction becomes a preference, and the third
//     time a rule (the one-off ladder).
//   - Said once, and a check agrees with its dry claims: a decision (two
//     independent sources agree).
//   - A check that disagrees flags it ("a check disagrees, maybe misread"),
//     an optional item for the owner; the statement stays.
//   - A newer statement that changes an earlier statement's point
//     supersedes it. A decision or rule is changed only by a newer
//     confirmed one: a statement that differs from it is an optional item
//     for the owner until it is itself promoted.
package statement

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/agent-sh/tacit/client/internal/check"
	"github.com/agent-sh/tacit/client/internal/modelrun"
	"github.com/agent-sh/tacit/client/internal/ownerwords"
)

// What a statement says, as the model reads it.
const (
	Choice      = "choice"      // a decision about something: a name, a design, one of two ways
	Instruction = "instruction" // how the owner wants work done from now on
)

// Relations of a statement to an earlier item.
const (
	Same    = "same"    // makes the same point again
	Changes = "changes" // the same point, but the owner now says something else
)

// Levels a statement is promoted to.
const (
	LevelDecision   = "decision"
	LevelPreference = "preference"
	LevelRule       = "rule"
)

// How a promotion was earned.
const (
	BySessions = "sessions" // said again in a separate session
	ByCheck    = "check"    // said once, and a check agrees
)

// Log line kinds (ledger.StatementsPath).
const (
	LogJudged     = "judged"     // the model's reading of a statement
	LogPromoted   = "promoted"   // code made a decision, preference or rule of statements
	LogSuperseded = "superseded" // a statement or promoted fact replaced by a newer one
	LogFlagged    = "flagged"    // a check disagrees with a statement
	LogDiffers    = "differs"    // a statement differs from a decision or rule that stands
)

// Relation is a statement's relation to an earlier item.
type Relation struct {
	Name     string `json:"name"`
	Relation string `json:"relation"`
}

// Judgement is the model's answer for one statement.
type Judgement struct {
	Name   string        `json:"name"`
	Says   string        `json:"says"`
	Same   []Relation    `json:"same,omitempty"`
	Claims []check.Claim `json:"claims,omitempty"`
}

// Entry is one line of the statements log.
type Entry struct {
	Kind string `json:"kind"`
	At   string `json:"at"`
	// Name is the statement (judged, flagged, differs) or the item replaced
	// (superseded).
	Name   string        `json:"name,omitempty"`
	Says   string        `json:"says,omitempty"`
	Same   []Relation    `json:"same,omitempty"`
	Claims []check.Claim `json:"claims,omitempty"`
	// Checks are code's checks of the claims (flagged, promoted by check).
	Checks []check.Result `json:"checks,omitempty"`
	// Fact is the fact a promotion made, Level its level, By how it was
	// earned, From the statements and earlier promoted facts it rests on,
	// Sessions the distinct sessions of their words.
	Fact     string   `json:"fact,omitempty"`
	Level    string   `json:"level,omitempty"`
	By       string   `json:"by,omitempty"`
	From     []string `json:"from,omitempty"`
	Sessions []string `json:"sessions,omitempty"`
	// Into is the newer item (superseded), or the decision a statement
	// differs from (differs).
	Into string `json:"into,omitempty"`
	// Hash says which check output a flag was for, so the same check flags
	// a statement once.
	Hash string `json:"hash,omitempty"`
}

// Line encodes a log line with its newline.
func (e Entry) Line() []byte {
	b, _ := json.Marshal(e)
	return append(b, '\n')
}

// ReadLog reads the log's lines in order; a line that does not parse is
// passed over.
func ReadLog(b []byte) []Entry {
	var out []Entry
	for _, l := range bytes.Split(b, []byte("\n")) {
		var e Entry
		if len(bytes.TrimSpace(l)) > 0 && json.Unmarshal(l, &e) == nil && e.Kind != "" {
			out = append(out, e)
		}
	}
	return out
}

// KindRuling is an Item that is one of the owner's rulings (a confirmed
// rule or decision, fact.IsRuling): binding already, never promoted. The
// step reads its dry claims (a canonical name) once, so the folder records
// can take it as the owner's decision.
const KindRuling = "ruling"

// Item is a statement, a fact promoted from statements, or a ruling, as
// the step and its model see it.
type Item struct {
	Name, Scope string
	// Kind is "statement", KindRuling, or the level of a promoted fact.
	Kind    string
	Words   string // their words, verbatim
	From    string // where they said them (claude:<session>, ...)
	Day     string // the day they said them (a promoted fact: the day it was made)
	Context string
	// Sessions are the distinct sessions its words came from: one for a
	// statement, the log's for a promoted fact.
	Sessions []string
}

// Hash is a short hash of s (a flag's check output).
func Hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:6])
}

// MaxCandidates bounds the earlier items one statement is compared with.
const MaxCandidates = 6

// Candidates are the earlier items the model compares s with: those whose
// words and context share at least two content stems with s's, or one and
// a folder name (names: the store's project, area and topic names), most
// shared first, newest first among equals, at most MaxCandidates. Code picks
// them; whether they make the same point is the model's.
func Candidates(s Item, earlier []Item, names map[string]bool) []Item {
	type scored struct {
		it Item
		n  int
	}
	text := func(it Item) string { return it.Words + "\n" + it.Context }
	mine := folderNames(text(s), names)
	var out []scored
	for _, it := range earlier {
		if it.Name == s.Name {
			continue
		}
		n := ownerwords.SharedStems(text(s), text(it))
		shared := 0
		for f := range folderNames(text(it), names) {
			if mine[f] {
				shared++
			}
		}
		score := n + 2*shared
		if score < 2 {
			continue
		}
		out = append(out, scored{it, score})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].n != out[j].n {
			return out[i].n > out[j].n
		}
		if out[i].it.Day != out[j].it.Day {
			return out[i].it.Day > out[j].it.Day
		}
		return out[i].it.Name < out[j].it.Name
	})
	var res []Item
	for i, sc := range out {
		if i == MaxCandidates {
			break
		}
		res = append(res, sc.it)
	}
	return res
}

func folderNames(text string, names map[string]bool) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.Fields(ownerwords.Normalize(text)) {
		if names[w] {
			out[w] = true
		}
	}
	// Dashed names: normalized text splits them, so look for them whole.
	low := strings.ToLower(text)
	for n := range names {
		if strings.Contains(n, "-") && strings.Contains(low, n) {
			out[n] = true
		}
	}
	return out
}

// Prompt is the statements step's prompt; %s is the statements (Render).
const Prompt = `You are the curator of tacit, a fact store shared by coding agents on one owner's machines. Below are STATEMENTS: things the owner said once, their own words verbatim, with the day, where they said it and a line of context. Under each are EARLIER items code found that may be about the same thing: earlier statements of theirs, or decisions, preferences and rules code already made from statements. Code counts the evidence and checks dry claims; you judge only what the words mean.

Some items are RULINGS instead: rules or decisions of the owner's already confirmed. For a ruling answer only says and claims (same is always empty); code reads its claims as the owner's decision.

For each statement answer:
- says: "choice" when it decides something (a name, a design, one of two ways, what something is), or "instruction" when it says how the owner wants work done from now on.
- same: for each EARLIER item listed under it that is about the same point, its relation: "same" when the statement makes that point again (other words are fine), or "changes" when it is about the same point but now says something different (the owner changed their mind). Leave out every earlier item that is about something else. Name only items listed under that statement.
- claims: the dry claims its words make that one source of truth settles, in these forms only:
    {"kind": "canonical", "name": "<the real name of a repository>", "other": "<another name of the same repository, when the words give one>"}
    {"kind": "exists", "repo": "<name>"} or {"kind": "absent", "repo": "<name>"}
    {"kind": "renamed", "from": "<old name>", "to": "<new name>"}
    {"kind": "path", "repo": "<name>", "path": "<repo-relative path>"}
    {"kind": "version", "repo": "<name>", "version": "<vX.Y.Z>"}
  Only what the words themselves claim, never what you infer; none when they claim nothing such. A statement shown with an earlier reading's claim that a check disagrees with is read again: that reading may have taken a name for another or a claim the words do not make. Read the words afresh and give the claims they make; give the same claim again only when the words do make it. Code checks each one: a claim a check confirms makes the statement a decision, and one a check refutes flags it for the owner.

Output ONLY a JSON array, one object per statement, no prose, no code fence:
  {"name": "<the statement's or ruling's name>", "says": "choice|instruction", "same": [{"name": "<earlier item>", "relation": "same|changes"}], "claims": []}

STATEMENTS:
%s
`

// Ask is one statement put to the model with the earlier items code found.
type Ask struct {
	S       Item
	Earlier []Item
	// Recheck: the statement was read before and a check disagrees with that
	// reading's claims (these lines); it is read again, since the earlier
	// reading may have misread the words.
	Recheck []string
}

// Render writes the statements section of the prompt.
func Render(asks []Ask) string {
	var b strings.Builder
	for _, a := range asks {
		s := a.S
		if s.Kind == KindRuling {
			fmt.Fprintf(&b, "### ruling=%s (%s)\nthe owner's ruling, confirmed (%s, %s): %q\ncontext: %s\n\n", s.Name, s.Scope, s.Day, s.From, s.Words, s.Context)
			continue
		}
		fmt.Fprintf(&b, "### statement=%s (%s)\nyou said once (%s, %s): %q\ncontext: %s\n", s.Name, s.Scope, s.Day, s.From, s.Words, s.Context)
		for _, l := range a.Recheck {
			fmt.Fprintf(&b, "an earlier reading's claim was checked by code and the check disagrees: %s\n", l)
		}
		if len(a.Earlier) == 0 {
			b.WriteString("earlier: none found\n")
		}
		for _, e := range a.Earlier {
			what := "statement"
			switch e.Kind {
			case "statement":
			case KindRuling:
				what = "the owner's ruling"
			default:
				what = e.Kind + " made from statements"
			}
			fmt.Fprintf(&b, "earlier %s (%s, %s, %s): %q; context: %s\n", e.Name, what, e.Day, strings.Join(e.Sessions, " "), e.Words, e.Context)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// Parse reads the model's answer, checking each judgement against the asks:
// it names a statement asked, says is choice or instruction, every relation
// names an earlier item listed under it with a known relation, every claim
// has its form. Problems are returned per statement for the retry.
func Parse(text string, asks []Ask) ([]Judgement, map[string][]string, error) {
	text = strings.TrimSpace(text)
	text = strings.TrimPrefix(strings.TrimPrefix(text, "```json"), "```")
	text = strings.TrimSpace(strings.TrimSuffix(text, "```"))
	if i := strings.Index(text, "["); i > 0 {
		text = text[i:]
	}
	if j := strings.LastIndex(text, "]"); j >= 0 {
		text = text[:j+1]
	}
	var js []Judgement
	if err := json.Unmarshal([]byte(text), &js); err != nil {
		return nil, nil, fmt.Errorf("%w: the statements answer must be a JSON array", modelrun.ErrInvalidResult)
	}
	byName := map[string]Ask{}
	for _, a := range asks {
		byName[a.S.Name] = a
	}
	problems := map[string][]string{}
	var ok []Judgement
	seen := map[string]bool{}
	for _, j := range js {
		a, known := byName[j.Name]
		if !known {
			continue
		}
		if seen[j.Name] {
			continue
		}
		seen[j.Name] = true
		var ps []string
		if j.Says != Choice && j.Says != Instruction {
			ps = append(ps, fmt.Sprintf("says: not choice or instruction (got %q)", j.Says))
		}
		listed := map[string]bool{}
		for _, e := range a.Earlier {
			listed[e.Name] = true
		}
		for _, r := range j.Same {
			switch {
			case !listed[r.Name]:
				ps = append(ps, fmt.Sprintf("same: %s is not an earlier item listed under this statement", r.Name))
			case r.Relation != Same && r.Relation != Changes:
				ps = append(ps, fmt.Sprintf("same: the relation to %s is not same or changes (got %q)", r.Name, r.Relation))
			}
		}
		for _, c := range j.Claims {
			ps = append(ps, c.Valid()...)
		}
		if len(ps) > 0 {
			problems[j.Name] = ps
			continue
		}
		ok = append(ok, j)
	}
	for _, a := range asks {
		if !seen[a.S.Name] {
			problems[a.S.Name] = append(problems[a.S.Name], "no answer for this statement")
		}
	}
	return ok, problems, nil
}

// RetryPrompt is prompt again with the answer and code's problems, asking
// for those statements only.
func RetryPrompt(prompt, answer string, problems map[string][]string) string {
	var b strings.Builder
	b.WriteString(prompt)
	b.WriteString("\nYOUR PREVIOUS ANSWER:\n")
	a := strings.TrimSpace(answer)
	if len(a) > 30_000 {
		a = a[:30_000] + "\n[... cut]"
	}
	b.WriteString(a)
	b.WriteString("\n\nCODE REFUSED THESE ANSWERS. The problems:\n")
	var names []string
	for n := range problems {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(&b, "- %s: %s\n", n, strings.Join(problems[n], "; "))
	}
	b.WriteString("Return a JSON array with answers for these statements only, the problems fixed. Output ONLY the JSON array.\n")
	return b.String()
}

// Verdict of a statement's checked claims: Agrees when at least one claim
// holds and none fails, Disagrees when one fails, "" otherwise (no claim,
// or no source could answer).
func Verdict(rs []check.Result) string {
	holds, fails := 0, 0
	for _, r := range rs {
		switch r.Verdict {
		case check.Holds:
			holds++
		case check.Fails:
			fails++
		}
	}
	switch {
	case fails > 0:
		return check.Fails
	case holds > 0:
		return check.Holds
	}
	return ""
}

// ErrNoStatements is a step with nothing to judge.
var ErrNoStatements = errors.New("no statements")
