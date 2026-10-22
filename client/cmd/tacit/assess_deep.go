package main

// needs-review is not a parking state (the owner, 2026-10-22: "Need review
// is not something that will surely happen, its what better happen, but
// nobody promise, the agents need to still try to fix it"). A folder left
// for review, or refused on the same evidence, comes back to the model with
// more evidence than it had, gathered by code and wider on each attempt:
// facts in other folders that name it, the sessions behind them, the
// forge's record of the repository, the decisions on folders alike and the
// owner's turns that name it. A folder waits only when that wider search
// found nothing new (its items are in the fingerprint), and even then it is
// asked again on the weekly retry. Every attempt is logged.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/assess"
	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/overlay"
	"github.com/agent-sh/tacit/client/internal/platform"
	"github.com/agent-sh/tacit/client/internal/record"
)

const (
	// reviewRetryDays is the slow cadence: a folder whose wider search found
	// nothing new is asked again after a week. The forge's answers on a
	// repository's name are kept 7 days (check.DefaultHitTTL), so each retry
	// reads them fresh; in the Mac shadow runs a re-ask on unchanged evidence
	// gave the same answer (assessHoldRuns), so a daily retry would pay for
	// the same answer six days in seven.
	reviewRetryDays = 7
	// deepMaxDepth bounds how far the search widens: depth d shows up to
	// deepPer*d items of each kind.
	deepMaxDepth = 4
	deepPer      = 6
	// reviewAttemptsLog is the attempts' log in the state directory.
	reviewAttemptsLog = "assess-attempts.jsonl"
)

// reviewState is a folder left for review: the fingerprint of what it was
// last decided on (its extra evidence included), when, after how many
// attempts, and the extra items it was shown.
type reviewState struct {
	Fingerprint string   `json:"fingerprint"`
	At          string   `json:"at"`
	Attempts    int      `json:"attempts"`
	Keys        []string `json:"keys,omitempty"`
}

// retryDue reports whether a folder last decided on day at is due for the
// weekly retry today.
func retryDue(at, today string) bool {
	a, err1 := time.Parse("2006-01-02", at)
	t, err2 := time.Parse("2006-01-02", today)
	if err1 != nil || err2 != nil {
		return true
	}
	return t.Sub(a) >= reviewRetryDays*24*time.Hour
}

// nextRetry is the day a folder last decided on at is asked again anyway.
func nextRetry(at string) string {
	a, err := time.Parse("2006-01-02", at)
	if err != nil {
		return "the next run"
	}
	return a.AddDate(0, 0, reviewRetryDays).Format("2006-01-02")
}

func keySet(keys []string) map[string]bool {
	out := map[string]bool{}
	for _, k := range keys {
		out[k] = true
	}
	return out
}

// deepen gathers c's extra evidence for its attempt-th re-attempt (wider
// with each), marking what an earlier attempt showed (seen) and when it was
// last decided (lastAt).
func (a *assessor) deepen(c *assess.Candidate, attempt int, seen []string, lastAt string) {
	if attempt < 1 {
		attempt = 1
	}
	depth := min(attempt, deepMaxDepth)
	per := deepPer * depth
	c.Attempt, c.LastAt, c.Seen = attempt, lastAt, keySet(seen)
	c.Extra = nil
	if c.Kind != "" {
		return // a proposed area or topic: its sessions are its evidence
	}
	name := strings.ToLower(c.Folder[strings.LastIndex(c.Folder, "/")+1:])
	names := a.namesOf(*c, name)
	hits := a.factsNaming(c.Folder, names, name)
	c.Extra = append(c.Extra, a.extraFacts(hits, names, per)...)
	c.Extra = append(c.Extra, a.extraSessions(c.Folder, hits, per)...)
	c.Extra = append(c.Extra, a.extraForge(c, per)...)
	c.Extra = append(c.Extra, a.extraAlike(c, name, per)...)
	c.Extra = append(c.Extra, a.extraTurns(names, 3*depth)...)
}

