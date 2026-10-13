package lint

import (
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/index"
)

// A TOML [[table]] header in a fenced block or an inline code span is code,
// not a link, so L6 does not ask for a fact of that name. A real dead link
// beside it still fails.
func TestCodeIsNotALinkForL6(t *testing.T) {
	body := "Copy the `[[keys]]` blocks.\n\n```toml\n[[keys]]\nprefix = \"x\"\n```\n\n~~~\n[[more]]\n~~~\n"
	root := t.TempDir()
	write(t, root, "machines/host-a/a.md", strings.Replace(good("a", "machines/host-a"), "body", body, 1))
	facts, _, _ := fact.Walk(root)
	write(t, root, "MEMORY.md", index.Generate(facts))
	fs, err := Run(root)
	if err != nil || len(fs) != 0 {
		t.Fatalf("code was read as links: %v %v", fs, err)
	}

	write(t, root, "machines/host-a/a.md", strings.Replace(good("a", "machines/host-a"), "body", body+"\nBut see [[really-gone]].\n", 1))
	fs, err = Run(root)
	if err != nil || len(fs) != 1 || fs[0].Rule != "L6" || !strings.Contains(fs[0].Msg, "[[really-gone]]") {
		t.Fatalf("a link beside the code: %v %v", fs, err)
	}
}
