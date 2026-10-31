package derive

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/agent-sh/tacit/client/internal/secretscan"
	"github.com/agent-sh/tacit/client/internal/store"
)

// TreeFile is one tracked file at the derived commit.
type TreeFile struct {
	Path string
}

// Commit is one commit of the window, newest first.
type Commit struct {
	Date  string   // committer date, YYYY-MM-DD
	Total int      // code files the commit touched, deleted ones included
	Files []string // those still tracked at the derived commit and safe to show
}

// Snapshot is everything the facts are computed from: one commit, its tree
// and the last Window code-touching commits before it. All of it is a
// function of the commit, so the facts are too.
type Snapshot struct {
	Repo   string
	Sha    string // Sha12 of the commit
	Date   string // its committer date, YYYY-MM-DD
	Ref    string // what the commit was read from: "origin/main", a local branch name, or "HEAD"
	Branch string // the checkout's branch when Ref is HEAD; "" when detached
	// Configured is true when Ref is the branch the config names for this repo;
	// Local when that is a local branch because the remote has none.
	Configured bool
	Local      bool
	OddBranch  bool // Ref is HEAD on a branch whose name is not safe to quote
	Tree       []TreeFile
	Window     []Commit

	// Omitted counts tracked files whose names are outside the safe character
	// set (see Usable); they are neither shown nor counted in the tree.
	// OmittedTouched counts those that the window's commits touched.
	Omitted        int
	OmittedTouched int

	dir string
	rev string // full sha, for follow-up git calls
}

// gitArgs pins the git settings that change output shape, so two machines
// with different global config read the same history the same way.
var gitArgs = []string{"-c", "core.quotePath=false", "-c", "diff.renames=true", "-c", "log.showSignature=false", "-c", "color.ui=never"}

func run(ctx context.Context, dir string, args ...string) (string, error) {
	return store.Git(ctx, dir, append(append([]string(nil), gitArgs...), args...)...)
}

// Resolved is the commit a checkout is read at.
type Resolved struct {
	Full       string // full sha
	Date       string // committer date, YYYY-MM-DD
	Ref        string // "origin/main", a local branch name, or "HEAD"
	Branch     string // checkout branch when Ref is HEAD, "" if detached
	Configured bool   // Ref is the branch the config names for this repo
	Local      bool   // Ref is a local branch (the configured branch is not on the remote)
	Missing    string // the configured branch, when neither the remote nor a local branch has it
	OddBranch  bool   // HEAD is on a branch whose name is not safe to write into a fact
}

// TargetBranch is the remote branch the facts will be read from: the one the
// config names (configured true), else the remote default branch
// (origin/HEAD's target, else origin/main, else origin/master), else "" when
// the checkout knows none. It reads only refs the checkout already has.
func TargetBranch(ctx context.Context, dir, override string) (branch string, configured bool) {
	if override != "" {
		return override, true
	}
	if out, err := run(ctx, dir, "symbolic-ref", "-q", "refs/remotes/origin/HEAD"); err == nil {
		if t, ok := strings.CutPrefix(strings.TrimSpace(out), "refs/remotes/origin/"); ok && ShowableBranch(t) {
			return t, false
		}
	}
	for _, b := range []string{"main", "master"} {
		if _, _, err := commitOf(ctx, dir, "refs/remotes/origin/"+b); err == nil {
			return b, false
		}
	}
	return "", false
}

// ShowableBranch reports whether a branch name may be written into a fact
// body or compared as a remote default: a valid name in the same conservative
// character set and length bound as a path, and not secret-shaped. Git allows
// far more in a branch name (`;`, `|`, `'`, `$`), and the body is text an
// agent reads.
func ShowableBranch(name string) bool { return ValidBranch(name) && Usable(name) }

// Resolve picks the commit to derive from: the branch the config names for
// this repo (its remote-tracking ref, else a local branch of that name), else
// the remote default branch, so that whatever feature branch happens to be
// checked out never becomes a shared fact. A checkout with none of these (no
// remote, or never fetched) falls back to HEAD, and the branch is recorded.
func Resolve(ctx context.Context, dir, override string) (Resolved, error) {
	branch, configured := TargetBranch(ctx, dir, override)
	var r Resolved
	if configured {
		for _, c := range []string{"refs/remotes/origin/" + branch, "refs/heads/" + branch} {
			if full, date, err := commitOf(ctx, dir, c); err == nil {
				r = Resolved{Full: full, Date: date, Configured: true}
				if strings.HasPrefix(c, "refs/heads/") {
					r.Ref, r.Local = branch, true
				} else {
					r.Ref = "origin/" + branch
				}
				return r, nil
			}
		}
		r.Missing = branch
		branch, _ = TargetBranch(ctx, dir, "") // fall through to the default, and say so
	}
	if branch != "" {
		if full, date, err := commitOf(ctx, dir, "refs/remotes/origin/"+branch); err == nil {
			r.Full, r.Date, r.Ref = full, date, "origin/"+branch
			return r, nil
		}
	}
	full, date, err := commitOf(ctx, dir, "HEAD")
	if err != nil {
		return Resolved{}, err
	}
	r.Full, r.Date, r.Ref = full, date, "HEAD"
	if out, err := run(ctx, dir, "symbolic-ref", "-q", "--short", "HEAD"); err == nil {
		if name := strings.TrimSpace(out); ShowableBranch(name) {
			r.Branch = name
		} else {
			r.OddBranch = true // a name git allows but a fact must not quote
		}
	}
	return r, nil
}

