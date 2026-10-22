package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// bigPush commits n files under dir/ on the server in one commit.
func bigPush(t *testing.T, server, dir string, n int, body string) string {
	t.Helper()
	os.MkdirAll(filepath.Join(server, dir), 0o755)
	for i := 0; i < n; i++ {
		os.WriteFile(filepath.Join(server, dir, fmt.Sprintf("f%04d.md", i)), []byte(fmt.Sprintf("%s %d\n", body, i)), 0o644)
	}
	run(t, server, "add", "-A")
	run(t, server, "commit", "-q", "-m", dir+" "+body)
	run(t, server, "push", "-q", "origin", "HEAD:main")
	return strings.TrimSpace(run(t, server, "rev-parse", "HEAD"))
}

func roClone(t *testing.T) (*Store, string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	_, server := seedPair(t)
	bare := filepath.Join(filepath.Dir(server), "store.git")
	s := &Store{Dir: filepath.Join(t.TempDir(), "ro")}
	if err := s.CloneReadOnly(context.Background(), bare, ""); err != nil {
		t.Fatal(err)
	}
	return s, server
}

func quietLog(t *testing.T) *[]string {
	var got []string
	old := Logf
	Logf = func(format string, args ...any) { got = append(got, fmt.Sprintf(format, args...)) }
	t.Cleanup(func() { Logf = old })
	return &got
}

func porcelain(t *testing.T, dir string) string {
	return strings.TrimSpace(run(t, dir, "status", "--porcelain", "--untracked-files=all", "--ignored"))
}

func agePath(t *testing.T, p string, d time.Duration) {
	old := time.Now().Add(-d)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
}

// A fast-forward killed half way (a smudge filter kills git after a few
// files: a real interruption, not a staged one) leaves HEAD old, files of
// the new commit written, a stale index.lock. A later sync proves the dirt
// is only toward the remote and finishes the update, logging it.
func TestKilledCheckoutIsFinishedBySync(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the killing filter is a shell script")
	}
	log := quietLog(t)
	s, server := roClone(t)
	ctx := context.Background()
	before, _ := s.Head(ctx)
	bigPush(t, server, "a", 300, "one")
	// A file a middle commit adds and the next deletes: an update that
	// stopped at the middle commit and resumed later finds it untracked.
	pushCommit(t, server, "middle.md")
	if err := s.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	middle := strings.TrimSpace(run(t, s.Dir, "rev-parse", "origin/main"))

	// The filter kills its git on the 40th file.
	bin := t.TempDir()
	count := filepath.Join(bin, "count")
	script := filepath.Join(bin, "smudge")
	os.WriteFile(script, []byte("#!/bin/sh\nn=$(cat "+count+" 2>/dev/null || echo 0); n=$((n+1)); echo $n > "+count+"\ncat\n[ \"$n\" = 40 ] && kill -9 $PPID\nexit 0\n"), 0o755)
	run(t, s.Dir, "config", "filter.slow.smudge", script)
	os.MkdirAll(filepath.Join(s.Dir, ".git", "info"), 0o755)
	os.WriteFile(filepath.Join(s.Dir, ".git", "info", "attributes"), []byte("*.md filter=slow\n"), 0o644)
	cmd := exec.Command("git", "merge", "--ff-only", "--quiet", "origin/main")
	cmd.Dir = s.Dir
	if err := cmd.Run(); err == nil {
		t.Fatal("the merge was not interrupted")
	}
	if got, _ := s.Head(ctx); got != before {
		t.Fatalf("HEAD moved to %s: the interruption came too late", got)
	}
	os.Remove(filepath.Join(s.Dir, ".git", "info", "attributes")) // the filter's job is done
	lock := filepath.Join(s.Dir, ".git", "index.lock")
	if _, err := os.Stat(lock); err != nil {
		t.Fatalf("no stale index.lock after the kill: %v", err)
	}
	if porcelain(t, s.Dir) == "" {
		t.Fatal("the interrupted checkout left a clean tree")
	}
	// The server moves on: middle.md is deleted again.
	run(t, server, "rm", "-q", "middle.md")
	run(t, server, "commit", "-q", "-m", "drop middle")
	run(t, server, "push", "-q", "origin", "HEAD:main")
	head := bigPush(t, server, "b", 50, "two")
	// The middle commit's file, as the interrupted update would have left it.
	os.WriteFile(filepath.Join(s.Dir, "middle.md"), []byte("middle.md\n"), 0o644)

	// Right after, the lock is fresh and no checkout claims it: a git may
	// still be writing, so sync does not touch it.
	if err := s.Sync(ctx); !errors.Is(err, ErrCheckoutRunning) {
		t.Fatalf("sync with a fresh lock: %v, want ErrCheckoutRunning", err)
	}
	agePath(t, lock, staleLockAge+time.Minute)
	if err := s.Sync(ctx); err != nil {
		t.Fatalf("sync after the interruption: %v", err)
	}
	if got, _ := s.Head(ctx); got != head {
		t.Fatalf("HEAD %s, want %s", got, head)
	}
	if st := porcelain(t, s.Dir); st != "" {
		t.Fatalf("not clean after the repair:\n%s", st)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "middle.md")); !os.IsNotExist(err) {
		t.Fatalf("middle.md (%s's, gone at the head) was left: %v", middle[:7], err)
	}
	all := strings.Join(*log, "\n")
	if !strings.Contains(all, "removed a stale lock") || !strings.Contains(all, "finished an interrupted update") || !strings.Contains(all, "removed 1 file(s)") {
		t.Fatalf("log: %s", all)
	}
	if !strings.Contains(string(s.readRepairLog()), "finished an interrupted update") {
		t.Fatalf("repair log: %s", s.readRepairLog())
	}
}

