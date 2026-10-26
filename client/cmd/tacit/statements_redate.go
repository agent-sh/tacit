package main

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/agent-sh/tacit/client/internal/curate"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/ledger"
)

// A statement is dated by the day the owner said its words (SPEC.md,
// "Statements"): the time of their turn, never the day a step read the
// session or wrote the fact. Statements written before this rule carry the
// day they were written, which in a rebuild is the run's. The steps that read
// statements again correct them: the statements step from the quote ledger's
// said (the day recorded with the words line), `tacit rulings one-offs` also
// from the owner's turns when the line records none, writing the day it finds
// into the ledger. Both are idempotent: a statement whose day agrees with its
// source is left as it is.

// statementRedate is one statement whose day disagrees with its source.
type statementRedate struct {
	Name string `json:"name"`
	Path string `json:"path"`
	From string `json:"from"` // the day it says
	To   string `json:"to"`   // the day the owner said it
	// By is where the day comes from: "ledger" (the words line's said) or
	// "turn" (their turn's time, now recorded in the ledger).
	By string `json:"by"`
	// line is the words line that records the day, when the ledger lacked
	// it; fix is a statement whose body or created date says another day
	// (without it, only the ledger gains the day).
	line *ledger.Quote
	fix  bool
}

// statementRedates are the corrections due on facts: each live statement
// (keep) whose words line in quotes has a said day, or, with said, whose turn
// code finds with its time, and whose body says another day. A day after
// today is no day they said it.
func statementRedates(facts []*fact.Fact, quotes map[string]ledger.Quote, said curate.SaidOn, today string, keep func(*fact.Fact) bool) []statementRedate {
	var out []statementRedate
	for _, f := range facts {
		if f.Meta.Type != fact.StatementType || !liveFact(f) || (keep != nil && !keep(f)) {
			continue
		}
		s := f.Meta.Source
		if s.OwnerWords == "" || s.OwnerWordsFrom == "" {
			continue
		}
		q, ok := quotes[ledger.Quote{Name: f.Meta.Name, Quote: s.OwnerWords, From: s.OwnerWordsFrom, Words: true}.Key()]
		if !ok {
			continue // lint L15's: no words line to date it by
		}
		day, by := q.Said, "ledger"
		var line *ledger.Quote
		if !fact.IsDate(day) {
			day, by = "", "turn"
			if said != nil {
				day = curate.SaidDay(said, s.OwnerWords, s.OwnerWordsFrom, q.TurnHash, today)
			}
			if fact.IsDate(day) {
				l := q
				l.Said, l.At = day, today
				line = &l
			}
		}
		if !fact.IsDate(day) || day > today {
			continue
		}
		cur, _, ok := fact.StatementLine(f.Body)
		if !ok {
			continue // lint L20's
		}
		fix := cur != day || f.Meta.Created != day
		if fix || line != nil {
			out = append(out, statementRedate{Name: f.Meta.Name, Path: f.Path, From: cur, To: day, By: by, line: line, fix: fix})
		}
	}
	return out
}

// redateWhy is the log line's reason for a correction.
func redateWhy(r statementRedate) string {
	src := "their words line's said day"
	if r.By == "turn" {
		src = "the time of their turn"
	}
	return "said on " + r.To + " (" + src + "), not " + r.From
}

// storeQuotes reads the store's quote ledger.
func storeQuotes(dir string) map[string]ledger.Quote {
	b, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(ledger.QuotesPath)))
	return ledger.Quotes(b)
}

// storeStatement reports a statement of the store's own (not this
// machine's local layer).
func storeStatement(f *fact.Fact) bool {
	return !strings.HasPrefix(f.Path, "local/") && fact.IsStatement(f)
}

// countRedates is how many corrections change a statement, and how many give
// the ledger a day it lacked.
func countRedates(rs []statementRedate) (fixed, lines int) {
	for _, r := range rs {
		if r.fix {
			fixed++
		}
		if r.line != nil {
			lines++
		}
	}
	return fixed, lines
}
