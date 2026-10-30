package search

import (
	"regexp"
	"strings"
	"unicode"
)

// The index tokenizer is FTS5's `porter unicode61 remove_diacritics 2`, so
// "amended" and "amend" are one term to every MATCH. Query-side logic that
// counts terms in Go (the two-match gate, the anchors) has to count them the
// same way, or one word typed twice in two forms passes the gate alone.
// stem is FTS5's Porter stemmer, step for step (sqlite ext/fts5,
// fts5PorterCb): tokens of 3 to 64 bytes are stemmed, others pass through.
// TestStemMatchesFTS5 checks it against SQLite's own tokenizer.
func stem(w string) string {
	if len(w) < 3 || len(w) > 64 {
		return w
	}
	b := []byte(w)
	b = porterStep1A(b)
	if b2, ok := porterStep1B(b); ok {
		b = b2
		if b3, ok := porterReplace(b, step1B2, nil); ok {
			b = b3
		} else if n := len(b); n >= 2 {
			c := b[n-1]
			if !porterVowel(c, false) && c != 'l' && c != 's' && c != 'z' && c == b[n-2] {
				b = b[:n-1]
			} else if porterMEq1(b) && porterOStar(b) {
				b = append(b, 'e')
			}
		} else if porterMEq1(b) && porterOStar(b) {
			b = append(b, 'e')
		}
	} else {
		b = b2
	}
	if n := len(b); n > 0 && b[n-1] == 'y' && porterHasVowel(b[:n-1]) {
		b[n-1] = 'i'
	}
	b, _ = porterReplace(b, step2, porterMGt0)
	b, _ = porterReplace(b, step3, porterMGt0)
	b = porterStep4(b)
	if n := len(b); n > 0 && b[n-1] == 'e' {
		if s := b[:n-1]; porterMGt1(s) || (porterMEq1(s) && !porterOStar(s)) {
			b = s
		}
	}
	if n := len(b); n > 1 && b[n-1] == 'l' && b[n-2] == 'l' && porterMGt1(b[:n-1]) {
		b = b[:n-1]
	}
	return string(b)
}

// stemKey is the form a query term takes in the index: unicode61 splits it on
// what is not a letter or digit (so foo_bar is the phrase "foo bar"), and
// each piece is stemmed. Diacritics are not folded here, so two non-ASCII
// spellings the index folds together may still count twice.
func stemKey(term string) string {
	parts := strings.FieldsFunc(term, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
	for i, p := range parts {
		parts[i] = stem(p)
	}
	return strings.Join(parts, " ")
}

// distinctStems keeps the first term of each stem, in order.
func distinctStems(terms []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range terms {
		k := stemKey(t)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, t)
	}
	return out
}

func porterVowel(c byte, yIsVowel bool) bool {
	return c == 'a' || c == 'e' || c == 'i' || c == 'o' || c == 'u' || (yIsVowel && c == 'y')
}

// porterGobbleVC skips one vowel run and one consonant: the end of the
// first VC in s, or 0 when there is none.
func porterGobbleVC(s []byte, prevCons bool) int {
	cons := prevCons
	i := 0
	for ; i < len(s); i++ {
		cons = !porterVowel(s[i], cons)
		if !cons {
			break
		}
	}
	for i++; i < len(s); i++ {
		cons = !porterVowel(s[i], cons)
		if cons {
			return i + 1
		}
	}
	return 0
}

func porterMGt0(s []byte) bool { return porterGobbleVC(s, false) != 0 }

func porterMGt1(s []byte) bool {
	n := porterGobbleVC(s, false)
	return n != 0 && porterGobbleVC(s[n:], true) != 0
}

func porterMEq1(s []byte) bool {
	n := porterGobbleVC(s, false)
	return n != 0 && porterGobbleVC(s[n:], true) == 0
}

// porterOStar: s ends consonant-vowel-consonant, the last not w, x or y.
func porterOStar(s []byte) bool {
	n := len(s)
	if n == 0 || s[n-1] == 'w' || s[n-1] == 'x' || s[n-1] == 'y' {
		return false
	}
	mask, cons := 0, false
	for _, c := range s {
		cons = !porterVowel(c, cons)
		mask <<= 1
		if cons {
			mask++
		}
	}
	return mask&7 == 5
}