// halfCheckout writes target's content for every second changed path and
// all its new files, and leaves an old index.lock: what an interrupted
// fast-forward leaves, staged by hand.
func halfCheckout(t *testing.T, s *Store, target string) {
	t.Helper()
	out := run(t, s.Dir, "diff", "--name-status", "--no-renames", "HEAD", target)
	i := 0
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		st, path, _ := strings.Cut(l, "\t")
		i++
		abs := filepath.Join(s.Dir, filepath.FromSlash(path))
		switch {
		case st == "D":
			os.Remove(abs)
		case st == "A" || i%2 == 0:
			os.MkdirAll(filepath.Dir(abs), 0o755)
			body := run(t, s.Dir, "show", target+":"+path)
			// The files git was writing when it was killed: a start of their
			// content (empty, or cut).
			switch i {
			case 3:
				body = ""
			case 4:
				body = body[:len(body)/2]
			}
			os.WriteFile(abs, []byte(body), 0o644)
		}
	}
	lock := filepath.Join(s.Dir, ".git", "index.lock")
	os.WriteFile(lock, nil, 0o644)
	agePath(t, lock, staleLockAge+time.Minute)
}

// Dirt that is not the remote's (a changed file, a new file, an ignored
// file) keeps the clone refused, with the reason; the clone is untouched.
func TestForeignDirtIsStillRefused(t *testing.T) {
	quietLog(t)
	for _, c := range []struct {
		name, want string
		dirty      func(s *Store)
	}{
		{"changed", "content that is not origin/main's", func(s *Store) {
			os.WriteFile(filepath.Join(s.Dir, "a", "f0001.md"), []byte("mine\n"), 0o644)
		}},
		{"new", "a file no commit up to origin/main has: notes.md", func(s *Store) {
			os.WriteFile(filepath.Join(s.Dir, "notes.md"), []byte("mine\n"), 0o644)
		}},
		{"ignored", "an ignored file", func(s *Store) {
			os.MkdirAll(filepath.Join(s.Dir, ".git", "info"), 0o755)
			os.WriteFile(filepath.Join(s.Dir, ".git", "info", "exclude"), []byte("*.tmp\n"), 0o644)
			os.WriteFile(filepath.Join(s.Dir, "x.tmp"), []byte("tmp\n"), 0o644)
		}},
		{"missing", "a file origin/main has is missing: README.md", func(s *Store) {
			os.Remove(filepath.Join(s.Dir, "README.md"))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, server := roClone(t)
			ctx := context.Background()
			bigPush(t, server, "a", 20, "one")
			if err := s.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			old, _ := s.Head(ctx)
			bigPush(t, server, "a", 20, "two")
			if err := s.Fetch(ctx); err != nil {
				t.Fatal(err)
			}
			halfCheckout(t, s, "origin/main")
			c.dirty(s)
			st := porcelain(t, s.Dir)
			err := s.Sync(ctx)
			if err == nil || !strings.Contains(err.Error(), "fix "+s.Dir+" by hand") || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("sync: %v, want a refusal naming %q", err, c.want)
			}
			if got, _ := s.Head(ctx); got != old {
				t.Fatal("HEAD moved")
			}
			if porcelain(t, s.Dir) != st {
				t.Fatal("the refused clone was changed")
			}
		})
	}
}

