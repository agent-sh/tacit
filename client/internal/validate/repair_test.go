package validate

import (
	"reflect"
	"testing"
)

func TestRepoNameReadsOnlyKnownNames(t *testing.T) {
	e := env(t)
	for in, want := range map[string]string{
		"widget":                               "widget",
		"Widget":                               "widget",
		" widget ":                             "widget",
		"widget-svc":                           "widget-svc", // another name of the checkout: delivery matches it
		"acme/widget":                          "widget",
		"Acme/Widget.git":                      "widget",
		"https://example.com/acme/widget":      "widget",
		"https://example.com/acme/widget.git/": "widget",
		"git@example.com:acme/widget-svc.git":  "widget-svc",
		"~/projects/widget":                    "widget",
		"/home/someone/projects/legacy":        "legacy",
		`C:\src\atlas`:                         "atlas",
	} {
		if got, ok := e.RepoName(in); !ok || got != want {
			t.Errorf("RepoName(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "client", "memory", "acme/unknown", "https://example.com/acme/", "~/projects/widget/src", "wt-api-hotfix", "projects/exp-013-g1"} {
		if got, ok := e.RepoName(in); ok {
			t.Errorf("RepoName(%q) = %q, want no name", in, got)
		}
	}
}

func TestRepairReposDropsWhatNamesNothing(t *testing.T) {
	e := env(t)
	got, changed, dropped := e.RepairRepos([]string{"acme/widget", "widget", "nope", "Legacy"})
	if want := []string{"widget", "legacy"}; !reflect.DeepEqual(got, want) || !changed || dropped != 1 {
		t.Errorf("RepairRepos = %v, %v, %d", got, changed, dropped)
	}
	if ps := e.Repos(got); len(ps) != 0 {
		t.Errorf("repaired names still refused: %v", ps)
	}
	if got, changed, dropped := e.RepairRepos([]string{"widget", "atlas"}); changed || dropped != 0 || len(got) != 2 {
		t.Errorf("valid names changed: %v %v %d", got, changed, dropped)
	}
}

// Only a label that opens a sentence is cut; other wording that claims the
// owner's authority has no neutral form code could write, and stays.
func TestStripOwnerLabels(t *testing.T) {
	for in, want := range map[string]string{
		"Owner ruling: never push to main":                         "Never push to main",
		"owner rule (2026-10-12): QA-only passes stay manual":      "QA-only passes stay manual",
		"Owner's rules: sign every tag.":                           "Sign every tag.",
		"Builds run in CI. Owner ruling: no local release builds.": "Builds run in CI. No local release builds.",
		`"Owner rule: rebase, never merge"`:                        `"Rebase, never merge"`,
		"Owner ruling:\n- keep the cap":                            "- keep the cap",
		"Owner ruling:\nowner rule: never push to main":            "Never push to main",
		"Owner ruling: owner rule: never push":                     "Never push",
		"Owner ruling:":                                            "",
	} {
		got := StripOwnerLabels(in)
		if got != want {
			t.Errorf("StripOwnerLabels(%q) = %q, want %q", in, got, want)
		}
		if ClaimsOwner(got, "") {
			t.Errorf("%q still claims the owner's authority", got)
		}
	}
	for _, in := range []string{
		"The owner said to rebase",
		"The owner wants agent sessions on the rig",
		"Per owner rule, tags are signed",
		"This follows the owner's rule: cite checks by tree hash", // mid-sentence: not a label
		"Tags are signed.",
	} {
		if got := StripOwnerLabels(in); got != in {
			t.Errorf("StripOwnerLabels(%q) = %q, want it unchanged", in, got)
		}
	}
}
