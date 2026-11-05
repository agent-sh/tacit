package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/overlay"
	"github.com/agent-sh/tacit/client/internal/record"
	"github.com/agent-sh/tacit/client/internal/search"
	"github.com/agent-sh/tacit/client/internal/store"
)

// restart is how the harness started the session, by SessionStart's source:
// Claude Code and Codex send the same five values (startup, resume, clear,
// compact, fork).
type restart int

const (
	// restartNone: a source tacit does not know. Nothing.
	restartNone restart = iota
	// restartFresh: a start (startup, or no source) or a clear. The context
	// holds no facts; the block is a start's.
	restartFresh
	// restartCompact: the compaction's summary took the context's place.
	restartCompact
	// restartResume: the session's old context is back (a resume), or a
	// copy of another session's (a fork); the store may have moved since it
	// was given its facts.
	restartResume
)

// startSources is the SessionStart matcher install writes: every source the
// hook answers (restartOf). legacyStartSources are the ones earlier builds
// wrote; install widens such an entry in place (Codex keys hook trust by
// position).
const startSources = "startup|resume|clear|compact|fork"

var legacyStartSources = []string{"startup", "startup|resume|clear|compact"}

func restartOf(source string) restart {
	switch source {
	case "", "startup", "clear":
		return restartFresh
	case "compact":
		return restartCompact
	case "resume", "fork":
		// A fork is a new session with the old one's context and no
		// delivered set of its own: a start's block, as a resume with no
		// record. The hook is not told which session it forked.
		return restartResume
	}
	return restartNone
}

const (
	// compactRead is how much of the transcript the summary is read from:
	// its end at the start after a compaction, or what came after the place
	// the transcript ended then, at the next prompt.
	compactRead = 4 << 20
	// summaryQueryTerms is how many of the summary's terms its query takes:
	// the prompt query's own cap (search.Terms).
	summaryQueryTerms = 24
	// A fact the summary holds: its name, or at least heldShare of its
	// description's terms of four letters or more, and heldMinTerms of them.
	heldShare    = 0.6
	heldMinTerms = 3
)

// restartBlock is the start hook's block after a compaction (afterCompaction)
// and on a resume (onResume).
func restartBlock(ctx context.Context, ix *search.Index, s *store.Store, in hookInput, sit search.Situation, recs map[string]*record.Record, start []search.Hit, r restart, mcp bool) string {
	if r == restartCompact {
		return afterCompaction(ctx, ix, in, sit, recs, start, mcp)
	}
	return onResume(ctx, ix, s, in, sit, start, mcp)
}

// afterCompaction: the compaction's summary is all the session has of what
// it was given, and it keeps a minority of the facts, fewer still of the
// owner's rules (eval/VALUE.md, "After a compaction"). The block is the start
// block again, from the store as it is now, then the facts the summary's own
// words select (summaryStep). A fact given before may come again; one
// retired, replaced or held back since is in no query.
//
// Claude Code runs the SessionStart hooks of a compaction before it writes
// the summary to the transcript. So the summary is taken here only when the
// transcript ends with it (no turn after it: a harness that writes it first);
// else the block is the whole start block, and the next prompt takes the
// summary from what the transcript gained since (pendingSummary). Either
// way the session's delivered set becomes what its context holds.
func afterCompaction(ctx context.Context, ix *search.Index, in hookInput, sit search.Situation, recs map[string]*record.Record, start []search.Hit, mcp bool) string {
	home, _ := os.UserHomeDir()
	given := factNames(delivered(in.SessionID))
	if old, ok := loadPending(in.SessionID); ok {
		// A compaction before the last one's first prompt: what the session
		// had been given before that one counts too.
		given = mergeNames(given, old.Given)
	}
	summary, size := "", int64(-1)
	if f, ok := transcriptFormat(home, in.TranscriptPath); ok {
		var tail string
		tail, size = readRange(in.TranscriptPath, home, -1, compactRead)
		if s, after, ok := digest.LastCompaction([]byte(tail), f); ok && !after {
			summary = s
		}
	}
	forget(in.SessionID)
	clearPending(in.SessionID)
	title := startTitleWhen(sit, mcp, ", again after the compaction")
	if summary == "" {
		logChose(start, "again")
		endLog(start)
		remember(in.SessionID, start)
		if size >= 0 {
			savePending(in.SessionID, pending{Offset: size, Given: given})
		}
		out := search.Block(start, title)
		if out != "" {
			out += startTreeLine(ctx, ix, sit, len(start), mcp)
		}
		return out
	}
	sum := summaryOf(summary)
	held, _ := heldOf(ctx, ix, sum, given)
	wasGiven := map[string]bool{}
	for _, n := range given {
		wasGiven[n] = true
	}
	var again []search.Hit
	for _, h := range start {
		// A fact the session was never given is in the summary only by its
		// name: a summary about the repo's work says its facts' words. One
		// it was given is also held as the start block has it (a name two
		// files hold: heldOf read the name's first file).
		if held[h.Name] || sum.named(h) || wasGiven[h.Name] && sum.holds(h) {
			held[h.Name] = true
			logDrop(h, "held")
			continue
		}
		again = append(again, h)
	}
	logChose(again, "again")
	endLog(again)
	rememberNames(in.SessionID, factNames(held))
	remember(in.SessionID, again)
	pulled := summaryStep(ctx, ix, in, sit, recs, summary)
	out := search.Block(again, title)
	if len(pulled) > 0 {
		if out == "" {
			out = title + "\n"
		}
		out += search.Block(pulled, summaryTitle)
	}
	if out != "" {
		out += startTreeLine(ctx, ix, sit, len(again), mcp)
	}
	return out
}

