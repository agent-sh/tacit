package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/gitev"
	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/links"
	"github.com/agent-sh/tacit/client/internal/lint"
	"github.com/agent-sh/tacit/client/internal/search"
	"github.com/agent-sh/tacit/client/internal/store"
)

// The owner's own writes, made from tacit app: edit a fact, retire or restore
// it, undo their last edit, write a new ruling, apply the curator's fix to a dead
// link in one of their rulings. Every write runs under the store lock on a fresh
// sync, passes the linter (a finding the store did not have before refuses the
// write and the caller keeps the draft), and is pushed once as curator, the
// writer the server lets touch curated facts, like a review mark. The commit
// subject starts with "owner: ", which is how the history names them as the
// editor and how "undo last edit" finds their last change. Nothing is deleted:
// a retired fact keeps its file, and a scope change moves the file in one
// commit.

// ownerCommit starts the subject of every commit the owner makes in the app.
const ownerCommit = "owner: "

// movedTrailer, in a commit body, names the path a fact had before a scope
// change moved it, so undo finds it without trusting git's rename detection.
const movedTrailer = "Tacit-Moved-From: "

// factSnap is every fact in the clone at one HEAD: the browse list, the
// links between facts and the facets.
type factSnap struct {
	head     string
	all      []*fact.Fact
	curated  map[string]*fact.Fact   // name -> curated fact
	byPath   map[string]*fact.Fact   // path -> fact, inbox notes included
	inbox    map[string][]*fact.Fact // name -> inbox notes, oldest first
	back     map[string][]string     // name -> curated facts that link to it
	unparsed int
	// held counts the curated files of each name: more than one, and a
	// fact's actions name its file (factDetail.Ref).
	held map[string]int
}

// snapshot reads the clone, or returns the read of the same HEAD. A write
// from the app resets it (snapReset); a sync that moved HEAD makes a new one.
func (rs *reviewServer) snapshot(ctx context.Context) (*factSnap, error) {
	head, _ := rs.s.Head(ctx)
	rs.snapMu.Lock()
	defer rs.snapMu.Unlock()
	if rs.snap != nil && head != "" && rs.snap.head == head {
		return rs.snap, nil
	}
	all, perrs, err := fact.Walk(rs.s.Dir)
	if err != nil {
		return nil, err
	}
	sn := &factSnap{head: head, all: all, curated: map[string]*fact.Fact{}, byPath: map[string]*fact.Fact{}, inbox: map[string][]*fact.Fact{}, back: map[string][]string{}, unparsed: len(perrs), held: map[string]int{}}
	for _, f := range all {
		sn.byPath[f.Path] = f
		if f.InInbox() {
			sn.inbox[f.Meta.Name] = append(sn.inbox[f.Meta.Name], f)
			continue
		}
		putByName(sn.curated, f)
		sn.held[f.Meta.Name]++
		for _, l := range f.Links() {
			sn.back[l] = append(sn.back[l], f.Meta.Name)
		}
	}
	for _, xs := range sn.inbox {
		sort.SliceStable(xs, func(i, j int) bool { return xs[i].Meta.Created < xs[j].Meta.Created })
	}
	for k := range sn.back {
		sort.Strings(sn.back[k])
	}
	rs.snap = sn
	return sn, nil
}

func (rs *reviewServer) snapReset() {
	rs.snapMu.Lock()
	rs.snap = nil
	rs.snapMu.Unlock()
}

// lookup finds a fact by store path (an inbox note is named by its path, since
// two writers' notes may share a name) or by name (a curated fact).
func (sn *factSnap) lookup(ref string) *fact.Fact {
	if strings.HasSuffix(ref, ".md") {
		return sn.byPath[ref]
	}
	if f := sn.curated[ref]; f != nil {
		return f
	}
	if xs := sn.inbox[ref]; len(xs) > 0 {
		return xs[0]
	}
	return nil
}

func isGenerated(f *fact.Fact) bool {
	return strings.HasPrefix(f.Meta.Scope, "derived/") && f.Meta.Source.Kind == "derived"
}

// repoOf lists the repos a fact is about: its project or derived scope and
// its applies_to.repos.
func reposOf(f *fact.Fact) []string {
	var out []string
	if root, rest, ok := strings.Cut(f.Meta.Scope, "/"); ok && (root == "projects" || root == "derived") {
		out = append(out, rest)
	}
	for _, r := range f.Meta.AppliesTo.Repos {
		if !contains(out, r) {
			out = append(out, r)
		}
	}
	return out
}

// --- browse and search ---

type browseQuery struct {
	Q       string `json:"q"`
	Scope   string `json:"scope"`
	Type    string `json:"type"`
	Repo    string `json:"repo"`
	Kind    string `json:"kind"`
	Rulings bool   `json:"rulings"`
	// Believed: what the agents filed as their preference that is not their
	// ruling (believed), the list beside Your rules.
	Believed bool   `json:"believed"`
	Status   string `json:"status"` // live (default), retired, replaced, all
	Where    string `json:"where"`  // curated (default), inbox, all
	Limit    int    `json:"limit"`
}

// factRow is one line of the list.
type factRow struct {
	Name         string `json:"name"`
	Path         string `json:"path"`
	Scope        string `json:"scope"`
	Type         string `json:"type"`
	Kind         string `json:"kind"`
	Description  string `json:"description"`
	Harness      string `json:"harness"`
	Host         string `json:"host"`
	Created      string `json:"created"`
	Updated      string `json:"updated,omitempty"`
	Retired      string `json:"retired,omitempty"`
	SupersededBy string `json:"superseded_by,omitempty"`
	Ruling       bool   `json:"ruling,omitempty"`
	Quote        string `json:"quote,omitempty"`    // a ruling's phrase in their words ("In your words")
	Endorsed     bool   `json:"endorsed,omitempty"` // a ruling because the owner said so (a decision), not a quote
	Words        string `json:"words,omitempty"`    // words of theirs in a fact that is not their rule (fact.OwnerWordsOf)
	Inbox        bool   `json:"inbox,omitempty"`
}

type browseResult struct {
	Rows     []factRow `json:"rows"`
	Matched  int       `json:"matched"` // rows that pass the filters; Rows holds at most Limit of them
	Total    int       `json:"total"`   // facts in the chosen place (curated, inbox or both) before filters
	Searched bool      `json:"searched"`
	Note     string    `json:"note,omitempty"`
}

