package ownerwords

// The item of a turn that holds a quote (SPEC.md, "Closing questions",
// Relayed). The owner often answers a list an agent put to them in one turn,
// a short line after each item's words, which they pasted: "<question>; -
// <answer>.  <question>; - <answer>", or one item to a line. What a quote of
// theirs is about is read from the item that holds it: from the end of the
// answer before it to the end of its own sentence. So the question's words
// above a terse answer count, and another item's words never do.

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// answerMark is a dash that opens an answer: alone, at a line's start or
// after a space, with a space after it ("; - keep both", "\n- no").
var answerMark = regexp.MustCompile(`(?m)(?:^|\s)[-–—]\s`)

// ItemAround is the part of turn, as the owner wrote it, that holds quote:
// from the end of the last answer before the quote (an answer runs from its
// dash to the end of its sentence or line) to the end of the sentence the
// quote ends in, or to the turn's end when no answer follows the quote. A
// turn with no answer before or after the quote is one item, whole. "" when
// the turn does not hold the quote word for word.
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
	begin, after := 0, false
	for _, m := range answerMark.FindAllStringIndex(turn, -1) {
		dash := strings.IndexFunc(turn[m[0]:m[1]], func(r rune) bool { return r == '-' || r == '–' || r == '—' }) + m[0]
		if dash >= to {
			after = true
			continue
		}
		if end := sentenceEnd(turn, m[1]); end <= from && end > begin {
			begin = end
		}
	}
	end := len(turn)
	if after {
		end = sentenceEnd(turn, to)
	}
	return turn[begin:end]
}

// sentenceEnd is where the sentence or line that runs at i ends: after a
// full stop, question or exclamation mark followed by a space or the end, at
// a line break, or at the end of the text.
func sentenceEnd(s string, i int) int {
	for j := i; j < len(s); j++ {
		switch s[j] {
		case '\n':
			return j
		case '.', '?', '!':
			if j+1 == len(s) || s[j+1] == ' ' || s[j+1] == '\n' || s[j+1] == '\t' {
				return j + 1
			}
		}
	}
	return len(s)
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
