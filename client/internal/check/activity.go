package check

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Activity is what a forge says of a repository's recent life: when it was
// last pushed to, whether it is archived, its newest commits on the default
// branch and its latest release. Evidence for a folder code could not
// settle otherwise (a folder left for review), never a verdict by itself.
type Activity struct {
	FullName   string        `json:"full_name"`
	PushedAt   string        `json:"pushed_at,omitempty"` // a day, YYYY-MM-DD
	Archived   bool          `json:"archived,omitempty"`
	Commits    []ForgeCommit `json:"commits,omitempty"` // newest first
	Release    string        `json:"release,omitempty"`
	ReleasedAt string        `json:"released_at,omitempty"`
}

// ForgeCommit is one commit of a repository's default branch on the forge.
type ForgeCommit struct {
	SHA   string `json:"sha"`
	Day   string `json:"day"`
	Login string `json:"login,omitempty"` // the forge account of its author, "" when none
}

// ActivityForge is a forge that can say a repository's Activity (n newest
// commits). GitHub through gh is one.
type ActivityForge interface {
	Activity(ctx context.Context, owner, name string, n int) (Activity, error)
}

// Activity asks gh for the repository, its n newest commits and its latest
// release (none is no error).
func (g *GitHub) Activity(ctx context.Context, owner, name string, n int) (Activity, error) {
	if !ValidRepo(owner, name) {
		return Activity{}, ErrNotFound
	}
	full := owner + "/" + name
	out, errb, err := g.run(ctx, g.Bin, "api", "repos/"+full)
	if err != nil {
		if strings.Contains(string(out)+string(errb), "HTTP 404") {
			return Activity{}, ErrNotFound
		}
		return Activity{}, ErrUnavailable
	}
	var r struct {
		FullName string `json:"full_name"`
		PushedAt string `json:"pushed_at"`
		Archived bool   `json:"archived"`
	}
	if json.Unmarshal(out, &r) != nil || !strings.Contains(r.FullName, "/") {
		return Activity{}, ErrUnavailable
	}
	a := Activity{FullName: r.FullName, PushedAt: day(r.PushedAt), Archived: r.Archived}
	if n > 0 {
		out, _, err := g.run(ctx, g.Bin, "api", "repos/"+r.FullName+"/commits?per_page="+strconv.Itoa(n))
		if err != nil {
			return Activity{}, ErrUnavailable
		}
		var cs []struct {
			SHA    string `json:"sha"`
			Commit struct {
				Author struct {
					Date string `json:"date"`
				} `json:"author"`
			} `json:"commit"`
			Author *struct {
				Login string `json:"login"`
			} `json:"author"`
		}
		if json.Unmarshal(out, &cs) != nil {
			return Activity{}, ErrUnavailable
		}
		for _, c := range cs {
			fc := ForgeCommit{SHA: strings.ToLower(c.SHA), Day: day(c.Commit.Author.Date)}
			if c.Author != nil {
				fc.Login = c.Author.Login
			}
			a.Commits = append(a.Commits, fc)
		}
	}
	if out, _, err := g.run(ctx, g.Bin, "api", "repos/"+r.FullName+"/releases/latest"); err == nil {
		var rel struct {
			Tag  string `json:"tag_name"`
			Date string `json:"published_at"`
		}
		if json.Unmarshal(out, &rel) == nil {
			a.Release, a.ReleasedAt = rel.Tag, day(rel.Date)
		}
	}
	return a, nil
}

func day(ts string) string {
	if len(ts) >= 10 {
		return ts[:10]
	}
	return ts
}

// ActivityTTL is how long a forge's Activity answer is kept: a day, as a
// push is news within one.
const ActivityTTL = 24 * time.Hour

// activityCommits is how many commits the cache asks for, whatever a caller
// wants: one answer serves every depth of a re-attempt.
const activityCommits = 20

// Activity is the forge's Activity for owner/name with its n newest commits
// (at most activityCommits), from the cache while it is fresh (ActivityTTL).
// A forge that cannot say it is ErrUnavailable.
func (c *Cache) Activity(ctx context.Context, owner, name string, n int) (Activity, error) {
	if c == nil {
		return Activity{}, ErrUnavailable
	}
	af, ok := c.Forge.(ActivityForge)
	if !ok {
		return Activity{}, ErrUnavailable
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.load()
	key := c.Forge.Name() + "-activity:" + strings.ToLower(owner+"/"+name)
	now := c.Now()
	cut := func(a Activity) Activity {
		if len(a.Commits) > n {
			a.Commits = append([]ForgeCommit(nil), a.Commits[:max(n, 0)]...)
		}
		return a
	}
	if e, ok := c.entries[key]; ok {
		if at, err := time.Parse(time.RFC3339, e.At); err == nil && now.Sub(at) < ActivityTTL {
			if !e.Found || e.Activity == nil {
				return Activity{}, ErrNotFound
			}
			return cut(*e.Activity), nil
		}
	}
	a, err := af.Activity(ctx, owner, name, activityCommits)
	switch {
	case err == nil:
		c.entries[key] = cacheEntry{Found: true, FullName: a.FullName, Activity: &a, At: stamp(now)}
		c.changed = true
	case errors.Is(err, ErrNotFound):
		c.entries[key] = cacheEntry{At: stamp(now)}
		c.changed = true
	}
	return cut(a), err
}
