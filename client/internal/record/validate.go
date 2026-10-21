package record

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/agent-sh/tacit/client/internal/fact"
)

// Problem is one refused value of a record: the field, the rule it breaks
// and a hint with the value and what to do instead.
type Problem struct {
	Field string
	Rule  string
	Hint  string
	// Folder is the record's folder, set by Chains (a whole-set check).
	Folder string
}

func (p Problem) String() string { return p.Field + ": " + p.Rule }

// Message is the problem with its hint, for the writer.
func (p Problem) Message() string {
	if p.Hint == "" {
		return p.String()
	}
	return p.String() + " (" + p.Hint + ")"
}

// Env is what Validate knows beyond the two records.
type Env struct {
	// Today is the writer's day (YYYY-MM-DD): a new history entry carries it.
	Today string
	// Records are the store's records as they are now (folder -> record),
	// the record checked included or not: aliases and suppression are
	// checked against the others.
	Records map[string]*Record
	// KnownSession reports a session or note a why cites (claude:<id>,
	// codex:<id>, note:<name>, ...) as one this machine or the store knows:
	// in the dreamer's registry, an inbox note's source.session, the quote
	// ledger. Nil: only the form is checked (lint).
	KnownSession func(cite string) bool
	// KnownCommit reports a commit a why cites (commit:<sha>) as one of the
	// folder's own: in its checkout, under this machine's git identity. Nil
	// with KnownSession set: no commit is known; both nil: only the form.
	KnownCommit func(folder, sha string) bool
	// QuoteBacked reports the owner's words (quote, where) as the store's
	// quote ledger records them. Nil: no quote is backed.
	QuoteBacked func(quote, from string) bool
	// ToolUsed reports a tool-name pattern that matched a tool call of the
	// cited session (the dreamer's tool usage on this machine). Nil: not
	// checked (another machine's sessions, lint).
	ToolUsed func(session, pattern string) bool
	// KnownRepo reports a repository name a topic's repos may name. Nil:
	// not checked.
	KnownRepo func(name string) bool
	// HoldsFacts reports a folder of the store that holds facts. Nil: not
	// checked (lint).
	HoldsFacts func(folder string) bool
	// Static is lint's check: the record as it stands, without the writer
	// (no history entry is new, no day is today's).
	Static bool
}

var (
	citeRx    = regexp.MustCompile(`\b([a-z][a-z0-9-]*):([A-Za-z0-9][A-Za-z0-9._-]*[A-Za-z0-9])`)
	citeKinds = func() map[string]bool {
		m := map[string]bool{"note": true, "session": true, "commit": true}
		for h := range fact.Harnesses {
			m[h] = true
		}
		return m
	}()
	toolRx = regexp.MustCompile(`^[A-Za-z0-9_*?.\-]+$`)
)

