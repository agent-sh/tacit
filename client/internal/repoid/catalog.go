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
// real directory to it. A separate clone is named for its main checkout
// (Mains).
type Catalog struct {
	repos   map[string]Repo   // slug -> repo
	aliases map[string]string // alias -> slug of the repo it names
	home    string
	mains   *Mains
	work    map[string][]Work // root -> evidence (the registry's)
}

// RegistryFile is the registry's name in the state directory.
const RegistryFile = "repos.json"

type registry struct {
	Repos map[string]regEntry `json:"repos"`
	// Work is the evidence of real work in every checkout, by root: the
	// sessions that edited files or committed there (RecordWork). A
	// checkout outside home or in a temp directory (Elsewhere) counts as a
	// repository only with it; for every checkout it is what a project's
	// assessment reads (sessions, first and last day).
	Work map[string][]Work `json:"work,omitempty"`
	// Folders is the same evidence for folders of the projects directory
	// that are no git repository, by the folder's path.
	Folders map[string][]Work `json:"folders,omitempty"`
}

// Work is one session's evidence of real work in a checkout: By is "edit"
// (a write or edit tool call on a file in it) or "commit" (a git commit
// run in it).
type Work struct {
	Session string `json:"session"`
	By      string `json:"by"`
	Seen    string `json:"seen"`
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
	return writeRegistry(path, reg)
}

// maxWork caps the sessions kept per root, the oldest dropped first; the
// count an assessment reads is the sessions kept.
const maxWork = 400

// WorkItem is one session's evidence of work for RecordWorks: in the
// checkout Root, or in the non-git project folder Folder.
type WorkItem struct {
	Root, Folder string
	Session, By  string
	// Day is when (YYYY-MM-DD); "" is today.
	Day string
}

// RecordWork adds a session's evidence of work in the checkout root to the
// registry at path; a session already recorded for root is not added again.
func RecordWork(path, root, session, by string) error {
	return RecordWorks(path, []WorkItem{{Root: root, Session: session, By: by}})
}

// RecordWorks adds sessions' evidence of work to the registry at path in one
// rewrite; a session already recorded for its root or folder is not added
// again.
func RecordWorks(path string, items []WorkItem) error {
	reg := readRegistry(path)
	changed := false
	today := time.Now().Format("2006-01-02")
	for _, it := range items {
		if it.Session == "" || it.By == "" || (it.Root == "") == (it.Folder == "") {
			continue
		}
		m, key := &reg.Work, it.Root
		if it.Folder != "" {
			m, key = &reg.Folders, it.Folder
		}
		if *m == nil {
			*m = map[string][]Work{}
		}
		ws := (*m)[key]
		dup := false
		for _, w := range ws {
			dup = dup || w.Session == it.Session
		}
		if dup {
			continue
		}
		day := it.Day
		if day == "" {
			day = today
		}
		ws = append(ws, Work{Session: it.Session, By: it.By, Seen: day})
		if len(ws) > maxWork {
			ws = ws[len(ws)-maxWork:]
		}
		(*m)[key] = ws
		changed = true
	}
	if !changed {
		return nil
	}
	return writeRegistry(path, reg)
}

// WorkFor is the evidence the registry at path holds for the checkout root.
func WorkFor(path, root string) []Work { return readRegistry(path).Work[root] }

// FolderWork is the evidence the registry at path holds for non-git project
// folders, by folder path.
func FolderWork(path string) map[string][]Work { return readRegistry(path).Folders }

// WorkSummary is what evidence of work says: the distinct sessions, and the
// first and last day one was recorded.
func WorkSummary(ws []Work) (sessions int, first, last string) {
	seen := map[string]bool{}
	for _, w := range ws {
		seen[w.Session] = true
		if first == "" || w.Seen < first {
			first = w.Seen
		}
		if w.Seen > last {
			last = w.Seen
		}
	}
	return len(seen), first, last
}

// Sessions are the distinct session ids of ws, newest recorded last.
func Sessions(ws []Work) []string {
	var out []string
	seen := map[string]bool{}
	for _, w := range ws {
		if !seen[w.Session] {
			seen[w.Session] = true
			out = append(out, w.Session)
		}
	}
	return out
}

func writeRegistry(path string, reg registry) error {
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
	c := &Catalog{repos: map[string]Repo{}, aliases: map[string]string{}, home: home, mains: LoadMains(home, projects), work: map[string][]Work{}}
	for _, r := range c.mains.All() {
		c.add(r)
	}
	cache := NewCache(home)
	cache.Mains = c.mains
	if registryPath != "" {
		reg := readRegistry(registryPath)
		c.work = reg.Work
		for _, slug := range sortedKeys(reg.Repos) {
			e := reg.Repos[slug]
			// Still a checkout, and still the same repository; one outside
			// home or in a temp directory only with evidence of work in it.
			if r, ok := cache.Of(e.Root); ok && r.Slug() == slug && r.Root == e.Root && c.Accepted(r) {
				for _, a := range e.Aliases {
					r.Aliases = addAlias(r.Aliases, r.Name, a)
				}
				c.add(r)
			}
		}
	}
	return c
}

// Accepted reports whether r counts as a repository of this machine: any
// checkout under home outside its temp directories, a main checkout, or one
// elsewhere (Elsewhere) with work in it from MinWorkSessions sessions.
func (c *Catalog) Accepted(r Repo) bool {
	if c == nil || !Elsewhere(r.Root, c.home, c.mains) {
		return true
	}
	return WorkSessions(c.work[r.Root]) >= MinWorkSessions
}

// Work is the evidence the catalog's registry holds for the checkout root.
func (c *Catalog) Work(root string) []Work {
	if c == nil {
		return nil
	}
	return c.work[root]
}

// Home is the home directory the catalog was loaded for.
func (c *Catalog) Home() string {
	if c == nil {
		return ""
	}
	return c.home
}

// Mains are the main checkouts the catalog's projects directory holds.
func (c *Catalog) Mains() *Mains {
	if c == nil {
		return nil
	}
	return c.mains
}

// Of is the repository path is in, a separate clone named for its main
// checkout.
func (c *Catalog) Of(path string) (Repo, bool) {
	cache := NewCache(c.Home())
	cache.Mains = c.Mains()
	return cache.Of(path)
}

func sortedKeys(m map[string]regEntry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
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

// Repo is the catalog's repository named slug: a checkout of this machine
// (Root set) or a name another machine's catalog added (Root empty).
func (c *Catalog) Repo(slug string) (Repo, bool) {
	if c == nil {
		return Repo{}, false
	}
	r, ok := c.repos[strings.ToLower(slug)]
	return r, ok
}

// LoadRegistryWork is the registry's evidence of work at path, checkouts
// and non-git folders together, by root or folder.
func LoadRegistryWork(path string) map[string][]Work {
	reg := readRegistry(path)
	out := map[string][]Work{}
	for k, v := range reg.Work {
		out[k] = v
	}
	for k, v := range reg.Folders {
		out[k] = append(out[k], v...)
	}
	return out
}
