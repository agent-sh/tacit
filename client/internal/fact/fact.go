// Package fact reads and writes one fact file: YAML frontmatter plus a
// Markdown body, as specified in SPEC.md.
package fact

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Types, scopes and source kinds the spec allows.
var (
	Types = set("user_standard", "design_decision", "past_mistake", "machine_fact", "code_change", "hot_spot", "structure", "history", "user", "reference", "result", "result_ref", "unclassified", StatementType)
	// Harnesses name key ids (<harness>@<host>) and inbox directories. The
	// server keeps the same list (server/lib/tacit_server/auth/key_id.ex).
	// "reader" is a key that only reads (a Mac over the public path): the
	// server refuses its pushes, and no fact is ever written as it.
	Harnesses   = set("claude", "codex", "pi", "hermes", "grok", "muse", "agy", "dreamer", "import", "curator", "human", "reader")
	SourceKinds = set("user", "observed", "web", "derived")
	ScopeRoots  = set("system", "user", "projects", "machines", "derived", "areas", "topics", "orgs")
)

var (
	slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	dateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	linkRe = regexp.MustCompile(`\[\[([A-Za-z0-9_][A-Za-z0-9_.-]*)([|#][^\]\n]*)?\]\]`)
)

// IsResult reports a result read from git (internal/results): `result` for a
// row log or a group a curated fact quotes, which the prompt hook may print,
// `result_ref` for the rest, which only search and the MCP tool find.
func IsResult(typ string) bool { return typ == "result" || typ == "result_ref" }

// AppliesTo is the situation join key.
type AppliesTo struct {
	Paths    []string `yaml:"paths,omitempty"`
	Symbols  []string `yaml:"symbols,omitempty"`
	Repos    []string `yaml:"repos,omitempty"`
	Hosts    []string `yaml:"hosts,omitempty"`
	Commands []string `yaml:"commands,omitempty"`
}

// Empty reports whether no list is set; such a fact is pull-only.
func (a AppliesTo) Empty() bool {
	return len(a.Paths)+len(a.Symbols)+len(a.Repos)+len(a.Hosts)+len(a.Commands) == 0
}

// Source records where a fact came from.
type Source struct {
	Harness      string `yaml:"harness"`
	Host         string `yaml:"host"`
	Session      string `yaml:"session,omitempty"`
	Kind         string `yaml:"kind"`
	ImportedFrom string `yaml:"imported_from,omitempty"`
	OriginalType string `yaml:"original_type,omitempty"`
	// The evidence that a kind user fact is the owner's own words (SPEC.md,
	// Rulings): a phrase of the fact found verbatim in one of their turns or
	// notes, and where; or the decision in review/decisions.jsonl where the owner
	// kept this fact themself.
	OwnerQuote     string `yaml:"owner_quote,omitempty"`
	OwnerQuoteFrom string `yaml:"owner_quote_from,omitempty"`
	Endorsed       string `yaml:"endorsed,omitempty"`
	// OwnerWords: a phrase of theirs that the fact holds, found in one of their
	// turns or notes, too few words to prove a ruling (ownerwords.ProvesRuling).
	// The fact stays an agent's statement; the words are evidence the verifier
	// weighs, recorded in the quote ledger like a quote (ledger.Quote.Words).
	OwnerWords     string `yaml:"owner_words,omitempty"`
	OwnerWordsFrom string `yaml:"owner_words_from,omitempty"`
	// Review and ReviewReason: the reviewer's verdict on the agent's note a
	// curated fact was made from, and why (SPEC.md, "The reviewer"). Written
	// by the curator in a store of format 4 (ledger.ReviewsFormat) only; a
	// field is a claim, and counts only when the reviewer's ledger backs it
	// (Fact.Review). Lint L21 checks the pair.
	Review       string `yaml:"review,omitempty"`
	ReviewReason string `yaml:"review_reason,omitempty"`
}

// OwnerWrote reports a fact the owner wrote themself in tacit app. A curated
// fact of harness human is their New rule (only the curator key writes curated
// facts, and only the app writes harness human ones). An inbox note claims it
// with harness human in their inbox/human@<host>/ directory, but a field and a
// directory are claims any writer can make: it counts only when the commit
// that added it is one of the app's owner commits (AppNote, set from git by
// the caller that loaded the store). A file of a machine's local overlay
// (path local/...) is never one: anything on that machine can write there,
// and the app never does.
func OwnerWrote(f *Fact) bool {
	if f == nil || f.Meta.Source.Harness != "human" || strings.HasPrefix(f.Path, "local/") {
		return false
	}
	if !f.InInbox() {
		return true
	}
	return strings.HasPrefix(f.Path, "inbox/human@") && f.AppNote
}

