package curate

import (
	"errors"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/modelrun"
)

func TestCheckedAnswersDistinguishEmptyFromFailure(t *testing.T) {
	if rows, err := ParseChecked("```json\n[]\n```"); err != nil || len(rows) != 0 {
		t.Fatal(rows, err)
	}
	for _, answer := range []string{"garbage", "{}", "[null]", "[{}]", `[{"origin":"note","description":"valid"}]`, "[{\"origin\":\"note\",\"description\":\"valid\"},null]"} {
		if _, err := ParseChecked(answer); !errors.Is(err, modelrun.ErrInvalidResult) {
			t.Fatalf("accepted malformed extraction %q", answer)
		}
	}
	for _, answer := range []string{"garbage", "[]", "[\"no\"]", "[\"no\",\"maybe\"]", "[\"no\",\"no\",\"no\"]"} {
		if _, err := ParseVerdictsChecked(answer, 2); !errors.Is(err, modelrun.ErrInvalidResult) {
			t.Fatalf("accepted incomplete/invalid judge %q", answer)
		}
	}
	if verdicts, err := ParseVerdictsChecked(`["contradicts","no","supersedes"]`, 3); err != nil || len(verdicts) != 3 {
		t.Fatal(verdicts, err)
	}
}

func TestCheckedRejectsInvalidRoutingFields(t *testing.T) {
	valid := `[{"origin":"note","name":"port","description":"The port is 7781","body":"The port is 7781.","type":"machine_fact","scope":"system","kind":"observed"}]`
	if entries, err := ParseChecked(valid); err != nil || len(entries) != 1 {
		t.Fatal(entries, err)
	}
	for _, field := range []string{`"scope":"system"`, `"type":"machine_fact"`, `"kind":"observed"`} {
		if _, err := ParseChecked(strings.Replace(valid, field, strings.Split(field, ":")[0]+`:"typo"`, 1)); err == nil {
			t.Fatalf("accepted invalid %s", field)
		}
	}
}

// A rejected answer names every entry and field that failed and the rule,
// and never the entry's text.
func TestCheckedNamesEachProblemWithoutTheText(t *testing.T) {
	answer := `[{"origin":"n","name":"a","description":"secret-ish text one","body":"b","type":"machine_fact","scope":"system","kind":"observed"},` +
		`{"origin":"n","name":"b","description":"secret-ish text two","body":"","type":"typo","scope":"projects/a_b","kind":"observed","applies_to":{"paths":[1]}}]`
	_, err := ParseChecked(answer)
	got := []string{}
	for _, p := range Problems(err) {
		got = append(got, p.String())
	}
	want := []string{"entry 2: body: empty", "entry 2: scope: the repo or host is not a slug (a-z, 0-9, -)", "entry 2: type: not a known type", "entry 2: applies_to.paths: a value is not a string"}
	if strings.Join(got, "|") != strings.Join(want, "|") || !errors.Is(err, modelrun.ErrInvalidResult) {
		t.Fatalf("problems %q, err %v", got, err)
	}
	if strings.Contains(err.Error(), "secret-ish") {
		t.Fatalf("the error holds the entry's text: %v", err)
	}
}
