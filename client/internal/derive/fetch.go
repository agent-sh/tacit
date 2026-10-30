package derive

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// FetchTimeout bounds the fetch of one branch of one checkout. Measured on the
// author's workstation at load 40 over ssh and https, one branch with a few days of new
// commits took 1.1 s (valkey-glide), 2.0 s (sglang dev), 2.6 s (memra) and 9.9
// s (sglang main, upstream's busiest branch). 30 s is three times the worst of
// those: a healthy fetch finishes, a dead or stalled link costs half a minute
// and not the three minutes a repo is otherwise allowed. A fetch cut short
// leaves the remote-tracking ref where it was (git updates it only after the
// objects arrived), so the read falls back to the local ref. Where one branch
// is further behind than 30 s can carry, the next run starts over and the
// stale read is visible by its commit date in the fact.
const FetchTimeout = 30 * time.Second

// maxFetchFailures: after this many fetches in a row fail, the rest of the
// run does not try. A dead network would otherwise cost every repo the
// timeout; a failure that is about one repo (a deleted remote, a revoked key)
// does not repeat three times in a row across different repos.
const maxFetchFailures = 3

// fetchSettings turn off everything a fetch does besides moving the one
// remote-tracking ref: no auto gc or maintenance, no commit-graph write, no
// submodule recursion, and a transfer that stalls under 1 KB/s for 10 s ends.
var fetchSettings = []string{
	"-c", "gc.auto=0", "-c", "maintenance.auto=false", "-c", "fetch.writeCommitGraph=false",
	"-c", "fetch.recurseSubmodules=false",
	// Git has no connect timeout; FetchTimeout bounds a connect to a dead host.
	"-c", "http.lowSpeedLimit=1000", "-c", "http.lowSpeedTime=10",
}

// fetcher counts consecutive failures across the repos of one run.
type fetcher struct {
	timeout  time.Duration
	failures int
	stopped  bool
}

func (f *fetcher) timeoutOrDefault() time.Duration {
	if f.timeout > 0 {
		return f.timeout
	}
	return FetchTimeout
}

// hasOrigin reports whether the checkout has a remote called origin.
func hasOrigin(ctx context.Context, dir string) bool {
	_, err := run(ctx, dir, "config", "--get", "remote.origin.url")
	return err == nil
}

// FetchBranch brings origin's branch into the remote-tracking ref
// refs/remotes/origin/<branch> and nothing else: not the working tree, not
// any local branch, not FETCH_HEAD, no tags. The refspec is explicit (and
// forced, a rebased dev moves backwards) so it works whatever refspec the
// remote is configured with.
func FetchBranch(ctx context.Context, dir, branch string, timeout time.Duration) error {
	if !ValidBranch(branch) {
		return fmt.Errorf("not a usable branch name: %q", branch)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	args := append(append([]string(nil), fetchSettings...),
		"fetch", "--no-tags", "--no-recurse-submodules", "--no-write-fetch-head", "--quiet",
		"origin", "+refs/heads/"+branch+":refs/remotes/origin/"+branch)
	_, err := run(ctx, dir, args...)
	return err
}

// shortErr is the first line of a git error, bounded, for a run report.
func shortErr(err error) string {
	s, _, _ := strings.Cut(err.Error(), "\n")
	if len(s) > 160 {
		s = s[:160] + "..."
	}
	return s
}
