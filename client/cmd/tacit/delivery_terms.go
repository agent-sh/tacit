package main

import (
	"math"
	"regexp"
	"strings"

	"github.com/agent-sh/tacit/client/internal/search"
	"github.com/agent-sh/tacit/client/internal/secretscan"
)

// The delivery log keeps a request's terms, never its text (SPEC.md,
// "Delivery log"). logTerms is those terms with every secret left out
// first, in this process, by two kinds of test: where a word stands (the
// value after a password, token or key name, a URL's user and password, a
// password glued onto -p) and its shape (the server's own key shapes, known
// key prefixes, long hex runs, random-looking runs of letters and digits).
// No redaction process runs in a hook.

// secretNameRx is a name whose value is a secret: PGPASSWORD=, --api-key,
// "Authorization: Bearer", "the password is".
var secretNameRx = regexp.MustCompile(`(?i)(pass(word|wd)?|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|auth(orization)?|bearer|credentials?)$`)

// keyPrefix starts a word that is a provider's key, with a tail of key
// characters long enough to be one (a name like hf_hub_download is no key).
var keyPrefix = regexp.MustCompile(`^(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|glpat-[A-Za-z0-9_-]{20,}|(?:sk|rk)_(?:live|test)_[A-Za-z0-9]{16,}|sk-[A-Za-z0-9_-]{16,}|xox[abceoprs]-[A-Za-z0-9-]{10,}|xapp-[A-Za-z0-9-]{10,}|tskey-[A-Za-z0-9-]{10,}|(?:AKIA|ASIA)[A-Z0-9]{16}|AIza[A-Za-z0-9_-]{30,}|ya29\.[A-Za-z0-9_-]{20,}|eyJ[A-Za-z0-9_-]{10,}\.|npm_[A-Za-z0-9]{30,}|pypi-[A-Za-z0-9_-]{30,}|hf_[A-Za-z0-9]{30,}|-----BEGIN)`)

var (
	hexRun = regexp.MustCompile(`[0-9a-fA-F]{32,}`)
	// keyRun is a run of key characters with no path or file separator.
	keyRun   = regexp.MustCompile(`[A-Za-z0-9+_=]{20,}`)
	userinfo = regexp.MustCompile(`://[^/@\s]*@`)
	// connective words between a secret's name and its value.
	connective = map[string]bool{"is": true, "was": true, "=": true, ":": true, "to": true, "of": true, "be": true}
)

// secretShaped reports a piece of a word (split as search.Terms splits:
// at / . -) that looks like a secret by its shape alone.
func secretShaped(p string) bool {
	if hexRun.MatchString(p) {
		return true // a 32-digit hex run: a key, a hash of one, or a full commit id
	}
	for _, m := range keyRun.FindAllString(p, -1) {
		if randomLooking(m) {
			return true
		}
	}
	return false
}

// randomLooking: letters of both cases and at least two digits spread in
// them, at 4 bits of entropy a character or more, as a random key has and
// an identifier (configureHTTPClientWithRetries, frontendServiceV2) does not.
func randomLooking(s string) bool {
	var up, low, dig int
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			up++
		case r >= 'a' && r <= 'z':
			low++
		case r >= '0' && r <= '9':
			dig++
		}
	}
	return up > 0 && low > 0 && dig >= 2 && entropy(s) >= 4.0
}

// entropy is s's Shannon entropy in bits per character.
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

func trimWord(w string) string { return strings.Trim(w, "\"'`,;()[]{}<>") }

// secretWord reports a whole word that is a key by the server's shapes or a
// provider's prefix, or a key=value or key:value whose name says secret.
func secretWord(w string) bool {
	w = trimWord(w)
	if secretscan.MatchString(w) || keyPrefix.MatchString(w) {
		return true
	}
	if i := strings.IndexAny(w, "=:"); i > 0 && i+1 < len(w) && secretNameRx.MatchString(strings.TrimLeft(w[:i], "-")) {
		return true
	}
	// mysql -pS3cret, a password glued onto a short -p (not -print).
	return len(w) > 2 && strings.HasPrefix(w, "-p") && !strings.HasPrefix(w, "--") && strings.ContainsAny(w[2:], "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789!@#$%^&*")
}

// logTerms is the query terms of text (search.Terms, as the selection reads
// them) once its secrets are left out.
func logTerms(text string) []string {
	text = userinfo.ReplaceAllString(text, "://")
	words := strings.Fields(text)
	var kept []string
	for i := 0; i < len(words); i++ {
		w := words[i]
		if secretWord(w) {
			continue
		}
		// A secret's name: the value after it (and after "is", ":", "=") goes.
		if name := strings.TrimRight(strings.TrimLeft(trimWord(w), "-"), "=:"); secretNameRx.MatchString(name) {
			kept = append(kept, w)
			j := i + 1
			for j < len(words) && connective[strings.ToLower(trimWord(words[j]))] {
				j++
			}
			if j < len(words) && (strings.HasSuffix(w, "=") || strings.HasSuffix(w, ":") || strings.HasPrefix(w, "-") || j > i+1 || strings.EqualFold(name, "bearer")) {
				i = j
				// "Authorization: Bearer <token>": the scheme, then the value.
				if s := strings.ToLower(trimWord(words[j])); (s == "bearer" || s == "basic" || s == "token") && j+1 < len(words) {
					i = j + 1
				}
			}
			continue
		}
		var pieces []string
		for _, p := range strings.FieldsFunc(w, func(r rune) bool { return r == '/' || r == '.' || r == '-' }) {
			if !secretShaped(trimWord(p)) {
				pieces = append(pieces, p)
			}
		}
		kept = append(kept, strings.Join(pieces, " "))
	}
	return search.Terms(strings.Join(kept, " "))
}
