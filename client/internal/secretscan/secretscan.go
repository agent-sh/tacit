// Package secretscan is the server's pre-receive secret scan
// (server/priv/hooks/pre-receive) in Go, for the client's checks that must
// refuse what the server refuses: the names a derived fact may show, and a
// local store's commits. The expressions are the hook's, byte for byte; the
// test holds them equal.
//
// Token shapes and private-key headers match Shapes. Three families of keys
// take two steps, since a fact name can start like one of them: a token is the
// family's prefix and a run of key characters, and a token is a key when that
// run holds what a real key has. An sk- token is 16 key characters
// ([A-Za-z0-9_-]) and a key holds an uppercase letter or underscore, a run of
// 20 letters and digits, or a lowercase UUID. A Slack token (xoxa-, xoxb-,
// xoxc-, xoxd-, xoxe-, xoxo-, xoxp-, xoxr-, xoxs-, xapp-) or a Tailscale one
// (tskey-) is 10 key characters ([A-Za-z0-9-]); a Slack key holds an
// uppercase letter, a run of 20 letters and digits, a run of 10 digits or two
// runs of 6 joined by a hyphen, or is a legacy workspace token's
// xoxa-<digit>- or xoxr-<digit>-; a Tailscale key holds an uppercase letter
// or a run of 20 letters and digits. A fact name is lowercase letters, digits
// and hyphens, so a slug with such a segment is no key.
//
// The hook greps a blob for any family's prefix (family_prefix_re) before
// the family steps, for speed; Match needs no such step, and the test holds
// the two decisions equal.
package secretscan

import "regexp"

var (
	// Shapes is the hook's secret_re: GitHub, AWS and Hugging Face token
	// shapes, Slack webhook URLs and private-key headers.
	Shapes = regexp.MustCompile(`(ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|AKIA[0-9A-Z]{16}|(^|[^A-Za-z0-9])hf_[A-Za-z0-9]{20,}|hooks\.slack\.com/(services|workflows|triggers)/[A-Za-z0-9+/]{43,}|-----BEGIN [A-Z ]*PRIVATE KEY-----`)
	// SkToken is the hook's sk_token_re: sk- and 16 or more key characters.
	SkToken = regexp.MustCompile(`(^|[^A-Za-z0-9])sk-[A-Za-z0-9_-]{16,}`)
	// SkKey is the hook's sk_key_re, matched against each token.
	SkKey = regexp.MustCompile(`sk-[A-Za-z0-9_-]*([A-Z_]|[A-Za-z0-9]{20}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})`)
	// SlackToken is the hook's slack_token_re: a Slack prefix and 10 or more
	// key characters.
	SlackToken = regexp.MustCompile(`(xox[abcdeoprs]|xapp)-[A-Za-z0-9-]{10,}`)
	// SlackKey is the hook's slack_key_re, matched against each token.
	SlackKey = regexp.MustCompile(`(xox[abcdeoprs]|xapp)-[A-Za-z0-9-]*([A-Z]|[A-Za-z0-9]{20}|[0-9]{10}|[0-9]{6}-[0-9]{6})|xox[ar]-[0-9]-`)
	// TskeyToken is the hook's tskey_token_re: tskey- and 10 or more key
	// characters.
	TskeyToken = regexp.MustCompile(`tskey-[A-Za-z0-9-]{10,}`)
	// TskeyKey is the hook's tskey_key_re, matched against each token.
	TskeyKey = regexp.MustCompile(`tskey-[A-Za-z0-9-]*([A-Z]|[A-Za-z0-9]{20})`)
)

// Family is one kind of key the hook takes in two steps: Token finds the
// candidates (grep -o), Key decides each one.
type Family struct {
	Name       string // the hook's variable prefix: <name>_token_re, <name>_key_re
	Token, Key *regexp.Regexp
}

// Families are the hook's two-step families, in the order it scans them.
var Families = []Family{{"sk", SkToken, SkKey}, {"slack", SlackToken, SlackKey}, {"tskey", TskeyToken, TskeyKey}}

// Match reports whether b holds what the hook refuses.
func Match(b []byte) bool {
	if Shapes.Match(b) {
		return true
	}
	for _, f := range Families {
		for _, t := range f.Token.FindAll(b, -1) {
			if f.Key.Match(t) {
				return true
			}
		}
	}
	return false
}

// MatchString is Match for a string.
func MatchString(s string) bool { return Match([]byte(s)) }
