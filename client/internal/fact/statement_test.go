package fact

import (
	"bytes"
	"strings"
	"testing"
)

// A statement's body opens with their words, dated, and keeps the context
// and the curator's labelled reading; a check's flag only appends.
func TestStatementBodyAndFlags(t *testing.T) {
	b := StatementBody("2026-10-21", "the real name is  storefront,\nshop is a name for now", "the repository's name", "storefront is canonical")
	day, words, ok := StatementLine(b)
	if !ok || day != "2026-10-21" || words != "the real name is storefront, shop is a name for now" {
		t.Fatalf("%q %q %v", day, words, ok)
	}
	if StatementContext(b) != "the repository's name" || !strings.Contains(b, GlossLabel+"storefront is canonical") {
		t.Fatalf("body %q", b)
	}
	if _, _, ok := StatementLine("You decided: x"); ok {
		t.Fatal("not a statement body")
	}
	file := []byte("---\nname: s\n---\n\n" + b)
	out, err := AppendCheckFlag(file, CheckFlagLine("2026-10-22", "storefront is the real name: fails"))
	if err != nil || !bytes.HasPrefix(out, file) || len(CheckFlags(string(out))) != 1 {
		t.Fatalf("%v %q", err, out)
	}
	if _, err := AppendCheckFlag(file, "not a flag"); err == nil {
		t.Fatal("a line that is no flag")
	}
	if d, ok := TypeDocOf(StatementType); !ok || !d.Owner {
		t.Fatal("the statement type is code's")
	}
	for _, m := range ModelTypes() {
		if m == StatementType {
			t.Fatal("a model may not give the statement type")
		}
	}
}
