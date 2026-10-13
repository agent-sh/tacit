package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Someone else's unwritten lines in today's review file are not committed
// with ours: the owner lines wait for a later run, the facts are still repaired.
func TestLinksDoNotCommitSomeoneElsesReviewLines(t *testing.T) {
	r := linkFixture(t, true)
	p := filepath.Join(r.clone, "review", "2026-10-10.md")
	os.MkdirAll(filepath.Dir(p), 0o755)
	mine := "## a note someone is typing\n\n- supersedes? [[a]] may replace [[b]]; owner decides\n"
	os.WriteFile(p, []byte(mine), 0o644)

	rep, err := r.links(false)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Deferred || !rep.Pushed {
		t.Fatalf("deferred=%v pushed=%v", rep.Deferred, rep.Pushed)
	}
	if got, _ := os.ReadFile(p); string(got) != mine {
		t.Fatalf("the file was touched:\n%s", got)
	}
	if out := gitIn(t, r.bare, "ls-tree", "-r", "--name-only", "main", "review"); strings.Contains(out, "2026-10-10.md") {
		t.Fatalf("the server holds the file: %s", out)
	}
	if !strings.Contains(r.onServer("projects/x/uses-old.md"), "[[new-engine]]") {
		t.Fatal("the facts were not repaired")
	}
	// Once the file is gone the lines are raised.
	os.Remove(p)
	if rep, err := r.links(false); err != nil || !rep.Pushed || strings.Count(r.onServer("review/2026-10-10.md"), "- LINK? ") != 2 {
		t.Fatalf("pushed=%v err=%v", rep != nil && rep.Pushed, err)
	}
}
