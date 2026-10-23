// Package repoid is tacit's one answer to "which repository is this path in".
// The dreamer (a session's cwd, the paths in a window), the curator (which
// project scopes and repo names are real), the hooks and the MCP server (the
// session's repo for delivery) and the tree index all ask it, so a fact is
// filed under the name it is later looked up by.
//
// A path names a repository only through a real git root: the nearest
// directory up the path, with symlinks resolved, that holds a .git directory
// or a .git file (a gitfile). A linked worktree's gitfile points into its main
// repository's .git/worktrees/<name> directory, and the worktree belongs to
// that main repository; a gitfile without one (a submodule) is its own
// repository. The name is the base name of the main checkout's real directory
// (symlinks resolved), so a symlinked checkout and all its worktrees have one
// name. A folder's name never names a repository by itself: a directory with
// no git root above it is in no repository, wherever it is.
//
// Three places are never a repository: the home directory itself (a dotfiles
// checkout there would make every session one project), the home directory's
// dot directories (~/.claude, ~/.cache, ~/.config: agent and tool state), and
// a main repository whose root is one of those. A worktree of a real
// repository kept in a dot directory (~/.codex/worktrees/x) still belongs to
// its main repository.
//
// The lookups read the file system only (no git process), so a hook can ask
// about many paths inside its budget; a Cache remembers each directory.
package repoid

import (
	"bufio"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Repo is the repository a path is in.
type Repo struct {
	// Name is the base name of the main checkout's real directory, as on disk
	// (case kept). Scope is its lower-case form when that is a slug.
	Name string
	// Root is the main checkout's real directory (symlinks resolved); for a
	// bare main repository, its git directory.
	Root string
	// Worktree is the checkout the path is in (a linked worktree, or Root).
	Worktree string
	// Aliases are other names a session may know the repository by, for
	// delivery only: the lexical name of a symlinked checkout the path ran
	// through, and the last part of the origin remote's URL. Lower case,
	// without Name. They never name a scope.
	Aliases []string
	// Remotes are the repository names of every remote in the repository's
	// config (origin's too: a fork's upstream, a mirror, the URL a renamed
	// repository still has), lower case, without Name, sorted. Delivery may
	// add them to Aliases (the repo-aliases setting); the catalog does not.
	Remotes []string
	// Origin is the origin remote's URL normalized (NormURL): host and path,
	// lower case, no scheme, user, port or .git; "" for none or a remote on
	// this file system. URLs are every remote's, the same way, sorted.
	Origin string
	URLs   []string
	// Clone is the root of a separate full clone (its own .git directory)
	// that Mains.Canonical named for the main checkout with the same origin;
	// "" otherwise. Worktree is still the clone's checkout.
	Clone string
}

// Slug is the project scope name of r: Name in lower case when that is a
// store slug ([a-z0-9][a-z0-9-]*), else "" (no project scope can hold it).
func (r Repo) Slug() string { return Slug(r.Name) }

var slugRx = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Slug is name in lower case when that is a store slug, else "".
func Slug(name string) string {
	s := strings.ToLower(name)
	if !slugRx.MatchString(s) {
		return ""
	}
	return s
}

// Of is the repository path is in, under home's rules (see the package
// comment), and false when there is none. path may name a file or directory
// that no longer exists: its nearest existing ancestor is looked up. A "~" or
// "~/" prefix is home.
func Of(path, home string) (Repo, bool) { return NewCache(home).Of(path) }

// Name is Of's Repo.Name, or "".
func Name(path, home string) string {
	r, _ := Of(path, home)
	return r.Name
}

// Cache answers Of for one home directory and remembers each directory it
// looked at. It is not safe for concurrent use.
type Cache struct {
	home  string // symlinks resolved
	alias string // home as given, when that differs (a symlinked home)
	dirs  map[string]found
	reals map[string]string // directory -> its real path
	files map[string]string // a path in a real directory -> its real path
	stats int               // file system lookups made
	// MaxDirs caps the directories one Cache stats (0 is no cap); a lookup
	// past it finds no repository. Stop, when set, is asked before each new
	// directory; true ends the lookups (a hook past its deadline).
	MaxDirs int
	Stop    func() bool
	// Mains, when set, names a separate clone for its main checkout
	// (Mains.Canonical) in every answer of Of.
	Mains *Mains
}

type found struct {
	repo Repo
	ok   bool
}

// NewCache resolves paths under home (cleaned, symlinks resolved).
func NewCache(home string) *Cache {
	c := &Cache{home: realPath(home), dirs: map[string]found{}, reals: map[string]string{}, files: map[string]string{}}
	if h := filepath.Clean(home); home != "" && h != c.home {
		c.alias = h
	}
	return c
}

// Home is the cache's home directory, symlinks resolved.
func (c *Cache) Home() string { return c.home }

func (c *Cache) cut() bool {
	return (c.MaxDirs > 0 && c.stats >= c.MaxDirs) || (c.Stop != nil && c.Stop())
}

// Stats is how many file system lookups the cache has made (what MaxDirs
// caps).
func (c *Cache) Stats() int { return c.stats }

// real is p with symlinks resolved, its directory's answer remembered.
func (c *Cache) real(p string) string {
	d, base := filepath.Dir(p), filepath.Base(p)
	rd, ok := c.reals[d]
	if !ok {
		c.stats++
		rd = realPath(d)
		c.reals[d] = rd
	}
	if d == p {
		return rd
	}
	cand := filepath.Join(rd, base)
	if r, ok := c.files[cand]; ok {
		return r
	}
	c.stats++
	r := cand
	if fi, err := os.Lstat(cand); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		r = realPath(cand)
	}
	c.files[cand] = r
	return r
}

