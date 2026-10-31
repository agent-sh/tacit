package store

// Moving the working tree to origin/main without ever leaving it half done.
//
// The network legs of a sync (fetch, ls-remote) run under the caller's
// deadline. The local leg (the checkout of what arrived) does not: a
// checkout killed at its deadline leaves HEAD at the old commit, the files
// half way to the new one and a stale .git/index.lock, and every later
// sync then refused the clone as dirty (a read-only clone whose pull hit
// the follower's deadline was left with many files modified or
// untracked). So:
//
//   - the local leg runs under its own deadline (localTimeout), detached
//     from the caller's; a caller whose time left is too short for the
//     update's size is not given a partial checkout but a Deferred error,
//     before git touches a file;
//   - git's pid is kept in .git/tacit-checkout while it runs, so a later
//     sync can tell a lock a live checkout holds from one a dead one left;
//   - a clone that is dirty only toward the remote (every changed or new
//     file holds the content of origin/main or of a commit between HEAD and
//     it, every missing file is gone there, nothing is ignored, HEAD is an
//     ancestor of origin/main) is what an interrupted update leaves: code
//     proves that path by path and finishes the update (reset to
//     origin/main), logging what it did. Any other dirt is refused as
//     before.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/platform"
)

const (
	// localTimeout bounds one local git step of a sync (status, checkout,
	// reset) on its own deadline. A fast-forward that rewrites every file of
	// a large store takes seconds; ten minutes is far above any real
	// checkout and still ends a git that hangs.
	localTimeout = 10 * time.Minute
	// staleLockAge is when a git lock no tacit checkout claims counts as
	// left by a dead git: git holds index.lock only while it writes the
	// index, and no local step of a sync may run longer than localTimeout.
	staleLockAge = localTimeout + 5*time.Minute
	// checkoutRate and checkoutFixed estimate a checkout's time from the
	// paths it changes, for a caller with a short deadline: 3,700 files a
	// second were measured; 300 leaves room for a slow or loaded disk.
	checkoutRate  = 300
	checkoutFixed = 2 * time.Second
	// checkoutMarker names the file that holds the running checkout's git
	// pid and start time.
	checkoutMarker = "tacit-checkout"
	// repairLog is the clone's own log of the repairs sync made (in .git,
	// never in the working tree).
	repairLog = "tacit-sync.log"
)

// Logf reports what sync repaired; tests and callers may replace it.
var Logf = func(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "tacit: sync: "+format+"\n", args...)
}

// Deferred is an update left for a caller with more time: the server is
// ahead by Paths changed files, which would take about Need, and the
// caller's deadline leaves Left. Nothing in the clone was touched.
type Deferred struct {
	Paths      int
	Need, Left time.Duration
}

func (e *Deferred) Error() string {
	return fmt.Sprintf("the update changes %d file(s), about %s of checkout, and this caller has %s left: left for the next sync with more time (nothing was touched)", e.Paths, e.Need.Round(time.Second), e.Left.Round(time.Second))
}

// localCtx is the local leg's context: the caller's values, not its
// deadline or cancellation, bounded by localTimeout.
func localCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), localTimeout)
}

// checkout runs a git step that writes the working tree, under the local
// deadline, with git's pid in the marker while it runs.
func (s *Store) checkout(ctx context.Context, args ...string) (string, error) {
	lctx, cancel := localCtx(ctx)
	defer cancel()
	marker := filepath.Join(s.Dir, ".git", checkoutMarker)
	out, err := gitRun(lctx, s.Dir, nil, "", func(pid int) {
		_ = os.WriteFile(marker, []byte(fmt.Sprintf("%d %d\n", pid, time.Now().Unix())), 0o600)
	}, args...)
	_ = os.Remove(marker)
	return out, err
}

// gitLocks are the locks a fast-forward or a reset takes.
func (s *Store) gitLocks() []string {
	g := filepath.Join(s.Dir, ".git")
	locks := []string{filepath.Join(g, "index.lock"), filepath.Join(g, "HEAD.lock"), filepath.Join(g, "ORIG_HEAD.lock")}
	if ref, err := os.ReadFile(filepath.Join(g, "HEAD")); err == nil {
		if r, ok := strings.CutPrefix(strings.TrimSpace(string(ref)), "ref: "); ok && !strings.Contains(r, "..") {
			locks = append(locks, filepath.Join(g, filepath.FromSlash(r)+".lock"))
		}
	}
	return locks
}

// marker reads the checkout marker: git's pid and when it started.
func (s *Store) marker() (pid int, at time.Time, ok bool) {
	b, err := os.ReadFile(filepath.Join(s.Dir, ".git", checkoutMarker))
	if err != nil {
		return 0, time.Time{}, false
	}
	f := strings.Fields(string(b))
	if len(f) != 2 {
		return 0, time.Time{}, true
	}
	pid, _ = strconv.Atoi(f[0])
	sec, _ := strconv.ParseInt(f[1], 10, 64)
	return pid, time.Unix(sec, 0), true
}