// HasEvidence reports whether a fact carries proof that its words are the
// owner's: the owner wrote them, a quote of theirs and where it is, or their endorsement.
//
// A field is only a claim: a quote counts when the store's quote ledger holds
// it for this fact (QuoteBacked), an endorsement when it names their standing
// decision that keeps this fact (EndorsedBacked). Walk and WithStore set both.
func HasEvidence(f *Fact) bool {
	s := f.Meta.Source
	return OwnerWrote(f) || (s.OwnerQuote != "" && s.OwnerQuoteFrom != "" && f.QuoteBacked) || (s.Endorsed != "" && f.EndorsedBacked)
}

// ClaimsEvidence reports evidence fields present on the file, backed or not.
func ClaimsEvidence(f *Fact) bool {
	s := f.Meta.Source
	return s.OwnerQuote != "" || s.OwnerQuoteFrom != "" || s.Endorsed != "" || s.OwnerWords != "" || s.OwnerWordsFrom != ""
}

// OwnerWordsOf is the phrase of theirs that a fact which is not their ruling
// holds (source.owner_words), when the store backs it: the quote ledger
// records it for this fact and no standing Not mine of theirs answers it
// (WordsBacked, set by Walk and WithStore). The verifier reads it as strong
// evidence and may not retire the fact against it.
func OwnerWordsOf(f *Fact) string {
	if f == nil || !f.WordsBacked || IsRuling(f) {
		return ""
	}
	return f.Meta.Source.OwnerWords
}

// IsRuling reports an owner ruling. In a store the rulings audit has run on
// (Audited: review/rulings-audit.json is there) it is source.kind user with
// the evidence that the words are theirs; everything else, whatever its type, is
// an agent's statement (SPEC.md, Rulings). Before the audit the store keeps
// the definition it had: source.kind user, or type user_standard.
func IsRuling(f *Fact) bool {
	if f == nil {
		return false
	}
	if !f.Audited {
		return f.Meta.Source.Kind == "user" || f.Meta.Type == "user_standard"
	}
	return f.Meta.Source.Kind == "user" && HasEvidence(f)
}

// Meta is the frontmatter.
type Meta struct {
	Name         string    `yaml:"name"`
	Description  string    `yaml:"description"`
	Type         string    `yaml:"type"`
	Scope        string    `yaml:"scope"`
	AppliesTo    AppliesTo `yaml:"applies_to,omitempty"`
	Source       Source    `yaml:"source"`
	Created      string    `yaml:"created"`
	Updated      string    `yaml:"updated,omitempty"`
	SupersededBy string    `yaml:"superseded_by,omitempty"`
	Retired      string    `yaml:"retired,omitempty"` // YYYY-MM-DD: outdated per git, retired on a staleness verdict; the evidence is in review/decisions.jsonl
	// LastChecked is the day the curator's rotation last judged the fact
	// against fresh proof (YYYY-MM-DD; empty: never). Only the rotation
	// writes it (PatchRelations); it is not a change to the fact, so the tree
	// index and the staleness check's dating do not read it. A store's
	// writers set it only once the store holds the format marker
	// (ledger.FormatPath): an older binary's strict decoder refuses the key.
	LastChecked   string   `yaml:"last_checked,omitempty"`
	DerivedFrom   string   `yaml:"derived_from,omitempty"`
	Origin        string   `yaml:"origin,omitempty"`         // curator output: name of the inbox note this fact came from
	ConflictsWith []string `yaml:"conflicts_with,omitempty"` // curator finding: facts this one contradicts, unresolved until one supersedes the other
}

// Fact is one parsed file.
type Fact struct {
	Path string // relative to the store root, forward slashes
	Meta Meta
	Body string

	// Store state, set by Walk (and AppNote by a caller that read git):
	// Audited, the store holds the rulings audit marker; AppNote, an inbox
	// note added by one of the app's owner commits.
	Audited bool
	AppNote bool
	// QuoteBacked, EndorsedBacked, WordsBacked: the store backs the fact's
	// evidence (its quote ledger, their standing decisions; StoreState.Apply).
	// NotMine: a standing decision of theirs says the fact is an agent's.
	// WordsRecorded: the ledger records its owner_words, whether or not a Not
	// mine of theirs lifted them (the app still shows them).
	QuoteBacked, EndorsedBacked, WordsBacked, WordsRecorded, NotMine bool
	// Review is the reviewer's verdict that stands on this agent's note, or
	// on the note a curated fact was made from (ledger.Reviews: ForNote,
	// ForOrigin), set by StoreState.Apply; "" when none (not reviewed, or no
	// agent's note). ReviewReason is why, ReviewDay its day.
	Review, ReviewReason, ReviewDay string
}

