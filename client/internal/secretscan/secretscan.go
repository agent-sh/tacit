// Package secretscan is the server's pre-receive secret scan
// (server/priv/hooks/pre-receive) in Go, for the client's checks that must
// refuse what the server refuses: the names a derived fact may show, and a
// local store's commits. The three expressions are the hook's, byte for byte;
// the test holds them equal.
//
// Token shapes and private-key headers match Shapes. An sk- key takes two
// steps: a token is sk- and at least 16 key characters ([A-Za-z0-9_-]), and
// a token is a key when that run holds an uppercase letter or underscore, a
// run of 20 letters and digits, or a lowercase UUID. A fact name is lowercase
// letters, digits and hyphens, so a slug with an sk- segment is no key.
package secretscan

import "regexp"

var (
	// Shapes is the hook's secret_re: GitHub, AWS, Slack, Tailscale and
	// Hugging Face token shapes and private-key headers.
	Shapes = regexp.MustCompile(`(ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|AKIA[0-9A-Z]{16}|xox[abprs]-[A-Za-z0-9-]{10,}|tskey-[A-Za-z0-9-]{10,}|(^|[^A-Za-z0-9])hf_[A-Za-z0-9]{20,}|-----BEGIN [A-Z ]*PRIVATE KEY-----`)
	// SkToken is the hook's sk_token_re: sk- and 16 or more key characters.
	SkToken = regexp.MustCompile(`(^|[^A-Za-z0-9])sk-[A-Za-z0-9_-]{16,}`)
	// SkKey is the hook's sk_key_re, matched against each token.
	SkKey = regexp.MustCompile(`sk-[A-Za-z0-9_-]*([A-Z_]|[A-Za-z0-9]{20}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})`)
)

// Match reports whether b holds what the hook refuses.
func Match(b []byte) bool {
	if Shapes.Match(b) {
		return true
	}
	for _, t := range SkToken.FindAll(b, -1) {
		if SkKey.Match(t) {
			return true
		}
	}
	return false
}

// MatchString is Match for a string.
func MatchString(s string) bool { return Match([]byte(s)) }
