package ownerwords

// The item of a turn that holds a quote (SPEC.md, "Closing questions",
// Relayed). The owner often answers a list an agent put to them in one turn,
// a short line after each item's words, which they pasted: "<question>; -
// <answer>.  <question>; - <answer>", or one question and one answer to a
// line. What a quote of theirs is about is read from the item that holds it:
// its question and its answer. So the question's words above a terse answer
// count, and another item's words never do; where the turn does not say
// which question the quote answers, there is no item.

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
var answerMark = regexp.MustCompile(`(?m)(?:^[ \t]*|[;:.)!?][ \t]+)([-–—])(?:[ \t]|$)`)

// unit is a sentence or a line of a turn, the pieces an item is made of.
type unit struct {
	start, end int
	kind       byte // 'q' a question or other text, 'a' an answer (it opens with a dash), 'b' both (a question with its answer after a dash), 'c' an answer going on (a sentence after an answer on its line)
	marks      []int
	question   bool // it ends with a question mark
}

// ItemAround is the part of turn, as the owner wrote it, that holds quote,
// the item that says which question the quote answers:
//
//   - In a turn with no answer dash: the sentences that hold the quote, with
//     the questions (sentences ending in "?") right above them.
//   - In a turn with answer dashes: the sentences or lines that hold the
//     quote, when they hold one answer and the quote reaches into it, and no
//     question follows the answer in them; with the question lines above an
//     answer line, back to the answer before them. A sentence after an answer
//     on its line goes on with that answer unless it asks ("?"), and a
//     dash-led line that asks is a question, its dash a bullet.
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
		if u.end > from && u.start < to {
			if qs < 0 {
				qs = i
			}
			qe = i
		}
	}
	start := qs
	if !dashed {
		for start > 0 && us[start-1].question {
			start--
		}
		return turn[us[start].start:us[qe].end]
	}
	answers, reached, answered := 0, false, false
	for _, u := range us[qs : qe+1] {
		if answered && u.kind == 'q' {
			return "" // the quote runs on into the next question
		}
		for _, d := range u.marks {
			answers++
			reached = reached || answerStart(turn, d) < to
		}
		answered = answered || len(u.marks) > 0
	}
	if answers != 1 || !reached {
		return ""
	}
	if us[qs].kind == 'a' {
		for start > 0 && us[start-1].kind == 'q' {
			start--
		}
	}
	return turn[us[start].start:us[qe].end]
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
// line break starts a new unit. Each unit carries the answer dashes in it.
func units(s string) []unit {
	var dashes []int
	for _, m := range answerMark.FindAllStringSubmatchIndex(s, -1) {
		dashes = append(dashes, m[2])
	}
	var out []unit
	start := 0
	answered := false // the line so far holds an answer
	add := func(end int) {
		if end <= start {
			return
		}
		t := s[start:end]
		u := unit{start: start, end: end}
		for _, d := range dashes {
			if d >= start && d < end {
				u.marks = append(u.marks, d)
			}
		}
		lead := start + len(t) - len(strings.TrimLeft(t, " \t\n\r"))
		bullet := len(u.marks) > 0 && u.marks[0] == lead
		u.question = strings.HasSuffix(strings.TrimSpace(t), "?")
		if bullet && u.question {
			u.marks, bullet = u.marks[1:], false // a bulleted question: its dash opens no answer
		}
		if start == 0 || strings.HasPrefix(t, "\n") {
			answered = false
		}
		switch {
		case len(u.marks) == 0 && answered && !u.question:
			u.kind = 'c'
		case len(u.marks) == 0:
			u.kind = 'q'
		case bullet && len(u.marks) == 1:
			u.kind = 'a'
		default:
			u.kind = 'b'
		}
		answered = (answered || len(u.marks) > 0) && !(u.kind == 'q' && u.question)
		out = append(out, u)
		start = end
	}
	for j := 0; j < len(s); j++ {
		switch s[j] {
		case '\n':
			add(j)
		case '.', '?', '!':
			if j+1 == len(s) || s[j+1] == ' ' || s[j+1] == '\n' || s[j+1] == '\t' {
				add(j + 1)
			}
		}
	}
	add(len(s))
	return out
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
