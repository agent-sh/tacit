package ownerwords

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"index/suffixarray"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/ledger"
)

// Owner sessions and turns are decided in one place, digest.SessionWho and
// digest.ClaudeTyped (digest/who.go); the dreamer reads the same verdicts.
// The names below are kept for callers of this package.
var (
	ClaudeOwnerEntrypoints = digest.ClaudeOwnerEntrypoints
	CodexOwnerOriginators  = digest.CodexOwnerOriginators
)

// Source is a transcript glob in one dialect.
type Source struct {
	Glob   string
	Format digest.Format
}

// DefaultSources are the transcripts of this machine: Claude Code projects
// (top-level sessions only; subagent transcripts are agents talking to
// agents), Codex sessions and pi sessions (top-level only: the glob,
// digest.PiSessionGlob, does not reach a subagent's <session>/tasks/).
// Hermes keeps its sessions in a database (AddHermes).
func DefaultSources(home string) []Source {
	return []Source{
		{Glob: filepath.Join(home, ".claude", "projects", "*", "*.jsonl"), Format: digest.Claude},
		{Glob: filepath.Join(home, ".codex", "sessions", "*", "*", "*", "*.jsonl"), Format: digest.Codex},
		{Glob: filepath.Join(home, ".codex", "sessions", "*", "*.jsonl"), Format: digest.Codex},
		{Glob: digest.PiSessionGlob(home), Format: digest.Pi},
	}
}

// DefaultDigests is where eval/mine/digest.py writes session digests.
func DefaultDigests(home string) string { return filepath.Join(home, ".cache", "tacit-mine") }

// Corpus is the owner's own turns, normalized for lookup and kept as the owner wrote
// them for the context of a match (a question, a negation).
type Corpus struct {
	data   []byte // "\x00 turn \x00 turn ..." (each normalized turn padded with spaces)
	starts []int  // offset of each turn's text in data
	refs   []string
	raw    []string // each turn as the owner wrote it
	at     []int64  // when each turn was written (Unix seconds), 0 when its source does not say
	sa     *suffixarray.Index
	turns  map[string][]int // ref -> turn indexes, for the session a caller prefers

	Stats Stats
}

// Stats says what the corpus was built from.
type Stats struct {
	OwnerSessions   int            `json:"owner_sessions"`
	OwnerTurns      int            `json:"owner_turns"`
	Turns           map[string]int `json:"turns"`            // owner turns by source: transcript (Claude Code, Codex), pi, hermes, digest, claude-history, codex-history
	AgentSessions   int            `json:"agent_sessions"`   // headless, SDK, runner or scratch sessions, skipped
	UnknownSessions int            `json:"unknown_sessions"` // no way to tell who started it (a digest whose transcript is gone), skipped
	AgentTurns      int            `json:"agent_turns"`      // turns in their sessions that an agent or a harness wrote, skipped
	RelayedLines    int            `json:"relayed_lines"`    // lines of agent text inside their turns (RelayedLines), skipped
}

// Builder collects turns.
type Builder struct {
	c     *Corpus
	buf   bytes.Buffer
	known map[string]bool
	// agentRefs are the sessions whose transcripts are an agent's: a prompt
	// history line of one of them (a lane a program typed into) is not theirs.
	agentRefs map[string]bool
	// pastes are texts the owner pasted into a prompt (the prompt history keeps
	// them apart); a transcript inlines them into their turn, and a paste is
	// often an agent's output the owner passed along.
	pastes []string
}

// NewBuilder starts an empty corpus.
func NewBuilder() *Builder {
	return &Builder{c: &Corpus{turns: map[string][]int{}, Stats: Stats{Turns: map[string]int{}}}, known: map[string]bool{}, agentRefs: map[string]bool{}}
}

// Has reports whether a session is already in the corpus.
func (b *Builder) Has(ref string) bool { return b.known[ref] }

// Add puts one of the owner's turns, read from source, in the corpus under a
// session ref ("claude:<session>", "codex:<session>", "pi:<session>",
// "hermes:<session>"). Text an
// agent or a harness injected (AgentInjected) is left out, and so is text the owner
// pasted.
func (b *Builder) Add(source, ref, text string) { b.AddAt(source, ref, text, time.Time{}) }

