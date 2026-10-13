package gitev

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/links"
)

// Side names which fact an event is about. A is the older or existing fact,
// B the newer or candidate one, as in the review pairs.
const (
	SideA    = "A"
	SideB    = "B"
	SideBoth = "A,B"
)

// Event kinds.
const (
	KindFact   = "fact"   // a date from a fact
	KindCommit = "commit" // a commit that touched the subject
	KindGap    = "gap"    // commits left out between two shown
	KindHead   = "head"   // what the repo shows now
	KindNote   = "note"   // something the reader should know about the search
)

// Event is one line of a timeline.
type Event struct {
	When string `json:"when"` // YYYY-MM-DD, or HEAD
	Kind string `json:"kind"`
	Side string `json:"side,omitempty"`
	Repo string `json:"repo,omitempty"`
	Ref  string `json:"ref,omitempty"` // short sha of a commit
	Text string `json:"text"`
	// By is a commit's identity in short form: the author, the committer when
	// different, and co-authors. Empty on other kinds.
	By string `json:"by,omitempty"`
	// Via says why a commit is here: files, identifiers or its message.
	Via string `json:"via,omitempty"`
}

// Timeline is the dated, merged account of two facts and the git history of
// what they are about.
type Timeline struct {
	Repos  []string `json:"repos"`
	Terms  []string `json:"terms"`
	Since  string   `json:"since,omitempty"`
	AsOf   string   `json:"as_of,omitempty"`
	Events []Event  `json:"events"`
	// Summary is the timeline in a few plain sentences, for the owner's page.
	Summary []string `json:"summary,omitempty"`
	// Incomplete: some read failed or ran out of time. The notes in Events say
	// which; a holder of the timeline must not keep it as if it were whole.
	Incomplete bool `json:"incomplete,omitempty"`
}

// Compact is the timeline as plain lines, for a caller that prints text.
type Compact struct {
	Repos []string `json:"repos,omitempty"`
	Terms []string `json:"terms,omitempty"`
	Lines []string `json:"lines"`
}

// Line renders one event as a single line.
func (e Event) Line() string {
	var b strings.Builder
	b.WriteString(e.When)
	if e.Side != "" {
		b.WriteString(" " + e.Side)
	}
	if e.Repo != "" {
		b.WriteString(" " + e.Repo)
	}
	if e.Ref != "" {
		b.WriteString(" " + e.Ref)
	}
	b.WriteString(" " + e.Text)
	if e.Via != "" {
		b.WriteString(" [" + e.Via + "]")
	}
	if e.By != "" {
		b.WriteString(" by " + e.By)
	}
	return b.String()
}

// Compact renders the timeline for a model.
func (t *Timeline) Compact() *Compact {
	c := &Compact{Repos: t.Repos, Terms: t.Terms}
	for _, e := range t.Events {
		c.Lines = append(c.Lines, e.Line())
	}
	return c
}

// FactRef is a fact with the inbox note the curator split it from, when
// there is one. The note's date is when the memory itself was written; the
// fact's own created date is when the curator filed it.
type FactRef struct {
	Fact   *fact.Fact
	Origin *fact.Fact
}

// Input is a pair of facts.
type Input struct{ A, B FactRef }

// Builder turns a pair of facts into a Timeline.
type Builder struct {
	G *Git
	// StoreDir is the clone of the store whose history dates the fact files.
	StoreDir string
	// Redact masks secrets in every text before it leaves the builder. The
	// default is the client's own redactor (digest.RedactAllContext, which stops
	// with the build's context); when it fails, Build fails and returns
	// nothing rather than text that might hold a secret.
	Redact func([]string) ([]string, error)
	// AsOf reads the repos as they were at this time (a replay); zero is now.
	AsOf time.Time
	// Lead is how far before the older fact the window opens (default 14
	// days: a fact is usually written some days after the change it records).
	Lead time.Duration
	// MaxLines caps the timeline (default 24). Facts' dates and the HEAD lines
	// are kept; commits are dropped from the middle first.
	MaxLines int
	// Now stands in for the clock in tests.
	Now func() time.Time
}

