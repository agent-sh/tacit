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
// value after a password, token or key name, a URL's user and password or
// secret query value, curl's -u, a password glued onto -p) and its shape (the server's own key shapes, known
// key prefixes, long hex runs, random-looking runs of letters and digits).
// No redaction process runs in a hook.

// secretNameRx is a name whose value is a secret, at a word boundary:
// password, --api-key, secret_key, "Authorization", session (not bypass,
// oauth). envSecretRx is an environment variable's name that ends in one
// (PGPASSWORD, DJANGO_SECRET_KEY).
var (
	secretNameRx = regexp.MustCompile(`(?i)(?:^|[_-])(?:pass(?:word|wd|phrase)?|pwd|secrets?|tokens?|(?:api|access|secret|signing|encryption|master|client|private)[_-]?keys?|auth(?:orization)?|bearer|credentials?|cookie|session(?:id)?)$`)
	envSecretRx  = regexp.MustCompile(`^[A-Z0-9_]*(?:PASSWORD|PASSWD|PASS|PWD|SECRET|TOKEN|KEY)$`)
)

// isSecretName reports a name (quotes, dashes and a trailing = or : aside)
// whose value is a secret.
func isSecretName(n string) bool {
	n = strings.Trim(n, "\"'`{[(,-=:")
	return n != "" && (secretNameRx.MatchString(n) || envSecretRx.MatchString(n))
}

// keyPrefix starts a word that is a provider's key, with a tail of key
// characters long enough to be one (a name like hf_hub_download is no key).
var keyPrefix = regexp.MustCompile(`^(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|glpat-[A-Za-z0-9_-]{20,}|(?:sk|rk)_(?:live|test)_[A-Za-z0-9]{16,}|sk-[A-Za-z0-9_-]{16,}|xox[abceoprs]-[A-Za-z0-9-]{10,}|xapp-[A-Za-z0-9-]{10,}|tskey-[A-Za-z0-9-]{10,}|(?:AKIA|ASIA)[A-Z0-9]{16}|AIza[A-Za-z0-9_-]{30,}|ya29\.[A-Za-z0-9_-]{20,}|eyJ[A-Za-z0-9_-]{10,}\.|npm_[A-Za-z0-9]{30,}|pypi-[A-Za-z0-9_-]{30,}|hf_[A-Za-z0-9]{30,}|-----BEGIN)`)

var (
	hexRun = regexp.MustCompile(`[0-9a-fA-F]{32,}`)
	// keyRun is a run of key characters with no path or file separator.
	keyRun   = regexp.MustCompile(`[A-Za-z0-9+_=]{20,}`)
	userinfo = regexp.MustCompile(`://[^/@\s]*@`)
	// queryParam is a URL query's name=value (?access_token=...).
	queryParam = regexp.MustCompile(`([?&;])([A-Za-z0-9_.-]+)=([^&;#\s]*)`)
	// wordRun is a run of key characters, path and URL separators included,
	// judged whole: a base64 key split at / or - is still one key.
	wordRun = regexp.MustCompile(`[A-Za-z0-9+/_=-]{20,}`)
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

func digits(s string) int {
	n := 0
	for _, r := range s {
		if r >= '0' && r <= '9' {
			n++
		}
	}
	return n
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
	if i := strings.IndexAny(w, "=:"); i > 0 && i+1 < len(w) && isSecretName(w[:i]) {
		return true
	}
	// A key in base64 or base64url is one run, however / and - split it.
	for _, m := range wordRun.FindAllString(w, -1) {
		if k := strings.Map(func(r rune) rune {
			if strings.ContainsRune("+/_=-", r) {
				return -1
			}
			return r
		}, m); len(k) >= 20 && randomLooking(k) && digits(k) >= 3 {
			return true
		}
	}
	// mysql -pS3cret, a password glued onto a short -p (not -print).
	return len(w) > 2 && strings.HasPrefix(w, "-p") && !strings.HasPrefix(w, "--") && strings.ContainsAny(w[2:], "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789!@#$%^&*")
}

// logTerms is the query terms of text (search.Terms, as the selection reads
// them) once its secrets are left out.
func logTerms(text string) []string {
	text = userinfo.ReplaceAllString(text, "://")
	text = queryParam.ReplaceAllStringFunc(text, func(m string) string {
		if p := queryParam.FindStringSubmatch(m); isSecretName(p[2]) {
			return p[1] + p[2] + "="
		}
		return m
	})
	words := strings.Fields(text)
	var kept []string
	for i := 0; i < len(words); i++ {
		w := words[i]
		if secretWord(w) {
			continue
		}
		// curl -u user:password: the credentials go.
		if t := trimWord(w); (t == "-u" || t == "--user") && i+1 < len(words) {
			kept = append(kept, w)
			i++
			continue
		}
		// A secret's name: the value after it (and after "is", ":", "=") goes.
		if name := trimWord(w); isSecretName(name) {
			kept = append(kept, w)
			j := i + 1
			for j < len(words) && connective[strings.ToLower(trimWord(words[j]))] {
				j++
			}
			if j < len(words) && (strings.HasSuffix(strings.Trim(w, "\"'"), "=") || strings.HasSuffix(strings.Trim(w, "\"'"), ":") || strings.HasPrefix(w, "-") || j > i+1 || strings.EqualFold(strings.Trim(name, "\"'"), "bearer")) {
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