// Expand makes path absolute and clean: a "~" prefix is home, and a path
// under home as given (a symlinked home) is moved under the real home.
func (c *Cache) Expand(path string) string {
	p := strings.TrimSpace(path)
	if p == "" {
		return ""
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		if c.home == "" {
			return ""
		}
		p = c.home + p[1:]
	}
	if !filepath.IsAbs(p) {
		return ""
	}
	p = filepath.Clean(p)
	if c.alias != "" && (p == c.alias || strings.HasPrefix(p, c.alias+string(filepath.Separator))) {
		p = c.home + p[len(c.alias):]
	}
	return p
}

// Of is the repository path is in; see the package Of.
func (c *Cache) Of(path string) (Repo, bool) {
	p := c.Expand(path)
	if p == "" || c.cut() {
		return Repo{}, false
	}
	// The lexical checkout a symlink ran through names an alias: with
	// ~/projects/old -> new, a session in ~/projects/old knows the repo as old.
	real := c.real(p)
	if c.cut() {
		return Repo{}, false
	}
	r, ok := c.lookup(real)
	if !ok {
		return Repo{}, false
	}
	if real != p {
		if lex := lexicalCheckout(p, real, r.Worktree); lex != "" {
			r.Aliases = addAlias(r.Aliases, r.Name, lex)
		}
	}
	if c.Mains != nil {
		r = c.Mains.Canonical(r)
	}
	return r, true
}

// lexicalCheckout is the base name the lexical path p gives the checkout
// that real (p with symlinks resolved) is in, when a symlink renamed it.
func lexicalCheckout(p, real, worktree string) string {
	rel, err := filepath.Rel(worktree, real)
	if err != nil || strings.HasPrefix(rel, "..") {
		return ""
	}
	lex := p
	if rel != "." {
		n := len(strings.Split(rel, string(filepath.Separator)))
		for i := 0; i < n; i++ {
			lex = filepath.Dir(lex)
		}
	}
	if b := filepath.Base(lex); b != filepath.Base(worktree) {
		return b
	}
	return ""
}

// lookup walks up from dir (real, absolute) to the nearest git root.
func (c *Cache) lookup(dir string) (Repo, bool) {
	var walked []string
	res := found{}
	for d := dir; ; d = filepath.Dir(d) {
		if v, ok := c.dirs[d]; ok {
			res = v
			break
		}
		if c.cut() {
			return Repo{}, false
		}
		walked = append(walked, d)
		c.stats++
		if r, isRoot, ok := c.rootAt(d); isRoot {
			res = found{r, ok}
			break
		}
		if filepath.Dir(d) == d {
			break
		}
	}
	for _, d := range walked {
		c.dirs[d] = res
	}
	return res.repo, res.ok
}