// Cites are the session, note and commit citations in text: <harness>:<id>,
// note:<name> or commit:<sha>, in the order they appear, once each.
func Cites(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range citeRx.FindAllStringSubmatch(text, -1) {
		if !citeKinds[m[1]] {
			continue
		}
		c := m[1] + ":" + m[2]
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out
}

// CiteID is a citation's id: the session id or note name after the colon.
func CiteID(cite string) string {
	_, id, _ := strings.Cut(cite, ":")
	return id
}

// Sessions are the distinct session ids r cites: in its creation entry's
// why and its evidence's sessions (notes and commits not counted), sorted.
func (r *Record) Sessions() []string {
	set := map[string]bool{}
	if n := len(r.Was); n > 0 {
		for _, c := range Cites(r.Was[n-1].Why) {
			if !strings.HasPrefix(c, "note:") && !strings.HasPrefix(c, "commit:") {
				set[CiteID(c)] = true
			}
		}
	}
	for _, e := range r.Evidence {
		for _, s := range e.Sessions {
			set[CiteID(s)] = true
		}
	}
	out := make([]string, 0, len(set))
	for s := range set {
		if s != "" {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// MinAreaSessions is how many distinct sessions an area or topic the
// curator's model creates must rest on (the owner's rule: Opus with two
// sessions of evidence is enough).
const MinAreaSessions = 2

// Validate checks rec, the record the writer by (ByOpus or ByOwner) wants
// in its folder, against old, the record there now (nil for a new one).
// Every problem is returned; an empty list accepts it.
func Validate(old, rec *Record, by string, env Env) []Problem {
	var out []Problem
	add := func(field, rule, hint string) { out = append(out, Problem{Field: field, Rule: rule, Hint: hint}) }
	if rec == nil {
		return []Problem{{Field: "record", Rule: "missing"}}
	}
	folder := rec.Folder
	allowed := FolderKinds(folder)
	switch {
	case len(allowed) == 0:
		add("folder", "is no folder that holds a record", fmt.Sprintf("%q; records live in projects/<name>, areas/<name>, areas/<area>/topics/<name> and topics/<name>", folder))
	case !kinds[rec.Kind]:
		add("kind", "not repo, folder, area or topic", fmt.Sprintf("got %q", rec.Kind))
	case !contains(allowed, rec.Kind):
		add("kind", "does not match the folder", fmt.Sprintf("%s holds kind %s, got %q", folder, strings.Join(allowed, " or "), rec.Kind))
	}
	if rec.Name != path.Base(folder) || !fact.IsSlug(rec.Name) {
		add("name", "is not the folder's last part", fmt.Sprintf("got %q for %s", rec.Name, folder))
	}
	d := strings.TrimSpace(rec.Description)
	switch {
	case d == "":
		add("description", "empty", "one line: what the folder is")
	case len([]rune(d)) > 200 || strings.ContainsAny(d, "\n\r"):
		add("description", "more than one line of 200 characters", "")
	}
	if !statuses[rec.Status] {
		add("status", "not an allowed status", fmt.Sprintf("got %q; one of %s", rec.Status, strings.Join(Statuses(), ", ")))
	}
	if !fact.IsDate(rec.Created) {
		add("created", "not YYYY-MM-DD", fmt.Sprintf("got %q", rec.Created))
	}
	if rec.CreatedBy != ByOpus && rec.CreatedBy != ByOwner {
		add("created_by", "not opus or owner", fmt.Sprintf("got %q", rec.CreatedBy))
	}

	// The history.
	if len(rec.Was) == 0 {
		add("was", "empty: the history holds at least the current status", "")
	} else if rec.Was[0].Status != rec.Status {
		add("was", "its newest entry is not the current status", fmt.Sprintf("status %q, newest entry %q", rec.Status, rec.Was[0].Status))
	}
	for i, e := range rec.Was {
		f := fmt.Sprintf("was[%d]", i)
		if !statuses[e.Status] {
			add(f+".status", "not an allowed status", fmt.Sprintf("got %q", e.Status))
		}
		if !fact.IsDate(e.Date) {
			add(f+".date", "not YYYY-MM-DD", fmt.Sprintf("got %q", e.Date))
		}
		if e.By != ByOpus && e.By != ByOwner {
			add(f+".by", "not opus or owner", fmt.Sprintf("got %q", e.By))
		}
		if strings.TrimSpace(e.Why) == "" {
			add(f+".why", "empty", "say why, citing the sessions or the owner's words it rests on")
		}
		if (e.Quote == "") != (e.QuoteFrom == "") {
			add(f+".quote", "quote and quote_from go together", "")
		}
		if e.Quote != "" && e.QuoteFrom != "" && env.QuoteBacked != nil && !env.QuoteBacked(e.Quote, e.QuoteFrom) {
			add(f+".quote", "the quote ledger does not record these words of the owner's", "")
		}
		if i > 0 && e.Date > rec.Was[i-1].Date {
			add(f+".date", "is later than the entry above it (newest first)", "")
		}
	}
	var fresh *Entry // the entry this write adds, if any
	switch {
	case env.Static:
	case old == nil:
		if len(rec.Was) != 1 {
			add("was", "a new record starts with exactly one entry", fmt.Sprintf("got %d", len(rec.Was)))
		}
		if len(rec.Was) > 0 {
			fresh = &rec.Was[0]
		}
	case sameEntries(rec.Was, old.Was):
	case len(rec.Was) == len(old.Was)+1 && sameEntries(rec.Was[1:], old.Was):
		fresh = &rec.Was[0]
	default:
		add("was", "is append only: the new history is the old one with at most one new entry on top", "")
	}
	if old != nil && !env.Static {
		if old.Created != rec.Created || old.CreatedBy != rec.CreatedBy {
			add("created", "a record's creation never changes", "")
		}
		if old.Kind != rec.Kind && !(by == ByOwner || fresh != nil) {
			add("kind", "changes only with a new history entry that says why", "")
		}
		if old.Status != rec.Status && fresh == nil {
			add("status", "changes only with a new history entry", "")
		}
	}
	ownerQuote := func(e *Entry) bool {
		return e != nil && e.Quote != "" && env.QuoteBacked != nil && env.QuoteBacked(e.Quote, e.QuoteFrom) && namesFolder(e.Quote, rec)
	}
	if fresh != nil {
		if fresh.Date != env.Today {
			add("was[0].date", "a new entry is dated today", fmt.Sprintf("got %q, today is %s", fresh.Date, env.Today))
		}
		switch {
		case fresh.By == by:
		case by == ByOpus && fresh.By == ByOwner && ownerQuote(fresh):
			// The owner's own words, directly about this folder, ledger-backed:
			// their decision, written for them (an owner quote beats Opus).
		case by == ByOpus && fresh.By == ByOwner:
			add("was[0].by", "owner is the owner's channel, or their words: a quote of theirs the ledger backs and that names this folder", "write by: opus and cite the quote in why as evidence")
		default:
			add("was[0].by", "is not the writer", fmt.Sprintf("the writer is %s, got %q", by, fresh.By))
		}
		if fresh.By == ByOpus {
			cites := Cites(fresh.Why)
			known := 0
			for _, c := range cites {
				switch {
				case env.KnownSession == nil:
					known++
				case strings.HasPrefix(c, "commit:"):
					if env.KnownCommit != nil && env.KnownCommit(folder, CiteID(c)) {
						known++
					}
				case env.KnownSession(c):
					known++
				}
			}
			// A folder no session ever touched (code's evidence shows none) has
			// no session to cite: a status that says so, and claims no work,
			// rests on that evidence itself.
			quietOK := noSessions(rec) && (rec.Status == StatusDead || rec.Status == StatusDormant || rec.Status == StatusNeedsReview)
			if known == 0 && !quietOK {
				add("was[0].why", "cites no session, note or commit code can find", "cite the session ids (claude:<id>, codex:<id>), note:<name> or the folder's own commits (commit:<sha>, as the evidence lists them) the decision rests on; a git date alone is no reason; a folder whose evidence shows no session at all may only be dead, dormant or needs-review")
			}
		}
		if fresh.Quote != "" && !ownerQuote(fresh) && fresh.By == ByOwner && by == ByOpus {
			add("was[0].quote", "is not a quote of the owner's that the ledger backs and that names the folder", "")
		}
		// The owner's status is never silently lost: Opus may only flag it.
		if old != nil && len(old.Was) > 0 && old.Was[0].By == ByOwner && fresh.By == ByOpus && rec.Status != StatusNeedsReview {
			add("status", "the owner set this folder's status: an agent may only flag it for review", fmt.Sprintf("use %s; the owner's %q stays in was", StatusNeedsReview, old.Status))
		}
	}
	if old == nil && !env.Static && rec.CreatedBy != by && !(rec.CreatedBy == ByOwner && fresh != nil && fresh.By == ByOwner && ownerQuote(fresh)) {
		add("created_by", "is not the writer", fmt.Sprintf("the writer is %s, got %q", by, rec.CreatedBy))
	}
	// The owner's pointer and names stay unless they change them.
	if old != nil && !env.Static && by == ByOpus && ownerMade(old) {
		if old.SuppressedBy != rec.SuppressedBy {
			add("suppressed_by", "the owner made this record: an agent does not change its suppression", "")
		}
		for _, a := range old.Aliases {
			if !contains(rec.Aliases, a) {
				add("aliases", "the owner made this record: an agent does not drop its aliases", a)
			}
		}
	}

	// Names.
	seenAlias := map[string]bool{}
	for _, a := range rec.Aliases {
		switch {
		case !fact.IsSlug(a):
			add("aliases", "not a slug (lower-case letters, digits and dashes)", fmt.Sprintf("got %q", a))
		case a == rec.Name:
			add("aliases", "is the folder's own name", a)
		case seenAlias[a]:
			add("aliases", "listed twice", a)
		case Broad(rec.Kind) == "project" && env.HoldsFacts != nil && env.HoldsFacts("projects/"+a) && !(old != nil && contains(old.Aliases, a)):
			// That folder needs a record of its own, suppressed into this
			// one, so its facts move here; an alias would leave them behind.
			add("aliases", "is the name of a project folder that holds facts", fmt.Sprintf("projects/%s holds facts: give it its own record with suppressed_by: %s instead", a, folder))
		}
		seenAlias[a] = true
	}
	for f, o := range env.Records {
		if f == folder || o == nil {
			continue
		}
		for _, a := range rec.Aliases {
			if contains(o.Names(), a) {
				add("aliases", "is another folder's name or alias", fmt.Sprintf("%s is %s's", a, f))
			}
		}
		if contains(o.Aliases, rec.Name) {
			add("name", "is another folder's alias", fmt.Sprintf("%s is an alias of %s", rec.Name, f))
		}
		if rec.Kind == KindTopic && o.Kind == KindTopic && o.Name == rec.Name {
			add("name", "a topic's name is unique across topics/ and every area's topics", fmt.Sprintf("%s is already %s", rec.Name, f))
		}
	}

	// Suppression.
	if s := rec.SuppressedBy; s != "" {
		target := env.Records[s]
		switch {
		case s == folder:
			add("suppressed_by", "points at the folder itself", "")
		case !IsFolder(s):
			add("suppressed_by", "is no folder that holds a record", fmt.Sprintf("got %q", s))
		case target == nil:
			add("suppressed_by", "names a folder with no record", fmt.Sprintf("%s; give it a record first", s))
		case !target.Live():
			add("suppressed_by", "names a folder that is itself suppressed: no chains", fmt.Sprintf("%s is suppressed by %s; point at that", s, target.SuppressedBy))
		case Broad(target.Kind) != Broad(rec.Kind):
			add("suppressed_by", "names a folder of another kind", fmt.Sprintf("a %s folds into a project, an area or topic into an area or topic; %s is a %s", rec.Kind, s, target.Kind))
		}
		for f, o := range env.Records {
			if f != folder && o != nil && o.SuppressedBy == folder {
				add("suppressed_by", "another folder is suppressed into this one: no chains", fmt.Sprintf("%s points here; point it at %s first", f, s))
			}
		}
	}

	// What each kind may hold.
	if rec.Kind != KindArea && (len(rec.Tools) > 0 || len(rec.Commands) > 0) {
		add("tools", "only an area has tools and commands", "")
	}
	if rec.Kind != KindTopic && len(rec.Repos) > 0 {
		add("repos", "only a topic has repos", "")
	}
	if rec.Kind != KindFolder && len(rec.Roots) > 0 {
		add("roots", "only a folder of kind folder has roots", "")
	}
	sessions := rec.Sessions()
	for _, t := range rec.Tools {
		if !toolRx.MatchString(t) || strings.Trim(t, "*?") == "" {
			add("tools", "not a tool-name pattern (letters, digits, _ . - and the globs * ?)", fmt.Sprintf("got %q", t))
			continue
		}
		if _, err := path.Match(t, ""); err != nil {
			add("tools", "not a well-formed glob", fmt.Sprintf("got %q", t))
			continue
		}
		if env.ToolUsed != nil && !env.Static && by == ByOpus && !(old != nil && contains(old.Tools, t)) {
			used := false
			for _, s := range sessions {
				used = used || env.ToolUsed(s, t)
			}
			if !used {
				add("tools", "matched no tool call of a session the record cites", fmt.Sprintf("%q; name a tool the cited sessions used", t))
			}
		}
	}
	for _, c := range rec.Commands {
		if strings.TrimSpace(c) == "" || c != strings.TrimSpace(c) || strings.IndexFunc(c, unicode.IsControl) >= 0 || len(c) > 80 {
			add("commands", "not a command prefix (one line, no surrounding spaces, at most 80 characters)", fmt.Sprintf("got %q", c))
		}
	}
	for _, r := range rec.Repos {
		if !fact.IsSlug(r) {
			add("repos", "not a repository name", fmt.Sprintf("got %q", r))
		} else if env.KnownRepo != nil && !env.KnownRepo(r) {
			add("repos", "names no known repository", r)
		}
	}
	for _, rt := range rec.Roots {
		if !fact.IsSlug(rt.Host) || strings.TrimSpace(rt.Path) == "" || strings.IndexFunc(rt.Path, unicode.IsControl) >= 0 {
			add("roots", "each is a host and a path", fmt.Sprintf("got %+v", rt))
		}
	}
	for i, e := range rec.Evidence {
		for _, dt := range []string{e.Gathered, e.FirstCommit, e.LastCommit, e.FirstWork, e.LastWork, e.FirstFact, e.LastFact} {
			if dt != "" && !fact.IsDate(dt) {
				add(fmt.Sprintf("evidence[%d]", i), "a date is not YYYY-MM-DD", dt)
			}
		}
		if e.Host != "" && !fact.IsSlug(e.Host) {
			add(fmt.Sprintf("evidence[%d].host", i), "not a host name", e.Host)
		}
	}
	// An area or topic the model made rests on two sessions, counted by code.
	if (rec.Kind == KindArea || rec.Kind == KindTopic) && rec.CreatedBy == ByOpus && len(sessions) < MinAreaSessions {
		add("evidence", fmt.Sprintf("an area or topic the curator creates rests on at least %d distinct sessions", MinAreaSessions), fmt.Sprintf("it cites %d", len(sessions)))
	}
	return out
}

// noSessions reports a record whose evidence (code's) shows no session at
// all: none worked there, none filed a note there, none proposed it.
func noSessions(r *Record) bool {
	for _, e := range r.Evidence {
		if e.WorkSessions > 0 || e.FactSessions > 0 || len(e.Sessions) > 0 {
			return false
		}
	}
	return true
}

// ownerMade reports a record the owner created or whose current status is theirs.
func ownerMade(r *Record) bool {
	return r.CreatedBy == ByOwner || (len(r.Was) > 0 && r.Was[0].By == ByOwner)
}

// namesFolder reports whether quote names the folder or one of its
// aliases as a whole word: the owner's words are about it.
func namesFolder(quote string, r *Record) bool {
	q := strings.ToLower(quote)
	for _, n := range r.Names() {
		if WholeWord(q, n) {
			return true
		}
	}
	return false
}

// WholeWord reports name in text (both lower case) with no letter, digit,
// dash or underscore on either side.
func WholeWord(text, name string) bool {
	if name == "" {
		return false
	}
	for off := 0; ; {
		i := strings.Index(text[off:], name)
		if i < 0 {
			return false
		}
		i += off
		j := i + len(name)
		if (i == 0 || !wordByte(text[i-1])) && (j == len(text) || !wordByte(text[j])) {
			return true
		}
		off = i + 1
	}
}

func wordByte(b byte) bool {
	return b == '-' || b == '_' || ('a' <= b && b <= 'z') || ('A' <= b && b <= 'Z') || ('0' <= b && b <= '9')
}

func sameEntries(a, b []Entry) bool {
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

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// Chains lists the suppression problems of a set of records as a whole:
// a pointer to a folder that is itself suppressed (a chain) or a loop.
func Chains(recs map[string]*Record) []Problem {
	var out []Problem
	folders := make([]string, 0, len(recs))
	for f := range recs {
		folders = append(folders, f)
	}
	sort.Strings(folders)
	for _, f := range folders {
		r := recs[f]
		if r.SuppressedBy == "" {
			continue
		}
		if _, _, err := Resolve(recs, f); err != nil {
			out = append(out, Problem{Field: "suppressed_by", Rule: "loops", Folder: f})
			continue
		}
		if t := recs[r.SuppressedBy]; t != nil && t.SuppressedBy != "" {
			out = append(out, Problem{Field: "suppressed_by", Rule: "points at a folder that is itself suppressed", Hint: fmt.Sprintf("%s -> %s -> %s", f, r.SuppressedBy, t.SuppressedBy), Folder: f})
		}
	}
	return out
}
