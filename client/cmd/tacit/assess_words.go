package main

// The owner's words beat dates (SPEC.md, "Folder records", Owner words beat
// dates): the store's notes that name a folder, the owner's rulings,
// statements and decisions first, are gathered before the first ask; an
// owner_status the model cites from them is checked (internal/assess,
// words.go) and stands against the folder's dates until the owner's real
// work on it after the day of the words, which code classifies here: the
// sessions that worked in its checkout, and its commits, here and on the
// forge, by the files their diffs change (maintenance.go).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/assess"
	"github.com/agent-sh/tacit/client/internal/check"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/overlay"
	"github.com/agent-sh/tacit/client/internal/record"
	"github.com/agent-sh/tacit/client/internal/repoid"
)

// format3 reports a store whose marker is format 3 or newer: a record's
// successor and an entry's owner words may be written (with the local
// layer, into the overlay, whatever the store's format).
func (a *assessor) format3() bool {
	if a.local {
		return true
	}
	f, ok, err := ledger.ReadFormat(a.dir)
	return err == nil && ok && f.Successors()
}

// noteTier is a fact's tier among the notes that name a folder.
func noteTier(f *fact.Fact) int {
	switch {
	case fact.IsRuling(f) || overlay.IsRuling(f):
		return assess.TierRuling
	case fact.IsStatement(f):
		return assess.TierStatement
	case fact.OwnerWordsOf(f) != "":
		return assess.TierWords
	case f.Meta.Type == "design_decision":
		return assess.TierDecision
	}
	return assess.TierOther
}

// notes fills in c's Notes: the live facts in other folders that name it by
// any of its names, the owner's tiers first, then newest.
func (a *assessor) notes(c *assess.Candidate) {
	c.Notes, c.NotesTotal = nil, 0
	name, ok := strings.CutPrefix(c.Folder, "projects/")
	if !ok || c.Kind != "" {
		return
	}
	name = strings.ToLower(name)
	hits := a.factsNaming(c.Folder, a.namesOf(*c, name), name)
	var ns []assess.Note
	for _, f := range hits {
		n := assess.Note{Name: f.Meta.Name, Scope: f.Meta.Scope, Type: f.Meta.Type, Created: f.Meta.Created, Session: f.Meta.Source.Session,
			Tier: noteTier(f), Description: f.Meta.Description, Body: f.Body}
		if n.Tier == assess.TierStatement {
			if day, _, ok := fact.StatementLine(f.Body); ok {
				n.Said = day
			}
		}
		ns = append(ns, n)
	}
	assess.SortNotes(ns)
	c.NotesTotal = len(ns)
	if len(ns) > assess.NotesShown {
		ns = ns[:assess.NotesShown]
	}
	c.Notes = ns
	folder, act := c.Folder, c.Activity
	c.RealWork = func(after string) assess.Changes { return a.realWork(folder, act, after) }
}

// forgeClassified bounds the owner's forge commits after a day code asks the
// forge about (each one gh call, kept for good): the newest first, and it
// stops at the first that is real work.
const forgeClassified = 12