func rowOf(f *fact.Fact) factRow {
	return factRow{Name: f.Meta.Name, Path: f.Path, Scope: f.Meta.Scope, Type: f.Meta.Type, Kind: f.Meta.Source.Kind, Description: f.Meta.Description,
		Harness: f.Meta.Source.Harness, Host: f.Meta.Source.Host, Created: f.Meta.Created, Updated: f.Meta.Updated, Retired: f.Meta.Retired,
		SupersededBy: f.Meta.SupersededBy, Ruling: isRuling(f), Quote: ownerQuoteOf(f), Endorsed: isRuling(f) && f.Meta.Source.Endorsed != "", Words: fact.OwnerWordsOf(f), Inbox: f.InInbox()}
}

func (q browseQuery) keep(f *fact.Fact) bool {
	switch q.Where {
	case "inbox":
		if !f.InInbox() {
			return false
		}
	case "all":
	default:
		if f.InInbox() {
			return false
		}
	}
	if q.Scope != "" && f.Meta.Scope != q.Scope {
		return false
	}
	if q.Type != "" && f.Meta.Type != q.Type {
		return false
	}
	if q.Kind != "" && f.Meta.Source.Kind != q.Kind {
		return false
	}
	if q.Repo != "" && !contains(reposOf(f), q.Repo) {
		return false
	}
	if q.Rulings && !isRuling(f) {
		return false
	}
	if q.Believed && !believed(f) {
		return false
	}
	switch q.Status {
	case "all":
	case "retired":
		return f.Meta.Retired != ""
	case "replaced":
		return f.Meta.SupersededBy != ""
	default:
		return f.Meta.Retired == "" && f.Meta.SupersededBy == ""
	}
	return true
}

// browse lists the facts that pass the filters. With a query, the full-text
// index ranks them (every term as a prefix), and a fact whose name holds the
// query as written comes first; without one, the newest first.
func (rs *reviewServer) browse(ctx context.Context, q browseQuery) (*browseResult, error) {
	sn, err := rs.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	if q.Limit <= 0 || q.Limit > 2000 {
		q.Limit = 400
	}
	res := &browseResult{}
	for _, f := range sn.all {
		place := q
		place.Scope, place.Type, place.Kind, place.Repo, place.Rulings, place.Believed, place.Status = "", "", "", "", false, false, "all"
		if place.keep(f) {
			res.Total++
		}
	}
	text := strings.TrimSpace(q.Q)
	var picked []*fact.Fact
	if text == "" {
		for _, f := range sn.all {
			if q.keep(f) {
				picked = append(picked, f)
			}
		}
		day := func(f *fact.Fact) string { return max(f.Meta.Updated, f.Meta.Created) }
		sort.SliceStable(picked, func(i, j int) bool {
			if a, b := day(picked[i]), day(picked[j]); a != b {
				return a > b
			}
			return picked[i].Meta.Name < picked[j].Meta.Name
		})
	} else {
		res.Searched = true
		low := strings.ToLower(text)
		seen := map[string]bool{}
		// A name that holds the query as typed (a slug, a part of one) first.
		var named []*fact.Fact
		for _, f := range sn.all {
			if strings.Contains(f.Meta.Name, low) && q.keep(f) {
				named = append(named, f)
			}
		}
		sort.SliceStable(named, func(i, j int) bool {
			ei, ej := named[i].Meta.Name == low, named[j].Meta.Name == low
			if ei != ej {
				return ei
			}
			return len(named[i].Meta.Name) < len(named[j].Meta.Name)
		})
		for _, f := range named {
			seen[f.Path] = true
			picked = append(picked, f)
		}
		ranked, rerr := rs.rank(ctx, sn, text, func(p string) bool { f := sn.byPath[p]; return f != nil && q.keep(f) })
		if rerr != nil {
			// The index could not be read: fall back to plain words.
			res.Note = "The search index could not be read (" + shortErr(rerr) + "); matching words as written."
			for _, f := range sn.all {
				if !seen[f.Path] && q.keep(f) && strings.Contains(strings.ToLower(f.Meta.Description+"\n"+f.Body), low) {
					picked = append(picked, f)
				}
			}
		}
		for _, r := range ranked {
			f := sn.byPath[r.Path]
			if f != nil && !seen[f.Path] && q.keep(f) {
				seen[f.Path] = true
				picked = append(picked, f)
			}
		}
	}
	res.Matched = len(picked)
	if len(picked) > q.Limit {
		picked = picked[:q.Limit]
	}
	res.Rows = make([]factRow, 0, len(picked))
	for _, f := range picked {
		res.Rows = append(res.Rows, rowOf(f))
	}
	return res, nil
}

// rank asks the full-text index of the clone at its HEAD (built when missing,
// like the hooks' own).
func (rs *reviewServer) rank(ctx context.Context, sn *factSnap, text string, keep func(string) bool) ([]search.Ranked, error) {
	if sn.head == "" {
		return nil, errors.New("the clone has no commit")
	}
	ix, err := search.Open(ctx, rs.s.Dir, sn.head)
	if err != nil {
		return nil, err
	}
	defer ix.Close()
	return ix.Rank(ctx, text, 2000, keep)
}

// appMeta is what the forms and filters offer.
type appMeta struct {
	Host     string   `json:"host"`
	Today    string   `json:"today"`
	Scopes   []string `json:"scopes"`
	Types    []string `json:"types"`
	Owned    []string `json:"owned_types"` // types a person may give a fact; the rest are generated
	Repos    []string `json:"repos"`
	Hosts    []string `json:"hosts"`
	Kinds    []string `json:"kinds"`
	Facts    int      `json:"facts"`
	Inbox    int      `json:"inbox"`
	Rulings  int      `json:"rulings"`
	Believed int      `json:"believed"` // live facts the agents believe the owner prefers, not their rulings
	Desktop  bool     `json:"desktop"`
	ViewOnly string   `json:"view_only,omitempty"` // the line the page shows when this machine cannot write
}

// ownedTypes are the types the owner may set in a form: the generated ones
// (hot spots, module maps, results) are written by the curator from git.
var ownedTypes = []string{"user_standard", "design_decision", "past_mistake", "machine_fact", "code_change", "history", "user", "reference"}

