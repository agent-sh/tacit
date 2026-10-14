// Package store manages the local clone of the tacit store: where it lives,
// syncing it from the server, and committing inbox facts back.
//
// Every git operation shells out to the git binary with an argv list and a
// hard deadline: git-remote-http can hang on DNS or a dead peer, so each
// call gets a process group, a WaitDelay and http timeouts. The clone is a
// plain git working tree so a human can read and fix it.
package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/agent-sh/tacit/client/internal/platform"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Store is a local clone.
type Store struct {
	Dir string

	mu    sync.Mutex
	depth int
	lockF *os.File
}

// WithLock runs fn holding the store lock: an in-process mutex plus an
// OS file lock on .git/tacit.lock, so the review page, the curator, the dreamer
// and `tacit add` never interleave writes and git operations on one clone.
// It waits up to the context deadline for another process to finish.
func (s *Store) WithLock(ctx context.Context, fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(filepath.Join(s.Dir, ".git", "tacit.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	for {
		err := platform.TryLock(f)
		if err == nil {
			break
		}
		if !errors.Is(err, platform.ErrBusy) {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the store is busy (another tacit process holds it); try again in a moment: %w", ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
	defer platform.Unlock(f)
	return fn()
}

// Default resolves the store path: $TACIT_STORE, else ~/.local/share/tacit/store.
func Default() (*Store, error) {
	if d := os.Getenv("TACIT_STORE"); d != "" {
		return &Store{Dir: d}, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return &Store{Dir: filepath.Join(home, ".local", "share", "tacit", "store")}, nil
}

// Exists reports whether the clone is present.
func (s *Store) Exists() bool {
	_, err := os.Stat(filepath.Join(s.Dir, ".git"))
	return err == nil
}

// httpArgs bound every network call: connect in 5 s, give up when the
// transfer drops under 1 KB/s for 5 s.
var httpArgs = []string{"-c", "http.connectTimeout=5", "-c", "http.lowSpeedLimit=1000", "-c", "http.lowSpeedTime=5"}

// Clone fetches the store from url and configures the credential helper so
// pushes authenticate as <harness>@<host> through `tacit credential`.
func (s *Store) Clone(ctx context.Context, url, harness string) error {
	if err := os.MkdirAll(filepath.Dir(s.Dir), 0o755); err != nil {
		return err
	}
	if _, err := git(ctx, "", append(httpArgs, "clone", "--quiet", url, s.Dir)...); err != nil {
		return err
	}
	return s.ConfigureCredentials(ctx, url, harness)
}

// CloneReadOnly clones url as a clone that only reads: inherited credential
// helpers are reset, tacit.readonly is set, so every write path refuses
// before it commits (ErrReadOnly), and origin's push URL is one no git can
// push to. For a machine that follows the store without a writer key.
//
// With as (a harness, normally "reader"), fetches sign with the key
// <as>@<host> through `tacit credential <as>`, as the public path asks on
// every request; tacit.as records it for the follower and authcheck. The
// helper answers git's fetches; the clone holds nothing that could push.
func (s *Store) CloneReadOnly(ctx context.Context, url, as string) error {
	if err := os.MkdirAll(filepath.Dir(s.Dir), 0o755); err != nil {
		return err
	}
	// --config lands in the new clone before its first fetch, so the clone
	// itself already asks with the key (and no inherited helper answers).
	args := append(append([]string{}, httpArgs...), "clone", "--quiet", "--config", "credential.helper=")
	if as != "" {
		exe, err := helperExe()
		if err != nil {
			return err
		}
		args = append(args,
			"--config", "credential."+serverBase(url)+".helper="+credentialCommand(exe, as),
			"--config", asKey+"="+as)
	}
	if _, err := git(ctx, "", append(args, url, s.Dir)...); err != nil {
		return err
	}
	if _, err := git(ctx, s.Dir, "config", "remote.origin.pushurl", noPushURL); err != nil {
		return err
	}
	_, err := git(ctx, s.Dir, "config", "--bool", readOnlyKey, "true")
	return err
}

// noPushURL is a read-only clone's push URL: git has no transport for it, so
// a `git push` from the clone fails locally and sends nothing.
const noPushURL = "read-only://this-clone-never-pushes"

// readOnlyKey marks a clone that must never commit or push.
const readOnlyKey = "tacit.readonly"

// asKey names the key a read-only clone fetches with (tacit clone --read-only --as).
const asKey = "tacit.as"

// ReadAs is the harness whose key this read-only clone fetches with, or "".
func (s *Store) ReadAs(ctx context.Context) string {
	out, err := git(ctx, s.Dir, "config", "--get", asKey)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// serverBase is the server's base URL for a clone URL (http://host:7447/git
// -> http://host:7447), the context the credential helper is set for.
func serverBase(url string) string {
	return strings.TrimSuffix(strings.TrimSuffix(url, "/"), "/git")
}

// helperExe is the running binary, symlinks resolved: what git calls as the
// credential helper.
func helperExe() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

// Git runs custom credential helpers through its own shell, including on
// Windows. Forward slashes and POSIX quoting preserve spaces/apostrophes.
func credentialCommand(exe, harness string) string {
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	return "!exec " + quote(filepath.ToSlash(exe)) + " credential " + quote(harness)
}

// ErrReadOnly is a write refused because the clone was made read-only.
var ErrReadOnly = errors.New("this clone is read-only (tacit clone --read-only): nothing is written or pushed from this machine")

// ReadOnly reports whether the clone was made with CloneReadOnly.
func (s *Store) ReadOnly(ctx context.Context) bool {
	out, err := git(ctx, s.Dir, "config", "--bool", "--get", readOnlyKey)
	return err == nil && strings.TrimSpace(out) == "true"
}

// ConfigureCredentials resets inherited helpers (a global `cache` helper
// replays expired tokens) and installs `tacit credential <harness>` for url,
// pointing at the binary that is running now.
func (s *Store) ConfigureCredentials(ctx context.Context, url, harness string) error {
	exe, err := helperExe()
	if err != nil {
		return err
	}
	if _, err := git(ctx, s.Dir, "config", "credential.helper", ""); err != nil {
		return err
	}
	key := "credential." + serverBase(url) + ".helper"
	_, _ = git(ctx, s.Dir, "config", "--unset-all", key)
	if _, err := git(ctx, s.Dir, "config", "--add", key, credentialCommand(exe, harness)); err != nil {
		return err
	}
	if _, err := git(ctx, s.Dir, "config", "user.name", harness); err != nil {
		return err
	}
	_, err = git(ctx, s.Dir, "config", "user.email", harness+"@tacit")
	return err
}

// RemoteURL returns origin's URL.
func (s *Store) RemoteURL(ctx context.Context) (string, error) {
	out, err := git(ctx, s.Dir, "remote", "get-url", "origin")
	return strings.TrimSpace(out), err
}

// Sync brings main up to date with origin. Local commits that are still
// waiting to be pushed (a push that failed on the network) are rebased on
// top, never dropped; inbox paths never conflict. It never blocks a session
// beyond the caller's deadline.
func (s *Store) Sync(ctx context.Context) error {
	return s.WithLock(ctx, func() error { return s.SyncLocked(ctx) })
}

// SyncLocked is Sync for a caller that already holds WithLock.
func (s *Store) SyncLocked(ctx context.Context) error {
	if s.Local(ctx) {
		return nil
	}
	if err := s.Fetch(ctx); err != nil {
		return err
	}
	return s.IntegrateLocked(ctx)
}

// Fetch updates origin/main from the server. The caller holds WithLock.
func (s *Store) Fetch(ctx context.Context) error {
	if s.Local(ctx) {
		return nil
	}
	_, err := git(ctx, s.Dir, append(httpArgs, "fetch", "--quiet", "origin", "main")...)
	return err
}

// followRef is where the follower's fetch lands. Nothing else reads or
// writes it, so an unlocked fetch into it cannot collide with a locked
// fetch into origin/main (the loser of two overlapping ref updates fails
// with "incorrect old value provided").
const followRef = "refs/tacit/follow/main"

// SyncBrief is Sync for a long-running caller (the feed follower). The
// network leg runs without the store lock, into a private ref; the lock is
// held only to point origin/main at what arrived and merge it. A hook,
// `tacit add` or the review page never waits behind a slow fetch, and none
// of them can lose a ref update to this one.
//
// The fetch passes --refmap= so git does not also update origin/main on its
// own, and --no-write-fetch-head so it does not rewrite the FETCH_HEAD that
// a concurrent `git pull --rebase` reads (git 2.29 or newer).
func (s *Store) SyncBrief(ctx context.Context) error {
	if s.Local(ctx) {
		return nil
	}
	if _, err := git(ctx, s.Dir, append(httpArgs, "fetch", "--quiet", "--no-write-fetch-head", "--refmap=", "origin", "+refs/heads/main:"+followRef)...); err != nil {
		return err
	}
	return s.WithLock(ctx, func() error {
		sha, err := git(ctx, s.Dir, "rev-parse", "--verify", followRef)
		if err != nil {
			return err
		}
		if _, err := git(ctx, s.Dir, "update-ref", "refs/remotes/origin/main", strings.TrimSpace(sha)); err != nil {
			return err
		}
		return s.IntegrateLocked(ctx)
	})
}

// RemoteAhead reports whether the server's main holds commits the clone's
// HEAD lacks: one ls-remote and a local ancestry check, no objects moved.
// An error means the answer is unknown (network), not that the clone is
// current.
func (s *Store) RemoteAhead(ctx context.Context) (bool, error) {
	if s.Local(ctx) {
		return false, nil
	}
	out, err := git(ctx, s.Dir, append(httpArgs, "ls-remote", "origin", "refs/heads/main")...)
	if err != nil {
		return false, err
	}
	f := strings.Fields(out)
	if len(f) == 0 {
		return false, errors.New("the server has no main")
	}
	n, err := git(ctx, s.Dir, "rev-list", "--count", f[0], "^HEAD")
	if err != nil {
		return true, nil // the server's commit is not even in this clone
	}
	return strings.TrimSpace(n) != "0", nil
}

// IntegrateLocked moves the working tree to origin/main: a fast-forward, or
// a rebase of local unpushed commits. The caller holds WithLock.
func (s *Store) IntegrateLocked(ctx context.Context) error {
	if s.Local(ctx) {
		return nil
	}
	if _, err := git(ctx, s.Dir, "merge", "--ff-only", "--no-overwrite-ignore", "--quiet", "origin/main"); err == nil {
		return nil
	}
	// Rebase and hard reset can overwrite ignored or untracked files. Once
	// a fast-forward fails, leave a dirty working tree for its owner to fix.
	if err := s.requireClean(ctx); err != nil {
		return err
	}
	_, err := git(ctx, s.Dir, "rebase", "--quiet", "origin/main")
	if err == nil {
		return nil
	}
	s.cleanup("rebase", "--abort")
	// Local commits that cannot be rebased would block every later sync.
	// Keep them on a ref a person can inspect, and follow the server.
	cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if derr := s.requireClean(cctx); derr != nil {
		return fmt.Errorf("the clone has local commits that conflict with the server and uncommitted changes; fix %s by hand: %w", s.Dir, err)
	}
	ref := fmt.Sprintf("refs/tacit/stranded/%d", time.Now().Unix())
	if _, uerr := git(cctx, s.Dir, "update-ref", ref, "HEAD"); uerr != nil {
		return err
	}
	if _, rerr := git(cctx, s.Dir, "reset", "--quiet", "--hard", "origin/main"); rerr != nil {
		return rerr
	}
	return &Stranded{Ref: ref}
}

// requireClean includes untracked and ignored files, neither of which is
// recoverable from a stranded commit ref.
func (s *Store) requireClean(ctx context.Context) error {
	out, err := git(ctx, s.Dir, "status", "--porcelain", "--untracked-files=all", "--ignored")
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) != "" {
		return fmt.Errorf("the clone has uncommitted, untracked or ignored files; fix %s by hand before syncing", s.Dir)
	}
	return nil
}

// Stranded means local commits conflicted with the server and were moved
// to Ref; the clone now matches the server.
type Stranded struct{ Ref string }

func (e *Stranded) Error() string {
	return "local commits conflicted with the server; they are kept at " + e.Ref + " and the clone now follows the server"
}

// Head returns the current commit sha.
func (s *Store) Head(ctx context.Context) (string, error) {
	out, err := git(ctx, s.Dir, "rev-parse", "HEAD")
	return strings.TrimSpace(out), err
}

// Rejected is returned when the server refused the push (policy: wrong
// writer scope, secret, bad key). The commit is undone and the files are
// set aside as <name>.md.rejected so the clone never freezes on them.
type Rejected struct {
	Reason string
	Files  []string
	// Remote is every line the server printed ("remote: ..."), without the
	// prefix, so an operator sees which file and which rule.
	Remote []string
}

func (r *Rejected) Error() string { return "push rejected: " + r.Reason }

// CommitPush adds the given store-relative paths, commits, and pushes main
// as writer (the harness whose key signs the push: `tacit credential` reads
// TACIT_WRITER). On a non-fast-forward it rebases and retries once. On a
// policy rejection it undoes the commit and sets the files aside; on a
// network failure it keeps the commit for the next Sync and push.
func (s *Store) CommitPush(ctx context.Context, rels []string, message, writer string) error {
	return s.WithLock(ctx, func() error { return s.CommitPushLocked(ctx, rels, message, writer) })
}

// CommitPushLocked is CommitPush for a caller that already holds WithLock.
// A push that lost a race is rebased and retried; if the rebase conflicts,
// the local commit is undone (tracked files back to the server's version)
// so the clone never stays diverged, and a *Conflict is returned.
func (s *Store) CommitPushLocked(ctx context.Context, rels []string, message, writer string) error {
	return s.commitPush(ctx, rels, message, writer, true, false)
}

// CommitPushOnceLocked commits and pushes without rebasing: if the server
// moved on, the local commit is undone and ErrBehind is returned, so a
// caller whose change is a pure function of the store (a review mark) can
// sync and apply it again on fresh state.
func (s *Store) CommitPushOnceLocked(ctx context.Context, rels []string, message, writer string) error {
	return s.commitPush(ctx, rels, message, writer, false, false)
}

// CommitPushNamedLocked commits only named files and retries a remote race
// with the same rebase/rollback rules as CommitPushLocked. Caller holds WithLock.
func (s *Store) CommitPushNamedLocked(ctx context.Context, rels []string, message, writer string) error {
	return s.commitPush(ctx, rels, message, writer, true, true)
}

// PushPendingLocked retries retained commits with each original writer. The
// caller holds WithLock and syncs first; a failed push must block cursor updates.
func (s *Store) PushPendingLocked(ctx context.Context) error {
	if s.Local(ctx) {
		return nil
	}
	_, err := s.pushPending(ctx)
	return err
}

// CommitPushNamedOnceLocked is CommitPushOnceLocked that commits only the
// named files (commit --only): a change someone already staged in the clone
// does not ride along.
func (s *Store) CommitPushNamedOnceLocked(ctx context.Context, rels []string, message, writer string) error {
	return s.commitPush(ctx, rels, message, writer, false, true)
}

// ErrBehind means the server has commits this clone did not have; the
// local commit was undone.
var ErrBehind = errors.New("the store moved on the server")

// Conflict means a local commit could not be rebased onto the server's
// main; it was undone and the server's version kept.
type Conflict struct{ Reason string }

func (c *Conflict) Error() string {
	return "conflict with the server's store, local change undone: " + c.Reason
}

// cleanup runs git with its own short deadline, so undoing a half-finished
// rebase still happens when the caller's context is already cancelled.
func (s *Store) cleanup(args ...string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, _ = git(ctx, s.Dir, args...)
}

var (
	reHTTPRefused = regexp.MustCompile(`returned error: 40[13]\b|HTTP 40[13]\b`)
	reHTTPBusy    = regexp.MustCompile(`returned error: 50[23]\b|HTTP 50[23]\b`)
)

// refRace is the server losing a race on the ref update itself; the push
// is fine on fresh state.
func refRace(msg string) bool {
	return strings.Contains(msg, "incorrect old value") || strings.Contains(msg, "failed to lock") || strings.Contains(msg, "cannot lock ref")
}

// refused is a push the server will never accept: its policy said no.
func refused(msg string) bool {
	return strings.Contains(msg, "pre-receive hook declined") || (strings.Contains(msg, "[remote rejected]") && !refRace(msg))
}

// authFailed is a credential problem: the commit is fine and is kept for
// the next push once the key works.
func authFailed(msg string) bool {
	return strings.Contains(msg, "Authentication failed") || reHTTPRefused.MatchString(msg)
}

func behind(msg string) bool {
	return strings.Contains(msg, "non-fast-forward") || strings.Contains(msg, "fetch first") || strings.Contains(msg, "[rejected]") || refRace(msg)
}

func (s *Store) commitPush(ctx context.Context, rels []string, message, writer string, rebase, only bool) error {
	if s.ReadOnly(ctx) {
		return ErrReadOnly
	}
	local := s.Local(ctx)
	if local {
		if err := s.checkLocalFiles(rels); err != nil {
			if errors.Is(err, errLocalPolicy) {
				s.cleanup(append([]string{"reset", "--quiet", "HEAD", "--"}, rels...)...)
				s.invalidateLocalIndexes(ctx)
				return s.rejectFiles(ctx, rels, err.Error())
			}
			return err
		}
		only = true // unrelated staged changes never ride a local write
	}
	// Store-wide merge rules belong to the curator, never an inbox writer.
	curator := writer == "curator" || strings.HasPrefix(writer, "curator@")
	if local || curator {
		s.ensureAttributes(ctx)
	}
	if !local && curator {
		// Include generated attributes in named commits and in rollback. A
		// rejected race must not leave our new file blocking the next sync.
		found := false
		for _, rel := range rels {
			found = found || rel == ".gitattributes"
		}
		if !found {
			rels = append(append([]string{}, rels...), ".gitattributes")
		}
	}
	args := append([]string{"add", "--"}, rels...)
	if _, err := git(ctx, s.Dir, args...); err != nil {
		return err
	}
	commit := []string{"commit", "--quiet", "-m", message}
	if only {
		// Name the files: whatever else is staged in the clone stays out of this commit.
		commit = append(append(commit, "--only", "--"), rels...)
	}
	if _, err := gitEnv(ctx, s.Dir, []string{"GIT_AUTHOR_NAME=" + writer, "GIT_COMMITTER_NAME=" + writer}, commit...); err != nil {
		s.cleanup(append([]string{"reset", "--quiet", "--"}, rels...)...)
		return err
	}
	if local {
		return nil
	}
	var last error
	for attempt := 0; attempt < 4; attempt++ {
		// No --quiet: it hides the server's "remote:" lines, which carry the
		// reason for a refusal (firstLine and Rejected.Remote read them). The
		// output is only looked at when the push fails.
		failedCommit, err := s.pushPending(ctx)
		if err == nil {
			return nil
		}
		last = err
		msg := err.Error()
		switch {
		case refused(msg):
			head, herr := s.Head(ctx)
			if herr != nil || failedCommit != head {
				return fmt.Errorf("an earlier pending commit was refused; all local commits and files are kept for repair: %w", err)
			}
			return s.undoRejected(ctx, rels, msg)
		case authFailed(msg):
			return fmt.Errorf("the server did not accept this machine's key (%s); the commit is kept and goes up with the next push", firstLine(msg))
		case reHTTPBusy.MatchString(msg):
			select {
			case <-ctx.Done():
				return last
			case <-time.After(time.Duration(attempt+1) * 2 * time.Second):
			}
			continue
		case behind(msg):
			if !rebase {
				s.undoLocal(rels)
				return ErrBehind
			}
			if _, rerr := git(ctx, s.Dir, append(httpArgs, "pull", "--rebase", "--quiet")...); rerr != nil {
				s.cleanup("rebase", "--abort")
				s.undoLocal(rels)
				return &Conflict{Reason: firstLine(rerr.Error())}
			}
			continue
		}
		break
	}
	return last
}

// pushPending sends each retained commit with the writer that made it.
// The server checks the whole tree diff per push; sending a mixed-writer
// backlog as HEAD would assign every change to the last writer.
func (s *Store) pushPending(ctx context.Context) (string, error) {
	args := []string{"rev-list", "--reverse", "HEAD"}
	if _, err := git(ctx, s.Dir, "rev-parse", "--verify", "origin/main"); err == nil {
		args = append(args, "^origin/main")
	}
	out, err := git(ctx, s.Dir, args...)
	if err != nil {
		return "", err
	}
	for _, commit := range strings.Fields(out) {
		writer, err := git(ctx, s.Dir, "show", "-s", "--format=%an", commit)
		if err != nil {
			return commit, err
		}
		_, err = gitEnv(ctx, s.Dir, []string{"TACIT_WRITER=" + strings.TrimSpace(writer)},
			append(httpArgs, "push", "origin", commit+":main")...)
		if err != nil {
			return commit, err
		}
	}
	return "", nil
}

// undoLocal drops the newest local commit and puts its tracked files back
// to HEAD; new inbox notes stay as untracked files for the next push, other
// new files are removed (the curator recreates them from the inbox).
func (s *Store) undoLocal(rels []string) {
	s.cleanup("reset", "--quiet", "--soft", "HEAD~1")
	s.cleanup(append([]string{"reset", "--quiet", "--"}, rels...)...)
	s.Restore(rels)
}

// Restore puts tracked files back to HEAD and removes new files outside
// the inbox. It runs on its own deadline so it works after a cancellation.
func (s *Store) Restore(rels []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, rel := range rels {
		if _, err := git(ctx, s.Dir, "cat-file", "-e", "HEAD:"+rel); err == nil {
			_, _ = git(ctx, s.Dir, "checkout", "--quiet", "HEAD", "--", rel)
		} else if !strings.HasPrefix(rel, "inbox/") {
			os.Remove(filepath.Join(s.Dir, rel))
		}
	}
}

func (s *Store) undoRejected(_ context.Context, rels []string, reason string) error {
	// Drop the commit, keep the working tree, then move the files aside.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := git(ctx, s.Dir, "reset", "--quiet", "--soft", "HEAD~1"); err != nil {
		return fmt.Errorf("%s; and undoing the commit failed: %w", reason, err)
	}
	if _, err := git(ctx, s.Dir, append([]string{"reset", "--quiet", "--"}, rels...)...); err != nil {
		return fmt.Errorf("%s; and resetting rejected paths failed: %w", reason, err)
	}
	return s.rejectFiles(ctx, rels, reason)
}

func (s *Store) rejectFiles(ctx context.Context, rels []string, reason string) error {
	// A new inbox note is set aside as .rejected so the writer can see it;
	// anything else (a curated fact, an index, a review file) is put back to
	// what the server has, so a refusal never loses a fact.
	var moved []string
	for _, rel := range rels {
		src := filepath.Join(s.Dir, rel)
		tracked := func() bool { _, err := git(ctx, s.Dir, "cat-file", "-e", "HEAD:"+rel); return err == nil }()
		switch {
		case strings.HasPrefix(rel, "inbox/") && strings.HasSuffix(rel, ".md") && !tracked:
			if err := os.Rename(src, src+".rejected"); err == nil {
				moved = append(moved, rel+".rejected")
			}
		case tracked:
			_, _ = git(ctx, s.Dir, "checkout", "--quiet", "HEAD", "--", rel)
		default:
			os.Remove(src)
		}
	}
	return &Rejected{Reason: firstLine(reason), Files: moved, Remote: remoteLines(reason)}
}

// ensureAttributes makes files that several machines append to merge by
// union, so two concurrent decisions or curator runs rebase cleanly.
// UnionFiles are the .gitattributes lines for files several machines append
// to: the decision log, the review files, the quote ledger. Each line is
// checked on its own, so a store that has the older lines gets the new one.
var UnionFiles = []string{"review/decisions.jsonl merge=union", "review/*.md merge=union", "review/owner-quotes.jsonl merge=union"}

func (s *Store) ensureAttributes(ctx context.Context) {
	p := filepath.Join(s.Dir, ".gitattributes")
	have, _ := os.ReadFile(p)
	var add string
	for _, l := range UnionFiles {
		if !strings.Contains(string(have), l) {
			add += l + "\n"
		}
	}
	if add == "" {
		return
	}
	if len(have) > 0 && !strings.HasSuffix(string(have), "\n") {
		add = "\n" + add
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	f.WriteString(add)
	f.Close()
	_, _ = git(ctx, s.Dir, "add", ".gitattributes")
}

// remoteLines returns the "remote:" lines of git's stderr, prefix removed,
// at most 12.
func remoteLines(msg string) []string {
	var out []string
	for _, l := range strings.Split(msg, "\n") {
		// the first line carries git's own "git HEAD:main: " prefix
		if _, t, ok := strings.Cut(strings.TrimSpace(l), "remote: "); ok {
			if t = strings.TrimSpace(t); t != "" && len(out) < 12 {
				out = append(out, t)
			}
		}
	}
	return out
}

// firstLine trims git's multi-line stderr to the line a person needs.
func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		if _, t, ok := strings.Cut(l, "remote: "); ok { // also when git's own "git HEAD:main: " comes first
			l = strings.TrimSpace(t)
		}
		if strings.HasPrefix(l, "tacit:") || strings.Contains(l, "declined") || strings.Contains(l, "remote rejected") || reHTTPRefused.MatchString(l) || strings.Contains(l, "Authentication failed") || strings.Contains(l, "CONFLICT") {
			return strings.TrimSpace(l)
		}
	}
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i > 0 {
		s = s[:i]
	}
	return s
}

// Changed lists store-relative paths changed between two commits.
func (s *Store) Changed(ctx context.Context, from, to string) ([]string, error) {
	out, err := git(ctx, s.Dir, "diff", "--name-only", from, to)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l != "" {
			paths = append(paths, l)
		}
	}
	return paths, nil
}

// RepoName returns the short name of the repository containing dir: the
// basename of the directory that holds the common `.git`, so a linked
// worktree and any subdirectory report the main repository's name. "" when
// dir is not in a repository.
func RepoName(ctx context.Context, dir string) string {
	out, err := git(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return ""
	}
	common := strings.TrimSpace(out) // /path/to/repo/.git
	if common == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(common); err == nil {
		common = resolved
	}
	if filepath.Base(common) == ".git" {
		return filepath.Base(filepath.Dir(common))
	}
	// A bare repository: its directory name without .git.
	return strings.TrimSuffix(filepath.Base(common), ".git")
}

// ShortHost is the machine's short host name, lowercased.
func ShortHost() string {
	h, _ := os.Hostname()
	h, _, _ = strings.Cut(h, ".")
	return strings.ToLower(h)
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	return gitEnv(ctx, dir, nil, args...)
}

// Git runs git in dir under the same hard deadline as the store's own calls
// (own process group, killed as a group when ctx ends). For read-only use on
// other checkouts, such as the curator's derived-fact pass.
func Git(ctx context.Context, dir string, args ...string) (string, error) {
	return git(ctx, dir, args...)
}

// gitEnv runs git with a hard deadline: own process group, killed as a
// group when the context ends, WaitDelay so a helper holding the pipes
// cannot keep Run waiting.
func gitEnv(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0"), env...)
	platform.Prepare(cmd)
	cmd.WaitDelay = 2 * time.Second
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			msg = "timeout: " + msg
		}
		if ctx.Err() != nil {
			return out.String(), fmt.Errorf("git %s: %s: %w", args[len(args)-1], msg, ctx.Err())
		}
		return out.String(), fmt.Errorf("git %s: %s", args[len(args)-1], msg)
	}
	return out.String(), nil
}
