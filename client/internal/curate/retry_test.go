package curate

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/modelrun"
)

const goodAnswer = `[{"origin":"n1","name":"widget-port","description":"widget listens on port 7781","type":"machine_fact","scope":"projects/widget","kind":"observed","applies_to":{},"body":"widget listens on port 7781"}]`
const badAnswer = `[{"origin":"n1","name":"widget-port","description":"widget listens on port 7781","type":"machine_fact","scope":"projects/kv-013-g1","kind":"observed","applies_to":{},"body":"widget listens on port 7781"}]`

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
	entries, cost, first, err := SplitChecked(context.Background(), run, "m", "SPLIT THESE", scopeCheck)
	if err != nil || len(entries) != 1 || entries[0].Scope != "projects/widget" {
		t.Fatalf("entries %v err %v", entries, err)
	}
	if first == nil || len(prompts) != 2 || cost != 0.02 {
		t.Fatalf("first %v, %d prompts, cost %v", first, len(prompts), cost)
	}
	retry := prompts[1]
	for _, s := range []string{"SPLIT THESE", badAnswer, "CODE REFUSED THAT ANSWER", "entry 1: scope: names no known repository (projects/kv-013-g1; use the note's own project)", "Return the whole JSON array again"} {
		if !strings.Contains(retry, s) {
			t.Errorf("retry prompt lacks %q:\n%s", s, retry)
		}
	}
	// Logged problems never carry the value.
	for _, p := range Problems(first) {
		if strings.Contains(p.String(), "kv-013") {
			t.Errorf("logged problem holds the value: %s", p)
		}
	}
}

func TestSplitCheckedGivesUpAfterOneRetry(t *testing.T) {
	calls := 0
	run := func(context.Context, string, string) (string, float64, error) { calls++; return badAnswer, 0, nil }
	_, _, first, err := SplitChecked(context.Background(), run, "m", "p", scopeCheck)
	if calls != 2 || first == nil || !errors.Is(err, modelrun.ErrInvalidResult) || len(Problems(err)) != 1 {
		t.Fatalf("calls %d first %v err %v", calls, first, err)
	}
	// A malformed answer is not sent back: the caller splits the batch.
	calls = 0
	run = func(context.Context, string, string) (string, float64, error) { calls++; return "not json", 0, nil }
	if _, _, first, err := SplitChecked(context.Background(), run, "m", "p", scopeCheck); !errors.Is(err, modelrun.ErrInvalidResult) || first != nil || calls != 1 {
		t.Fatalf("malformed: %v %v %d", first, err, calls)
	}
	// A runner error is not the model's answer: no retry.
	calls = 0
	run = func(context.Context, string, string) (string, float64, error) {
		calls++
		return "", 0, errors.New("down")
	}
	if _, _, first, err := SplitChecked(context.Background(), run, "m", "p", scopeCheck); err == nil || first != nil || calls != 1 {
		t.Fatalf("runner error: %v %v %d", first, err, calls)
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
