package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSyncPreservesUncommittedCollision(t *testing.T) {
	for _, ignored := range []bool{false, true} {
		t.Run(fmt.Sprintf("ignored=%t", ignored), func(t *testing.T) {
			s, server := seedPair(t)
			if ignored {
				os.WriteFile(filepath.Join(s.Dir, ".git", "info", "exclude"), []byte("a.md\n"), 0600)
			}
			os.WriteFile(filepath.Join(s.Dir, "a.md"), []byte("uncommitted local evidence\n"), 0600)
			before, _ := s.Head(context.Background())
			pushCommit(t, server, "a.md")
			if err := s.Sync(context.Background()); err == nil {
				t.Fatal("sync should refuse a collision")
			}
			got, _ := os.ReadFile(filepath.Join(s.Dir, "a.md"))
			if string(got) != "uncommitted local evidence\n" {
				t.Fatalf("Sync destroyed local content: %q", got)
			}
			if after, _ := s.Head(context.Background()); before != after {
				t.Fatalf("HEAD moved: %s -> %s", before, after)
			}
		})
	}
}

func TestFirstAgentWriteAgainstRealHook(t *testing.T) {
	s, server := seedPair(t)
	bare := strings.TrimSpace(run(t, server, "remote", "get-url", "origin"))
	hook, err := os.ReadFile("../../../server/priv/hooks/pre-receive")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bare, "hooks", "pre-receive"), hook, 0755); err != nil {
		t.Fatal(err)
	}
	run(t, s.Dir, "config", "user.email", "writer@example.test")
	rel := "inbox/claude@host/note.md"
	os.MkdirAll(filepath.Dir(filepath.Join(s.Dir, rel)), 0700)
	os.WriteFile(filepath.Join(s.Dir, rel), []byte("a safe first note\n"), 0600)
	if err := s.CommitPush(context.Background(), []string{rel}, "first safe note", "claude@host"); err != nil {
		t.Fatalf("safe first write refused: %v", err)
	}
}

func TestPendingWriterDoesNotRejectAnotherWriter(t *testing.T) {
	s, server := seedPair(t)
	os.WriteFile(filepath.Join(server, ".gitattributes"), []byte(strings.Join(UnionFiles, "\n")+"\n"), 0600)
	run(t, server, "add", ".gitattributes")
	run(t, server, "commit", "-q", "-m", "attributes")
	run(t, server, "push", "-q", "origin", "HEAD:main")
	if err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	bare := strings.TrimSpace(run(t, server, "remote", "get-url", "origin"))
	hook, err := os.ReadFile("../../../server/priv/hooks/pre-receive")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(bare, "hooks", "pre-receive"), hook, 0755)
	run(t, s.Dir, "config", "user.email", "writer@example.test")
	first := "inbox/claude@host/first.md"
	second := "inbox/codex@host/second.md"
	os.MkdirAll(filepath.Dir(filepath.Join(s.Dir, first)), 0700)
	os.MkdirAll(filepath.Dir(filepath.Join(s.Dir, second)), 0700)
	os.WriteFile(filepath.Join(s.Dir, first), []byte("safe claude note\n"), 0600)
	run(t, s.Dir, "remote", "set-url", "origin", filepath.Join(s.Dir, "offline.git"))
	if err := s.CommitPush(context.Background(), []string{first}, "safe claude note", "claude@host"); err == nil {
		t.Fatal("expected network failure")
	}
	run(t, s.Dir, "remote", "set-url", "origin", bare)
	os.WriteFile(filepath.Join(s.Dir, second), []byte("safe codex note\n"), 0600)
	if err := s.CommitPush(context.Background(), []string{second}, "safe codex note", "codex@host"); err != nil {
		t.Fatalf("valid second writer note quarantined due to retained first writer commit: %v; remaining ahead=%s", err, strings.TrimSpace(run(t, s.Dir, "rev-list", "--count", "origin/main..HEAD")))
	}
	for _, rel := range []string{first, second} {
		if got := strings.TrimSpace(run(t, server, "--git-dir="+bare, "show", "main:"+rel)); !strings.Contains(got, "safe") {
			t.Fatalf("note not pushed: %s", rel)
		}
	}
	if got := strings.TrimSpace(run(t, s.Dir, "rev-list", "--count", "origin/main..HEAD")); got != "0" {
		t.Fatalf("unflushed commits: %s", got)
	}

}

func TestEarlierRefusalKeepsLatestWriterNote(t *testing.T) {
	s, server := seedPair(t)
	bare := strings.TrimSpace(run(t, server, "remote", "get-url", "origin"))
	hook, err := os.ReadFile("../../../server/priv/hooks/pre-receive")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bare, "hooks", "pre-receive"), hook, 0755); err != nil {
		t.Fatal(err)
	}
	run(t, s.Dir, "config", "user.email", "writer@example.test")
	first := "inbox/wrong@host/first.md"
	second := "inbox/codex@host/second.md"
	for _, rel := range []string{first, second} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(s.Dir, rel)), 0700); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(s.Dir, first), []byte("wrong writer scope\n"), 0600)
	run(t, s.Dir, "remote", "set-url", "origin", filepath.Join(s.Dir, "offline.git"))
	if err := s.CommitPush(context.Background(), []string{first}, "pending", "claude@host"); err == nil {
		t.Fatal("expected network failure")
	}
	run(t, s.Dir, "remote", "set-url", "origin", bare)
	os.WriteFile(filepath.Join(s.Dir, second), []byte("valid codex note\n"), 0600)
	if err := s.CommitPush(context.Background(), []string{second}, "valid note", "codex@host"); err == nil || !strings.Contains(err.Error(), "earlier pending commit") {
		t.Fatalf("earlier refusal: %v", err)
	}
	if got := strings.TrimSpace(run(t, s.Dir, "rev-list", "--count", "origin/main..HEAD")); got != "2" {
		t.Fatalf("pending commits lost: %s", got)
	}
	for _, rel := range []string{first, second} {
		if _, err := os.Stat(filepath.Join(s.Dir, rel)); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(s.Dir, rel+".rejected")); !os.IsNotExist(err) {
			t.Fatalf("unrelated note quarantined: %s", rel)
		}
	}
	if got := run(t, s.Dir, "show", "HEAD:"+second); got != "valid codex note\n" {
		t.Fatalf("latest commit lost: %q", got)
	}
}
