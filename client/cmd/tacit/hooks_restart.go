package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/search"
	"github.com/agent-sh/tacit/client/internal/store"
)

// restart is how the harness started the session, by SessionStart's source:
// Claude Code and Codex send the same five values (startup, resume, clear,
// compact, fork).
type restart int

const (
	// restartNone: a fork, or a source tacit does not know. Nothing.
	restartNone restart = iota
	// restartFresh: a start (startup, or no source) or a clear. The context
	// holds no facts; the block is a start's.
	restartFresh
	// restartCompact: the compaction's summary took the context's place.
	restartCompact
	// restartResume: the session's old context is back; the store may have
	// moved since it was given its facts.
	restartResume
)

// startSources is the SessionStart matcher install writes: every source the
// hook answers (restartOf). legacyStartSources is the one earlier releases
// wrote; install widens its entry in place (Codex keys hook trust by
// position).
const (
	startSources       = "startup|resume|clear|compact"
	legacyStartSources = "startup"
)

func restartOf(source string) restart {
	switch source {
	case "", "startup", "clear":
		return restartFresh
	case "compact":
		return restartCompact
	case "resume":
		return restartResume
	}
	return restartNone
}

const (
	// compactTail is how much of the transcript's end the start after a
	// compaction reads for its summary: the harness writes the summary just
	// before it starts the session again, and Claude Code attaches the files
	// the session read after it.
	compactTail = 4 << 20
	// compactQueryChars is how much of the summary the query for its facts
	// reads.
	compactQueryChars = 12000
	// A fact the summary holds: its name, or at least heldShare of its
	// description's terms of four letters or more, and heldMinTerms of them.
	heldShare    = 0.6
	heldMinTerms = 3
)

// restartBlock is the start hook's block after a compaction (afterCompaction)
// and on a resume (onResume).
func restartBlock(ctx context.Context, ix *search.Index, s *store.Store, in hookInput, sit search.Situation, start []search.Hit, r restart, mcp bool) string {
	if r == restartCompact {
		return afterCompaction(ctx, ix, in, sit, start, mcp)
	}
	return onResume(ctx, ix, s, in, sit, start, mcp)
}

// afterCompaction: the compaction's summary is all the session has of what
// it was given, and it keeps a minority of the facts, fewer still of the
// owner's rules (eval/VALUE.md, "After a compaction"). So the block is the
// start block again, from the store as it is now, less the facts the summary
// still holds, then the facts the summary's own words select (the prompt
// hook's selection, local only). A fact given before may come again; one
// retired, replaced or held back since is in no query. The session's
// delivered set becomes what its context now holds: the facts the summary
// holds and this block. Without a summary (none found in the transcript's
// end) the block is the whole start block.
func afterCompaction(ctx context.Context, ix *search.Index, in hookInput, sit search.Situation, start []search.Hit, mcp bool) string {
	home, _ := os.UserHomeDir()
	summary := compactionSummary(in.TranscriptPath, home)
	sum := summaryOf(summary)
	held := map[string]bool{}
	if given := factNames(delivered(in.SessionID)); len(given) > 0 && summary != "" {
		if had, err := ix.Named(ctx, given); err == nil {
			for n, h := range had {
				if sum.holds(h) {
					held[n] = true
				}
			}
		}
	}
	var again []search.Hit
	for _, h := range start {
		if held[h.Name] || sum.holds(h) {
			held[h.Name] = true
			continue
		}
		again = append(again, h)
	}
	seen := map[string]bool{}
	for n := range held {
		seen[n] = true
	}
	for _, h := range again {
		seen[h.Name] = true
	}
	var pulled []search.Hit
	if q := clipRunes(summary, compactQueryChars); promptWorthy(q) {
		if hits, err := promptHitsWith(ctx, ix, sit, q, seen, true); err == nil {
			for _, h := range withoutClaims(hits) {
				if !seen[h.Name] {
					seen[h.Name] = true
					pulled = append(pulled, h)
				}
			}
		}
	}
	forget(in.SessionID)
	keep := make([]string, 0, len(held))
	for n := range held {
		keep = append(keep, n)
	}
	sort.Strings(keep)
	rememberNames(in.SessionID, keep)
	remember(in.SessionID, again)
	remember(in.SessionID, pulled)

	title := startTitleWhen(sit, mcp, ", again after the compaction")
	out := search.Block(again, title)
	if len(pulled) > 0 {
		if out == "" {
			out = title + "\n"
		}
		out += search.Block(pulled, "# tacit: facts matching the compaction's summary")
	}
	if out != "" {
		out += startTreeLine(ctx, ix, sit, len(again), mcp)
	}
	return out
}

