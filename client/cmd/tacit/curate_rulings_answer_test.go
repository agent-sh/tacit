package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rulingsAnswers makes the stand-in CLI of quickHome answer the rulings pass
// with first, and its retry (the prompt code sends back) with retry; every
// other prompt is answered as quickHome does. A transcript with one turn of
// the owner's gives the machine owner history, so the rulings pass runs.
func rulingsAnswers(t *testing.T, home, first, retry string) {
	t.Helper()
	tr := filepath.Join(home, ".claude", "projects", "-home-u", "s1.jsonl")
	os.MkdirAll(filepath.Dir(tr), 0o700)
	os.WriteFile(tr, []byte(`{"type":"user","entrypoint":"cli","sessionId":"s1","message":{"content":"keep the quokka driver on its own port please"}}`+"\n"), 0o600)
	t.Setenv("FIXTURE_RULINGS", first)
	t.Setenv("FIXTURE_RULINGS_RETRY", retry)
	fake := os.Getenv("TACIT_CLAUDE_COMMAND")
	b, err := os.ReadFile(fake)
	if err != nil {
		t.Fatal(err)
	}
	s := strings.Replace(string(b), `case "$in" in
`, `case "$in" in
*"CODE REFUSED THAT ANSWER"*"OWNER RULINGS already extracted"*|*"OWNER RULINGS already extracted"*"CODE REFUSED THAT ANSWER"*) printf '%s\n' "$FIXTURE_RULINGS_RETRY" ;;
*"OWNER RULINGS already extracted"*) printf '%s\n' "$FIXTURE_RULINGS" ;;
`, 1)
	if s == string(b) {
		t.Fatal("the stand-in CLI has no case to extend")
	}
	os.WriteFile(fake, []byte(s), 0o700)
}

// cliResult is the claude CLI's JSON result line for text.
func cliResult(text string) string {
	return `{"result":` + jsonString(text) + `,"total_cost_usd":0.01}`
}

func jsonString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return `"` + r.Replace(s) + `"`
}

// The rulings pass of a note with no owner words is most often empty, and
// the model put a sentence before half of those answers: "The note quotes no
// owner words, so there is nothing to extract." and the array in a fence.
// Code refuses that answer whole. It goes back to the model once, saying why,
// and the clean answer is taken: the quick run curates the note and exits 0.
// Before v0.9.16 it was not sent back; a note alone in its batch failed every
// quick run (exit 1) until the full run took it a day later.
func TestQuickCurateSendsBackARulingsAnswerThatIsNoArray(t *testing.T) {
	home, prompts := quickHome(t)
	rulingsAnswers(t, home, cliResult("The note quotes no owner words, so there is nothing to extract.\n\n```json\n[]\n```"), cliResult("[]"))
	var code int
	errs := captureStderr(t, func() { code = runCurate([]string{"--quick"}) })
	if code != 0 {
		t.Fatalf("quick exit %d:\n%s", code, errs)
	}
	if !strings.Contains(errs, "curate: invalid answer: rulings (sent back): ") {
		t.Fatalf("the run does not say it sent the answer back:\n%s", errs)
	}
	sentBack := 0
	for _, p := range prompts() {
		if strings.Contains(p, "OWNER RULINGS already extracted") && strings.Contains(p, "CODE REFUSED THAT ANSWER") && strings.Contains(p, "curator must return a JSON array") {
			sentBack++
		}
	}
	if sentBack != 1 {
		t.Fatalf("%d rulings answer(s) sent back, want 1", sentBack)
	}
	if _, err := os.Stat(filepath.Join(home, "store", "system", "fresh-note-fact.md")); err != nil {
		t.Fatalf("the note was not curated: %v\n%s", err, errs)
	}
}

// A model whose second answer is no JSON array either fails the run: the
// batch of one note cannot be split, nothing is written, and the run exits 1.
func TestQuickCurateFailsWhenTheRulingsAnswerIsNoArrayTwice(t *testing.T) {
	home, _ := quickHome(t)
	prose := cliResult("Nothing to extract here. []")
	rulingsAnswers(t, home, prose, prose)
	var code int
	errs := captureStderr(t, func() { code = runCurate([]string{"--quick"}) })
	if code != 1 || !strings.Contains(errs, "curate: rulings: invalid model result: curator must return a JSON array") {
		t.Fatalf("quick exit %d:\n%s", code, errs)
	}
	if _, err := os.Stat(filepath.Join(home, "store", "system", "fresh-note-fact.md")); err == nil {
		t.Fatal("a note whose rulings pass failed was curated")
	}
}
