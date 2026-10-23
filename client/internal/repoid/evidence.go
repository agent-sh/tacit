package repoid

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Commits is what git says of a checkout's commits under this machine's
// git identity (user.email, else user.name, as the checkout's config
// resolves it): how many, and the first and last commit day. It says
// whether the work is this machine's owner's, never whether it is done.
type Commits struct {
	Own   int    `json:"own"`
	First string `json:"first,omitempty"`
	Last  string `json:"last,omitempty"`
	// Days are the distinct days of those commits, newest first: the
	// folder's activity days (assess.Activity).
	Days []string `json:"days,omitempty"`
}

// gitTimeout bounds one git call of the evidence.
const gitTimeout = 20 * time.Second

func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// identity is the git author identity this machine commits under in dir.
func identity(ctx context.Context, dir string) string {
	if e, err := gitOut(ctx, dir, "config", "user.email"); err == nil && e != "" {
		return e
	}
	if n, err := gitOut(ctx, dir, "config", "user.name"); err == nil && n != "" {
		return n
	}
	return ""
}

// CommitEvidence reads Commits for the checkout at root; ok is false when
// git cannot say (no identity, no history git can read).
func CommitEvidence(ctx context.Context, root string) (Commits, bool) {
	id := identity(ctx, root)
	if id == "" {
		return Commits{}, false
	}
	if _, err := gitOut(ctx, root, "rev-parse", "--verify", "-q", "HEAD"); err != nil {
		return Commits{}, false
	}
	out, err := gitOut(ctx, root, "log", "--all", "--fixed-strings", "--author="+id, "--format=%cs")
	if err != nil {
		return Commits{}, false
	}
	var c Commits
	seen := map[string]bool{}
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); !dayRx.MatchString(l) {
			continue
		}
		c.Own++
		if !seen[l] {
			seen[l] = true
			c.Days = append(c.Days, l)
		}
		if c.First == "" || l < c.First {
			c.First = l
		}
		if l > c.Last {
			c.Last = l
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(c.Days)))
	return c, true
}

var dayRx = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// CommitCache keeps CommitEvidence per checkout by its HEAD and identity,
// in a state file (CommitCacheFile), so a run asks git for the history
// only of checkouts that moved.
type CommitCache struct {
	path    string
	entries map[string]cachedCommits
	changed bool
}

// commitCacheV is the cache entries' form (1: with the commits' days).
const commitCacheV = 1

// CommitCacheFile is the cache's name in the state directory.
const CommitCacheFile = "commit-evidence.json"

type cachedCommits struct {
	// V is the entry's form: commitCacheV reads the commits' days too (an
	// older entry is read again).
	V       int     `json:"v,omitempty"`
	Head    string  `json:"head"`
	ID      string  `json:"id"`
	Commits Commits `json:"commits"`
	OK      bool    `json:"ok"`
}

// LoadCommitCache reads the cache at path (none: empty).
func LoadCommitCache(path string) *CommitCache {
	c := &CommitCache{path: path, entries: map[string]cachedCommits{}}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &c.entries)
	}
	if c.entries == nil {
		c.entries = map[string]cachedCommits{}
	}
	return c
}

// Get is CommitEvidence for root, from the cache while root's HEAD and
// identity are unchanged.
func (c *CommitCache) Get(ctx context.Context, root string) (Commits, bool) {
	head, _ := gitOut(ctx, root, "rev-parse", "--verify", "-q", "HEAD")
	id := identity(ctx, root)
	if e, ok := c.entries[root]; ok && e.Head == head && e.ID == id && e.V >= commitCacheV {
		return e.Commits, e.OK
	}
	cm, ok := CommitEvidence(ctx, root)
	c.entries[root] = cachedCommits{V: commitCacheV, Head: head, ID: id, Commits: cm, OK: ok}
	c.changed = true
	return cm, ok
}

// Save writes the cache when it changed (atomic rewrite; best effort).
func (c *CommitCache) Save() error {
	if !c.changed || c.path == "" {
		return nil
	}
	keys := make([]string, 0, len(c.entries))
	for k := range c.entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	b, err := json.MarshalIndent(c.entries, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

// OwnCommit is one commit under this machine's git identity.
type OwnCommit struct {
	Hash    string // abbreviated
	Day     string // committer date, YYYY-MM-DD
	Subject string
}

// RecentOwnCommits are the newest n commits of the checkout at root under
// this machine's git identity (as CommitEvidence counts them), newest first;
// nil when git cannot say.
func RecentOwnCommits(ctx context.Context, root string, n int) []OwnCommit {
	id := identity(ctx, root)
	if id == "" || n <= 0 {
		return nil
	}
	out, err := gitOut(ctx, root, "log", "--all", "--fixed-strings", "--author="+id, "-n", strconv.Itoa(n), "--format=%h%x09%cs%x09%s")
	if err != nil {
		return nil
	}
	var cs []OwnCommit
	for _, l := range strings.Split(out, "\n") {
		parts := strings.SplitN(l, "\t", 3)
		if len(parts) != 3 || !dayRx.MatchString(parts[1]) {
			continue
		}
		cs = append(cs, OwnCommit{Hash: parts[0], Day: parts[1], Subject: parts[2]})
	}
	return cs
}

// ChangedCommit is one commit with the files its diff changes (a merge's
// against its first parent).
type ChangedCommit struct {
	Hash  string // full
	Day   string // committer date, YYYY-MM-DD
	Merge bool
	Files []string
}

// OwnCommitsAfter are the commits of the checkout at root under this
// machine's git identity whose committer day is after the day after
// (YYYY-MM-DD), newest first, at most n, each with the files it changes;
// nil when git cannot say or there are none.
func OwnCommitsAfter(ctx context.Context, root, after string, n int) []ChangedCommit {
	id := identity(ctx, root)
	if id == "" || n <= 0 || !dayRx.MatchString(after) {
		return nil
	}
	out, err := gitOut(ctx, root, "log", "--all", "--fixed-strings", "--author="+id, "--since="+after+"T23:59:59", "-n", strconv.Itoa(n),
		"--diff-merges=first-parent", "--name-only", "--format=%x1e%H%x09%cs%x09%P")
	if err != nil {
		return nil
	}
	var cs []ChangedCommit
	for _, rec := range strings.Split(out, "\x1e") {
		lines := strings.Split(strings.TrimSpace(rec), "\n")
		if len(lines) == 0 {
			continue
		}
		parts := strings.SplitN(lines[0], "\t", 3)
		if len(parts) < 2 || !dayRx.MatchString(parts[1]) || parts[1] <= after {
			continue
		}
		c := ChangedCommit{Hash: parts[0], Day: parts[1], Merge: len(parts) == 3 && len(strings.Fields(parts[2])) > 1}
		for _, f := range lines[1:] {
			if f = strings.TrimSpace(f); f != "" {
				c.Files = append(c.Files, f)
			}
		}
		cs = append(cs, c)
	}
	return cs
}
