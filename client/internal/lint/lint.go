// Package lint checks a store tree against the rules in SPEC.md (L1 to L17).
package lint

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/index"
	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/ownerwords"
	"github.com/agent-sh/tacit/client/internal/record"
	"github.com/agent-sh/tacit/client/internal/tree"
)

// Finding is one rule violation.
type Finding struct {
	Rule string // L1..L12
	Path string
	Msg  string
}

func (f Finding) String() string { return fmt.Sprintf("%s %s: %s", f.Rule, f.Path, f.Msg) }

// Run lints the store at root and returns findings sorted by path then rule.
func Run(root string) ([]Finding, error) {
	facts, perrs, err := fact.Walk(root)
	if err != nil {
		return nil, err
	}
	var out []Finding
	for _, e := range perrs {
		path, msg, _ := strings.Cut(e.Error(), ": ")
		out = append(out, Finding{"L1", path, strings.Join(strings.Fields(msg), " ")})
	}
	ev := readEvidence(root)
	names := map[string]bool{}
	for _, f := range facts {
		names[f.Meta.Name] = true
	}
	for _, f := range facts {
		out = append(out, check(f, names)...)
		out = append(out, ownerEvidence(f, ev)...)
	}
	out = append(out, records(root, facts)...)
	// L10: committed index must equal the generated one.
	if b, rerr := os.ReadFile(filepath.Join(root, "MEMORY.md")); rerr == nil {
		if want := index.Generate(facts); string(b) != want {
			out = append(out, Finding{"L10", "MEMORY.md", "differs from generated index; run `tacit index`"})
		}
	}
	// L16: the folder indexes equal the generated ones (once the store has
	// its root index; a store from before the tree is checked only for the
	// index files it holds).
	recs, _ := record.Walk(root)
	stale, terr := tree.Check(root, tree.GenerateWith(facts, recs, ""))
	if terr != nil {
		return nil, terr
	}
	for _, msg := range stale {
		p, m, _ := strings.Cut(msg, ": ")
		out = append(out, Finding{"L16", p, m})
	}
	return out, nil
}

func check(f *fact.Fact, names map[string]bool) []Finding {
	m := f.Meta
	var out []Finding
	add := func(rule, msg string) { out = append(out, Finding{rule, f.Path, msg}) }

	// L1 required fields
	for _, kv := range [][2]string{{"name", m.Name}, {"description", m.Description}, {"type", m.Type}, {"scope", m.Scope}, {"source.harness", m.Source.Harness}, {"source.host", m.Source.Host}, {"source.kind", m.Source.Kind}, {"created", m.Created}} {
		if kv[1] == "" {
			add("L1", "missing "+kv[0])
		}
	}
	// L2 name is the stem and a slug
	if m.Name != "" {
		if !fact.IsSlug(m.Name) {
			add("L2", "name is not a slug: "+m.Name)
		}
		if m.Name != f.Stem() {
			add("L2", fmt.Sprintf("name %q does not equal file stem %q", m.Name, f.Stem()))
		}
	}
	// L3 known type; unclassified only in inbox
	if m.Type != "" {
		if !fact.Types[m.Type] {
			add("L3", "unknown type "+m.Type)
		} else if m.Type == "unclassified" && !f.InInbox() {
			add("L3", "unclassified outside inbox/")
		}
	}
	// L4 curated dir equals scope; scope shape valid everywhere
	if m.Scope != "" {
		if !fact.ValidScope(m.Scope) {
			add("L4", "invalid scope "+m.Scope)
		} else if !f.InInbox() && f.Dir() != m.Scope {
			add("L4", fmt.Sprintf("directory %q does not equal scope %q", f.Dir(), m.Scope))
		}
	}
	if f.InInbox() {
		parts := strings.Split(f.Path, "/")
		harness, host, ok := "", "", false
		if len(parts) >= 3 {
			harness, host, ok = strings.Cut(parts[1], "@")
		}
		if !ok || !fact.Harnesses[harness] || !fact.IsSlug(host) {
			add("L4", "inbox path must be inbox/<harness>@<host>/... with a known harness")
		} else if m.Source.Harness != "" && m.Source.Harness != harness {
			// The directory is the writer; a note may not claim another one (a
			// harness human note outside inbox/human@ would pose as the owner's).
			add("L4", fmt.Sprintf("source.harness %q differs from the inbox directory's %q", m.Source.Harness, harness))
		}
	}
	// L5 dates
	if m.Created != "" && !fact.IsDate(m.Created) {
		add("L5", "created is not YYYY-MM-DD: "+m.Created)
	}
	if m.Updated != "" && !fact.IsDate(m.Updated) {
		add("L5", "updated is not YYYY-MM-DD: "+m.Updated)
	}
	if m.LastChecked != "" && !fact.IsDate(m.LastChecked) {
		add("L5", "last_checked is not YYYY-MM-DD: "+m.LastChecked)
	}
	// L6 links resolve (curated only; inbox links are resolved by the curator
	// on promotion, and imported facts may point at facts that were never written)
	if !f.InInbox() {
		for _, l := range f.Links() {
			if !names[l] {
				add("L6", "unresolved link [["+l+"]]")
			}
		}
	}
	// L7 superseded_by resolves
	if m.SupersededBy != "" && !names[m.SupersededBy] {
		add("L7", "superseded_by names no fact: "+m.SupersededBy)
	}
	// L14 retired is a date, and only a curated fact is retired
	if m.Retired != "" {
		if !fact.IsDate(m.Retired) {
			add("L14", "retired is not YYYY-MM-DD: "+m.Retired)
		} else if f.InInbox() {
			add("L14", "retired on an inbox fact")
		}
	}
	// L13 conflicts_with names existing facts and never the fact itself
	for _, c := range m.ConflictsWith {
		if c == m.Name {
			add("L13", "conflicts_with names itself")
		} else if !names[c] {
			add("L13", "conflicts_with names no fact: "+c)
		}
	}
	// L8 no web facts in system/
	if m.Scope == "system" && m.Source.Kind == "web" {
		add("L8", "system/ fact with source.kind web")
	}
	// L9 derived_from only and always under derived/
	isDerived := strings.HasPrefix(m.Scope, "derived/")
	if isDerived && m.DerivedFrom == "" {
		add("L9", "derived/ fact without derived_from")
	}
	if !isDerived && m.DerivedFrom != "" {
		add("L9", "derived_from on a non-derived fact")
	}
	// L11 description length, body present
	if len([]rune(m.Description)) > 200 {
		add("L11", fmt.Sprintf("description is %d chars (max 200)", len([]rune(m.Description))))
	}
	if strings.TrimSpace(f.Body) == "" {
		add("L11", "empty body")
	}
	// L15 a curated ruling carries the owner's words; evidence is well formed
	// L12 known enum values
	if m.Source.Kind != "" && !fact.SourceKinds[m.Source.Kind] {
		add("L12", "unknown source.kind "+m.Source.Kind)
	}
	if m.Source.Harness != "" && !fact.Harnesses[m.Source.Harness] {
		add("L12", "unknown source.harness "+m.Source.Harness)
	}
	return out
}

