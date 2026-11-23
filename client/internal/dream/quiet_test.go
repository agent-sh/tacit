package dream

import (
	"context"
	"strings"
	"testing"
)

// With quiet types the prompt and the schema do not offer them, and an
// agent's fact of one is dropped; the owner's words of any type are not.
// With none, the prompt and the schema are the ones before, byte for byte.
func TestQuietTypesLeaveThePromptAndTheSchema(t *testing.T) {
	// The list the prompt and the schema hold, once each, is Types: an edit
	// to either that drops it makes the replacements no-ops, which this
	// catches.
	if strings.Count(Prompt, typeList) != 1 || strings.Count(outputSchema, schemaEnum) != 1 {
		t.Fatalf("the prompt or the schema no longer holds the dreamer's types once: %q / %q", typeList, schemaEnum)
	}
	var none Quiet
	if PromptFor("demo", "host-a", nil, []string{"U: hi"}, none) != PromptFor("demo", "host-a", nil, []string{"U: hi"}) || none.schema() != outputSchema || quietOf(context.Background()).schema() != outputSchema {
		t.Fatal("no quiet types changed the prompt or the schema")
	}
	q := Quiet{"structure": true, "history": true}
	p := PromptFor("demo", "host-a", []string{"- held"}, []string{"U: hi"}, q)
	if strings.Contains(p, typeList) || !strings.Contains(p, `"type": "user_standard|machine_fact|past_mistake|design_decision"`) || !strings.Contains(p, "Write no fact whose only type would be history or structure") || !strings.Contains(p, "ALREADY HELD") {
		t.Fatalf("prompt:\n%s", p)
	}
	if sc := quietOf(WithQuiet(context.Background(), q)).schema(); strings.Contains(sc, `"structure"`) || strings.Contains(sc, `"history"`) || !strings.Contains(sc, `"design_decision"]`) {
		t.Fatalf("schema: %s", sc)
	}
	if !q.Drops(Extracted{Type: "structure", Kind: "observed"}) || q.Drops(Extracted{Type: "history", Kind: "statement"}) || q.Drops(Extracted{Type: "history", Kind: "user"}) || q.Drops(Extracted{Type: "past_mistake", Kind: "observed"}) {
		t.Fatal("Drops")
	}
}