// namesOf are the names the folder goes by: its own, its record's aliases,
// the catalogs' other names of its checkout, its forge names.
func (a *assessor) namesOf(c assess.Candidate, name string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(n string) {
		n = strings.ToLower(strings.TrimSpace(n))
		if n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	add(name)
	if c.Old != nil {
		for _, al := range c.Old.Aliases {
			add(al)
		}
		if f := c.Old.Forge; f != "" {
			add(f[strings.Index(f, "/")+1:]) // owner/name, past the host
		}
	}
	if r, ok := a.catalog.Repo(name); ok {
		for _, al := range r.Aliases {
			add(al)
		}
	}
	for _, h := range a.shared[name] {
		for _, al := range h.Aliases {
			add(al)
		}
	}
	if c.Name != nil {
		add(c.Name.Forge)
		add(c.Name.Renamed)
	}
	return out
}

// wordish reports a name that is one plain word (no dash, dot, digit or
// slash, under 8 letters: run, tools, client, beacon): it may be a common
// word, so its whole-word hits count only while they are few (maxWordHits);
// past that a fact names the folder by it only as a path or a repository.
func wordish(n string) bool {
	return len(n) < 8 && !strings.ContainsAny(n, "-._/0123456789")
}

// maxWordHits is how many facts a wordish name may name as a plain word and
// still be taken for the folder's name rather than the word ("run" is in
// thousands of facts, "beacon" in a few).
const maxWordHits = 20

// namesIt reports text naming n as a path or repository ("projects/run",
// "agent-sh/tools", "`run`") or, for a name that is no plain word, as a
// whole word; word reports a whole-word hit of a wordish name.
func namesIt(text, n string) (hit, word bool) {
	if strings.Contains(n, "/") {
		return strings.Contains(text, n), false
	}
	if !wordish(n) {
		return record.WholeWord(text, n), false
	}
	if pathNameRx(n).MatchString(text) {
		return true, false
	}
	return false, record.WholeWord(text, n)
}

var pathRxCache = map[string]*regexp.Regexp{}

func pathNameRx(n string) *regexp.Regexp {
	if rx, ok := pathRxCache[n]; ok {
		return rx
	}
	q := regexp.QuoteMeta(n)
	rx := regexp.MustCompile(`(?:[a-z0-9_.~-]/` + q + `(?:$|[^a-z0-9_-])|` + "`" + q + "`)")
	pathRxCache[n] = rx
	return rx
}

// factsNaming are the live curated facts outside folder that name one of
// names (namesIt) or list it in applies_to.repos, newest first.
func (a *assessor) factsNaming(folder string, names []string, name string) []*fact.Fact {
	if a.lowText == nil {
		a.lowText = map[*fact.Fact]string{}
	}
	var hits []*fact.Fact
	words := map[string][]*fact.Fact{} // wordish name -> its plain-word hits
	for _, f := range a.facts {
		if f.Meta.Scope == folder || f.InInbox() || overlay.IsInbox(f.Path) || f.Meta.SupersededBy != "" || f.Meta.Retired != "" {
			continue
		}
		hit := false
		for _, r := range f.Meta.AppliesTo.Repos {
			for _, n := range names {
				if strings.EqualFold(r, n) {
					hit = true
				}
			}
		}
		if !hit {
			t, ok := a.lowText[f]
			if !ok {
				t = strings.ToLower(f.Meta.Description + "\n" + f.Body)
				a.lowText[f] = t
			}
			for _, n := range names {
				h, w := namesIt(t, n)
				if h {
					hit = true
					break
				}
				if w {
					words[n] = append(words[n], f)
				}
			}
		}
		if hit {
			hits = append(hits, f)
		}
	}
	in := map[*fact.Fact]bool{}
	for _, f := range hits {
		in[f] = true
	}
	for _, n := range names {
		if ws := words[n]; len(ws) <= maxWordHits {
			for _, f := range ws {
				if !in[f] {
					in[f] = true
					hits = append(hits, f)
				}
			}
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Meta.Created != hits[j].Meta.Created {
			return hits[i].Meta.Created > hits[j].Meta.Created
		}
		return hits[i].Meta.Name < hits[j].Meta.Name
	})
	return hits
}

func (a *assessor) extraFacts(hits []*fact.Fact, names []string, per int) []assess.Extra {
	if len(hits) == 0 {
		return []assess.Extra{{Line: fmt.Sprintf("store search for %s: no fact in another folder names it", strings.Join(names, ", "))}}
	}
	out := []assess.Extra{{Line: fmt.Sprintf("store search for %s: %d fact(s) in other folders name it or list it in applies_to (newest first, up to %d):", strings.Join(names, ", "), len(hits), per)}}
	for i, f := range hits {
		if i == per {
			break
		}
		sess := ""
		if s := f.Meta.Source.Session; s != "" {
			sess = ", session:" + s
		}
		out = append(out, assess.Extra{Key: "fact:" + f.Meta.Name,
			Line: fmt.Sprintf("  - [[%s]] (%s, %s, %s%s): %s", f.Meta.Name, f.Meta.Scope, f.Meta.Type, f.Meta.Created, sess, fact.ClipDescription(f.Meta.Description, 160))})
	}
	return out
}

// extraSessions are the sessions behind the facts that name the folder
// elsewhere, and the sessions the registry has working in a folder of its
// name: who touched it, and when.
func (a *assessor) extraSessions(folder string, hits []*fact.Fact, per int) []assess.Extra {
	type sess struct {
		last   string
		n      int
		scopes map[string]bool
	}
	by := map[string]*sess{}
	for _, f := range hits {
		id := f.Meta.Source.Session
		if id == "" {
			continue
		}
		s := by[id]
		if s == nil {
			s = &sess{scopes: map[string]bool{}}
			by[id] = s
		}
		s.n++
		s.scopes[f.Meta.Scope] = true
		if f.Meta.Created > s.last {
			s.last = f.Meta.Created
		}
	}
	ids := make([]string, 0, len(by))
	for id := range by {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if by[ids[i]].last != by[ids[j]].last {
			return by[ids[i]].last > by[ids[j]].last
		}
		return ids[i] < ids[j]
	})
	if len(ids) == 0 {
		return nil
	}
	out := []assess.Extra{{Line: fmt.Sprintf("sessions that filed those facts (%d, newest first, up to %d): each touched this folder's subject from another folder", len(ids), per)}}
	for i, id := range ids {
		if i == per {
			break
		}
		s := by[id]
		var sc []string
		for k := range s.scopes {
			sc = append(sc, k)
		}
		sort.Strings(sc)
		out = append(out, assess.Extra{Key: "session:" + id, Line: fmt.Sprintf("  - session:%s (%s): %d fact(s) naming it, in %s", id, s.last, s.n, strings.Join(sc, ", "))})
	}
	return out
}

