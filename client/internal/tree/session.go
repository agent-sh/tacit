package tree

import (
	"fmt"
	"strings"
)

// Session is who a read of the store answers for, and the one rule of what
// it may read, for the tree tool (tacit_tree, `tacit tree`) and for search
// (tacit_search, `tacit search`) alike (SPEC.md, "Search and the tree"):
//
//   - by default: system, user, the session's repository under every name
//     delivery knows it by (projects/<name> and derived/<name>), and this
//     machine (machines/<host>); this machine's local layer is read with
//     the store when it is on (the caller's: ReadOptions.Local, the search
//     index's overlay);
//   - another repository's or machine's scope only when the caller names it
//     (a path or scope given explicitly), and then that scope alone.
type Session struct {
	// Repos are the session's repository and the other names delivery
	// knows it by (any case).
	Repos []string
	// Host is this machine's name.
	Host string
}

func (s Session) repoSet() map[string]bool {
	m := map[string]bool{}
	for _, r := range s.Repos {
		if r != "" {
			m[strings.ToLower(r)] = true
		}
	}
	return m
}

// Scopes are the scope folders the session reads by default: system, user,
// projects/<name> and derived/<name> for each of Repos (in order, once),
// and machines/<host>.
func (s Session) Scopes() []string {
	out := []string{"system", "user"}
	seen := map[string]bool{}
	for _, r := range s.Repos {
		if r = strings.ToLower(r); r != "" && !seen[r] {
			seen[r] = true
			out = append(out, "projects/"+r, "derived/"+r)
		}
	}
	if s.Host != "" {
		out = append(out, "machines/"+strings.ToLower(s.Host))
	}
	return out
}

// Reads reports whether the session reads folder p (store-relative, as
// Clean returns it): the root, the scope roots, the default scopes and
// anything below them, and, when named is a scope the caller named
// (Named), that scope and anything below it.
func (s Session) Reads(p, named string) bool {
	if p == "" {
		return true
	}
	if named != "" && (p == named || strings.HasPrefix(p, named+"/")) {
		return true
	}
	segs := strings.Split(p, "/")
	switch segs[0] {
	case "system", "user":
		return true
	case "projects", "derived":
		return len(segs) == 1 || s.repoSet()[segs[1]]
	case "machines":
		return len(segs) == 1 || (s.Host != "" && segs[1] == strings.ToLower(s.Host))
	}
	return false
}

// Named is the scope folder a path the caller asked for names: system or
// user, or projects/<x>, machines/<x> or derived/<x> (its first two parts);
// "" for the root, a scope root alone, or a path that is in no scope.
func Named(p string) string {
	segs := strings.Split(p, "/")
	switch segs[0] {
	case "system", "user":
		return segs[0]
	case "projects", "machines", "derived":
		if len(segs) >= 2 && segs[1] != "" {
			return segs[0] + "/" + segs[1]
		}
	}
	return ""
}

// Scope validates a scope the caller named (a search's scope filter): one
// scope folder, cleaned as Clean does and lower-cased.
func Scope(scope string) (string, error) {
	p, err := Clean(strings.ToLower(scope))
	if err != nil || p == "" || Named(p) != p {
		return "", fmt.Errorf("scope must be user, system, projects/<repo>, machines/<host> or derived/<repo>")
	}
	return p, nil
}
