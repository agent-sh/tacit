package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A read-only clone syncs like any other, has no credential helper, and
// refuses every commit before it makes one: the server never sees a push.
func TestReadOnlyCloneSyncsAndRefusesWrites(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	_, server := seedPair(t)
	bare := filepath.Join(filepath.Dir(server), "store.git")
	ctx := context.Background()
	s := &Store{Dir: filepath.Join(t.TempDir(), "ro")}
	if err := s.CloneReadOnly(ctx, bare, ""); err != nil {
		t.Fatal(err)
	}
	if !s.ReadOnly(ctx) {
		t.Fatal("clone is not marked read-only")
	}
	if out, _ := git(ctx, s.Dir, "config", "--get-regexp", `^credential\..*helper$`); strings.TrimSpace(out) != "credential.helper" {
		t.Fatalf("credential helpers: %q, want only the reset", out)
	}

	head := pushCommit(t, server, "news")
	if err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Head(ctx); got != head {
		t.Fatalf("sync: head %s, want %s", got, head)
	}

	os.MkdirAll(filepath.Join(s.Dir, "inbox", "claude@host-b"), 0o755)
	rel := "inbox/claude@host-b/x.md"
	os.WriteFile(filepath.Join(s.Dir, rel), []byte("x\n"), 0o644)
	if err := s.CommitPush(ctx, []string{rel}, "fact: x", "claude"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("CommitPush: %v, want ErrReadOnly", err)
	}
	if got, _ := s.Head(ctx); got != head {
		t.Fatal("a read-only clone made a commit")
	}
	if remote := strings.TrimSpace(run(t, server, "ls-remote", bare, "refs/heads/main")); !strings.HasPrefix(remote, head) {
		t.Fatalf("the server moved: %s", remote)
	}

	// A clone made with a key is not read-only.
	w := &Store{Dir: filepath.Join(t.TempDir(), "rw")}
	if err := w.Clone(ctx, bare, "claude"); err != nil {
		t.Fatal(err)
	}
	if w.ReadOnly(ctx) {
		t.Fatal("a writer clone reads as read-only")
	}
}
