package digest

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestClaudeLines(t *testing.T) {
	requireScrump(t)
	in := strings.Join([]string{
		`{"type":"user","entrypoint":"cli","message":{"content":"please fix the build"}}`,
		`{"type":"user","message":{"content":"<task-notification>done</task-notification>"}}`,
		`{"type":"user","isMeta":true,"message":{"content":"injected"}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Running tests."},{"type":"tool_use","name":"Bash","input":{"command":"pkill -f job"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","is_error":true,"content":[{"type":"text","text":"Command exited with code 144 token=ghp_abcdefghijklmnopqrstuvwxyz1234"}]}]}}`,
		`{"type":"attachment"}`,
		`not json`,
	}, "\n")
	got, err := Finish(Lines(strings.NewReader(in), Claude, true))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"U: please fix the build",
		"H: <task-notification>done</task-notification>",
		"H: injected",
		"A: Running tests.",
		"T: Bash pkill -f job",
		"E: Command exited with code 144 token=" + strings.Repeat("*", 34),
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if !Interesting(got) {
		t.Fatal("digest with an owner line, an error and assistant text should be interesting")
	}
}

func TestCodexLines(t *testing.T) {
	in := strings.Join([]string{
		`{"type":"session_meta","payload":{"cwd":"/x"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":">>> RETAINED USER INSTRUCTIONS\nx"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"deploy it"}]}}`,
		`{"type":"response_item","payload":{"type":"function_call","name":"exec","arguments":"{\"cmd\":\"ls -t\"}"}}`,
		`{"type":"response_item","payload":{"type":"function_call_output","output":[{"type":"input_text","text":"Process exited with code 2: invalid value for --time"}]}}`,
		`{"type":"response_item","payload":{"type":"function_call_output","output":"all good"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Done."}]}}`,
	}, "\n")
	got := Lines(strings.NewReader(in), Codex, true)
	want := []string{
		"H: >>> RETAINED USER INSTRUCTIONS x",
		"U: deploy it",
		"T: exec ls -t",
		"E: Process exited with code 2: invalid value for --time",
		"A: Done.",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestRedact(t *testing.T) {
	requireScrump(t)
	// Redact runs scrump's text profile; the secret must be gone and the
	// surrounding text intact.
	for in, secret := range map[string]string{
		"key AKIAABCDEFGHIJKLMNOP here":                      "AKIAABCDEFGHIJKLMNOP",
		"api_key: supersecretvalue123":                       "supersecretvalue123",
		"Authorization: Bearer xxxxxxxxxxxxxxxxxxxxxxxx":     "xxxxxxxxxxxxxxxxxxxxxxxx",
		"token=" + strings.Repeat("ghp_abcdefghijklmnop", 2): "ghp_abcdefghijklmnop",
	} {
		out := Redact(in)
		if strings.Contains(out, secret) || !strings.Contains(out, "*") {
			t.Errorf("Redact(%q) = %q", in, out)
		}
	}
	if out := Redact("plain words only"); out != "plain words only" {
		t.Errorf("over-redacted: %q", out)
	}
}

// scrumpErr is why scrump with the text profile cannot run here, or nil.
var scrumpErr error

func TestMain(m *testing.M) {
	if scrumpErr = RedactionReady(); scrumpErr != nil {
		fmt.Fprintln(os.Stderr, "scrump with the text profile is not on PATH (install it from https://github.com/avifenesh/scrump); the redaction tests skip:", scrumpErr)
	}
	os.Exit(m.Run())
}

// requireScrump skips a test that needs the real redactor when it is absent.
func requireScrump(t *testing.T) {
	t.Helper()
	if scrumpErr != nil {
		t.Skip("needs scrump with the text profile: " + scrumpErr.Error())
	}
}

// A secret that straddles a digest line cap is still masked whole: the cut
// happens after redaction.
func TestFinishRedactsBeforeTheCut(t *testing.T) {
	requireScrump(t)
	key := "AKIA" + strings.Repeat("Q", 16)
	for _, pad := range []int{tMax - 10, tMax - 2, tMax + 5} {
		line := "T: Bash " + strings.Repeat("a ", pad/2) + "echo " + key + " done"
		out, err := Finish([]string{line})
		if err != nil {
			t.Fatal(err)
		}
		for i := 4; i <= len(key); i++ {
			if strings.Contains(out[0], key[:i]) && i >= 8 {
				t.Fatalf("pad %d: %d characters of the key survive: %q", pad, i, out[0])
			}
		}
		if r := []rune(out[0]); len(r) > tMax {
			t.Fatalf("pad %d: line not cut to its cap: %d", pad, len(r))
		}
	}
}

// Without scrump nothing passes through, and callers that must not proceed
// get an error.
func TestRedactFailsClosed(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	in := "pass" + "word=" + "Zq7Vx9" + "Lm2Kp4 and more"
	if out := Redact(in); out == in || strings.Contains(out, "Zq7Vx9") {
		t.Fatalf("passed through without scrump: %q", out)
	}
	if _, err := RedactErr(in); err == nil {
		t.Fatal("RedactErr returned no error without scrump")
	}
	if _, err := Finish([]string{"U: " + in}); err == nil {
		t.Fatal("Finish returned no error without scrump")
	}
	if err := RedactionReady(); err == nil {
		t.Fatal("RedactionReady passed without scrump")
	}
}

// A hit that spans lines or eats a record separator does not fail the
// window or the batch: the fallback redacts one record at a time.
func TestRedactionFallsBackPerRecord(t *testing.T) {
	requireScrump(t)
	block := "-----BEGIN " + "PRIVATE KEY-----"
	lines := []string{"U: " + block, "A: MIIEvQIBADANBgMIIEvQIBADANBg", "H: next line", "U: the db pass" + "word=" + "Zq7Vx9" + "Lm2Kp4"}
	out, err := Finish(lines)
	if err != nil || len(out) != len(lines) || strings.Contains(out[3], "Zq7Vx9") || out[2] != "H: next line" {
		t.Fatalf("Finish: %v %q", err, out)
	}
	recs, err := RedactAll([]string{"the db pass" + "word=" + "Zq7Vx9" + "Lm2Kp4", "Fix: rotate it", "q"})
	if err != nil || len(recs) != 3 || recs[1] != "Fix: rotate it" || recs[2] != "q" || strings.Contains(recs[0], "Zq7Vx9") {
		t.Fatalf("RedactAll: %v %q", err, recs)
	}
}