// AddAt is Add for a turn whose source records when it was written: the
// day a statement of theirs was said is that turn's (SaidAt), never the day
// a step read it.
func (b *Builder) AddAt(source, ref, text string, at time.Time) {
	text, ok := OwnerPart(text)
	if !ok {
		b.c.Stats.AgentTurns++
		return
	}
	for _, p := range b.pastes {
		if strings.Contains(text, p) {
			text = strings.ReplaceAll(text, p, " ")
		}
	}
	text, dropped := dropRelayed(text)
	b.c.Stats.RelayedLines += dropped
	n := Normalize(text)
	if n == "" {
		return
	}
	if !b.known[ref] {
		b.known[ref] = true
		b.c.Stats.OwnerSessions++
	}
	b.c.Stats.OwnerTurns++
	b.c.Stats.Turns[source]++
	b.buf.WriteString("\x00 ")
	b.c.turns[ref] = append(b.c.turns[ref], len(b.c.starts))
	b.c.starts = append(b.c.starts, b.buf.Len())
	b.c.refs = append(b.c.refs, ref)
	b.c.raw = append(b.c.raw, text)
	var sec int64
	if !at.IsZero() && at.Unix() > 0 {
		sec = at.Unix()
	}
	b.c.at = append(b.c.at, sec)
	b.buf.WriteString(n)
	b.buf.WriteString(" ")
}

// Agent text the owner relayed inside their own turn, with no paste record (a lane's
// report, a chat model's answer): the owner types plain lowercase lines in a
// terminal, an agent writes markdown. A line is left out of their turn when it
// is a markdown header, holds a backtick or "**", or is a bullet or numbered
// item that opens with a capital, a backtick, "*" or "["; and so is every line
// of a block of three or more capitalized prose lines (eight words or more,
// ending in ".", ":" or ";"). Measured on the author's 17,241 turns (2026-10-11): it
// drops 982 distinct lines (5,302 in all) from 214 turns, and each of a random
// sample of 20 is an agent's.
var (
	mdHeader   = regexp.MustCompile(`^\s*#{1,6}\s+\S`)
	mdBullet   = regexp.MustCompile("^\\s*([-*•]|\\d+[.)])\\s+[A-Z`*\\[]")
	capProse   = regexp.MustCompile(`^[A-Z][^\n]*[.:;]\s*$`)
	proseBlock = 3
)

// dropRelayed removes the lines of text that are agent text the owner relayed, and
// says how many it removed.
func dropRelayed(text string) (string, int) {
	if !strings.ContainsAny(text, "\n#`*-•0123456789") {
		return text, 0
	}
	ls := strings.Split(text, "\n")
	drop := make([]bool, len(ls))
	for i := 0; i < len(ls); {
		j := i
		for j < len(ls) && capProse.MatchString(strings.TrimSpace(ls[j])) && len(strings.Fields(ls[j])) >= 8 {
			j++
		}
		if j-i >= proseBlock {
			for k := i; k < j; k++ {
				drop[k] = true
			}
		}
		if j == i {
			j++
		}
		i = j
	}
	n := 0
	kept := ls[:0]
	for i, l := range ls {
		if drop[i] || mdHeader.MatchString(l) || markdownCode.MatchString(l) || mdBullet.MatchString(l) {
			n++
			continue
		}
		kept = append(kept, l)
	}
	if n == 0 {
		return text, 0
	}
	return strings.Join(kept, "\n"), n
}

// Build indexes the turns.
func (b *Builder) Build() *Corpus {
	b.c.data = append(b.buf.Bytes(), 0)
	b.c.sa = suffixarray.New(b.c.data)
	return b.c
}

// Empty reports a corpus with none of their turns: words cannot be checked, so
// no fact may be decided on it.
func (c *Corpus) Empty() bool { return c == nil || c.Stats.OwnerTurns == 0 }

// Match is where a quote is one of their turns.
type Match struct {
	Ref      string
	TurnHash string // ledger.TurnHash of the turn as the owner wrote it
	Near     bool   // a typo of theirs the quote corrected
	Question bool   // the words are in a question the owner asked
	Negated  bool   // a negation of theirs comes right before them
}

// Clean reports a match that states something: not their question, not
// negated.
func (m Match) Clean() bool { return !m.Question && !m.Negated }

// Find returns where the quote is one of their turns: a clean match in the
// given session first, then anywhere, exact before near (unless exactOnly);
// when every match is a question or negated, the first of those. ok is false
// when the owner never wrote it.
func (c *Corpus) Find(quote, session string, exactOnly bool) (Match, bool) {
	segs := Segments(quote)
	if segs == nil || c == nil || c.sa == nil {
		return Match{}, false
	}
	var fallback *Match
	try := func(idx []int, near bool) (Match, bool) {
		for _, i := range idx {
			m, ok := c.matchTurn(i, segs, near)
			if !ok {
				continue
			}
			if m.Clean() {
				return m, true
			}
			if fallback == nil {
				fallback = &m
			}
		}
		return Match{}, false
	}
	var preferred []int
	if session != "" {
		for _, src := range []string{"claude:", "codex:", "pi:", "hermes:"} {
			preferred = append(preferred, c.turns[src+session]...)
		}
	}
	if m, ok := try(preferred, false); ok {
		return m, true
	}
	if m, ok := try(c.candidates(segs[0]), false); ok {
		return m, true
	}
	if !exactOnly && len(segs) == 1 && len(strings.Fields(segs[0])) >= 4 {
		if m, ok := try(preferred, true); ok {
			return m, true
		}
		if m, ok := try(c.nearCandidates(strings.Fields(segs[0])), true); ok {
			return m, true
		}
	}
	if fallback != nil {
		return *fallback, true
	}
	return Match{}, false
}

