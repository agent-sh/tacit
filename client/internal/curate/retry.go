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

// maxEchoChars bounds the previous answer a retry prompt repeats.
const maxEchoChars = 60_000

// SplitChecked asks run for prompt's answer, parses it (ParseChecked) and
// checks its entries (check, may be nil). An answer refused for its entries
// (a field ParseChecked or check rejects) is sent back to the model once,
// with every problem and the value it refused; with whole set, so is an
// answer refused whole, that is no JSON array (a sentence before it, a code
// fence after one) or not one that parses, with the parser's reason. The
// model's second answer stands or falls as it is: code never edits an
// answer to make it pass, and a second refusal is invalid as before (the
// curator splits the batch, and a note refused alone counts against it). An
// answer refused whole without whole is returned at once, as before (the
// caller's bisection handles it). first is the first answer's refusal (nil
// when it passed or was not sent back), for the caller's log; err is the
// final result's (a runner error, or an invalid answer wrapping
// modelrun.ErrInvalidResult).
//
// The rulings pass sends back whole refusals since v0.9.16: it answered with
// a sentence before an empty array about half the time when a note held no
// owner words, and every such answer sent back came back clean
// (eval/VALUE.md, "The rulings answer"). The split pass does not: its
// answers are long, and no whole refusal of one was measured.
func SplitChecked(ctx context.Context, run Runner, model, prompt string, check Check, whole bool) (entries []Entry, cost float64, first, err error) {
	res, cost, err := run(ctx, model, prompt)
	if err != nil {
		return nil, cost, nil, err
	}
	entries, err = parseAndCheck(res, check)
	if err == nil || !errors.Is(err, modelrun.ErrInvalidResult) || (len(Problems(err)) == 0 && (!whole || WholeRefusal(err) == "")) {
		// Valid, a runner's error, or an answer refused whole the caller
		// splits (whole unset).
		return entries, cost, nil, err
	}
	first = err
	res2, c2, rerr := run(ctx, model, RetryPrompt(prompt, res, err))
	cost += c2
	if rerr != nil {
		return nil, cost, first, rerr
	}
	entries, err = parseAndCheck(res2, check)
	return entries, cost, first, err
}

func parseAndCheck(res string, check Check) ([]Entry, error) {
	entries, err := ParseChecked(res)
	if err != nil || check == nil {
		return entries, err
	}
	if ps := check(entries); len(ps) > 0 {
		return nil, &InvalidEntries{Problems: ps}
	}
	return entries, nil
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
	b.WriteString("\n\nCODE REFUSED THAT ANSWER. Code checks every value; it does not change your entries for you. The problems:\n")
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
