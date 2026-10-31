package curate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/modelrun"
)

const goodAnswer = `[{"origin":"n1","name":"widget-port","description":"widget listens on port 7781","type":"machine_fact","scope":"projects/widget","kind":"observed","applies_to":{},"body":"widget listens on port 7781"}]`
const badAnswer = `[{"origin":"n1","name":"widget-port","description":"widget listens on port 7781","type":"machine_fact","scope":"projects/exp-013-g1","kind":"observed","applies_to":{},"body":"widget listens on port 7781"}]`

func scopeCheck(es []Entry) []Problem {
	var out []Problem
	for i, e := range es {
		if e.Scope != "projects/widget" {
			out = append(out, Problem{Entry: i + 1, Field: "scope", Rule: "names no known repository", Hint: e.Scope + "; use the note's own project"})
		}
	}
	return out
}

// A refused answer goes back once with the problems and the value; the
// model's fix is taken as it is. Code never edits the entry.
func TestSplitCheckedSendsARefusalBackOnce(t *testing.T) {
	var prompts []string
	answers := []string{badAnswer, goodAnswer}
	run := func(_ context.Context, _, prompt string) (string, float64, error) {
		prompts = append(prompts, prompt)
		a := answers[0]
		answers = answers[1:]
		return a, 0.01, nil
	}
	entries, cost, first, err := SplitChecked(context.Background(), run, "m", "SPLIT THESE", scopeCheck, false)
	if err != nil || len(entries) != 1 || entries[0].Scope != "projects/widget" {
		t.Fatalf("entries %v err %v", entries, err)
	}
	if first == nil || len(prompts) != 2 || cost != 0.02 {
		t.Fatalf("first %v, %d prompts, cost %v", first, len(prompts), cost)
	}
	retry := prompts[1]
	for _, s := range []string{"SPLIT THESE", badAnswer, "CODE REFUSED THAT ANSWER", "entry 1: scope: names no known repository (projects/exp-013-g1; use the note's own project)", "Return the whole JSON array again"} {
		if !strings.Contains(retry, s) {
			t.Errorf("retry prompt lacks %q:\n%s", s, retry)
		}
	}
	// Logged problems never carry the value.
	for _, p := range Problems(first) {
		if strings.Contains(p.String(), "exp-013") {
			t.Errorf("logged problem holds the value: %s", p)
		}
	}
}

func TestSplitCheckedGivesUpAfterOneRetry(t *testing.T) {
	calls := 0
	run := func(context.Context, string, string) (string, float64, error) { calls++; return badAnswer, 0, nil }
	_, _, first, err := SplitChecked(context.Background(), run, "m", "p", scopeCheck, false)
	if calls != 2 || first == nil || !errors.Is(err, modelrun.ErrInvalidResult) || len(Problems(err)) != 1 {
		t.Fatalf("calls %d first %v err %v", calls, first, err)
	}
	// An answer refused whole is not sent back unless the caller asks
	// (whole, the rulings pass): the caller splits the batch. Asked, it goes
	// back once, and a second one is invalid as it is.
	for _, answer := range []string{"not json", "[{\"origin\": "} {
		for _, whole := range []bool{false, true} {
			calls = 0
			run = func(context.Context, string, string) (string, float64, error) { calls++; return answer, 0, nil }
			_, _, first, err := SplitChecked(context.Background(), run, "m", "p", scopeCheck, whole)
			want := map[bool]int{false: 1, true: 2}[whole]
			if !errors.Is(err, modelrun.ErrInvalidResult) || WholeRefusal(err) == "" || (first != nil) != whole || calls != want {
				t.Fatalf("%q, whole %v: first %v, err %v, %d call(s)", answer, whole, first, err, calls)
			}
		}
	}
	// A runner error is not the model's answer: no retry, also one that
	// wraps ErrInvalidResult (no completed result).
	for _, rerr := range []error{errors.New("down"), fmt.Errorf("%w: missing completed model result", modelrun.ErrInvalidResult)} {
		calls = 0
		run = func(context.Context, string, string) (string, float64, error) {
			calls++
			return "", 0, rerr
		}
		if _, _, first, err := SplitChecked(context.Background(), run, "m", "p", scopeCheck, true); err == nil || first != nil || calls != 1 {
			t.Fatalf("runner error %v: %v %v %d", rerr, first, err, calls)
		}
	}
}

// The rulings pass's answer is most often empty, and the model put a
// sentence before half of the empty ones: code refuses such an answer whole.
// It goes back once, saying why, and the model's clean answer is taken.
// Before v0.9.16 it was not sent back, and a note alone in its batch failed
// every quick run.
func TestSplitCheckedSendsBackAnAnswerThatIsNoArray(t *testing.T) {
	const prose = "The note quotes no owner words, so there are no rulings to extract.\n\n```json\n[]\n```"
	var prompts []string
	answers := []string{prose, "[]"}
	run := func(_ context.Context, _, prompt string) (string, float64, error) {
		prompts = append(prompts, prompt)
		a := answers[0]
		answers = answers[1:]
		return a, 0.01, nil
	}
	entries, cost, first, err := SplitChecked(context.Background(), run, "m", "RULINGS OF THESE", scopeCheck, true)
	if err != nil || len(entries) != 0 || !errors.Is(first, modelrun.ErrInvalidResult) || len(prompts) != 2 || cost != 0.02 {
		t.Fatalf("entries %v err %v first %v, %d prompts, cost %v", entries, err, first, len(prompts), cost)
	}
	for _, s := range []string{"RULINGS OF THESE", prose, "CODE REFUSED THAT ANSWER", "- invalid model result: curator must return a JSON array", "Output ONLY the JSON array."} {
		if !strings.Contains(prompts[1], s) {
			t.Errorf("retry prompt lacks %q:\n%s", s, prompts[1])
		}
	}
}

// The split prompt shows the defined types, and none that code generates.
func TestSplitPromptListsTheModelTypes(t *testing.T) {
	for _, s := range []string{"past_mistake: something that went wrong", "user: who the owner is", "never written here"} {
		if !strings.Contains(SplitPrompt, s) {
			t.Errorf("split prompt lacks %q", s)
		}
	}
	if strings.Contains(SplitPrompt, "  - hot_spot:") {
		t.Error("split prompt offers hot_spot")
	}
}
