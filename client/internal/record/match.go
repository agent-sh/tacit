package record

import "path"

// MatchesTool reports whether a tool call's name matches one of the area's
// tool-name patterns (globs on the whole name: mcp__*__Ticketing*).
func (r *Record) MatchesTool(name string) bool {
	if r == nil || name == "" {
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
