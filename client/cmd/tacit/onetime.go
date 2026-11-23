package main

// `tacit rulings one-time` (SPEC.md, Rulings, "One-time words"): the live
// rulings, the facts the statements step made of statements and the
// statements whose words are one-time words, settled by code with no model.
// Words that hold for the moment only (ownerwords.OneTimeClear: a
// go-ahead, a list of chores, words tied to the moment) never were a rule:
// the fact is retired (never deleted) and a line in review/statements.jsonl
// keeps the words and why. An instruction of the task at hand
// (ownerwords.OneTimeTask) in the user or system scope may still be a rule
// the owner meant to keep: it is not changed, and the owner gets a question
// in review/<date>-onetime.md (`check? ... outcome=onetime`), which the page
// lists like the rotation's: still true keeps it, outdated retires it.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/ownerwords"
	"github.com/agent-sh/tacit/client/internal/statement"
	"github.com/agent-sh/tacit/client/internal/store"
)

// oneTimeItem is one fact the step settles.
type oneTimeItem struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Scope  string `json:"scope"`
	Type   string `json:"type"`
	What   string `json:"what"`   // ruling, promoted, statement
	Action string `json:"action"` // retire, ask
	Words  string `json:"words"`
}

const (
	oneTimeRetire = "retire"
	oneTimeAsk    = "ask"
	// outcomeOneTime is the question's outcome on its check? line.
	outcomeOneTime = "onetime"
)

// oneTimePlan reads the store at dir: every live curated fact the owner did
// not write in the app that is their ruling, a fact the statements step
// made, or a statement, whose words are one-time words. asked are the facts
// a onetime question already names, which are not asked again.
func oneTimePlan(facts []*fact.Fact, log []statement.Entry, asked map[string]bool) []oneTimeItem {
	promoted := map[string]bool{}
	for _, e := range log {
		if e.Kind == statement.LogPromoted && e.Fact != "" {
			promoted[e.Fact] = true
		}
	}
	var out []oneTimeItem
	for _, f := range facts {
		m := f.Meta
		if f.InInbox() || strings.HasPrefix(m.Scope, "derived/") || m.SupersededBy != "" || m.Retired != "" || fact.OwnerWrote(f) {
			continue
		}
		it := oneTimeItem{Name: m.Name, Path: f.Path, Scope: m.Scope, Type: m.Type}
		switch {
		case promoted[m.Name]:
			it.What = "promoted"
		case fact.IsRuling(f):
			it.What = "ruling"
		case m.Type == fact.StatementType:
			it.What = "statement"
		default:
			continue
		}
		it.Words = m.Source.OwnerQuote
		if it.Words == "" {
			it.Words = m.Source.OwnerWords
		}
		if it.Words == "" || m.Source.Endorsed != "" && f.EndorsedBacked {
			continue // no words to read, or a fact the owner kept themself
		}
		switch ownerwords.OneTime(it.Words) {
		case ownerwords.OneTimeClear:
			it.Action = oneTimeRetire
		case ownerwords.OneTimeTask:
			// A statement stays what they said once; a fact of type user is
			// who the owner is, not an instruction.
			if it.What == "statement" || m.Type == "user" || ownerwords.MintsRuling(it.Words, m.Scope) || asked[m.Name] {
				continue
			}
			it.Action = oneTimeAsk
		default:
			continue
		}
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// oneTimeAsked are the facts a onetime question names in the review files.
func oneTimeAsked(dir string) map[string]bool {
	out := map[string]bool{}
	for _, r := range parseCheckFiles(dir) {
		if r.outcome == outcomeOneTime {
			out[r.name] = true
		}
	}
	return out
}

// oneTimeFiles are the step's writes: each retired fact (retired and
// updated today, every other byte as it was), its line in the statements
// log, and the questions in review/<today>-onetime.md.
func oneTimeFiles(dir string, items []oneTimeItem, today string) (map[string][]byte, string, error) {
	files := map[string][]byte{}
	var logLines []byte
	var questions, retired []string
	for _, it := range items {
		switch it.Action {
		case oneTimeRetire:
			data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(it.Path)))
			if err != nil {
				return nil, "", err
			}
			f, err := fact.Parse(it.Path, data)
			if err != nil {
				return nil, "", fmt.Errorf("%s: %w", it.Path, err)
			}
			f.Meta.Retired, f.Meta.Updated = today, today
			out, err := fact.PatchRelations(data, f)
			if err != nil {
				return nil, "", fmt.Errorf("%s: %w", it.Path, err)
			}
			files[it.Path] = out
			logLines = append(logLines, statement.Entry{Kind: statement.LogOneTime, At: today, Name: it.Name, Words: it.Words,
				Why: "one-time words (" + it.What + "): an approval to go on, a list of chores or words tied to the moment are never a standing rule"}.Line()...)
			retired = append(retired, "- "+it.Name+" ("+it.Scope+", "+it.What+")")
		case oneTimeAsk:
			questions = append(questions, checkLine(it.Name, it.Scope, outcomeOneTime, today),
				"  The curator reads these words as an instruction of the task at hand, not a rule for every session: \""+oneLine(it.Words)+"\"",
				"  Still true keeps it as your rule; outdated retires it.")
		}
	}
	if len(logLines) > 0 {
		cur, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(ledger.StatementsPath)))
		files[ledger.StatementsPath] = append(withNewline(cur), logLines...)
	}
	if len(questions) > 0 {
		rel := "review/" + today + "-onetime.md"
		cur, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if len(cur) == 0 {
			cur = []byte("# One-time words\n\n")
		}
		files[rel] = append(withNewline(cur), []byte(strings.Join(questions, "\n")+"\n")...)
	}
	if len(files) == 0 {
		return nil, "", nil
	}
	nAsk := len(questions) / 3
	msg := fmt.Sprintf("curator: one-time words: %d fact(s) retired, %d question(s) for the owner\n\n"+
		"Words that hold for the moment only (a go-ahead, a list of chores, words tied to the\n"+
		"moment) are never a ruling, a preference or a decision (SPEC.md, Rulings, One-time words).\n"+
		"No model was called.\n\n%s", len(retired), nAsk, strings.Join(retired, "\n"))
	return files, strings.TrimRight(msg, "\n"), nil
}