// SaidIn is every session in which the owner said the quote as a statement
// of theirs (exact words, or a typo of theirs the quote corrected as Find
// forgives one; not their question, not negated): their refs, each once,
// sorted. A prompt history line with no session (claude:history,
// codex:history) names no session and is not counted, and neither is a
// turn word for word the same as one already counted (a resumed, forked or
// imported copy of the session: on the rig, Codex forks and Hermes
// continuations repeat the owner's turns under new ids). Code counts sessions
// with it (SPEC.md, "One-offs"): words said in one session are a statement,
// never a rule.
func (c *Corpus) SaidIn(quote string) []string {
	segs := Segments(quote)
	if segs == nil || c == nil || c.sa == nil {
		return nil
	}
	seen := map[string]bool{}
	turns := map[string]bool{} // the turns counted, by their normalized text
	var out []string
	try := func(idx []int, near bool) {
		for _, i := range idx {
			ref := c.refs[i]
			if seen[ref] || strings.HasSuffix(ref, ":history") {
				continue
			}
			text := Normalize(c.raw[i])
			if turns[text] {
				continue // a copy of a turn counted already
			}
			if m, ok := c.matchTurn(i, segs, near); ok && m.Clean() {
				seen[ref], turns[text] = true, true
				out = append(out, ref)
			}
		}
	}
	try(c.candidates(segs[0]), false)
	if len(segs) == 1 && len(strings.Fields(segs[0])) >= 4 {
		try(c.nearCandidates(strings.Fields(segs[0])), true)
	}
	sort.Strings(out)
	return out
}

// TurnIn is the owner's turn, as they wrote it, that holds quote as a
// statement of theirs (as SaidIn counts it) in the session ref, or in any
// session when ref holds none; "" when no turn does.
func (c *Corpus) TurnIn(quote, ref string) string {
	segs := Segments(quote)
	if segs == nil || c == nil || c.sa == nil {
		return ""
	}
	find := func(idx []int, near bool, want string) string {
		for _, i := range idx {
			if want != "" && c.refs[i] != want {
				continue
			}
			if m, ok := c.matchTurn(i, segs, near); ok && m.Clean() {
				return c.raw[i]
			}
		}
		return ""
	}
	exact := c.candidates(segs[0])
	var near []int
	if len(segs) == 1 && len(strings.Fields(segs[0])) >= 4 {
		near = c.nearCandidates(strings.Fields(segs[0]))
	}
	for _, want := range []string{ref, ""} {
		if t := find(exact, false, want); t != "" {
			return t
		}
		if t := find(near, true, want); t != "" {
			return t
		}
	}
	return ""
}

// SaidAt is when the owner said quote as a statement of theirs (as SaidIn
// counts it): the time of the earliest turn that holds it in the session ref,
// the turn whose hash is turnHash (ledger.TurnHash of the turn as they wrote
// it) first. Only when ref holds no such turn (its transcript is gone, or the
// words are in a resumed or forked copy of it) is it the earliest such turn
// in any session. Zero when no turn holding it records its time.
func (c *Corpus) SaidAt(quote, ref, turnHash string) time.Time {
	segs := Segments(quote)
	if segs == nil || c == nil || c.sa == nil {
		return time.Time{}
	}
	idx := c.candidates(segs[0])
	var near []int
	if len(segs) == 1 && len(strings.Fields(segs[0])) >= 4 {
		near = c.nearCandidates(strings.Fields(segs[0]))
	}
	// match reports the turns holding the quote, in ref (want) or anywhere.
	type hit struct {
		at   int64
		hash bool
	}
	collect := func(want string) []hit {
		var out []hit
		seen := map[int]bool{}
		for k, set := range [][]int{idx, near} {
			for _, i := range set {
				if seen[i] || (want != "" && c.refs[i] != want) {
					continue
				}
				seen[i] = true
				if m, ok := c.matchTurn(i, segs, k == 1); ok && m.Clean() {
					out = append(out, hit{at: c.at[i], hash: turnHash != "" && m.TurnHash == turnHash})
				}
			}
		}
		return out
	}
	earliest := func(hs []hit, hashOnly bool) int64 {
		var best int64
		for _, h := range hs {
			if h.at > 0 && (!hashOnly || h.hash) && (best == 0 || h.at < best) {
				best = h.at
			}
		}
		return best
	}
	hs := []hit(nil)
	if ref != "" && !strings.HasSuffix(ref, ":history") {
		hs = collect(ref)
	}
	if len(hs) == 0 {
		hs = collect("")
	}
	sec := earliest(hs, true)
	if sec == 0 {
		sec = earliest(hs, false)
	}
	if sec == 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0)
}

