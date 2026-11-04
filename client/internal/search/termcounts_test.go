package search

import "testing"

// TermCounts counts a term by the tokenizer, not inside another word.
func TestTermCountsCountsWholeTerms(t *testing.T) {
	got := TermCounts("cli client decline; cli.go and the cli")
	if got["cli"] != 3 || got["client"] != 1 || got["decline"] != 1 {
		t.Fatalf("counts %v", got)
	}
}
