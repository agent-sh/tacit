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
// UTF-8, tokens at the 16 and 20 thresholds, UUIDs, and the other shapes.
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
	shapes, token, key := expr("secret_re"), expr("sk_token_re"), expr("sk_key_re")
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
	hookRefuses := func() bool {
		if grep("-aEq", "--", shapes, blob) {
			return true
		}
		out, err := exec.Command("sh", "-c", `LC_ALL=C grep -aEo -- "$1" "$2" > "$3"; s=$?; [ $s -le 1 ] && exit 0; exit $s`, "sh", token, blob, tokens).CombinedOutput()
		if err != nil {
			t.Fatalf("token step: %v %s", err, out)
		}
		return grep("-aEq", "--", key, tokens)
	}
	// Short pieces and many hyphens keep runs under 20, so each arm decides
	// on its own; every input holds an sk- token.
	pieces := []string{"sk-", "-", "-", "-", "_", "ab", "Q", "0", "abcd", "xyz9", "\n", "\r\n", "\x00", "\xff", "\xc3", "é", " ", "=",
		strings.Repeat("a1", 10), "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d", "0A1B2C3D-4E5F", "ghp_" + strings.Repeat("z", 20),
		"xoxb-", "AKIA" + strings.Repeat("Z", 16), "hf_" + strings.Repeat("y", 20)}
	r := rand.New(rand.NewPCG(3, 9))
	refused := 0
	for i := 0; i < 1500; i++ {
		var b strings.Builder
		for n := r.IntN(12) + 1; n > 0; n-- {
			b.WriteString(pieces[r.IntN(len(pieces))])
		}
		b.WriteString("sk-")
		for n := r.IntN(14) + 4; n > 0; n-- {
			b.WriteString(pieces[5+r.IntN(4)])
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
		if got {
			refused++
		}
	}
	if refused < 300 || refused > 1200 {
		t.Fatalf("%d of 1500 inputs refused: the inputs do not exercise both outcomes", refused)
	}
}