var reDate = regexp.MustCompile(`\b(20\d\d)-(\d\d)-(\d\d)\b`)

func (b *Builder) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

func parseDay(s string) (time.Time, bool) {
	t, err := time.Parse("2006-01-02", strings.Trim(strings.TrimSpace(s), `"`))
	return t, err == nil
}

// dated is what a fact says about when it is from.
type dated struct {
	asOf       time.Time // best estimate of when what it says was true
	asOfWhy    string
	events     []Event
	incomplete bool      // the store history could not be read
	lastStore  time.Time // the newest commit of the fact's file in the store
	storeN     int       // how many commits touched it
}

func (b *Builder) factDates(ctx context.Context, side string, r FactRef) dated {
	f := r.Fact
	var d dated
	// Nothing a fact says is from after the time the read is as of, and a text
	// date after the day the fact was filed is a plan, not a record.
	upper := b.now()
	if !b.AsOf.IsZero() {
		upper = b.AsOf
	}
	created, okC := parseDay(f.Meta.Created)
	if okC && created.AddDate(0, 0, 1).Before(upper) {
		upper = created.AddDate(0, 0, 1)
	}
	var newest time.Time
	for _, m := range reDate.FindAllString(f.Meta.Description+"\n"+f.Body, -1) {
		if t, ok := parseDay(m); ok && t.Before(upper) && t.After(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)) && t.After(newest) {
			newest = t
		}
	}
	// add keeps a date only if it is not after the replay time.
	add := func(t time.Time, text string) bool {
		if !b.AsOf.IsZero() && t.After(b.AsOf) {
			return false
		}
		d.events = append(d.events, Event{When: t.Format("2006-01-02"), Kind: KindFact, Side: side, Text: text})
		return true
	}
	if !newest.IsZero() && add(newest, "reads as of this day (newest date in its text)") {
		d.asOf, d.asOfWhy = newest, "text"
	}
	if o := r.Origin; o != nil {
		if t, ok := parseDay(o.Meta.Created); ok && add(t, "source note written ("+originKind(o)+")") && d.asOf.IsZero() {
			d.asOf, d.asOfWhy = t, "source"
		}
	}
	if okC {
		verb := "written"
		if f.Meta.Source.Harness == "curator" {
			verb = "curated into the store"
		}
		if add(created, verb) && d.asOf.IsZero() {
			d.asOf, d.asOfWhy = created, "created"
		}
	}
	if u, ok := parseDay(f.Meta.Updated); ok && (!okC || !u.Equal(created)) {
		add(u, "updated")
	}
	first, last, n, err := b.storeHistory(ctx, f.Path)
	if err != nil {
		d.incomplete = true
	}
	d.lastStore, d.storeN = last, n
	if n > 0 {
		if !okC || first.Format("2006-01-02") != created.Format("2006-01-02") {
			add(first, "first commit in store git")
		}
		if n > 1 && last.Format("2006-01-02") != first.Format("2006-01-02") {
			add(last, fmt.Sprintf("last change in store git (%d commits)", n))
		}
	}
	return d
}

func originKind(o *fact.Fact) string {
	switch o.Meta.Source.Harness {
	case "import":
		return "imported memory"
	case "dreamer":
		return "dreamer note"
	case "":
		return "note"
	}
	return o.Meta.Source.Harness + " note"
}