// ErrCheckoutRunning is a sync that found a live git still writing the
// clone (a checkout another process started): nothing is touched.
var ErrCheckoutRunning = errors.New("another git is still writing this clone; try again when it is done")

// clearStaleLocks removes the git locks a dead git left, and the marker of
// a checkout that is over. A lock is stale when the marker's git is gone
// (or the marker is older than any checkout may run), or, with no marker,
// when the lock is older than staleLockAge. A live one is ErrCheckoutRunning.
// The caller holds WithLock, so no tacit process of this machine is
// writing the clone.
func (s *Store) clearStaleLocks(now time.Time) error {
	pid, at, hasMarker := s.marker()
	live := hasMarker && platform.Alive(pid) && now.Sub(at) < staleLockAge
	var held []string
	for _, l := range s.gitLocks() {
		fi, err := os.Lstat(l)
		if err != nil {
			continue
		}
		rel, _ := filepath.Rel(s.Dir, l)
		rel = filepath.ToSlash(rel)
		switch {
		case live:
			return fmt.Errorf("%w (%s is held by git pid %d, started %s ago)", ErrCheckoutRunning, rel, pid, now.Sub(at).Round(time.Second))
		case hasMarker:
			why := fmt.Sprintf("the checkout that held it (git pid %d) is gone", pid)
			if pid > 0 && platform.Alive(pid) {
				why = fmt.Sprintf("its checkout started %s ago, longer than any may run", now.Sub(at).Round(time.Second))
			}
			held = append(held, rel+": "+why)
		case now.Sub(fi.ModTime()) >= staleLockAge:
			held = append(held, fmt.Sprintf("%s: no checkout claims it and it is %s old", rel, now.Sub(fi.ModTime()).Round(time.Second)))
		default:
			return fmt.Errorf("%w (%s is %s old and no tacit checkout claims it; it counts as stale after %s)", ErrCheckoutRunning, rel, now.Sub(fi.ModTime()).Round(time.Second), staleLockAge)
		}
		if err := os.Remove(l); err != nil {
			return fmt.Errorf("remove the stale %s: %w", rel, err)
		}
	}
	if hasMarker && !live {
		_ = os.Remove(filepath.Join(s.Dir, ".git", checkoutMarker))
	}
	for _, h := range held {
		s.logRepair("removed a stale lock", []string{h})
	}
	return nil
}

