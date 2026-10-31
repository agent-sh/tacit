package ownerwords

// The item of a turn that holds a quote (SPEC.md, "Closing questions",
// Relayed). The owner often answers a list an agent put to them in one turn,
// a short line after each item's words, which they pasted: "<question>; -
// <answer>.  <question>; - <answer>", or one question and one answer to a
// line. What a quote of theirs is about is read from the item that holds it:
// its question and its answer. So the question's words above a terse answer
// count, and another item's words never do; where the turn does not say
// where one item ends and the next begins, there is no item.

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// answerMark is a dash that opens an answer: alone, at a line's start or
// after a space, with a space after it ("; - keep both", "\n- no").
var answerMark = regexp.MustCompile(`(?m)(?:^|\s)[-–—]\s`)

// unit is a sentence or a line of a turn, the pieces an item is made of: a
// question (no answer in it), an answer (it opens with a dash), or both (a
// question with its answer after a dash inside it).
type unit struct {
	start, end int
	kind       byte // 'q' a question or other text, 'a' an answer, 'b' both
	marks      int  // the answers it opens
}

// ItemAround is the part of turn, as the owner wrote it, that holds quote:
// the sentences or lines the quote is in, and when the first of them is an
// answer (it opens with a dash) the question lines above it, back to the
// answer before them. A dash-led line that ends with a question mark is a
// question, its dash a bullet. A turn with no answer in it is one item,
// whole. "" when the turn does not hold the quote word for word, or when the
// item would hold more than one answer: the turn does not say where one item
// ends and the next begins ("q; - a; q; - a"), and a caller reads the quote
// alone.
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
	any := false
	qs, qe := -1, -1
	for i, u := range us {
		any = any || u.marks > 0
		if u.end > from && u.start < to {
			if qs < 0 {
				qs = i
			}
			qe = i
		}
	}
	if !any {
		return turn
	}
	start := qs
	if us[qs].kind == 'a' {
		for start > 0 && us[start-1].kind == 'q' {
			start--
		}
	}
	marks := 0
	for _, u := range us[start : qe+1] {
		marks += u.marks
	}
	if marks > 1 {
		return ""
	}
	return turn[us[start].start:us[qe].end]
}

// units splits a turn into its sentences and lines: a sentence ends after a
// full stop, question or exclamation mark followed by a space or the end; a
// line break starts a new unit.
func units(s string) []unit {
	var out []unit
	start := 0
	add := func(end int) {
		if end <= start {
			return
		}
		t := s[start:end]
		u := unit{start: start, end: end, marks: len(answerMark.FindAllStringIndex(t, -1))}
		trimmed := strings.TrimSpace(t)
		bullet := answerMark.MatchString(trimmed) && answerMark.FindStringIndex(trimmed)[0] == 0
		if bullet && strings.HasSuffix(trimmed, "?") {
			u.marks-- // a bulleted question: its dash opens no answer
			bullet = false
		}
		switch {
		case u.marks == 0:
			u.kind = 'q'
		case bullet && u.marks == 1:
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
