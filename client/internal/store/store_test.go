package store

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func TestRepoNameSubdirAndWorktree(t *testing.T) {
	root := t.TempDir()
	// root is the home: a checkout in one of the home's dot directories is
	// never a repository (repoid), and Go's temp dir may be one (GOTMPDIR or
	// TMPDIR under ~/.cache). The check that root names no repository takes
	// no git root above Go's temp dir.
	t.Setenv("HOME", root)
	t.Setenv("USERPROFILE", root)
	repo := filepath.Join(root, "memra")
	os.MkdirAll(filepath.Join(repo, "src", "kv"), 0o755)
	run(t, repo, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(repo, "a"), []byte("a"), 0o644)
	run(t, repo, "add", "a")
	run(t, repo, "commit", "-q", "-m", "a")
	wt := filepath.Join(root, "wt-fix")
	run(t, repo, "worktree", "add", "-q", wt, "-b", "fix")
	ctx := context.Background()
	for _, c := range []struct{ dir, want string }{
		{repo, "memra"},
		{filepath.Join(repo, "src", "kv"), "memra"},
		{wt, "memra"},
		{root, ""},
	} {
		if got := RepoName(ctx, c.dir); got != c.want {
			t.Errorf("RepoName(%s) = %q, want %q", c.dir, got, c.want)
		}
	}
}

// A push the server refuses must not leave the clone with a local commit
// that blocks every later sync. The refusal here is a pre-receive hook on a
// bare repo; the files end up set aside as .rejected.
func TestCommitPushRejectionDoesNotFreezeTheClone(t *testing.T) {
	root := t.TempDir()
	bare := filepath.Join(root, "store.git")
	run(t, root, "init", "-q", "--bare", "-b", "main", bare)
	hook := filepath.Join(bare, "hooks", "pre-receive")
	os.WriteFile(hook, []byte("#!/bin/sh\nwhile read o n r; do git diff-tree --no-commit-id --name-only -r \"$n\" | grep -q evil && { echo 'tacit: refused' >&2; exit 1; }; done\nexit 0\n"), 0o755)
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
	if len(rej.Files) != 1 || !strings.HasSuffix(rej.Files[0], ".rejected") {
		t.Fatalf("files %v", rej.Files)
	}
	head, _ := s.Head(ctx)
	seedHead := strings.TrimSpace(run(t, seed, "rev-parse", "HEAD"))
	if head != seedHead {
		t.Fatalf("local commit not undone: %s vs %s", head, seedHead)
	}
	if err := s.Sync(ctx); err != nil {
		t.Fatalf("sync after rejection: %v", err)
	}
	// A good push still works and a later sync is clean.
	os.WriteFile(filepath.Join(clone, "inbox", "x@h", "good.md"), []byte("---\nname: good\n---\nbody\n"), 0o644)
	if err := s.CommitPush(ctx, []string{"inbox/x@h/good.md"}, "good", "x"); err != nil {
		t.Fatalf("good push: %v", err)
	}
	if err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
}

// A git call must end at the deadline even when the remote never answers.
func TestGitDeadlineBoundsAHangingRemote(t *testing.T) {
	root := t.TempDir()
	run(t, root, "init", "-q", "-b", "main", root)
	// A URL to a port nothing listens on: connect fails fast; the lowSpeed
	// settings cover the slow case. Here we check the deadline itself with a
	// command that would otherwise wait: a fetch against a sleeping helper.
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := gitEnv(ctx, root, []string{"GIT_SSH_COMMAND=sleep 30"}, "fetch", "ssh://localhost/nowhere.git")
	if err == nil {
		t.Fatal("expected an error")
	}
	if d := time.Since(start); d > 6*time.Second {
		t.Fatalf("fetch ran %v past a 1.5 s deadline", d)
	}
}

// seedPair makes a bare "server", a clone of it, and returns the server's
// working copy so a test can push a new commit to it.
func seedPair(t *testing.T) (clone *Store, server string) {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, "store.git")
	run(t, root, "init", "-q", "--bare", "-b", "main", bare)
	server = filepath.Join(root, "seed")
	run(t, root, "clone", "-q", bare, server)
	os.WriteFile(filepath.Join(server, "README.md"), []byte("x\n"), 0o644)
	run(t, server, "add", "README.md")
	run(t, server, "commit", "-q", "-m", "seed")
	run(t, server, "push", "-q", "origin", "HEAD:main")
	dir := filepath.Join(root, "clone")
	run(t, root, "clone", "-q", bare, dir)
	return &Store{Dir: dir}, server
}

// pushCommit commits a file on the server side and returns the new HEAD.
func pushCommit(t *testing.T, server, name string) string {
	t.Helper()
	os.WriteFile(filepath.Join(server, name), []byte(name+"\n"), 0o644)
	run(t, server, "add", name)
	run(t, server, "commit", "-q", "-m", name)
	run(t, server, "push", "-q", "origin", "HEAD:main")
	return strings.TrimSpace(run(t, server, "rev-parse", "HEAD"))
}

