package overlay

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/fact"
)

func note(name, desc, body string) *fact.Fact {
	return &fact.Fact{Meta: fact.Meta{
		Name: name, Description: desc, Type: "unclassified", Scope: "user", Created: "2026-10-11",
		Source: fact.Source{Harness: "claude", Host: "host-b", Kind: "observed"},
	}, Body: body + "\n"}
}

func TestOutside(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "store")
	os.MkdirAll(store, 0o755)
	for _, c := range []struct {
		dir  string
		want bool
	}{
		{filepath.Join(root, "local"), true},
		{filepath.Join(root, "store-local"), true}, // a name prefix is not inside
		{store, false},
		{filepath.Join(store, "local"), false},
		{filepath.Join(store, "a", "b"), false},
		{filepath.Join(store, "..", "store", "x"), false},
	} {
		if got := Outside(c.dir, store); got != c.want {
			t.Errorf("Outside(%s) = %v, want %v", c.dir, got, c.want)
		}
	}
}

// A TACIT_LOCAL whose path runs through a symlink into the clone is inside,
// also before the overlay directory exists; and a link to outside is fine.
func TestOutsideResolvesSymlinks(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "store")
	os.MkdirAll(store, 0o755)
	link := filepath.Join(root, "innocent")
	if err := os.Symlink(store, link); err != nil {
		t.Fatal(err)
	}
	if Outside(filepath.Join(link, "local"), store) {
		t.Fatal("a not-yet-created overlay under a link into the clone counts as outside")
	}
	if Outside(link, store) {
		t.Fatal("a link to the clone counts as outside")
	}
	// The store itself reached through a link.
	if Outside(filepath.Join(store, "local"), link) {
		t.Fatal("the overlay inside a store named through a link counts as outside")
	}
	away := filepath.Join(root, "away")
	os.MkdirAll(away, 0o755)
	out := filepath.Join(root, "out")
	os.Symlink(away, out)
	if !Outside(filepath.Join(out, "local"), store) {
		t.Fatal("a link to a directory outside the clone counts as inside")
	}
	// Add refuses the linked-in overlay and writes nothing.
	if _, err := Add(filepath.Join(link, "local"), store, "claude@host-b", note("x", "d", "b"), func() string { return "s" }); !errors.Is(err, ErrInside) {
		t.Fatalf("Add through a link into the clone: %v, want ErrInside", err)
	}
	if _, err := os.Stat(filepath.Join(store, "local")); !os.IsNotExist(err) {
		t.Fatal("Add wrote into the clone")
	}
}

// A dangling symlink into the clone (x -> <store>/newdir, newdir missing)
// is followed by its target, so the overlay it names counts as inside, also
// through a relative link and a chain of links; a dangling link outside is
// fine, and a link loop is refused.
func TestOutsideFollowsDanglingSymlinks(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "store")
	os.MkdirAll(store, 0o755)

	abs := filepath.Join(root, "x")
	if err := os.Symlink(filepath.Join(store, "newdir"), abs); err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join(root, "y")
	os.Symlink(filepath.Join("store", "newdir"), rel)
	chain := filepath.Join(root, "z")
	os.Symlink(abs, chain)
	for _, d := range []string{abs, filepath.Join(abs, "local"), rel, filepath.Join(rel, "local"), filepath.Join(chain, "local")} {
		if Outside(d, store) {
			t.Errorf("Outside(%s) = true for a dangling link into the clone", d)
		}
	}
	if _, err := Add(filepath.Join(abs, "local"), store, "claude@host-b", note("x", "d", "b"), func() string { return "s" }); !errors.Is(err, ErrInside) {
		t.Fatalf("Add through a dangling link into the clone: %v, want ErrInside", err)
	}
	if _, err := os.Stat(filepath.Join(store, "newdir")); !os.IsNotExist(err) {
		t.Fatal("Add created the link's target inside the clone")
	}

	away := filepath.Join(root, "w")
	os.Symlink(filepath.Join(root, "elsewhere", "newdir"), away)
	if !Outside(filepath.Join(away, "local"), store) {
		t.Error("a dangling link outside the clone counts as inside")
	}

	a, b := filepath.Join(root, "loop-a"), filepath.Join(root, "loop-b")
	os.Symlink(b, a)
	os.Symlink(a, b)
	if Outside(filepath.Join(a, "local"), store) {
		t.Error("a symlink loop counts as outside")
	}
}

func TestAddLoadFindStamp(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "store")
	dir := filepath.Join(root, "local")
	os.MkdirAll(store, 0o755)
	if s := Stamp(dir); s != "" {
		t.Fatalf("stamp of a missing overlay = %q", s)
	}
	n := 0
	suffix := func() string { n++; return "s" + strings.Repeat("x", n) }

	p, err := Add(dir, store, "claude@host-b", note("brew", "brew is in /opt/homebrew", "body"), suffix)
	if err != nil || p != "local/claude@host-b/brew.md" {
		t.Fatalf("Add = %q, %v", p, err)
	}
	info, err := os.Stat(filepath.Join(dir, "claude@host-b", "brew.md"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("file: %v %v", info, err)
	}
	st1 := Stamp(dir)
	if st1 == "" {
		t.Fatal("no stamp after an add")
	}

	// Same description and body, another harness and name: not written again.
	again, err := Add(dir, store, "codex@host-b", note("other", "brew is in /opt/homebrew", "body"), suffix)
	if err != nil || again != p {
		t.Fatalf("duplicate Add = %q, %v; want %q", again, err, p)
	}
	// Same name, another fact: a suffix, nothing overwritten.
	q, err := Add(dir, store, "claude@host-b", note("brew", "another note", "else"), suffix)
	if err != nil || q != "local/claude@host-b/brew-sx.md" {
		t.Fatalf("second Add = %q, %v", q, err)
	}
	if Stamp(dir) == st1 {
		t.Fatal("the stamp did not change with a new note")
	}

	// A stray file that does not parse, a symlink and a temp file are skipped.
	os.WriteFile(filepath.Join(dir, "claude@host-b", "broken.md"), []byte("no frontmatter"), 0o600)
	os.Symlink(filepath.Join(dir, "claude@host-b", "brew.md"), filepath.Join(dir, "claude@host-b", "link.md"))
	os.WriteFile(filepath.Join(dir, "claude@host-b", "half.md.tmp"), []byte("x"), 0o600)
	facts, errs := Load(dir)
	if len(facts) != 2 || len(errs) != 1 {
		t.Fatalf("Load: %d facts, errs %v", len(facts), errs)
	}
	for _, f := range facts {
		if !strings.HasPrefix(f.Path, Prefix+"claude@host-b/") {
			t.Fatalf("path %q", f.Path)
		}
	}

	if f, ok := Find(dir, "brew"); !ok || f != filepath.Join(dir, "claude@host-b", "brew.md") {
		t.Fatalf("Find = %q, %v", f, ok)
	}
	if _, ok := Find(dir, "link"); ok {
		t.Fatal("Find followed a symlink")
	}
	if _, ok := Find(dir, "../x"); ok {
		t.Fatal("Find took a non-slug name")
	}
}
