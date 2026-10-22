package check

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// KindAccount is a claim that a name is an account or organization on the
// forge (a folder named for one holds facts about many of its
// repositories: it is not one repository).
const KindAccount = "account"

// CheckoutName is the name repoid gives the repository that name is, or is
// another name of, by every machine's catalog: the base name of its main
// checkout's directory (a symlink's name and the origin remote's name are
// aliases). canon is "" when no catalog has name, or when machines disagree
// (their main checkouts have different directory names); where says which
// catalogs said what, one item each, in host order.
func (c *Checker) CheckoutName(name string) (canon string, where []string) {
	name = strings.ToLower(strings.TrimSpace(name))
	seen := map[string]bool{}
	var names []string
	add := func(n, w string) {
		n = strings.ToLower(n)
		where = append(where, w)
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	if c.Catalog != nil {
		if r, ok := c.Catalog.Repo(name); ok && r.Root != "" {
			add(r.Slug(), fmt.Sprintf("%s is the main checkout's directory on %s", r.Slug(), c.Host))
		} else if own := c.Catalog.AliasOf(name); own != "" {
			if r, ok := c.Catalog.Repo(own); ok && r.Root != "" {
				add(own, fmt.Sprintf("%s is another name (a symlink's or the origin remote's) of the checkout %s on %s", name, own, c.Host))
			}
		}
	}
	hosts := make([]string, 0)
	byHost := map[string][]string{}
	for n, hs := range c.Shared {
		for _, h := range hs {
			if h.Host == c.Host {
				continue
			}
			match := n == name
			for _, a := range h.Aliases {
				match = match || strings.ToLower(a) == name
			}
			if !match {
				continue
			}
			if byHost[h.Host] == nil {
				hosts = append(hosts, h.Host)
			}
			byHost[h.Host] = append(byHost[h.Host], n)
		}
	}
	sort.Strings(hosts)
	for _, h := range hosts {
		ns := byHost[h]
		sort.Strings(ns)
		for _, n := range ns {
			if n == name {
				add(n, fmt.Sprintf("%s is the main checkout's directory on %s", n, h))
			} else {
				add(n, fmt.Sprintf("%s is another name (a symlink's or the origin remote's) of the checkout %s on %s", name, n, h))
			}
		}
	}
	if len(names) == 1 {
		return names[0], where
	}
	return "", where
}

// sharedSame are the other names the shared catalogs of other machines say
// are name's repository: an entry's aliases when name is its checkout's
// name, and the checkout's own name and its other aliases when name is one
// of its aliases.
func (c *Checker) sharedSame(name string, add func(other, output string)) {
	var keys []string
	for n := range c.Shared {
		keys = append(keys, n)
	}
	sort.Strings(keys)
	for _, n := range keys {
		for _, h := range c.Shared[n] {
			if h.Host == c.Host {
				continue
			}
			isAlias := false
			for _, a := range h.Aliases {
				isAlias = isAlias || strings.ToLower(a) == name
			}
			switch {
			case n == name:
				for _, a := range h.Aliases {
					add(a, fmt.Sprintf("%s is another name (a symlink's or the origin remote's) of the checkout %s on %s", strings.ToLower(a), n, h.Host))
				}
			case isAlias:
				add(n, fmt.Sprintf("%s is another name (a symlink's or the origin remote's) of the checkout %s on %s", name, n, h.Host))
				for _, a := range h.Aliases {
					if strings.ToLower(a) != name {
						add(a, fmt.Sprintf("%s and %s are other names of the checkout %s on %s", name, strings.ToLower(a), n, h.Host))
					}
				}
			}
		}
	}
}

// ForgeAccount is a forge's answer for an account name.
type ForgeAccount struct {
	Login string `json:"login"`
	// Type is the forge's word: "User" or "Organization" on GitHub.
	Type string `json:"type"`
}

// AccountForge is a forge that can tell an account or organization from a
// repository (GitHub: `gh api users/<name>`).
type AccountForge interface {
	Account(ctx context.Context, name string) (ForgeAccount, error)
}

// Account is the GitHub account or organization name, or ErrNotFound.
func (g *GitHub) Account(ctx context.Context, name string) (ForgeAccount, error) {
	if !ownerRx.MatchString(name) {
		return ForgeAccount{}, ErrNotFound
	}
	out, errb, err := g.run(ctx, g.Bin, "api", "users/"+name)
	if err != nil {
		all := string(out) + string(errb)
		if strings.Contains(all, "HTTP 404") || strings.Contains(all, `"status":"404"`) {
			return ForgeAccount{}, ErrNotFound
		}
		return ForgeAccount{}, ErrUnavailable
	}
	var a ForgeAccount
	if json.Unmarshal(out, &a) != nil || a.Login == "" {
		return ForgeAccount{}, ErrUnavailable
	}
	return a, nil
}

// Account is the forge's answer for an account name and when it was
// fetched, from the cache while it is fresh; ErrUnavailable from a forge
// that cannot tell accounts.
func (c *Cache) Account(ctx context.Context, name string) (ForgeAccount, time.Time, error) {
	af, ok := c.Forge.(AccountForge)
	if !ok {
		return ForgeAccount{}, c.Now(), ErrUnavailable
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.load()
	key := c.Forge.Name() + ":account:" + strings.ToLower(name)
	now := c.Now()
	if e, ok := c.entries[key]; ok {
		if at, err := time.Parse(time.RFC3339, e.At); err == nil {
			ttl := c.MissTTL
			if e.Found {
				ttl = c.HitTTL
			}
			if now.Sub(at) < ttl {
				if !e.Found {
					return ForgeAccount{}, at, ErrNotFound
				}
				login, typ, _ := strings.Cut(e.FullName, " ")
				return ForgeAccount{Login: login, Type: typ}, at, nil
			}
		}
	}
	a, err := af.Account(ctx, name)
	switch {
	case err == nil:
		c.entries[key] = cacheEntry{Found: true, FullName: a.Login + " " + a.Type, At: stamp(now)}
		c.changed = true
	case errors.Is(err, ErrNotFound):
		c.entries[key] = cacheEntry{At: stamp(now)}
		c.changed = true
	}
	return a, now, err
}

// Account checks that name is an account or organization the owner's
// repositories live under: the owner part of remote URLs in the catalogs
// (or the forge's own account), confirmed by the forge where one answers. A
// name no catalog remote is under and that is not the forge's account is
// not looked up: every common word is somebody's account on a forge, and
// only the owner's accounts say what a folder of theirs is.
func (c *Checker) Account(ctx context.Context, name string) Result {
	name = strings.ToLower(strings.TrimSpace(name))
	r := Result{Kind: KindAccount, Claim: name + " is an account or organization the catalogs' repositories live under", Source: "remotes", Inputs: inputs("name", name), At: stamp(c.now())}
	var under []string
	seen := map[string]bool{}
	consider := func(urls []string) {
		for _, u := range urls {
			host, rest, ok := strings.Cut(u, "/")
			if !ok {
				continue
			}
			o, repo, ok := strings.Cut(rest, "/")
			if ok && strings.ToLower(o) == name && !seen[u] {
				seen[u] = true
				under = append(under, host+"/"+o+"/"+repo)
			}
		}
	}
	if c.Catalog != nil {
		for _, n := range c.Catalog.Names() {
			rr, _ := c.Catalog.Repo(n)
			consider(rr.URLs)
		}
	}
	for _, hs := range c.Shared {
		for _, h := range hs {
			consider(h.URLs)
		}
	}
	sort.Strings(under)
	user := ""
	if c.Forge != nil {
		user = strings.ToLower(c.Forge.Forge.User(ctx))
	}
	if len(under) == 0 && name != user {
		r.Verdict, r.Output = Unknown, "no remote of any checkout lives under "+name+", and it is not the forge's account"
		return r
	}
	ex := under
	if len(ex) > 3 {
		ex = ex[:3]
	}
	out := fmt.Sprintf("%d remote(s) of the checkouts live under %s", len(under), name)
	if len(ex) > 0 {
		out += " (" + strings.Join(ex, ", ") + ")"
	}
	if name == user {
		out += "; it is the forge's own account"
	}
	r.Verdict, r.Output = Holds, out
	if c.Forge == nil {
		return r
	}
	a, when, err := c.Forge.Account(ctx, name)
	switch {
	case err == nil:
		r.Source, r.Value, r.At = "remotes+forge:"+c.Forge.Forge.Name(), a.Type, stamp(when)
		r.Output += fmt.Sprintf("; the forge: users/%s is an account of type %s", name, a.Type)
	case errors.Is(err, ErrNotFound):
		r.Source, r.At = "remotes+forge:"+c.Forge.Forge.Name(), stamp(when)
		r.Verdict, r.Output = Fails, out+"; but the forge has no account "+name
	}
	return r
}

// Claim is a dry claim a statement of the owner's makes, as the curator's
// model read it from their words; code checks it (Checker.Claim). Kinds:
// "canonical" (Name is the real name of a repository, Other, optional,
// another name of it), "exists" and "absent" (Repo), "renamed" (From was renamed to To),
// "path" (Path exists in Repo at HEAD), "version" (Version is a release of
// Repo).
type Claim struct {
	Kind    string `json:"kind"`
	Name    string `json:"name,omitempty"`
	Other   string `json:"other,omitempty"`
	Repo    string `json:"repo,omitempty"`
	From    string `json:"from,omitempty"`
	To      string `json:"to,omitempty"`
	Path    string `json:"path,omitempty"`
	Version string `json:"version,omitempty"`
}

// Valid reports problems with a claim's form: a known kind with the fields
// it needs, each a slug where it names a repository.
func (cl Claim) Valid() []string {
	slug := func(field, v string) []string {
		v = strings.ToLower(strings.TrimSpace(v))
		if v == "" || !nameRx.MatchString(v) || strings.Contains(v, "/") {
			return []string{fmt.Sprintf("claim %s: %s %q is not a repository name", cl.Kind, field, v)}
		}
		return nil
	}
	switch cl.Kind {
	case "canonical":
		if strings.TrimSpace(cl.Other) == "" {
			return slug("name", cl.Name)
		}
		return append(slug("name", cl.Name), slug("other", cl.Other)...)
	case "exists", "absent":
		return slug("repo", cl.Repo)
	case "renamed":
		return append(slug("from", cl.From), slug("to", cl.To)...)
	case "path":
		if strings.TrimSpace(cl.Path) == "" {
			return append(slug("repo", cl.Repo), "claim path: no path")
		}
		return slug("repo", cl.Repo)
	case "version":
		if strings.TrimSpace(cl.Version) == "" {
			return append(slug("repo", cl.Repo), "claim version: no version")
		}
		return slug("repo", cl.Repo)
	}
	return []string{fmt.Sprintf("claim kind %q is not canonical, exists, absent, renamed, path or version", cl.Kind)}
}

// Names are the repository names a claim is about.
func (cl Claim) Names() []string {
	var out []string
	for _, n := range []string{cl.Name, cl.Other, cl.Repo, cl.From, cl.To} {
		if n = strings.ToLower(strings.TrimSpace(n)); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// Claim checks one claim; root is the checkout of the claim's repository on
// this machine ("" for none: a path or a version gives unknown). A claim
// with a bad form gives unknown.
func (c *Checker) Claim(ctx context.Context, cl Claim, root string) Result {
	if ps := cl.Valid(); len(ps) > 0 {
		return Result{Kind: KindRepo, Claim: "a claim " + cl.Kind, Source: "claim", Output: strings.Join(ps, "; "), Verdict: Unknown, At: stamp(c.now())}
	}
	lower := strings.ToLower
	switch cl.Kind {
	case "canonical":
		return c.Canonical(ctx, lower(cl.Name), lower(cl.Other))
	case "exists", "absent":
		rc := c.Repo(ctx, lower(cl.Repo))
		s := rc.Summary()
		if cl.Kind == "absent" {
			s.Claim = "repository " + lower(cl.Repo) + " exists nowhere"
			switch rc.Verdict {
			case Holds:
				s.Verdict = Fails
			case Fails:
				s.Verdict = Holds
			}
		}
		return s
	case "renamed":
		from, to := lower(cl.From), lower(cl.To)
		rc := c.Repo(ctx, from)
		r := Result{Kind: KindRepo, Claim: fmt.Sprintf("repository %s was renamed to %s", from, to), Source: "forge", Inputs: inputs("from", from, "to", to), At: stamp(c.now())}
		switch {
		case rc.Renamed == to:
			r.Verdict, r.Output, r.Value = Holds, fmt.Sprintf("the forge has %s as %s now", from, rc.Forge), rc.Forge
		case rc.Renamed != "":
			r.Verdict, r.Output, r.Value = Fails, fmt.Sprintf("the forge has %s as %s now", from, rc.Forge), rc.Forge
		case rc.Forge != "":
			r.Verdict, r.Output, r.Value = Fails, fmt.Sprintf("the forge has %s under its own name (%s)", from, rc.Forge), rc.Forge
		default:
			r.Verdict, r.Output = Unknown, "the forge says nothing of a rename"
		}
		return r
	case "path":
		if root == "" {
			return Result{Kind: KindPath, Claim: fmt.Sprintf("path %s exists in %s", cl.Path, lower(cl.Repo)), Source: "git:HEAD", Inputs: inputs("repo", lower(cl.Repo), "path", cl.Path), Output: "no checkout of " + lower(cl.Repo) + " on " + c.Host, Verdict: Unknown, At: stamp(c.now())}
		}
		return c.Path(ctx, lower(cl.Repo), root, cl.Path)
	case "version":
		if root == "" {
			return Result{Kind: KindVersion, Claim: fmt.Sprintf("version %s is a release of %s", cl.Version, lower(cl.Repo)), Source: "git:tags", Inputs: inputs("repo", lower(cl.Repo), "version", cl.Version), Output: "no checkout of " + lower(cl.Repo) + " on " + c.Host, Verdict: Unknown, At: stamp(c.now())}
		}
		return c.Version(ctx, lower(cl.Repo), root, cl.Version)
	}
	return Result{Kind: KindRepo, Claim: cl.Kind, Source: "claim", Verdict: Unknown, At: stamp(c.now())}
}

// Canonical checks that name is the real name of a repository and other
// another name of it, against what repoid makes the real name: the main
// checkout's directory on every machine that has one (CheckoutName), a
// symlink's and the origin remote's names being aliases; for a name no
// catalog has, the forge's rename.
func (c *Checker) Canonical(ctx context.Context, name, other string) Result {
	if other == "" {
		r := Result{Kind: KindRepo, Claim: fmt.Sprintf("%s is the real name of its repository", name), Source: "checkouts", Inputs: inputs("name", name), At: stamp(c.now())}
		cn, wn := c.CheckoutName(name)
		where := strings.Join(wn, "; ")
		switch {
		case cn == name:
			r.Verdict, r.Output, r.Value = Holds, where, name
		case cn != "":
			r.Verdict, r.Output, r.Value = Fails, where, cn
		default:
			if where == "" {
				where = "no catalog has " + name
			}
			r.Verdict, r.Output = Unknown, where
		}
		return r
	}
	r := Result{Kind: KindRepo, Claim: fmt.Sprintf("%s is the real name of the repository and %s another name of it", name, other), Source: "checkouts", Inputs: inputs("name", name, "other", other), At: stamp(c.now())}
	cn, wn := c.CheckoutName(name)
	co, wo := c.CheckoutName(other)
	where := strings.Join(append(append([]string(nil), wn...), wo...), "; ")
	switch {
	case cn == name && co == name:
		r.Verdict, r.Output, r.Value = Holds, where, name
	case cn == other && co == other:
		r.Verdict, r.Output, r.Value = Fails, where, other
	case cn != "" && co != "" && cn != co:
		r.Verdict, r.Output = Fails, "they are names of two repositories: "+where
	case cn == name && co == "":
		rc := c.Repo(ctx, other)
		r.Source = "checkouts+forge"
		if rc.Renamed == name {
			r.Verdict, r.Output, r.Value = Holds, where+"; the forge has "+other+" as "+rc.Forge+" now", name
		} else {
			r.Verdict, r.Output = Unknown, where+"; no catalog has "+other
		}
	default:
		if where == "" {
			where = "no catalog has " + name + " or " + other
		}
		r.Verdict, r.Output = Unknown, where
	}
	return r
}

// ForgeURL is the remote URL a repository's forge facts are read from: its
// origin when known, else its one remote on the forge's host, else the one
// under the forge's own account; "" when none is (several remotes, none the
// account's).
func (c *Checker) ForgeURL(ctx context.Context, origin string, urls []string) string {
	if c.Forge == nil {
		return ""
	}
	host := c.Forge.Forge.Host() + "/"
	if strings.HasPrefix(origin, host) {
		return origin
	}
	var on []string
	for _, u := range urls {
		if strings.HasPrefix(u, host) && strings.Count(u, "/") == 2 {
			on = append(on, u)
		}
	}
	sort.Strings(on)
	if len(on) == 1 {
		return on[0]
	}
	if user := strings.ToLower(c.Forge.Forge.User(ctx)); user != "" {
		var mine []string
		for _, u := range on {
			if o, _, _ := strings.Cut(strings.TrimPrefix(u, host), "/"); strings.ToLower(o) == user {
				mine = append(mine, u)
			}
		}
		if len(mine) == 1 {
			return mine[0]
		}
	}
	return ""
}

// ForgeName checks the current forge name of the repository at url (a
// normalized forge URL, host/owner/name): the forge follows renames, so its
// full name is the name the repository lives under now. Value is the
// current normalized URL ("" when the forge did not answer or has none).
func (c *Checker) ForgeName(ctx context.Context, url string) Result {
	r := Result{Kind: KindRepo, Claim: "the current forge name of " + url, Source: "forge", Inputs: inputs("url", url), At: stamp(c.now())}
	if c.Forge == nil {
		r.Verdict, r.Output = Unknown, "no forge"
		return r
	}
	host, rest, _ := strings.Cut(url, "/")
	owner, name, ok := strings.Cut(rest, "/")
	if host != c.Forge.Forge.Host() || !ok || !ValidRepo(owner, name) {
		r.Verdict, r.Output = Unknown, url+" is not a repository on "+c.Forge.Forge.Host()
		return r
	}
	r.Source = "forge:" + c.Forge.Forge.Name()
	fr, when, err := c.Forge.Lookup(ctx, owner, name)
	r.At = stamp(when)
	switch {
	case err == nil:
		r.Verdict, r.Value = Holds, strings.ToLower(host+"/"+fr.FullName)
		r.Output = fmt.Sprintf("%s/%s is %s", owner, name, fr.FullName)
		if !strings.EqualFold(fr.FullName, owner+"/"+name) {
			r.Output += " now (renamed)"
		}
	case errors.Is(err, ErrNotFound):
		r.Verdict, r.Output = Fails, fmt.Sprintf("not found: %s/%s", owner, name)
		if u := strings.ToLower(c.Forge.Forge.User(ctx)); u != "" {
			r.Output += " (asked as " + u + ", who sees their own private repositories)"
		}
	default:
		r.Verdict, r.Output = Unknown, "the forge did not answer"
	}
	return r
}

// OrgOf is the forge organization a repository at url (host/owner/name)
// belongs to: its owner, when the forge says that owner is an organization
// ("" for a personal account, or when the forge did not answer), with the
// account check.
func (c *Checker) OrgOf(ctx context.Context, url string) (string, Result) {
	_, rest, _ := strings.Cut(url, "/")
	owner, _, ok := strings.Cut(rest, "/")
	if !ok {
		return "", Result{Kind: KindAccount, Claim: "the owner of " + url, Source: "forge", Verdict: Unknown, Output: "no owner in " + url, At: stamp(c.now())}
	}
	r := c.Account(ctx, owner)
	if r.Verdict == Holds && strings.EqualFold(r.Value, "Organization") {
		return strings.ToLower(owner), r
	}
	return "", r
}

// IsOrg reports a name the forge says is an organization (the account check
// holds with type Organization) whose repositories the catalogs' remotes
// hold, with that check.
func (c *Checker) IsOrg(ctx context.Context, name string) (bool, Result) {
	r := c.Account(ctx, name)
	return r.Verdict == Holds && strings.EqualFold(r.Value, "Organization"), r
}

// PathForge is a forge that can tell whether a path exists at a
// repository's default branch (GitHub: `gh api repos/<o>/<r>/contents/<p>`).
type PathForge interface {
	Contents(ctx context.Context, owner, name, path string) error
}

// Contents is nil when path exists in owner/name on GitHub at its default
// branch, ErrNotFound when it does not, ErrUnavailable when gh did not say.
func (g *GitHub) Contents(ctx context.Context, owner, name, path string) error {
	if !ValidRepo(owner, name) || !relPathRx.MatchString(path) {
		return ErrNotFound
	}
	out, errb, err := g.run(ctx, g.Bin, "api", "repos/"+owner+"/"+name+"/contents/"+path, "--jq", ".type")
	if err != nil {
		all := string(out) + string(errb)
		if strings.Contains(all, "HTTP 404") || strings.Contains(all, `"status":"404"`) {
			return ErrNotFound
		}
		return ErrUnavailable
	}
	if t := strings.TrimSpace(string(out)); t == "" {
		return ErrUnavailable
	}
	return nil
}

// relPathRx is a repository-relative path a forge may be asked about: no
// leading slash, no "..", no glob, no query.
var relPathRx = regexp.MustCompile(`^[A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+)*$`)

// Contents is the forge's answer for path in owner/name, from the cache
// while it is fresh (a found path HitTTL, a missing one MissTTL).
func (c *Cache) Contents(ctx context.Context, owner, name, path string) (time.Time, error) {
	pf, ok := c.Forge.(PathForge)
	if !ok {
		return c.Now(), ErrUnavailable
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.load()
	key := c.Forge.Name() + ":contents:" + strings.ToLower(owner+"/"+name) + ":" + path
	now := c.Now()
	if e, ok := c.entries[key]; ok {
		if at, err := time.Parse(time.RFC3339, e.At); err == nil {
			ttl := c.MissTTL
			if e.Found {
				ttl = c.HitTTL
			}
			if now.Sub(at) < ttl {
				if !e.Found {
					return at, ErrNotFound
				}
				return at, nil
			}
		}
	}
	err := pf.Contents(ctx, owner, name, path)
	switch {
	case err == nil:
		c.entries[key] = cacheEntry{Found: true, At: stamp(now)}
		c.changed = true
	case errors.Is(err, ErrNotFound):
		c.entries[key] = cacheEntry{At: stamp(now)}
		c.changed = true
	}
	return now, err
}

// ForgePath checks that path exists in the forge repository full
// (owner/name) at its default branch: the source when no checkout here
// holds the repository.
func (c *Checker) ForgePath(ctx context.Context, repo, full, path string) Result {
	path = strings.Trim(strings.TrimPrefix(strings.TrimSpace(path), "./"), "/")
	r := Result{Kind: KindPath, Claim: fmt.Sprintf("path %s exists in %s", path, repo), Source: "forge", Inputs: inputs("repo", repo, "forge", full, "path", path), At: stamp(c.now())}
	owner, name, ok := strings.Cut(full, "/")
	if c.Forge == nil || !ok {
		r.Verdict, r.Output = Unknown, "no forge repository to read"
		return r
	}
	r.Source = "forge:" + c.Forge.Forge.Name()
	when, err := c.Forge.Contents(ctx, owner, name, path)
	r.At = stamp(when)
	switch {
	case err == nil:
		r.Verdict, r.Output = Holds, fmt.Sprintf("%s has %s at its default branch", full, path)
	case errors.Is(err, ErrNotFound):
		r.Verdict, r.Output = Fails, fmt.Sprintf("%s has no %s at its default branch", full, path)
	default:
		r.Verdict, r.Output = Unknown, "the forge did not answer"
	}
	return r
}