// The follower's network leg takes no store lock: a fetch runs while another
// process holds it, and only the merge waits.
func TestSyncBriefFetchesWithoutTheLock(t *testing.T) {
	s, server := seedPair(t)
	pushed := pushCommit(t, server, "a.md")
	holder := &Store{Dir: s.Dir} // a second process's view: its own flock
	release := make(chan struct{})
	held := make(chan struct{})
	go holder.WithLock(context.Background(), func() error { close(held); <-release; return nil })
	<-held

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	before := strings.TrimSpace(run(t, s.Dir, "rev-parse", "origin/main"))
	// The merge needs the lock: with a short deadline SyncBrief reports busy,
	// but the fetch before it has already run, into its private ref, leaving
	// origin/main and the working tree alone.
	short, cancelShort := context.WithTimeout(ctx, 400*time.Millisecond)
	defer cancelShort()
	if err := s.SyncBrief(short); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("SyncBrief under a held lock: %v", err)
	}
	if got := strings.TrimSpace(run(t, s.Dir, "rev-parse", followRef)); got != pushed {
		t.Fatalf("private ref %s, want the server's %s", got, pushed)
	}
	if got := strings.TrimSpace(run(t, s.Dir, "rev-parse", "origin/main")); got != before {
		t.Fatalf("an unlocked fetch moved origin/main: %s -> %s", before, got)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "a.md")); err == nil {
		t.Fatal("fetch must not touch the working tree")
	}
	close(release)
	if err := s.SyncBrief(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "a.md")); err != nil {
		t.Fatalf("merge did not happen: %v", err)
	}
}

// A fetch that hangs on the network must not keep hooks and writers out of
// the store: while SyncBrief is stuck in git, the lock is free.
func TestSyncBriefDoesNotHoldTheLockOverAHungFetch(t *testing.T) {
	s, _ := seedPair(t)
	run(t, s.Dir, "remote", "set-url", "origin", "ssh://localhost/nowhere.git")
	t.Setenv("GIT_SSH_COMMAND", "sh -c 'sleep 30' --")
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.SyncBrief(ctx) }()
	time.Sleep(500 * time.Millisecond) // let git start and block

	other := &Store{Dir: s.Dir}
	lctx, lcancel := context.WithTimeout(context.Background(), time.Second)
	defer lcancel()
	start := time.Now()
	if err := other.WithLock(lctx, func() error { return nil }); err != nil {
		t.Fatalf("lock unavailable during a hung fetch: %v", err)
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("took the lock in %v", d)
	}
	cancel()
	<-done
}

// slowPacks makes every fetch spend a second inside pack-objects, so two
// fetches started together finish their transfers together and reach the
// ref update at the same moment. (packObjectsHook is read only from
// protected config, which GIT_CONFIG_* counts as.)
func slowPacks(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "uploadpack.packObjectsHook")
	t.Setenv("GIT_CONFIG_VALUE_0", "sleep 1; exec")
}

// Two overlapping fetches of origin/main are not safe: the second ref update
// fails with "incorrect old value provided". The follower fetches without the
// lock, so it must never write origin/main; a hook's sync or a `pull
// --rebase` under the lock must not be able to lose to it.
func TestFollowerFetchCannotMakeALockedFetchLose(t *testing.T) {
	locked := map[string]func(ctx context.Context, s *Store) error{
		"hook sync": func(ctx context.Context, s *Store) error { return s.Sync(ctx) },
		"pull --rebase": func(ctx context.Context, s *Store) error {
			return s.WithLock(ctx, func() error {
				_, err := git(ctx, s.Dir, append(httpArgs, "pull", "--rebase", "--quiet")...)
				return err
			})
		},
	}
	for name, op := range locked {
		t.Run(name, func(t *testing.T) {
			s, server := seedPair(t)
			slowPacks(t)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			for round := 0; round < 4; round++ {
				want := pushCommit(t, server, fmt.Sprintf("r%d.md", round))
				follower, hook := &Store{Dir: s.Dir}, &Store{Dir: s.Dir}
				var errs [2]error
				start := make(chan struct{})
				var wg sync.WaitGroup
				wg.Add(2)
				go func() { defer wg.Done(); <-start; errs[0] = follower.SyncBrief(ctx) }()
				go func() { defer wg.Done(); <-start; errs[1] = op(ctx, hook) }()
				close(start)
				wg.Wait()
				if errs[0] != nil || errs[1] != nil {
					t.Fatalf("round %d: follower: %v; locked side: %v", round, errs[0], errs[1])
				}
				if got := strings.TrimSpace(run(t, s.Dir, "rev-parse", "HEAD")); got != want {
					t.Fatalf("round %d: HEAD %s, want %s", round, got, want)
				}
			}
		})
	}
}

func TestRemoteAhead(t *testing.T) {
	s, server := seedPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if ahead, err := s.RemoteAhead(ctx); err != nil || ahead {
		t.Fatalf("current clone: ahead=%v err=%v", ahead, err)
	}
	pushCommit(t, server, "new.md")
	if ahead, err := s.RemoteAhead(ctx); err != nil || !ahead {
		t.Fatalf("server moved, nothing fetched: ahead=%v err=%v", ahead, err)
	}
	if err := s.SyncBrief(ctx); err != nil {
		t.Fatal(err)
	}
	if ahead, err := s.RemoteAhead(ctx); err != nil || ahead {
		t.Fatalf("after a sync: ahead=%v err=%v", ahead, err)
	}
	// A local commit the server lacks does not count as the server being ahead.
	os.WriteFile(filepath.Join(s.Dir, "local.md"), []byte("l\n"), 0o644)
	run(t, s.Dir, "add", "local.md")
	run(t, s.Dir, "commit", "-q", "-m", "local")
	if ahead, err := s.RemoteAhead(ctx); err != nil || ahead {
		t.Fatalf("local commit ahead of the server: ahead=%v err=%v", ahead, err)
	}
	// An unreachable server is an unknown, not "current".
	run(t, s.Dir, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git"))
	if _, err := s.RemoteAhead(ctx); err == nil {
		t.Fatal("an unreachable server must be an error")
	}
}
