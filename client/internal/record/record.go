// Package record reads, writes and checks a folder's record (SPEC.md,
// "Folder records"): the file _record.md in a project, area or topic
// folder of the store that says what the folder is. Its frontmatter is
// strict, like a fact's, and it is not a fact (fact.IsStoreFile):
//
//	kind          repo | folder | area | topic, matching the folder
//	name          the folder's last part
//	description   one line, at most 200 characters
//	status        active | dormant | dead | shipped | needs-review
//	was           every status the folder had, newest first, the current
//	              one first; never shortened (append only)
//	evidence      what the deciding call saw, written by code, never by a model
//	created, created_by
//	aliases       other names the folder is known by; unique store-wide
//	suppressed_by the folder that replaces this one; empty while it is live
//	tools, commands  areas only: tool-name patterns and command prefixes
//	repos         topics only: repositories the topic is tied to
//	roots         kind folder only: where the folder lives, per machine
//	org           a project's forge organization (its origin's owner when
//	              the forge says that owner is an organization), by code
//	forge         a project's current forge name (normalized URL,
//	              github.com/<owner>/<name>), by code from the forge
//	successor     a dead project's successor: the live project folder the
//	              owner's words say replaces it (projects/<name>); its
//	              facts stay as history (not a suppression)
//
// A folder orgs/<name> has a record of kind org: an organization on the
// forge (GitHub: an account of type Organization) whose facts hold across
// its repositories. Code makes it from the forge's answer, never a model.
//
// An agent decides the values; code checks them (Validate) and fills in
// only what it derives reliably (the evidence block, dates).
package record

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/agent-sh/tacit/client/internal/check"
	"github.com/agent-sh/tacit/client/internal/fact"
)

// FileName is the record file in a folder.
const FileName = fact.RecordFile

// Kinds and statuses.
const (
	KindRepo   = "repo"   // a git repository (a projects/<name> folder)
	KindFolder = "folder" // a project folder that is no git repository
	KindArea   = "area"   // an ongoing system or practice (areas/<name>)
	KindTopic  = "topic"  // a bounded work stream (topics/<name>, areas/<a>/topics/<name>)
	KindOrg    = "org"    // a forge organization (orgs/<name>): facts across its repositories

	StatusActive      = "active"
	StatusDormant     = "dormant"
	StatusDead        = "dead"
	StatusShipped     = "shipped"
	StatusNeedsReview = "needs-review"

	ByOpus  = "opus"  // the curator's model, or an agent's command
	ByOwner = "owner" // the owner, through their channel (the app), or their own words
)

var (
	kinds    = map[string]bool{KindRepo: true, KindFolder: true, KindArea: true, KindTopic: true, KindOrg: true}
	statuses = map[string]bool{StatusActive: true, StatusDormant: true, StatusDead: true, StatusShipped: true, StatusNeedsReview: true}
)

// Statuses lists the allowed statuses, for messages.
func Statuses() []string {
	return []string{StatusActive, StatusDormant, StatusDead, StatusShipped, StatusNeedsReview}
}

// Entry is one status of the folder's history (was).
type Entry struct {
	Status string `yaml:"status"`
	Date   string `yaml:"date"`
	By     string `yaml:"by"`
	Why    string `yaml:"why"`
	// Quote and QuoteFrom: the owner's own words this status rests on, and
	// where they said them, when the entry is theirs through their words
	// (by owner, written by the curator): the quote ledger must back them.
	Quote     string `yaml:"quote,omitempty"`
	QuoteFrom string `yaml:"quote_from,omitempty"`
	// Words, WordsFrom and WordsOn: the owner's words about the folder's
	// status that an agent's entry rests on (a note or statement of theirs
	// code showed and checked, verbatim), where they are and the day they
	// were said: the status stands against the dates until the owner's real
	// work on the folder after that day (store format 3).
	Words     string `yaml:"words,omitempty"`
	WordsFrom string `yaml:"words_from,omitempty"`
	WordsOn   string `yaml:"words_on,omitempty"`
}

// RestsOnWords reports an entry whose status rests on the owner's words
// (Words), with the day they were said.
func (e Entry) RestsOnWords() (string, bool) {
	return e.WordsOn, e.Words != "" && e.WordsOn != ""
}

