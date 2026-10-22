package check

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/repoid"
)

// accountForge is fakeForge with accounts: name -> type, "" for a 404.
type accountForge struct {
	fakeForge
	accounts map[string]string
}

func (f *accountForge) Account(_ context.Context, name string) (ForgeAccount, error) {
	f.calls++
	t, ok := f.accounts[name]
	switch {
	case !ok:
		return ForgeAccount{}, ErrUnavailable
	case t == "":
		return ForgeAccount{}, ErrNotFound
	}
	return ForgeAccount{Login: name, Type: t}, nil
}

// namesMachine is machine's widget with a symlink to it (gizmo), and a
// shared catalog of another machine with a checkout pikadon known as ginza
// (its origin's name), under the organization tools-org.
func namesMachine(t *testing.T) *Checker {
	t.Helper()
	home, _ := machine(t)
	if err := os.Symlink("widget", filepath.Join(home, "projects", "gizmo")); err != nil {
		t.Fatal(err)
	}
	cat := repoid.LoadCatalog(home, "", filepath.Join(home, "projects"))
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	ff := &accountForge{fakeForge: fakeForge{user: "owner", repos: map[string]string{
		"acme/widget": "acme/widget", "owner/ginza": "owner/ginza", "tools-org/hammer": "tools-org/anvil", "owner/gone": "",
	}}, accounts: map[string]string{"acme": "Organization", "owner": "User", "tools-org": "Organization"}}
	cache := NewCache(ff, filepath.Join(t.TempDir(), CacheFile))
	cache.Now = func() time.Time { return now }
	return &Checker{Host: "box", Catalog: cat, Forge: cache, Now: func() time.Time { return now },
		Shared: map[string][]repoid.HostRepo{
			"pikadon": {{Host: "rig", Aliases: []string{"ginza"}, URLs: []string{"github.com/owner/ginza"}}},
			"hammer":  {{Host: "rig", URLs: []string{"github.com/tools-org/hammer"}}},
		}}
}

// The canonical name of a repository by its checkouts: the main checkout's
// directory, here or on another machine; a symlink's or the origin's name is
// another name of it.
func TestCanonicalByTheMainCheckoutsDirectory(t *testing.T) {
	c := namesMachine(t)
	ctx := context.Background()
	for _, tc := range []struct{ name, other, verdict, value string }{
		{"widget", "gizmo", Holds, "widget"},
		{"gizmo", "widget", Fails, "widget"},
		{"pikadon", "ginza", Holds, "pikadon"},
		{"ginza", "pikadon", Fails, "pikadon"},
		{"pikadon", "", Holds, "pikadon"},
		{"ginza", "", Fails, "pikadon"},
		{"nowhere", "", Unknown, ""},
	} {
		r := c.Canonical(ctx, tc.name, tc.other)
		if r.Verdict != tc.verdict || r.Value != tc.value {
			t.Errorf("canonical %s/%s: %s %q (%s)", tc.name, tc.other, r.Verdict, r.Value, r.Output)
		}
	}
	if r := c.Canonical(ctx, "widget", "pikadon"); r.Verdict != Fails || !strings.Contains(r.Output, "two repositories") {
		t.Errorf("two repositories: %+v", r)
	}
	if cn, where := c.CheckoutName("ginza"); cn != "pikadon" || len(where) != 1 || !strings.Contains(where[0], "on rig") {
		t.Errorf("checkout name of ginza: %q %v", cn, where)
	}
	// A claim without its other name is well formed; a bad name is not.
	if ps := (Claim{Kind: "canonical", Name: "pikadon"}).Valid(); len(ps) != 0 {
		t.Errorf("%v", ps)
	}
	if ps := (Claim{Kind: "canonical", Name: "Owner/x"}).Valid(); len(ps) == 0 {
		t.Error("an owner/name is no repository name")
	}
}

// The forge's current name of a repository follows renames; its org is its
// owner when the forge says that owner is an organization; a personal
// account is none; an account no catalog's remote lives under is not
// looked up.
func TestForgeNameAndOrg(t *testing.T) {
	c := namesMachine(t)
	ctx := context.Background()
	if r := c.ForgeName(ctx, "github.com/owner/ginza"); r.Verdict != Holds || r.Value != "github.com/owner/ginza" {
		t.Errorf("ginza %+v", r)
	}
	r := c.ForgeName(ctx, "github.com/tools-org/hammer")
	if r.Verdict != Holds || r.Value != "github.com/tools-org/anvil" || !strings.Contains(r.Output, "renamed") {
		t.Errorf("hammer %+v", r)
	}
	if r := c.ForgeName(ctx, "github.com/owner/gone"); r.Verdict != Fails || !strings.Contains(r.Output, "who sees their own private repositories") {
		t.Errorf("gone %+v", r)
	}
	if org, r := c.OrgOf(ctx, "github.com/tools-org/anvil"); org != "tools-org" || r.Value != "Organization" {
		t.Errorf("tools-org %q %+v", org, r)
	}
	if org, r := c.OrgOf(ctx, "github.com/owner/ginza"); org != "" || r.Value != "User" {
		t.Errorf("a personal account is no org: %q %+v", org, r)
	}
	if ok, r := c.IsOrg(ctx, "stranger"); ok || r.Verdict != Unknown {
		t.Errorf("an account no remote lives under: %v %+v", ok, r)
	}
	// The URL to read: the origin, else the one remote on the forge.
	if u := c.ForgeURL(ctx, "github.com/acme/widget", []string{"github.com/acme/widget", "github.com/owner/widget"}); u != "github.com/acme/widget" {
		t.Errorf("origin %q", u)
	}
	if u := c.ForgeURL(ctx, "", []string{"github.com/acme/widget", "github.com/owner/widget"}); u != "github.com/owner/widget" {
		t.Errorf("two remotes, the account's %q", u)
	}
	if u := c.ForgeURL(ctx, "", []string{"github.com/acme/widget", "github.com/beta/widget"}); u != "" {
		t.Errorf("two remotes, none the account's %q", u)
	}
}

func (f *accountForge) Contents(_ context.Context, owner, name, path string) error {
	f.calls++
	if owner+"/"+name+":"+path == "acme/widget:infra/cdk.json" {
		return nil
	}
	return ErrNotFound
}

// A path in a repository with no checkout here is read at the forge's
// default branch, the answer cached.
func TestForgePath(t *testing.T) {
	c := namesMachine(t)
	ctx := context.Background()
	if r := c.ForgePath(ctx, "widget", "acme/widget", "infra/cdk.json"); r.Verdict != Holds || r.Source != "forge:github" {
		t.Fatalf("%+v", r)
	}
	if r := c.ForgePath(ctx, "widget", "acme/widget", "infra/bin/infra.js"); r.Verdict != Fails || !strings.Contains(r.Output, "has no infra/bin/infra.js") {
		t.Fatalf("%+v", r)
	}
	ff := c.Forge.Forge.(*accountForge)
	n := ff.calls
	c.ForgePath(ctx, "widget", "acme/widget", "infra/cdk.json")
	if ff.calls != n {
		t.Fatal("asked the forge again")
	}
}
