package ownerwords

import (
	"sort"
	"strings"
)

// A turn two sessions hold (SPEC.md, "Who typed a turn"): Build reads every
// session at once, so it settles what one transcript alone cannot.
//
//   - copies: one turn in two sessions. A fork, or a session resumed into a
//     new transcript, carries its turns over with their time, Claude Code
//     with their uuid too. The transcript turns of one text and one uuid, or,
//     without a uuid, of one instant to the millisecond, are one turn; so
//     is a turn a Claude Code fork carried over (older than the fork) and
//     another session's prompt history line of it. It stays in one session:
//     the one whose prompt history records it (the session the person typed
//     it in), else one whose transcript did not carry it over, else the
//     first read. In the other sessions its turns are left out, and so are
//     their history lines of it.
//   - casts: a transcript turn with the same text in another session within
//     BroadcastWindow. A person types a message into one session; the same
//     words in several at once are a program's, which may type into a
//     person's sessions as it does into its own. Each such turn is left out,
//     and so are the prompt history's lines of it. Only turns timed to the
//     millisecond (timed) make one: a history line's time is cut to the
//     second, and it may outlive its transcript while a fork's copy of the
//     turn lives on. A turn a fork carried over is a copy, never one.
//
// A prompt history line records a turn when its text is the turn's, or, at
// the turn's moment (BroadcastWindow), when one text holds the other on word
// boundaries (three words or more; the history keeps a slash command's name
// and an offered note's header that the turn's text leaves out) or the
// line's parts between its pastes' placeholders are in the turn in order
// (the transcript keeps a short paste in the turn).

// timed reports a turn whose source records its time to the millisecond: a
// transcript's line (Claude Code, Codex, pi). The prompt histories and
// Hermes record it to the second, and a digest not at all.
func (b *Builder) timed(i int) bool {
	return b.ms[i] != 0 && (b.sources[i] == "transcript" || b.sources[i] == "pi")
}

// history reports a prompt history's line.
func (b *Builder) history(i int) bool {
	return b.sources[i] == "claude-history" || b.sources[i] == "codex-history"
}

// sessionless reports a prompt history line that names no session.
func sessionless(ref string) bool { return strings.HasSuffix(ref, ":history") }

// near reports two times known and at most BroadcastWindow apart.
func near(x, y int64) bool {
	d := x - y
	return x != 0 && y != 0 && d <= BroadcastWindow.Milliseconds() && d >= -BroadcastWindow.Milliseconds()
}

// histories indexes the prompt history lines by session: in time order, and
// by text.
type histories struct {
	b      *Builder
	norm   []string
	all    []int                       // every line, by ms
	byTime map[string][]int            // a session -> its lines, by ms
	byText map[string]map[string][]int // a session -> a text -> its lines
}

func (b *Builder) historiesOf(norm []string) *histories {
	h := &histories{b: b, norm: norm, byTime: map[string][]int{}, byText: map[string]map[string][]int{}}
	for i := range b.c.raw {
		if !b.history(i) {
			continue
		}
		r := b.c.refs[i]
		h.all = append(h.all, i)
		h.byTime[r] = append(h.byTime[r], i)
		if h.byText[r] == nil {
			h.byText[r] = map[string][]int{}
		}
		h.byText[r][norm[i]] = append(h.byText[r][norm[i]], i)
	}
	byMS := func(xs []int) { sort.SliceStable(xs, func(x, y int) bool { return b.ms[xs[x]] < b.ms[xs[y]] }) }
	byMS(h.all)
	for _, xs := range h.byTime {
		byMS(xs)
	}
	return h
}

// near is the history lines of session ref within BroadcastWindow of ms;
// with ref "", of any session.
func (h *histories) near(ref string, ms int64) []int {
	xs := h.byTime[ref]
	if ref == "" {
		xs = h.all
	}
	w := BroadcastWindow.Milliseconds()
	lo := sort.Search(len(xs), func(k int) bool { return h.b.ms[xs[k]] >= ms-w })
	var out []int
	for k := lo; k < len(xs) && h.b.ms[xs[k]] <= ms+w; k++ {
		out = append(out, xs[k])
	}
	return out
}

// recording is the history lines of session ref that record turn: of its
// text at any time, else, at its moment, of a text the rule above matches.
func (h *histories) recording(ref string, turn int) []int {
	out := append([]int(nil), h.byText[ref][h.norm[turn]]...)
	for _, x := range h.near(ref, h.b.ms[turn]) {
		if h.norm[x] != h.norm[turn] && h.records(x, turn) {
			out = append(out, x)
		}
	}
	return out
}

