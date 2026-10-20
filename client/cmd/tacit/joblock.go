package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/agent-sh/tacit/client/internal/platform"
)

// The dreamer and the curator each hold one lock file in the state dir for a
// whole run (dreamer.lock, curator.lock): the 4-hour dream, the idle dream
// and the dream a PreCompact hook starts read and move the same cursors, and
// a quick curate and the full one split the same pending notes. The lock is
// an OS file lock, so it is released when the process ends, however it ends.

// tryJobLock takes <stateDir>/<name>.lock without waiting. It returns
// platform.ErrBusy when another process holds it.
func tryJobLock(stateDir, name string) (release func(), err error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(stateDir, name+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := platform.TryLock(f); err != nil {
		f.Close()
		return nil, err
	}
	// The holder's pid, for a person looking; nothing reads it.
	f.Truncate(0)
	f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	return func() { platform.Unlock(f); f.Close() }, nil
}

// waitJobLock takes the lock, waiting for its holder while ctx allows;
// waiting is called once, when the lock is first found held.
func waitJobLock(ctx context.Context, stateDir, name string, waiting func()) (func(), error) {
	told := false
	for {
		release, err := tryJobLock(stateDir, name)
		if !errors.Is(err, platform.ErrBusy) {
			return release, err
		}
		if !told && waiting != nil {
			waiting()
			told = true
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}
