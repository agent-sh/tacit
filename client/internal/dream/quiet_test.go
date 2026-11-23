package dream

import (
	"strings"
	"testing"
)

// With quiet types the prompt and the schema do not offer them, and an
// agent's fact of one is dropped; the owner's words of any type are not.
// With none, the prompt is the one before, byte for byte.
func TestQuietTypesLeaveThePromptAndTheSchema(t *testing.T) {
	t.Cleanup(func() { Quiet = map[string]bool{} })
	before := PromptFor("demo", "host-a", nil, []string{"U: hi"})
	if !strings.Contains(before, typeList) || schema() != outputSchema {
		t.Fatal("no quiet types changed the prompt or the schema")
	}
	Quiet = map[string]bool{"structure": true, "history": true}
	p := PromptFor("demo", "host-a", []string{"- held"}, []string{"U: hi"})
	if strings.Contains(p, typeList) || !strings.Contains(p, `"type": "user_standard|machine_fact|past_mistake|design_decision"`) || !strings.Contains(p, "Write no fact whose only type would be history or structure") {
		t.Fatalf("prompt:\n%s", p)
	}
	if sc := schema(); strings.Contains(sc, `"structure"`) || strings.Contains(sc, `"history"`) || !strings.Contains(sc, `"design_decision"]`) {
		t.Fatalf("schema: %s", sc)
	}
	if !DropQuiet(Extracted{Type: "structure", Kind: "observed"}) || DropQuiet(Extracted{Type: "history", Kind: "statement"}) || DropQuiet(Extracted{Type: "history", Kind: "user"}) || DropQuiet(Extracted{Type: "past_mistake", Kind: "observed"}) {
		t.Fatal("DropQuiet")
	}
}