// storeHistory reads the first and last commit date of a fact file in the
// store clone, and how many commits touched it. A replay sees only the
// commits made before AsOf.
func (b *Builder) storeHistory(ctx context.Context, rel string) (first, last time.Time, n int, err error) {
	if b.StoreDir == "" || rel == "" {
		return
	}
	args := []string{"log", "--format=%at\t%H\t%s"}
	if !b.AsOf.IsZero() {
		args = append(args, "--before="+gitDate(b.AsOf))
	}
	o, _, err := b.G.run(ctx, b.StoreDir, 1<<20, append(args, "--", rel)...)
	if err != nil {
		return
	}
	for _, l := range strings.Split(o, "\n") {
		ts, rest, _ := strings.Cut(strings.TrimSpace(l), "\t")
		sha, subject, _ := strings.Cut(rest, "\t")
		if strings.HasPrefix(subject, links.CommitPrefix) && b.linkRepairOnly(ctx, sha, rel) {
			continue // a link repair changes no word of what the fact says
		}
		t := unix(ts)
		if t.IsZero() {
			continue
		}
		n++
		if last.IsZero() {
			last = t
		}
		first = t
	}
	return
}

// linkRepairOnly says whether the commit's change to the fact file rel is
// exactly what the link-repair step writes (links.IsRepair of the file before
// and after). The subject alone proves nothing: anyone can use it, and a commit
// of that step may carry more. A commit that cannot be read, or that created
// the file, counts as an edit.
func (b *Builder) linkRepairOnly(ctx context.Context, sha, rel string) bool {
	if sha == "" {
		return false
	}
	after, cut1, err1 := b.G.run(ctx, b.StoreDir, 1<<20, "show", sha+":"+rel)
	before, cut2, err2 := b.G.run(ctx, b.StoreDir, 1<<20, "show", sha+"^:"+rel)
	if err1 != nil || err2 != nil || cut1 || cut2 {
		return false
	}
	return links.IsRepair([]byte(before), []byte(after))
}

// Build reads the timeline for a pair. Every text read from git or taken
// from the facts is redacted in one call before anything is clipped or laid
// out (a secret cut in half is no longer recognised), and the build fails
// closed: if the redactor fails there is no timeline.
func (b *Builder) Build(ctx context.Context, in Input) (*Timeline, error) {
	if in.A.Fact == nil || in.B.Fact == nil {
		return nil, errors.New("gitev: a timeline needs two facts")
	}
	da, db := b.factDates(ctx, SideA, in.A), b.factDates(ctx, SideB, in.B)
	older := olderOf(da, db)
	sub, w, evidence := b.read(ctx, in, older)
	var ptrs []*string
	ptrs = append(ptrs, subjectPtrs(&sub)...)
	for i := range evidence {
		ptrs = append(ptrs, evidencePtrs(&evidence[i])...)
	}
	for _, d := range []*dated{&da, &db} {
		for i := range d.events {
			ptrs = append(ptrs, &d.events[i].Text)
		}
	}
	if err := b.redactPtrs(ctx, ptrs); err != nil {
		return nil, err
	}
	t := &Timeline{Repos: sub.Repos, Terms: append(append([]string{}, sub.Paths...), sub.Idents...), Incomplete: da.incomplete || db.incomplete}
	if !w.Since.IsZero() {
		t.Since = w.Since.Format("2006-01-02")
	}
	if !b.AsOf.IsZero() {
		t.AsOf = b.AsOf.Format("2006-01-02 15:04")
	}
	var evs []Event
	evs = append(evs, da.events...)
	evs = append(evs, db.events...)
	evs = mergeSides(evs)
	// A term is absent only when no repo has it: a path from one repo is
	// not "not present" in the others.
	somewhere := map[string]bool{}
	for _, ev := range evidence {
		for _, st := range ev.State {
			if st.Present {
				somewhere[st.Term] = true
			}
		}
	}
	reported := map[string]bool{}
	for _, ev := range evidence {
		t.Incomplete = t.Incomplete || ev.Incomplete
		evs = append(evs, repoEvents(ev, older, somewhere, reported)...)
	}
	if len(sub.Repos) == 0 {
		evs = append(evs, Event{When: "HEAD", Kind: KindNote, Text: "no repo under the projects directory is named by these facts, so git has nothing to compare"})
	}
	if da.incomplete || db.incomplete {
		evs = append(evs, Event{When: "HEAD", Kind: KindNote, Text: "the store history of a fact could not be read, so its store dates are missing"})
	}
	t.Events = order(evs, b.maxLines())
	now := b.AsOf
	if now.IsZero() {
		now = time.Now()
	}
	t.Summary = summarize(evidence, sub.Repos, older, somewhere, in.A.Fact == in.B.Fact, t.Incomplete, now)
	return t, nil
}

