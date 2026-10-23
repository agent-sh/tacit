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
//     one another machine's catalog in the store lists (repoid.CatalogFile,
//     which the caller adds to the catalog), or, until projects are assessed
//     on their own, one whose projects/<repo>
//     or derived/<repo> folder already holds facts in the store. Another name
//     a checkout is known by (a symlink's, its origin remote's) is accepted
//     in applies_to.repos, where delivery matches it, and refused as a scope
//     with the repository's own name in the message;
//   - a machine (machines/<host>) is this one, one that wrote to the store
//     (a key in keys/, an inbox directory) or one whose
//     folder already holds facts;
//   - a project with a folder record (internal/record) is known by its
//     name, and a repository record by its aliases too; once the store's
//     format marker says every folder has a record (records_complete), a
//     project folder that holds facts is no longer known for that alone;
//   - an area or topic (areas/<a>, areas/<a>/topics/<t>, topics/<t>) is
//     known only by its record, and only in a store with the format marker.
//
// A scope inside a suppressed folder is redirected to the folder that
// replaces it (Redirect): the pointer is a checked decision of the owner's
// or the curator's, so code may follow it.
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
	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/record"
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
	// Records are the folder records of the store and, with the local
	// layer, of its curated directory (a store record wins), folder ->
	// record.
	Records map[string]*record.Record
	// Format: the store holds a format marker this binary writes (new
	// roots and records may be written); RecordsComplete: every fact
	// folder has a record, so the interim rule is off.
	Format, RecordsComplete bool
	// Format2: the marker is format 2 or newer (ledger.Format.Orgs): org
	// folders (orgs/<name>) may be written.
	Format2 bool
}

// NewEnv builds the checks' knowledge from the folders of the store at
// storeDir and, when layer is not "", of the local layer's curated directory
// (a project or machine folder that holds at least one fact file), the
// store's writers (keys/<harness>@<host>.pub, inbox/<harness>@<host>/), this
// host and the machine's catalog. It reads directories, not facts.
func NewEnv(storeDir, layer string, catalog *repoid.Catalog, host string) Env {
	e := Env{Catalog: catalog, StoreRepos: map[string]bool{}, Hosts: map[string]bool{}, Records: map[string]*record.Record{}}
	if h := strings.ToLower(host); fact.IsSlug(h) {
		e.Hosts[h] = true
	}
	if storeDir != "" {
		e.Format, _ = fact.FormatWrites(storeDir)
		if st, err := fact.LoadState(storeDir); err == nil {
			e.RecordsComplete = e.Format && st.Format.RecordsComplete
		}
		if f, ok, err := ledger.ReadFormat(storeDir); err == nil && ok {
			e.Format2 = f.Orgs()
		}
	}
	if layer != "" {
		// This machine's overlay never leaves it: its own orgs are written
		// whatever the store's format.
		e.Format2 = true
	}
	for _, root := range []string{storeDir, layer} {
		recs, _ := record.Walk(root)
		for f, r := range recs {
			if _, have := e.Records[f]; !have {
				e.Records[f] = r
			}
		}
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
			if !f.IsDir() && strings.HasSuffix(f.Name(), ".md") && !fact.IsStoreFile(f.Name()) {
				out = append(out, en.Name())
				break
			}
		}
	}
	return out
}

// KnownRepo reports a repository name the store or this machine knows by
// its own name: a checkout's, a repository record's name or alias, or
// (until records are complete) a project folder that holds facts.
func (e Env) KnownRepo(name string) bool {
	n := strings.ToLower(name)
	if e.Catalog.Has(n) || (!e.RecordsComplete && e.StoreRepos[n]) {
		return true
	}
	if f := record.RepoOwner(e.Records, n); f != "" && e.Records[f].Kind == record.KindRepo {
		return true
	}
	return false
}

// Redirect is the folder that holds a suppressed scope's content now (the
// suppression pointers followed), and true when scope is in a suppressed
// folder; scope itself and false otherwise.
func (e Env) Redirect(scope string) (string, bool) {
	if len(e.Records) == 0 {
		return scope, false
	}
	w, chain, err := record.Resolve(e.Records, scope)
	if err != nil || len(chain) == 0 || e.Records[w] == nil {
		return scope, false
	}
	return w, true
}