// forgeFull is the owner/name the forge has the folder's repository under.
func forgeFull(c *assess.Candidate) string {
	if c.Name != nil && c.Name.Forge != "" {
		return c.Name.Forge
	}
	if c.Old != nil && c.Old.Forge != "" {
		f := c.Old.Forge
		return f[strings.Index(f, "/")+1:]
	}
	return ""
}

// extraForge is the forge's record of the folder's repository: last push,
// archived, latest release and newest commits, the owner's account's
// listed as the folder's own (citable, knownCommit).
func (a *assessor) extraForge(c *assess.Candidate, per int) []assess.Extra {
	full := forgeFull(c)
	owner, name, ok := strings.Cut(full, "/")
	if !ok || a.checker == nil || a.checker.Forge == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(a.ctx, time.Minute)
	defer cancel()
	act, err := a.checker.Forge.Activity(ctx, owner, name, min(per, 20))
	if err != nil {
		return nil
	}
	user := strings.ToLower(a.checker.Forge.Forge.User(ctx))
	head := fmt.Sprintf("forge: github.com/%s: last push %s", act.FullName, orDefaultStr(act.PushedAt, "unknown"))
	out := []assess.Extra{}
	keys := []string{"forge:pushed:" + act.PushedAt}
	if act.Archived {
		head += ", archived"
		keys = append(keys, "forge:archived")
	}
	if act.Release != "" {
		head += fmt.Sprintf(", latest release %s (%s)", act.Release, act.ReleasedAt)
		keys = append(keys, "forge:release:"+act.Release)
	}
	if user != "" {
		head += "; the owner's forge account is " + user
	}
	out = append(out, assess.Extra{Key: strings.Join(keys, " "), Line: head})
	for i, cm := range act.Commits {
		if i == per {
			break
		}
		sha := cm.SHA
		if len(sha) > 12 {
			sha = sha[:12]
		}
		who := orDefaultStr(cm.Login, "no forge account")
		if user != "" && strings.EqualFold(cm.Login, user) {
			who += " (the owner's: an own commit, cite as commit:" + sha + ")"
			a.commitsShown[c.Folder] = append(a.commitsShown[c.Folder], strings.ToLower(cm.SHA))
		}
		out = append(out, assess.Extra{Key: "forge:commit:" + cm.SHA, Line: fmt.Sprintf("  - commit:%s %s by %s", sha, cm.Day, who)})
	}
	return out
}

