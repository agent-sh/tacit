package search

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/platform"
)

// One builder per index file across processes: the builder holds an
// exclusive lock on <index>.lock (flock, LockFileEx on Windows) while it
// builds. The operating system drops the lock when its process exits, so a
// builder that crashed or was killed leaves no lock behind, only a file that
// the next builder locks again and prune removes. The file records the
// holder's pid and start time for a person reading the directory, and for
// lockStaleAge.
//
// Without it, every hook that found no index for the current overlay
// started its own full build (several <index>.<pid>.tmp at once) and none
// finished inside its deadline.

// lockStaleAge is how long a waiter respects a lock whose holder is alive.
// A full build of a large store took 9 to 25 s, and the callers that build
// in the background give up after 5 minutes, so a builder holding the lock
// for this long is stuck, and a waiter builds its own copy rather than wait
// forever (a duplicate build is
// wasted work, never a wrong index: every build writes its own temp file and
// renames it into place).
var lockStaleAge = 10 * time.Minute

// lockPoll is how often a waiter tries the lock again.
var lockPoll = 25 * time.Millisecond

type buildLock struct {
	f    *os.File
	path string
}

// tryLock takes the lock at path, or returns platform.ErrBusy when another
// holder has it. A lock file removed and created again between the open and
// the lock (a release, or prune) is not the file at path any more; it is
// opened again, so two holders never hold two different files of one name.
func tryLock(path string) (*buildLock, error) {
	for range 8 {
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
		if err != nil {
			return nil, err
		}
		if err := platform.TryLock(f); err != nil {
			f.Close()
			return nil, err
		}
		held, err1 := f.Stat()
		named, err2 := os.Stat(path)
		if err1 != nil || err2 != nil || !os.SameFile(held, named) {
			platform.Unlock(f)
			f.Close()
			continue
		}
		f.Truncate(0)
		f.WriteAt([]byte(fmt.Sprintf("%d %d\n", os.Getpid(), time.Now().Unix())), 0)
		return &buildLock{f: f, path: path}, nil
	}
	return nil, platform.ErrBusy
}

// waitLock takes the lock at path, waiting while another process holds it,
// until ctx ends. It returns nil and no error when the holder has held it
// longer than lockStaleAge: the caller builds without the lock.
func waitLock(ctx context.Context, path string) (*buildLock, error) {
	for {
		lk, err := tryLock(path)
		if err == nil {
			return lk, nil
		}
		if !errors.Is(err, platform.ErrBusy) {
			return nil, err
		}
		if _, since, ok := lockHolder(path); ok && time.Since(since) > lockStaleAge {
			return nil, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(lockPoll):
		}
	}
}

// building reports whether a process holds the lock at path now.
func building(path string) bool {
	lk, err := tryLock(path)
	if err == nil {
		lk.release()
		return false
	}
	return errors.Is(err, platform.ErrBusy)
}

// release removes the lock file while it is still held (a waiter that then
// locks the removed file sees it is not the file at path and opens again),
// then unlocks it. On Windows an open file is not always removable; prune
// removes it later.
func (lk *buildLock) release() {
	if lk == nil {
		return
	}
	os.Remove(lk.path)
	platform.Unlock(lk.f)
	lk.f.Close()
}

// lockHolder reads the pid and start time a holder wrote into the lock file.
func lockHolder(path string) (int, time.Time, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, time.Time{}, false
	}
	parts := strings.Fields(string(b))
	if len(parts) != 2 {
		return 0, time.Time{}, false
	}
	pid, err1 := strconv.Atoi(parts[0])
	sec, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, time.Time{}, false
	}
	return pid, time.Unix(sec, 0), true
}
