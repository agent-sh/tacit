package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallPreservesOtherHooksInSameEntry(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hooks.json")
	os.WriteFile(p, []byte(`{"hooks":{"UserPromptSubmit":[{"hooks":[{"type":"command","command":"/old/tacit hook prompt"},{"type":"command","command":"guard.py"}]}]}}`), 0600)
	if err := installHooksFor(p, "/bin/tacit", "linux"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	if !strings.Contains(string(got), "guard.py") {
		t.Fatalf("install silently deleted unrelated hook: %s", got)
	}
}