func olderOf(da, db dated) time.Time {
	older := da.asOf
	if older.IsZero() || (!db.asOf.IsZero() && db.asOf.Before(older)) {
		older = db.asOf
	}
	return older
}

// Evidence reads the subject of a pair and the repo evidence for it, without
// laying anything out: the structured form for callers that reason over
// identities and dates themselves (the curator's staleness check, results
// taken from git). Every text in it is redacted like Build's, and the call
// fails closed the same way.
func (b *Builder) Evidence(ctx context.Context, in Input) (Subject, Window, []RepoEvidence, error) {
	if in.A.Fact == nil || in.B.Fact == nil {
		return Subject{}, Window{}, nil, errors.New("gitev: needs two facts")
	}
	older := olderOf(b.factDates(ctx, SideA, in.A), b.factDates(ctx, SideB, in.B))
	sub, w, ev := b.read(ctx, in, older)
	ptrs := subjectPtrs(&sub)
	for i := range ev {
		ptrs = append(ptrs, evidencePtrs(&ev[i])...)
	}
	if err := b.redactPtrs(ctx, ptrs); err != nil {
		return Subject{}, Window{}, nil, err
	}
	return sub, w, ev, nil
}

func (b *Builder) read(ctx context.Context, in Input, older time.Time) (Subject, Window, []RepoEvidence) {
	sub := b.G.SubjectOf(in.A.Fact, in.B.Fact)
	if older.IsZero() || len(sub.Repos) == 0 {
		return sub, Window{}, nil
	}
	lead := b.Lead
	if lead <= 0 {
		lead = 14 * 24 * time.Hour
	}
	w := Window{Since: older.Add(-lead), Anchor: older, AsOf: b.AsOf}
	// One budget of ten commits shared across the repos.
	w.Limit = 10 / len(sub.Repos)
	if w.Limit < 4 {
		w.Limit = 4
	}
	return sub, w, b.G.Collect(ctx, sub, w)
}

func (b *Builder) maxLines() int {
	if b.MaxLines > 0 {
		return b.MaxLines
	}
	return 24
}

// mergeSides joins events of the two facts with the same date and text.
func mergeSides(evs []Event) []Event {
	type key struct{ when, text string }
	idx := map[key]int{}
	var out []Event
	for _, e := range evs {
		k := key{e.When, e.Text}
		if i, ok := idx[k]; ok && out[i].Side != e.Side {
			out[i].Side = SideBoth
			continue
		} else if ok {
			continue
		}
		idx[k] = len(out)
		out = append(out, e)
	}
	return out
}

// order sorts events by date (HEAD last), keeps fact dates before commits
// of the same day, and trims to max lines dropping commits first.
func order(evs []Event, max int) []Event {
	rank := func(e Event) int {
		switch e.Kind {
		case KindFact:
			return 0
		case KindCommit, KindGap:
			return 1
		case KindNote:
			return 2
		}
		return 3
	}
	sort.SliceStable(evs, func(i, j int) bool {
		hi, hj := evs[i].When == "HEAD", evs[j].When == "HEAD"
		if hi != hj {
			return hj
		}
		if evs[i].When != evs[j].When {
			return evs[i].When < evs[j].When
		}
		return rank(evs[i]) < rank(evs[j])
	})
	for len(evs) > max {
		// Drop a commit nearest the middle of the commits.
		var at []int
		for i, e := range evs {
			if e.Kind == KindCommit || e.Kind == KindGap {
				at = append(at, i)
			}
		}
		if len(at) == 0 {
			return evs[:max]
		}
		i := at[len(at)/2]
		evs = append(evs[:i], evs[i+1:]...)
	}
	return evs
}