// Evidence is what code gathered for a decision, from one machine (git,
// the dreamer's registry) or from sessions (areas and topics). A model
// never writes it: the step that asks the model overwrites it.
type Evidence struct {
	Host     string `yaml:"host,omitempty"`
	Gathered string `yaml:"gathered,omitempty"`
	GitRoot  bool   `yaml:"git_root,omitempty"`
	// Checkout is main, worktree or clone (repoid).
	Checkout string   `yaml:"checkout,omitempty"`
	Remotes  []string `yaml:"remotes,omitempty"`
	// OwnCommits are the commits under this machine's git identity; nil
	// when git could not say (no history readable).
	OwnCommits   *int   `yaml:"own_commits,omitempty"`
	FirstCommit  string `yaml:"first_commit,omitempty"`
	LastCommit   string `yaml:"last_commit,omitempty"`
	WorkSessions int    `yaml:"work_sessions,omitempty"`
	FirstWork    string `yaml:"first_work,omitempty"`
	LastWork     string `yaml:"last_work,omitempty"`
	// UpstreamClone is code's flag: no own commits and no work sessions.
	UpstreamClone bool `yaml:"upstream_clone,omitempty"`
	// FactSessions: distinct sessions whose notes or facts are in the
	// folder, with the first and last day of those facts.
	FactSessions int    `yaml:"fact_sessions,omitempty"`
	FirstFact    string `yaml:"first_fact,omitempty"`
	LastFact     string `yaml:"last_fact,omitempty"`
	// Paths: for a folder that is no git repository, the folders sessions
	// worked in that bear its name (home-relative).
	Paths []string `yaml:"paths,omitempty"`
	// Sessions: for an area or topic, the sessions whose notes proposed it.
	Sessions []string `yaml:"sessions,omitempty"`
	// Checks: what code verified of the folder's dry claims (its name as a
	// repository, a suppression target's), each with its source, inputs,
	// output, verdict and time (internal/check).
	Checks []check.Result `yaml:"checks,omitempty"`
}

// Root is where a folder of kind folder lives on one machine.
type Root struct {
	Host string `yaml:"host"`
	Path string `yaml:"path"`
}

// Record is one folder's record.
type Record struct {
	Kind         string     `yaml:"kind"`
	Name         string     `yaml:"name"`
	Description  string     `yaml:"description"`
	Status       string     `yaml:"status"`
	Was          []Entry    `yaml:"was"`
	Evidence     []Evidence `yaml:"evidence,omitempty"`
	Created      string     `yaml:"created"`
	CreatedBy    string     `yaml:"created_by"`
	Aliases      []string   `yaml:"aliases,omitempty"`
	SuppressedBy string     `yaml:"suppressed_by,omitempty"`
	Tools        []string   `yaml:"tools,omitempty"`
	Commands     []string   `yaml:"commands,omitempty"`
	Repos        []string   `yaml:"repos,omitempty"`
	Roots        []Root     `yaml:"roots,omitempty"`
	// Org is a project's forge organization, Forge its current forge name
	// (github.com/<owner>/<name>): both written by code from the forge's
	// answers (store format 2), never by a model.
	Org   string `yaml:"org,omitempty"`
	Forge string `yaml:"forge,omitempty"`
	// Successor is the live project folder that replaces a dead project
	// (projects/<name>), from the owner's words, checked by code: the
	// project and its facts stay, as history (store format 3). Unlike
	// suppressed_by (the same thing under another name, its facts moved),
	// a successor is another project.
	Successor string `yaml:"successor,omitempty"`

	// Folder is the store-relative folder the record is in (its scope);
	// Body the free text after the frontmatter. Neither is frontmatter.
	Folder string `yaml:"-"`
	Body   string `yaml:"-"`
	// LocalStatus: the status and history are this machine's local layer's,
	// over the store's record (MergeLocal); never written.
	LocalStatus bool `yaml:"-"`
}

// Rel is the store-relative path of folder's record.
func Rel(folder string) string { return folder + "/" + FileName }

// Parse reads the record of folder from data, strictly: an unknown key is
// an error, like a fact's.
func Parse(folder string, data []byte) (*Record, error) {
	fm, body, ok := fact.Split(data)
	if !ok {
		return nil, errors.New("no frontmatter block")
	}
	var r Record
	if err := fact.Decode(fm, &r); err != nil {
		return nil, fmt.Errorf("frontmatter: %w", err)
	}
	r.Folder = folder
	r.Body = strings.TrimSpace(string(body))
	return &r, nil
}

