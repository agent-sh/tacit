package check

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/agent-sh/tacit/client/internal/repoid"
)

// Checker runs the checks one machine can run: against its catalog of
// checkouts (with every other machine's shared catalog), its checkouts' git
// history, and the forge when one is configured.
type Checker struct {
	// Host is this machine's short name.
	Host string
	// Catalog is this machine's catalog (repoid), other machines' names in
	// it (AddShared) or not.
	Catalog *repoid.Catalog
	// Shared is every machine's shared catalog in the store, by name
	// (repoid.StoreEvidence).
	Shared map[string][]repoid.HostRepo
	// Forge, nil for none: no forge verdict.
	Forge *Cache
	// MaxOwners bounds the forge owners a bare name is looked for under.
	MaxOwners int
	Now       func() time.Time

	mu     sync.Mutex
	repos  map[string]RepoCheck
	owners []string
	trees  map[string]map[string]bool // root -> paths at HEAD
	heads  map[string]string
	tags   map[string][]string
}

// RepoCheck is what the sources say of one repository name.
type RepoCheck struct {
	Name string
	// Results: one per source asked (checkouts, remotes, forge), in that order.
	Results []Result
	// Verdict: Holds when a checkout, a remote or the forge has the name;
	// Fails when no checkout or remote has it and the forge answered that
	// no owner has it (or that it was renamed); Unknown otherwise.
	Verdict string
	// Checkout is the catalog repository the name is, or is another name of
	// ("" for none).
	Checkout string
	// Renamed is the name the forge says the repository lives under now
	// ("" when it was not renamed).
	Renamed string
	// Forge is the full name (owner/name) the forge has it under.
	Forge string
}

// Summary is the checks of a name as one result.
func (rc RepoCheck) Summary() Result {
	var srcs, outs []string
	at := ""
	for _, r := range rc.Results {
		srcs = append(srcs, r.Source)
		outs = append(outs, r.Source+" "+r.Verdict+": "+r.Output)
		if r.At > at {
			at = r.At
		}
	}
	return Result{Kind: KindRepo, Claim: "repository " + rc.Name + " exists under that name", Source: strings.Join(srcs, "+"),
		Inputs: inputs("name", rc.Name), Output: strings.Join(outs, "; "), Verdict: rc.Verdict, Value: rc.Forge, At: at}
}

// Lines is every source's result, one line each.
func (rc RepoCheck) Lines() []string { return Lines(rc.Results) }

func (c *Checker) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// forgeOwners are the forge accounts a bare name is looked for under: the
// forge's own user first, then the owners of the catalogs' remotes on the
// forge's host, most repositories first, at most MaxOwners.
func (c *Checker) forgeOwners(ctx context.Context) []string {
	if c.owners != nil || c.Forge == nil {
		return c.owners
	}
	count := map[string]int{}
	host := c.Forge.Forge.Host() + "/"
	addURLs := func(urls []string) {
		for _, u := range urls {
			if rest, ok := strings.CutPrefix(u, host); ok {
				if o, _, ok := strings.Cut(rest, "/"); ok && ownerRx.MatchString(o) {
					count[o]++
				}
			}
		}
	}
	for _, n := range c.Catalog.Names() {
		r, _ := c.Catalog.Repo(n)
		addURLs(r.URLs)
	}
	for _, hs := range c.Shared {
		for _, h := range hs {
			addURLs(h.URLs)
		}
	}
	var out []string
	if u := strings.ToLower(c.Forge.Forge.User(ctx)); u != "" {
		out = append(out, u)
		delete(count, u)
	}
	var rest []string
	for o := range count {
		rest = append(rest, o)
	}
	sort.Slice(rest, func(i, j int) bool {
		if count[rest[i]] != count[rest[j]] {
			return count[rest[i]] > count[rest[j]]
		}
		return rest[i] < rest[j]
	})
	out = append(out, rest...)
	max := c.MaxOwners
	if max <= 0 {
		max = 6
	}
	if len(out) > max {
		out = out[:max]
	}
	c.owners = out
	if c.owners == nil {
		c.owners = []string{}
	}
	return c.owners
}