// repoEvents turns one repo's evidence into events.
func repoEvents(ev RepoEvidence, anchor time.Time, somewhere, reported map[string]bool) []Event {
	var out []Event
	for _, c := range ev.Commits {
		out = append(out, Event{When: c.At.Format("2006-01-02"), Kind: KindCommit, Repo: ev.Name, Ref: c.Short, Text: clip(c.Subject, 90), By: identity(c), Via: via(c)})
	}
	if ev.Between > 0 && !ev.BetweenTo.IsZero() {
		out = append(out, Event{When: ev.BetweenFrom.Format("2006-01-02"), Kind: KindGap, Repo: ev.Name,
			Text: fmt.Sprintf("%s on the subject are left out until %s", more(ev.Between), ev.BetweenTo.Format("2006-01-02"))})
	}
	if ev.Head == "" {
		for _, n := range ev.Notes {
			out = append(out, Event{When: "HEAD", Kind: KindNote, Repo: ev.Name, Text: n})
		}
		return out
	}
	where := ev.Branch
	if where == "" {
		where = "at"
	}
	head := fmt.Sprintf("%s %s (%s): %s since %s", where, ev.Head, ev.HeadAt.Format("2006-01-02"), commits(ev.CommitsSince), anchor.Format("2006-01-02"))
	switch {
	case len(ev.State) == 0:
	case ev.Matched == 0:
		head += "; none touch the subject"
	case ev.Capped:
		head += fmt.Sprintf("; %d+ touch the subject", ev.Matched)
	case ev.Matched == 1:
		head += "; 1 touches the subject"
	default:
		head += fmt.Sprintf("; %d touch the subject", ev.Matched)
	}
	out = append(out, Event{When: "HEAD", Kind: KindHead, Repo: ev.Name, Text: head})
	var present, absent, many []string
	for _, st := range ev.State {
		switch {
		case st.Unknown:
		case st.Generic:
			many = append(many, st.Term)
		case st.Present && len(st.Where) > 0 && st.Kind == "ident":
			present = append(present, fmt.Sprintf("%s in %s", st.Term, st.Where[0]+plus(st.Count-1)))
		case st.Present:
			present = append(present, st.Term+countOf(st))
		case !somewhere[st.Term] && !reported[st.Term]:
			reported[st.Term] = true
			absent = append(absent, st.Term)
		}
	}
	if len(present) > 0 {
		out = append(out, Event{When: "HEAD", Kind: KindHead, Repo: ev.Name, Text: "present: " + strings.Join(present, "; ")})
	}
	if len(absent) > 0 {
		out = append(out, Event{When: "HEAD", Kind: KindHead, Repo: ev.Name, Text: "not present: " + strings.Join(absent, "; ")})
	}
	for _, n := range ev.Notes {
		out = append(out, Event{When: "HEAD", Kind: KindNote, Repo: ev.Name, Text: n})
	}
	return out
}

func more(n int) string {
	if n == 1 {
		return "1 more commit"
	}
	return fmt.Sprintf("%d more commits", n)
}

func commits(n int) string {
	if n == 1 {
		return "1 commit"
	}
	return fmt.Sprintf("%d commits", n)
}

func plus(n int) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf(" +%d", n)
}

func countOf(st TermState) string {
	if st.Count > 1 {
		return fmt.Sprintf(" (%d paths)", st.Count)
	}
	return ""
}

func clip(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n-1]) + "…"
}