// SaidOn is SaidAt's day (YYYY-MM-DD, this machine's time zone), "" when
// no turn holding the quote records its time.
func (c *Corpus) SaidOn(quote, ref, turnHash string) string {
	t := c.SaidAt(quote, ref, turnHash)
	if t.IsZero() {
		return ""
	}
	return t.Local().Format("2006-01-02")
}

// LineTime is a transcript line's own time: its top-level timestamp, as
// Claude Code, Codex and pi write it (RFC 3339, or Unix seconds or
// milliseconds); zero when it has none.
func LineTime(line []byte) time.Time {
	var o struct {
		Timestamp json.RawMessage `json:"timestamp"`
	}
	if json.Unmarshal(line, &o) != nil || len(o.Timestamp) == 0 {
		return time.Time{}
	}
	return jsonTime(o.Timestamp)
}

// jsonTime reads a JSON time: an RFC 3339 string, or Unix seconds or
// milliseconds.
func jsonTime(raw json.RawMessage) time.Time {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t
		}
		return time.Time{}
	}
	var n float64
	if json.Unmarshal(raw, &n) == nil && n > 0 {
		if n > 1e12 {
			n /= 1000 // milliseconds
		}
		return time.Unix(int64(n), 0)
	}
	return time.Time{}
}

// candidates are the turns holding seg exactly.
func (c *Corpus) candidates(seg string) []int {
	seen := map[int]bool{}
	var out []int
	for _, off := range c.sa.Lookup([]byte(" "+seg+" "), 1000) {
		if i := sort.SearchInts(c.starts, off+2) - 1; i >= 0 && !seen[i] {
			seen[i] = true
			out = append(out, i)
		}
	}
	sort.Ints(out)
	return out
}

// nearCandidates are the turns holding any two adjacent words of the quote.
func (c *Corpus) nearCandidates(qw []string) []int {
	seen := map[int]bool{}
	var out []int
	for i := 0; i+1 < len(qw); i++ {
		for _, off := range c.sa.Lookup([]byte(" "+qw[i]+" "+qw[i+1]+" "), 200) {
			if t := sort.SearchInts(c.starts, off+2) - 1; t >= 0 && !seen[t] {
				seen[t] = true
				out = append(out, t)
			}
		}
	}
	sort.Ints(out)
	return out
}

// matchTurn finds the quote in turn i, word by word, and reads its context.
func (c *Corpus) matchTurn(i int, segs []string, near bool) (Match, bool) {
	toks := tokenize(c.raw[i])
	words := make([]string, len(toks))
	for k, t := range toks {
		words[k] = t.w
	}
	start, end := -1, -1
	if near {
		qw := strings.Fields(segs[0])
		at := nearAt(words, qw)
		if at < 0 {
			return Match{}, false
		}
		start, end = at, at+len(qw)
	} else {
		at := 0
		for si, s := range segs {
			sw := strings.Fields(s)
			k := indexWords(words[at:], sw)
			if k < 0 {
				return Match{}, false
			}
			if si == 0 {
				start = at + k
			}
			at += k + len(sw)
		}
		end = at
	}
	m := Match{Ref: c.refs[i], TurnHash: ledger.TurnHash(Normalize(c.raw[i])), Near: near}
	// A question: the sentence the words are in ends with a question mark and
	// asks (opens with a question word, or the words open it): "what would you
	// build?" is one, "please never rent without asking me, ok?" is not.
	first := start
	for first > 0 && !toks[first-1].endS && !toks[first-1].endQ {
		first--
	}
	for k := end - 1; k < len(toks); k++ {
		if toks[k].endQ {
			m.Question = first == start || questionWords[toks[first].w]
			break
		}
		if toks[k].endS {
			break
		}
	}
	// A negation of theirs right before the words ("not", "dont", "never" ...),
	// within the same sentence.
	for k := start - 1; k >= 0 && k >= start-3; k-- {
		if toks[k].endS || toks[k].endQ {
			break
		}
		if negationWords[toks[k].w] {
			m.Negated = true
			break
		}
	}
	return m, true
}