// onResume: the session's context is back as it was, with the facts it was
// given, but the store may have moved. The block is the start block's facts
// the session was never given, and a line for each fact it was given that no
// hook would deliver now: retired, replaced (with the fact that replaced
// it), or held back by the reviewer's verdict. A session with no record of
// what it was given (none kept, or one cleaned away after a week) gets a
// start's block.
func onResume(ctx context.Context, ix *search.Index, s *store.Store, in hookInput, sit search.Situation, start []search.Hit, mcp bool) string {
	given := delivered(in.SessionID)
	if len(given) == 0 {
		remember(in.SessionID, start)
		return search.Block(start, startTitle(sit, mcp)) + startTreeLine(ctx, ix, sit, len(start), mcp)
	}
	var fresh []search.Hit
	for _, h := range start {
		if !given[h.Name] {
			fresh = append(fresh, h)
		}
	}
	shown := map[string]bool{}
	for _, h := range fresh {
		shown[h.Name] = true
	}
	lines, replacing := withdrawnLines(ctx, ix, s, factNames(given), given, shown)
	remember(in.SessionID, fresh)
	remember(in.SessionID, replacing)
	out := search.Block(fresh, "# tacit: new for "+repoTitle(sit)+" on "+sit.Host+" since this session was given its facts ("+openDetail(mcp)+")")
	if len(lines) > 0 {
		out += "# tacit: no longer holds, given to this session earlier\n" + strings.Join(lines, "\n") + "\n"
	}
	return out
}

// withdrawnLines is a line for each of names that no hook would deliver now
// (Index.Withdrawn), in name order: "- <name>: retired <day>", "- <name>:
// replaced by <new>" with the new fact's description when it is live and the
// session has not got it (that fact is returned, as delivered), or the
// reviewer's verdict (search.ReviewMark).
func withdrawnLines(ctx context.Context, ix *search.Index, s *store.Store, names []string, given, shown map[string]bool) ([]string, []search.Hit) {
	gone, err := ix.Withdrawn(ctx, names)
	if err != nil || len(gone) == 0 {
		return nil, nil
	}
	order := make([]string, 0, len(gone))
	for n := range gone {
		order = append(order, n)
	}
	sort.Strings(order)
	why := map[string]string{}
	by := map[string]string{}
	var next []string
	for _, n := range order {
		h := gone[n]
		if f := storeFact(s, h.Path); f != nil && f.Meta.SupersededBy != "" {
			by[n] = f.Meta.SupersededBy
			next = append(next, f.Meta.SupersededBy)
			continue
		} else if f != nil && f.Meta.Retired != "" {
			why[n] = "retired " + f.Meta.Retired
			continue
		}
		if m := search.ReviewMark(h.Review); m != "" {
			why[n] = m
		} else {
			why[n] = "no longer delivered"
		}
	}
	live := map[string]search.Hit{}
	if len(next) > 0 {
		named, err1 := ix.Named(ctx, next)
		dead, err2 := ix.Withdrawn(ctx, next)
		if err1 == nil && err2 == nil {
			for n, h := range named {
				if _, out := dead[n]; !out && !h.Inbox {
					live[n] = h
				}
			}
		}
	}
	var lines []string
	var replacing []search.Hit
	for _, n := range order {
		if b, ok := by[n]; ok {
			line := "- " + n + ": replaced by " + b
			if h, ok := live[b]; ok && !given[b] && !shown[b] {
				line += ": " + strings.Join(strings.Fields(h.Description), " ")
				shown[b] = true
				replacing = append(replacing, h)
			}
			lines = append(lines, line)
			continue
		}
		lines = append(lines, "- "+n+": "+why[n])
	}
	return lines, replacing
}

// storeFact reads the store's file at path, or nil (a local note's path, a
// file gone since the index was built).
func storeFact(s *store.Store, path string) *fact.Fact {
	if s == nil || path == "" || strings.HasPrefix(path, "local/") {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(s.Dir, filepath.FromSlash(path)))
	if err != nil {
		return nil
	}
	f, err := fact.Parse(path, b)
	if err != nil {
		return nil
	}
	return f
}

// compactionSummary is the summary the harness wrote for the session's last
// compaction, read from the end of its transcript (readTailN: a regular file
// under home), or "": no transcript, one of no known format, no summary.
func compactionSummary(path, home string) string {
	f, ok := transcriptFormat(home, path)
	if !ok {
		return ""
	}
	tail := readTailN(path, home, compactTail)
	s, _ := digest.CompactSummary([]byte(tail), f)
	return s
}

// summaryHold is what a compaction's summary holds, to tell whether it still
// carries a fact the session was given.
type summaryHold struct {
	text  string          // the summary, lower case
	terms map[string]bool // its terms, by search.TermKey
}

func summaryOf(summary string) summaryHold {
	h := summaryHold{text: strings.ToLower(summary), terms: map[string]bool{}}
	for _, t := range search.Terms(summary) {
		h.terms[search.TermKey(t)] = true
	}
	return h
}

// holds: the summary names the fact (its name, or the name's words in
// order), or holds most of what its description says: at least heldShare of
// the description's terms of four letters or more, and at least heldMinTerms
// of them.
func (s summaryHold) holds(h search.Hit) bool {
	if s.text == "" {
		return false
	}
	if strings.Contains(s.text, h.Name) || strings.Contains(s.text, strings.ReplaceAll(h.Name, "-", " ")) {
		return true
	}
	keys := map[string]bool{}
	for _, t := range search.Terms(h.Description) {
		if utf8.RuneCountInString(t) >= 4 {
			keys[search.TermKey(t)] = true
		}
	}
	if len(keys) < heldMinTerms {
		return false
	}
	n := 0
	for k := range keys {
		if s.terms[k] {
			n++
		}
	}
	return float64(n) >= heldShare*float64(len(keys))
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
