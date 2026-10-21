package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/agent-sh/tacit/client/internal/auth"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/follow"
	"github.com/agent-sh/tacit/client/internal/store"
)

// Exit codes of `tacit follow` that the service unit knows about. 78 (EX_CONFIG)
// is an invocation a restart would only repeat: the unit lists it in
// RestartPreventExitStatus. Everything else restarts, including 2, which is
// what Go exits with on a panic. 75 (EX_TEMPFAIL) is another follower already
// running for the store.
const (
	exitConfig  = 78
	exitRunning = 75
)

// runFollow implements `tacit follow`: hold the server's change feed open
// and, when it announces a push, run what `tacit sync` runs (fetch,
// fast-forward, reindex). The last synced seq and a heartbeat go to
// ~/.local/state/tacit/follow.json (a per-store name for any other store);
// the SessionStart hook skips its own network sync while that heartbeat is
// fresh and recently verified against the server.
//
// Reading the feed over the tailnet needs no key (the tailnet identity is
// enough), so by default nothing is signed, except on a read-only clone made
// with `--as` (tacit.as), which signs with that key: the server's public path
// asks a key on every request. --as HARNESS signs each connection explicitly.
//
// A missing clone or an unreadable origin is waited out, not fatal: the
// clone may be mid-re-clone and a lookup can time out under load. Exit codes:
// 0 after a signal, 78 for a bad invocation, 75 when another follower holds
// the store.
func runFollow(args []string) int {
	code := runFollowCode(args)
	if code == exitConfig && underLaunchd(args) {
		// launchd cannot exempt one exit code from KeepAlive the way
		// RestartPreventExitStatus does; under it a bad invocation exits 0
		// (SuccessfulExit=false restarts only failures) and the reason stays
		// in the log above.
		fmt.Fprintln(os.Stderr, "follow: not restarting: fix the invocation above, then launchctl kickstart -k gui/$(id -u)/"+launchdFollowLabel)
		return 0
	}
	return code
}

// underLaunchd reports the --launchd flag, read from the raw arguments so it
// holds even when another flag fails to parse.
func underLaunchd(args []string) bool {
	for _, a := range args {
		if a == "--launchd" || a == "-launchd" || a == "--launchd=true" || a == "-launchd=true" {
			return true
		}
	}
	return false
}

func runFollowCode(args []string) int {
	fs := flag.NewFlagSet("follow", flag.ContinueOnError)
	url := fs.String("url", "", "server base URL (default: the clone's origin without /git)")
	as := fs.String("as", "", "sign each connection as <harness>@<host> (default: anonymous; reading needs no key)")
	logPath := fs.String("log", "", "write the log to this file, at most 1 MiB plus one FILE.1 (for launchd, which rotates nothing)")
	fs.Bool("launchd", false, "run under launchd (macOS): a bad invocation exits 0 instead of 78, so KeepAlive does not restart it")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return exitConfig
	}

	s, err := store.Default()
	if err != nil {
		fmt.Fprintln(os.Stderr, "follow:", err)
		return exitConfig
	}
	if s.Exists() && s.Local(context.Background()) {
		fmt.Println("follow: local store; no server feed is needed")
		return exitConfig
	}
	opts := follow.Options{
		StatePath: follow.StatePath(s.Dir),
		Store:     s.Dir,
		Resolve:   func(ctx context.Context) (string, error) { return followURL(ctx, s, *url) },
		Sync:      func(ctx context.Context) (string, error) { return followSync(ctx, s) },
		Verify:    s.RemoteAhead,
		Logf:      func(f string, a ...any) { fmt.Fprintf(os.Stderr, "follow: "+f+"\n", a...) },
	}
	if *logPath != "" {
		l, err := openCapLog(*logPath, followLogCap)
		if err != nil {
			fmt.Fprintln(os.Stderr, "follow: --log:", err)
			return exitConfig
		}
		opts.Logf = func(f string, a ...any) { l.Printf("follow: "+f, a...) }
	}
	if *as != "" {
		if !fact.IsSlug(*as) {
			fmt.Fprintln(os.Stderr, "follow: --as wants a harness name")
			return exitConfig
		}
		id := *as + "@" + shortHost(mustHostname())
		dir, err := auth.Dir()
		if err != nil {
			fmt.Fprintln(os.Stderr, "follow:", err)
			return exitConfig
		}
		key, err := auth.Load(dir, id)
		if err != nil {
			fmt.Fprintln(os.Stderr, "follow: no key for "+id+":", err)
			return exitConfig
		}
		opts.Auth = func() (string, string, error) { return id, key.Token(time.Now()), nil }
	} else {
		// A read-only clone made with --as (the public path asks a key on
		// every request) signs the feed with the same key. Read at each
		// connection: the clone may appear after the follower starts.
		opts.Auth = func() (string, string, error) { return cloneAuth(s) }
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = follow.Run(ctx, opts)
	var running *follow.ErrRunning
	switch {
	case errors.As(err, &running):
		fmt.Fprintln(os.Stderr, "follow:", err)
		return exitRunning
	case err != nil:
		fmt.Fprintln(os.Stderr, "follow:", err)
		return exitConfig
	}
	return 0
}

