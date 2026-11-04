package main

import (
	"math"
	"regexp"
	"strings"

	"github.com/agent-sh/tacit/client/internal/search"
	"github.com/agent-sh/tacit/client/internal/secretscan"
)

// The delivery log keeps a request's terms, never its text (SPEC.md,
// "Delivery log"). logTerms is those terms with every secret-shaped word of
// the text left out first, in this process: the server's own key shapes
// (secretscan), known key prefixes, long hex or base64 runs and
// high-entropy words. No redaction process runs in a hook.

// keyPrefix starts a word that is a provider's key whatever follows it.
var keyPrefix = regexp.MustCompile(`^(?:gh[pousr]_|github_pat_|glpat-|sk_live_|sk_test_|rk_live_|xox[abceoprs]-|xapp-|tskey-|sk-|AKIA|ASIA|AIza|ya29\.|eyJ|npm_|pypi-|hf_|-----BEGIN)`)

var (
	hexRun    = regexp.MustCompile(`[0-9a-fA-F]{32,}`)
	base64Run = regexp.MustCompile(`[A-Za-z0-9+/_=-]{24,}`)
)

// secretWord reports a word (or a key=value's value) shaped like a secret.
func secretWord(w string) bool {
	w = strings.Trim(w, "\"'`,;()[]{}<>")
	if w == "" {
		return false
	}
	if secretscan.MatchString(w) || keyPrefix.MatchString(w) {
		return true
	}
	if i := strings.IndexAny(w, "=:"); i >= 0 && i+1 < len(w) && secretWord(w[i+1:]) {
		return true
	}
	if hexRun.MatchString(w) {
		return true
	}
	if m := base64Run.FindString(w); m != "" && mixedClasses(m) {
		return true
	}
	return len(w) >= 20 && keyChars.MatchString(w) && entropy(w) >= 4.0
}

// keyChars is a word of key characters only: no path or file name.
var keyChars = regexp.MustCompile(`^[A-Za-z0-9_+=-]+$`)

// mixedClasses: upper and lower case letters and a digit, as a random key
// has and a word or a path rarely does.
func mixedClasses(s string) bool {
	var up, low, dig bool
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			up = true
		case r >= 'a' && r <= 'z':
			low = true
		case r >= '0' && r <= '9':
			dig = true
		}
	}
	return up && low && dig
}

// entropy is the word's Shannon entropy in bits per character.
func entropy(s string) float64 {
	n := map[rune]float64{}
	total := 0.0
	for _, r := range s {
		n[r]++
		total++
	}
	h := 0.0
	for _, c := range n {
		p := c / total
		h -= p * math.Log2(p)
	}
	return h
}

// logTerms is the query terms of text (search.Terms, as the selection reads
// them) once its secret-shaped words are left out.
func logTerms(text string) []string {
	words := strings.Fields(text)
	kept := words[:0:0]
	for _, w := range words {
		if !secretWord(w) {
			kept = append(kept, w)
		}
	}
	return search.Terms(strings.Join(kept, " "))
}
