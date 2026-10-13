package ownerwords

import (
	"regexp"
	"strings"
	"unicode"
)

// The owner may type fast (a dropped, doubled or swapped letter) and an agent
// quoting them often fixes the spelling. A near match accepts that and nothing
// more: the same words in the same order, where a few words may each be one
// typo away from theirs (one edit in a word of four letters or more, a swap of
// two letters counting as one, two edits in a word of eight or more), at most
// one word in four, never in a number, a negation or a changed first letter,
// and never a whole word added, dropped or moved.

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }
func toLower(r rune) rune    { return unicode.ToLower(r) }

// NearIn reports whether quote occurs in text with at most corrected typos.
func NearIn(text, quote string) bool {
	segs := Segments(quote)
	if segs == nil || len(segs) != 1 {
		return false
	}
	qw := strings.Fields(segs[0])
	return len(qw) >= 4 && nearAt(strings.Fields(Normalize(text)), qw) >= 0
}

// nearAt is the word index in tw where qw occurs with at most corrected
// typos, or -1.
func nearAt(tw, qw []string) int {
	n := len(qw)
	allowed := max(1, n/4)
	for i := 0; i+n <= len(tw); i++ {
		diff := 0
		for j := 0; j < n && diff <= allowed; j++ {
			a, b := tw[i+j], qw[j]
			if a == b {
				continue
			}
			if !typo(a, b) {
				diff = allowed + 1
				break
			}
			diff++
		}
		if diff <= allowed {
			return i
		}
	}
	return -1
}

// typo reports two spellings of one word: one edit apart in a word of four
// letters or more, two in a word of eight or more, the same first letter.
// Numbers and negations must match.
func typo(a, b string) bool {
	if digits.MatchString(a) || digits.MatchString(b) || negations[a] || negations[b] {
		return false
	}
	ra, rb := []rune(a), []rune(b)
	if ra[0] != rb[0] {
		return false // enable/unable, dont/wont: a different word, not a slip
	}
	la, lb := len(ra), len(rb)
	short := min(la, lb)
	switch {
	case short >= 8:
		return editDistance(a, b, 2) <= 2
	case short >= 4:
		return editDistance(a, b, 1) <= 1
	}
	return false
}

var digits = regexp.MustCompile(`\d`)

// negations must match exactly: "isnt" and "inst" are not one word misspelled.
var negations = map[string]bool{"dont": true, "wont": true, "cant": true, "isnt": true, "doesnt": true, "didnt": true,
	"wasnt": true, "arent": true, "shouldnt": true, "couldnt": true, "wouldnt": true, "havent": true, "hasnt": true, "none": true}

// editDistance is the edit distance of a and b where swapping two adjacent
// letters is one edit ("reivew" for "review"), or limit+1 when the lengths
// alone rule it out (words are short; no other early exit).
func editDistance(a, b string, limit int) int {
	ra, rb := []rune(a), []rune(b)
	if d := len(ra) - len(rb); d > limit || -d > limit {
		return limit + 1
	}
	pp := make([]int, len(rb)+1) // row i-2
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			if i > 1 && j > 1 && ra[i-1] == rb[j-2] && ra[i-2] == rb[j-1] {
				cur[j] = min(cur[j], pp[j-2]+1)
			}
		}
		pp, prev, cur = prev, cur, pp
	}
	return prev[len(rb)]
}
