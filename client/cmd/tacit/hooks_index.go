package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/agent-sh/tacit/client/internal/platform"
	"github.com/agent-sh/tacit/client/internal/search"
	"github.com/agent-sh/tacit/client/internal/store"
)

// Which callers read the exact index and which may read an older one.
//
// The hooks (session-start, prompt, pre-tool) and the tree tool's counts are
// bound by latency: openHookIndex (search.OpenRecent) reads the closest
// index there is and leaves the build to a detached `tacit hook index`, so a
// new HEAD or a change of this machine's overlay never costs a session a
// hook. What they print may miss the newest local notes or facts for one
// prompt.
//
// Everything else opens the exact index (openIndex, search.Open), building
// it when missing and waiting for a process that is building it: the
// curator's conflict checks, which must never judge a fact against an index
// that lacks the facts it might conflict with; `tacit search` and the MCP
// search tool, an explicit question whose answer must hold what was just
// written; sync and follow, which build ahead for the hooks; the owner app.

// indexBuildBudget bounds a build that is not answering anyone: sync,
// follow, the hooks' background build. Measured on a copy of a large store
// and its overlay on a heavily loaded machine: a build of the store from
// nothing took 8.8 to 24.5 s, the overlay's file built from the store's
// 0.17 s. Sync's own deadline (2 minutes) is the network's, so the index gets
// its own.
const indexBuildBudget = 5 * time.Minute

// indexBuildLog caps the background builds' log, as dreamSessionLog does.
const indexBuildLog = 1 << 20

// openHookIndex is the hooks' index: the closest one there is, never a build
// while one is there (search.OpenRecent).
func openHookIndex(ctx context.Context, s *store.Store) (*search.Index, error) {
	head, err := s.Head(ctx)
	if err != nil {
		return nil, err
	}
	return search.OpenRecent(ctx, s.Dir, head, startIndexBuild)
}

// startIndexBuild starts `tacit hook index` detached (platform.StartDetached:
// it outlives the hook), with no stdin and its output in index-build.log in
// the state dir. Tests substitute it.
var startIndexBuild = func() {
	exe, err := hookExe()
	if err != nil {
		return
	}
	home, _ := os.UserHomeDir()
	dir := platform.StateDir(home)
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	path := filepath.Join(dir, "index-build.log")
	if fi, err := os.Stat(path); err == nil && fi.Size() > indexBuildLog {
		os.Rename(path, path+".1")
	}
	log, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer log.Close()
	cmd := exec.Command(exe, "hook", "index")
	cmd.Stdout, cmd.Stderr = log, log
	if p, err := platform.StartDetached(cmd); err == nil {
		p.Release()
	}
}

// buildIndexForHooks is `tacit hook index`: build the exact index for the
// store's HEAD and this machine's overlay, as the hooks will ask for it.
// Another process building it already makes this one wait and then find it.
func buildIndexForHooks(s *store.Store) {
	ctx, cancel := context.WithTimeout(context.Background(), indexBuildBudget)
	defer cancel()
	t0 := time.Now()
	ix, err := openIndex(ctx, s)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s tacit hook index: %v\n", time.Now().Format(time.RFC3339), err)
		return
	}
	ix.Close()
	if d := time.Since(t0); d > time.Second {
		fmt.Fprintf(os.Stderr, "%s tacit hook index: ready in %v\n", time.Now().Format(time.RFC3339), d.Round(time.Millisecond))
	}
}

// openBuildIndex opens the exact index with indexBuildBudget, not the
// caller's deadline (a sync's is for the network), while a cancel of the
// caller's context (a signal) still stops it.
func openBuildIndex(ctx context.Context, dir, head string) (*search.Index, error) {
	ictx, cancel := context.WithTimeout(context.WithoutCancel(ctx), indexBuildBudget)
	defer cancel()
	stop := context.AfterFunc(ctx, func() {
		if errors.Is(ctx.Err(), context.Canceled) {
			cancel()
		}
	})
	defer stop()
	return search.Open(ictx, dir, head)
}
