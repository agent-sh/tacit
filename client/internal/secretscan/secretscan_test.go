package secretscan

import (
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The Go scan is the server's: a name or file the hook refuses must be
// refused here too (a derived fact naming it would get the curator's whole
// push refused), and one it takes must be taken.
func TestTheExpressionsAreTheHooks(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "server", "priv", "hooks", "pre-receive"))
	if err != nil {
		t.Fatal(err)
	}
	exprs := map[string]*regexp.Regexp{"secret_re": Shapes}
	for _, f := range Families {
		exprs[f.Name+"_token_re"], exprs[f.Name+"_key_re"] = f.Token, f.Key
	}
	if len(exprs) != 7 {
		t.Fatalf("%d expressions, want secret_re and three families", len(exprs))
	}
	for name, re := range exprs {
		m := regexp.MustCompile(`(?m)^` + name + `='([^']*)'`).FindSubmatch(b)
		if m == nil {
			t.Fatalf("%s not found in the pre-receive hook", name)
		}
		if string(m[1]) != re.String() {
			t.Fatalf("%s differs from the hook's:\ngo:   %s\nhook: %s", name, re, m[1])
		}
	}
	// The hook skips the family steps for a blob with no family's token: its
	// gate is the union of the token expressions, in the loop's order.
	var union []string
	for _, f := range Families {
		union = append(union, "$"+f.Name+"_token_re")
	}
	if want := `any_token_re="` + strings.Join(union, "|") + `"`; !regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(want) + `$`).Match(b) {
		t.Fatalf("the hook's gate is not %s", want)
	}
	// The hook scans the families its loop names, in this order.
	var names []string
	for _, f := range Families {
		names = append(names, f.Name)
	}
	if m := regexp.MustCompile(`(?m)^\s*for family in ([a-z ]+); do$`).FindSubmatch(b); m == nil || string(m[1]) != strings.Join(names, " ") {
		t.Fatalf("the hook's family loop is %q, want %q", m, strings.Join(names, " "))
	}
}