// runOneTime implements `tacit rulings one-time [--dry-run] [--json FILE]`.
func runOneTime(args []string) int {
	fs := flag.NewFlagSet("rulings one-time", flag.ExitOnError)
	dry := fs.Bool("dry-run", false, "print what it would retire and ask, write nothing, do not sync")
	jsonOut := fs.String("json", "", "write every item to this file as JSON lines")
	fs.Parse(args)
	s, err := store.Default()
	if err != nil || !s.Exists() {
		fmt.Fprintln(os.Stderr, "rulings one-time: no store")
		return 2
	}
	if !*dry && refuseWriter(context.Background(), s, "curator", "rulings one-time") {
		return 2
	}
	plan := func() ([]oneTimeItem, error) {
		facts, _, err := fact.Walk(s.Dir)
		if err != nil {
			return nil, err
		}
		b, _ := os.ReadFile(filepath.Join(s.Dir, filepath.FromSlash(ledger.StatementsPath)))
		return oneTimePlan(facts, statement.ReadLog(b), oneTimeAsked(s.Dir)), nil
	}
	var items []oneTimeItem
	if *dry {
		if items, err = plan(); err != nil {
			fmt.Fprintln(os.Stderr, "rulings one-time:", err)
			return 1
		}
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		pushed, err := curatorWrite(ctx, s, func() (map[string][]byte, string, error) {
			var err error
			if items, err = plan(); err != nil {
				return nil, "", err
			}
			return oneTimeFiles(s.Dir, items, today())
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "rulings one-time:", err)
			return 1
		}
		if !pushed {
			items = nil
		}
	}
	verb := map[bool]string{true: "would ", false: ""}[*dry]
	n := map[string]int{}
	for _, it := range items {
		n[it.Action]++
		fmt.Printf("%s%s %s (%s, %s): %s\n", verb, it.Action, it.Name, it.Scope, it.What, oneLine(it.Words))
	}
	fmt.Printf("rulings one-time: %sretire %d, %sask %d\n", verb, n[oneTimeRetire], verb, n[oneTimeAsk])
	if *jsonOut != "" {
		var b []byte
		for _, it := range items {
			l, _ := json.Marshal(it)
			b = append(append(b, l...), '\n')
		}
		if err := os.WriteFile(*jsonOut, b, 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "rulings one-time:", err)
			return 1
		}
	}
	return 0
}
