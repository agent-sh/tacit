// Package validate checks the metadata a model or an agent writes on a fact
// or a note before it is written (SPEC.md, "Metadata checks"): the curator's
// split entries, tacit_add and `tacit add`, and the local layer's curator.
// A value is accepted only when it is allowed, well formed, and what it names
// exists; otherwise the write is refused with a Problem the writer can fix.
// Code never puts a guess of its own in place of a refused value. What code
// cannot check reliably (whether a fact is task-only, whether a command is
// right) is not checked here.
//
// What "exists" means:
//
//   - a repository (a projects/<repo> scope, a name in applies_to.repos) is
//     one this machine has a checkout of (repoid's catalog: a real git root),
//     or, until projects are assessed on their own, one whose projects/<repo>
//     or derived/<repo> folder already holds facts in the store. Another name
//     a checkout is known by (a symlink's, its origin remote's) is accepted
//     in applies_to.repos, where delivery matches it, and refused as a scope
//     with the repository's own name in the message;
//   - a machine (machines/<host>) is this one, one that wrote to the store
//     (a key in keys/, an inbox directory) or one whose
//     folder already holds facts.
package validate

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/repoid"
)

// Problem is one refused value: the field, the rule it breaks (no value of
// the writer's in it, so it can be logged), and Hint, the value and what to
// do instead, for the writer to fix (a model's retry, an agent's error).
type Problem struct {
	Field string
	Rule  string
	Hint  string
}

func (p Problem) String() string { return p.Field + ": " + p.Rule }

// Message is the problem for the writer: the rule and the hint.
func (p Problem) Message() string {
	if p.Hint == "" {
		return p.String()
	}
	return p.String() + " (" + p.Hint + ")"
}

// Env is what the checks know: this machine's repositories and the store's
// folders and writers.
type Env struct {
	Catalog *repoid.Catalog
	// StoreRepos are the repo names of projects/<repo> and derived/<repo>
	// folders that hold facts (curated, in the store or the local layer).
	StoreRepos map[string]bool
	// Hosts are the machines a machines/<host> scope may name.
	Hosts map[string]bool
}

// NewEnv builds the checks' knowledge from the folders of the store at
// storeDir and, when layer is not "", of the local layer's curated directory
// (a project or machine folder that holds at least one fact file), the
// store's writers (keys/<harness>@<host>.pub, inbox/<harness>@<host>/), this
// host and the machine's catalog. It reads directories, not facts.
func NewEnv(storeDir, layer string, catalog *repoid.Catalog, host string) Env {
	e := Env{Catalog: catalog, StoreRepos: map[string]bool{}, Hosts: map[string]bool{}}
	if h := strings.ToLower(host); fact.IsSlug(h) {
		e.Hosts[h] = true
	}
	for _, root := range []string{storeDir, layer} {
		if root == "" {
			continue
		}
		for _, kind := range []string{"projects", "derived", "machines"} {
			for _, name := range factFolders(filepath.Join(root, kind)) {
				if kind == "machines" {
					e.Hosts[name] = true
				} else {
					e.StoreRepos[name] = true
				}
			}
		}
	}
	if storeDir != "" {
		for _, sub := range []string{"keys", "inbox"} {
			entries, err := os.ReadDir(filepath.Join(storeDir, sub))
			if err != nil {
				continue
			}
			for _, en := range entries {
				id := strings.TrimSuffix(en.Name(), ".pub")
				if _, h, ok := strings.Cut(id, "@"); ok && fact.IsSlug(h) {
					e.Hosts[h] = true
				}
			}
		}
	}
	return e
}