func indexWords(words, sub []string) int {
	for i := 0; i+len(sub) <= len(words); i++ {
		ok := true
		for j := range sub {
			if words[i+j] != sub[j] {
				ok = false
				break
			}
		}
		if ok {
			return i
		}
	}
	return -1
}

// questionWords open a question.
var questionWords = map[string]bool{"what": true, "why": true, "how": true, "which": true, "where": true, "when": true, "who": true, "whom": true,
	"should": true, "shall": true, "do": true, "does": true, "did": true, "can": true, "could": true, "would": true, "will": true, "is": true, "are": true,
	"was": true, "were": true, "have": true, "has": true, "may": true, "might": true, "isnt": true, "dont": true, "doesnt": true, "arent": true}

// negationWords turn what follows them around.
var negationWords = map[string]bool{"not": true, "no": true, "never": true, "dont": true, "didnt": true, "doesnt": true, "isnt": true, "wasnt": true,
	"arent": true, "cant": true, "cannot": true, "wont": true, "shouldnt": true, "couldnt": true, "wouldnt": true, "nor": true, "without": true, "stop": true}

// tok is one normalized word and whether a sentence or a question ends right
// after it.
type tok struct {
	w          string
	endS, endQ bool
}

// tokenize splits text into the words Normalize keeps, marking sentence ends:
// a question mark, a full stop or exclamation mark followed by a space or the
// end, and a line break.
func tokenize(s string) []tok {
	var out []tok
	var w strings.Builder
	flush := func() {
		if w.Len() > 0 {
			out = append(out, tok{w: w.String()})
			w.Reset()
		}
	}
	rs := []rune(s)
	for i, r := range rs {
		switch {
		case r == '\'' || r == '’' || r == '‘' || r == '`':
			continue
		case isWordRune(r):
			w.WriteRune(toLower(r))
		default:
			flush()
			if len(out) == 0 {
				continue
			}
			last := &out[len(out)-1]
			switch {
			case r == '?':
				last.endQ = true
			case r == '\n':
				last.endS = true
			case (r == '.' || r == '!') && (i+1 == len(rs) || rs[i+1] == ' ' || rs[i+1] == '\n'):
				last.endS = true
			}
		}
	}
	flush()
	return out
}

// FindIn reports whether the quote is in one of their turns in session ref
// (exactly; context not read).
func (c *Corpus) FindIn(ref, quote string) bool {
	segs := Segments(quote)
	if segs == nil || c == nil {
		return false
	}
	for _, i := range c.turns[ref] {
		if _, ok := c.matchTurn(i, segs, false); ok {
			return true
		}
	}
	return false
}

// Sessions is the number of owner sessions in the corpus.
func (c *Corpus) Sessions() int { return len(c.turns) }

// --- transcripts ---

var (
	sessionIDRx = regexp.MustCompile(`"id":"([^"]*)"`)
)

// AgentCwd reports a working directory only agents start sessions in
// (digest.AgentCwd).
func AgentCwd(cwd string) bool { return digest.AgentCwd(cwd) }

// Who says whether a transcript can hold owner turns, from its path and first
// bytes: "owner", "agent" or "unknown" (digest.SessionWho). For Claude Code
// each turn is decided again on its own line (claudeUserTurns).
func Who(path string, head []byte, f digest.Format) string {
	return string(digest.SessionWho(path, head, f).Who)
}

const headBytes = 256 << 10

func readHead(path string) []byte {
	fh, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer fh.Close()
	b := make([]byte, headBytes)
	n, _ := io.ReadFull(fh, b)
	return b[:n]
}

// AddTranscripts adds the owner's turns of every owner session in sources.
// A file that cannot be read is an error: a corpus missing their words would
// decide facts against them.
func (b *Builder) AddTranscripts(sources []Source) error {
	for _, src := range sources {
		files, err := filepath.Glob(src.Glob)
		if err != nil {
			return err
		}
		sort.Strings(files)
		for _, f := range files {
			if fi, err := os.Lstat(f); err != nil || !fi.Mode().IsRegular() {
				continue
			}
			head := readHead(f)
			switch Who(f, head, src.Format) {
			case "agent":
				b.c.Stats.AgentSessions++
				b.agentRefs[sessionRef(f, head, src.Format)] = true
				continue
			case "unknown":
				b.c.Stats.UnknownSessions++
				continue
			}
			if err := b.addTranscript(f, src.Format); err != nil {
				return fmt.Errorf("%s: %w", f, err)
			}
		}
	}
	return nil
}