func (rs *reviewServer) meta(ctx context.Context) (*appMeta, error) {
	sn, err := rs.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	m := &appMeta{Host: store.ShortHost(), Today: time.Now().Format("2006-01-02"), Owned: ownedTypes, Desktop: desktopBuild, ViewOnly: rs.viewOnly}
	scopes, types, repos, hosts, kinds := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, f := range sn.all {
		if f.InInbox() {
			m.Inbox++
		} else {
			m.Facts++
			if isRuling(f) && f.Meta.Retired == "" && f.Meta.SupersededBy == "" {
				m.Rulings++
			}
			if believed(f) && f.Meta.Retired == "" && f.Meta.SupersededBy == "" {
				m.Believed++
			}
		}
		if fact.ValidScope(f.Meta.Scope) {
			scopes[f.Meta.Scope] = true
		}
		if f.Meta.Type != "" {
			types[f.Meta.Type] = true
		}
		if f.Meta.Source.Kind != "" {
			kinds[f.Meta.Source.Kind] = true
		}
		for _, r := range reposOf(f) {
			if fact.IsSlug(r) {
				repos[r] = true
			}
		}
		if root, rest, ok := strings.Cut(f.Meta.Scope, "/"); ok && root == "machines" && fact.IsSlug(rest) {
			hosts[rest] = true
		}
	}
	hosts[m.Host] = true
	keys := func(s map[string]bool) []string {
		out := make([]string, 0, len(s))
		for k := range s {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	m.Scopes, m.Types, m.Repos, m.Hosts, m.Kinds = keys(scopes), keys(types), keys(repos), keys(hosts), keys(kinds)
	return m, nil
}

// --- one fact ---

type appliesView struct {
	Paths    []string `json:"paths"`
	Symbols  []string `json:"symbols"`
	Repos    []string `json:"repos"`
	Hosts    []string `json:"hosts"`
	Commands []string `json:"commands"`
}

func appliesOf(a fact.AppliesTo) appliesView {
	n := func(xs []string) []string {
		if xs == nil {
			return []string{}
		}
		return xs
	}
	return appliesView{n(a.Paths), n(a.Symbols), n(a.Repos), n(a.Hosts), n(a.Commands)}
}

func (a appliesView) meta() fact.AppliesTo {
	clean := func(xs []string) []string {
		var out []string
		for _, x := range xs {
			if x = strings.TrimSpace(x); x != "" && !contains(out, x) {
				out = append(out, x)
			}
		}
		return out
	}
	return fact.AppliesTo{Paths: clean(a.Paths), Symbols: clean(a.Symbols), Repos: clean(a.Repos), Hosts: clean(a.Hosts), Commands: clean(a.Commands)}
}

// linkView is one [[link]] of a body and what it reaches.
type linkView struct {
	Name  string `json:"name"`
	State string `json:"state"` // live, retired, replaced, inbox, missing
}

type histEntry struct {
	Sha     string `json:"sha"`
	When    string `json:"when"`
	Who     string `json:"who"`
	Subject string `json:"subject"`
	Yours   bool   `json:"yours"`
}

type undoView struct {
	Can     bool   `json:"can"`
	Why     string `json:"why,omitempty"`
	Subject string `json:"subject,omitempty"`
	When    string `json:"when,omitempty"`
}

type factDetail struct {
	factView
	// Ref is what the page sends to act on this fact: its name, or its store
	// path when it is an inbox note or another curated file holds its name.
	Ref          string      `json:"ref"`
	Host         string      `json:"host"`
	Session      string      `json:"session,omitempty"`
	ImportedFrom string      `json:"imported_from,omitempty"`
	OriginalType string      `json:"original_type,omitempty"`
	DerivedFrom  string      `json:"derived_from,omitempty"`
	AppliesTo    appliesView `json:"applies_to"`
	Inbox        bool        `json:"inbox"`
	Ruling       bool        `json:"ruling"`
	Believed     bool        `json:"believed,omitempty"`     // filed by agents as their preference, not their ruling
	CanNotMine   bool        `json:"can_not_mine,omitempty"` // Not mine is offered: one agents believe the owner prefers
	NotMine      bool        `json:"not_mine,omitempty"`     // their standing Not mine on it: Undo Not mine is offered
	// RecordedWords: words of theirs the ledger records for it, shown also while
	// their Not mine keeps them from guarding it.
	RecordedWords string      `json:"recorded_words,omitempty"`
	Endorsed      bool        `json:"endorsed,omitempty"` // their rule on their decision ("This is mine", a kept pair)
	CanMine       bool        `json:"can_mine,omitempty"` // This is mine is offered
	Generated     bool        `json:"generated"`
	Rev           string      `json:"rev"` // the file as read, for "it changed since you opened it"
	Links         []linkView  `json:"links"`
	Backlinks     []string    `json:"backlinks"`
	OriginState   string      `json:"origin_state,omitempty"` // live (an inbox note), curated, missing
	History       []histEntry `json:"history"`
	HistoryNote   string      `json:"history_note,omitempty"`
	CanEdit       bool        `json:"can_edit"`
	CanRetire     bool        `json:"can_retire"`
	CanRestore    bool        `json:"can_restore"`
	CanReplace    bool        `json:"can_replace"`   // Replace with a new rule is offered: a live fact the owner may change
	Why           string      `json:"why,omitempty"` // why it cannot be edited or retired here
	Undo          undoView    `json:"undo"`
}

func revOf(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}

func detailView(f *fact.Fact) factView {
	return factView{Name: f.Meta.Name, Path: f.Path, Scope: f.Meta.Scope, Type: f.Meta.Type, Kind: f.Meta.Source.Kind,
		Description: f.Meta.Description, Body: strings.TrimSpace(f.Body), Created: f.Meta.Created, Updated: f.Meta.Updated,
		Harness: f.Meta.Source.Harness, Origin: f.Meta.Origin, SupersededBy: f.Meta.SupersededBy, ConflictsWith: f.Meta.ConflictsWith, Retired: f.Meta.Retired,
		Ruling: isRuling(f), OwnerQuote: ownerQuoteOf(f), OwnerWords: fact.OwnerWordsOf(f)}
}

func (rs *reviewServer) factDetail(ctx context.Context, ref string) (*factDetail, error) {
	sn, err := rs.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	f := sn.lookup(ref)
	if f == nil {
		return nil, fmt.Errorf("no fact named %s", ref)
	}
	data, err := os.ReadFile(filepath.Join(rs.s.Dir, filepath.FromSlash(f.Path)))
	if err != nil {
		return nil, err
	}
	d := &factDetail{factView: detailView(f), Host: f.Meta.Source.Host, Session: f.Meta.Source.Session, ImportedFrom: f.Meta.Source.ImportedFrom,
		OriginalType: f.Meta.Source.OriginalType, DerivedFrom: f.Meta.DerivedFrom, AppliesTo: appliesOf(f.Meta.AppliesTo), Inbox: f.InInbox(),
		Ruling: isRuling(f), Generated: isGenerated(f), Rev: revOf(data), Links: []linkView{}, Backlinks: []string{},
		Believed: believed(f), Endorsed: isRuling(f) && f.Meta.Source.Endorsed != "",
		CanMine:    !f.InInbox() && !isRuling(f) && !isGenerated(f) && f.Meta.Retired == "" && f.Meta.SupersededBy == "",
		CanNotMine: believed(f) && f.Meta.Retired == "" && f.Meta.SupersededBy == "",
		NotMine:    f.NotMine && !isRuling(f) && !f.InInbox()}
	d.Ref = f.Meta.Name
	if f.InInbox() || sn.held[f.Meta.Name] > 1 {
		d.Ref = f.Path
	}
	if f.WordsRecorded && !isRuling(f) {
		d.RecordedWords = f.Meta.Source.OwnerWords
	}
	if d.ConflictsWith == nil {
		d.ConflictsWith = []string{}
	}
	for _, l := range f.Links() {
		st := "missing"
		if c := sn.curated[l]; c != nil {
			switch {
			case c.Meta.SupersededBy != "":
				st = "replaced"
			case c.Meta.Retired != "":
				st = "retired"
			default:
				st = "live"
			}
		} else if len(sn.inbox[l]) > 0 {
			st = "inbox"
		}
		d.Links = append(d.Links, linkView{Name: l, State: st})
	}
	if !f.InInbox() {
		d.Backlinks = append(d.Backlinks, sn.back[f.Meta.Name]...)
	}
	if o := f.Meta.Origin; o != "" {
		switch {
		case len(sn.inbox[o]) > 0:
			d.OriginState = "inbox"
		case sn.curated[o] != nil:
			d.OriginState = "curated"
		default:
			d.OriginState = "missing"
		}
	}
	switch {
	case f.InInbox():
		d.Why = "An agent wrote this note and the curator has not reviewed it yet. Notes are read-only here: make it your rule to keep it in your own words."
	case d.Generated:
		d.Why = "Generated from git by the curator; its next run rewrites it, so it is not edited or retired here."
	default:
		d.CanEdit = true
		d.CanRetire = f.Meta.Retired == "" && f.Meta.SupersededBy == ""
		d.CanRestore = f.Meta.Retired != ""
		d.CanReplace = d.CanRetire
		if f.Meta.SupersededBy != "" && f.Meta.Retired == "" {
			d.Why = "Replaced by " + f.Meta.SupersededBy + ", so it already does not reach agents. Change that answer in Needs you."
		}
	}
	d.History, d.HistoryNote = rs.history(ctx, f.Path)
	d.Undo = undoOf(d)
	if rs.viewOnly != "" {
		// Nothing is offered: no edit, retire, restore, undo or rule.
		d.CanEdit, d.CanRetire, d.CanRestore, d.CanReplace, d.CanMine, d.CanNotMine = false, false, false, false, false, false
		d.Undo = undoView{Why: rs.viewOnly}
		d.Why = rs.viewOnly
	}
	return d, nil
}

// history is the store's own git log of one fact file: who wrote each change
// and when, newest first, following a move.
func (rs *reviewServer) history(ctx context.Context, rel string) ([]histEntry, string) {
	hctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := store.Git(hctx, rs.s.Dir, "log", "--follow", "-n", "40", "--format=%H%x1f%an%x1f%aI%x1f%s", "--", rel)
	if err != nil {
		return []histEntry{}, "The store's history could not be read: " + shortErr(err)
	}
	hs := []histEntry{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		p := strings.Split(l, "\x1f")
		if len(p) != 4 {
			continue
		}
		yours := strings.HasPrefix(p[3], ownerCommit) || (strings.HasPrefix(p[3], "review: ") && !strings.HasSuffix(p[3], " by agent"))
		who := p[1]
		if yours {
			who = "you"
		}
		when := p[2]
		if t, err := time.Parse(time.RFC3339, p[2]); err == nil {
			when = t.Local().Format("2006-01-02 15:04")
		}
		hs = append(hs, histEntry{Sha: p[0][:12], When: when, Who: who, Subject: p[3], Yours: yours})
	}
	if len(hs) == 0 {
		return hs, "Not committed yet."
	}
	return hs, ""
}

func undoOf(d *factDetail) undoView {
	if !d.CanEdit {
		return undoView{Why: "Nothing here is yours to undo."}
	}
	if len(d.History) == 0 {
		return undoView{Why: "This fact has no history in the store yet."}
	}
	h := d.History[0]
	if !strings.HasPrefix(h.Subject, ownerCommit) {
		return undoView{Why: fmt.Sprintf("The last change (%s, %s) is not one you made here.", h.Who, h.When)}
	}
	// The rule's own first commit; the fact a new rule replaced can be put back.
	if strings.HasPrefix(h.Subject, ownerCommit+"new ruling "+d.Name+" ") {
		return undoView{Why: "You wrote this rule here; retire it to stop it reaching agents."}
	}
	return undoView{Can: true, Subject: strings.TrimPrefix(h.Subject, ownerCommit), When: h.When}
}

// --- the write path ---

// ownerChange is one write the owner asked for, planned on the fresh clone.
type ownerChange struct {
	write  map[string][]byte // store-relative path -> new bytes
	remove []string          // tracked paths taken out (the old path of a move)
	msg    string
	name   string // the fact's name after the change
	note   string // set when there is nothing to write: why
}

// saveResult is what the page shows after a write.
type saveResult struct {
	Saved bool     `json:"saved"`
	Name  string   `json:"name,omitempty"`
	Note  string   `json:"note,omitempty"`
	Lint  []string `json:"lint,omitempty"` // findings that refused the write; the draft stays
	// Changed: the edit form was opened on a version of the fact that is no
	// longer the one in the store (another machine or tool changed it). Nothing
	// was saved; the page shows the new version beside the draft.
	Changed bool `json:"changed,omitempty"`
	tries   int  // how many times the locked part ran (a lost push race runs it again)
}

// lintKey names a finding by the fact's file name rather than its path, so a
// scope change that moves a fact does not make its old findings look new.
func lintKey(f lint.Finding) string {
	return f.Rule + "\x00" + strings.TrimSuffix(filepath.Base(f.Path), ".md") + "\x00" + f.Msg
}

// newFindings lists what after has that before did not. L10 is about
// MEMORY.md, which the write regenerates.
func newFindings(before, after []lint.Finding) []string {
	had := map[string]bool{}
	for _, f := range before {
		had[lintKey(f)] = true
	}
	var out []string
	for _, f := range after {
		if f.Rule != "L10" && !indexFinding(f) && !had[lintKey(f)] {
			out = append(out, f.String())
		}
	}
	return out
}

// ownerWrite runs plan on a freshly synced clone under the store lock, writes
// what it returns, regenerates MEMORY.md, lints, and pushes once as curator.
// The server moving during the push starts the locked part over (the plan is a
// function of the store, so it is computed again). A lint finding the store did
// not have, a refusal or a failure puts every file back as it was.
func (rs *reviewServer) ownerWrite(ctx context.Context, plan func() (*ownerChange, error)) (*saveResult, error) {
	if err := rs.writeRefusal(ctx); err != nil {
		return &saveResult{Note: viewOnlyLine}, err
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.freshNotes = nil
	res := &saveResult{}
	err := rs.s.WithLock(ctx, func() error {
		for attempt := 0; attempt < 4; attempt++ {
			res.tries = attempt + 1
			var syncNote string
			if err := rs.s.SyncLocked(ctx); err != nil {
				var st *store.Stranded
				if !errors.As(err, &st) {
					return fmt.Errorf("could not sync the store, nothing was saved (%s)", shortErr(err))
				}
				syncNote = st.Error()
			}
			rs.snapReset()
			rs.loadFacts()
			ch, err := plan()
			if err != nil {
				return err
			}
			if len(ch.write) == 0 && len(ch.remove) == 0 {
				res.Name, res.Note = ch.name, strings.TrimSpace(syncNote+" "+ch.note)
				return nil
			}
			head, _ := rs.s.Head(ctx)
			before, err := lint.Run(rs.s.Dir)
			if err != nil {
				return fmt.Errorf("tacit's checks could not run, nothing was saved (%s)", shortErr(err))
			}
			rels, err := rs.applyChange(ch)
			putBack := func() {
				rs.s.Restore(rels)
				rs.dropFreshNotes() // Restore keeps new inbox files; their note goes with the write
				search.Invalidate(head)
			}
			if err != nil {
				putBack()
				return fmt.Errorf("could not write the change, nothing was saved (%s)", shortErr(err))
			}
			after, err := lint.Run(rs.s.Dir)
			if err != nil {
				putBack()
				return fmt.Errorf("tacit's checks could not run, nothing was saved (%s)", shortErr(err))
			}
			if fresh := newFindings(before, after); len(fresh) > 0 {
				putBack()
				res.Lint = fresh
				res.Note = "Not saved: tacit's checks refuse this change. Your draft is kept."
				return nil
			}
			if attempt == 0 && rs.beforePush != nil {
				rs.beforePush() // tests: another writer lands first
			}
			// The commit names its files: whatever else is staged in the clone
			// (a killed run, a hand `git add`) stays out of an owner commit.
			// .gitattributes is named because the push path may create it.
			// The owner's own text is theirs: the mask every other write goes
			// through leaves it as they wrote it (SPEC.md, "App").
			err = rs.s.CommitPushNamedOnceLocked(store.WithOwnerText(ctx), append(rels, ".gitattributes"), ch.msg, "curator")
			var rej *store.Rejected
			switch {
			case err == nil:
				res.Saved, res.Name, res.Note = true, ch.name, strings.TrimSpace(syncNote+" "+ch.note)
				return nil
			case errors.Is(err, store.ErrBehind):
				search.Invalidate(head)
				continue
			case errors.As(err, &rej):
				search.Invalidate(head)
				return fmt.Errorf("the server refused this change: %s. Your draft is kept", rej.Reason)
			}
			if now, _ := rs.s.Head(ctx); now == head {
				// No commit was made: put the files back.
				putBack()
				return fmt.Errorf("could not commit, nothing was saved (%s)", shortErr(err))
			}
			// The commit stays here and goes up with the next push.
			res.Saved, res.Name = true, ch.name
			res.Note = "Saved on this machine, not on the server yet: " + shortErr(err)
			return nil
		}
		rs.dropFreshNotes()
		return errors.New("the store kept changing on the server; nothing was saved, try again")
	})
	rs.snapReset()
	rs.loadFacts()
	return res, err
}

// applyChange writes and removes the planned files and regenerates MEMORY.md
// and the folder indexes.
// It returns every path it touched, for the commit or for putting them back.
func (rs *reviewServer) applyChange(ch *ownerChange) ([]string, error) {
	var rels []string
	paths := make([]string, 0, len(ch.write))
	for p := range ch.write {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, rel := range paths {
		if !safeRel(rel) && !ownerRecordRel(rel) {
			return rels, fmt.Errorf("refusing path %s", rel)
		}
		rels = append(rels, rel)
		full := filepath.Join(rs.s.Dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return rels, err
		}
		if err := writeFileAtomic(rs.s.Dir, rel, ch.write[rel]); err != nil {
			return rels, err
		}
	}
	for _, rel := range ch.remove {
		if !safeRel(rel) {
			return rels, fmt.Errorf("refusing path %s", rel)
		}
		rels = append(rels, rel)
		if err := os.Remove(filepath.Join(rs.s.Dir, filepath.FromSlash(rel))); err != nil && !errors.Is(err, os.ErrNotExist) {
			return rels, err
		}
	}
	all, _, err := fact.Walk(rs.s.Dir)
	if err != nil {
		return rels, err
	}
	ix, err := regenIndexes(context.Background(), rs.s.Dir, all, nil)
	rels = append(rels, ix...)
	return rels, err
}

// safeRel accepts a store-relative fact path the owner's writes may touch.
// ownerRecordRel: besides curated facts, an owner write may append to the
// decision log and add their own inbox note (inbox/human@<this host>/<name>.md).
func ownerRecordRel(rel string) bool {
	if rel == "review/decisions.jsonl" || rel == ledger.MarkerPath {
		return true
	}
	dir, file := filepath.ToSlash(filepath.Dir(rel)), filepath.Base(rel)
	return dir == "inbox/human@"+store.ShortHost() && strings.HasSuffix(file, ".md") && fact.IsSlug(strings.TrimSuffix(file, ".md"))
}

func safeRel(rel string) bool {
	if rel == "" || strings.HasPrefix(rel, "/") || strings.Contains(rel, "..") || strings.HasPrefix(rel, ".git") || !strings.HasSuffix(rel, ".md") {
		return false
	}
	root, _, _ := strings.Cut(rel, "/")
	return fact.ScopeRoots[root] && root != "derived"
}

// ownerFact is a curated fact the owner may change, with its bytes, read from
// the clone under the lock.
func (rs *reviewServer) ownerFact(ctx context.Context, ref string) (*fact.Fact, []byte, error) {
	sn, err := rs.snapshot(ctx)
	if err != nil {
		return nil, nil, err
	}
	f := sn.lookup(ref)
	switch {
	case f == nil:
		return nil, nil, fmt.Errorf("no fact named %s", ref)
	case f.InInbox():
		return nil, nil, errors.New("an inbox note is read-only; make it your rule to keep it")
	case isGenerated(f):
		return nil, nil, errors.New("this fact is generated from git and the curator's next run rewrites it, so it is not changed here")
	}
	if out, _ := store.Git(ctx, rs.s.Dir, "status", "--porcelain", "--", f.Path); strings.TrimSpace(out) != "" {
		return nil, nil, fmt.Errorf("%s has changes on this machine that are not committed; nothing was saved", f.Path)
	}
	data, err := os.ReadFile(filepath.Join(rs.s.Dir, filepath.FromSlash(f.Path)))
	if err != nil {
		return nil, nil, err
	}
	return f, data, nil
}

func today() string { return time.Now().Format("2006-01-02") }

// checkScope says what is wrong with a scope the owner chose.
func checkScope(scope string) error {
	switch {
	case !fact.ValidScope(scope):
		return fmt.Errorf("%q is not a scope: use projects/<repo>, machines/<host>, user, system, areas/<area>, areas/<area>/topics/<topic> or topics/<topic>", scope)
	case strings.HasPrefix(scope, "derived/"):
		return errors.New("derived/ holds what the curator generates from git; choose another scope")
	}
	return nil
}

func cleanLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func cleanBody(s string) string { return strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n")) }

// factDraft is the edit form.
type factDraft struct {
	Rev         string      `json:"rev"`
	Description string      `json:"description"`
	Body        string      `json:"body"`
	Scope       string      `json:"scope"`
	Type        string      `json:"type"`
	AppliesTo   appliesView `json:"applies_to"`
}

func checkText(desc, body string) error {
	switch {
	case desc == "":
		return errors.New("the description is empty; it is the line agents see")
	case len([]rune(desc)) > 200:
		return fmt.Errorf("the description is %d characters; the most is 200", len([]rune(desc)))
	case body == "":
		return errors.New("the body is empty")
	}
	return nil
}

// errStaleForm: the form's rev is not the file's.
var errStaleForm = errors.New("this fact changed since you opened it; nothing was saved and your draft is kept beside the new version")

func (rs *reviewServer) editFact(ctx context.Context, ref string, d factDraft) (*saveResult, error) {
	res, err := rs.editFactWrite(ctx, ref, d)
	if errors.Is(err, errStaleForm) {
		return &saveResult{Changed: true, Name: ref, Note: errStaleForm.Error()}, nil
	}
	return res, err
}

func (rs *reviewServer) editFactWrite(ctx context.Context, ref string, d factDraft) (*saveResult, error) {
	return rs.ownerWrite(ctx, func() (*ownerChange, error) {
		f, data, err := rs.ownerFact(ctx, ref)
		if err != nil {
			return nil, err
		}
		if d.Rev == "" || d.Rev != revOf(data) {
			return nil, errStaleForm
		}
		desc, body, scope, typ := cleanLine(d.Description), cleanBody(d.Body), strings.TrimSpace(d.Scope), strings.TrimSpace(d.Type)
		if err := checkText(desc, body); err != nil {
			return nil, err
		}
		if err := checkScope(scope); err != nil {
			return nil, err
		}
		if !fact.Types[typ] || typ == "unclassified" || (typ != f.Meta.Type && !contains(ownedTypes, typ)) {
			return nil, fmt.Errorf("%q is not a type you can give a fact", typ)
		}
		nf := &fact.Fact{Path: scope + "/" + f.Meta.Name + ".md", Meta: f.Meta, Body: body + "\n"}
		nf.Meta.Description, nf.Meta.Scope, nf.Meta.Type = desc, scope, typ
		nf.Meta.AppliesTo = d.AppliesTo.meta()
		var changed []string
		if desc != f.Meta.Description {
			changed = append(changed, "description")
		}
		if body != strings.TrimSpace(f.Body) {
			changed = append(changed, "body")
		}
		if scope != f.Meta.Scope {
			changed = append(changed, "scope")
		}
		if typ != f.Meta.Type {
			changed = append(changed, "type")
		}
		if fmt.Sprint(appliesOf(nf.Meta.AppliesTo)) != fmt.Sprint(appliesOf(f.Meta.AppliesTo)) {
			changed = append(changed, "applies_to")
		}
		ch := &ownerChange{name: f.Meta.Name}
		if len(changed) == 0 {
			ch.note = "Nothing changed."
			return ch, nil
		}
		nf.Meta.Updated = today()
		files := map[string][]byte{}
		rs.keepOwnWords(files, f, nf)
		b, err := nf.Encode()
		if err != nil {
			return nil, err
		}
		files[nf.Path] = b
		ch.write = files
		ch.msg = fmt.Sprintf("%sedit %s (%s)\n\nEdited by the owner in tacit app.", ownerCommit, f.Meta.Name, strings.Join(changed, ", "))
		if nf.Path != f.Path {
			if _, err := os.Stat(filepath.Join(rs.s.Dir, filepath.FromSlash(nf.Path))); err == nil {
				return nil, fmt.Errorf("%s already exists; nothing was saved", nf.Path)
			}
			ch.remove = []string{f.Path}
			ch.msg += "\n" + movedTrailer + f.Path
		}
		return ch, nil
	})
}

// setRetired retires a fact (retired: today) or restores it (the field
// cleared). Only those two lines and updated change.
func (rs *reviewServer) setRetired(ctx context.Context, ref string, retire bool) (*saveResult, error) {
	return rs.ownerWrite(ctx, func() (*ownerChange, error) {
		f, data, err := rs.ownerFact(ctx, ref)
		if err != nil {
			return nil, err
		}
		ch := &ownerChange{name: f.Meta.Name}
		c := *f
		verb := "retire"
		if retire {
			switch {
			case f.Meta.Retired != "":
				ch.note = "Already retired on " + f.Meta.Retired + "."
				return ch, nil
			case f.Meta.SupersededBy != "":
				ch.note = "Already replaced by " + f.Meta.SupersededBy + "; it does not reach agents."
				return ch, nil
			}
			c.Meta.Retired = today()
		} else {
			if f.Meta.Retired == "" {
				ch.note = "This fact is not retired."
				return ch, nil
			}
			c.Meta.Retired, verb = "", "restore"
		}
		c.Meta.Updated = today()
		nb, err := fact.PatchRelations(data, &c)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.Path, err)
		}
		ch.write = map[string][]byte{f.Path: nb}
		ch.msg = fmt.Sprintf("%s%s %s\n\nBy the owner in tacit app.", ownerCommit, verb, f.Meta.Name)
		return ch, nil
	})
}

// undoLast puts a fact back to what it was before the owner's last change to
// it in the app, when that is the newest commit on its file. The undo is a new
// commit, so a second undo redoes.
func (rs *reviewServer) undoLast(ctx context.Context, ref string) (*saveResult, error) {
	return rs.ownerWrite(ctx, func() (*ownerChange, error) {
		f, _, err := rs.ownerFact(ctx, ref)
		if err != nil {
			return nil, err
		}
		out, err := store.Git(ctx, rs.s.Dir, "log", "-1", "--follow", "--format=%H%x1f%an%x1f%s%x1f%B", "--", f.Path)
		if err != nil {
			return nil, fmt.Errorf("the store's history could not be read (%s)", shortErr(err))
		}
		p := strings.SplitN(strings.TrimSpace(out), "\x1f", 4)
		if len(p) != 4 {
			return nil, errors.New("this fact has no history in the store yet")
		}
		sha, who, subj, msg := p[0], p[1], p[2], p[3]
		if !strings.HasPrefix(subj, ownerCommit) {
			return nil, fmt.Errorf("the last change to this fact (%s: %s) is not one you made here, so there is nothing of yours to undo", who, subj)
		}
		old := f.Path
		for _, l := range strings.Split(msg, "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(l), movedTrailer); ok && safeRel(strings.TrimSpace(v)) {
				old = strings.TrimSpace(v)
			}
		}
		prev, err := store.Git(ctx, rs.s.Dir, "show", sha+"^:"+old)
		if err != nil {
			return nil, errors.New("you wrote this fact here; retire it to stop it reaching agents")
		}
		ch := &ownerChange{name: f.Meta.Name, write: map[string][]byte{old: []byte(prev)}}
		if old != f.Path {
			if _, err := os.Stat(filepath.Join(rs.s.Dir, filepath.FromSlash(old))); err == nil {
				return nil, fmt.Errorf("%s exists again; nothing was undone", old)
			}
			ch.remove = []string{f.Path}
			ch.msg = "\n" + movedTrailer + f.Path
		}
		ch.msg = ownerCommit + "undo " + strings.TrimPrefix(subj, ownerCommit) + "\n\nUndone by the owner in tacit app (was " + sha[:12] + ")." + ch.msg
		// Undoing This is mine (or an edit that rested their rule on it) reopens
		// that decision, so the audit does not make the fact theirs again; undoing
		// the undo records it again.
		if pf, perr := fact.Parse(old, []byte(prev)); perr == nil {
			id := mineID(f.Meta.Name)
			switch now, was := f.Meta.Source.Endorsed == id, pf.Meta.Source.Endorsed == id; {
			case now && !was:
				rs.appendDecision(ch.write, decision{ID: id, Action: "reopen", A: f.Meta.Name, At: time.Now().Format(time.RFC3339)})
			case was && !now:
				rs.appendDecision(ch.write, decision{ID: id, Action: actionMine, A: f.Meta.Name, At: time.Now().Format(time.RFC3339)})
			}
		}
		return ch, nil
	})
}

// rulingDraft is the new-ruling form.
type rulingDraft struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Body        string `json:"body"`
	Scope       string `json:"scope"`
	Origin      string `json:"origin"` // the inbox note it was promoted from, if any
	// Replaces: a live fact the new rule takes the place of. It gets
	// superseded_by the new rule in the same commit, so it stops reaching
	// agents and stays in history; Undo on it puts it back.
	Replaces string `json:"replaces,omitempty"`
}

// firstSentence is the description a ruling gets when the form left it empty.
func firstSentence(body string) string {
	line := cleanLine(body)
	for i, r := range line {
		if (r == '.' || r == '!' || r == '?') && (i+1 == len(line) || line[i+1] == ' ') {
			return line[:i+1]
		}
	}
	return line
}

// newRuling writes the owner's own words as a curated fact: source.kind user,
// type user_standard, the scope the owner chose. With Replaces, the fact it
// names is replaced by the new rule in the same commit.
func (rs *reviewServer) newRuling(ctx context.Context, d rulingDraft) (*saveResult, error) {
	return rs.ownerWrite(ctx, func() (*ownerChange, error) {
		sn, err := rs.snapshot(ctx)
		if err != nil {
			return nil, err
		}
		name, scope, body := strings.TrimSpace(d.Name), strings.TrimSpace(d.Scope), cleanBody(d.Body)
		desc := cleanLine(d.Description)
		if desc == "" {
			desc = fact.ClipDescription(firstSentence(body), 200)
		}
		if err := checkText(desc, body); err != nil {
			return nil, err
		}
		if !fact.IsSlug(name) {
			return nil, fmt.Errorf("%q is not a name: lower case letters, digits and hyphens", name)
		}
		if err := checkScope(scope); err != nil {
			return nil, err
		}
		if c := sn.curated[name]; c != nil {
			return nil, fmt.Errorf("a fact named %s already exists (%s); choose another name", name, c.Path)
		}
		if xs := sn.inbox[name]; len(xs) > 0 && name != d.Origin {
			return nil, fmt.Errorf("an inbox note is named %s (%s); choose another name", name, xs[0].Path)
		}
		m := fact.Meta{Name: name, Description: desc, Type: "user_standard", Scope: scope,
			Source: fact.Source{Harness: "human", Host: store.ShortHost(), Kind: "user"}, Created: today()}
		if root, repo, ok := strings.Cut(scope, "/"); ok && root == "projects" {
			m.AppliesTo.Repos = []string{repo}
		}
		if d.Origin != "" && len(sn.inbox[d.Origin]) > 0 {
			m.Origin = d.Origin
		}
		nf := &fact.Fact{Path: scope + "/" + name + ".md", Meta: m, Body: body + "\n"}
		if _, err := os.Stat(filepath.Join(rs.s.Dir, filepath.FromSlash(nf.Path))); err == nil {
			return nil, fmt.Errorf("%s already exists; choose another name", nf.Path)
		}
		b, err := nf.Encode()
		if err != nil {
			return nil, err
		}
		write := map[string][]byte{nf.Path: b}
		msg := fmt.Sprintf("%snew ruling %s (%s)", ownerCommit, name, scope)
		if old := strings.TrimSpace(d.Replaces); old != "" {
			f, data, err := rs.ownerFact(ctx, old)
			if err != nil {
				return nil, err
			}
			switch {
			case f.Meta.Retired != "":
				return nil, fmt.Errorf("%s was retired on %s; restore it first, or write the rule without replacing it", f.Meta.Name, f.Meta.Retired)
			case f.Meta.SupersededBy != "":
				return nil, fmt.Errorf("%s is already replaced by %s", f.Meta.Name, f.Meta.SupersededBy)
			}
			c := *f
			c.Meta.SupersededBy, c.Meta.Updated = name, today()
			ob, err := fact.PatchRelations(data, &c)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", f.Path, err)
			}
			write[f.Path] = ob
			msg += ", replacing " + f.Meta.Name
		}
		msg += "\n\nWritten by the owner in tacit app."
		if m.Origin != "" {
			msg += " Promoted from the inbox note " + m.Origin + "."
		}
		return &ownerChange{name: name, write: write, msg: msg}, nil
	})
}

