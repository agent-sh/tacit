package secretscan

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The Go scan is the server's: a name or file the hook refuses must be
// refused here too (a derived fact naming it would get the curator's whole
// push refused), and one it takes must be taken.
func TestTheExpressionsAreTheHooks(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "server", "priv", "hooks", "pre-receive"))
	if err != nil {
		t.Fatal(err)
	}
	for name, re := range map[string]*regexp.Regexp{"secret_re": Shapes, "sk_token_re": SkToken, "sk_key_re": SkKey} {
		m := regexp.MustCompile(`(?m)^` + name + `='([^']*)'`).FindSubmatch(b)
		if m == nil {
			t.Fatalf("%s not found in the pre-receive hook", name)
		}
		if string(m[1]) != re.String() {
			t.Fatalf("%s differs from the hook's:\ngo:   %s\nhook: %s", name, re, m[1])
		}
	}
}

// Fact names with an sk- segment are no key, whatever their length; a key
// is refused wherever it sits, also in a short token and on a long line.
func TestFactNamesPassKeysDoNot(t *testing.T) {
	for _, s := range []string{
		"- [[lint-cc-sk-017-only-plain-commands]]: a fact",
		"sk-learn-pipeline-cache-notes",
		"- [docs-sk-locale-fallback-order-v2](docs-sk-locale-fallback-order-v2.md)",
		"task-sk-is-not-a-key-either-way and disk-sk-",
		strings.Repeat("sk-", 100_000),
	} {
		if MatchString(s) {
			t.Errorf("refused %.60q", s)
		}
	}
	for _, s := range []string{
		"API_KEY=sk-" + strings.Repeat("ab12", 4) + "Q",         // 16 characters, one uppercase
		"sk-" + strings.Repeat("abcd-", 4) + "_x",               // an underscore
		"sk-" + strings.Repeat("a1", 10),                        // 20 letters and digits
		"sk-lf-00000000-0000-4000-8000-000000000000",            // a lowercase UUID
		strings.Repeat("sk-", 50_000) + "sk-abcdefgh-ijklmnopQ", // after a long run of sk-
		"token: ghp_" + strings.Repeat("Q", 36),
	} {
		if !MatchString(s) {
			t.Errorf("took %.60q", s)
		}
	}
}

// Linear in the input: a long line of sk- runs that holds no key costs what
// any line of its size does.
func TestALongLineIsScannedInLinearTime(t *testing.T) {
	line := []byte(strings.Repeat("sk-", 2_200_000)) // 6.6 MB, one line
	start := time.Now()
	if Match(line) {
		t.Fatal("refused a line of sk- runs")
	}
	if d := time.Since(start); d > 20*time.Second {
		t.Fatalf("6.6 MB took %s", d)
	}
}