// Split separates frontmatter from body. ok is false when the file has no
// leading `---` block.
func Split(b []byte) (fm, body []byte, ok bool) {
	first, rest, _ := bytes.Cut(b, []byte("\n"))
	if string(bytes.TrimRight(first, "\r")) != "---" {
		return nil, b, false
	}
	start := len(b) - len(rest)
	for off := start; off < len(b); {
		line, _, found := bytes.Cut(b[off:], []byte("\n"))
		if string(bytes.TrimRight(line, "\r")) == "---" {
			end := off + len(line)
			if found {
				end++
			}
			return b[start:off], b[end:], true
		}
		if !found {
			break
		}
		off += len(line) + 1
	}
	return nil, b, false
}

// Decode parses frontmatter strictly: unknown keys are errors.
func Decode(fm []byte, dst any) error {
	dec := yaml.NewDecoder(bytes.NewReader(fm))
	dec.KnownFields(true)
	if err := dec.Decode(dst); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// DecodeLoose parses frontmatter from another system, ignoring unknown keys.
func DecodeLoose(fm []byte, dst any) error {
	if err := yaml.Unmarshal(fm, dst); err != nil {
		return err
	}
	return nil
}

// Parse reads one fact. relPath is the store-relative path used in reports.
func Parse(relPath string, data []byte) (*Fact, error) {
	fm, body, ok := Split(data)
	if !ok {
		return nil, errors.New("no frontmatter block")
	}
	var m Meta
	if err := Decode(fm, &m); err != nil {
		return nil, fmt.Errorf("frontmatter: %w", err)
	}
	return &Fact{Path: filepath.ToSlash(relPath), Meta: m, Body: string(body)}, nil
}

// Encode renders a fact file. Keys follow the struct order in SPEC.md.
func (f *Fact) Encode() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("---\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&f.Meta); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	buf.WriteString("---\n\n")
	buf.WriteString(strings.TrimLeft(f.Body, "\n"))
	if !strings.HasSuffix(buf.String(), "\n") {
		buf.WriteString("\n")
	}
	return buf.Bytes(), nil
}

// Links returns the [[names]] referenced in the body, deduplicated and sorted.
// A [[name]] in a fenced code block or an inline code span is code, not a link.
func (f *Fact) Links() []string {
	seen := map[string]bool{}
	for _, sp := range LinkSpans(f.Body) {
		seen[strings.TrimSpace(sp.Target)] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// LinkSpan is one [[link]] in a body: the byte range of the whole link, its
// target, and what follows the target inside the brackets (`|label` or
// `#section`, with the separator; empty for a plain link).
type LinkSpan struct {
	Start, End int
	Target     string
	Suffix     string
}

// LinkAt returns the [[link]] that starts at the first byte of s (Start is 0),
// and whether there is one. It does not look at code.
func LinkAt(s string) (LinkSpan, bool) {
	m := linkRe.FindStringSubmatchIndex(s)
	if m == nil || m[0] != 0 {
		return LinkSpan{}, false
	}
	sp := LinkSpan{Start: 0, End: m[1], Target: s[m[2]:m[3]]}
	if m[4] >= 0 {
		sp.Suffix = s[m[4]:m[5]]
	}
	return sp, true
}

// LinkSpans returns every [[link]] in body in the order they appear. It is the
// one definition of a link: Links, the linter (L6) and the curator's link repair
// all use it. A [[name]] inside a fenced code block (``` or ~~~), an indented
// code block (four spaces after a blank line, outside a list) or an inline code
// span is code (a TOML [[table]] header, an example), not a link.
func LinkSpans(body string) []LinkSpan {
	var out []LinkSpan
	if !strings.Contains(body, "[[") {
		return nil
	}
	code := codeRanges(body)
	for _, m := range linkRe.FindAllStringSubmatchIndex(body, -1) {
		if inCode(code, m[0], m[1]) {
			continue
		}
		sp := LinkSpan{Start: m[0], End: m[1], Target: body[m[2]:m[3]]}
		if m[4] >= 0 {
			sp.Suffix = body[m[4]:m[5]]
		}
		out = append(out, sp)
	}
	return out
}

// ClipDescription shortens a description to at most max runes at a word
// boundary (the index line is read by a model; a mid-word cut is noise).
func ClipDescription(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	cut := max - 1
	for cut > max/2 && r[cut] != ' ' {
		cut--
	}
	return strings.TrimRight(string(r[:cut]), " ,;:") + "…"
}

// Stem is the file name without extension.
func (f *Fact) Stem() string { return strings.TrimSuffix(filepath.Base(f.Path), ".md") }

// Dir is the store-relative directory.
func (f *Fact) Dir() string { return filepath.ToSlash(filepath.Dir(f.Path)) }

// InInbox reports whether the fact lives under inbox/.
func (f *Fact) InInbox() bool { return strings.HasPrefix(f.Path, "inbox/") }

// IsSlug, IsDate and ValidScope are the shape checks the linter uses.
func IsSlug(s string) bool { return slugRe.MatchString(s) }
func IsDate(s string) bool { return dateRe.MatchString(s) }

// ValidScope accepts `system`, `user`, `projects/<repo>`, `machines/<host>`,
// `derived/<repo>`, `areas/<area>`, `areas/<area>/topics/<topic>`,
// `topics/<topic>` and `orgs/<org>`: the one scope deeper than two parts is a topic an area
// owns. The parts after the root are slugs for the new roots; the older
// roots keep their shape check (one part, not empty).
func ValidScope(s string) bool {
	root, rest, has := strings.Cut(s, "/")
	if !ScopeRoots[root] {
		return false
	}
	switch root {
	case "system", "user":
		return !has
	case "areas":
		parts := strings.Split(rest, "/")
		switch len(parts) {
		case 1:
			return IsSlug(parts[0])
		case 3:
			return IsSlug(parts[0]) && parts[1] == "topics" && IsSlug(parts[2])
		}
		return false
	case "topics", "orgs":
		return has && IsSlug(rest)
	default:
		return has && rest != "" && !strings.Contains(rest, "/")
	}
}

// NewRoot reports a scope under the roots of the folder records format
// (areas/, topics/, and format 2's orgs/): written only into a store with
// the format marker (orgs/: of format 2, OrgRoot).
func NewRoot(scope string) bool {
	root, _, _ := strings.Cut(scope, "/")
	return root == "areas" || root == "topics" || root == "orgs"
}

// OrgRoot reports a scope under orgs/, a forge organization's folder:
// written only into a store whose format marker is 2 or newer.
func OrgRoot(scope string) bool { return strings.HasPrefix(scope, "orgs/") }

// IndexFile is the generated index file of a store folder (internal/tree).
// Its name starts with an underscore, which a fact's name (a slug) cannot,
// so it never collides with a fact and is never read as one.
const IndexFile = "_index.md"

// RecordFile is a folder's record (internal/record): what the folder is
// (kind, status and its history, aliases, a suppression pointer), written
// by the curator or the owner. OverviewFile is a folder's cited overview.
// Both are store files, not facts (IsStoreFile).
const (
	RecordFile   = "_record.md"
	OverviewFile = "_overview.md"
)

// IsIndexFile reports a folder index file by its base name.
func IsIndexFile(name string) bool { return name == IndexFile }

// IsStoreFile reports a file of the store that is not a fact, by its base
// name: every name that starts with an underscore (the folder index, a
// folder's record and overview, a machine's catalog). A fact's name is a
// slug, which never starts with one, so the two never collide; every
// reader of facts (Walk, the local layer, the metadata checks) passes
// these over, whatever new kind of store file a later binary adds.
func IsStoreFile(name string) bool { return strings.HasPrefix(name, "_") }

// ParseError is a fact file Walk could not read as a fact: its path,
// relative to the walk's root, and why.
type ParseError struct {
	Path string
	Err  error
}

func (e *ParseError) Error() string { return e.Path + ": " + e.Err.Error() }
func (e *ParseError) Unwrap() error { return e.Err }

// UnreadNames are the names the files of errs hold by their stem: a file
// that does not parse still takes its name, so a writer never names a new
// fact after it and replaces it.
func UnreadNames(errs []error) []string {
	var out []string
	for _, err := range errs {
		var pe *ParseError
		if errors.As(err, &pe) {
			out = append(out, strings.TrimSuffix(path.Base(pe.Path), ".md"))
		}
	}
	return out
}

// Walk loads every .md fact under root, in path order. Skipped: the
// generated MEMORY.md, every store file (IsStoreFile: the folder indexes,
// records and overviews, _index.md, _record.md, _overview.md), a root README.md,
// keys/, review/, dot directories and symlinks (a symlink could point
// outside the store). Files that fail to
// parse are returned in errs with their path.
func Walk(root string) (facts []*Fact, errs []error, err error) {
	err = filepath.WalkDir(root, func(p string, d os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		name := d.Name()
		if d.IsDir() {
			if p != root && (strings.HasPrefix(name, ".") || name == "keys" || name == "review") {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(name, ".md") || IsStoreFile(name) {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if rel == "MEMORY.md" || rel == "README.md" {
			return nil
		}
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		f, perr := Parse(rel, data)
		if perr != nil {
			errs = append(errs, &ParseError{Path: rel, Err: perr})
			return nil
		}
		facts = append(facts, f)
		return nil
	})
	sort.Slice(facts, func(i, j int) bool { return facts[i].Path < facts[j].Path })
	if err != nil {
		return facts, errs, err
	}
	st, serr := LoadState(root)
	if serr != nil {
		return nil, nil, serr // fail closed: never judge facts without the marker they are under
	}
	for _, f := range facts {
		st.Apply(f)
	}
	return facts, errs, nil
}

func set(xs ...string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}
