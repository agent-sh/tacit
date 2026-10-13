package lint

import (
	"strings"
	"testing"
)

// An indented code block is code for L6 as well.
func TestIndentedCodeIsNotALinkForL6(t *testing.T) {
	root := t.TempDir()
	write(t, root, "machines/host-a/a.md", strings.Replace(good("a", "machines/host-a"), "body", "Copy the keys:\n\n    [[keys]]\n    prefix = \"x\"\n\nthen done.", 1))
	fs, err := Run(root)
	if err != nil || len(fs) != 0 {
		t.Fatalf("indented code was read as a link: %v %v", fs, err)
	}
	write(t, root, "machines/host-a/a.md", strings.Replace(good("a", "machines/host-a"), "body", "- item\n\n    [[in-a-list-item]] is prose", 1))
	fs, err = Run(root)
	if err != nil || len(fs) != 1 || fs[0].Rule != "L6" {
		t.Fatalf("a list item's paragraph is not code: %v %v", fs, err)
	}
}