const summaryTitle = "# tacit: facts matching the compaction's summary"

// summaryStep is the facts the summary's words select, which the session
// has not got: the prompt hook's selection, local only, never the utility
// provider's (a summary holds far more of the session than a prompt), with
// the summary's query (summaryQuery) as the request and the scopes a request
// reaches (withPromptScopes: the topics the summary names, the session's
// orgs). They join the delivered set.
func summaryStep(ctx context.Context, ix *search.Index, in hookInput, sit search.Situation, recs map[string]*record.Record, summary string) []search.Hit {
	q := summaryQuery(ctx, ix, summary)
	if !promptWorthy(q) {
		return nil
	}
	sit = withPromptScopes(sit, recs, summary)
	seen := delivered(in.SessionID)
	beginLog("summary", in, q, repoNames(sit), ix)
	hits, err := promptHitsWith(ctx, ix, sit, q, seen, true)
	if err != nil {
		endLog(nil)
		return nil
	}
	var out []search.Hit
	kept := withoutClaims(hits)
	logDropped(hits, kept, "withheld")
	for _, h := range kept {
		if !seen[h.Name] {
			seen[h.Name] = true
			out = append(out, h)
		}
	}
	logDropped(kept, out, "seen") // a second file of a name already given
	endLog(out)
	remember(in.SessionID, out)
	return out
}

// pending is what the start after a compaction leaves for the next prompt
// when the transcript did not hold the summary yet: where the transcript
// ended then, and the facts the session had been given before the compaction.
type pending struct {
	Offset int64    `json:"offset"`
	Given  []string `json:"given"`
}

func pendingFile(id string) string {
	if p := sessionFile(id); p != "" {
		return p + ".compact"
	}
	return ""
}

// heldFile marks that a PreToolUse push read the record's summary and put
// the facts it holds of Given in the delivered set (pendingHeld). It is a
// file of its own, made once (O_EXCL), so a push never rewrites the record
// a prompt may be reading or removing.
func heldFile(id string) string {
	if p := pendingFile(id); p != "" {
		return p + ".held"
	}
	return ""
}

// savePending writes the record whole or not at all (a temporary file
// renamed over it), so a prompt never reads it half written.
func savePending(id string, p pending) {
	path := pendingFile(id)
	if path == "" {
		return
	}
	b, _ := json.Marshal(p)
	os.MkdirAll(filepath.Dir(path), 0o700)
	tmp := path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, path)
	}
}

func loadPending(id string) (pending, bool) {
	var p pending
	path := pendingFile(id)
	if path == "" {
		return p, false
	}
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &p) != nil {
		return p, false
	}
	return p, true
}

func mergeNames(a, b []string) []string {
	out := slices.Concat(a, b)
	slices.Sort(out)
	return slices.Compact(out)
}

func clearPending(id string) {
	if path := pendingFile(id); path != "" {
		os.Remove(path)
		os.Remove(heldFile(id))
	}
}

// pendingSummary is the prompt hook's part after a compaction whose summary
// the start could not read: the first prompt after it reads the summary from
// what the transcript gained since the place it ended at the start, adds the
// facts given before the compaction that the summary holds to the delivered
// set, and returns the block of the facts the summary's words select. The
// compaction's entries are written before the next prompt, so the first
// prompt settles it: pending is dropped whatever it finds.
func pendingSummary(ctx context.Context, ix *search.Index, in hookInput, sit search.Situation, recs map[string]*record.Record) string {
	path := pendingFile(in.SessionID)
	if path == "" {
		return ""
	}
	p, ok := loadPending(in.SessionID)
	heldDone := heldMarked(in.SessionID, p)
	os.Remove(path)
	os.Remove(heldFile(in.SessionID))
	if !ok || p.Offset < 0 {
		return ""
	}
	summary, ok := pendingText(in, p)
	if !ok {
		return ""
	}
	if !heldDone {
		_ = rememberHeld(ctx, ix, in, summaryOf(summary), p.Given)
	}
	return search.Block(summaryStep(ctx, ix, in, sit, recs, summary), summaryTitle)
}

