package store

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// syntheticKeys builds random keys of the shapes providers issue, never a
// real one: Anthropic and OpenAI (gitleaks' current rules), the legacy 48
// characters, 32 hex, OpenRouter, Langfuse and LiteLLM, and an Anthropic body
// with no 20-character alphanumeric run. The fixed keys at the end can each
// be caught by one arm of the rule only: an uppercase letter or underscore
// anywhere in the run (at its start, its middle, its end, in a 16-character
// token), and a UUID.
func syntheticKeys(seed uint64) map[string]string {
	r := rand.New(rand.NewPCG(seed, 7))
	pick := func(alphabet string, n int) string {
		var b strings.Builder
		for range n {
			b.WriteByte(alphabet[r.IntN(len(alphabet))])
		}
		return b.String()
	}
	const (
		b64url = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
		alnum  = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
		hex    = "0123456789abcdef"
	)
	var chunks []string
	for len(strings.Join(chunks, "-")) < 93 {
		chunks = append(chunks, pick(alnum, 19))
	}
	return map[string]string{
		"anthropic api03":   "sk-ant-api03-" + pick(b64url, 93) + "AA",
		"anthropic admin01": "sk-ant-admin01-" + pick(b64url, 93) + "AA",
		"anthropic no run":  "sk-ant-api03-" + strings.Join(chunks, "-")[:93] + "AA",
		"openai project":    "sk-proj-" + pick(b64url, 74) + "T3BlbkFJ" + pick(b64url, 74),
		"openai service":    "sk-svcacct-" + pick(b64url, 58) + "T3BlbkFJ" + pick(b64url, 58),
		"openai legacy":     "sk-" + pick(alnum, 20) + "T3BlbkFJ" + pick(alnum, 20),
		"legacy 48":         "sk-" + pick(alnum, 48),
		"32 hex":            "sk-" + pick(hex, 32),
		"openrouter":        "sk-or-v1-" + pick(hex, 64),
		"langfuse":          fmt.Sprintf("sk-lf-%s-%s-4%s-a%s-%s", pick(hex, 8), pick(hex, 4), pick(hex, 3), pick(hex, 3), pick(hex, 12)),
		"litellm":           "sk-" + pick(b64url, 22),
		"mark at start":     "sk-Qabcd-efghi-jklmn-opqrs",
		"mark in middle":    "sk-abcd-efghi_jklm-nopqrs",
		"mark at end":       "sk-abcd-efghi-jklmn-opqrsQ",
		"short token":       "sk-abcd-efgh-ijkLmn",
		"uuid only":         "sk-lf-00000000-0000-4000-8000-000000000000",
	}
}

// Fact names are lowercase letters, digits and hyphens. One with an sk-
// segment is no key: v0.9.14 refused every push that held one, and the
// curator's batches never landed. Real key shapes are still refused.
func TestLocalCommitTakesSkFactNamesAndRefusesSkKeys(t *testing.T) {
	ctx := context.Background()
	s := &Store{Dir: filepath.Join(t.TempDir(), "facts")}
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	names := []string{"lint-cc-sk-017-only-plain-commands", "sk-learn-pipeline-cache-notes", "docs-sk-locale-fallback-order-v2"}
	var b strings.Builder
	for _, n := range names {
		fmt.Fprintf(&b, "- [[%s]]: a fact\n- [%s](%s.md) - a fact\n", n, n, n)
	}
	rel := "projects/lint/" + names[0] + ".md"
	os.MkdirAll(filepath.Dir(filepath.Join(s.Dir, rel)), 0o700)
	os.WriteFile(filepath.Join(s.Dir, rel), []byte("---\nname: "+names[0]+"\n---\n\n"+b.String()), 0o600)
	if err := s.CommitPush(ctx, []string{rel}, "names", "curator"); err != nil {
		t.Fatalf("fact names with sk- refused: %v", err)
	}
	for seed := uint64(1); seed <= 20; seed++ {
		for shape, key := range syntheticKeys(seed) {
			before, _ := s.Head(ctx)
			p := "keys/" + strings.ReplaceAll(shape, " ", "-") + ".md"
			os.MkdirAll(filepath.Join(s.Dir, "keys"), 0o700)
			os.WriteFile(filepath.Join(s.Dir, p), []byte("export KEY=\""+key+"\"\n"), 0o600)
			err := s.CommitPush(ctx, []string{p}, "key", "curator")
			if _, ok := err.(*Rejected); !ok {
				t.Fatalf("seed %d: %s key was not refused (%v): %s", seed, shape, err, key)
			}
			if head, _ := s.Head(ctx); head != before {
				t.Fatalf("seed %d: refused %s key moved HEAD", seed, shape)
			}
		}
	}
}
