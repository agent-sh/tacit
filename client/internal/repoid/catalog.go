package repoid

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Catalog is the repositories this machine has a checkout of, as Of names
// them: what the curator checks a project scope and applies_to.repos
// against. It is built from two sources, both read off the file system:
//
//   - the registry (RegistryFile in the state directory): every repository
//     the dreamer resolved a session's window to (Record), whatever the
//     directory layout; an entry counts only while its root is still a git
//     root;
//   - a scan of the projects directory ($TACIT_PROJECTS, else ~/projects)
//     two levels deep (an organisation folder of checkouts is one level).
//
// No name is guessed: a repository is in the catalog only when Of resolves a
// real directory to it.
type Catalog struct {
	repos   map[string]Repo   // slug -> repo
	aliases map[string]string // alias -> slug of the repo it names
}

// RegistryFile is the registry's name in the state directory.
const RegistryFile = "repos.json"

type registry struct {
	Repos map[string]regEntry `json:"repos"`
}

type regEntry struct {
	Root    string   `json:"root"`
	Aliases []string `json:"aliases,omitempty"`
	Seen    string   `json:"seen"`
}

// Record adds repos to the registry at path (atomic rewrite; a lost race
// with another writer loses at most that writer's new entries, which the
// next run records again).
func Record(path string, repos ...Repo) error {
	if len(repos) == 0 {
		return nil
	}
	reg := readRegistry(path)
	changed := false
	today := time.Now().Format("2006-01-02")
	for _, r := range repos {
		slug := r.Slug()
		if slug == "" || r.Root == "" {
			continue
		}
		e := reg.Repos[slug]
		if e.Root == r.Root && e.Seen == today && equal(e.Aliases, r.Aliases) {
			continue
		}
		aliases := e.Aliases
		if e.Root != r.Root {
			aliases = nil
		}
		for _, a := range r.Aliases {
			aliases = addAlias(aliases, slug, a)
		}
		reg.Repos[slug] = regEntry{Root: r.Root, Aliases: aliases, Seen: today}
		changed = true
	}
	if !changed {
		return nil
	}
	b, err := json.MarshalIndent(reg, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".repos-*.json")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(append(b, '\n'))
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), path)
	}
	if werr != nil {
		os.Remove(tmp.Name())
	}
	return werr
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func readRegistry(path string) registry {
	reg := registry{Repos: map[string]regEntry{}}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &reg)
	}
	if reg.Repos == nil {
		reg.Repos = map[string]regEntry{}
	}
	return reg
}

// LoadCatalog builds the catalog for home from the registry at registryPath
// ("" for none) and the projects directory projects ("" for none).
func LoadCatalog(home, registryPath, projects string) *Catalog {
	c := &Catalog{repos: map[string]Repo{}, aliases: map[string]string{}}
	cache := NewCache(home)
	if registryPath != "" {
		reg := readRegistry(registryPath)
		for _, slug := range sortedKeys(reg.Repos) {
			e := reg.Repos[slug]
			// Still a checkout, and still the same repository.
			if r, ok := cache.Of(e.Root); ok && r.Slug() == slug && r.Root == e.Root {
				for _, a := range e.Aliases {
					r.Aliases = addAlias(r.Aliases, r.Name, a)
				}
				c.add(r)
			}
		}
	}
	if projects != "" {
		c.scan(cache, projects, 2)
	}
	return c
}

func sortedKeys(m map[string]regEntry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// scan adds the git roots in dir down to depth levels.
func (c *Catalog) scan(cache *Cache, dir string, depth int) {
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
				c.add(r)
			}
			continue
		}
		if depth > 1 {
			c.scan(cache, p, depth-1)
		}
	}
}

func (c *Catalog) add(r Repo) {
	slug := r.Slug()
	if slug == "" {
		return
	}
	if old, ok := c.repos[slug]; ok {
		for _, a := range r.Aliases {
			old.Aliases = addAlias(old.Aliases, old.Name, a)
		}
		r = old
	}
	c.repos[slug] = r
	for _, a := range r.Aliases {
		if Slug(a) != "" {
			if _, taken := c.aliases[a]; !taken {
				c.aliases[a] = slug
			}
		}
	}
}

// Add puts r in the catalog (a caller that resolved one itself, such as the
// session's own cwd).
func (c *Catalog) Add(r Repo) { c.add(r) }

// Has reports a repository of this machine named slug (lower case).
func (c *Catalog) Has(slug string) bool {
	if c == nil {
		return false
	}
	_, ok := c.repos[strings.ToLower(slug)]
	return ok
}

// AliasOf is the slug of the repository alias names, when alias is only an
// alias here (not a repository's own name), else "".
func (c *Catalog) AliasOf(alias string) string {
	if c == nil {
		return ""
	}
	a := strings.ToLower(alias)
	if _, own := c.repos[a]; own {
		return ""
	}
	return c.aliases[a]
}

// Names lists the catalog's repository slugs, sorted.
func (c *Catalog) Names() []string {
	if c == nil {
		return nil
	}
	out := make([]string, 0, len(c.repos))
	for k := range c.repos {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
