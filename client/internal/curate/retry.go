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
// checks its entries (check, may be nil). A refused answer is sent back to
// the model once, with why: every problem of an entry a field rule or check
// rejects and the value it refused, or that the answer is no JSON array (a
// sentence before it, a code fence after one) or not one that parses. The
// model's second answer stands or falls as it is: code never edits an
// answer to make it pass, and a second refusal is invalid as before (the
// curator splits the batch, and a note refused alone counts against it).
// first is the first answer's refusal (nil when it passed), for the caller's
// log; err is the final result's (a runner error, or an invalid answer
// wrapping modelrun.ErrInvalidResult).
//
// Until v0.9.16 an answer that was no JSON array was not sent back: the batch
// was split instead, and a lone note failed. The rulings pass answers with a
// sentence before an empty array about half the time when a note holds no
// owner words, and every such answer sent back came back clean
// (eval/VALUE.md, "The rulings answer").
func SplitChecked(ctx context.Context, run Runner, model, prompt string, check Check) (entries []Entry, cost float64, first, err error) {
	res, cost, err := run(ctx, model, prompt)
	if err != nil {
		return nil, cost, nil, err
	}
	entries, err = parseAndCheck(res, check)
	if err == nil || !errors.Is(err, modelrun.ErrInvalidResult) {
		// Valid, or a runner's error: nothing to send back.
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
