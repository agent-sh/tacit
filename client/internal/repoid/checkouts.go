package repoid

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// A checkout of a repository other than its main one (a linked worktree, a
// separate clone) is a folder with a name of its own: a session there may
// leave facts that later read as a project of that name. Its ancestor is
// known for sure only while the checkout exists, from git's own metadata (a
// worktree's gitfile names its main repository's git directory; a clone's
// origin names the repository it was cloned from). The registry keeps that
// relation from the first time a session or a scan sees the checkout, so
// the assessment still finds the ancestor after the checkout is removed,
// and the shared catalog carries the checkouts' directory names (never
// their paths) to every machine.

// Checkout kinds.
const (
	CheckoutWorktree = "worktree"
	CheckoutClone    = "clone"
	// CheckoutAny is a checkout another machine's shared catalog names,
	// which carries the names only: a linked worktree or a separate clone.
	CheckoutAny = "worktree or clone"
)

// Checkout is one recorded checkout that is not its repository's main one.
type Checkout struct {
	// Repo is the slug of the repository it belongs to: the main checkout's
	// name (a worktree, a clone of a main checkout), else the repository its
	// origin URL names (a clone of a repository with no main checkout here).
	Repo string `json:"repo"`
	// Main is the main checkout's root, "" for a clone with none here.
	Main string `json:"main,omitempty"`
	// Kind is CheckoutWorktree or CheckoutClone.
	Kind string `json:"kind"`
	// Origin is the origin remote's URL, normalized (NormURL), "" for none.
	Origin string `json:"origin,omitempty"`
	Seen   string `json:"seen"`
}

// CheckoutOf is the relation r's checkout has to its repository, when that
// checkout is no main checkout: a linked worktree (Worktree differs from
// Root), a separate clone named for its main checkout (Clone), or a clone
// outside the projects directory whose origin names another repository.
// path is the checkout's root.
func CheckoutOf(r Repo, mains *Mains) (path string, c Checkout, ok bool) {
	switch {
	case r.Clone != "":
		return r.Clone, Checkout{Repo: r.Slug(), Main: r.Root, Kind: CheckoutClone, Origin: r.Origin}, r.Slug() != ""
	case r.Worktree != "" && r.Worktree != r.Root:
		return r.Worktree, Checkout{Repo: r.Slug(), Main: r.Root, Kind: CheckoutWorktree, Origin: r.Origin}, r.Slug() != ""
	case r.Origin != "" && !mains.In(r.Root):
		// A clone of a repository this machine has no main checkout of: its
		// ancestor is what the origin names, when that is another name.
		if n := Slug(URLName(r.Origin)); n != "" && n != r.Slug() {
			return r.Root, Checkout{Repo: n, Kind: CheckoutClone, Origin: r.Origin}, true
		}
	}
	return "", Checkout{}, false
}

// RecordCheckouts adds the checkouts of repos that are no main checkout
// (CheckoutOf) to the registry at path, by the checkout's root. A relation
// already recorded is kept as it was (its Seen day moves on).
func RecordCheckouts(path string, mains *Mains, repos ...Repo) error {
	reg := readRegistry(path)
	today := time.Now().Format("2006-01-02")
	changed := false
	for _, r := range repos {
		p, c, ok := CheckoutOf(r, mains)
		if !ok || p == "" {
			continue
		}
		c.Seen = today
		if old, had := reg.Checkouts[p]; had && old.Repo == c.Repo && old.Kind == c.Kind && old.Main == c.Main && old.Origin == c.Origin && old.Seen == today {
			continue
		}
		if reg.Checkouts == nil {
			reg.Checkouts = map[string]Checkout{}
		}
		reg.Checkouts[p] = c
		changed = true
	}
	if !changed {
		return nil
	}
	return writeRegistry(path, reg)
}

// RecordedCheckout is the recorded checkout of the registry at path that p
// is in (the longest recorded root p is under), with that root.
func RecordedCheckout(path, p string) (string, Checkout, bool) {
	if p == "" {
		return "", Checkout{}, false
	}
	p = filepath.Clean(p)
	best := ""
	var out Checkout
	for root, c := range readRegistry(path).Checkouts {
		if (p == root || strings.HasPrefix(p, root+string(filepath.Separator))) && len(root) > len(best) {
			best, out = root, c
		}
	}
	return best, out, best != ""
}

// LinkedWorktrees are the linked worktrees git lists for the main checkout
// at root (<root>/.git/worktrees/*/gitdir, or <root>/worktrees for a bare
// repository): each worktree's directory, whether it still exists or not.
func LinkedWorktrees(root string) []string {
	admin := filepath.Join(root, ".git", "worktrees")
	if fi, err := os.Stat(filepath.Join(root, ".git")); err != nil || !fi.IsDir() {
		admin = filepath.Join(root, "worktrees")
	}
	entries, err := os.ReadDir(admin)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(admin, e.Name(), "gitdir"))
		if err != nil {
			continue
		}
		g := strings.TrimSpace(string(b))
		if g == "" {
			continue
		}
		if !filepath.IsAbs(g) {
			g = filepath.Join(admin, e.Name(), g)
		}
		out = append(out, filepath.Dir(filepath.Clean(g)))
	}
	sort.Strings(out)
	return out
}

// checkoutName is a recorded relation by the checkout directory's name.
type checkoutName struct {
	repo, kind string
}

// addCheckoutName records that the checkout directory name n belongs to
// repo; a name two repositories' checkouts share names neither.
func (c *Catalog) addCheckoutName(n, repo, kind string) {
	n = strings.ToLower(n)
	if Slug(n) == "" || repo == "" || n == repo {
		return
	}
	if c.checkouts == nil {
		c.checkouts = map[string]checkoutName{}
	}
	if old, ok := c.checkouts[n]; ok {
		if old.repo != repo {
			c.checkouts[n] = checkoutName{} // ambiguous: no answer
		}
		return
	}
	c.checkouts[n] = checkoutName{repo: repo, kind: kind}
}

// CheckoutOwner is the repository whose linked worktree or separate clone
// had the directory name name (recorded on this machine, listed by git for
// a main checkout here, or in another machine's shared catalog), with the
// kind; false when none did, or the checkouts of two repositories did.
func (c *Catalog) CheckoutOwner(name string) (repo, kind string, ok bool) {
	if c == nil || c.checkouts == nil {
		return "", "", false
	}
	cn, ok := c.checkouts[strings.ToLower(name)]
	if !ok || cn.repo == "" {
		return "", "", false
	}
	return cn.repo, cn.kind, true
}

// checkoutNamesOf are the directory names of slug's other checkouts that
// this machine knows (CheckoutOwner's answers for it), sorted.
func (c *Catalog) checkoutNamesOf(slug string) []string {
	var out []string
	for n, cn := range c.checkouts {
		if cn.repo == slug {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// loadCheckouts fills the catalog's checkout names: the registry's recorded
// checkouts, and the linked worktrees git lists for every repository of the
// catalog with a checkout here.
func (c *Catalog) loadCheckouts(reg registry) {
	roots := make([]string, 0, len(reg.Checkouts))
	for p := range reg.Checkouts {
		roots = append(roots, p)
	}
	sort.Strings(roots)
	for _, p := range roots {
		ch := reg.Checkouts[p]
		c.addCheckoutName(filepath.Base(p), ch.Repo, ch.Kind)
	}
	for _, slug := range c.Names() {
		r := c.repos[slug]
		if r.Root == "" {
			continue
		}
		for _, wt := range LinkedWorktrees(r.Root) {
			c.addCheckoutName(filepath.Base(wt), slug, CheckoutWorktree)
		}
	}
}
