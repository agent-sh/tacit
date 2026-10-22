package check

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Forge answers whether a repository exists on a code host and under which
// name it lives now (a host that follows renames answers with the new name).
// It is pluggable: GitHub through the gh command is the one built in.
type Forge interface {
	// Name is the forge's name in a result's source ("github").
	Name() string
	// Host is the normalized URL host its repositories live on ("github.com").
	Host() string
	// Lookup is the repository owner/name: its current full name, or
	// ErrNotFound, or ErrUnavailable when the forge could not answer.
	Lookup(ctx context.Context, owner, name string) (ForgeRepo, error)
	// User is the account the forge is used as ("" when unknown): its
	// repositories are where a bare name is looked for first.
	User(ctx context.Context) string
}

// ForgeRepo is a forge's answer for one repository.
type ForgeRepo struct {
	FullName string `json:"full_name"`
	Archived bool   `json:"archived,omitempty"`
}

var (
	// ErrNotFound: the forge answered, and has no such repository (that the
	// account can see).
	ErrNotFound = errors.New("not found")
	// ErrUnavailable: the forge did not answer (no network, no login, a
	// rate limit): no verdict.
	ErrUnavailable = errors.New("forge unavailable")
)

var (
	ownerRx = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)
	nameRx  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
)

// ValidRepo reports an owner and name a forge may be asked about: the
// characters a GitHub owner and repository may have, never "." or "..".
func ValidRepo(owner, name string) bool {
	return ownerRx.MatchString(owner) && nameRx.MatchString(name) && name != "." && name != ".."
}

// GitHub is the GitHub forge through the gh command (its own login).
type GitHub struct {
	Bin string // the gh executable
	// run executes gh; tests substitute it.
	run  func(ctx context.Context, bin string, args ...string) (stdout, stderr []byte, err error)
	once sync.Once
	user string
}

// ghTimeout bounds one gh call.
const ghTimeout = 20 * time.Second

func runGH(ctx context.Context, bin string, args ...string) ([]byte, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, ghTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1", "NO_COLOR=1", "GH_PAGER=", "PAGER=cat")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.Bytes(), errb.Bytes(), err
}

// NewGitHub is the GitHub forge when gh is installed and logged in ("gh auth
// status" succeeds), else nil: no forge, no forge verdicts.
func NewGitHub(ctx context.Context) Forge {
	bin, err := exec.LookPath("gh")
	if err != nil {
		return nil
	}
	g := &GitHub{Bin: bin, run: runGH}
	if _, _, err := g.run(ctx, bin, "auth", "status"); err != nil {
		return nil
	}
	return g
}

func (g *GitHub) Name() string { return "github" }
func (g *GitHub) Host() string { return "github.com" }

func (g *GitHub) Lookup(ctx context.Context, owner, name string) (ForgeRepo, error) {
	if !ValidRepo(owner, name) {
		return ForgeRepo{}, ErrNotFound
	}
	out, errb, err := g.run(ctx, g.Bin, "api", "repos/"+owner+"/"+name)
	if err != nil {
		all := string(out) + string(errb)
		if strings.Contains(all, "HTTP 404") || strings.Contains(all, `"status":"404"`) {
			return ForgeRepo{}, ErrNotFound
		}
		return ForgeRepo{}, ErrUnavailable
	}
	var r ForgeRepo
	if json.Unmarshal(out, &r) != nil || !strings.Contains(r.FullName, "/") {
		return ForgeRepo{}, ErrUnavailable
	}
	return r, nil
}

func (g *GitHub) User(ctx context.Context) string {
	g.once.Do(func() {
		out, _, err := g.run(ctx, g.Bin, "api", "user", "--jq", ".login")
		if l := strings.TrimSpace(string(out)); err == nil && ownerRx.MatchString(l) {
			g.user = l
		}
	})
	return g.user
}

// Cache keeps a forge's answers in a state file with a time to live, so a
// run asks the forge only about repositories it has not asked about lately,
// and a run with no network still has the answers it had. A found
// repository is kept for HitTTL, a missing one for MissTTL (shorter: a
// repository made since is found again soon). The forge's failures are
// never kept.
type Cache struct {
	Forge   Forge
	Path    string
	HitTTL  time.Duration
	MissTTL time.Duration
	Now     func() time.Time

	mu      sync.Mutex
	entries map[string]cacheEntry
	changed bool
	loaded  bool
}

type cacheEntry struct {
	Found    bool   `json:"found"`
	FullName string `json:"full_name,omitempty"`
	Archived bool   `json:"archived,omitempty"`
	At       string `json:"at"`
	// Activity is a repository's Activity (Cache.Activity's entries).
	Activity *Activity `json:"activity,omitempty"`
}

// Default times to live of the forge's answers.
const (
	DefaultHitTTL  = 7 * 24 * time.Hour
	DefaultMissTTL = 24 * time.Hour
)

// CacheFile is the forge cache's name in the state directory.
const CacheFile = "forge-cache.json"

// NewCache wraps f (nil: nil) with the cache at path.
func NewCache(f Forge, path string) *Cache {
	if f == nil {
		return nil
	}
	return &Cache{Forge: f, Path: path, HitTTL: DefaultHitTTL, MissTTL: DefaultMissTTL, Now: time.Now}
}

func (c *Cache) load() {
	if c.loaded {
		return
	}
	c.loaded = true
	c.entries = map[string]cacheEntry{}
	if c.Path == "" {
		return
	}
	if b, err := os.ReadFile(c.Path); err == nil {
		_ = json.Unmarshal(b, &c.entries)
	}
	if c.entries == nil {
		c.entries = map[string]cacheEntry{}
	}
}

// Lookup is the forge's answer for owner/name and when it was fetched, from
// the cache while it is fresh.
func (c *Cache) Lookup(ctx context.Context, owner, name string) (ForgeRepo, time.Time, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.load()
	key := c.Forge.Name() + ":" + strings.ToLower(owner+"/"+name)
	now := c.Now()
	if e, ok := c.entries[key]; ok {
		if at, err := time.Parse(time.RFC3339, e.At); err == nil {
			ttl := c.MissTTL
			if e.Found {
				ttl = c.HitTTL
			}
			if now.Sub(at) < ttl {
				if !e.Found {
					return ForgeRepo{}, at, ErrNotFound
				}
				return ForgeRepo{FullName: e.FullName, Archived: e.Archived}, at, nil
			}
		}
	}
	r, err := c.Forge.Lookup(ctx, owner, name)
	switch {
	case err == nil:
		c.entries[key] = cacheEntry{Found: true, FullName: r.FullName, Archived: r.Archived, At: stamp(now)}
		c.changed = true
	case errors.Is(err, ErrNotFound):
		c.entries[key] = cacheEntry{At: stamp(now)}
		c.changed = true
	}
	return r, now, err
}

// Save writes the cache when it changed (atomic rewrite; best effort).
func (c *Cache) Save() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.changed || c.Path == "" {
		return nil
	}
	b, err := json.MarshalIndent(c.entries, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.Path), 0o700); err != nil {
		return err
	}
	tmp := c.Path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, c.Path); err != nil {
		return err
	}
	c.changed = false
	return nil
}
