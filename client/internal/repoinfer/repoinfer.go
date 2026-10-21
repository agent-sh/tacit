// Package repoinfer names the repository a piece of agent activity is about
// when the session's working directory does not say: an agent started in
// the home directory or a folder of checkouts (~/workplace, ~/src) works in
// a repo by path, `cd` and `git -C`, and its facts belong to that repo, not
// to the folder it started in.
//
// Only paths under the home directory count. A path counts for the repo
// repoid finds for it: the nearest real git root up the path, a worktree for
// its main repository. A folder's name never counts by itself. Paths in the
// home directory's dot directories (~/.claude, ~/.config) are agent and tool
// state, not repos, and never count. Relative paths (`cd widget`) and
// Windows paths are not read.
package repoinfer

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/agent-sh/tacit/client/internal/repoid"
)

// pathRx matches an absolute or home-relative path of two or more parts
// inside free text: a command, a tool input, a JSON string. It stops at
// quotes, spaces and the usual delimiters around a path in a command line.
var pathRx = regexp.MustCompile(`(?:~|/[A-Za-z0-9._@+-]+)(?:/[A-Za-z0-9._@+-]+)+`)

// workRx ends the text before a path that the agent works in, not one it
// only reads or runs a tool from: cd, git -C, an edit's target (digest T:
// lines, a Claude tool input's file_path, a Codex tool call's file:).
var workRx = regexp.MustCompile(`(?:\bcd\s+|\bgit\s+-C\s+|T: (?:Edit|Write|MultiEdit|NotebookEdit)\s+|"file_path"\s*:\s*"|\bfile:\s+)$`)

// Strong is how much more a path the agent works in (workRx) counts than one
// it only mentions. Measured in eval/VALUE.md ("The repo of a session that
// starts outside one"): with work paths at 3 and a three-quarter majority, 34
// of 57 sampled windows right, 11 partly and 12 wrong, against main's cwd
// rule at 12, 6 and 21 of 39.
const Strong = 3

type mention struct {
	path string
	work bool
}

// paths are the absolute and ~ paths in t. A match that follows a letter,
// digit, ':' or '/' is part of a URL or a relative path and is skipped; one
// that follows an escaped newline or tab (\n, \t, \r in a JSON string, as
// tool results are stored) starts a line and counts.
func paths(t string) []mention {
	var out []mention
	for _, ix := range pathRx.FindAllStringIndex(t, -1) {
		if i := ix[0]; i > 0 {
			c := t[i-1]
			escaped := i >= 2 && t[i-2] == '\\' && (c == 'n' || c == 't' || c == 'r')
			if !escaped && (c == ':' || c == '/' || c == '.' || c == '_' || c == '-' || c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z') {
				continue
			}
		}
		lo := max(0, ix[0]-24)
		out = append(out, mention{strings.TrimRight(t[ix[0]:ix[1]], "."), workRx.MatchString(t[lo:ix[0]])})
	}
	return out
}

// Resolver names repos for paths under one home directory and remembers the
// directories it has looked at, so one call over a transcript stats each
// directory once. The repository of a path is repoid's (a real git root,
// worktrees to their main repository, symlinks resolved); a path mentioned
// in text counts only under the home directory and outside its dot
// directories, so a system path that happens to be a checkout (a toolchain,
// a package manager) is not read as the session's work. It is not safe for
// concurrent use.
type Resolver struct {
	ids   *repoid.Cache
	roots map[string]string // name -> main checkout, for the names Name returned
	// MaxDirs caps the directories one Resolver stats (0 is no cap); a lookup
	// past it names no repo. The prompt hook sets it to bound its time on a
	// slow file system.
	MaxDirs int
	// Stop, when set, is asked before each new directory; true stops the
	// lookups (a hook past its deadline).
	Stop func() bool
}

// NewResolver resolves paths under home, which is cleaned and has its
// symlinks resolved the way the paths' are.
func NewResolver(home string) *Resolver {
	return &Resolver{ids: repoid.NewCache(home), roots: map[string]string{}}
}

// WithMains names a separate clone for its main checkout
// (repoid.Mains.Canonical) and returns r.
func (r *Resolver) WithMains(m *repoid.Mains) *Resolver {
	r.ids.Mains = m
	return r
}

// Stats is how many file system lookups the resolver has made (what MaxDirs
// caps).
func (r *Resolver) Stats() int { return r.ids.Stats() }

// Root is the main checkout of a repo name this resolver returned, or "".
func (r *Resolver) Root(name string) string { return r.roots[name] }

// Name is the repo name for path, or "" when the path is outside home, in
// home's dot directories, or in no repository.
func (r *Resolver) Name(path string) string {
	home := r.ids.Home()
	if home == "" {
		return ""
	}
	r.ids.MaxDirs, r.ids.Stop = r.MaxDirs, r.Stop
	p := r.ids.Expand(path)
	sep := string(filepath.Separator)
	if !strings.HasPrefix(p, home+sep) || strings.HasPrefix(p, home+sep+".") {
		return ""
	}
	repo, ok := r.ids.Of(p)
	if !ok {
		return ""
	}
	r.roots[repo.Name] = repo.Root
	return repo.Name
}

// Name is Resolver.Name with a resolver of its own.
func Name(path, home string) string { return NewResolver(home).Name(path) }

// Counts tallies the repos the paths in texts belong to: a path the agent
// works in counts Strong times, any other once, and weight multiplies both
// (a path the owner typed counts for more than one in a tool result).
func (r *Resolver) Counts(texts []string, weight int, into map[string]int) map[string]int {
	if into == nil {
		into = map[string]int{}
	}
	for _, t := range texts {
		for _, m := range paths(t) {
			if n := r.Name(m.path); n != "" {
				w := weight
				if m.work {
					w *= Strong
				}
				into[n] += w
			}
		}
	}
	return into
}

// Counts is Resolver.Counts with a resolver of its own.
func Counts(texts []string, home string, weight int, into map[string]int) map[string]int {
	return NewResolver(home).Counts(texts, weight, into)
}

// Dominant is the repo with most mentions when it has at least minCount of
// them and three quarters of all; "" when the activity is spread over
// several repos or touched none, so its facts are not filed under one. A
// wrong repo hides a fact from every other session, while no repo leaves it
// to the user and machine scopes: the bar is high on purpose.
func Dominant(counts map[string]int, minCount int) string {
	total, best, bestN := 0, "", 0
	names := make([]string, 0, len(counts))
	for n := range counts {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		c := counts[n]
		total += c
		if c > bestN {
			best, bestN = n, c
		}
	}
	if bestN < minCount || 4*bestN < 3*total {
		return ""
	}
	return best
}