// RedirectFact moves f (a new fact or note, not yet written) to the folder
// that replaces its scope's, path and scope both, and reports whether it
// did.
func (e Env) RedirectFact(f *fact.Fact) bool {
	w, ok := e.Redirect(f.Meta.Scope)
	if !ok {
		return false
	}
	f.Meta.Scope = w
	if !f.InInbox() {
		f.Path = w + "/" + f.Meta.Name + ".md"
	}
	return true
}

// RepoNames lists the repositories a project scope may name, sorted.
func (e Env) RepoNames() []string {
	set := map[string]bool{}
	for _, n := range e.Catalog.Names() {
		set[n] = true
	}
	if !e.RecordsComplete {
		for n := range e.StoreRepos {
			set[n] = true
		}
	}
	for f, r := range e.Records {
		if r.Kind == record.KindRepo && r.Live() {
			set[strings.TrimPrefix(f, "projects/")] = true
		}
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
		return &Problem{Field: "scope", Rule: "not user, system, projects/<repo>, machines/<host>, areas/<area>, areas/<area>/topics/<topic>, topics/<topic> or orgs/<org> (lower-case letters, digits and dashes)", Hint: fmt.Sprintf("got %q", scope)}
	}
	switch root {
	case "orgs":
		if !e.Format2 {
			return &Problem{Field: "scope", Rule: "org folders are written only once the store's format marker is 2 or newer", Hint: fmt.Sprintf("%s; use the project or user scope", scope)}
		}
		if r := e.Records[scope]; r == nil || r.Kind != record.KindOrg || !r.Live() {
			return &Problem{Field: "scope", Rule: "names no org with a record", Hint: fmt.Sprintf("%s; code makes an org's folder from the forge's answer; use a listed one, or the project or user scope", scope)}
		}
		return nil
	case "areas", "topics":
		if !e.Format {
			return &Problem{Field: "scope", Rule: "areas and topics are written only once the store holds the format marker", Hint: fmt.Sprintf("%s; use the project, user or machines/<host> scope", scope)}
		}
		r := e.Records[scope]
		want := record.KindArea
		if root == "topics" || strings.Contains(rest, "/") {
			want = record.KindTopic
		}
		if r == nil || r.Kind != want {
			return &Problem{Field: "scope", Rule: "names no " + want + " with a record", Hint: fmt.Sprintf("%s; an %s is created by the curator from two sessions' evidence or by the owner; use a listed one, or the project, user or machines/<host> scope", scope, want)}
		}
		return nil
	case "projects":
		if r := e.Records[scope]; r != nil {
			return nil // a recorded folder; one suppressed is redirected (Redirect)
		}
		if e.KnownRepo(rest) {
			return nil
		}
		if own := e.Catalog.AliasOf(rest); own != "" {
			return &Problem{Field: "scope", Rule: "names another name of a repository, not the repository", Hint: fmt.Sprintf("%s is another name of the checkout %s: use projects/%s", rest, own, own)}
		}
		return &Problem{Field: "scope", Rule: "names no known repository: no checkout on this machine or in any machine's catalog, and no project folder in the store", Hint: fmt.Sprintf("projects/%s; use the project of the note it came from, user for a fact that holds across repositories, or machines/<host> for one about a machine", rest)}
	case "machines":
		if e.Hosts[rest] {
			return nil
		}
		return &Problem{Field: "scope", Rule: "names no known machine", Hint: fmt.Sprintf("machines/%s; known machines: %s", rest, strings.Join(sorted(e.Hosts), ", "))}
	}
	return nil
}

