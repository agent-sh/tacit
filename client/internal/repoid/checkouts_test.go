package repoid

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A linked worktree's or a separate clone's directory name keeps its
// repository after the checkout is gone: git lists the worktree while it
// exists, the registry keeps what a session saw, and the shared catalog
// carries the names (never the paths) to other machines.
func TestCheckoutsKeepTheirAncestor(t *testing.T) {
	home := realTemp(t)
	p := filepath.Join(home, "projects")
	t.Setenv("TACIT_PROJECTS", p)
	main := filepath.Join(p, "widget")
	mkRepo(t, main, "git@github.com:acme/widget.git")
	wt := filepath.Join(home, "work", "wt-widget-fix-42")
	mkWorktree(t, main, wt)
	// git's own gitdir file in the admin directory, as `git worktree add` writes it.
	write(t, filepath.Join(main, ".git", "worktrees", filepath.Base(wt), "gitdir"), filepath.Join(wt, ".git")+"\n")
	// A clone of another repository, with no main checkout here.
	up := filepath.Join(home, "scratch", "upstream-copy")
	mkRepo(t, up, "https://github.com/other/libthing.git")

	c := LoadCatalog(home, "", p)
	if repo, kind, ok := c.CheckoutOwner("wt-widget-fix-42"); !ok || repo != "widget" || kind != CheckoutWorktree {
		t.Fatalf("listed worktree: %q %q %v", repo, kind, ok)
	}
	if _, _, ok := c.CheckoutOwner("widget"); ok {
		t.Fatal("a main checkout's own name is no other checkout")
	}

	// A session in the worktree and one in the clone: recorded with their
	// repositories.
	reg := filepath.Join(home, "state", RegistryFile)
	mains := LoadMains(home, p)
	r1, _ := OfMachine(filepath.Join(wt, "src"), home)
	r2, _ := OfMachine(up, home)
	if err := RecordCheckouts(reg, mains, r1, r2); err != nil {
		t.Fatal(err)
	}
	if _, ch, ok := RecordedCheckout(reg, filepath.Join(wt, "src", "x.go")); !ok || ch.Repo != "widget" || ch.Kind != CheckoutWorktree || ch.Main != main {
		t.Fatalf("recorded worktree: %+v %v", ch, ok)
	}
	if _, ch, ok := RecordedCheckout(reg, up); !ok || ch.Repo != "libthing" || ch.Kind != CheckoutClone || ch.Origin != "github.com/other/libthing" {
		t.Fatalf("recorded clone: %+v %v", ch, ok)
	}

	// The worktree is removed (directory and git's admin entry): the
	// registry still knows it.
	os.RemoveAll(wt)
	os.RemoveAll(filepath.Join(main, ".git", "worktrees"))
	c = LoadCatalog(home, reg, p)
	if repo, _, ok := c.CheckoutOwner("wt-widget-fix-42"); !ok || repo != "widget" {
		t.Fatalf("removed worktree: %q %v", repo, ok)
	}
	if repo, _, ok := c.CheckoutOwner("upstream-copy"); !ok || repo != "libthing" {
		t.Fatalf("clone: %q %v", repo, ok)
	}

	// Another machine reads the names from the shared catalog; no path.
	b := c.Shared()
	if !strings.Contains(string(b), `"wt-widget-fix-42"`) || strings.Contains(string(b), home) {
		t.Fatalf("shared catalog:\n%s", b)
	}
	other := LoadCatalog(realTemp(t), "", "")
	other.AddShared(b)
	if repo, kind, ok := other.CheckoutOwner("wt-widget-fix-42"); !ok || repo != "widget" || kind != CheckoutAny {
		t.Fatalf("other machine: %q %q %v", repo, kind, ok)
	}

	// A name two repositories' checkouts share names neither.
	c.addCheckoutName("scratch", "widget", CheckoutWorktree)
	c.addCheckoutName("scratch", "gadget", CheckoutWorktree)
	c.addCheckoutName("scratch", "widget", CheckoutWorktree)
	if _, _, ok := c.CheckoutOwner("scratch"); ok {
		t.Fatal("an ambiguous name was given an owner")
	}
}