var (
	// <source>:<id>: claude:, codex:, pi:, hermes:<session> or note:<name> today. Any
	// lower-case source name passes, so a ledger entry from a source this
	// client no longer reads stays valid.
	quoteFromRx = regexp.MustCompile(`^[a-z][a-z0-9-]*:[A-Za-z0-9][A-Za-z0-9._-]*$`)
	decisionRx  = regexp.MustCompile(`^[0-9a-f]{12}$`)
)

// evidence is what L15 checks a fact's claims against in the store itself.
type evidence struct {
	quotes   map[string]ledger.Quote
	endorsed map[string]ledger.Endorsement
}

func readEvidence(root string) evidence {
	ql, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(ledger.QuotesPath)))
	dl, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(ledger.DecisionsPath)))
	return evidence{quotes: ledger.Quotes(ql), endorsed: ledger.Endorsements(dl)}
}

// ownerEvidence is L15. A curated fact with source.kind user is an owner
// ruling and must say why the words are theirs: written by them (harness human),
// a quote of theirs that the fact holds and that the curator's quote ledger
// records for it (review/owner-quotes.jsonl: name, quote, where, a hash of their
// turn), or their standing decision that kept it (review/decisions.jsonl). A
// claim the store does not back fails here even after the transcript it names
// is gone. Ruling evidence on a fact that is not kind user is an error too, so
// a demoted fact cannot keep a stale claim; owner_words, a phrase of theirs too
// short to be a ruling, may sit on any fact and needs its own ledger entry
// (marked words, so it never backs an owner_quote). Inbox notes may claim kind user (the
// curator decides), and a store the rulings audit has not run on yet keeps the
// old definition, so a bare kind user is a finding only after the audit.
func ownerEvidence(f *fact.Fact, ev evidence) []Finding {
	s := f.Meta.Source
	var out []Finding
	add := func(msg string) { out = append(out, Finding{"L15", f.Path, msg}) }
	hasAny := s.OwnerQuote != "" || s.OwnerQuoteFrom != "" || s.Endorsed != ""
	if hasAny && s.Kind != "user" {
		add("owner evidence on a fact whose source.kind is " + s.Kind)
	}
	if (s.OwnerQuote == "") != (s.OwnerQuoteFrom == "") {
		add("source.owner_quote and source.owner_quote_from go together")
	}
	if s.OwnerQuote != "" && !ownerwords.In(f.Meta.Description+"\n"+f.Body, s.OwnerQuote) {
		add("source.owner_quote is not in the fact's text, or is too short to be evidence")
	}
	if s.OwnerQuoteFrom != "" && !quoteFromRx.MatchString(s.OwnerQuoteFrom) {
		add("source.owner_quote_from is not <source>:<id> (claude:<session>, codex:<session>, pi:<session>, hermes:<session>, note:<name>): " + s.OwnerQuoteFrom)
	}
	if s.Endorsed != "" && !decisionRx.MatchString(s.Endorsed) {
		add("source.endorsed is not a decision id: " + s.Endorsed)
	} else if s.Endorsed != "" && !f.InInbox() && ev.endorsed[f.Meta.Name].ID != s.Endorsed {
		add("source.endorsed names no standing decision of the owner's that keeps this fact: " + s.Endorsed)
	}
	if s.OwnerQuote != "" && s.OwnerQuoteFrom != "" && !f.InInbox() {
		if _, ok := ev.quotes[ledger.Quote{Name: f.Meta.Name, Quote: s.OwnerQuote, From: s.OwnerQuoteFrom}.Key()]; !ok {
			add("source.owner_quote has no entry in the quote ledger (" + ledger.QuotesPath + ")")
		}
	}
	// owner_words: their phrase on a fact that stays an agent's (any kind), too
	// short to be a ruling's quote; the ledger records it as words.
	if (s.OwnerWords == "") != (s.OwnerWordsFrom == "") {
		add("source.owner_words and source.owner_words_from go together")
	}
	if s.OwnerWords != "" && !ownerwords.In(f.Meta.Description+"\n"+f.Body, s.OwnerWords) {
		add("source.owner_words is not in the fact's text")
	}
	if s.OwnerWordsFrom != "" && !quoteFromRx.MatchString(s.OwnerWordsFrom) {
		add("source.owner_words_from is not <source>:<id> (claude:<session>, codex:<session>, pi:<session>, hermes:<session>, note:<name>): " + s.OwnerWordsFrom)
	}
	if s.OwnerWords != "" && s.OwnerWordsFrom != "" && !f.InInbox() {
		if _, ok := ev.quotes[ledger.Quote{Name: f.Meta.Name, Quote: s.OwnerWords, From: s.OwnerWordsFrom, Words: true}.Key()]; !ok {
			add("source.owner_words has no entry in the quote ledger (" + ledger.QuotesPath + ")")
		}
	}
	if !f.Audited && !f.InInbox() && fact.ClaimsEvidence(f) {
		add("owner evidence in a store the rulings audit has not run on (no " + ledger.MarkerPath + "): only the audit writes it")
	}
	if s.Kind == "user" && !f.InInbox() && f.Audited && !fact.HasEvidence(f) {
		add("source.kind user without the owner's words: no owner_quote, no endorsement, not written by them (tacit rulings audit)")
	}
	return out
}