// pendingHeld is the PreToolUse hook's part of a compaction whose summary
// the start could not read: once the transcript holds the summary, the facts
// given before the compaction that it holds join the delivered set, so a
// push before the next prompt does not give them again. The record stays
// for that prompt, which gives the summary's selection (pendingSummary).
func pendingHeld(ctx context.Context, ix *search.Index, in hookInput) {
	marker := heldFile(in.SessionID)
	if marker == "" || ctx.Err() != nil {
		return
	}
	p, ok := loadPending(in.SessionID)
	if !ok || p.Offset < 0 || heldMarked(in.SessionID, p) {
		return
	}
	summary, ok := pendingText(in, p)
	if !ok {
		return
	}
	// A lookup the push's deadline cut makes no marker: the next push, or
	// the prompt, tries again.
	if rememberHeld(ctx, ix, in, summaryOf(summary), p.Given) != nil || ctx.Err() != nil {
		return
	}
	if f, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600); err == nil {
		fmt.Fprint(f, p.Offset)
		f.Close()
	}
}

// heldMarked reports a marker made for this record: it holds the record's
// offset, so a marker a push made for an earlier compaction's record (made
// while the next one's start replaced it) does not count for this one.
func heldMarked(id string, p pending) bool {
	b, err := os.ReadFile(heldFile(id))
	return err == nil && strings.TrimSpace(string(b)) == strconv.FormatInt(p.Offset, 10)
}

// pendingText is the summary of the compaction a record speaks of: the last
// one in what the transcript gained since the place the record holds.
func pendingText(in hookInput, p pending) (string, bool) {
	home, _ := os.UserHomeDir()
	f, ok := transcriptFormat(home, in.TranscriptPath)
	if !ok {
		return "", false
	}
	gained, size := readRange(in.TranscriptPath, home, p.Offset, compactRead)
	if size < p.Offset {
		return "", false // the transcript was cut or replaced
	}
	summary, _, ok := digest.LastCompaction([]byte(gained), f)
	return summary, ok
}

// rememberHeld puts the facts of given that the summary holds in the
// delivered set; an error when the lookup failed.
func rememberHeld(ctx context.Context, ix *search.Index, in hookInput, sum summaryHold, given []string) error {
	held, err := heldOf(ctx, ix, sum, given)
	if err != nil {
		return err
	}
	seen := delivered(in.SessionID)
	var keep []string
	for n := range held {
		if !seen[n] {
			keep = append(keep, n)
		}
	}
	sort.Strings(keep)
	rememberNames(in.SessionID, keep)
	return nil
}

// heldOf is, of the names the session was given, those whose facts the
// summary holds: still in its context. An error when the lookup failed.
func heldOf(ctx context.Context, ix *search.Index, sum summaryHold, given []string) (map[string]bool, error) {
	held := map[string]bool{}
	if len(given) == 0 || sum.text == "" {
		return held, nil
	}
	had, err := ix.Named(ctx, given)
	if err != nil {
		return held, err
	}
	for n, h := range had {
		if sum.holds(h) {
			held[n] = true
		}
	}
	return held, nil
}