// rootAt reports whether d holds a .git entry (isRoot) and, when it does,
// the repository it makes (ok false: a git root that names no repository
// here, such as home, a dot directory, or a gitfile that points nowhere; the
// walk stops there, as git's own does).
func (c *Cache) rootAt(d string) (r Repo, isRoot, ok bool) {
	gitPath := filepath.Join(d, ".git")
	fi, err := os.Lstat(gitPath)
	if err != nil {
		return Repo{}, false, false
	}
	var common, main, name string
	switch {
	case fi.IsDir():
		common = gitPath
	case fi.Mode().IsRegular():
		gitdir, gerr := readGitfile(gitPath)
		if gerr != nil {
			return Repo{}, true, false
		}
		var linked bool
		common, linked = commonDir(gitdir)
		if common == "" {
			return Repo{}, true, false
		}
		if !linked {
			// A gitfile whose git directory is its own (a submodule): the
			// checkout holding the gitfile is the repository.
			main = realPath(d)
			name = filepath.Base(main)
		}
	default:
		return Repo{}, false, false // a symlink named .git is not followed
	}
	if main == "" {
		if filepath.Base(common) == ".git" {
			main = realPath(filepath.Dir(common))
			name = filepath.Base(main)
		} else {
			// A bare main repository (repo.git) with linked worktrees.
			main = realPath(common)
			name = strings.TrimSuffix(filepath.Base(main), ".git")
		}
	}
	if name == "" || name == "." || name == string(filepath.Separator) || c.excluded(main) {
		return Repo{}, true, false
	}
	r = Repo{Name: name, Root: main, Worktree: d}
	rem := readRemotes(common)
	if rem.origin != "" {
		r.Aliases = addAlias(r.Aliases, name, rem.origin)
	}
	for _, n := range rem.names {
		r.Remotes = addAlias(r.Remotes, name, n)
	}
	r.Origin = rem.originURL
	for _, u := range rem.urls {
		r.URLs = addURL(r.URLs, u)
	}
	return r, true, true
}

// excluded: home itself and anything in home's dot directories.
func (c *Cache) excluded(main string) bool {
	if c.home == "" {
		return false
	}
	if main == c.home {
		return true
	}
	return strings.HasPrefix(main, c.home+string(filepath.Separator)+".")
}

// readGitfile is the directory a .git file points at ("gitdir: <path>",
// relative to the file's directory when not absolute).
func readGitfile(path string) (string, error) {
	fh, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer fh.Close()
	line, _ := bufio.NewReader(fh).ReadString('\n')
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(line), "gitdir:")
	if !ok || strings.TrimSpace(gitdir) == "" {
		return "", errors.New("not a gitfile")
	}
	gitdir = strings.TrimSpace(gitdir)
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(filepath.Dir(path), gitdir)
	}
	return filepath.Clean(gitdir), nil
}

// commonDir is the common git directory for the git directory a gitfile
// names, as git finds it: the gitdir must be a git directory (it has a HEAD;
// otherwise git refuses the checkout, and so does this); a linked worktree's
// gitdir has a commondir file naming the main repository's git directory
// (linked true); without one the gitdir is its own (a submodule). "" when
// it names no git directory.
func commonDir(gitdir string) (string, bool) {
	if _, err := os.Stat(filepath.Join(gitdir, "HEAD")); err != nil {
		return "", false
	}
	if b, err := os.ReadFile(filepath.Join(gitdir, "commondir")); err == nil {
		cd := strings.TrimSpace(string(b))
		if cd != "" {
			if !filepath.IsAbs(cd) {
				cd = filepath.Join(gitdir, cd)
			}
			cd = filepath.Clean(cd)
			if fi, err := os.Stat(cd); err == nil && fi.IsDir() {
				return cd, true
			}
			return "", false
		}
	}
	return gitdir, false
}

var (
	urlRx     = regexp.MustCompile(`^\s*url\s*=\s*(\S+)\s*$`)
	remoteSec = regexp.MustCompile(`^\[remote\s+"([^"]*)"\]$`)
)

// remotes is what a repository's config says of its remotes.
type remotes struct {
	origin    string   // the repository name of origin's URL (URLName)
	names     []string // the same for every remote's URL
	originURL string   // origin's URL, normalized (NormURL)
	urls      []string // every remote's URL, normalized, where it is a network URL
}

