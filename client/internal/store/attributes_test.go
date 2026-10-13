package store

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Each union line is checked on its own: a store with the older two lines
// gets the quote ledger's, once.
func TestEnsureAttributesAddsEachMissingLine(t *testing.T) {
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	p := filepath.Join(dir, ".gitattributes")
	os.WriteFile(p, []byte("review/decisions.jsonl merge=union\nreview/*.md merge=union\n"), 0o644)
	s := &Store{Dir: dir}
	s.ensureAttributes(context.Background())
	s.ensureAttributes(context.Background())
	b, _ := os.ReadFile(p)
	if strings.Count(string(b), "review/owner-quotes.jsonl merge=union") != 1 || strings.Count(string(b), "review/decisions.jsonl merge=union") != 1 {
		t.Fatalf(".gitattributes:\n%s", b)
	}
}