// identity is a commit's author and, when different, committer and
// co-authors, by name only: enough to tell the owner's own commits from an
// agent's, which a caller decides by comparing against a list it trusts.
func identity(c Commit) string {
	parts := []string{clip(c.Author.Name, 24)}
	if c.Committer.Name != "" && c.Committer.Name != c.Author.Name {
		parts = append(parts, "committed by "+clip(c.Committer.Name, 24))
	}
	for _, co := range c.CoAuthors {
		name, _, _ := strings.Cut(co, "<")
		if name = strings.TrimSpace(name); name != "" {
			parts = append(parts, "co-author "+clip(name, 24))
		}
	}
	return strings.Join(parts, ", ")
}

func via(c Commit) string {
	var v []string
	if len(c.Files) > 0 {
		var fs []string
		for _, f := range c.Files[:min(2, len(c.Files))] {
			if r := []rune(f); len(r) > 52 {
				f = "…" + string(r[len(r)-51:])
			}
			fs = append(fs, f)
		}
		v = append(v, strings.Join(fs, ", "))
	}
	if len(c.Terms) > 0 {
		v = append(v, "touches "+strings.Join(c.Terms[:min(3, len(c.Terms))], ", "))
	}
	if c.Message && len(c.Terms) == 0 {
		v = append(v, "message")
	}
	return strings.Join(v, "; ")
}

// subjectPtrs and evidencePtrs list every free text that came from the facts
// or from git, so one redactor call covers them all before layout.
func subjectPtrs(s *Subject) []*string {
	var p []*string
	for i := range s.Repos {
		p = append(p, &s.Repos[i])
	}
	for i := range s.Paths {
		p = append(p, &s.Paths[i])
	}
	for i := range s.Idents {
		p = append(p, &s.Idents[i])
	}
	return p
}

func commitPtrs(c *Commit) []*string {
	p := []*string{&c.Subject, &c.Author.Name, &c.Author.Email, &c.Committer.Name, &c.Committer.Email}
	for j := range c.CoAuthors {
		p = append(p, &c.CoAuthors[j])
	}
	for j := range c.Files {
		p = append(p, &c.Files[j])
	}
	for j := range c.Terms {
		p = append(p, &c.Terms[j])
	}
	for j := range c.Dropped {
		p = append(p, &c.Dropped[j])
	}
	return p
}

func evidencePtrs(ev *RepoEvidence) []*string {
	p := []*string{&ev.Name, &ev.Branch, &ev.HeadSubject}
	for i := range ev.Notes {
		p = append(p, &ev.Notes[i])
	}
	for i := range ev.Commits {
		p = append(p, commitPtrs(&ev.Commits[i])...)
	}
	for i := range ev.Changed {
		p = append(p, commitPtrs(&ev.Changed[i])...)
	}
	for i := range ev.Deleted {
		p = append(p, &ev.Deleted[i].Term)
		p = append(p, commitPtrs(&ev.Deleted[i].Commit)...)
	}
	for i := range ev.State {
		st := &ev.State[i]
		p = append(p, &st.Term)
		for j := range st.Where {
			p = append(p, &st.Where[j])
		}
	}
	return p
}

// redactPtrs masks the texts in place with one redactor call. A record
// separator inside a text would break the call's framing, so it is turned
// into a space first. The redactor keeps lengths, which is what makes it safe
// to clip afterwards.
func (b *Builder) redactPtrs(ctx context.Context, ptrs []*string) error {
	in := make([]string, len(ptrs))
	for i, p := range ptrs {
		in[i] = strings.ReplaceAll(*p, "\x1e", " ")
	}
	// The default redactor stops when the build's context ends, so an
	// abandoned build does not wait for scrump.
	var out []string
	var err error
	if b.Redact != nil {
		out, err = b.Redact(in)
	} else {
		out, err = digest.RedactAllContext(ctx, in)
	}
	if err != nil {
		return fmt.Errorf("gitev: redaction failed, no timeline: %w", err)
	}
	if len(out) != len(in) {
		return errors.New("gitev: redaction returned a different number of texts, no timeline")
	}
	for i, p := range ptrs {
		*p = out[i]
	}
	return nil
}