// realWork is the owner's activity on folder after the day after, as code
// classifies it: the sessions that worked in its checkout or folder here
// and on other machines (work), its commits under this machine's git
// identity here and the forge's by the owner's account (work when their
// diff changes more than maintenance files; a bot's is maintenance), and
// other machines' commit days that neither shows (unknown).
func (a *assessor) realWork(folder string, act *assess.Activity, after string) assess.Changes {
	key := folder + "@" + after
	if a.workMemo == nil {
		a.workMemo = map[string]assess.Changes{}
	}
	if ch, ok := a.workMemo[key]; ok {
		return ch
	}
	var ch assess.Changes
	name := strings.TrimPrefix(folder, "projects/")
	seen := map[string]bool{} // commit days and shas classified
	addCommit := func(day, sha string, files []string, bot string) {
		cite := "commit:" + shortSHA(sha)
		seen[strings.ToLower(sha)] = true
		w := assess.Work{Day: day, Kind: "commit", Cite: cite}
		switch real := assess.RealFiles(files); {
		case bot != "":
			w.Why = "a bot's commit (" + bot + ")"
			ch.Maintenance = append(ch.Maintenance, w)
		case len(files) == 0:
			w.Why = "changes no file"
			ch.Maintenance = append(ch.Maintenance, w)
		case len(real) == 0:
			w.Why = "only maintenance files: " + strings.Join(clipFiles(files, 4), ", ")
			ch.Maintenance = append(ch.Maintenance, w)
		default:
			w.Real, w.Why = true, "changes "+strings.Join(clipFiles(real, 4), ", ")
			ch.Real = append(ch.Real, w)
		}
		seen["day:"+day] = true
	}
	ctx, cancel := context.WithTimeout(a.ctx, 2*time.Minute)
	defer cancel()
	if r, ok := a.catalog.Repo(name); ok && r.Root != "" {
		for _, cm := range repoid.OwnCommitsAfter(ctx, r.Root, after, 50) {
			addCommit(cm.Day, cm.Hash, cm.Files, "")
		}
		for _, w := range a.catalog.Work(r.Root) {
			if w.Seen > after {
				ch.Real = append(ch.Real, assess.Work{Day: w.Seen, Kind: "session", Cite: "session:" + w.Session, Why: "worked in its checkout here", Real: true})
			}
		}
	} else if a.host != "" {
		for p, ws := range repoid.FolderWork(a.reg) {
			if filepath.Base(p) != name {
				continue
			}
			for _, w := range ws {
				if w.Seen > after {
					ch.Real = append(ch.Real, assess.Work{Day: w.Seen, Kind: "session", Cite: "session:" + w.Session, Why: "worked in its folder here", Real: true})
				}
			}
		}
	}
	if act != nil && act.Forge != nil && a.checker != nil && a.checker.Forge != nil {
		owner, repo, _ := strings.Cut(act.Forge.FullName, "/")
		n := 0
		for _, cm := range act.Forge.Commits {
			if cm.Day <= after || seenSHA(seen, cm.SHA) {
				continue
			}
			if assess.BotAuthor(cm.Login) {
				addCommit(cm.Day, cm.SHA, nil, cm.Login)
				continue
			}
			if act.User == "" || !strings.EqualFold(cm.Login, act.User) {
				continue // another's commit: not the owner's activity
			}
			if len(ch.Real) > 0 || n == forgeClassified {
				break
			}
			n++
			d, err := a.checker.Forge.Commit(ctx, owner, repo, cm.SHA)
			if err != nil {
				ch.Unknown = append(ch.Unknown, assess.Work{Day: cm.Day, Kind: "commit", Cite: "commit:" + shortSHA(cm.SHA), Why: "the forge did not say what it changes"})
				seen["day:"+cm.Day] = true
				continue
			}
			bot := ""
			if assess.BotAuthor(d.Login, d.Name, d.Email) {
				bot = strings.TrimSpace(d.Name)
			}
			addCommit(cm.Day, cm.SHA, d.Files, bot)
		}
	}
	for _, h := range a.shared[name] {
		if h.Host == a.host {
			continue
		}
		if h.LastWork > after {
			ch.Real = append(ch.Real, assess.Work{Day: h.LastWork, Kind: "catalog", Why: h.Host + "'s sessions worked in its checkout", Real: true})
		}
		if h.OwnCommits != nil && *h.OwnCommits > 0 && h.LastCommit > after && !seen["day:"+h.LastCommit] {
			ch.Unknown = append(ch.Unknown, assess.Work{Day: h.LastCommit, Kind: "catalog", Why: h.Host + "'s own commit, whose diff no checkout or forge here shows"})
		}
	}
	for _, ws := range [][]assess.Work{ch.Real, ch.Maintenance, ch.Unknown} {
		sort.SliceStable(ws, func(i, j int) bool { return ws[i].Day > ws[j].Day })
	}
	a.workMemo[key] = ch
	return ch
}

func seenSHA(seen map[string]bool, sha string) bool {
	sha = strings.ToLower(sha)
	for k := range seen {
		if len(k) >= 7 && !strings.HasPrefix(k, "day:") && (strings.HasPrefix(sha, k) || strings.HasPrefix(k, sha)) {
			return true
		}
	}
	return false
}

func clipFiles(fs []string, n int) []string {
	if len(fs) <= n {
		return fs
	}
	return append(append([]string(nil), fs[:n]...), fmt.Sprintf("%d more", len(fs)-n))
}

