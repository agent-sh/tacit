package links

import (
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/fact"
)

// A TOML [[table]] header in a fence or an inline span is code: it is no dead
// link, so a fact that holds nothing else is not planned at all, and the plan
// leaves its bytes alone. A real dead link in the same body is still repaired
// and the code next to it is not.
func TestTomlTablesInCodeAreNeitherDeadNorTouched(t *testing.T) {
	tr := &tree{t: t, root: t.TempDir()}
	code := "Copy the `[[keys]]` blocks:\n\n```toml\n[[keys]]\nprefix = \"x\"\n```\n\n~~~\n[[more]]\n~~~"
	tr.add("projects/x", "only-code", "", code)
	tr.add("projects/x", "both", "", code+"\n\nSee [[really-gone]].")
	tr.add("projects/x", "ruling", "kind: user\n", code)

	p := tr.plan(nil)
	if fixOf(p, "only-code") != nil || len(p.Rulings) != 0 || len(p.Generated) != 0 {
		t.Fatalf("code was planned: %+v", p)
	}
	fx := fixOf(p, "both")
	if fx == nil || len(fx.Edits) != 1 || fx.Edits[0].Target != "really-gone" {
		t.Fatalf("plan for the fact with a real dead link: %+v", fx)
	}
	want := code + "\n\nSee really-gone.\n"
	_, body, _ := fact.Split(fx.Data)
	if got := strings.TrimPrefix(string(body), "\n"); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