// sessionRef is the corpus ref of a transcript's session, as addTranscript
// names it.
func sessionRef(path string, head []byte, f digest.Format) string {
	name := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	switch f {
	case digest.Pi:
		if h, ok := digest.ReadPiHeader(head); ok && h.ID != "" {
			return "pi:" + h.ID
		}
		return "pi:" + name
	case digest.Codex:
		if m, ok := digest.ReadCodexMeta(head); ok && m.ID != "" {
			return "codex:" + m.ID
		}
		return "codex:" + name
	}
	return "claude:" + name
}

func (b *Builder) addTranscript(path string, f digest.Format) error {
	fh, err := os.Open(path)
	if err != nil {
		return err
	}
	defer fh.Close()
	asked := digest.CodexAsked{} // the options a Codex agent's questions offered
	ref := ""
	if f == digest.Claude {
		ref = "claude:" + strings.TrimSuffix(filepath.Base(path), ".jsonl")
	}
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<20), 128<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if f == digest.Pi {
			if ref == "" {
				if h, ok := digest.ReadPiHeader(line); ok && h.ID != "" {
					ref = "pi:" + h.ID
				} else {
					ref = "pi:" + strings.TrimSuffix(filepath.Base(path), ".jsonl")
				}
			}
			if bytes.Contains(line, []byte(`"role":"user"`)) {
				for _, t := range piUserTurns(line) {
					b.AddAt("pi", ref, t, LineTime(line))
				}
			}
			continue
		}
		if f == digest.Claude {
			if !bytes.Contains(line, []byte(`"type":"user"`)) && !bytes.Contains(line, []byte(`"queued_command"`)) {
				continue
			}
			turns, skipped := claudeUserTurns(line)
			b.c.Stats.AgentTurns += skipped
			if len(turns) > 0 {
				at := LineTime(line)
				for _, t := range turns {
					b.AddAt("transcript", ref, t, at)
				}
			}
			continue
		}
		asked.NoteLine(line)
		if ref == "" && bytes.Contains(line, []byte(`"session_meta"`)) {
			if m := sessionIDRx.FindSubmatch(line); m != nil {
				ref = "codex:" + string(m[1])
			}
		}
		if !bytes.Contains(line, []byte(`"role":"user"`)) {
			continue
		}
		if ref == "" {
			ref = "codex:" + strings.TrimSuffix(filepath.Base(path), ".jsonl")
		}
		if turns := codexUserTurns(line, asked); len(turns) > 0 {
			at := LineTime(line)
			for _, t := range turns {
				b.AddAt("transcript", ref, t, at)
			}
		}
	}
	return sc.Err()
}

// claudeUserTurns is what the owner typed on one transcript line: a user turn
// a person typed (digest.ClaudeTyped: origin.kind "human", else turnOrigin or
// promptSource, else, from a version that marked none, a person's
// entrypoint), or a message they typed while the agent worked, which Claude
// Code records as a queued_command attachment (digest.ClaudeQueued). Harness
// text, compaction summaries, pasted blocks, sidechains, scheduled prompts
// and agents' turns are not theirs; a slash command's arguments are. skipped
// counts the turns left out as an agent's (a tool result is no turn).
func claudeUserTurns(line []byte) (out []string, skipped int) {
	if texts, typed, _, ok := digest.ClaudeQueuedLine(line); ok {
		if !typed {
			for _, t := range texts {
				if strings.TrimSpace(t) != "" {
					return nil, 1
				}
			}
			return nil, 0
		}
		return typedTexts(texts)
	}
	var o struct {
		digest.ClaudeMarks
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &o) != nil || o.Type != "user" || o.IsMeta || o.IsCompact || o.TranscriptOnly {
		return nil, 0
	}
	if typed, _ := digest.ClaudeTyped(o.ClaudeMarks); !typed {
		if hasText(o.Message.Content) {
			return nil, 1
		}
		return nil, 0
	}
	var texts []string
	var s string
	if json.Unmarshal(o.Message.Content, &s) == nil {
		texts = []string{s}
	} else {
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(o.Message.Content, &blocks) == nil {
			for _, bl := range blocks {
				if bl.Type == "text" {
					texts = append(texts, bl.Text)
				}
			}
		}
	}
	return typedTexts(texts)
}

// typedTexts is the owner's words in the texts of a turn a person typed:
// pasted blocks left out, and what a harness or another agent wrote into
// the turn (digest.TurnPieces: reminders, frames, a side agent's note; a
// slash command's arguments are theirs). skipped counts the texts with
// nothing of theirs.
func typedTexts(texts []string) (out []string, skipped int) {
	for _, t := range texts {
		t = pastedBlock.ReplaceAllString(t, " ")
		if strings.TrimSpace(t) == "" {
			continue
		}
		own := digest.OwnerPieces(digest.TurnPieces(t))
		if len(own) == 0 {
			skipped++
			continue
		}
		out = append(out, own...)
	}
	return out, skipped
}