// factFolders are the slug-named folders in dir that hold a fact file.
func factFolders(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, en := range entries {
		if !en.IsDir() || !fact.IsSlug(en.Name()) {
			continue
		}
		files, err := os.ReadDir(filepath.Join(dir, en.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			if !f.IsDir() && strings.HasSuffix(f.Name(), ".md") && !fact.IsIndexFile(f.Name()) {
				out = append(out, en.Name())
				break
			}
		}
	}
	return out
}

// KnownRepo reports a repository name the store or this machine knows by
// its own name.
func (e Env) KnownRepo(name string) bool {
	n := strings.ToLower(name)
	return e.Catalog.Has(n) || e.StoreRepos[n]
}

// RepoNames lists the repositories a project scope may name, sorted.
func (e Env) RepoNames() []string {
	set := map[string]bool{}
	for _, n := range e.Catalog.Names() {
		set[n] = true
	}
	for n := range e.StoreRepos {
		set[n] = true
	}
	return sorted(set)
}

func sorted(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Scope checks a scope a model or an agent gave. inbox: the scope of an
// inbox note (the writer's belief, still checked: a note filed under a
// project that does not exist is never delivered, and the curator copies
// most scopes as they are).
func (e Env) Scope(scope string) *Problem {
	if scope == "" {
		return &Problem{Field: "scope", Rule: "empty", Hint: "use user, system, projects/<repo> or machines/<host>"}
	}
	root, rest, _ := strings.Cut(scope, "/")
	if root == "derived" {
		return &Problem{Field: "scope", Rule: "derived/ is for facts code generates from git", Hint: "use projects/" + rest + " for a fact about that repository"}
	}
	if !fact.ValidScope(scope) || ((root == "projects" || root == "machines") && !fact.IsSlug(rest)) {
		return &Problem{Field: "scope", Rule: "not user, system, projects/<repo> or machines/<host> (lower-case letters, digits and dashes)", Hint: fmt.Sprintf("got %q", scope)}
	}
	switch root {
	case "projects":
		if e.KnownRepo(rest) {
			return nil
		}
		if own := e.Catalog.AliasOf(rest); own != "" {
			return &Problem{Field: "scope", Rule: "names another name of a repository, not the repository", Hint: fmt.Sprintf("%s is another name of the checkout %s: use projects/%s", rest, own, own)}
		}
		return &Problem{Field: "scope", Rule: "names no known repository: no checkout on this machine and no project folder in the store", Hint: fmt.Sprintf("projects/%s; use the project of the note it came from, user for a fact that holds across repositories, or machines/<host> for one about a machine", rest)}
	case "machines":
		if e.Hosts[rest] {
			return nil
		}
		return &Problem{Field: "scope", Rule: "names no known machine", Hint: fmt.Sprintf("machines/%s; known machines: %s", rest, strings.Join(sorted(e.Hosts), ", "))}
	}
	return nil
}

// Type checks a type a model or an agent gave, for a fact in scope (inbox:
// an inbox note's).
func Type(typ, scope string, inbox bool) *Problem {
	d, ok := fact.TypeDocOf(typ)
	switch {
	case typ == "":
		return &Problem{Field: "type", Rule: "empty", Hint: "one of " + strings.Join(fact.ModelTypes(), ", ")}
	case !ok:
		return &Problem{Field: "type", Rule: "not a known type", Hint: fmt.Sprintf("got %q; one of %s", typ, strings.Join(fact.ModelTypes(), ", "))}
	case d.Generated:
		return &Problem{Field: "type", Rule: "is generated by code from git, never written by a model or an agent", Hint: fmt.Sprintf("%s: %s; choose one of %s", typ, d.Doc, strings.Join(fact.ModelTypes(), ", "))}
	case d.InboxOnly && !inbox:
		return &Problem{Field: "type", Rule: "unclassified is for inbox notes only", Hint: "one of " + strings.Join(fact.ModelTypes(), ", ")}
	case d.UserScope && scope != "user":
		return &Problem{Field: "type", Rule: "type user (who the owner is) belongs in the user scope", Hint: "use scope user, or another type for a fact about " + scope}
	}
	return nil
}

// Kind checks a source kind a model or an agent gave.
func Kind(kind string) *Problem {
	switch kind {
	case "user", "observed", "web":
		return nil
	case "derived":
		return &Problem{Field: "kind", Rule: "derived is for facts code generates", Hint: "use observed"}
	}
	return &Problem{Field: "kind", Rule: "not user, observed or web", Hint: fmt.Sprintf("got %q", kind)}
}

// Repos checks applies_to.repos: each a repository name the store or this
// machine knows (its own name, or another name of a checkout here, which
// delivery matches too).
func (e Env) Repos(repos []string) []Problem {
	var out []Problem
	for _, r := range repos {
		n := strings.ToLower(strings.TrimSpace(r))
		switch {
		case !fact.IsSlug(n):
			out = append(out, Problem{Field: "applies_to.repos", Rule: "not a repository name (lower-case letters, digits and dashes)", Hint: fmt.Sprintf("got %q; give the repository's name, not a path, URL or owner/repo", r)})
		case e.KnownRepo(n) || e.Catalog.AliasOf(n) != "":
		default:
			out = append(out, Problem{Field: "applies_to.repos", Rule: "names no known repository: delivery would never match it", Hint: fmt.Sprintf("%q; name a repository that exists, or leave it out", r)})
		}
	}
	return out
}

var (
	driveRx  = regexp.MustCompile(`^[A-Za-z]:`)
	schemeRx = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*://`)
)

// Paths checks applies_to.paths: each a repo-relative path or glob (SPEC.md,
// Frontmatter), not an absolute or home path (a machine's paths go in the
// fact's text), no "..", no URL, a glob that parses.
func Paths(paths []string) []Problem {
	var out []Problem
	bad := func(p, rule string) {
		out = append(out, Problem{Field: "applies_to.paths", Rule: rule, Hint: fmt.Sprintf("got %q; applies_to.paths are paths inside the repository, like src/main.go or docs/*.md; a machine's absolute paths belong in the fact's text", p)})
	}
	for _, p := range paths {
		switch {
		case strings.TrimSpace(p) == "":
			bad(p, "empty")
		case p != strings.TrimSpace(p) || strings.IndexFunc(p, unicode.IsControl) >= 0:
			bad(p, "has surrounding spaces or control characters")
		case strings.HasPrefix(p, "/") || strings.HasPrefix(p, "~") || driveRx.MatchString(p) || strings.HasPrefix(p, `\\`):
			bad(p, "is absolute, not relative to the repository")
		case schemeRx.MatchString(p):
			bad(p, "is a URL, not a path")
		case strings.Contains(p, `\`):
			bad(p, "uses backslashes; paths use /")
		case hasDotDot(p):
			bad(p, "climbs out with ..")
		default:
			if _, err := path.Match(p, ""); err != nil {
				bad(p, "is not a well-formed glob")
			}
		}
	}
	return out
}

func hasDotDot(p string) bool {
	for _, s := range strings.Split(p, "/") {
		if s == ".." {
			return true
		}
	}
	return false
}

// attributionRx is wording that says the owner ruled: on a fact that is not
// their ruling it claims an authority the fact has no evidence for.
var attributionRx = regexp.MustCompile(`(?i)\bowner(?:'s|’s)?\s+(?:ruling|rule|ruled)\b`)

// Attribution refuses "owner ruling" wording on a fact that is not an owner
// ruling (no verified quote of theirs): a reading agent would take the
// agent's statement for the owner's word.
func Attribution(desc, body string, ruling bool) *Problem {
	if ruling {
		return nil
	}
	if attributionRx.MatchString(desc) || attributionRx.MatchString(body) {
		return &Problem{Field: "description", Rule: "says the owner ruled, but the fact is not an owner ruling (no verified quote of theirs)", Hint: "state the fact as an agent's observation, without 'owner ruling' or 'owner rule'; keep kind user only with their exact words"}
	}
	return nil
}

// Meta checks everything a model or an agent wrote on m: scope, type, kind,
// applies_to, and the attribution of its text (ruling: the fact is an owner
// ruling with their verified quote, or, for an inbox note, it claims kind
// user, which the curator decides). inbox: an inbox note.
func (e Env) Meta(m fact.Meta, body string, inbox, ruling bool) []Problem {
	var out []Problem
	add := func(p *Problem) {
		if p != nil {
			out = append(out, *p)
		}
	}
	add(e.Scope(m.Scope))
	add(Type(m.Type, m.Scope, inbox))
	add(Kind(m.Source.Kind))
	out = append(out, e.Repos(m.AppliesTo.Repos)...)
	out = append(out, Paths(m.AppliesTo.Paths)...)
	add(Attribution(m.Description, body, ruling || (inbox && m.Source.Kind == "user")))
	return out
}

// Error joins problems into one message for a writer.
func Error(ps []Problem) error {
	if len(ps) == 0 {
		return nil
	}
	msgs := make([]string, len(ps))
	for i, p := range ps {
		msgs[i] = p.Message()
	}
	return fmt.Errorf("refused: %s", strings.Join(msgs, "; "))
}