// A staged half checkout with a lock whose checkout marker names a git that
// is gone is repaired at once; one that names a live git is left alone.
func TestStaleLockByMarker(t *testing.T) {
	log := quietLog(t)
	s, server := roClone(t)
	ctx := context.Background()
	head := bigPush(t, server, "a", 30, "one")
	if err := s.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	halfCheckout(t, s, "origin/main")
	lock := filepath.Join(s.Dir, ".git", "index.lock")
	os.Chtimes(lock, time.Now(), time.Now()) // fresh: only the marker can make it stale
	marker := filepath.Join(s.Dir, ".git", checkoutMarker)

	os.WriteFile(marker, []byte(fmt.Sprintf("%d %d\n", os.Getpid(), time.Now().Unix())), 0o600)
	if err := s.Sync(ctx); !errors.Is(err, ErrCheckoutRunning) || !strings.Contains(err.Error(), "git pid") {
		t.Fatalf("sync with a live marker: %v", err)
	}
	dead := exec.Command("git", "--version")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(marker, []byte(fmt.Sprintf("%d %d\n", dead.Process.Pid, time.Now().Unix())), 0o600)
	if err := s.Sync(ctx); err != nil {
		t.Fatalf("sync with a dead marker: %v", err)
	}
	if got, _ := s.Head(ctx); got != head || porcelain(t, s.Dir) != "" {
		t.Fatalf("HEAD %s (want %s), status %q", got, head, porcelain(t, s.Dir))
	}
	for _, p := range []string{lock, marker} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s is left", p)
		}
	}
	if !strings.Contains(strings.Join(*log, "\n"), "is gone") {
		t.Fatalf("log: %v", *log)
	}
}

// A writer clone with a local commit is never reset by the repair: HEAD is
// not an ancestor of the server's, so the proof fails and today's refusal
// stands. With no local commit the same proof repairs a writer as well.
func TestWriterCloneRepairNeedsTheSameProof(t *testing.T) {
	quietLog(t)
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	ctx := context.Background()

	s, server := seedPair(t)
	run(t, s.Dir, "config", "user.email", "w@t")
	run(t, s.Dir, "config", "user.name", "w")
	head := bigPush(t, server, "a", 30, "one")
	if err := s.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	halfCheckout(t, s, "origin/main")
	if err := s.Sync(ctx); err != nil {
		t.Fatalf("writer, no local commit: %v", err)
	}
	if got, _ := s.Head(ctx); got != head {
		t.Fatalf("HEAD %s, want %s", got, head)
	}

	w, server2 := seedPair(t)
	run(t, w.Dir, "config", "user.email", "w@t")
	run(t, w.Dir, "config", "user.name", "w")
	os.WriteFile(filepath.Join(w.Dir, "local.md"), []byte("local\n"), 0o644)
	run(t, w.Dir, "add", "local.md")
	run(t, w.Dir, "commit", "-q", "-m", "local")
	mine, _ := w.Head(ctx)
	bigPush(t, server2, "a", 30, "one")
	if err := w.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	halfCheckout(t, w, "origin/main")
	err := w.Sync(ctx)
	if err == nil || !strings.Contains(err.Error(), "HEAD is not an ancestor") {
		t.Fatalf("writer with a local commit: %v", err)
	}
	if got, _ := w.Head(ctx); got != mine {
		t.Fatal("the local commit was dropped")
	}
}

// A caller whose deadline is too short for the update's checkout gets a
// Deferred and an untouched clone; a cancelled caller does not cut a
// checkout short (the local leg has its own deadline).
func TestShortDeadlineDefersAndCancelDoesNotCut(t *testing.T) {
	s, server := roClone(t)
	ctx := context.Background()
	old, _ := s.Head(ctx)
	head := bigPush(t, server, "a", 1500, "one")
	if err := s.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var d *Deferred
	if err := s.WithLock(short, func() error { return s.IntegrateLocked(short) }); !errors.As(err, &d) || d.Paths != 1500 {
		t.Fatalf("short deadline: %v, want Deferred of 1500 paths", err)
	}
	if got, _ := s.Head(ctx); got != old || porcelain(t, s.Dir) != "" {
		t.Fatal("a deferred update touched the clone")
	}
	gone, stop := context.WithCancel(ctx)
	stop()
	if err := s.IntegrateLocked(gone); err != nil {
		t.Fatalf("cancelled caller: %v", err)
	}
	if got, _ := s.Head(ctx); got != head || porcelain(t, s.Dir) != "" {
		t.Fatalf("HEAD %s, want %s, clean", got, head)
	}
}
