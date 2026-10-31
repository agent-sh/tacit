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

// A scope whose repo or host is in capitals is lower-cased, as the dreamer
// and delivery do, not rejected; any other invalid scope still rejects.
func TestCheckedLowercasesAScopeInCapitals(t *testing.T) {
	answer := `[{"origin":"n","name":"a","description":"d","body":"b","type":"machine_fact","scope":"projects/AcmeWidgetData","kind":"observed"},{"origin":"n","name":"b","description":"d","body":"b","type":"machine_fact","scope":"machines/Dev-Box","kind":"observed"}]`
	entries, err := ParseChecked(answer)
	if err != nil || len(entries) != 2 || entries[0].Scope != "projects/acmewidgetdata" || entries[1].Scope != "machines/dev-box" {
		t.Fatalf("entries %+v, err %v", entries, err)
	}
	for _, scope := range []string{"projects/Acme_Widget", "derived/x", "projects/a/b"} {
		if _, err := ParseChecked(strings.Replace(answer, "projects/AcmeWidgetData", scope, 1)); err == nil {
			t.Errorf("accepted scope %q", scope)
		}
	}
}

// An applies_to key is the model's text: a problem names only the known
// keys. Scopes are lower-cased in ASCII only.
func TestCheckedNamesOnlyKnownAppliesToKeys(t *testing.T) {
	ans := `[{"origin":"n","name":"a","description":"d","body":"b","type":"machine_fact","scope":"user","kind":"observed","applies_to":{"the owner's secret plan text":[1],"repos":[2]}}]`
	_, err := ParseChecked(ans)
	var got []string
	for _, p := range Problems(err) {
		got = append(got, p.String())
	}
	if strings.Join(got, "|") != "entry 1: applies_to.repos: a value is not a string|entry 1: applies_to.<unknown key>: a value is not a string" || strings.Contains(err.Error(), "secret") {
		t.Fatalf("problems %q", got)
	}
	kelvin := `[{"origin":"n","name":"a","description":"d","body":"b","type":"machine_fact","scope":"projects/Kiwi","kind":"observed"}]`
	if _, err := ParseChecked(kelvin); err == nil {
		t.Fatal("a Unicode letter lower-cased into a slug")
	}
}