// extraAlike are the decisions on folders alike: near names, and the other
// repositories of the same forge owner, each settled (not left for review).
func (a *assessor) extraAlike(c *assess.Candidate, name string, per int) []assess.Extra {
	var near, peers []*record.Record
	owner, _, _ := strings.Cut(forgeFull(c), "/")
	owner = strings.ToLower(owner)
	for _, f := range sortedRecordFolders(a.recs) {
		r := a.recs[f]
		if f == c.Folder || !strings.HasPrefix(f, "projects/") || r.Status == record.StatusNeedsReview || len(r.Was) == 0 {
			continue
		}
		if _, ok := assess.Near(name, strings.TrimPrefix(f, "projects/")); ok {
			near = append(near, r)
			continue
		}
		if owner != "" && r.Live() && (strings.EqualFold(r.Org, owner) || strings.HasPrefix(strings.ToLower(r.Forge), "github.com/"+owner+"/")) {
			peers = append(peers, r)
		}
	}
	line := func(r *record.Record) string {
		s := fmt.Sprintf("  - %s: %s, %s", r.Folder, r.Kind, r.Status)
		if r.SuppressedBy != "" {
			s += ", suppressed by " + r.SuppressedBy
		}
		return s + fmt.Sprintf(" (%s by %s): %s", r.Was[0].Date, r.Was[0].By, oneLineClip(r.Was[0].Why, 160))
	}
	var out []assess.Extra
	if len(near) > 0 {
		out = append(out, assess.Extra{Line: fmt.Sprintf("decisions on folders with near names (%d):", len(near))})
		for i, r := range near {
			if i == per {
				break
			}
			out = append(out, assess.Extra{Key: "alike:" + r.Folder + ":" + r.Status + ":" + r.SuppressedBy, Line: line(r)})
		}
	}
	if len(peers) > 0 {
		count := map[string]int{}
		for _, r := range peers {
			count[r.Status]++
		}
		var parts []string
		for _, st := range record.Statuses() {
			if count[st] > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", count[st], st))
			}
		}
		sort.SliceStable(peers, func(i, j int) bool { return peers[i].Was[0].Date > peers[j].Was[0].Date })
		out = append(out, assess.Extra{Line: fmt.Sprintf("decisions on other repositories of %s on the forge (%d: %s), newest first:", owner, len(peers), strings.Join(parts, ", "))})
		for i, r := range peers {
			if i == per {
				break
			}
			out = append(out, assess.Extra{Key: "peer:" + r.Folder + ":" + r.Status, Line: line(r)})
		}
	}
	return out
}

