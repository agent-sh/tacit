package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/follow"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newPair builds a bare server repo with one commit and a clone of it under
// root. push commits a file on the server side and returns the new HEAD.
func newPair(t *testing.T, root string) (clone string, push func(name string) string) {
	t.Helper()
	bare := filepath.Join(root, "store.git")
	gitIn(t, root, "init", "-q", "--bare", "-b", "main", bare)
	server := filepath.Join(root, "server")
	gitIn(t, root, "clone", "-q", bare, server)
	push = func(name string) string {
		os.WriteFile(filepath.Join(server, name), []byte(name), 0o644)
		gitIn(t, server, "add", name)
		gitIn(t, server, "commit", "-q", "-m", name)
		gitIn(t, server, "push", "-q", "origin", "HEAD:main")
		return gitIn(t, server, "rev-parse", "HEAD")
	}
	push("seed.txt")
	clone = filepath.Join(root, "clone")
	gitIn(t, root, "clone", "-q", bare, clone)
	return clone, push
}

// runStart runs `tacit hook session-start` with a Claude-style payload.
func runStart(t *testing.T, cwd string) {
	t.Helper()
	in := filepath.Join(t.TempDir(), "stdin.json")
	os.WriteFile(in, []byte(`{"session_id":"abc","cwd":"`+cwd+`","hook_event_name":"SessionStart","source":"startup"}`), 0o600)
	f, err := os.Open(in)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	oldIn, oldOut := os.Stdin, os.Stdout
	devnull, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	os.Stdin, os.Stdout = f, devnull
	defer func() { os.Stdin, os.Stdout = oldIn, oldOut; devnull.Close() }()
	if code := runHook([]string{"session-start"}); code != 0 {
		t.Fatalf("hook exit %d", code)
	}
}

// SessionStart syncs the clone unless a healthy follower has just confirmed
// it is current. With the follower's heartbeat fresh the hook leaves the
// network alone (the clone stays where it is, though the server moved); with
// no follower, or a stale one, it syncs as it always did.
func TestSessionStartSkipsTheSyncOnlyForAFreshFollower(t *testing.T) {
	root := t.TempDir()
	clone, push := newPair(t, root)
	head := func() string { return gitIn(t, clone, "rev-parse", "HEAD") }
	seeded := head()

	t.Setenv("TACIT_STORE", clone)
	t.Setenv("TACIT_INDEX", filepath.Join(root, "index"))
	t.Setenv("HOME", filepath.Join(root, "home"))
	statePath := filepath.Join(root, "follow.json")
	t.Setenv("TACIT_FOLLOW_STATE", statePath)

	// The server moves. A fresh heartbeat for this clone: no sync.
	moved := push("one.txt")
	fresh := follow.State{PID: os.Getpid(), Store: clone, Connected: true, AliveAt: time.Now(), VerifiedAt: time.Now()}
	if err := follow.Save(statePath, fresh); err != nil {
		t.Fatal(err)
	}
	runStart(t, clone)
	if got := head(); got != seeded {
		t.Fatalf("hook synced (%s) despite a fresh follower", got)
	}

	// The same follower, one push announced and not yet merged: sync.
	stale := fresh
	stale.Behind = true
	follow.Save(statePath, stale)
	runStart(t, clone)
	if got := head(); got != moved {
		t.Fatalf("hook did not sync while the follower was behind: %s, want %s", got, moved)
	}

	// A live stream whose last check against the server is too old: sync.
	unverified := fresh
	unverified.VerifiedAt = time.Now().Add(-follow.VerifiedFor - time.Minute)
	moved = push("two.txt")
	follow.Save(statePath, unverified)
	runStart(t, clone)
	if got := head(); got != moved {
		t.Fatalf("hook trusted a follower that had not verified the clone: %s, want %s", got, moved)
	}

	// No follower at all: sync.
	moved = push("three.txt")
	os.Remove(statePath)
	runStart(t, clone)
	if got := head(); got != moved {
		t.Fatalf("hook did not sync without a follower: %s, want %s", got, moved)
	}
}