// porterHasVowel is FTS5's (*v*): a y counts as a vowel anywhere but first.
func porterHasVowel(s []byte) bool {
	for i, c := range s {
		if porterVowel(c, i > 0) {
			return true
		}
	}
	return false
}

func porterStep1A(b []byte) []byte {
	n := len(b)
	if b[n-1] != 's' {
		return b
	}
	switch {
	case b[n-2] == 'e':
		if (n > 4 && b[n-4] == 's' && b[n-3] == 's') || (n > 3 && b[n-3] == 'i') {
			return b[:n-2]
		}
		return b[:n-1]
	case b[n-2] == 's':
		return b
	}
	return b[:n-1]
}

// porterStep1B removes -eed (to -ee), -ed and -ing; ok when -ed or -ing went.
func porterStep1B(b []byte) ([]byte, bool) {
	n := len(b)
	switch {
	case n > 3 && strings.HasSuffix(string(b), "eed"):
		if porterMGt0(b[:n-3]) {
			return b[:n-1], false
		}
	case n > 2 && strings.HasSuffix(string(b), "ed"):
		if porterHasVowel(b[:n-2]) {
			return b[:n-2], true
		}
	case n > 3 && strings.HasSuffix(string(b), "ing"):
		if porterHasVowel(b[:n-3]) {
			return b[:n-3], true
		}
	}
	return b, false
}

type porterRule struct{ suffix, with string }

var (
	step1B2 = []porterRule{{"at", "ate"}, {"bl", "ble"}, {"iz", "ize"}}
	step2   = []porterRule{{"ational", "ate"}, {"tional", "tion"}, {"enci", "ence"}, {"anci", "ance"}, {"izer", "ize"}, {"logi", "log"},
		{"bli", "ble"}, {"alli", "al"}, {"entli", "ent"}, {"eli", "e"}, {"ousli", "ous"}, {"ization", "ize"}, {"ation", "ate"},
		{"ator", "ate"}, {"alism", "al"}, {"iveness", "ive"}, {"fulness", "ful"}, {"ousness", "ous"}, {"aliti", "al"},
		{"iviti", "ive"}, {"biliti", "ble"}}
	step3 = []porterRule{{"icate", "ic"}, {"ative", ""}, {"alize", "al"}, {"iciti", "ic"}, {"ical", "ic"}, {"ful", ""}, {"ness", ""}}
	step4 = []string{"al", "ance", "ence", "er", "ic", "able", "ible", "ant", "ement", "ment", "ent", "ion", "ou", "ism", "ate", "iti", "ous", "ive", "ize"}
)

// porterReplace applies the first rule whose suffix b ends with, behind a
// stem of at least one byte, when cond (nil: always) holds for that stem. A
// matching suffix whose condition fails ends the step, as in Porter's rules,
// where the longest matching suffix is the only one tried.
func porterReplace(b []byte, rules []porterRule, cond func([]byte) bool) ([]byte, bool) {
	for _, r := range rules {
		if len(b) > len(r.suffix) && strings.HasSuffix(string(b), r.suffix) {
			s := b[:len(b)-len(r.suffix)]
			if cond != nil && !cond(s) {
				return b, false
			}
			return append(append([]byte{}, s...), r.with...), true
		}
	}
	return b, false
}

func porterStep4(b []byte) []byte {
	for _, suf := range step4 {
		if len(b) > len(suf) && strings.HasSuffix(string(b), suf) {
			s := b[:len(b)-len(suf)]
			if suf == "ion" {
				if n := len(s); n > 0 && (s[n-1] == 's' || s[n-1] == 't') && porterMGt1(s) {
					return s
				}
				return b
			}
			if porterMGt1(s) {
				return s
			}
			return b
		}
	}
	return b
}

// Stem is the index's stem of one lowercase word (porter, as FTS5 stems).
func Stem(w string) string { return stem(w) }

var wordRx = regexp.MustCompile(`[\p{L}\p{N}_]+`)

// Words splits text into lowercase words, without stop-word removal or
// splitting limits: for matching commands a prompt names.
func Words(text string) []string { return wordRx.FindAllString(strings.ToLower(text), -1) }

// TermKey is the form a query term takes in the index: its pieces stemmed.
// Words compared by it are compared as the index compares them.
func TermKey(term string) string { return stemKey(term) }