// Repo checks that a repository named name (a slug) exists under that name:
// a checkout or another name of one in any machine's catalog, a remote URL
// that ends in it, and the forge.
func (c *Checker) Repo(ctx context.Context, name string) RepoCheck {
	name = strings.ToLower(strings.TrimSpace(name))
	c.mu.Lock()
	if c.repos == nil {
		c.repos = map[string]RepoCheck{}
	}
	if rc, ok := c.repos[name]; ok {
		c.mu.Unlock()
		return rc
	}
	c.mu.Unlock()
	rc := c.repo(ctx, name)
	c.mu.Lock()
	c.repos[name] = rc
	c.mu.Unlock()
	return rc
}

func (c *Checker) repo(ctx context.Context, name string) RepoCheck {
	rc := RepoCheck{Name: name}
	now := stamp(c.now())
	in := inputs("name", name)

	// Checkouts: this machine's, then every other machine's shared catalog.
	var where []string
	if r, ok := c.Catalog.Repo(name); ok && r.Root != "" {
		where = append(where, "a checkout on "+c.Host)
		rc.Checkout = r.Slug()
	}
	if own := c.Catalog.AliasOf(name); own != "" {
		where = append(where, fmt.Sprintf("another name (a symlink's or a remote's) of the checkout %s on %s", own, c.Host))
		if rc.Checkout == "" {
			rc.Checkout = own
		}
	}
	hosts := map[string]bool{c.Host: true}
	for _, h := range c.Shared[name] {
		hosts[h.Host] = true
		if h.Host != c.Host {
			where = append(where, "a checkout on "+h.Host)
			if rc.Checkout == "" {
				rc.Checkout = name
			}
		}
	}
	for n, hs := range c.Shared {
		for _, h := range hs {
			hosts[h.Host] = true
			for _, a := range h.Aliases {
				if a == name && n != name && h.Host != c.Host {
					where = append(where, fmt.Sprintf("another name of the checkout %s on %s", n, h.Host))
					if rc.Checkout == "" {
						rc.Checkout = n
					}
				}
			}
		}
	}
	var hl []string
	for h := range hosts {
		if h != "" {
			hl = append(hl, h)
		}
	}
	sort.Strings(hl)
	if len(where) > 0 {
		rc.Results = append(rc.Results, Result{Kind: KindRepo, Claim: "repository " + name + " has a checkout", Source: "checkouts", Inputs: in, Output: strings.Join(where, "; "), Verdict: Holds, Value: rc.Checkout, At: now})
	} else {
		rc.Results = append(rc.Results, Result{Kind: KindRepo, Claim: "repository " + name + " has a checkout", Source: "checkouts", Inputs: in, Output: "no checkout and no other name of one in the catalogs of " + strings.Join(hl, ", "), Verdict: Fails, At: now})
	}

	// Remotes: a checkout's remote URL whose last part is the name.
	var remotes []string
	seen := map[string]bool{}
	addRemote := func(u string) {
		if repoid.URLName(u) == name && !seen[u] {
			seen[u] = true
			remotes = append(remotes, u)
		}
	}
	for _, n := range c.Catalog.Names() {
		r, _ := c.Catalog.Repo(n)
		for _, u := range r.URLs {
			addRemote(u)
		}
	}
	for _, hs := range c.Shared {
		for _, h := range hs {
			for _, u := range h.URLs {
				addRemote(u)
			}
		}
	}
	sort.Strings(remotes)

	// The forge: the remotes' owners on its host first, then the owners a
	// bare name is looked for under.
	var forgeRes *Result
	if c.Forge != nil {
		f := c.Forge.Forge
		var owners []string
		addOwner := func(o string) {
			o = strings.ToLower(o)
			for _, x := range owners {
				if x == o {
					return
				}
			}
			owners = append(owners, o)
		}
		for _, u := range remotes {
			if rest, ok := strings.CutPrefix(u, f.Host()+"/"); ok {
				if o, _, ok := strings.Cut(rest, "/"); ok {
					addOwner(o)
				}
			}
		}
		// A name a checkout has needs the forge only to learn a rename of its
		// remote; a name nothing local has is looked for under every owner.
		if len(where) == 0 && len(remotes) == 0 {
			for _, o := range c.forgeOwners(ctx) {
				addOwner(o)
			}
		}
		var missing []string
		unavailable := 0
		at := ""
		for _, o := range owners {
			if !ValidRepo(o, name) {
				continue
			}
			r, when, err := c.Forge.Lookup(ctx, o, name)
			if ts := stamp(when); ts > at {
				at = ts
			}
			switch {
			case err == nil:
				_, got, _ := strings.Cut(r.FullName, "/")
				fr := Result{Kind: KindRepo, Claim: "repository " + name + " exists under that name", Source: "forge:" + f.Name(), Inputs: inputs("name", name, "owner", o), Value: r.FullName, At: stamp(when)}
				if strings.EqualFold(got, name) {
					fr.Verdict, fr.Output = Holds, fmt.Sprintf("%s/%s is %s", o, name, r.FullName)
					if r.Archived {
						fr.Output += " (archived)"
					}
					rc.Forge = r.FullName
				} else {
					fr.Verdict, fr.Output = Fails, fmt.Sprintf("%s/%s was renamed: it is %s now", o, name, r.FullName)
					rc.Forge, rc.Renamed = r.FullName, strings.ToLower(got)
				}
				forgeRes = &fr
			case errors.Is(err, ErrNotFound):
				missing = append(missing, o+"/"+name)
			default:
				unavailable++
			}
			if forgeRes != nil {
				break
			}
		}
		if forgeRes == nil {
			fr := Result{Kind: KindRepo, Claim: "repository " + name + " exists under that name", Source: "forge:" + f.Name(), Inputs: inputs("name", name, "owners", strings.Join(owners, ",")), At: at}
			if at == "" {
				fr.At = now
			}
			switch {
			case unavailable == 0 && len(missing) > 0:
				fr.Verdict, fr.Output = Fails, "not found: "+strings.Join(missing, ", ")
			case len(owners) == 0:
				fr.Verdict, fr.Output = Unknown, "no owner on the forge to look under"
			default:
				fr.Verdict, fr.Output = Unknown, fmt.Sprintf("the forge did not answer for %d of %d owner(s)", unavailable, len(owners))
			}
			if len(owners) > 0 || (len(where) == 0 && len(remotes) == 0) {
				forgeRes = &fr
			}
		}
	}
	// A remote counts unless the forge says that very repository is gone.
	if len(remotes) > 0 {
		v, out := Holds, "remote "+strings.Join(remotes, ", ")
		if forgeRes != nil && forgeRes.Verdict == Fails && rc.Renamed == "" {
			gone := true
			for _, u := range remotes {
				_, op, _ := strings.Cut(u, "/")
				if !strings.Contains(forgeRes.Output, op) {
					gone = false
				}
			}
			if gone {
				v, out = Fails, out+", which the forge no longer has"
			}
		}
		rc.Results = append(rc.Results, Result{Kind: KindRepo, Claim: "repository " + name + " is a remote of a checkout", Source: "remotes", Inputs: in, Output: out, Verdict: v, At: now})
	} else {
		rc.Results = append(rc.Results, Result{Kind: KindRepo, Claim: "repository " + name + " is a remote of a checkout", Source: "remotes", Inputs: in, Output: "no remote URL of any checkout ends in /" + name, Verdict: Fails, At: now})
	}
	if forgeRes != nil {
		rc.Results = append(rc.Results, *forgeRes)
	}

	local := rc.Results[0].Verdict == Holds || rc.Results[1].Verdict == Holds
	switch {
	case local || (forgeRes != nil && forgeRes.Verdict == Holds):
		rc.Verdict = Holds
	case forgeRes != nil && forgeRes.Verdict == Fails:
		rc.Verdict = Fails
	default:
		rc.Verdict = Unknown
	}
	return rc
}