// successorFolder is the live project folder a successor's name resolves to
// (its own folder, a folder whose name or alias it is, through suppression),
// with a problem when none; wait reports one with no record yet that this
// run decides later (the decision waits for it to the end of the run), later
// one with no record that is a project folder all the same (a repository by
// its check, or a folder that holds facts): the record is linked to it by
// code once it has one (linkSuccessors).
func (a *assessor) successorFolder(self, name string, pending map[string]*record.Record) (folder, problem string, wait, later bool) {
	n := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(name, "projects/")))
	n = strings.ReplaceAll(n, " ", "-")
	recs := map[string]*record.Record{}
	for f, r := range a.recs {
		recs[f] = r
	}
	for f, r := range pending {
		recs[f] = r
	}
	f := "projects/" + n
	if recs[f] == nil {
		if o := record.RepoOwner(recs, n); o != "" {
			f = o
		}
	}
	if recs[f] == nil {
		switch {
		case f == self:
			return "", fmt.Sprintf("successor %s: that is this folder itself", name), false, false
		case a.runFolders[f] && !a.finalPass:
			return f, "", true, false
		case a.holdsFacts(f) || a.checker != nil && a.checker.Repo(a.ctx, n).Verdict == check.Holds:
			return f, "", false, true
		}
		return "", fmt.Sprintf("successor %s: no project folder of that name or alias has a record, and no repository of that name exists by its check", name), false, false
	}
	w, _, err := record.Resolve(recs, f)
	r := recs[w]
	switch {
	case err != nil || r == nil:
		return "", fmt.Sprintf("successor %s: its suppression loops or ends at no record", name), false, false
	case w == self:
		return "", fmt.Sprintf("successor %s: that is this folder itself (or suppressed into it)", name), false, false
	case record.Broad(r.Kind) != "project" || !strings.HasPrefix(w, "projects/") || notProject(r):
		return "", fmt.Sprintf("successor %s: %s is no live project (%s, %s)", name, w, r.Kind, r.Status), false, false
	}
	return w, "", false, false
}

// wordsReview is the optional review item of a folder whose owner's words
// the owner's later work overrode: one check? line on the note, with the
// work, never waiting on the owner (SPEC.md, "Rotation": optional outcomes).
func wordsReview(c assess.Candidate, s assess.Said, ch assess.Changes, today string) []string {
	return []string{checkLine(s.Note.Name, s.Note.Scope, "activity", today),
		fmt.Sprintf("  %s: the owner's words of %s (%q) say %s, but the owner worked on it after that day, so its dates stand: %s", c.Folder, s.Day, clipOne(s.Words, 200), s.Says, ch.Summary(4))}
}

// restsOnWords reports a record entry whose status rests on the owner's
// words (code wrote it: WordsOutcome), and their day: a status the dates do
// not move (statusDrift) until the owner's real work after that day.
func restsOnWords(e record.Entry) (string, bool) {
	if day, ok := e.RestsOnWords(); ok {
		return day, true
	}
	pre := assess.WordsWhy + e.Status + " on the owner's words of "
	if rest, ok := strings.CutPrefix(e.Why, pre); ok && len(rest) >= 10 && fact.IsDate(rest[:10]) {
		return rest[:10], true
	}
	return "", false
}

