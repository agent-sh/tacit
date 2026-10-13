package fact

import "testing"

// An indented code block (four spaces or a tab after a blank line, outside a
// list) is code; the same indentation in a paragraph or a list item is not.
func TestIndentedCodeIsNotALink(t *testing.T) {
	for name, tc := range map[string]struct{ body, want string }{
		"after a blank line":       {"Copy this:\n\n    [[keys]]\n    prefix = 1\n\n[[real]]", "real"},
		"a tab":                    {"Copy this:\n\n\t[[keys]]\n\n[[real]]", "real"},
		"blank line inside":        {"x\n\n    [[a]]\n\n    [[b]]\n\n[[real]]", "real"},
		"ends at a dedent":         {"x\n\n    [[a]]\nback [[real]]", "real"},
		"at the start":             {"    [[keys]]\n\n[[real]]", "real"},
		"after a paragraph line":   {"text\n    [[keys]] continues the paragraph", "keys"},
		"list item continuation":   {"- item\n\n    [[keys]] is a paragraph of the item\n\n[[real]]", "keys,real"},
		"nested list item":         {"- item\n\n    - nested [[keys]]", "keys"},
		"numbered list":            {"1. item\n\n    [[keys]] still the item", "keys"},
		"list ends at a paragraph": {"- item\n\ntext\n\n    [[keys]]\n\n[[real]]", "real"},
		"three spaces is not code": {"x\n\n   [[real]]", "real"},
		"fence inside indented":    {"x\n\n    ```\n    [[a]]\n    ```\n\n[[real]]", "real"},
	} {
		if got := targets(tc.body); got != tc.want {
			t.Errorf("%s: links %q, want %q", name, got, tc.want)
		}
	}
}
