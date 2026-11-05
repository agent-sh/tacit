package search

import (
	"strings"
	"testing"
)

// A title block prints an agent's note as its name, whose it is and its
// type, without the description; the owner's rules and rulings stay whole,
// and the marks a line carries (unreviewed, this machine) stay.
func TestTitleBlockKeepsTheOwnersLinesWhole(t *testing.T) {
	hits := []Hit{
		{Name: "pool-lock-order", Type: "past_mistake", Created: "2026-10-08", Description: "Take the shard lock before the slot lock"},
		{Name: "signoff", Type: "user_standard", Ruling: true, Created: "2026-10-01", Description: "Every commit carries a Signed-off-by trailer"},
		{Name: "queue-choice", Type: "design_decision", Ruling: true, InTask: true, Created: "2026-10-02", Description: "Jobs go through the one queue"},
		{Name: "brew-prefix", Type: "machine_fact", Local: true, Created: "2026-10-03", Description: "homebrew prefix is /opt/homebrew"},
	}
	got := TitleBlock(hits, "# t")
	for _, want := range []string{
		"- pool-lock-order: agent note (2026-10-08), past mistake\n",
		"- RULE signoff: Every commit carries a Signed-off-by trailer",
		"- queue-choice: you said (2026-10-02): Jobs go through the one queue\n",
		"- brew-prefix (this machine): agent note (2026-10-03), machine fact\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "shard lock") || strings.Contains(got, "homebrew") {
		t.Errorf("an agent note's description in a title block:\n%s", got)
	}
	if full := Block(hits, "# t"); !strings.Contains(full, "Take the shard lock before the slot lock") {
		t.Errorf("Block lost the description:\n%s", full)
	}
}

// A title is built from the whose label and the type, never cut out of the
// rendered line: a computed fact whose description holds parentheses, and a
// note with no date, are titles too. A note holding the owner's words and a
// note they confirmed stay whole.
func TestTitleBlockLabelsAndOwnersNotes(t *testing.T) {
	hits := []Hit{
		{Name: "widget-hot-spots", Type: "hot_spot", Kind: "derived", Scope: "derived/widget", Description: "Hot spots in widget, last 30 code commits (path, commits): src/a.go"},
		{Name: "lock-order", Type: "past_mistake", Description: "Lock order (shard first): take the shard lock"},
		{Name: "said-words", Type: "design_decision", Words: true, Created: "2026-10-04", Description: "keep the queue single"},
		{Name: "confirmed-note", Type: "design_decision", Review: "confirmed", Created: "2026-10-05", Description: "the pool has one queue"},
	}
	got := TitleBlock(hits, "# t")
	for _, want := range []string{
		"- widget-hot-spots (computed from git): hot spot\n",
		"- lock-order: agent note, past mistake\n",
		"- said-words: agent note holding your words (2026-10-04): keep the queue single\n",
		"- confirmed-note: you confirmed (2026-10-05): the pool has one queue\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "shard first") || strings.Contains(got, "30 code commits") {
		t.Errorf("a description in a title:\n%s", got)
	}
}