// hasText reports a user line's content that holds text (a string, or a text
// block): a turn someone wrote, not only tool results.
func hasText(content json.RawMessage) bool {
	var s string
	if json.Unmarshal(content, &s) == nil {
		return strings.TrimSpace(s) != ""
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	_ = json.Unmarshal(content, &blocks)
	for _, bl := range blocks {
		if bl.Type == "text" && strings.TrimSpace(bl.Text) != "" {
			return true
		}
	}
	return false
}

var pastedBlock = regexp.MustCompile(`(?s)<pasted_content\b[^>]*>.*?</pasted_content>`)

// piUserTurns is what the owner typed in one pi user message: its text
// blocks, less harness frames (an attached file, a skill expansion) and
// pasted tables or logs (pi keeps no record of pastes either).
func piUserTurns(line []byte) []string {
	var o struct {
		Type    string `json:"type"`
		Message struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &o) != nil || o.Type != "message" || o.Message.Role != "user" {
		return nil
	}
	var texts []string
	var s string
	if json.Unmarshal(o.Message.Content, &s) == nil {
		texts = []string{s}
	} else {
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		_ = json.Unmarshal(o.Message.Content, &blocks)
		for _, bl := range blocks {
			if bl.Type == "text" {
				texts = append(texts, bl.Text)
			}
		}
	}
	var out []string
	for _, t := range texts {
		for _, own := range digest.OwnerPieces(digest.TurnPieces(t)) {
			if !PastedBlob(own) {
				out = append(out, own)
			}
		}
	}
	return out
}

// codexUserTurns is what the owner typed in one Codex user message: its
// texts split by who wrote each part (digest.TurnPieces: a session imported
// from Claude Code holds its frames and notices too; a question reply holds
// their answers), an answer that is an option its question offered (asked)
// and pasted tables or logs left out.
func codexUserTurns(line []byte, asked digest.CodexAsked) []string {
	var o struct {
		Type    string `json:"type"`
		Payload struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &o) != nil || o.Type != "response_item" || o.Payload.Type != "message" || o.Payload.Role != "user" {
		return nil
	}
	var out []string
	for _, c := range o.Payload.Content {
		for _, own := range digest.OwnerPieces(asked.Chosen(digest.TurnPieces(c.Text))) {
			if !PastedBlob(own) {
				out = append(out, own)
			}
		}
	}
	return out
}

var logLine = regexp.MustCompile(`^\s*(\d{4}-\d{2}-\d{2}|\d{1,2}:\d{2}|\[|\$ |>>> |[A-Z][A-Z_]{2,}[:=]|[-*] \S+: |\S+\.(go|rs|py|ts|js|md):\d+)`)

// PastedBlob reports a message that is mostly a pasted table or log: six
// lines or more, at least half of them table rows (two or more "|") or log
// lines (a timestamp, a bracket, a prompt, a file:line). Codex keeps no record
// of what the owner pasted, so this is how its transcripts drop an agent's output the owner
// passed along.
func PastedBlob(t string) bool {
	lines := strings.Split(strings.TrimSpace(t), "\n")
	if len(lines) < 6 {
		return false
	}
	n := 0
	for _, l := range lines {
		if strings.Count(l, "|") >= 2 || logLine.MatchString(l) {
			n++
		}
	}
	return n*2 >= len(lines)
}

// harnessFrame is a user message a harness or an agent framed: Claude Code's
// and Codex's own tags and notices, and any message that opens with a tag
// (<task-notification>, <codex_delegation>, <synthetic>, <environment_context>
// ...).
func harnessFrame(t string) bool {
	return digest.HarnessText(digest.Claude, t) || digest.HarnessText(digest.Codex, t) || strings.HasPrefix(strings.TrimSpace(t), "<")
}

// --- digests ---

// AddDigests adds the owner turns (U: lines) of the session digests under dir
// (eval/mine/digest.py: claude/<project>/<session>.md, codex/<session>.md)
// for sessions the corpus does not hold yet. Who started the session is
// decided from its transcript (`src`) when that still exists, else from the
// digest's `entry` header field (digestWho); a session neither decides is
// skipped. Digest lines are clipped (700 characters) and redacted, so a
// transcript is the better source whenever it exists.
func (b *Builder) AddDigests(dir string) error {
	var files []string
	for _, pat := range []string{filepath.Join(dir, "claude", "*", "*.md"), filepath.Join(dir, "codex", "*.md")} {
		m, err := filepath.Glob(pat)
		if err != nil {
			return err
		}
		files = append(files, m...)
	}
	sort.Strings(files)
	for _, path := range files {
		if strings.Contains(filepath.Base(path), "--sub-") {
			continue // a subagent's transcript: its prompts are an agent's, under the parent's session id
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		format := digest.Claude
		if filepath.Base(filepath.Dir(path)) == "codex" {
			format = digest.Codex
		}
		h := ParseDigestHeader(data)
		ref := ""
		switch {
		case format == digest.Claude && h.Session != "" && h.Session != "None":
			ref = "claude:" + h.Session
		case format == digest.Claude:
			ref = "claude:" + strings.TrimSuffix(filepath.Base(path), ".md")
		case h.Session != "" && h.Session != "None":
			ref = "codex:" + h.Session
		default:
			ref = "codex:" + strings.TrimSuffix(filepath.Base(path), ".md")
		}
		if b.known[ref] {
			continue
		}
		who := digestWho(h, format)
		if b.agentRefs[ref] {
			who = "agent"
		}
		switch who {
		case "agent":
			b.c.Stats.AgentSessions++
			continue
		case "unknown":
			b.c.Stats.UnknownSessions++
			continue
		}
		for _, l := range strings.Split(string(data), "\n") {
			if t, ok := strings.CutPrefix(l, "U: "); ok {
				if harnessFrame(t) {
					continue
				}
				b.Add("digest", ref, strings.TrimSuffix(t, "…"))
			}
		}
	}
	return nil
}

// digestWho decides a digest's session: from its transcript when that still
// exists (digest.SessionWho), else from the digest's `entry` field. Without the
// transcript, a Codex session that names a person's front end is unknown: a
// Codex subagent names its parent's front end too, and only the transcript's
// session_meta tells it apart.
func digestWho(h DigestHeader, format digest.Format) string {
	if AgentCwd(h.Cwd) {
		return "agent"
	}
	if h.Src != "" {
		if head := readHead(h.Src); len(head) > 0 {
			return Who(h.Src, head, format)
		}
	}
	switch {
	case h.Entry == "":
		return "unknown"
	case format == digest.Claude && ClaudeOwnerEntrypoints[h.Entry]:
		return "owner"
	case format == digest.Codex && CodexOwnerOriginators[h.Entry]:
		return "unknown"
	}
	return "agent"
}

// DigestHeader is the first two lines of a digest:
// "# session <id>" and "# cwd <dir>  ts <time>  src <path>[  entry <who>]".
type DigestHeader struct{ Session, Cwd, Src, Entry string }

// ParseDigestHeader reads a digest's header.
func ParseDigestHeader(data []byte) DigestHeader {
	var h DigestHeader
	lines := strings.SplitN(string(data), "\n", 3)
	if len(lines) > 0 {
		h.Session = strings.TrimSpace(strings.TrimPrefix(lines[0], "# session"))
	}
	if len(lines) > 1 {
		l := lines[1]
		if i := strings.LastIndex(l, "  entry "); i >= 0 {
			h.Entry = strings.TrimSpace(l[i+len("  entry "):])
			if h.Entry == "None" {
				h.Entry = "" // digest.py writes Python's None for a session that did not say
			}
			l = l[:i]
		}
		if i := strings.Index(l, "  src "); i >= 0 {
			h.Src = strings.TrimSpace(l[i+len("  src "):])
			l = l[:i]
		}
		if c, ok := strings.CutPrefix(l, "# cwd "); ok {
			if i := strings.Index(c, "  ts "); i >= 0 {
				c = c[:i]
			}
			h.Cwd = strings.TrimSpace(c)
		}
	}
	return h
}

// Mention is one of their turns that names a word: where (a session ref),
// the hash of the turn, the turn as they wrote it (what of it is theirs:
// AddAt), and when they wrote it (zero when its source does not say).
type Mention struct {
	Ref, TurnHash, Text string
	At                  time.Time
}

// Mentions are up to max of their turns that hold name as words (name
// normalized: "valkey-review-bot" is the words valkey review bot), the
// newest first; turns whose source records no time come last, in corpus
// order. What they said last about a thing is what decides it, so the
// newest are the ones shown.
func (c *Corpus) Mentions(name string, max int) []Mention {
	n := Normalize(name)
	if c == nil || c.sa == nil || n == "" || max <= 0 {
		return nil
	}
	idx := c.candidates(n)
	sort.SliceStable(idx, func(i, j int) bool { return c.at[idx[i]] > c.at[idx[j]] })
	var out []Mention
	for _, i := range idx {
		m := Mention{Ref: c.refs[i], TurnHash: ledger.TurnHash(Normalize(c.raw[i])), Text: c.raw[i]}
		if c.at[i] > 0 {
			m.At = time.Unix(c.at[i], 0)
		}
		out = append(out, m)
		if len(out) >= max {
			break
		}
	}
	return out
}
