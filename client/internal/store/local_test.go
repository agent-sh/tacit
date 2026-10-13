package store

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLocalStoreCommitsAndSyncsWithoutRemote(t *testing.T) {
	ctx := context.Background()
	s := &Store{Dir: filepath.Join(t.TempDir(), "facts")}
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	first, _ := s.Head(ctx)
	if !s.Local(ctx) {
		t.Fatal("not marked local")
	}
	if _, err := s.RemoteURL(ctx); err == nil {
		t.Fatal("local store acquired a remote")
	}
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	p := "inbox/claude@host/note.md"
	os.MkdirAll(filepath.Dir(filepath.Join(s.Dir, p)), 0o700)
	os.WriteFile(filepath.Join(s.Dir, p), []byte("local note\n"), 0o600)
	if err := s.CommitPush(ctx, []string{p}, "fact: local note", "claude"); err != nil {
		t.Fatal(err)
	}
	second, _ := s.Head(ctx)
	if first == second {
		t.Fatal("write was not committed")
	}
	if err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncBrief(ctx); err != nil {
		t.Fatal(err)
	}
	if ahead, err := s.RemoteAhead(ctx); err != nil || ahead {
		t.Fatalf("local remote check: %v %v", ahead, err)
	}
	if status := run(t, s.Dir, "status", "--porcelain"); strings.TrimSpace(status) != "" {
		t.Fatalf("dirty store: %s", status)
	}
}

func TestLocalInitRefusesExistingDataAndServerClone(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	p := filepath.Join(root, "keep")
	os.WriteFile(p, []byte("mine"), 0o600)
	s := &Store{Dir: root}
	if err := s.Init(ctx); err == nil {
		t.Fatal("initialized a non-empty directory")
	}
	if b, _ := os.ReadFile(p); string(b) != "mine" {
		t.Fatal("existing data changed")
	}
	other := t.TempDir()
	run(t, other, "init", "-q", "-b", "main")
	if err := (&Store{Dir: other}).Init(ctx); err == nil {
		t.Fatal("converted an existing repository")
	}
}

func TestLocalCommitRefusesSecretsAndLeavesOtherStagedFilesAlone(t *testing.T) {
	ctx := context.Background()
	s := &Store{Dir: filepath.Join(t.TempDir(), "facts")}
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Head(ctx)
	os.WriteFile(filepath.Join(s.Dir, "bad.md"), []byte("ghp_"+strings.Repeat("a", 40)), 0o600)
	if err := s.CommitPush(ctx, []string{"bad.md"}, "bad", "claude"); err == nil {
		t.Fatal("secret was committed")
	}
	if head, _ := s.Head(ctx); head != before {
		t.Fatal("rejected write changed HEAD")
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "bad.md")); !os.IsNotExist(err) {
		t.Fatal("refused secret remains a normal fact file")
	}
	os.WriteFile(filepath.Join(s.Dir, "mine.md"), []byte("unrelated"), 0o600)
	run(t, s.Dir, "add", "mine.md")
	os.WriteFile(filepath.Join(s.Dir, "good.md"), []byte("safe note"), 0o600)
	if err := s.CommitPush(ctx, []string{"good.md"}, "safe", "claude"); err != nil {
		t.Fatal(err)
	}
	if files := run(t, s.Dir, "show", "--format=", "--name-only", "HEAD"); strings.Contains(files, "mine.md") {
		t.Fatal("unrelated staged file was committed")
	}
	if !strings.Contains(run(t, s.Dir, "diff", "--cached", "--name-only"), "mine.md") {
		t.Fatal("unrelated staged file was changed")
	}
}

func TestLocalRefusedInboxNoteIsQuarantinedAndCacheInvalidated(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	s := &Store{Dir: filepath.Join(root, "facts")}
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	rel := "inbox/claude@host/bad.md"
	path := filepath.Join(s.Dir, rel)
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.WriteFile(path, []byte("ghp_"+strings.Repeat("a", 40)), 0o600)
	head, _ := s.Head(ctx)
	cache := filepath.Join(root, "index")
	os.MkdirAll(cache, 0o700)
	t.Setenv("TACIT_INDEX", cache)
	index := filepath.Join(cache, "index-v5-"+head+".db")
	os.WriteFile(index, []byte("cached dirty tree"), 0o600)
	err := s.CommitPush(ctx, []string{rel}, "bad", "claude")
	if _, ok := err.(*Rejected); !ok {
		t.Fatalf("policy refusal: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("refused note remains eligible for curation")
	}
	if _, err := os.Stat(path + ".rejected"); err != nil {
		t.Fatal("refused note was not preserved as rejected")
	}
	if _, err := os.Stat(index); !os.IsNotExist(err) {
		t.Fatal("dirty-tree index cache survived refusal")
	}
}

func TestCredentialHelperExecutablePathWithSpacesAndApostrophe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture; Windows client lifecycle is tested separately")
	}
	dir := t.TempDir()
	run(t, dir, "init", "-q", "-b", "main")
	bin := filepath.Join(dir, "my tools")
	os.MkdirAll(bin, 0o700)
	exe := filepath.Join(bin, "tacit's helper")
	os.WriteFile(exe, []byte("#!/bin/sh\n[ \"$1\" = credential ] && [ \"$2\" = codex ] && [ \"$3\" = get ] || exit 2\nprintf 'username=fixture\\npassword=fixture\\n'\n"), 0o700)
	run(t, dir, "config", "credential.helper", credentialCommand(exe, "codex"))
	cmd := exec.Command("git", "credential", "fill")
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader("protocol=https\nhost=example.invalid\n\n")
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "username=fixture") {
		t.Fatalf("credential helper path: %v %s", err, out)
	}
}
