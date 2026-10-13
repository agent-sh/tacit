package fact

import (
	"strings"
	"testing"
)

func targets(body string) string {
	var out []string
	for _, sp := range LinkSpans(body) {
		out = append(out, sp.Target)
	}
	return strings.Join(out, ",")
}

func TestLinksInCodeAreNotLinks(t *testing.T) {
	for name, tc := range map[string]struct{ body, want string }{
		"plain":                   {"see [[a]] and [[b|x]]", "a,b"},
		"backtick fence":          {"```toml\n[[keys]]\n```\n[[real]]", "real"},
		"tilde fence":             {"~~~\n[[keys]]\n~~~\n[[real]]", "real"},
		"fence with info":         {"```toml title=x\n[[keys]]\n```\n[[real]]", "real"},
		"longer fence":            {"````\n```\n[[keys]]\n```\n````\n[[real]]", "real"},
		"short closer is no end":  {"````\n[[keys]]\n```\n[[still-code]]\n````\n[[real]]", "real"},
		"unclosed fence":          {"[[before]]\n```\n[[keys]]\n[[after]]", "before"},
		"indented fence":          {"   ```\n[[keys]]\n   ```\n[[real]]", "real"},
		"inline span":             {"copy the `[[keys]]` blocks and [[real]]", "real"},
		"double backtick span":    {"a ``[[keys]] with ` inside`` then [[real]]", "real"},
		"span across a line":      {"a `x\n[[keys]]` b [[real]]", "real"},
		"span cannot cross para":  {"a `x\n\n[[keys]]` b [[real]]", "keys,real"},
		"unmatched backtick":      {"a ` lone [[keys]] and [[real]]", "keys,real"},
		"escaped backtick":        {"a \\`[[keys]]\\` b", "keys"},
		"two spans":               {"`[[a]]` [[b]] `[[c]]` [[d]]", "b,d"},
		"tick-info fence is span": {"```x`y\n[[keys]]\n```", "keys"},
		"link beside fence":       {"[[a]]\n```\n[[x]]\n```\n[[b]]", "a,b"},
	} {
		if got := targets(tc.body); got != tc.want {
			t.Errorf("%s: links %q, want %q", name, got, tc.want)
		}
	}
	f := &Fact{Body: "`[[keys]]`\n```\n[[more]]\n```\n[[a]] [[a]]"}
	if got := strings.Join(f.Links(), ","); got != "a" {
		t.Errorf("Links() = %q", got)
	}
}
