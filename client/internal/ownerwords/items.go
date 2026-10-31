package ownerwords

// The item of a turn that holds a quote (SPEC.md, "Closing questions",
// Relayed). The owner often answers a list an agent put to them in one turn,
// a short line after each item's words, which they pasted: "<question>; -
// <answer>.  <question>; - <answer>", or a question line and an answer line
// under it. What a quote of theirs is about is read from the item that holds
// it: its question and its answer. So the question's words above a terse
// answer count, and another item's words never do; where the turn does not
// say which question the quote answers, there is no item.

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// answerMark is a dash that opens an answer, with a space after it: at a
// line's start, or after a clause ends (";", ":", ".", ")", "?", "!") and a
// space ("; - keep both", "). - yes", "\n- no"). A dash between two words
// ("keep it - the other is stale") joins them and opens nothing.
var answerMark = regexp.MustCompile(`(?m)(?:^[ \t\x{00a0}]*|[;:.)!?][ \t\x{00a0}]+)([-–—])(?:[ \t\x{00a0}]|$)`)

// unit is a sentence or a line of a turn, the pieces an item is made of.
type unit struct {
	start, end int
	line       int  // the line it starts on
	kind       byte // 'q' a question or other text, 'a' an answer (it opens with a dash), 'b' both (a question with its answer after a dash in it), 'c' an answer going on
	marks      []int
	question   bool // it ends with a question mark
	blank      bool // nothing but spaces
}

// ItemAround is the part of turn, as the owner wrote it, that holds quote,
// the item that says which question the quote answers.
//
// A turn is read in sentences and lines. A sentence after an answer on its
// line goes on with that answer, unless it asks ("?") or an answer dash
// follows it on that line, which makes it that answer's question; a
// dash-led line that asks is a question, its dash a bullet. The quote's
// sentences must hold one answer, which the quote reaches into, and no
// question after it. The item is those sentences, with the answer's question
// before it: the sentences before the answer on its line, back to the answer
// before them, or, for an answer that opens its line, the nearest line above
// it, the part of that line after its own answer when it holds one. A quote
// that starts in an answer's going on starts at that answer.
//
// In a turn with no answer dash at all, the quote's sentences are the
// answer, and the question is the questions ("?") right before it on its
// line or, when it opens its line, the nearest line above it when that line
// asks.
//
// "" when the turn does not hold the quote word for word, or when no item
// says which question it answers: the quote is a question's words alone, it
// runs from an answer into the next question, or the turn does not say where
// one item ends and the next begins ("q; - a; q; - a").
func ItemAround(turn, quote string) string {
	segs := Segments(quote)
	if segs == nil {
		return ""
	}
	toks := tokenizeAt(turn)
	words := make([]string, len(toks))
	for k, t := range toks {
		words[k] = t.w
	}
	at, first := 0, -1
	for si, s := range segs {
		sw := strings.Fields(s)
		k := indexWords(words[at:], sw)
		if k < 0 {
			return ""
		}
		if si == 0 {
			first = at + k
		}
		at += k + len(sw)
	}
	from, to := toks[first].start, toks[at-1].end
	us := units(turn)
	dashed := false
	qs, qe := -1, -1
	for i, u := range us {
		dashed = dashed || len(u.marks) > 0
		if !u.blank && u.end > from && u.start < to {
			if qs < 0 {
				qs = i
			}
			qe = i
		}
	}
	if !dashed {
		return dashless(turn, us, qs, qe)
	}
	if us[qs].kind == 'c' {
		for qs > 0 && us[qs].kind == 'c' {
			qs-- // a quote that starts in an answer's going on starts at that answer
		}
	}
	answers, reached, answered := 0, false, false
	for _, u := range us[qs : qe+1] {
		if u.blank {
			continue
		}
		if answered && u.kind == 'q' {
			return "" // the quote runs on into the next question
		}
		for _, d := range u.marks {
			answers++
			reached = reached || answerStart(turn, d) < to
		}
		answered = answered || len(u.marks) > 0 || u.kind == 'c'
	}
	if answers != 1 || !reached {
		return ""
	}
	start := qs
	if k := us[qs].kind; k == 'a' || k == 'b' {
		// The answer's question: the sentences before it on its line.
		for start > 0 && us[start-1].line == us[qs].line && us[start-1].kind == 'q' {
			start--
		}
		// An answer that opens its line answers the line above it: the
		// part of it after its own answer, when it holds one.
		if k == 'a' && opens(us, qs) {
			if l := lineAbove(us, qs); l >= 0 {
				start = askedOn(us, l, start)
			}
		}
	}
	end := qe
	for end+1 < len(us) && us[end+1].line == us[qe].line && us[end+1].kind == 'c' {
		end++ // the answer going on
	}
	return turn[us[start].start:us[end].end]
}

// dashless is ItemAround for a turn with no answer dash: the quote's
// sentences, with the questions right before them on their line, or the line
// above when they open theirs and that line asks.
func dashless(turn string, us []unit, qs, qe int) string {
	asked := false
	for _, u := range us[qs : qe+1] {
		if u.blank {
			continue
		}
		if asked && u.question {
			return "" // the quote runs on from an answer into the next question
		}
		asked = asked || !u.question
	}
	start := qs
	for start > 0 && us[start-1].line == us[qs].line && us[start-1].question {
		start--
	}
	if opens(us, start) {
		if l := lineAbove(us, start); l >= 0 && asks(us, l) {
			start = firstOf(us, l)
		}
	}
	return turn[us[start].start:us[qe].end]
}

