package check

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// Ref is one dry reference code finds in a fact's text: a repository on a
// forge (a github.com/<owner>/<name> URL), a path written in backticks, a
// version string (v1.2.3).
type Ref struct {
	Kind  string
	Owner string // a repository's owner on the forge
	Value string // the repository's name, the path, the version
}

var (
	forgeURLRx = regexp.MustCompile(`(?i)\bgithub\.com[/:]([A-Za-z0-9](?:[A-Za-z0-9-]{0,38}))/([A-Za-z0-9._-]{1,100})`)
	tickRx     = regexp.MustCompile("`([^`\\s]{3,200})`")
	versionRx  = regexp.MustCompile(`(?:^|[^A-Za-z0-9.\-/])v(\d+\.\d+\.\d+(?:-[0-9A-Za-z.]+)?)\b`)
	pathRx     = regexp.MustCompile(`^[A-Za-z0-9_.@-][A-Za-z0-9_.@/-]*/[A-Za-z0-9_.@-]+/?$`)
)

// MaxRefs bounds the references one fact's check reads.
const MaxRefs = 8

// Refs are the dry references in text, once each, in order, at most MaxRefs:
// forge repositories first, then paths, then versions. Only forms code
// reads with no judgement: a path is a backticked token with a slash that
// is no URL, no absolute or home path, no glob, no option and no variable.
func Refs(text string) []Ref {
	var out []Ref
	seen := map[string]bool{}
	add := func(r Ref) {
		k := r.Kind + "\x00" + strings.ToLower(r.Owner) + "\x00" + r.Value
		if !seen[k] && len(out) < MaxRefs {
			seen[k] = true
			out = append(out, r)
		}
	}
	for _, m := range forgeURLRx.FindAllStringSubmatch(text, -1) {
		name := strings.TrimSuffix(strings.TrimRight(m[2], "."), ".git")
		if name == "" || name == "." || name == ".." {
			continue
		}
		add(Ref{Kind: KindRepo, Owner: strings.ToLower(m[1]), Value: strings.ToLower(name)})
	}
	for _, m := range tickRx.FindAllStringSubmatch(text, -1) {
		p := strings.TrimRight(m[1], ".,;:")
		if !pathRx.MatchString(p) || strings.Contains(p, "://") || strings.HasPrefix(p, "-") || strings.Contains(p, "..") {
			continue
		}
		if strings.HasPrefix(strings.ToLower(p), "github.com/") {
			continue
		}
		add(Ref{Kind: KindPath, Value: strings.TrimSuffix(strings.TrimPrefix(p, "./"), "/")})
	}
	for _, m := range versionRx.FindAllStringSubmatch(text, -1) {
		add(Ref{Kind: KindVersion, Value: "v" + m[1]})
	}
	return out
}

// Facts checks a fact's references: forge repositories always; paths and
// versions when the fact is about a repository with a checkout here (repo
// and root, "" when none). Results come in the references' order.
func (c *Checker) Facts(ctx context.Context, text, repo, root string) []Result {
	var out []Result
	for _, r := range Refs(text) {
		switch r.Kind {
		case KindRepo:
			out = append(out, c.ForgeRepo(ctx, r.Owner, r.Value))
		case KindPath:
			if root != "" {
				out = append(out, c.Path(ctx, repo, root, r.Value))
			}
		case KindVersion:
			if root != "" {
				out = append(out, c.Version(ctx, repo, root, r.Value))
			}
		}
	}
	return out
}

// ForgeRepo checks the repository owner/name a fact names by URL: the forge
// first (it follows renames), then the catalogs' remotes; Unknown when
// neither can say.
func (c *Checker) ForgeRepo(ctx context.Context, owner, name string) Result {
	full := owner + "/" + name
	r := Result{Kind: KindRepo, Claim: "repository " + full + " exists under that name", Inputs: inputs("owner", owner, "name", name), At: stamp(c.now())}
	if c.Forge != nil && ValidRepo(owner, name) {
		fr, when, err := c.Forge.Lookup(ctx, owner, name)
		r.Source, r.At = "forge:"+c.Forge.Forge.Name(), stamp(when)
		switch {
		case err == nil && strings.EqualFold(fr.FullName, full):
			r.Verdict, r.Output, r.Value = Holds, full+" is "+fr.FullName, fr.FullName
			return r
		case err == nil:
			_, got, _ := strings.Cut(fr.FullName, "/")
			if strings.EqualFold(got, name) {
				// Moved to another owner, same name: the name holds.
				r.Verdict, r.Output, r.Value = Holds, full+" moved to "+fr.FullName, fr.FullName
			} else {
				r.Verdict, r.Output, r.Value = Fails, full+" was renamed: it is "+fr.FullName+" now", fr.FullName
			}
			return r
		case errors.Is(err, ErrNotFound):
			r.Verdict, r.Output = Fails, "not found: "+full
			return r
		}
	}
	// No forge answer: a checkout whose remote is this URL says it exists.
	want := strings.ToLower("github.com/" + full)
	for _, n := range c.Catalog.Names() {
		rp, _ := c.Catalog.Repo(n)
		for _, u := range rp.URLs {
			if u == want {
				r.Source, r.Verdict, r.Output = "remotes", Holds, "the remote of the checkout "+n
				return r
			}
		}
	}
	r.Source, r.Verdict, r.Output = "remotes", Unknown, "no forge answer and no checkout has this remote"
	return r
}

type manifest struct {
	file    string
	version func(content string) string
}

var manifests = []manifest{
	{"package.json", func(s string) string {
		var v struct {
			Version string `json:"version"`
		}
		_ = json.Unmarshal([]byte(s), &v)
		return strings.TrimSpace(v.Version)
	}},
	{"Cargo.toml", func(s string) string {
		var v struct {
			Package struct {
				Version any `toml:"version"`
			} `toml:"package"`
			Workspace struct {
				Package struct {
					Version string `toml:"version"`
				} `toml:"package"`
			} `toml:"workspace"`
		}
		_ = toml.Unmarshal([]byte(s), &v)
		if x, ok := v.Package.Version.(string); ok && x != "" {
			return x
		}
		return v.Workspace.Package.Version
	}},
	{"pyproject.toml", func(s string) string {
		var v struct {
			Project struct {
				Version string `toml:"version"`
			} `toml:"project"`
			Tool struct {
				Poetry struct {
					Version string `toml:"version"`
				} `toml:"poetry"`
			} `toml:"tool"`
		}
		_ = toml.Unmarshal([]byte(s), &v)
		if v.Project.Version != "" {
			return v.Project.Version
		}
		return v.Tool.Poetry.Version
	}},
	{"VERSION", func(s string) string {
		s = strings.TrimSpace(s)
		if semver(s) != nil {
			return s
		}
		return ""
	}},
}
