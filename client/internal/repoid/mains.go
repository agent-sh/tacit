package repoid

import (
	"os"
	"path/filepath"
	"strings"
)

// Mains are the main checkouts of a machine: the git roots in its projects
// directory ($TACIT_PROJECTS, else ~/projects), two levels deep (an
// organisation folder of checkouts is one level). They are where the owner
// keeps their checkouts, so a checkout there is a repository under its own
// directory's name.
//
// A separate full clone elsewhere (its own .git directory, in a task folder,
// a scratch directory) is the same repository as the main checkout whose
// origin has the same normalized URL (NormURL), and Canonical names it so: a
// clone ~/tasks/t1/shop of the repository checked out as ~/projects/storefront
// is storefront, and shop becomes one of its aliases. Nothing else is guessed:
// a clone whose origin matches no main checkout, or more than one, keeps its
// own name, and two clones outside the projects directory are never merged
// with each other, since neither is known to be the main one.
type Mains struct {
	dir   string            // the projects directory, symlinks resolved
	byURL map[string][]Repo // normalized origin URL -> main checkouts
	all   []Repo            // every main checkout, in scan order
}

// ProjectsDir is the projects directory for home: $TACIT_PROJECTS, else
// home/projects.
func ProjectsDir(home string) string {
	if d := os.Getenv("TACIT_PROJECTS"); d != "" {
		return d
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, "projects")
}

// LoadMains scans projects ("" for none) for home's main checkouts.
func LoadMains(home, projects string) *Mains {
	m := &Mains{byURL: map[string][]Repo{}}
	if projects == "" {
		return m
	}
	m.dir = realPath(projects)
	cache := NewCache(home)
	at := map[string]int{} // root -> index in all
	scanRoots(cache, projects, 2, func(r Repo) {
		if i, ok := at[r.Root]; ok {
			// A symlink in the projects directory to another checkout there:
			// one repository, the symlink's name an alias.
			for _, a := range r.Aliases {
				m.all[i].Aliases = addAlias(m.all[i].Aliases, m.all[i].Name, a)
			}
			return
		}
		at[r.Root] = len(m.all)
		m.all = append(m.all, r)
	})
	for _, r := range m.all {
		if r.Origin != "" {
			m.byURL[r.Origin] = append(m.byURL[r.Origin], r)
		}
	}
	return m
}

// scanRoots calls fn for each git root in dir down to depth levels, as Of
// names it.
func scanRoots(cache *Cache, dir string, depth int, fn func(Repo)) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		fi, err := os.Stat(p) // a symlinked checkout counts
		if err != nil || !fi.IsDir() {
			continue
		}
		if _, err := os.Lstat(filepath.Join(p, ".git")); err == nil {
			if r, ok := cache.Of(p); ok {
				fn(r)
			}
			continue
		}
		if depth > 1 {
			scanRoots(cache, p, depth-1, fn)
		}
	}
}

// In reports whether root (real) is in the projects directory.
func (m *Mains) In(root string) bool {
	if m == nil || m.dir == "" {
		return false
	}
	return root == m.dir || strings.HasPrefix(root, m.dir+string(filepath.Separator))
}

// All are the main checkouts.
func (m *Mains) All() []Repo {
	if m == nil {
		return nil
	}
	return m.all
}

// Canonical is r named for the one main checkout whose origin has r's
// origin URL, when r is a separate clone of it (a different root outside
// the projects directory); r itself otherwise. The clone keeps its
// checkout (Worktree) and its own root in Clone; its own name and aliases
// become aliases of the main checkout's.
func (m *Mains) Canonical(r Repo) Repo {
	if m == nil || r.Origin == "" || m.In(r.Root) {
		return r
	}
	ms := m.byURL[r.Origin]
	if len(ms) != 1 || ms[0].Root == r.Root {
		return r
	}
	main := ms[0]
	out := r
	out.Name, out.Root, out.Clone = main.Name, main.Root, r.Root
	out.Aliases = nil
	for _, a := range append(append([]string{r.Name}, r.Aliases...), main.Aliases...) {
		out.Aliases = addAlias(out.Aliases, main.Name, a)
	}
	out.Remotes = nil
	for _, a := range append(append([]string(nil), r.Remotes...), main.Remotes...) {
		out.Remotes = addAlias(out.Remotes, main.Name, a)
	}
	for _, u := range main.URLs {
		out.URLs = addURL(out.URLs, u)
	}
	return out
}

// OfMachine is Of with a separate clone named for its main checkout
// (Mains.Canonical, the projects directory ProjectsDir). The projects
// directory is scanned only for a checkout outside it that has an origin,
// so a session in a main checkout costs no more than Of.
func OfMachine(path, home string) (Repo, bool) {
	r, ok := Of(path, home)
	if !ok || r.Origin == "" {
		return r, ok
	}
	projects := ProjectsDir(home)
	if projects == "" {
		return r, ok
	}
	if d := realPath(projects); r.Root == d || strings.HasPrefix(r.Root, d+string(filepath.Separator)) {
		return r, ok
	}
	return LoadMains(home, projects).Canonical(r), true
}

// NameMachine is OfMachine's Repo.Name, or "".
func NameMachine(path, home string) string {
	r, _ := OfMachine(path, home)
	return r.Name
}