func commitOf(ctx context.Context, dir, ref string) (full, date string, err error) {
	out, err := run(ctx, dir, "log", "-1", "--format=%H %cs", ref, "--")
	if err != nil {
		return "", "", err
	}
	f := strings.Fields(out)
	if len(f) != 2 {
		return "", "", fmt.Errorf("no commits at %s in %s", ref, dir)
	}
	return f[0], f[1], nil
}

// Sha12 is the abbreviation written to derived_from. A fixed length, so the
// text does not change as the repository grows and git would lengthen a
// short sha.
func Sha12(full string) string {
	if len(full) > 12 {
		return full[:12]
	}
	return full
}

// Collect reads the tree and the commit window of the checkout at dir, at the
// commit Resolve picked.
func Collect(ctx context.Context, dir, repo string, r Resolved) (*Snapshot, error) {
	s := &Snapshot{Repo: repo, Sha: Sha12(r.Full), Date: r.Date, Ref: r.Ref, Branch: r.Branch, Configured: r.Configured, Local: r.Local, OddBranch: r.OddBranch, dir: dir, rev: r.Full}

	out, err := run(ctx, dir, "ls-tree", "-r", "--name-only", "-z", r.Full)
	if err != nil {
		return nil, err
	}
	tracked := map[string]bool{}
	hidden := map[string]bool{}
	for _, p := range strings.Split(out, "\x00") {
		if p == "" {
			continue
		}
		if !Usable(p) {
			s.Omitted++
			hidden[p] = true
			continue
		}
		s.Tree = append(s.Tree, TreeFile{Path: p})
		tracked[p] = true
	}
	slices.SortFunc(s.Tree, func(a, b TreeFile) int { return strings.Compare(a.Path, b.Path) })

	args := []string{"log", r.Full, "--no-merges", "-" + strconv.Itoa(Window), "--name-only", "--format=tformat:%x01%cs", "--"}
	out, err = run(ctx, dir, append(args, pathspecs()...)...)
	if err != nil {
		return nil, err
	}
	var touchedHidden map[string]bool
	s.Window, touchedHidden = parseWindow(out, tracked, hidden)
	s.OmittedTouched = len(touchedHidden)
	return s, nil
}

// parseWindow reads `git log --name-only --format=tformat:%x01%cs` output.
// It also returns the distinct hidden (unsafe-named) files the window touched.
func parseWindow(out string, tracked, hidden map[string]bool) ([]Commit, map[string]bool) {
	var win []Commit
	touchedHidden := map[string]bool{}
	for _, blk := range strings.Split(out, "\x01") {
		if blk == "" {
			continue
		}
		lines := strings.Split(blk, "\n")
		c := Commit{Date: strings.TrimSpace(lines[0])}
		seen := map[string]bool{}
		for _, l := range lines[1:] {
			if l == "" || seen[l] {
				continue
			}
			seen[l] = true
			c.Total++
			switch {
			case tracked[l]:
				c.Files = append(c.Files, l)
			case hidden[l]:
				touchedHidden[l] = true
			}
		}
		slices.Sort(c.Files)
		win = append(win, c)
	}
	return win, touchedHidden
}

// lastChange is the date of the newest commit that touched anything under
// path (any file type), or "" when git has none.
func (s *Snapshot) lastChange(ctx context.Context, path string) string {
	out, err := run(ctx, s.dir, "log", "-1", "--format=%cs", s.rev, "--", path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// pathRe is the only shape of path that is written into a fact. File and
// directory names in a repository are text chosen by whoever committed them,
// and a fact's description is the line a hook prints into an agent's context;
// many repositories take outside contributions. So a name outside this
// conservative set is left out and counted, not quoted. The set also keeps
// out everything that could corrupt a fact: no `[[` (lint L6 would read a
// link), no backtick, quote, colon, space or newline.
var pathRe = regexp.MustCompile(`^[A-Za-z0-9._@+=,~-]+(/[A-Za-z0-9._@+=,~-]+)*$`)

// maxComponent bounds one path component. Real names are far shorter; a
// longer one is more likely a payload than a file.
const maxComponent = 100

// Usable reports whether a repo-relative path may be written into a fact.
func Usable(p string) bool {
	if !pathRe.MatchString(p) || secretscan.MatchString(p) {
		return false
	}
	for _, c := range strings.Split(p, "/") {
		if len(c) > maxComponent {
			return false
		}
	}
	return true
}
