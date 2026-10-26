package curate

import (
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/relevance"
)

// The split curates the owner's words only with their future use stated.
func TestGateOnSplitEntries(t *testing.T) {
	keep := &relevance.Judgement{Keep: true, Category: "instruction", Reason: "how he wants reviews run"}
	if ok, _ := Gate(Entry{Kind: "user", OwnerQuote: "review every agent note before it is curated", Relevance: keep}); !ok {
		t.Fatal("a kept ruling dropped")
	}
	if ok, _ := Gate(Entry{Kind: "observed"}); !ok {
		t.Fatal("an observed entry gated")
	}
	ok, d := Gate(Entry{Origin: "n1", Kind: KindStatement, OwnerQuote: "yes do it", Relevance: &relevance.Judgement{Keep: false, Category: "acknowledgement", Reason: "a go-ahead"}})
	if ok || d.Note != "n1" || d.Words != "yes do it" || d.Category != "acknowledgement" {
		t.Fatalf("%v %+v", ok, d)
	}
	if ok, d := Gate(Entry{Kind: "user", OwnerQuote: "x y z", Relevance: &relevance.Judgement{Keep: true, Category: "vibes", Reason: "r"}}); ok || !strings.HasPrefix(d.Reason, "refused:") {
		t.Fatalf("a keep with no future use passed: %+v", d)
	}
	for _, p := range []string{SplitPrompt, RulingsPrompt} {
		if !strings.Contains(p, `"relevance"`) {
			t.Fatal("a prompt asks for no relevance")
		}
	}
}