// logRepair reports a repair and keeps it in the clone's own log.
func (s *Store) logRepair(what string, detail []string) {
	Logf("%s: %s", what, strings.Join(firstN(detail, 5), "; "))
	line, _ := json.Marshal(map[string]any{"at": time.Now().UTC().Format(time.RFC3339), "what": what, "detail": firstN(detail, 50), "count": len(detail)})
	f, err := os.OpenFile(filepath.Join(s.Dir, ".git", repairLog), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err == nil {
		f.Write(append(line, '\n'))
		f.Close()
	}
}

func firstN(xs []string, n int) []string {
	if len(xs) > n {
		return append(append([]string(nil), xs[:n]...), fmt.Sprintf("... %d more", len(xs)-n))
	}
	return xs
}

// changedPaths counts the paths a move from HEAD to target changes (a tree
// diff: no file is read).
func (s *Store) changedPaths(ctx context.Context, target string) (int, error) {
	out, err := git(ctx, s.Dir, "diff", "--name-only", "--no-renames", "-z", "HEAD", target)
	if err != nil {
		return 0, err
	}
	return strings.Count(out, "\x00"), nil
}

// deferUpdate is a Deferred when the caller's deadline is too short for
// the checkout the update needs, nil otherwise.
func (s *Store) deferUpdate(ctx context.Context, target string) error {
	dl, ok := ctx.Deadline()
	if !ok {
		return nil
	}
	left := time.Until(dl)
	if left > localTimeout {
		return nil
	}
	lctx, cancel := localCtx(ctx)
	defer cancel()
	n, err := s.changedPaths(lctx, target)
	if err != nil {
		return nil // the merge says what is wrong
	}
	need := checkoutFixed + time.Duration(n)*time.Second/checkoutRate
	if n > 0 && need > left {
		return &Deferred{Paths: n, Need: need, Left: left}
	}
	return nil
}

// dirt is one path git status lists.
type dirt struct {
	xy, path string
}

// status lists the working tree's changes, untracked and ignored files.
// The caller passes the local deadline (localCtx): a status is never cut short by the network
// legs' deadline (an interrupted update showed it running on an already
// spent one). On a large store it takes a fraction of a second with a stale
// index.lock and seconds on a fresh copy whose index has to be re-read.
func (s *Store) status(ctx context.Context) ([]dirt, error) {
	out, err := git(ctx, s.Dir, "status", "--porcelain=v1", "-z", "--no-renames", "--untracked-files=all", "--ignored")
	if err != nil {
		return nil, err
	}
	var ds []dirt
	for _, e := range strings.Split(out, "\x00") {
		if len(e) < 4 {
			continue
		}
		ds = append(ds, dirt{xy: e[:2], path: e[3:]})
	}
	return ds, nil
}

// proof is what code proved of a dirty clone: every path toward target.
type proof struct {
	target string
	paths  int
	// leftover: files of a commit between HEAD and target that target no
	// longer has; a reset leaves them untracked, so the repair removes them.
	leftover []string
}

// towardRemote proves that the clone's dirt is only an interrupted update
// toward target: HEAD is an ancestor of target; nothing is ignored; every
// listed file that exists holds the content (as git hashes it, filters
// applied) of target's blob at its path or of a blob some commit between
// HEAD and target wrote there; every listed file that is gone is absent
// from target or was deleted by such a commit. A path no commit of that
// range or target names is outside the store's layout and fails it. It
// returns why not when the proof fails. lctx is the local deadline.
func (s *Store) towardRemote(lctx context.Context, ds []dirt, target string) (proof, string) {
	p := proof{target: target, paths: len(ds)}
	if _, err := git(lctx, s.Dir, "merge-base", "--is-ancestor", "HEAD", target); err != nil {
		return p, "HEAD is not an ancestor of origin/main (local commits)"
	}
	for _, d := range ds {
		if d.xy == "!!" {
			return p, "an ignored file: " + d.path
		}
		if strings.ContainsAny(d.path, "\n\r") || strings.HasPrefix(d.path, "\"") {
			return p, "a path git quotes: " + d.path
		}
	}
	// target's tree, and what each commit between HEAD and it wrote.
	tree, err := git(lctx, s.Dir, "ls-tree", "-r", "-z", "--full-tree", target)
	if err != nil {
		return p, "ls-tree: " + err.Error()
	}
	inTarget := map[string]string{}
	for _, e := range strings.Split(tree, "\x00") {
		meta, path, ok := strings.Cut(e, "\t")
		if !ok {
			continue
		}
		f := strings.Fields(meta)
		if len(f) == 3 && f[1] == "blob" {
			inTarget[path] = f[2]
		}
	}
	allowed := map[string]map[string]bool{}
	deleted := map[string]bool{}
	allow := func(path, blob string) {
		if allowed[path] == nil {
			allowed[path] = map[string]bool{}
		}
		allowed[path][blob] = true
	}
	for path, blob := range inTarget {
		allow(path, blob)
	}
	raw, err := git(lctx, s.Dir, "log", "--raw", "-z", "-m", "--no-renames", "--no-abbrev", "--format=", "HEAD.."+target)
	if err != nil {
		return p, "log: " + err.Error()
	}
	fields := strings.Split(raw, "\x00")
	for i := 0; i+1 < len(fields); i++ {
		m := strings.TrimLeft(fields[i], "\n")
		if !strings.HasPrefix(m, ":") {
			continue
		}
		f := strings.Fields(m[1:])
		path := fields[i+1]
		i++
		if len(f) < 5 {
			continue
		}
		if strings.HasPrefix(f[4], "D") {
			deleted[path] = true
		} else if strings.Trim(f[3], "0") != "" {
			allow(path, f[3])
		}
	}
	// Hash every listed file that exists, in one git process.
	var exist []string
	for _, d := range ds {
		fi, err := os.Lstat(filepath.Join(s.Dir, filepath.FromSlash(d.path)))
		switch {
		case err == nil && fi.Mode().IsRegular():
			if allowed[d.path] == nil {
				return p, "a file no commit up to origin/main has: " + d.path
			}
			exist = append(exist, d.path)
		case err == nil:
			return p, "not a regular file: " + d.path
		case errors.Is(err, os.ErrNotExist):
			if _, there := inTarget[d.path]; there && !deleted[d.path] {
				return p, "a file origin/main has is missing: " + d.path
			}
		default:
			return p, err.Error()
		}
	}
	if len(exist) > 0 {
		out, err := gitEnvInput(lctx, s.Dir, nil, strings.Join(exist, "\n")+"\n", "hash-object", "--stdin-paths")
		if err != nil {
			return p, "hash-object: " + err.Error()
		}
		hs := strings.Fields(out)
		if len(hs) != len(exist) {
			return p, "hash-object answered for " + strconv.Itoa(len(hs)) + " of " + strconv.Itoa(len(exist)) + " files"
		}
		var partial []string
		for i, path := range exist {
			if !allowed[path][hs[i]] {
				partial = append(partial, path)
				continue
			}
			if _, there := inTarget[path]; !there {
				p.leftover = append(p.leftover, path)
			}
		}
		if bad := s.notPartial(lctx, partial, allowed); bad != "" {
			return p, "content that is not origin/main's or a commit's before it: " + bad
		}
		for _, path := range partial {
			if _, there := inTarget[path]; !there {
				p.leftover = append(p.leftover, path)
			}
		}
	}
	sort.Strings(p.leftover)
	return p, ""
}

// maxPartial bounds the files an interruption may leave half written: git
// writes a file in place, so the files its workers were writing when it was
// killed hold a start of their content (an empty file, in a measured
// kill). More than this is no interruption.
const maxPartial = 16

// notPartial is the first of paths whose content is not a start of a blob
// allowed at its path (what a killed checkout leaves of the file it was
// writing), "" when every one is; a start loses nothing when the repair
// writes the whole blob.
func (s *Store) notPartial(lctx context.Context, paths []string, allowed map[string]map[string]bool) string {
	if len(paths) == 0 {
		return ""
	}
	if len(paths) > maxPartial {
		return fmt.Sprintf("%s (and %d more files that differ)", paths[0], len(paths)-1)
	}
	for _, path := range paths {
		have, err := os.ReadFile(filepath.Join(s.Dir, filepath.FromSlash(path)))
		if err != nil {
			return path
		}
		ok := false
		for blob := range allowed[path] {
			want, err := git(lctx, s.Dir, "cat-file", "blob", blob)
			if err == nil && strings.HasPrefix(want, string(have)) {
				ok = true
				break
			}
		}
		if !ok {
			return path
		}
	}
	return ""
}

// repair finishes an interrupted update the proof covers: the clone is
// reset to the proof's target and the files of commits between that
// target no longer has are removed, each one checked again first.
func (s *Store) repair(ctx context.Context, p proof) error {
	head, _ := git(ctx, s.Dir, "rev-parse", "--short", "HEAD")
	if _, err := s.checkout(ctx, "reset", "--quiet", "--hard", p.target); err != nil {
		return err
	}
	var removed []string
	for _, path := range p.leftover {
		abs := filepath.Join(s.Dir, filepath.FromSlash(path))
		if _, err := git(ctx, s.Dir, "ls-files", "--error-unmatch", "--", path); err == nil {
			continue // tracked again: not ours to remove
		}
		if os.Remove(abs) == nil {
			removed = append(removed, path)
		}
	}
	tgt, _ := git(ctx, s.Dir, "rev-parse", "--short", p.target)
	detail := []string{fmt.Sprintf("HEAD %s -> %s; %d changed path(s) proven toward the remote", strings.TrimSpace(head), strings.TrimSpace(tgt), p.paths)}
	if len(removed) > 0 {
		detail = append(detail, fmt.Sprintf("removed %d file(s) of commits in between that origin/main no longer has: %s", len(removed), strings.Join(firstN(removed, 5), ", ")))
	}
	s.logRepair("finished an interrupted update", detail)
	return nil
}

// recoverDirty finishes an interrupted update when the clone's dirt is
// proven toward origin/main (towardRemote); it returns false with why
// when there is dirt the proof does not cover, true when the clone is
// clean or was repaired.
func (s *Store) recoverDirty(ctx context.Context) (bool, string, error) {
	ds, err := s.status(ctx)
	if err != nil {
		return false, "", err
	}
	if len(ds) == 0 {
		return true, "", nil
	}
	target, err := git(ctx, s.Dir, "rev-parse", "--verify", "origin/main")
	if err != nil {
		return false, "no origin/main", nil
	}
	p, why := s.towardRemote(ctx, ds, strings.TrimSpace(target))
	if why != "" {
		return false, why, nil
	}
	if err := s.repair(ctx, p); err != nil {
		return false, "", err
	}
	return true, "", nil
}

// dirtyError is the refusal of a clone whose dirt is not proven toward the
// remote, with the first reason.
func (s *Store) dirtyError(why string) error {
	msg := fmt.Sprintf("the clone has uncommitted, untracked or ignored files; fix %s by hand before syncing", s.Dir)
	if why != "" {
		msg += " (not an interrupted update: " + why + ")"
	}
	return errors.New(msg)
}

// readLog is the clone's repair log, for tests and doctor.
func (s *Store) readRepairLog() []byte {
	b, _ := os.ReadFile(filepath.Join(s.Dir, ".git", repairLog))
	return bytes.TrimSpace(b)
}