func oneLineClip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// extraTurns are the owner's turns that name the folder by any of its
// names (redacted; left out when redaction fails).
func (a *assessor) extraTurns(names []string, n int) []assess.Extra {
	if a.corpus == nil {
		return nil
	}
	var texts, refs []string
	seen := map[string]bool{}
	for _, nm := range names {
		if strings.Contains(nm, "/") {
			continue
		}
		for _, m := range a.corpus.Mentions(nm, n) {
			if seen[m.Ref] {
				continue
			}
			seen[m.Ref] = true
			texts = append(texts, clipAround(m.Text, nm, 400))
			refs = append(refs, m.Ref)
		}
	}
	if len(texts) == 0 {
		return nil
	}
	red, err := digest.RedactAll(texts)
	if err != nil {
		return nil
	}
	out := []assess.Extra{{Line: fmt.Sprintf("the owner's turns that name it (%d):", len(texts))}}
	for i := range texts {
		out = append(out, assess.Extra{Key: "turn:" + refs[i], Line: fmt.Sprintf("  - owner's turn (%s): %q", refs[i], red[i])})
	}
	return out
}

// reviewAttempt is one line of the attempts' log: a folder left for review
// (or refused on the same evidence) asked again, what code gathered for it,
// and how it came out.
type reviewAttempt struct {
	At      string   `json:"at"`
	Folder  string   `json:"folder"`
	Attempt int      `json:"attempt"`
	Why     string   `json:"why"`
	Items   int      `json:"items"`
	New     int      `json:"new"`
	NewKeys []string `json:"new_keys,omitempty"`
	Outcome string   `json:"outcome"` // the status decided, "refused", or "waits"
}

// logAttempts appends the run's attempts to the state directory's log.
func (a *assessor) logAttempts(lines []reviewAttempt) {
	if len(lines) == 0 {
		return
	}
	dir := platform.StateDir(a.home)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, reviewAttemptsLog), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	for _, l := range lines {
		b, _ := json.Marshal(l)
		f.Write(append(b, '\n'))
	}
}

// attemptOf is c's attempt line: its items and the new ones.
func attemptOf(c assess.Candidate, today, why, outcome string) reviewAttempt {
	ra := reviewAttempt{At: today, Folder: c.Folder, Attempt: c.Attempt, Why: why, Items: len(c.ExtraKeys()), New: c.NewExtra(), Outcome: outcome}
	for _, k := range c.ExtraKeys() {
		if !c.Seen[k] && len(ra.NewKeys) < 20 {
			ra.NewKeys = append(ra.NewKeys, k)
		}
	}
	return ra
}

// reviewDue gathers the extra evidence of c, a folder left for review, and
// reports whether it is asked this run, with why: never asked with extra
// evidence yet, the wider search found something new, or the weekly retry
// (or the rotation's run, retry). Otherwise it waits, logged.
func (a *assessor) reviewDue(c *assess.Candidate) (string, bool) {
	st, had := a.held.Review[c.Folder]
	a.deepen(c, st.Attempts+1, st.Keys, st.At)
	fp := a.fingerprint(*c)
	switch {
	case !had:
		return "left for review; asked again with the extra evidence code gathered for it", true
	case fp != st.Fingerprint && c.NewExtra() > 0:
		return fmt.Sprintf("left for review on %s; code's wider search found %d new item(s)", st.At, c.NewExtra()), true
	case fp != st.Fingerprint:
		return fmt.Sprintf("left for review on %s; its evidence changed since", st.At), true
	case a.retry || retryDue(st.At, a.today):
		return fmt.Sprintf("left for review on %s; the weekly retry (its wider search found nothing new)", st.At), true
	}
	a.reviewWaiting++
	a.attempts = append(a.attempts, attemptOf(*c, a.today, "left for review", "waits"))
	fmt.Fprintf(os.Stderr, "assess: %s waits: left for review on %s and its wider search (%d item(s)) found nothing new; asked again on %s\n", c.Folder, st.At, len(c.ExtraKeys()), nextRetry(st.At))
	return "", false
}