// summaryQuery is the summary as a query of summaryQueryTerms terms: the
// prompt query takes a request's first terms, and a summary opens with the
// harness's own words. Each term live curated names and descriptions hold
// is weighted by how often the summary says it, damped (1 + ln tf: a long
// text repeats its common words), and how few facts hold it (log((N+1)/(df+1)),
// the relationship evidence's weight), and the heaviest make the query, in
// the summary's order. Names and descriptions are kept as written, and a
// summary says their words in other forms ("wombats"), so a term is looked
// up as written, as its stem and without a plural or past ending, and terms
// count by stem: one term of the query per stem, said as often as all its
// forms are. A term no fact holds in any of those forms is left out: it
// cannot match a name or a description.
func summaryQuery(ctx context.Context, ix *search.Index, summary string) string {
	terms := search.AllTerms(summary)
	counts := search.TermCounts(summary)
	var look []string
	for _, t := range terms {
		look = append(look, wordForms(t)...)
	}
	df, n, err := ix.SummaryFrequency(ctx, mergeNames(look, nil))
	if err != nil || len(df) == 0 {
		return ""
	}
	byStem := map[string]int{}
	for tok, c := range df {
		k := search.TermKey(tok)
		byStem[k] = max(byStem[k], c)
	}
	type scored struct {
		term  string
		at    int
		tf    int
		score float64
	}
	var all []*scored
	first := map[string]*scored{}
	for i, t := range terms {
		k := search.TermKey(t)
		d, ok := byStem[k]
		if !ok {
			continue
		}
		if sc, seen := first[k]; seen {
			sc.tf += counts[t]
			continue
		}
		sc := &scored{term: t, at: i, tf: counts[t]}
		sc.score = math.Log(float64(n+1) / float64(d+1))
		first[k] = sc
		all = append(all, sc)
	}
	for _, sc := range all {
		sc.score *= 1 + math.Log(float64(max(1, sc.tf)))
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].score > all[j].score })
	if len(all) > summaryQueryTerms {
		all = all[:summaryQueryTerms]
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at < all[j].at })
	words := make([]string, len(all))
	for i, sc := range all {
		words[i] = sc.term
	}
	return strings.Join(words, " ")
}

// wordForms are the spellings of t a name or a description may hold: as
// written, its stem, and without a plural or past ending.
func wordForms(t string) []string {
	out := []string{t, search.TermKey(t)}
	for _, end := range []string{"ies", "es", "s", "ed", "ing"} {
		if base, ok := strings.CutSuffix(t, end); ok && utf8.RuneCountInString(base) >= 3 {
			if end == "ies" {
				base += "y"
			}
			out = append(out, base)
		}
	}
	return out
}

// onResume: the session's context is back as it was, with the facts it was
// given, but the store may have moved. The block is the start block's facts
// the session was never given, and a line for each fact it was given that is
// now retired, replaced (naming the fact that replaced it), or held back by
// the reviewer's verdict. A session with no record of what it was given (none
// kept, or one cleaned away after a week) gets a start's block.
func onResume(ctx context.Context, ix *search.Index, s *store.Store, in hookInput, sit search.Situation, start []search.Hit, mcp bool) string {
	given := delivered(in.SessionID)
	if len(given) == 0 {
		logChose(start, "start")
		endLog(start)
		remember(in.SessionID, start)
		return search.Block(start, startTitle(sit, mcp)) + startTreeLine(ctx, ix, sit, len(start), mcp)
	}
	var fresh []search.Hit
	for _, h := range start {
		if !given[h.Name] {
			fresh = append(fresh, h)
		} else {
			logDrop(h, "seen")
		}
	}
	logChose(fresh, "resume")
	endLog(fresh)
	remember(in.SessionID, fresh)
	out := search.Block(fresh, "# tacit: new for "+repoTitle(sit)+" on "+sit.Host+" since this session was given its facts ("+openDetail(mcp)+")")
	if lines, gone := withdrawnLines(ctx, ix, s, factNames(given)); len(lines) > 0 {
		out += "# tacit: no longer holds, given to this session earlier (" + openDetail(mcp) + ")\n" + strings.Join(lines, "\n") + "\n"
		unremember(in.SessionID, gone) // said once: the next resume does not say it again
	}
	return out
}

// unremember drops names from the session's delivered set.
func unremember(id string, names []string) {
	set := delivered(id)
	if len(set) == 0 || len(names) == 0 {
		return
	}
	for _, n := range names {
		delete(set, n)
	}
	keep := make([]string, 0, len(set))
	for n := range set {
		keep = append(keep, n)
	}
	sort.Strings(keep)
	forget(id)
	rememberNames(id, keep)
}

// withdrawnLines is a line for each of names whose facts no hook would
// deliver now (Index.Withdrawn), in name order: "- <name>: retired <day>",
// "- <name>: replaced by <new>", or the reviewer's verdict
// (search.ReviewMark); "retired or replaced" when the file cannot be read.
// The new fact is only named: whether a hook would deliver it is its own
// question. The names the lines are for are returned with them.
func withdrawnLines(ctx context.Context, ix *search.Index, s *store.Store, names []string) ([]string, []string) {
	gone, err := ix.Withdrawn(ctx, names)
	if err != nil || len(gone) == 0 {
		return nil, nil
	}
	order := make([]string, 0, len(gone))
	for n := range gone {
		order = append(order, n)
	}
	sort.Strings(order)
	var lines []string
	for _, n := range order {
		h := gone[n]
		why := "retired or replaced"
		switch f := factAt(s, h.Path); {
		case f != nil && f.Meta.SupersededBy != "" && ix.Hidden(f.Meta.SupersededBy):
			why = "replaced" // its successor does not exist in the session's view
		case f != nil && f.Meta.SupersededBy != "":
			why = "replaced by " + f.Meta.SupersededBy
		case f != nil && f.Meta.Retired != "":
			why = "retired " + f.Meta.Retired
		case search.ReviewMark(h.Review) != "":
			why = search.ReviewMark(h.Review)
		}
		lines = append(lines, "- "+n+": "+why)
	}
	return lines, order
}