// UnknownRepo reports a well-formed projects/<repo> scope that names
// neither a repository the checks know nor an alias of one: the one refusal
// a later catalog can lift (a note from a machine whose catalog has not
// reached the store yet), which the curator waits on (SPEC.md, Metadata
// checks) instead of refusing.
func (e Env) UnknownRepo(scope string) bool {
	root, rest, _ := strings.Cut(strings.ToLower(scope), "/")
	return root == "projects" && fact.IsSlug(rest) && e.Records[root+"/"+rest] == nil && !e.KnownRepo(rest) && e.Catalog.AliasOf(rest) == ""
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
	case d.Owner:
		return &Problem{Field: "type", Rule: "a statement is written by code from the owner's own words it verified, never by a model or an agent", Hint: "give the entry kind statement with owner_quote (the curator's split), or choose one of " + strings.Join(fact.ModelTypes(), ", ")}
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

// absolute reports a path that names a place on a machine, not in a
// repository: a leading /, ~, drive letter or UNC prefix.
func absolute(p string) bool {
	return strings.HasPrefix(p, "/") || strings.HasPrefix(p, "~") || driveRx.MatchString(p) || strings.HasPrefix(p, `\\`)
}

// Paths checks applies_to.paths: each a repo-relative path or glob (SPEC.md,
// Frontmatter), no "..", no URL, a glob that parses. machine: the fact is
// about a machine (a machines/<host> scope), whose paths are that machine's
// and may be absolute; any other fact's absolute or home path is refused
// (RepoPaths has already made one inside the repository's checkout here
// relative).
func Paths(paths []string, machine bool) []Problem {
	var out []Problem
	bad := func(p, rule string) {
		out = append(out, Problem{Field: "applies_to.paths", Rule: rule, Hint: fmt.Sprintf("got %q; applies_to.paths are paths inside the repository, like src/main.go or docs/*.md (an absolute path inside this machine's checkout of the fact's repository is made relative by code); a machine's absolute paths belong in a machines/<host> fact or in the fact's text", p)})
	}
	for _, p := range paths {
		abs := absolute(p)
		switch {
		case strings.TrimSpace(p) == "":
			bad(p, "empty")
		case p != strings.TrimSpace(p) || strings.IndexFunc(p, unicode.IsControl) >= 0:
			bad(p, "has surrounding spaces or control characters")
		case abs && !machine:
			bad(p, "is absolute, not relative to the repository")
		case schemeRx.MatchString(p):
			bad(p, "is a URL, not a path")
		case strings.Contains(p, `\`) && !(machine && abs):
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

// RepoPaths makes each absolute or ~ path in m's applies_to.paths that lies
// inside this machine's checkout of the fact's repository relative to that
// checkout, which is what applies_to.paths hold. The repository is the
// fact's projects/<repo> scope, else its one applies_to.repos name; the
// checkout is repoid's for the path (the nearest git root, symlinks resolved
// on both sides, a worktree or a separate clone of the repository counting
// as its checkout). A path anywhere else, a fact with no one repository and
// a machine's fact (whose absolute paths stand) are left as they are, for
// Paths to judge. m's slice is replaced, never written in place.
func (e Env) RepoPaths(m *fact.Meta) {
	if e.Catalog == nil || len(m.AppliesTo.Paths) == 0 || strings.HasPrefix(m.Scope, "machines/") {
		return
	}
	repo := ""
	if r, ok := strings.CutPrefix(m.Scope, "projects/"); ok {
		repo = strings.ToLower(r)
	} else if len(m.AppliesTo.Repos) == 1 {
		repo = strings.ToLower(strings.TrimSpace(m.AppliesTo.Repos[0]))
	}
	if repo == "" {
		return
	}
	ids := repoid.NewCache(e.Catalog.Home())
	ids.Mains = e.Catalog.Mains()
	out := append([]string(nil), m.AppliesTo.Paths...)
	for i, p := range out {
		if !absolute(p) || p != strings.TrimSpace(p) {
			continue
		}
		x := ids.Expand(p)
		if x == "" {
			continue
		}
		r, ok := ids.Of(x)
		if !ok || !names(r, repo) {
			continue
		}
		rel, err := filepath.Rel(r.Worktree, repoid.RealPath(x))
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		out[i] = filepath.ToSlash(rel)
	}
	m.AppliesTo.Paths = out
}

// names reports whether repo is r's name or one of its aliases.
func names(r repoid.Repo, repo string) bool {
	if r.Slug() == repo {
		return true
	}
	for _, a := range r.Aliases {
		if a == repo {
			return true
		}
	}
	return false
}

func hasDotDot(p string) bool {
	for _, s := range strings.Split(p, "/") {
		if s == ".." {
			return true
		}
	}
	return false
}

// attributionRx is wording that says the owner ruled, said, decided or
// wants something: on a fact that is not their verified words it claims an
// authority the fact has no evidence for. It is the one list: writes are
// refused on it (Attribution) and delivery holds back a stored fact it
// matches (ClaimsOwner).
var attributionRx = regexp.MustCompile(`(?i)\bowner(?:'s|’s)?\s+(?:ruling|rulings|rule|rules|ruled|said|says|decided|wants|wanted)\b`)

// negationRx ends text that negates what follows it: "not an", "no",
// "never the", "isn't an".
var negationRx = regexp.MustCompile(`(?i)\b(?:not|no|never|isn't|isn’t|nor|without)(?:\s+(?:an?|the|any|one|their|its))?\s+$`)

// ClaimsOwner reports text that claims the owner's authority: it says the
// owner ruled, said, decided or wants something (attributionRx), other than
// right after a negation ("this is not an owner ruling"). On a fact that is
// not the owner's verified words it is a claim without evidence.
func ClaimsOwner(desc, body string) bool {
	for _, t := range []string{desc, body} {
		for _, m := range attributionRx.FindAllStringIndex(t, -1) {
			if !negationRx.MatchString(t[:m[0]]) {
				return true
			}
		}
	}
	return false
}

// OrgMembers are the live project folders whose record names org as their
// forge organization (record.Org, filled by code from the forge), by name,
// sorted.
func (e Env) OrgMembers(org string) []string {
	set := map[string]bool{}
	for f, r := range e.Records {
		if r.Live() && record.Broad(r.Kind) == "project" && r.Org == org && strings.HasPrefix(f, "projects/") {
			set[r.Name] = true
		}
	}
	return sorted(set)
}

// MinOrgRepos is how many of an org's repositories a fact must name for a
// model to file it in the org's folder (or the org itself).
const MinOrgRepos = 2

// OrgFact checks a fact a model or an agent files in an org's folder
// (orgs/<org>): code verifies it is about the org, by naming at least
// MinOrgRepos of the org's repositories (in applies_to.repos, or as whole
// words in its text), or the org itself (its name as a whole word, its
// forge URL). nil for any other scope. Membership comes from the records'
// org (the forge), never from what a fact mentions.
func (e Env) OrgFact(m fact.Meta, body string) *Problem {
	org, ok := strings.CutPrefix(m.Scope, "orgs/")
	if !ok || e.Records[m.Scope] == nil {
		return nil
	}
	text := strings.ToLower(m.Description + "\n" + body)
	if record.WholeWord(text, org) {
		return nil
	}
	members := e.OrgMembers(org)
	named := map[string]bool{}
	for _, n := range members {
		if record.WholeWord(text, n) {
			named[n] = true
		}
		for _, r := range m.AppliesTo.Repos {
			if strings.EqualFold(r, n) {
				named[n] = true
			}
		}
	}
	if len(named) >= MinOrgRepos {
		return nil
	}
	return &Problem{Field: "scope", Rule: fmt.Sprintf("a fact goes up to an org only when it names %d or more of the org's repositories, or the org itself", MinOrgRepos), Hint: fmt.Sprintf("%s: it names %d of %s; file it in the one repository it is about, or in user", m.Scope, len(named), strings.Join(members, ", "))}
}

// Attribution refuses wording that claims the owner's authority ("owner
// ruling", "the owner said", "the owner decided", "the owner wants") on a
// fact that is not the owner's verified words (ruling: their ruling with a
// verified quote, or a fact whose words of theirs the store backs): a
// reading agent would take the agent's statement for the owner's word.
// Delivery holds back a stored fact that fails it (search.Hit.Claim).
func Attribution(desc, body string, ruling bool) *Problem {
	if ruling || !ClaimsOwner(desc, body) {
		return nil
	}
	return &Problem{Field: "description", Rule: "says the owner ruled, said, decided or wants it, but the fact is not the owner's verified words (no verified quote of theirs)", Hint: "state the fact as an agent's observation, without 'owner ruling', 'owner rule', 'the owner said', 'decided' or 'wants'; keep kind user only with their exact words"}
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
	if !inbox {
		add(e.OrgFact(m, body))
	}
	add(Type(m.Type, m.Scope, inbox))
	add(Kind(m.Source.Kind))
	out = append(out, e.Repos(m.AppliesTo.Repos)...)
	out = append(out, Paths(m.AppliesTo.Paths, strings.HasPrefix(m.Scope, "machines/"))...)
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
