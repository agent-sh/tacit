package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A refusal carries the lines the server printed, so an operator sees which
// file and which rule, not only "pre-receive hook declined". The first of
// those lines arrives behind git's own "git HEAD:main: " prefix.
func TestRejectedCarriesTheServersLines(t *testing.T) {
	root := t.TempDir()
	bare := filepath.Join(root, "store.git")
	run(t, root, "init", "-q", "--bare", "-b", "main", bare)
	hook := filepath.Join(bare, "hooks", "pre-receive")
	script := "#!/bin/sh\nwhile read o n r; do git diff-tree --no-commit-id --name-only -r \"$n\" | grep -q evil && { echo 'tacit: inbox/x@h/evil.md looks like it contains a secret' >&2; echo 'tacit: second line' >&2; exit 1; }; done\nexit 0\n"
	os.WriteFile(hook, []byte(script), 0o755)
	seed := filepath.Join(root, "seed")
	run(t, root, "clone", "-q", bare, seed)
	os.WriteFile(filepath.Join(seed, "README.md"), []byte("x\n"), 0o644)
	run(t, seed, "add", "README.md")
	run(t, seed, "commit", "-q", "-m", "seed")
	run(t, seed, "push", "-q", "origin", "HEAD:main")
	clone := filepath.Join(root, "clone")
	run(t, root, "clone", "-q", bare, clone)
	run(t, clone, "config", "user.email", "writer@example.test")

	s := &Store{Dir: clone}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	os.MkdirAll(filepath.Join(clone, "inbox", "x@h"), 0o755)
	os.WriteFile(filepath.Join(clone, "inbox", "x@h", "evil.md"), []byte("---\nname: evil\n---\nbody\n"), 0o644)
	err := s.CommitPush(ctx, []string{"inbox/x@h/evil.md"}, "evil", "x")
	rej, ok := err.(*Rejected)
	if !ok {
		t.Fatalf("expected Rejected, got %v", err)
	}
	if rej.Reason != "tacit: inbox/x@h/evil.md looks like it contains a secret" {
		t.Fatalf("reason %q", rej.Reason)
	}
	if len(rej.Remote) < 2 || rej.Remote[0] != "tacit: inbox/x@h/evil.md looks like it contains a secret" || !strings.Contains(strings.Join(rej.Remote, "|"), "tacit: second line") {
		t.Fatalf("remote lines %q", rej.Remote)
	}
}