// ownerWordsOutcome checks d's owner_status for c and, when its newest item
// holds, is code's decision on it (assess.WordsOutcome): the words' status
// and successor, or the dates when the owner worked on it since, with the
// optional review lines of that case. wait reports a successor this run
// gives a record later: the decision waits to the run's end. problems
// refuse the decision (sent back to the model).
func (a *assessor) ownerWordsOutcome(c assess.Candidate, d assess.Decision, pending map[string]*record.Record) (out *assess.Decision, wait bool, problems, review []string) {
	if len(d.OwnerStatus) == 0 || c.Kind != "" || d.Kind == assess.KindNone || strings.TrimSpace(d.SuppressedBy) != "" {
		return nil, false, nil, nil
	}
	said, ps := assess.CheckOwnerWords(c, d)
	if len(ps) > 0 {
		return nil, false, ps, nil
	}
	if len(said) == 0 {
		return nil, false, nil, nil
	}
	s := said[0]
	succ, later := "", false
	if s.Successor != "" {
		f, prob, w, l := a.successorFolder(c.Folder, s.Successor, pending)
		switch {
		case w:
			return nil, true, nil, nil
		case prob != "":
			return nil, false, []string{prob}, nil
		}
		succ, later = f, l
	}
	ch := c.Changes(s.Day)
	nd, stood := assess.WordsOutcome(c, d, s, succ, ch)
	if later && nd.Successor != "" {
		// Its successor has no record yet: code links it once it has one.
		nd.Successor = ""
		nd.Why = strings.Replace(nd.Why, "; replaced by "+succ, "; replaced by "+succ+" (linked once it has a record)", 1)
		if a.wantSuccessor == nil {
			a.wantSuccessor = map[string]string{}
		}
		a.wantSuccessor[c.Folder] = succ
	}
	if !a.format3() {
		// An older binary's decoder refuses the fields: the status and its
		// why only (the why holds the words, the day and the successor).
		nd.Words, nd.Successor = nil, ""
	}
	if stood {
		fmt.Fprintf(os.Stderr, "assess: %s: %s on the owner's words of %s (note:%s)%s; %s\n", c.Folder, nd.Status, s.Day, s.Note.Name, map[bool]string{true: ", replaced by " + succ, false: ""}[succ != ""], ch.Summary(3))
	} else {
		fmt.Fprintf(os.Stderr, "assess: %s: the owner's words of %s (note:%s) say %s, but the owner worked on it since: code's %s from its dates; %s\n", c.Folder, s.Day, s.Note.Name, s.Says, nd.Status, ch.Summary(3))
		review = wordsReview(c, s, ch, a.today)
	}
	return &nd, false, nil, review
}

// noteKey is what identifies a note across a move to another folder (the
// migration copies it under a new name, with its text): its tier and text.
func noteKey(n assess.Note) string {
	h := sha256.Sum256([]byte(strings.Join(strings.Fields(strings.ToLower(n.Description+"\n"+n.Body)), " ")))
	return fmt.Sprintf("%d:%s", n.Tier, hex.EncodeToString(h[:8]))
}

// ownerNoteKeys are the keys of c's notes of the owner's tiers, sorted.
func ownerNoteKeys(c assess.Candidate) []string {
	var out []string
	for _, n := range c.Notes {
		if n.OwnerTier() {
			out = append(out, noteKey(n))
		}
	}
	sort.Strings(out)
	return out
}

// unseenOwnerNotes counts c's notes of the owner's tiers that the last
// decision of its folder did not see (all of them for a folder decided
// before notes were shown).
func (a *assessor) unseenOwnerNotes(c assess.Candidate) int {
	if a.held == nil {
		return 0
	}
	seen := keySet(a.held.Notes[c.Folder])
	n := 0
	for _, k := range ownerNoteKeys(c) {
		if !seen[k] {
			n++
		}
	}
	return n
}

// linkSuccessors are code's decisions that link a dead record to the
// successor its owner's words named once that has a live record
// (heldState.Successors): the record with its successor, no new entry (its
// status stands). An entry whose record is no longer dead on those words is
// dropped.
func (a *assessor) linkSuccessors(held *heldState) []decided {
	var out []decided
	pending := map[string]*record.Record{}
	for _, f := range sortedKeysStr(held.Successors) {
		want := held.Successors[f]
		r := a.recs[f]
		if r == nil || !r.Live() || r.Status != record.StatusDead || r.Successor != "" || len(r.Was) == 0 {
			delete(held.Successors, f)
			continue
		}
		if _, ok := restsOnWords(r.Was[0]); !ok {
			delete(held.Successors, f)
			continue
		}
		w, prob, _, later := a.successorFolder(f, want, pending)
		if prob != "" || later || w == "" {
			continue // not yet
		}
		rec := r.Clone()
		rec.Successor = w
		if ps := record.Validate(r, rec, record.ByOpus, a.recordEnv(pending, nil)); len(ps) > 0 {
			fmt.Fprintf(os.Stderr, "assess: %s: successor %s not linked: %s\n", f, w, strings.Join(messages(ps), "; "))
			continue
		}
		pending[f] = rec
		delete(held.Successors, f)
		out = append(out, decided{c: assess.Candidate{Folder: f, Reason: "its successor has a record now"}, rec: rec, old: r})
	}
	return out
}