// --- dead links in rulings (LINK? lines) ---

const kindLink = "link"

type linkFlag struct {
	Target string `json:"target"`
	Why    string `json:"why"`
	Would  string `json:"would"` // what the curator would write: plain text, or [[fact]]
	Line   string `json:"line"`
}

// linkItem is a LINK? line as the queue shows it: one ruling, one dead link.
type linkItem struct {
	ID         string      `json:"id"`
	Kind       string      `json:"kind"`
	Source     string      `json:"source"`
	A          *factView   `json:"a"`
	Single     bool        `json:"single"`
	Link       linkFlag    `json:"link"`
	State      string      `json:"state"` // open, settled (the ruling no longer has the dead link), gone
	Allowed    []string    `json:"allowed"`
	NeedsOwner bool        `json:"needs_owner"`
	Notes      []ownerNote `json:"notes,omitempty"` // the owner's notes on it
	edit       links.Edit
}

// linkItems reads the LINK? lines of review/*.md, one item per ruling and
// target, and asks the link planner whether the dead link is still there.
// The caller holds rs.mu (the items read rs.facts).
func (rs *reviewServer) linkItems(ctx context.Context) []linkItem {
	files, _ := filepath.Glob(filepath.Join(rs.s.Dir, "review", "*.md"))
	sort.Strings(files)
	type ref struct{ ruling, target, src, line string }
	var refs []ref
	seen := map[string]bool{}
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			if m := links.LineRe.FindStringSubmatch(sc.Text()); m != nil && !seen[links.Key(m[1], m[2])] {
				seen[links.Key(m[1], m[2])] = true
				refs = append(refs, ref{m[1], m[2], filepath.Base(f), sc.Text()})
			}
		}
		fh.Close()
	}
	if len(refs) == 0 {
		return nil
	}
	open := map[string]links.Edit{}
	var planner *links.Planner
	if sn, err := rs.snapshot(ctx); err == nil {
		planner = links.NewPlanner(rs.s.Dir, sn.all, isRuling, nil)
		if plan, err := planner.Plan(); err == nil {
			for _, fx := range plan.Rulings {
				for _, e := range fx.Edits {
					open[links.Key(fx.Name, e.Target)] = e
				}
			}
		}
	}
	var out []linkItem
	for _, r := range refs {
		// A line an older curator raised for a link to what the ruling
		// replaced (the statements a rebuilt ruling was made of): the link
		// is where the rule came from, not a broken one, so it is no item.
		if f := rs.facts[r.ruling]; planner != nil && f != nil && planner.History(f, r.target) {
			continue
		}
		it := linkItem{ID: pairID(kindLink, r.ruling, r.target), Kind: kindLink, Source: r.src, A: rs.view(r.ruling), Single: true, Allowed: []string{}, Notes: rs.ownerNotes().For(pairID(kindLink, r.ruling, r.target), r.ruling)}
		it.Link = linkFlag{Target: r.target, Line: r.line}
		if _, rest, ok := strings.Cut(r.line, ", and "); ok {
			it.Link.Why, _, _ = strings.Cut(rest, "; this is an owner ruling")
		}
		e, isOpen := open[links.Key(r.ruling, r.target)]
		switch {
		case it.A.Missing || it.A.SupersededBy != "" || it.A.Retired != "":
			it.State = "gone"
		case isOpen:
			it.State, it.edit, it.NeedsOwner = "open", e, true
			it.Link.Why, it.Link.Would = e.Detail, "plain text"
			if e.To != "" {
				it.Link.Would = "[[" + e.To + "]]"
			}
			if rs.viewOnly == "" {
				it.Allowed = []string{"apply", "edit"}
			}
		default:
			it.State = "settled"
		}
		out = append(out, it)
	}
	return out
}