// Encode renders the record file.
func (r *Record) Encode() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("---\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(r); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	buf.WriteString("---\n\n")
	if b := strings.TrimSpace(r.Body); b != "" {
		buf.WriteString(b + "\n")
	}
	return buf.Bytes(), nil
}

// Clone is a deep copy of r.
func (r *Record) Clone() *Record {
	if r == nil {
		return nil
	}
	c := *r
	c.Was = append([]Entry(nil), r.Was...)
	c.Evidence = make([]Evidence, len(r.Evidence))
	for i, e := range r.Evidence {
		c.Evidence[i] = e
		c.Evidence[i].Remotes = append([]string(nil), e.Remotes...)
		c.Evidence[i].Paths = append([]string(nil), e.Paths...)
		c.Evidence[i].Sessions = append([]string(nil), e.Sessions...)
		c.Evidence[i].Checks = append([]check.Result(nil), e.Checks...)
		if e.OwnCommits != nil {
			n := *e.OwnCommits
			c.Evidence[i].OwnCommits = &n
		}
	}
	if len(r.Evidence) == 0 {
		c.Evidence = nil
	}
	c.Aliases = append([]string(nil), r.Aliases...)
	c.Tools = append([]string(nil), r.Tools...)
	c.Commands = append([]string(nil), r.Commands...)
	c.Repos = append([]string(nil), r.Repos...)
	c.Roots = append([]Root(nil), r.Roots...)
	return &c
}

// Live reports a record that is not suppressed.
func (r *Record) Live() bool { return r != nil && r.SuppressedBy == "" }

// Quiet reports a folder no hook delivers from (the owner's rule: dead and
// shipped folders are quiet, but search and the tree find them).
func (r *Record) Quiet() bool {
	return r != nil && (r.Status == StatusDead || r.Status == StatusShipped)
}

// ReadByDefault reports a live area or topic a session reads without
// naming it: active, or waiting for review.
func (r *Record) ReadByDefault() bool {
	return r.Live() && (r.Status == StatusActive || r.Status == StatusNeedsReview)
}

// Names are the folder's own name and its aliases.
func (r *Record) Names() []string { return append([]string{r.Name}, r.Aliases...) }

// ForgeName is the repository name part of the record's current forge name
// ("" for none).
func (r *Record) ForgeName() string {
	if r == nil || r.Forge == "" {
		return ""
	}
	return strings.ToLower(path.Base(r.Forge))
}

// ForgeLabel is how the forge of a normalized URL is named to a reader:
// "GitHub" for github.com, else the host.
func ForgeLabel(url string) string {
	host, _, _ := strings.Cut(url, "/")
	if host == "github.com" {
		return "GitHub"
	}
	return host
}

// Display is the folder's name as every reader shows it: the intended name
// (the folder's), and the current forge name when it differs, so an agent
// uses the right URL: "pikadon (GitHub: ginza)".
func (r *Record) Display() string {
	if r == nil {
		return ""
	}
	return DisplayName(r.Name, r.Forge)
}

// DisplayName is Display for a name and a forge name (normalized URL).
func DisplayName(name, forge string) string {
	fn := ""
	if forge != "" {
		fn = strings.ToLower(path.Base(forge))
	}
	if fn == "" || fn == strings.ToLower(name) {
		return name
	}
	return name + " (" + ForgeLabel(forge) + ": " + fn + ")"
}

// FolderKinds are the kinds a record of folder may have, and the folder's
// root ("projects", "areas", "topics", "orgs"); none for a folder that holds no
// record (system, user, machines/..., derived/..., a deeper path).
func FolderKinds(folder string) []string {
	segs := strings.Split(folder, "/")
	for _, s := range segs {
		if !fact.IsSlug(s) {
			return nil
		}
	}
	switch {
	case len(segs) == 2 && segs[0] == "projects":
		return []string{KindRepo, KindFolder}
	case len(segs) == 2 && segs[0] == "areas":
		return []string{KindArea}
	case len(segs) == 2 && segs[0] == "topics":
		return []string{KindTopic}
	case len(segs) == 4 && segs[0] == "areas" && segs[2] == "topics":
		return []string{KindTopic}
	case len(segs) == 2 && segs[0] == "orgs":
		return []string{KindOrg}
	}
	return nil
}