// readRemotes reads the repository's config (no git process); the zero
// value when there is none.
func readRemotes(common string) (out remotes) {
	fh, err := os.Open(filepath.Join(common, "config"))
	if err != nil {
		return out
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	remote, in := "", false
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(l, "[") {
			m := remoteSec.FindStringSubmatch(l)
			in = m != nil
			if in {
				remote = m[1]
			}
			continue
		}
		if !in {
			continue
		}
		if m := urlRx.FindStringSubmatch(l); m != nil {
			n := URLName(m[1])
			if n == "" {
				continue
			}
			out.names = append(out.names, n)
			u := NormURL(m[1])
			if u != "" {
				out.urls = append(out.urls, u)
			}
			if remote == "origin" && out.origin == "" {
				out.origin, out.originURL = n, u
			}
		}
	}
	return out
}

// NormURL is a remote URL as one repository's identity on every machine:
// host and path, lower case, with no scheme, user name or password, port,
// trailing slash or .git ("git@github.com:Acme/Widget.git" and
// "https://u:tok@github.com/acme/widget" are both "github.com/acme/widget").
// A remote on the file system (a path, a file:// URL) is "": it says nothing
// another machine can match, and it is a local path.
func NormURL(u string) string {
	u = strings.TrimSpace(u)
	var host, p string
	if i := strings.Index(u, "://"); i >= 0 {
		if strings.EqualFold(u[:i], "file") {
			return ""
		}
		rest := u[i+3:]
		j := strings.IndexByte(rest, '/')
		if j < 0 {
			return ""
		}
		host, p = rest[:j], rest[j+1:]
	} else {
		// scp-like: [user@]host:path, with the colon before any slash; a
		// one-letter host is a Windows drive.
		colon, slash := strings.IndexByte(u, ':'), strings.IndexByte(u, '/')
		if colon <= 1 || (slash >= 0 && slash < colon) {
			return ""
		}
		host, p = u[:colon], u[colon+1:]
	}
	if at := strings.LastIndexByte(host, '@'); at >= 0 {
		host = host[at+1:]
	}
	if c := strings.LastIndexByte(host, ':'); c >= 0 && !strings.Contains(host, "]") {
		host = host[:c]
	}
	host = strings.ToLower(host)
	p = strings.Trim(strings.TrimSuffix(strings.Trim(p, "/"), ".git"), "/")
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	if host == "" || p == "" || strings.ContainsAny(host, "/\\ ") {
		return ""
	}
	return host + "/" + strings.ToLower(p)
}

func addURL(urls []string, u string) []string {
	for _, x := range urls {
		if x == u {
			return urls
		}
	}
	out := append(append([]string(nil), urls...), u)
	sort.Strings(out)
	return out
}

// URLName is the repository name of a remote URL or an owner/repo form: its
// last path part without .git, lower case.
func URLName(u string) string {
	u = strings.TrimSuffix(strings.TrimRight(strings.TrimSpace(u), "/"), ".git")
	if i := strings.LastIndexAny(u, "/:"); i >= 0 {
		u = u[i+1:]
	}
	return strings.ToLower(u)
}

func addAlias(aliases []string, name, a string) []string {
	a = strings.ToLower(a)
	if a == "" || a == strings.ToLower(name) {
		return aliases
	}
	for _, x := range aliases {
		if x == a {
			return aliases
		}
	}
	out := append(append([]string(nil), aliases...), a)
	sort.Strings(out)
	return out
}

// realPath is p with every symlink resolved; for a path that does not exist,
// its nearest existing ancestor is resolved and the rest appended.
func realPath(p string) string {
	if p == "" {
		return ""
	}
	p = filepath.Clean(p)
	var rest []string
	for cur := p; ; {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			for i := len(rest) - 1; i >= 0; i-- {
				r = filepath.Join(r, rest[i])
			}
			return r
		} else if !errors.Is(err, fs.ErrNotExist) {
			return p
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = append(rest, filepath.Base(cur))
		cur = parent
	}
}

// RealPath is p with symlinks resolved as Of resolves it.
func RealPath(p string) string { return realPath(p) }
