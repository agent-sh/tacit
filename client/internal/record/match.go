package record

import (
	"path"
	"strings"
)

// MatchesTool reports whether an MCP tool call's name matches one of the
// area's tool-name patterns (globs on the whole name: mcp__*__Ticketing*).
// Only MCP tools: a pattern that matches a harness's own tool (Bash, Edit)
// matched every shell call or edit of a session (an area written with tools
// [Bash] pushed at each session's first command: 7 lines in 3 days, none
// useful; eval/DELIVERY.md, "Pushes keyed on a bare program"). Shell
// commands reach an area through its commands.
func (r *Record) MatchesTool(name string) bool {
	if r == nil || !strings.HasPrefix(name, "mcp__") {
		return false
	}
	for _, p := range r.Tools {
		if ok, _ := path.Match(p, name); ok {
			return true
		}
	}
	return false
}

// Pushes reports an area the PreToolUse hook delivers from: live, active or
// waiting for review. Dormant, dead and shipped areas never push.
func (r *Record) Pushes() bool {
	return r != nil && r.Kind == KindArea && r.ReadByDefault()
}
