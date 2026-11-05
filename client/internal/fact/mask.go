package fact

import "regexp"

// nameWord is a run that can be a fact name: lowercase letters, digits and
// hyphens.
var nameWord = regexp.MustCompile(`[a-z0-9][a-z0-9-]*`)

// Withheld is what MaskNames puts in a hidden name's place.
const Withheld = "(a withheld fact)"

// NamesAny reports whether text names one of names as a word of its own (a
// longer name that holds it does not count).
func NamesAny(text string, names map[string]bool) bool {
	if len(names) == 0 {
		return false
	}
	for _, w := range nameWord.FindAllString(text, -1) {
		if names[w] {
			return true
		}
	}
	return false
}

// MaskNames is text with each of names, as a word of its own, replaced by
// Withheld: a session's view of another fact's text never names a fact the
// session is denied (SPEC.md, Hold-outs).
func MaskNames(text string, names map[string]bool) string {
	if len(names) == 0 {
		return text
	}
	return nameWord.ReplaceAllStringFunc(text, func(w string) string {
		if names[w] {
			return Withheld
		}
		return w
	})
}
