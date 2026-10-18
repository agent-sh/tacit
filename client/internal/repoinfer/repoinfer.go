// Package repoinfer names the repository a piece of agent activity is about
// when the session's working directory does not say: an agent started in
// the home directory or a folder of checkouts (~/workplace, ~/src) works in
// a repo by path, `cd` and `git -C`, and its facts belong to that repo, not
// to the folder it started in.
//
// Only paths under the home directory count. A path counts for the repo
// whose root holds it: the nearest directory up the path with a .git entry
// (a worktree, whose .git is a file, counts for its repository), or, for a
// Brazil package that was removed from a workspace that still exists, the
// <workspace>/src/<Package>/ shape. Paths in the home directory's dot
// directories (~/.claude, ~/.config) are agent and tool state, not repos,
// and never count. Relative paths (`cd widget`) and Windows paths are not
// read.
package repoinfer

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// pathRx matches an absolute or home-relative path of two or more parts
// inside free text: a command, a tool input, a JSON string. It stops at
// quotes, spaces and the usual delimiters around a path in a command line.
var pathRx = regexp.MustCompile(`(?:~|/[A-Za-z0-9._@+-]+)(?:/[A-Za-z0-9._@+-]+)+`)

// srcRx is the Brazil package shape <workspace>/src/<Package>/<more>.
var srcRx = regexp.MustCompile(`^(.+)/src/([A-Za-z][A-Za-z0-9._-]+)/[^/]`)

// workRx ends the text before a path that the agent works in, not one it
// only reads or runs a tool from: cd, git -C, an edit's target (digest T:
// lines, a Claude tool input's file_path, a Codex tool call's file:).
var workRx = regexp.MustCompile(`(?:\bcd\s+|\bgit\s+-C\s+|T: (?:Edit|Write|MultiEdit|NotebookEdit)\s+|"file_path"\s*:\s*"|\bfile:\s+)$`)

// Strong is how much more a path the agent works in (workRx) counts than one
// it only mentions. Measured on 283 dream windows (eval/VALUE.md): with work
// paths at 3 and a three-quarter majority, 23 of 29 inferred repos were
// right and none wrong; counting every path once, 23 of 40 right and 10
// wrong.
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
// directory once. It is not safe for concurrent use.
type Resolver struct {
	home  string            // symlinks resolved
	alias string            // home as given, when that differs (a symlinked home)
	roots map[string]string // directory -> repo root ("" for none)
	// MaxDirs caps the directories one Resolver looks at (0 is no cap); a
	// lookup past it names no repo. The prompt hook sets it to bound its
	// time on a slow file system.
	MaxDirs int
	// Stop, when set, is asked before each new directory; true stops the
	// lookups (a hook past its deadline).
	Stop func() bool
}

// NewResolver resolves paths under home, which is cleaned and has its
// symlinks resolved the way the paths' are.
func NewResolver(home string) *Resolver {
	r := &Resolver{home: real(home), roots: map[string]string{}}
	if h := filepath.Clean(home); home != "" && h != r.home {
		r.alias = h
	}
	return r
}

func real(p string) string {
	if p == "" {
		return ""
	}
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// root is the root of the repository that holds dir: the nearest directory
// up to home with a .git entry; "" when there is none or the lookup was cut.
func (r *Resolver) root(dir string) string {
	var walked []string
	root := ""
	for d := dir; ; d = filepath.Dir(d) {
		if v, ok := r.roots[d]; ok {
			root = v
			break
		}
		if d == r.home || !strings.HasPrefix(d, r.home+string(filepath.Separator)) {
			break
		}
		if (r.MaxDirs > 0 && len(r.roots) >= r.MaxDirs) || (r.Stop != nil && r.Stop()) {
			return ""
		}
		walked = append(walked, d)
		if fi, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			root = d
			if fi.Mode().IsRegular() {
				root = worktreeRepo(d)
			}
			break
		}
	}
	for _, d := range walked {
		r.roots[d] = root
	}
	return root
}

// worktreeRepo is the repository a worktree at dir belongs to: its .git file
// says "gitdir: <repo>/.git/worktrees/<name>". A file that says something
// else (a submodule) leaves dir its own root.
func worktreeRepo(dir string) string {
	fh, err := os.Open(filepath.Join(dir, ".git"))
	if err != nil {
		return dir
	}
	defer fh.Close()
	line, _ := bufio.NewReader(fh).ReadString('\n')
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(line), "gitdir:")
	if !ok {
		return dir
	}
	gitdir = strings.TrimSpace(gitdir)
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(dir, gitdir)
	}
	if i := strings.Index(filepath.ToSlash(gitdir), "/.git/worktrees/"); i > 0 {
		return filepath.Clean(gitdir[:i])
	}
	return dir
}

// Name is the repo name for path, or "" when the path is outside home, in
// home's dot directories, or in no repository.
func (r *Resolver) Name(path string) string {
	if r.home == "" {
		return ""
	}
	p := path
	if p == "~" || strings.HasPrefix(p, "~/") {
		p = r.home + p[1:]
	}
	p = filepath.Clean(p)
	if r.alias != "" && strings.HasPrefix(p, r.alias+string(filepath.Separator)) {
		p = r.home + p[len(r.alias):]
	}
	if !strings.HasPrefix(p, r.home+string(filepath.Separator)) || strings.HasPrefix(p, r.home+string(filepath.Separator)+".") {
		return ""
	}
	// A worktree belongs to its repository.
	for _, m := range []string{"/.claude/worktrees/", "/.local/runners/", "/.worktrees/"} {
		if i := strings.Index(p, m); i > 0 {
			p = p[:i]
		}
	}
	dir := p
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		dir = filepath.Dir(dir)
	}
	if root := r.root(dir); root != "" {
		name := filepath.Base(root)
		// ~/projects/<repo>-worktrees/<task> is <repo>'s.
		if parent := filepath.Base(filepath.Dir(root)); strings.HasSuffix(parent, "-worktrees") && parent != "-worktrees" {
			return strings.TrimSuffix(parent, "-worktrees")
		}
		return name
	}
	// A package removed from a Brazil workspace that still exists.
	if m := srcRx.FindStringSubmatch(p); m != nil {
		if _, err := os.Stat(filepath.Join(m[1], "packageInfo")); err == nil {
			return m[2]
		}
	}
	return ""
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