// records is L17, the folder records format (SPEC.md, "Folder records",
// "Store format"): the format marker reads; nothing of the format (a
// record, a fact in areas/ or topics/, last_checked) is in a store without
// it, since an older binary reads such a store wrong; every record parses
// and holds as it stands (record.Validate, static: the history's shape,
// the kind for its folder, aliases unique, suppression of the right kind
// with no chain or loop, an area or topic of the curator's on two
// sessions, the owner's words backed by the quote ledger); a fact in an
// area or topic folder has its folder's record; and once the marker says
// records are complete, every project folder with facts has one too.
func records(root string, facts []*fact.Fact) []Finding {
	var out []Finding
	add := func(p, msg string) { out = append(out, Finding{"L17", p, msg}) }
	format, haveFormat, ferr := ledger.ReadFormat(root)
	if ferr != nil {
		add(ledger.FormatPath, "the format marker cannot be read: "+strings.Join(strings.Fields(ferr.Error()), " "))
	}
	recs, errs := record.Walk(root)
	for _, e := range errs {
		p, msg, _ := strings.Cut(e.Error(), ": ")
		add(p, strings.Join(strings.Fields(msg), " "))
	}
	if !haveFormat {
		for f := range recs {
			add(record.Rel(f), "a folder record in a store without the format marker ("+ledger.FormatPath+")")
		}
		for _, f := range facts {
			if !f.InInbox() && fact.NewRoot(f.Meta.Scope) {
				add(f.Path, "a fact in "+f.Meta.Scope+" in a store without the format marker")
			}
			if f.Meta.LastChecked != "" {
				add(f.Path, "last_checked in a store without the format marker")
			}
		}
	}
	ql, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(ledger.QuotesPath)))
	env := record.Env{Static: true, Records: recs, QuoteBacked: ledger.Backing(ql)}
	folders := make([]string, 0, len(recs))
	for f := range recs {
		folders = append(folders, f)
	}
	sort.Strings(folders)
	for _, f := range folders {
		for _, p := range record.Validate(nil, recs[f], "", env) {
			add(record.Rel(f), p.String())
		}
	}
	for _, p := range record.Chains(recs) {
		add(record.Rel(p.Folder), p.String())
	}
	missing := map[string]bool{}
	for _, f := range facts {
		if f.InInbox() || !record.IsFolder(f.Meta.Scope) || f.Dir() != f.Meta.Scope {
			continue
		}
		r := recs[f.Meta.Scope]
		switch {
		case fact.NewRoot(f.Meta.Scope) && r == nil:
			add(f.Path, "a fact in "+f.Meta.Scope+", which has no record")
		case r == nil && format.RecordsComplete && !missing[f.Meta.Scope]:
			missing[f.Meta.Scope] = true
			add(f.Meta.Scope, "a folder with facts and no record (the format marker says records are complete)")
		}
	}
	return out
}
