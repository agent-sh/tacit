package main

// The owner's own answer, relayed (SPEC.md, "Closing questions"). The owner
// answers in the session they are in, often a line typed under each item of
// a list an agent put to them, and no question kind may send them to tacit
// app to answer it again (the owner, 2026-10-31). So an agent passes their
// answer on: `tacit questions close ID --proof words --words W`, where code
// finds W in the owner's own turns. The words are their answer to the
// question when they were said on or after the day it was asked, after the
// owner's own last word on it (a mark, a reopen, a keep, a note, an edit in
// tacit app), and in an item of their turn on its subject: the question's
// words they pasted and the terse answer they typed after them. Such a
// closure is the owner's answer: the guards that keep an agent off the
// owner's questions do not hold it back (a name two files hold, a finding
// they marked, reopened or kept a fact of, their note on it, an ask
// addressed to them), and it may change their ruling on their later words on
// its subject. It is recorded as an agent's, relayed: it endorses nothing,
// and the owner's reopen undoes it. Without such words every guard stands.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/agentreview"
	"github.com/agent-sh/tacit/client/internal/store"
)

// ownerAnswer is the owner's turn a words proof rests on, as code found it in
// their turns: the item of the turn that holds the words, as they wrote it
// (ownerwords.ItemAround; "" when the turn does not say which question they
// answer), when they said it and its day (this machine's time).
type ownerAnswer struct {
	item string
	at   time.Time
	on   string
}

// relayQuestion is what a relay is checked against: when the question was
// asked (a day, or a time), what it asked, the facts it is about, and the
// owner's own last word on it (ownerLast).
type relayQuestion struct {
	asked, question string
	facts           []*factView
	last            ownerWord
}

// relays says why the owner's words a closure rests on are not their answer
// to q, or nil when they are.
func (rs *reviewServer) relays(o *op, q relayQuestion) error {
	a := o.answer
	if o.Proof != "words" || a == nil {
		return errors.New("the closure rests on no words of the owner's")
	}
	if a.on == "" || a.at.IsZero() {
		return errors.New("their turn records no time, so nothing tells it from words said before the question")
	}
	day := askedDay(q.asked)
	if day == "" {
		return errors.New("the day the question was asked is not recorded")
	}
	if a.on < day {
		return fmt.Errorf("they said them on %s, before the question was asked (%s)", a.on, day)
	}
	var paths []string
	for _, f := range q.facts {
		if f != nil && !f.Missing {
			paths = append(paths, f.Path)
		}
	}
	edited, err := rs.ownerEdited(paths...)
	if err != nil {
		return fmt.Errorf("the owner's own edits of its facts could not be read (%s)", shortErr(err))
	}
	last := q.last.own
	if edited.After(last) {
		last = edited
	}
	if !last.IsZero() && !a.at.After(last) {
		return fmt.Errorf("the owner's own answer, note or edit on it (%s) is later than these words", last.Local().Format("2006-01-02 15:04"))
	}
	// An answer of theirs an agent relayed stands over older words; the same
	// turn may answer two questions on one fact.
	if a.at.Before(q.last.relayed) {
		return fmt.Errorf("an answer of theirs an agent relayed, said %s, is later than these words", q.last.relayed.Local().Format("2006-01-02 15:04"))
	}
	if a.item == "" {
		return errors.New("their turn does not say which question these words answer: they are no answer in it, they run on into the next question, or its items run together")
	}
	if !agentreview.OnSubject(q.question, factsText(q.facts...), a.item) {
		return errors.New("the item of their turn that holds them is about another subject")
	}
	return nil
}

// askedDay is the day a question was asked (this machine's time): of an RFC
// 3339 time, or a day a review file's name starts with ("2026-10-31-names").
func askedDay(s string) string {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Local().Format("2006-01-02")
	}
	if len(s) >= 10 {
		if _, err := time.Parse("2006-01-02", s[:10]); err == nil {
			return s[:10]
		}
	}
	return ""
}

// ownerWord is the owner's own last word on a question: own, the latest
// record of theirs (a relay rests only on words said after it), and relayed,
// the latest time they said the words of an answer of theirs an agent relayed
// (a relay rests on words not older than it).
type ownerWord struct{ own, relayed time.Time }

// ownerLast is the owner's last word on a finding or on a fact it names: a
// mark, a reopen, a keep in another answer, a note, This is mine, and an
// answer of theirs an agent relayed, at the time they said its words (or the
// record's time, when that is not recorded). So their reopen of a relayed
// answer is never undone by the words it relayed, and older words of theirs
// never undo a newer answer.
func (rs *reviewServer) ownerLast(id string, names ...string) ownerWord {
	var w ownerWord
	b, err := os.ReadFile(filepath.Join(rs.s.Dir, "review", "decisions.jsonl"))
	if err != nil {
		return w
	}
	named := map[string]bool{}
	for _, n := range names {
		if n != "" {
			named[n] = true
		}
	}
	for _, l := range strings.Split(string(b), "\n") {
		var d decision
		if json.Unmarshal([]byte(l), &d) != nil || d.ID == "" || (d.ID != id && !named[d.A] && !named[d.B]) {
			continue
		}
		switch {
		case d.By == "":
			if t, err := time.Parse(time.RFC3339, d.At); err == nil && t.After(w.own) {
				w.own = t
			}
		case d.By == "agent" && d.Relayed:
			t, err := time.Parse(time.RFC3339, d.WordsAt)
			if err != nil {
				t, err = time.Parse(time.RFC3339, d.At)
			}
			if err == nil && t.After(w.relayed) {
				w.relayed = t
			}
		}
	}
	return w
}