// gitTimeout bounds one git call of a check.
const gitTimeout = 30 * time.Second

func git(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	out, err := cmd.Output()
	return string(out), err
}

// head is root's HEAD commit, "" when git cannot read it.
func (c *Checker) head(ctx context.Context, root string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.heads == nil {
		c.heads = map[string]string{}
	}
	if h, ok := c.heads[root]; ok {
		return h
	}
	out, err := git(ctx, root, "rev-parse", "--verify", "-q", "HEAD")
	h := ""
	if err == nil {
		h = strings.TrimSpace(out)
	}
	c.heads[root] = h
	return h
}

// tree is every path git tracks at root's HEAD (files and their folders),
// nil when git cannot list it.
func (c *Checker) tree(ctx context.Context, root string) map[string]bool {
	c.mu.Lock()
	if c.trees == nil {
		c.trees = map[string]map[string]bool{}
	}
	if t, ok := c.trees[root]; ok {
		c.mu.Unlock()
		return t
	}
	c.mu.Unlock()
	var t map[string]bool
	if out, err := git(ctx, root, "ls-tree", "-r", "--name-only", "-z", "HEAD"); err == nil {
		t = map[string]bool{}
		for _, p := range strings.Split(out, "\x00") {
			if p == "" {
				continue
			}
			t[p] = true
			for d := p; strings.Contains(d, "/"); {
				d = d[:strings.LastIndex(d, "/")]
				t[d] = true
			}
		}
	}
	c.mu.Lock()
	c.trees[root] = t
	c.mu.Unlock()
	return t
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// Path checks that rel exists in the checkout of repo at root, at HEAD: as
// written from the checkout's root, or as the end of a tracked path (a path
// written from a subfolder).
func (c *Checker) Path(ctx context.Context, repo, root, rel string) Result {
	rel = strings.Trim(strings.TrimPrefix(strings.TrimSpace(rel), "./"), "/")
	r := Result{Kind: KindPath, Claim: fmt.Sprintf("path %s exists in %s", rel, repo), Source: "git:HEAD", Inputs: inputs("repo", repo, "path", rel), At: stamp(c.now())}
	h := c.head(ctx, root)
	t := c.tree(ctx, root)
	if h == "" || t == nil {
		r.Verdict, r.Output = Unknown, "git could not read the checkout's HEAD"
		return r
	}
	r.Inputs["head"] = short(h)
	if t[rel] {
		r.Verdict, r.Output = Holds, fmt.Sprintf("at %s: %s is tracked", short(h), rel)
		return r
	}
	var ends []string
	for p := range t {
		if strings.HasSuffix(p, "/"+rel) {
			ends = append(ends, p)
		}
	}
	sort.Strings(ends)
	if len(ends) > 0 {
		if len(ends) > 3 {
			ends = ends[:3]
		}
		r.Verdict, r.Output, r.Value = Holds, fmt.Sprintf("at %s: tracked as %s", short(h), strings.Join(ends, ", ")), ends[0]
		return r
	}
	r.Verdict, r.Output = Fails, fmt.Sprintf("at %s: no tracked path is or ends in %s", short(h), rel)
	return r
}

func (c *Checker) tagList(ctx context.Context, root string) ([]string, bool) {
	c.mu.Lock()
	if c.tags == nil {
		c.tags = map[string][]string{}
	}
	if t, ok := c.tags[root]; ok {
		c.mu.Unlock()
		return t, t != nil
	}
	c.mu.Unlock()
	out, err := git(ctx, root, "tag", "-l")
	var t []string
	if err == nil {
		t = []string{}
		for _, l := range strings.Split(out, "\n") {
			if l = strings.TrimSpace(l); l != "" {
				t = append(t, l)
			}
		}
	}
	c.mu.Lock()
	c.tags[root] = t
	c.mu.Unlock()
	return t, err == nil
}

// Version checks that v (a version string, with or without a leading v) is
// a tag of the checkout of repo at root, or the version a manifest at HEAD
// declares (package.json, Cargo.toml, pyproject.toml, VERSION). It fails
// only when git read the tags and a manifest or a tag exists to compare
// with; a checkout with neither gives Unknown.
func (c *Checker) Version(ctx context.Context, repo, root, v string) Result {
	bare := strings.TrimPrefix(strings.TrimSpace(v), "v")
	r := Result{Kind: KindVersion, Claim: fmt.Sprintf("version %s is a release of %s", v, repo), Source: "git:tags", Inputs: inputs("repo", repo, "version", v), At: stamp(c.now())}
	tags, ok := c.tagList(ctx, root)
	if !ok {
		r.Verdict, r.Output = Unknown, "git could not read the checkout's tags"
		return r
	}
	for _, t := range tags {
		if t == bare || t == "v"+bare || strings.HasSuffix(t, "/v"+bare) || strings.HasSuffix(t, "/"+bare) {
			r.Verdict, r.Output, r.Value = Holds, "tag "+t, t
			return r
		}
	}
	h := c.head(ctx, root)
	var declared []string
	if h != "" {
		for _, m := range manifests {
			out, err := git(ctx, root, "show", "HEAD:"+m.file)
			if err != nil {
				continue
			}
			if mv := m.version(out); mv != "" {
				declared = append(declared, m.file+" "+mv)
				if strings.TrimPrefix(mv, "v") == bare {
					r.Source, r.Verdict, r.Output, r.Value = "manifest:"+m.file, Holds, fmt.Sprintf("at %s: %s declares %s", short(h), m.file, mv), mv
					return r
				}
			}
		}
	}
	if len(tags) == 0 && len(declared) == 0 {
		r.Verdict, r.Output = Unknown, "no tags and no manifest version to compare with"
		return r
	}
	r.Source = "git:tags+manifests"
	out := fmt.Sprintf("no tag %s among %d tag(s)", v, len(tags))
	if latest := latestTag(tags); latest != "" {
		out += ", latest " + latest
	}
	if len(declared) > 0 {
		out += "; HEAD declares " + strings.Join(declared, ", ")
	}
	r.Verdict, r.Output = Fails, out
	return r
}

// latestTag is the highest semantic version among tags, "" for none.
func latestTag(tags []string) string {
	best, bv := "", []int(nil)
	for _, t := range tags {
		v := semver(t)
		if v == nil {
			continue
		}
		if bv == nil || less(bv, v) {
			best, bv = t, v
		}
	}
	return best
}

func semver(t string) []int {
	t = t[strings.LastIndex(t, "/")+1:]
	t = strings.TrimPrefix(t, "v")
	if i := strings.IndexAny(t, "-+"); i >= 0 {
		t = t[:i]
	}
	parts := strings.Split(t, ".")
	if len(parts) != 3 {
		return nil
	}
	out := make([]int, 3)
	for i, p := range parts {
		n := 0
		if p == "" {
			return nil
		}
		for _, ch := range p {
			if ch < '0' || ch > '9' {
				return nil
			}
			n = n*10 + int(ch-'0')
		}
		out[i] = n
	}
	return out
}

func less(a, b []int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// Same is what the catalog proves of name and other repositories: one
// repository under several names. A name that is another name of a
// checkout (a symlink's, the origin remote's) is that checkout's; a
// checkout's other names are its own; two checkouts whose origin remotes
// have one normalized URL are clones of one repository. One result per
// other name, its Value the other name; none for none.
func (c *Checker) Same(name string) []Result {
	name = strings.ToLower(strings.TrimSpace(name))
	now := stamp(c.now())
	var out []Result
	seen := map[string]bool{name: true}
	add := func(other, output string) {
		other = strings.ToLower(other)
		if other == "" || seen[other] {
			return
		}
		seen[other] = true
		out = append(out, Result{Kind: KindRepo, Claim: fmt.Sprintf("%s and %s are one repository", name, other), Source: "checkouts",
			Inputs: inputs("name", name, "other", other), Output: output, Verdict: Holds, Value: other, At: now})
	}
	own, ok := c.Catalog.Repo(name)
	if !ok || own.Root == "" {
		if x := c.Catalog.AliasOf(name); x != "" {
			add(x, fmt.Sprintf("%s is another name (a symlink's or the origin remote's) of the checkout %s on %s", name, x, c.Host))
			own, ok = c.Catalog.Repo(x)
		}
	}
	if !ok || own.Root == "" {
		return out
	}
	for _, a := range own.Aliases {
		add(a, fmt.Sprintf("%s is another name (a symlink's or the origin remote's) of the checkout %s on %s", a, own.Slug(), c.Host))
	}
	if own.Origin != "" {
		for _, n := range c.Catalog.Names() {
			r, _ := c.Catalog.Repo(n)
			if r.Root != "" && r.Root != own.Root && r.Origin == own.Origin {
				add(n, fmt.Sprintf("the checkouts %s and %s on %s have one origin, %s", own.Slug(), n, c.Host, own.Origin))
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Value < out[j].Value })
	return out
}