// records reports the history line x a record of turn, at its moment.
func (h *histories) records(x, turn int) bool {
	nx, nt := h.norm[x], h.norm[turn]
	if nx == nt {
		return true
	}
	if !near(h.b.ms[x], h.b.ms[turn]) {
		return false
	}
	if holdsWords(nt, nx) || holdsWords(nx, nt) {
		return true
	}
	parts := h.b.parts[x]
	if len(parts) < 2 {
		return false
	}
	rest := " " + nt + " "
	for _, part := range parts {
		k := strings.Index(rest, " "+part+" ")
		if k < 0 {
			return false
		}
		rest = rest[k+1+len(part):] // from the space after the part
	}
	return true
}

// holdsWords reports text holding words, three words or more, on word
// boundaries (both normalized).
func holdsWords(text, words string) bool {
	return len(strings.Fields(words)) >= 3 && strings.Contains(" "+text+" ", " "+words+" ")
}

// twoSessions finds the copies and the casts among the turns (above).
func (b *Builder) twoSessions() (copies, casts map[int]bool) {
	copies, casts = map[int]bool{}, map[int]bool{}
	norm := make([]string, len(b.c.raw))
	byText := map[string][]int{}
	for i, raw := range b.c.raw {
		norm[i] = Normalize(raw)
		if b.timed(i) {
			byText[norm[i]] = append(byText[norm[i]], i)
		}
	}
	hs := b.historiesOf(norm)
	for _, idx := range byText {
		b.copiesOf(idx, hs, copies)
	}
	for _, idx := range byText {
		b.castsOf(idx, hs, copies, casts)
	}
	return copies, casts
}

// copiesOf marks the copies of idx, the timed turns of one text in the order
// they were read.
func (b *Builder) copiesOf(idx []int, hs *histories, copies map[int]bool) {
	byUUID, byMS := map[string]int{}, map[int64]int{}
	var groups []int // each group's first turn, in read order
	members := map[int][]int{}
	for _, i := range idx {
		g, ok := -1, false
		if id := b.uuids[i]; id != "" {
			g, ok = byUUID[id]
			if !ok {
				byUUID[id] = i
			}
		} else {
			g, ok = byMS[b.ms[i]]
			if !ok {
				byMS[b.ms[i]] = i
			}
		}
		if !ok {
			g = i
			groups = append(groups, g)
		}
		members[g] = append(members[g], i)
	}
	for _, g := range groups {
		ms := members[g]
		refs := map[string]bool{}
		for _, m := range ms {
			refs[b.c.refs[m]] = true
		}
		// A turn a fork carried over is a copy of another session's turn
		// its history records, whether or not that session's transcript is
		// still here.
		if len(refs) == 1 && b.older[g] {
			for _, x := range hs.near("", b.ms[g]) {
				if r := b.c.refs[x]; !refs[r] && !sessionless(r) && hs.records(x, g) {
					refs[r] = true
				}
			}
		}
		if len(refs) < 2 {
			continue
		}
		keep := ""
		for _, r := range sortedRefs(refs, b, ms) {
			if len(hs.recording(r, g)) > 0 {
				keep = r // the session the person typed it in
				break
			}
		}
		for _, m := range ms {
			if keep == "" && !b.older[m] {
				keep = b.c.refs[m]
			}
		}
		if keep == "" {
			keep = b.c.refs[g]
		}
		for _, m := range ms {
			if b.c.refs[m] != keep {
				copies[m] = true
			}
		}
		for r := range refs {
			if r == keep {
				continue
			}
			for _, x := range hs.recording(r, g) {
				copies[x] = true
			}
		}
	}
}

// sortedRefs are refs in the order their turns were read (ms), then the
// rest by name: which session's history is asked first.
func sortedRefs(refs map[string]bool, b *Builder, ms []int) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range ms {
		if r := b.c.refs[m]; refs[r] && !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	var rest []string
	for r := range refs {
		if !seen[r] {
			rest = append(rest, r)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

// castsOf marks the broadcasts among idx, less its copies and the turns a
// fork carried over.
func (b *Builder) castsOf(idx []int, hs *histories, copies, casts map[int]bool) {
	var timed []int
	for _, i := range idx {
		if !copies[i] && !b.older[i] {
			timed = append(timed, i)
		}
	}
	sort.SliceStable(timed, func(x, y int) bool { return b.ms[timed[x]] < b.ms[timed[y]] })
	for x, i := range timed {
		for _, j := range timed[x+1:] {
			if !near(b.ms[i], b.ms[j]) {
				break
			}
			if b.c.refs[i] != b.c.refs[j] {
				casts[i], casts[j] = true, true
			}
		}
	}
	for _, m := range timed {
		if !casts[m] {
			continue
		}
		for _, r := range []string{b.c.refs[m], strings.SplitN(b.c.refs[m], ":", 2)[0] + ":history"} {
			for _, x := range hs.near(r, b.ms[m]) {
				if !copies[x] && hs.records(x, m) {
					casts[x] = true
				}
			}
		}
	}
}