// IsFolder reports a folder that may hold a record.
func IsFolder(folder string) bool { return len(FolderKinds(folder)) > 0 }

// Broad is a kind's family for suppression: a project folds into a
// project, an area or topic into an area or topic. An org is its own: it
// never folds into a project (only a project folder named for the org folds
// into the org, OrgFolds).
func Broad(kind string) string {
	switch kind {
	case KindRepo, KindFolder:
		return "project"
	case KindArea, KindTopic:
		return "area-or-topic"
	case KindOrg:
		return "org"
	}
	return ""
}

// OrgFolds reports the one fold across families: a project folder named for
// a forge organization (projects/agent-sh) into that organization's folder
// (orgs/agent-sh), whose names hold the project folder's name.
func OrgFolds(project, org *Record) bool {
	if project == nil || org == nil || Broad(project.Kind) != "project" || org.Kind != KindOrg {
		return false
	}
	for _, n := range org.Names() {
		if n == project.Name {
			return true
		}
	}
	return false
}

// Walk reads every record under root: projects/*, areas/*, areas/*/topics/*,
// topics/* and orgs/*, folder -> record. A record that does not parse is in errs
// with its path; symlinks are not followed.
func Walk(root string) (map[string]*Record, []error) {
	out := map[string]*Record{}
	var errs []error
	if root == "" {
		return out, nil
	}
	var paths []string
	for _, pat := range []string{"projects/*/", "areas/*/", "areas/*/topics/*/", "topics/*/", "orgs/*/"} {
		m, _ := filepath.Glob(filepath.Join(root, filepath.FromSlash(pat), FileName))
		paths = append(paths, m...)
	}
	sort.Strings(paths)
	for _, p := range paths {
		rel, err := filepath.Rel(root, p)
		if err != nil {
			continue
		}
		rel = filepath.ToSlash(rel)
		folder := path.Dir(rel)
		if !IsFolder(folder) {
			continue
		}
		fi, err := os.Lstat(p)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if dfi, err := os.Lstat(filepath.Dir(p)); err != nil || dfi.Mode()&os.ModeSymlink != 0 {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", rel, err))
			continue
		}
		r, err := Parse(folder, b)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", rel, err))
			continue
		}
		out[folder] = r
	}
	return out, errs
}

// ErrCycle is a suppression chain that comes back on itself.
var ErrCycle = errors.New("suppressed_by loops")

// Resolve follows suppressed_by from folder to the live folder that holds
// its content now: winner is folder itself when it has no record or is
// live; chain lists every suppressed folder passed on the way, folder
// first. A pointer to a folder with no record ends the chain there; a loop
// is ErrCycle.
func Resolve(recs map[string]*Record, folder string) (winner string, chain []string, err error) {
	seen := map[string]bool{}
	cur := folder
	for {
		r := recs[cur]
		if r == nil || r.SuppressedBy == "" {
			return cur, chain, nil
		}
		if seen[cur] {
			return "", chain, ErrCycle
		}
		seen[cur] = true
		chain = append(chain, cur)
		cur = r.SuppressedBy
	}
}

// Losers are the folders suppressed into folder, directly or through a
// chain, sorted.
func Losers(recs map[string]*Record, folder string) []string {
	var out []string
	for f := range recs {
		if f == folder {
			continue
		}
		if w, chain, err := Resolve(recs, f); err == nil && len(chain) > 0 && w == folder {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}

// RepoOwner is the folder of the live or suppressed project record whose
// name or alias is name (lower case), "" for none.
func RepoOwner(recs map[string]*Record, name string) string {
	n := strings.ToLower(name)
	var hit []string
	for f, r := range recs {
		if Broad(r.Kind) != "project" {
			continue
		}
		for _, x := range r.Names() {
			if x == n {
				hit = append(hit, f)
			}
		}
	}
	sort.Strings(hit)
	if len(hit) == 0 {
		return ""
	}
	// Its own folder first: projects/<name>.
	for _, f := range hit {
		if f == "projects/"+n {
			return f
		}
	}
	return hit[0]
}