// relayKept is the facts a standing answer of the owner's, relayed by an
// agent, kept (left keeps A, right keeps B, both keeps both): no agent's own
// mark or closure retires one, as none retires a fact their own answer kept.
// Their reopen of the answer ends it.
func (rs *reviewServer) relayKept() map[string]bool {
	out := map[string]bool{}
	marks, _ := rs.decisions()
	for _, d := range marks {
		if d.By != "agent" || !d.Relayed {
			continue
		}
		switch markOf(d.Action) {
		case "left":
			out[d.A] = true
		case "right":
			out[d.B] = true
		case "both":
			out[d.A], out[d.B] = true, true
		}
	}
	return out
}

// theirs is what no agent's own mark or closure touches: the findings and
// facts the owner touched (ownerTouched) and the facts their standing relayed
// answers kept.
func (rs *reviewServer) theirs() map[string]bool {
	out := rs.ownerTouched()
	for n := range rs.relayKept() {
		out[n] = true
	}
	return out
}

// ownerEdited is the time of the owner's newest commit of their own in tacit
// app (`owner: `: an edit, a retire, a restore, an undo, a new rule) on any of
// the files; zero for none. Those write no decision, and their newer word on
// a fact stands over older words of theirs.
func (rs *reviewServer) ownerEdited(paths ...string) (time.Time, error) {
	if len(paths) == 0 {
		return time.Time{}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := store.Git(ctx, rs.s.Dir, append([]string{"log", "-n", "400", "--format=%cI%x1f%s", "--"}, paths...)...)
	if err != nil {
		return time.Time{}, err
	}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		at, subject, ok := strings.Cut(l, "\x1f")
		if !ok || !strings.HasPrefix(subject, ownerCommit) {
			continue
		}
		if t, err := time.Parse(time.RFC3339, at); err == nil {
			return t, nil // newest first
		}
	}
	return time.Time{}, nil
}

// notTheirAnswer is a guard's refusal, with why the owner's words a closure
// gave are no answer of theirs to the question.
func notTheirAnswer(msg string, o *op, why error) error {
	if o.Proof == "words" && why != nil {
		return fmt.Errorf("%s; these words are not the owner's answer to it: %v", msg, why)
	}
	return errors.New(msg)
}

// rulingRefusal is the refusal of a closure that would change the owner's
// ruling: it is no answer of theirs, or their answer is not later than the
// rule or not on its subject.
func rulingRefusal(msg string, o *op, why error) error {
	if o.Relayed {
		return errors.New(msg + "; these words of theirs are not later than the rule, or not on its subject")
	}
	return notTheirAnswer(msg, o, why)
}

// factsText is what a question's facts say, for the same-subject rule: each
// one's name and description, and its scope's own name (the repository or
// the machine). Not its file: folder words (user, projects, machines) name
// no subject.
func factsText(facts ...*factView) string {
	var parts []string
	for _, f := range facts {
		if f == nil {
			continue
		}
		parts = append(parts, f.Name, f.Description)
		if i := strings.LastIndexByte(f.Scope, '/'); i >= 0 {
			parts = append(parts, f.Scope[i+1:])
		}
	}
	return strings.Join(parts, "\n")
}

// pairAsked is when the question a pair carries was asked: its ask, or its
// query.
func pairAsked(it reviewItem) string {
	if it.Decision == nil && it.Ask != nil {
		return it.Ask.At
	}
	if it.Decision != nil {
		return it.Decision.At
	}
	return ""
}

// nameQuestion is what a name two files hold asks, when no agent asked it in
// its own words.
func nameQuestion(it reviewItem) string {
	return "Two files hold the name " + it.A.Name + ": " + it.A.Path + " and " + it.B.Path + ". Which one stands?"
}

// relayMoot closes, with the owner's own answer, a question of theirs that
// went moot: a fact it is about was retired, replaced or is gone, and the
// moot pass leaves the owner's questions to them. Nothing changes but the
// record, which keeps their words, what the agent read them as and why the
// question is moot. The op keeps its mark: a lost push race applies it again.
func (rs *reviewServer) relayMoot(o *op, closes, a, b, question, why string) (string, []string, error) {
	if err := closureText(o); err != nil {
		return "", nil, err
	}
	o.closedMoot = true
	d := closureDecision(o, closes, a, b)
	d.Action = mootMark
	d.Evidence = strings.TrimSpace(why + "; their answer read as " + relayedAnswer(o.Mark, a, b) + ". " + o.Evidence)
	d.Question = question
	if o.dry {
		return "the closure would land: " + why, nil, nil
	}
	files := map[string][]byte{}
	rs.appendDecision(files, d)
	return writeFiles(rs.s.Dir, "", files)
}

// relayedAnswer is what an agent read the owner's words as, on a question
// whose answer changes nothing.
func relayedAnswer(mark, a, b string) string {
	switch mark {
	case "left":
		return "keep " + a
	case "right":
		return "keep " + b
	case "both":
		return "both stand"
	case "wrong":
		return "neither stands"
	case "current":
		return "still true"
	case "retire":
		return "retire it"
	}
	return mark
}
