// Package repoinfer names the repository a piece of agent activity is about
// when the session's working directory does not say: an agent started in
// the home directory or a folder of checkouts (~/workplace, ~/src) works in
// a repo by path, `cd` and `git -C`, and its facts belong to that repo, not
// to the folder it started in.
//
// A path counts for the repo whose root holds it: the nearest directory up
// the path with a .git entry (a checkout, a worktree, a Brazil package
// under <workspace>/src/<Package>). A path that no longer exists still
// counts when it has the <workspace>/src/<Package>/ shape. Paths in the home
// directory's dot directories (~/.claude, ~/.config) are agent and tool
// state, not repos, and never count.
package repoinfer

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// pathRx matches an absolute or home-relative path of two or more parts
// inside free text: a command, a tool input, a JSON string. It stops at
// quotes, spaces and the usual delimiters around a path in a command line.
// A match that follows a letter, digit, ':' or '/' is part of a URL or a
// relative path and is skipped (see paths).
var pathRx = regexp.MustCompile(`(?:~|/[A-Za-z0-9._@+-]+)(?:/[A-Za-z0-9._@+-]+)+`)

// paths are the absolute and ~ paths in t, URLs left out.
func paths(t string) []string {
	var out []string
	for _, ix := range pathRx.FindAllStringIndex(t, -1) {
		if ix[0] > 0 {
			c := t[ix[0]-1]
			if c == ':' || c == '/' || c == '.' || c == '_' || c == '-' || c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' {
				continue
			}
		}
		out = append(out, t[ix[0]:ix[1]])
	}
	return out
}

// srcRx is the Brazil workspace shape, <workspace>/src/<Package>/..., which
// names the package even when the workspace was deleted since.
var srcRx = regexp.MustCompile(`/src/([A-Za-z][A-Za-z0-9._-]+)(?:/|$)`)

var (
	mu    sync.Mutex
	roots = map[string]string{} // directory -> repo root ("" for none)
)

// Root is the root of the repository that holds path, or "" when no
// directory up to home (or /) has a .git entry.
func Root(path, home string) string {
	dir := filepath.Clean(path)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		dir = filepath.Dir(dir)
	}
	mu.Lock()
	defer mu.Unlock()
	var walked []string
	root := ""
	for d := dir; ; d = filepath.Dir(d) {
		if r, ok := roots[d]; ok {
			root = r
			break
		}
		walked = append(walked, d)
		if d == home || d == "/" || d == "." || d == filepath.Dir(d) {
			break
		}
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			root = d
			break
		}
	}
	for _, d := range walked {
		roots[d] = root
	}
	return root
}

// Name is the repo name for path: its root's base name, or the package of a
// <workspace>/src/<Package>/ path, or "" when the path is in no repo.
func Name(path, home string) string {
	p := filepath.Clean(expand(path, home))
	if home != "" && (p == home || strings.HasPrefix(p, home+string(filepath.Separator)+".")) {
		return ""
	}
	// A worktree belongs to its repository.
	for _, m := range []string{"/.claude/worktrees/", "/.local/runners/", "/.worktrees/"} {
		if i := strings.Index(p, m); i > 0 {
			p = p[:i]
		}
	}
	if r := Root(p, home); r != "" && r != home {
		return filepath.Base(r)
	}
	if m := srcRx.FindStringSubmatch(p); m != nil {
		return m[1]
	}
	return ""
}

func expand(p, home string) string {
	if home != "" && (p == "~" || strings.HasPrefix(p, "~/")) {
		return home + p[1:]
	}
	return p
}

// Counts tallies the repos the paths in texts belong to; weight multiplies
// each mention (a path the owner typed counts for more than one in a tool
// result).
func Counts(texts []string, home string, weight int, into map[string]int) map[string]int {
	if into == nil {
		into = map[string]int{}
	}
	for _, t := range texts {
		for _, p := range paths(t) {
			if n := Name(strings.TrimRight(p, ".,:;)"), home); n != "" {
				into[n] += weight
			}
		}
	}
	return into
}

// Dominant is the repo with most mentions when it has at least minCount of
// them and at least half of all mentions; "" when the activity is spread
// over several repos or touched none, so its facts are not filed under one.
func Dominant(counts map[string]int, minCount int) string {
	total, best, bestN := 0, "", 0
	names := make([]string, 0, len(counts))
	for n := range counts {
		names = append(names, n)
	}
	sort.Strings(names) // ties break the same way every run
	for _, n := range names {
		c := counts[n]
		total += c
		if c > bestN {
			best, bestN = n, c
		}
	}
	if bestN < minCount || 2*bestN < total {
		return ""
	}
	return best
}
