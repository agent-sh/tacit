package check

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

type activityForge struct {
	fakeForge
	acts  map[string]Activity
	asked int
}

func (f *activityForge) Activity(_ context.Context, owner, name string, n int) (Activity, error) {
	f.asked++
	a, ok := f.acts[owner+"/"+name]
	if !ok {
		return Activity{}, ErrNotFound
	}
	if len(a.Commits) > n {
		a.Commits = a.Commits[:n]
	}
	return a, nil
}

// The forge's activity is kept a day, a missing repository too; a forge
// that cannot say it (no Activity) is unavailable, as is no forge.
func TestCacheActivity(t *testing.T) {
	ctx := context.Background()
	f := &activityForge{acts: map[string]Activity{"o/x": {FullName: "o/x", PushedAt: "2026-10-20", Commits: []ForgeCommit{{SHA: "a1", Day: "2026-10-20", Login: "o"}, {SHA: "b2", Day: "2026-10-01"}}}}}
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "forge.json")
	c := NewCache(f, path)
	c.Now = func() time.Time { return now }
	a, err := c.Activity(ctx, "o", "x", 1)
	if err != nil || a.PushedAt != "2026-10-20" || len(a.Commits) != 1 || a.Commits[0].Login != "o" {
		t.Fatalf("%+v %v", a, err)
	}
	if _, err := c.Activity(ctx, "o", "gone", 1); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	c2 := NewCache(f, path)
	c2.Now = func() time.Time { return now.Add(time.Hour) }
	c2.Activity(ctx, "o", "x", 1)
	c2.Activity(ctx, "o", "gone", 1)
	if f.asked != 2 {
		t.Fatalf("asked %d times, want 2 (cached)", f.asked)
	}
	c2.Now = func() time.Time { return now.Add(ActivityTTL + time.Minute) }
	c2.Activity(ctx, "o", "x", 1)
	if f.asked != 3 {
		t.Fatalf("a day-old answer was not asked again")
	}
	if _, err := NewCache(&fakeForge{}, "").Activity(ctx, "o", "x", 1); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("a forge with no Activity: %v", err)
	}
	var none *Cache
	if _, err := none.Activity(ctx, "o", "x", 1); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("no forge: %v", err)
	}
}

// GitHub's Activity reads the repository, its commits and its latest
// release from gh; no release is no error, and an empty repository (no
// commits to list) is a repository with none.
func TestGitHubActivity(t *testing.T) {
	answers := map[string]string{
		"api repos/o/x":                    `{"full_name":"o/x","pushed_at":"2026-10-20T10:00:00Z","archived":true}`,
		"api repos/o/empty":                `{"full_name":"o/empty","pushed_at":"2026-10-20T10:00:00Z"}`,
		"api repos/o/x/commits?per_page=2": `[{"sha":"ABC","commit":{"author":{"date":"2026-10-19T09:00:00Z"}},"author":{"login":"o"}},{"sha":"def","commit":{"author":{"date":"2026-10-02T09:00:00Z"}},"author":null}]`,
	}
	g := &GitHub{Bin: "gh", run: func(_ context.Context, _ string, args ...string) ([]byte, []byte, error) {
		k := args[0] + " " + args[1]
		if v, ok := answers[k]; ok {
			return []byte(v), nil, nil
		}
		return nil, []byte("gh: Not Found (HTTP 404)"), errors.New("exit 1")
	}}
	a, err := g.Activity(context.Background(), "o", "x", 2)
	if err != nil || a.FullName != "o/x" || a.PushedAt != "2026-10-20" || !a.Archived || len(a.Commits) != 2 || a.Commits[0].SHA != "abc" || a.Commits[0].Day != "2026-10-19" || a.Commits[0].Login != "o" || a.Commits[1].Login != "" || a.Release != "" {
		t.Fatalf("%+v %v", a, err)
	}
	if _, err := g.Activity(context.Background(), "o", "missing", 2); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if e, err := g.Activity(context.Background(), "o", "empty", 2); err != nil || e.FullName != "o/empty" || len(e.Commits) != 0 {
		t.Fatalf("empty: %+v %v", e, err)
	}
}