// A fact name with a tskey- or a Slack segment (xoxb-, xoxp-, xapp-, ...) is
// no key: the rule before v0.9.16 took the prefix and 10 key characters for
// one, as sk- was before v0.9.15. A key is refused by each arm alone.
func TestSlackAndTailscaleNamesPassKeysDoNot(t *testing.T) {
	for _, s := range []string{
		"- [[tskey-auth-expires-in-90-days]]: a fact",
		"use-tskey-auth-for-ci-runners-not-oauth-clients",
		"- [slack-xoxb-token-scopes-for-the-bot](slack-xoxb-token-scopes-for-the-bot.md) - a fact",
		"name: xoxp-user-token-needs-chat-write-2026-10-28",
		"xapp-level-token-for-socket-mode and xoxe-refresh-rotation-every-12-hours",
		"xoxa-xoxr-xoxs-legacy-tokens-are-retired",
		"- [[slack-xoxa-2-workspace-tokens-are-retired]]: a fact",
		"slack-xoxr-1-refresh-for-workspace-apps and xoxd-session-cookie-notes",
		strings.Repeat("xoxb-", 100_000),
		strings.Repeat("tskey-", 100_000),
	} {
		if MatchString(s) {
			t.Errorf("refused %.60q", s)
		}
	}
	for _, s := range []string{
		"SLACK=xoxb-abcd-efgh-ijkL",                            // an uppercase letter
		"xoxa-" + strings.Repeat("ab", 10),                     // 20 letters and digits
		"xoxs-0123456789-abc",                                  // 10 digits
		"xapp-1-" + strings.Repeat("c", 20),                    // the app-level prefix
		"xoxe-1-" + strings.Repeat("d", 20),                    // the refresh prefix
		"xoxe.xoxp-1-" + strings.Repeat("e", 20),               // a configuration token
		"TS_AUTHKEY=tskey-auth-abcdeCNTRL-xyz",                 // its id's CNTRL
		"tskey-" + strings.Repeat("a1", 10),                    // 20 letters and digits
		"xoxs-012345-678901-abc",                               // two ids of 6 digits
		"xoxa-1-abcdefgh",                                      // a legacy workspace token's version
		"xoxd-Ab3%2Fq9ZxYwVuTsRqPoNmLkJiHgFeDcBa%2B0123456789", // a session cookie, encoded early
		"https:\\/\\/hooks.slack.com\\/services\\/T0\\/B0\\/" + strings.Repeat("x", 40), // a webhook URL in JSON
		"xoxo-0123456789-0123456789-abc",                                                // the legacy xoxo- prefix
		"https://hooks.slack.com/services/T0/B0/" + strings.Repeat("x", 40),             // a webhook URL
		strings.Repeat("tskey-", 50_000) + "tskey-abcdefghijQ",                          // after a long run
	} {
		if !MatchString(s) {
			t.Errorf("took %.60q", s)
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
		"API_KEY=sk-abcd-efgh-ijkLmn",                           // a 16-character token, one uppercase letter
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

// The Go scan decides as the hook's grep steps do, on inputs built from the
// pieces where the two could differ: line breaks, NUL and bytes that are not
// UTF-8, tokens at each family's thresholds (16 or 10 key characters, a run of
// 20, 10 digits), UUIDs, and the other shapes.
func TestGoDecidesAsTheHooksGrep(t *testing.T) {
	out, err := exec.Command("grep", "--version").Output()
	if err != nil || !strings.Contains(string(out), "GNU grep") {
		t.Skip("needs GNU grep, the hook's engine")
	}
	hook, err := os.ReadFile(filepath.Join("..", "..", "..", "server", "priv", "hooks", "pre-receive"))
	if err != nil {
		t.Fatal(err)
	}
	expr := func(name string) string {
		return string(regexp.MustCompile(`(?m)^` + name + `='([^']*)'`).FindSubmatch(hook)[1])
	}
	shapes := expr("secret_re")
	dir := t.TempDir()
	blob, tokens := filepath.Join(dir, "blob"), filepath.Join(dir, "tokens")
	grep := func(args ...string) bool {
		cmd := exec.Command("grep", args...)
		cmd.Env = append(os.Environ(), "LC_ALL=C")
		err := cmd.Run()
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return false
		}
		if err != nil {
			t.Fatal(err)
		}
		return true
	}
	var union []string
	for _, f := range Families {
		union = append(union, expr(f.Name+"_token_re"))
	}
	gate := strings.Join(union, "|")
	hookRefuses := func() bool {
		if grep("-aEq", "--", shapes, blob) {
			return true
		}
		if !grep("-aEq", "--", gate, blob) {
			return false
		}
		for _, f := range Families {
			out, err := exec.Command("sh", "-c", `LC_ALL=C grep -aEo -- "$1" "$2" > "$3"; s=$?; [ $s -le 1 ] && exit 0; exit $s`, "sh", expr(f.Name+"_token_re"), blob, tokens).CombinedOutput()
			if err != nil {
				t.Fatalf("%s token step: %v %s", f.Name, err, out)
			}
			if grep("-aEq", "--", expr(f.Name+"_key_re"), tokens) {
				return true
			}
		}
		return false
	}
	// Short pieces and many hyphens keep runs under 20, so each arm decides
	// on its own; every input holds a token of one family.
	pieces := []string{"sk-", "xoxb-", "xapp-", "tskey-", "-", "-", "_", "ab", "Q", "0", "abcd", "xyz9", "\n", "\r\n", "\x00", "\xff", "\xc3", "é", " ", "=",
		strings.Repeat("a1", 10), "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d", "0A1B2C3D-4E5F", "0123456789", "012345678", "ghp_" + strings.Repeat("z", 20),
		"AKIA" + strings.Repeat("Z", 16), "hf_" + strings.Repeat("y", 20)}
	body := []string{"ab", "Q", "0", "abcd", "xyz9", "01234", "56789", "a1a1a1a1a1", "012"}
	starts := []string{"sk-", "xoxb-", "xoxs-", "xapp-", "xoxe-", "xoxa-1-", "xoxo-", "tskey-"}
	r := rand.New(rand.NewPCG(3, 9))
	// Per start, the inputs a family step refused that secret_re did not:
	// each start's own arms decide some inputs alone.
	refused := map[string]int{}
	for i := 0; i < 3000; i++ {
		var b strings.Builder
		for n := r.IntN(12) + 1; n > 0; n-- {
			b.WriteString(pieces[r.IntN(len(pieces))])
		}
		prefix := starts[r.IntN(len(starts))]
		b.WriteString(prefix)
		for n := r.IntN(10) + 2; n > 0; n-- {
			b.WriteString(body[r.IntN(len(body))])
			b.WriteString([]string{"-", "-", "", "_"}[r.IntN(4)])
		}
		in := []byte(b.String())
		if err := os.WriteFile(blob, in, 0o600); err != nil {
			t.Fatal(err)
		}
		got, want := Match(in), hookRefuses()
		if got != want {
			t.Fatalf("input %q: Go says %v, the hook's grep %v", in, got, want)
		}
		if got && !Shapes.Match(in) {
			refused[prefix]++
		}
	}
	t.Logf("refused by a family step alone: %v", refused)
	for _, p := range starts {
		if n := refused[p]; n < 40 || n > 330 {
			t.Fatalf("%d of about 375 %s inputs refused by a family step alone (%v): the inputs do not exercise both outcomes", n, p, refused)
		}
	}
}