// opens reports that unit i is the first one with words on its line.
func opens(us []unit, i int) bool {
	for j := i - 1; j >= 0 && us[j].line == us[i].line; j-- {
		if !us[j].blank {
			return false
		}
	}
	return true
}

// lineAbove is the nearest line above unit i's that has words in it; -1 for
// none.
func lineAbove(us []unit, i int) int {
	for j := i - 1; j >= 0; j-- {
		if us[j].line < us[i].line && !us[j].blank {
			return us[j].line
		}
	}
	return -1
}

// firstOf is the first unit of line l.
func firstOf(us []unit, l int) int {
	for i, u := range us {
		if u.line == l {
			return i
		}
	}
	return -1
}

// askedOn is where the question line l puts to the answer below it begins:
// the line's start, or the first question sentence after its own last answer
// or answer going on; start when the line asks nothing after it.
func askedOn(us []unit, l, start int) int {
	from := -1
	for i, u := range us {
		if u.line != l || u.blank {
			continue
		}
		if u.kind != 'q' {
			from = -1 // an answer, or one going on: what asks comes after it
			continue
		}
		if from < 0 {
			from = i
		}
	}
	if from < 0 {
		return start
	}
	if from == firstWords(us, l) {
		return firstOf(us, l) // the whole line asks
	}
	return from
}

// firstWords is the first unit with words on line l.
func firstWords(us []unit, l int) int {
	for i, u := range us {
		if u.line == l && !u.blank {
			return i
		}
	}
	return -1
}

// asks reports that line l ends with a question.
func asks(us []unit, l int) bool {
	last := -1
	for i, u := range us {
		if u.line == l && !u.blank {
			last = i
		}
	}
	return last >= 0 && us[last].question
}

// answerStart is where the answer a dash at d opens begins: past the dash and
// the spaces after it.
func answerStart(s string, d int) int {
	_, size := utf8.DecodeRuneInString(s[d:])
	i := d + size
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	return i
}

// units splits a turn into its sentences and lines: a sentence ends after a
// full stop, question or exclamation mark followed by a space or the end; a
// line break starts a new unit, and a new line. Each unit carries the answer
// dashes in it.
func units(s string) []unit {
	var dashes []int
	for _, m := range answerMark.FindAllStringSubmatchIndex(s, -1) {
		dashes = append(dashes, m[2])
	}
	var out []unit
	start, line := 0, 0
	add := func(end int) {
		if end <= start {
			return
		}
		t := s[start:end]
		if strings.HasPrefix(t, "\n") && start > 0 {
			line++
		}
		u := unit{start: start, end: end, line: line, blank: strings.TrimSpace(t) == ""}
		for _, d := range dashes {
			if d >= start && d < end {
				u.marks = append(u.marks, d)
			}
		}
		lead := start + len(t) - len(strings.TrimLeftFunc(t, unicode.IsSpace))
		bullet := len(u.marks) > 0 && u.marks[0] == lead
		u.question = strings.HasSuffix(strings.TrimSpace(t), "?")
		if bullet && u.question {
			u.marks, bullet = u.marks[1:], false // a bulleted question: its dash opens no answer
		}
		switch {
		case len(u.marks) == 0:
			u.kind = 'q'
		case bullet && len(u.marks) == 1:
			u.kind = 'a'
		default:
			u.kind = 'b'
		}
		out = append(out, u)
		start = end
	}
	for j := 0; j < len(s); j++ {
		switch s[j] {
		case '\n':
			add(j)
		case '.', '?', '!':
			if j+1 == len(s) || s[j+1] == ' ' || s[j+1] == '\n' || s[j+1] == '\t' || s[j+1] == '\r' {
				add(j + 1)
			}
		}
	}
	add(len(s))
	// A sentence after an answer on its line goes on with it, unless it asks
	// or the next sentence on the line is an answer, whose question it is.
	answered := false
	for i := range out {
		u := &out[i]
		if i == 0 || u.line != out[i-1].line {
			answered = false
		}
		if u.blank {
			continue
		}
		switch {
		case u.kind == 'a' || u.kind == 'b':
			answered = true
		case u.question:
			answered = false
		case answered && !answerNext(out, i):
			u.kind = 'c'
		case answered:
			answered = false // the next answer's question
		}
	}
	return out
}

// answerNext reports that the next sentence with words on unit i's line is an
// answer that opens with its dash.
func answerNext(us []unit, i int) bool {
	for j := i + 1; j < len(us) && us[j].line == us[i].line; j++ {
		if !us[j].blank {
			return us[j].kind == 'a'
		}
	}
	return false
}

// tokAt is one word of tokenize with where it sits in the text (byte
// offsets, end exclusive).
type tokAt struct {
	w          string
	start, end int
}

// tokenizeAt is tokenize's words with their offsets: the same words, so a
// quote matched against one matches against the other.
func tokenizeAt(s string) []tokAt {
	var out []tokAt
	var w strings.Builder
	start, end := -1, -1
	flush := func() {
		if w.Len() > 0 {
			out = append(out, tokAt{w: w.String(), start: start, end: end})
			w.Reset()
		}
		start = -1
	}
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == '\'' || r == '’' || r == '‘' || r == '`':
		case isWordRune(r):
			if start < 0 {
				start = i
			}
			w.WriteRune(unicode.ToLower(r))
			end = i + size
		default:
			flush()
		}
		i += size
	}
	flush()
	return out
}