// applyLinkFix writes into the ruling what the curator would have written for
// one dead link, and nothing else.
func (rs *reviewServer) applyLinkFix(ctx context.Context, id string) (*saveResult, error) {
	return rs.ownerWrite(ctx, func() (*ownerChange, error) {
		var it *linkItem
		for _, x := range rs.linkItems(ctx) {
			if x.ID == id {
				xx := x
				it = &xx
			}
		}
		if it == nil {
			return nil, errors.New("this dead link is no longer in the review files")
		}
		ch := &ownerChange{name: it.A.Name}
		if it.State != "open" {
			ch.note = "The rule no longer has this broken link."
			return ch, nil
		}
		f, data, err := rs.ownerFact(ctx, it.A.Name)
		if err != nil {
			return nil, err
		}
		out, ok := links.Rewrite(data, map[string]links.Edit{it.Link.Target: it.edit})
		if !ok || string(out) == string(data) {
			return nil, errors.New("the link could not be rewritten; edit the rule instead")
		}
		ch.write = map[string][]byte{f.Path: out}
		ch.msg = fmt.Sprintf("%slink in %s: [[%s]] to %s\n\nApplied by the owner in tacit app.", ownerCommit, f.Meta.Name, it.Link.Target, it.Link.Would)
		return ch, nil
	})
}

