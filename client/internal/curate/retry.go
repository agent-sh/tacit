package curate

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/agent-sh/tacit/client/internal/modelrun"
)

// Check validates a parsed answer's entries beyond their shape: whether what
// they name exists, whether their type and wording fit (internal/validate).
// It returns the problems, entries numbered from 1.
type Check func([]Entry) []Problem

// Repaired is one fix Repair made to an entry: the entry (1-based, as the
// model numbered it), the field and what code did. It never holds the
// entry's text or the value, so it can be logged.
type Repaired struct {
	Entry  int
	Field  string
	Action string
}

func (r Repaired) String() string { return fmt.Sprintf("entry %d: %s: %s", r.Entry, r.Field, r.Action) }

// Repair fixes, before Check, what code can fix in a parsed answer's
// entries without a guess (SPEC.md, Metadata checks: a value made reliable,
// a value dropped that no delivery could match, a whole entry dropped that is
// not what its pass asks for). keep reports, for each entry, whether it
// stays; Repair may change an entry in place. It returns the fixes, entries
// numbered from 1.
type Repair func(entries []Entry, keep []bool) []Repaired

// maxEchoChars bounds the previous answer a retry prompt repeats.
const maxEchoChars = 60_000

// SplitChecked asks run for prompt's answer, parses it (ParseChecked),
// repairs its entries (repair, may be nil) and checks what is left (check,
// may be nil). Repair runs first so that a mechanical fault code can fix
// (a path inside the repository's checkout, owner/repo for a repository's
// name, a value that names nothing, a label that claims the owner's authority
// on a fact that is not their words) never costs a second model call. An
// answer still refused for its entries is sent back to the model once, with
// every problem left and the value it refused, entries numbered as the model
// numbered them; with whole set, so is an answer refused whole, that is no
// JSON array (a sentence before it, a code fence after one) or not one that
// parses, with the parser's reason. The model's second answer is repaired
// the same way and then stands or falls as it is: a second refusal is
// invalid as before (the curator splits the batch, and a note refused alone
// counts against it). An answer refused whole without whole is returned at
// once, as before (the caller's bisection handles it). repaired is the fixes
// to the answer returned; first is the first answer's refusal (nil when it
// passed or was not sent back), for the caller's log; err is the final
// result's (a runner error, or an invalid answer wrapping
// modelrun.ErrInvalidResult).
//
// The rulings pass sends back whole refusals since v0.9.16: it often
// answered with a sentence before an empty array when a note held no owner
// words, and every such answer sent back came back clean
// (eval/VALUE.md, "The rulings answer"). The split pass does not: its
// answers are long, and no whole refusal of one was measured.
func SplitChecked(ctx context.Context, run Runner, model, prompt string, repair Repair, check Check, whole bool) (entries []Entry, cost float64, repaired []Repaired, first, err error) {
	res, cost, err := run(ctx, model, prompt)
	if err != nil {
		return nil, cost, nil, nil, err
	}
	entries, repaired, err = RepairAndCheck(res, repair, check)
	if err == nil || !errors.Is(err, modelrun.ErrInvalidResult) || (len(Problems(err)) == 0 && (!whole || WholeRefusal(err) == "")) {
		// Valid, a runner's error, or an answer refused whole the caller
		// splits (whole unset).
		return entries, cost, repaired, nil, err
	}
	first = err
	res2, c2, rerr := run(ctx, model, RetryPrompt(prompt, res, err))
	cost += c2
	if rerr != nil {
		return nil, cost, nil, first, rerr
	}
	entries, repaired, err = RepairAndCheck(res2, repair, check)
	return entries, cost, repaired, first, err
}

// RepairAndCheck parses an answer (ParseChecked), repairs its entries and
// checks those it keeps. A problem names its entry as the answer numbers it,
// so a retry prompt that repeats the answer points at the right one.
func RepairAndCheck(res string, repair Repair, check Check) ([]Entry, []Repaired, error) {
	entries, err := ParseChecked(res)
	if err != nil {
		return entries, nil, err
	}
	return RepairEntries(entries, repair, check)
}

// RepairEntries is RepairAndCheck on entries already parsed.
func RepairEntries(entries []Entry, repair Repair, check Check) ([]Entry, []Repaired, error) {
	var repaired []Repaired
	keep := make([]bool, len(entries))
	for i := range keep {
		keep[i] = true
	}
	if repair != nil {
		repaired = repair(entries, keep)
	}
	kept := make([]Entry, 0, len(entries))
	orig := make([]int, 0, len(entries))
	for i, e := range entries {
		if keep[i] {
			kept = append(kept, e)
			orig = append(orig, i+1)
		}
	}
	if check == nil {
		return kept, repaired, nil
	}
	if ps := check(kept); len(ps) > 0 {
		for k := range ps {
			if n := ps[k].Entry; n >= 1 && n <= len(orig) {
				ps[k].Entry = orig[n-1]
			}
		}
		return nil, repaired, &InvalidEntries{Problems: ps}
	}
	return kept, repaired, nil
}

// RetryPrompt is prompt again with the refused answer and why code refused
// it, for the one retry.
func RetryPrompt(prompt, answer string, refusal error) string {
	var b strings.Builder
	b.WriteString(prompt)
	b.WriteString("\nYOUR PREVIOUS ANSWER:\n")
	a := strings.TrimSpace(answer)
	if len(a) > maxEchoChars {
		a = a[:maxEchoChars] + "\n[... cut]"
	}
	b.WriteString(a)
	b.WriteString("\n\nCODE REFUSED THAT ANSWER. Code checks every value and fixes only what it can fix without a guess; it fixed nothing below for you. The problems:\n")
	if ps := Problems(refusal); len(ps) > 0 {
		for _, p := range ps {
			fmt.Fprintf(&b, "- %s\n", p.Feedback())
		}
	} else {
		fmt.Fprintf(&b, "- %v\n", refusal)
	}
	b.WriteString("Return the whole JSON array again, every entry, with these problems fixed and nothing else changed. Output ONLY the JSON array.\n")
	return b.String()
}