// factAt reads the file a hit names: a store path in the clone, a local
// note's path (overlay.Prefix) in this machine's overlay; nil when it cannot
// be read.
func factAt(s *store.Store, path string) *fact.Fact {
	if s == nil || path == "" {
		return nil
	}
	full := filepath.Join(s.Dir, filepath.FromSlash(path))
	if rest, ok := strings.CutPrefix(path, overlay.Prefix); ok {
		full = filepath.Join(overlay.Dir(), filepath.FromSlash(rest))
	}
	b, err := os.ReadFile(full)
	if err != nil {
		return nil
	}
	f, err := fact.Parse(path, b)
	if err != nil {
		return nil
	}
	return f
}

// summaryHold is what a compaction's summary holds, to tell whether it still
// carries a fact the session was given.
type summaryHold struct {
	text   string            // the summary, lower case
	pieces []map[string]bool // the terms of each of its sentences and lines, by search.TermKey
}

// pieceEnd ends a sentence or a line of a summary; a full stop inside a word
// (a file name, a version) does not.
var pieceEnd = regexp.MustCompile(`\n|[.!?;](?:\s|$)`)

func summaryOf(summary string) summaryHold {
	h := summaryHold{text: strings.ToLower(summary)}
	for _, piece := range pieceEnd.Split(summary, -1) {
		keys := map[string]bool{}
		for _, t := range search.AllTerms(piece) {
			keys[search.TermKey(t)] = true
		}
		if len(keys) > 0 {
			h.pieces = append(h.pieces, keys)
		}
	}
	return h
}

// named: the summary names the fact, by its name as a whole name, or for a
// name of three words or more by its words in order as whole words.
func (s summaryHold) named(h search.Hit) bool {
	if s.text == "" {
		return false
	}
	if wordIn(s.text, h.Name, nameRune) {
		return true
	}
	words := strings.Split(h.Name, "-")
	return len(words) >= 3 && wordIn(s.text, strings.Join(words, " "), wordRune)
}

// holds: the summary names the fact (its name as a whole name, or for a
// name of three words or more its words in order as whole words), or, for a
// fact that is no ruling, one of its sentences or lines holds most of what
// the description says: at least heldShare of the description's terms of
// four letters or more, and at least heldMinTerms of them. A long summary
// holds most words somewhere, and a rule's words are the work's words: a
// ruling counts as held only by its name.
func (s summaryHold) holds(h search.Hit) bool {
	if s.named(h) {
		return true
	}
	if s.text == "" || h.Ruling {
		return false
	}
	keys := map[string]bool{}
	for _, t := range search.AllTerms(h.Description) {
		if utf8.RuneCountInString(t) >= 4 {
			keys[search.TermKey(t)] = true
		}
	}
	if len(keys) < heldMinTerms {
		return false
	}
	need := max(heldMinTerms, int(math.Ceil(heldShare*float64(len(keys)))))
	for _, piece := range s.pieces {
		n := 0
		for k := range keys {
			if piece[k] {
				n++
			}
		}
		if n >= need {
			return true
		}
	}
	return false
}

func wordRune(r rune) bool { return r >= 'a' && r <= 'z' || r >= '0' && r <= '9' }
func nameRune(r rune) bool { return wordRune(r) || r == '-' }

// wordIn reports whether text holds w with no rune of in on either side:
// "demo-note" is not in "demo-note-v2", "go test" not in "go testing".
func wordIn(text, w string, in func(rune) bool) bool {
	for i := 0; w != ""; {
		j := strings.Index(text[i:], w)
		if j < 0 {
			return false
		}
		at := i + j
		before, _ := utf8.DecodeLastRuneInString(text[:at])
		after, _ := utf8.DecodeRuneInString(text[at+len(w):])
		if (at == 0 || !in(before)) && (at+len(w) == len(text) || !in(after)) {
			return true
		}
		i = at + 1
	}
	return false
}

// factNames are the fact names of a delivered set, sorted: the set also
// records the areas pushed (areaSeen), which are no facts.
func factNames(set map[string]bool) []string {
	var out []string
	for n := range set {
		if !strings.Contains(n, ":") {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}