// queue is everything waiting on the owner, for the app's Needs you: the
// pairs, the stale checks (with the agents' questions on them) and the dead
// links in their rulings, and among past answers the reviewer's questions an
// agent closed, which the owner may reopen (SPEC.md, "Closing questions").
// The rulings audit adds nothing here: what agents believe the owner prefers
// waits in its own place. The caller holds rs.mu.
func (rs *reviewServer) queue(ctx context.Context) []any {
	out := rs.allItems()
	for _, it := range rs.linkItems(ctx) {
		out = append(out, it)
	}
	for _, it := range rs.questionItems() {
		out = append(out, it)
	}
	if out == nil {
		out = []any{}
	}
	return out
}

// factTimeline is "What git says" for one fact, built and remembered like a
// proof's.
func (rs *reviewServer) factTimeline(ctx context.Context, ref string) (*gitev.Timeline, error) {
	if rs.tl == nil {
		return nil, errors.New("no timeline here")
	}
	sn, err := rs.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	f := sn.lookup(ref)
	if f == nil {
		return nil, fmt.Errorf("no fact named %s", ref)
	}
	c := *f
	r := gitev.FactRef{Fact: &c}
	if xs := sn.inbox[f.Meta.Origin]; f.Meta.Origin != "" && len(xs) > 0 {
		oc := *xs[0]
		r.Origin = &oc
	}
	rs.mu.Lock()
	return rs.flightLocked(ctx, "fact|"+f.Path+"|"+f.Meta.Updated+"|"+f.Meta.Retired, gitev.Input{A: r})
}