// cloneAuth is the key id and a fresh token for the harness a read-only
// clone fetches with (tacit.as), or "" (anonymous) when it names none.
func cloneAuth(s *store.Store) (string, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	as := s.ReadAs(ctx)
	if as == "" || !s.Exists() {
		return "", "", nil
	}
	if !fact.IsSlug(as) {
		return "", "", fmt.Errorf("the clone's tacit.as %q is not a harness name", as)
	}
	id := as + "@" + shortHost(mustHostname())
	dir, err := auth.Dir()
	if err != nil {
		return "", "", err
	}
	key, err := auth.Load(dir, id)
	if err != nil {
		return "", "", fmt.Errorf("the clone fetches as %s but this machine has no key for it: %w", id, err)
	}
	return id, key.Token(time.Now()), nil
}

// followURL is the feed endpoint for the clone: --url, else the clone's
// origin. An error means "not yet" and is retried by the follower.
func followURL(ctx context.Context, s *store.Store, override string) (string, error) {
	if !s.Exists() {
		return "", errors.New("no store at " + s.Dir + "; run `tacit clone <url> <harness>`")
	}
	if override != "" {
		return eventsURL(override), nil
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	origin, err := s.RemoteURL(rctx)
	if err != nil || origin == "" {
		return "", fmt.Errorf("cannot read the clone's origin: %v", err)
	}
	return eventsURL(origin), nil
}

// eventsURL turns the clone's origin (http://host:7447/git) or a bare base
// URL into the feed endpoint.
func eventsURL(base string) string {
	base = strings.TrimSuffix(strings.TrimSuffix(base, "/"), "/git")
	return base + "/events"
}

func mustHostname() string {
	h, _ := os.Hostname()
	return h
}

// followSync is what `tacit sync` does, arranged for a process that stays
// up: the fetch runs without the store lock (SyncBrief), so a hook or
// `tacit add` never waits behind a slow network; the lock covers only the
// local merge. The index is rebuilt for the new HEAD so the next hook finds
// it ready. Local commits stranded by a conflict leave the clone matching
// the server, which is a successful sync.
func followSync(ctx context.Context, s *store.Store) (string, error) {
	err := s.SyncBrief(ctx)
	var stranded *store.Stranded
	if err != nil && !errors.As(err, &stranded) {
		return "", err
	}
	head, err := s.Head(ctx)
	if err != nil {
		return "", err
	}
	ix, err := openBuildIndex(ctx, s.Dir, head)
	if err != nil {
		return "", fmt.Errorf("index: %w", err)
	}
	ix.Close()
	if stranded != nil {
		fmt.Fprintln(os.Stderr, "follow:", stranded)
	}
	return head, nil
}
