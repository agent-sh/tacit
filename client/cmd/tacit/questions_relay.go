package main

// The owner's own answer, relayed (SPEC.md, "Closing questions"). The owner
// answers in the session they are in, often a line typed under each item of
// a list an agent put to them, and no question kind may send them to tacit
// app to answer it again (the owner, 2026-10-31). So an agent passes their
// answer on: `tacit questions close ID --proof words --words W`, where code
// finds W in the owner's own turns. The words are their answer to the
// question when they were said on or after the day it was asked, after the
// owner's own last word on it (a mark, a reopen, a keep, a note), and in a
// turn on its subject, read whole: the question's words they pasted and the
// terse answer they typed after them. Such a closure is the owner's answer:
// the guards that keep an agent off the owner's questions do not hold it back
// (a name two files hold, a finding they marked, reopened or kept a fact of,
// their note on it, an ask addressed to them), and it may change their ruling
// on their later words on its subject. It is recorded as an agent's,
// relayed: it endorses nothing, and the owner's reopen undoes it. Without
// such words every guard stands.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/agentreview"
)

// ownerAnswer is the owner's turn a words proof rests on, as code found it in
// their turns: the turn whole, as they wrote it, when they said it and its
// day (this machine's time).
type ownerAnswer struct {
	turn string
	at   time.Time
	on   string
}

// relayQuestion is what a relay is checked against: when the question was
// asked (a day, or a time), what it asked, the facts it is about, and the
// owner's own last record on it (zero for none).
type relayQuestion struct {
	asked, question string
	facts           []*factView
	last            time.Time
}

// relays says why the owner's words a closure rests on are not their answer
// to q, or nil when they are.
func (o *op) relays(q relayQuestion) error {
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
	if !q.last.IsZero() && !a.at.After(q.last) {
		return fmt.Errorf("the owner's own answer or note on it (%s) is later than these words", q.last.Local().Format("2006-01-02 15:04"))
	}
	if !agentreview.OnSubject(q.question, factsText(q.facts...), a.turn) {
		return errors.New("their turn is about another subject")
	}
	return nil
}

// factsText is what a question's facts say, for the same-subject rule: each
// one's name, file and description.
func factsText(facts ...*factView) string {
	var parts []string
	for _, f := range facts {
		if f != nil {
			parts = append(parts, f.Name, f.Path, f.Description)
		}
	}
	return strings.Join(parts, "\n")
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

// ownerLast is the time of the owner's latest record on a finding or on a
// fact it names: a mark, a reopen, a keep in another answer, a note, This is
// mine; zero for none. A relay rests only on words they said after it, so
// their reopen of a relayed answer is never undone by the words it relayed.
func (rs *reviewServer) ownerLast(id string, names ...string) time.Time {
	b, err := os.ReadFile(filepath.Join(rs.s.Dir, "review", "decisions.jsonl"))
	if err != nil {
		return time.Time{}
	}
	named := map[string]bool{}
	for _, n := range names {
		if n != "" {
			named[n] = true
		}
	}
	var last time.Time
	for _, l := range strings.Split(string(b), "\n") {
		var d decision
		if json.Unmarshal([]byte(l), &d) != nil || d.ID == "" || d.By != "" || (d.ID != id && !named[d.A] && !named[d.B]) {
			continue
		}
		if t, err := time.Parse(time.RFC3339, d.At); err == nil && t.After(last) {
			last = t
		}
	}
	return last
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
