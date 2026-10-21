package overlay

import (
	"errors"
	"io/fs"
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

// The local layer's files: a dreamer note is an inbox note; a fact of the
// local curator is a ruling only with its quote in the local ledger; an
// agent's note, a dreamer note and a curated fact that only claims the fields
// never are. The review directory holds no facts and is not loaded or found.
func TestLocalLayerFiles(t *testing.T) {
	t.Setenv("TACIT_LOCAL_LAYER", "on")
	root := t.TempDir()
	store := filepath.Join(root, "store")
	dir := filepath.Join(root, "local")
	os.MkdirAll(store, 0o755)
	claim := func(name, harness string) *fact.Fact {
		f := note(name, "never deploy the walrus service on fridays without a plan", "body")
		f.Meta.Type, f.Meta.Source.Harness, f.Meta.Source.Kind = "user_standard", harness, "user"
		f.Meta.Source.OwnerQuote, f.Meta.Source.OwnerQuoteFrom = "never deploy the walrus service on fridays without a plan", "claude:s9"
		return f
	}
	put := func(rel string, f *fact.Fact) {
		t.Helper()
		b, err := f.Encode()
		if err != nil {
			t.Fatal(err)
		}
		if err := Write(dir, store, rel, b); err != nil {
			t.Fatal(err)
		}
	}
	put("curated/user/backed.md", claim("backed", "curator"))
	put("curated/user/unbacked.md", claim("unbacked", "curator"))
	put("curated/user/human.md", claim("human", "human"))
	put("inbox/dreamer@host-b/dreamed.md", claim("dreamed", "dreamer"))
	os.MkdirAll(filepath.Join(dir, "codex@host-b"), 0o700)
	b, _ := claim("agents", "codex").Encode()
	os.WriteFile(filepath.Join(dir, "codex@host-b", "agents.md"), b, 0o600)
	line := func(name string) string {
		return `{"name":"` + name + `","quote":"never deploy the walrus service on fridays without a plan","from":"claude:s9","turn_hash":"x","at":"2026-10-20"}` + "\n"
	}
	before := Stamp(dir)
	for _, n := range []string{"backed", "human", "dreamed", "agents"} {
		if err := Append(dir, store, QuotesRel, []byte(line(n))); err != nil {
			t.Fatal(err)
		}
	}
	if Stamp(dir) == before {
		t.Fatal("the stamp ignores the local quote ledger")
	}
	if err := Append(dir, store, ReviewDir+"/2026-10-20.md", []byte("- supersedes? [[a]] may replace the store's [[b]]\n")); err != nil {
		t.Fatal(err)
	}
	// The layer's folder indexes (no frontmatter) are no facts, and Remove
	// keeps to the layer like Write.
	for _, rel := range []string{"curated/" + fact.IndexFile, "curated/user/" + fact.IndexFile} {
		if err := Write(dir, store, rel, []byte("# index\n")); err != nil {
			t.Fatal(err)
		}
	}
	if err := Remove(dir, store, "curated/"+fact.IndexFile); err != nil {
		t.Fatal(err)
	}
	if err := Remove(dir, store, "../x.md"); err == nil {
		t.Fatal("Remove left the layer")
	}
	facts, errs := Load(dir)
	if len(errs) != 0 || len(facts) != 5 {
		t.Fatalf("Load: %d facts, %v", len(facts), errs)
	}
	for _, f := range facts {
		want := f.Meta.Name == "backed"
		if IsRuling(f) != want {
			t.Errorf("%s (%s): IsRuling = %v, want %v", f.Meta.Name, f.Path, IsRuling(f), want)
		}
		if IsInbox(f.Path) != (f.Meta.Name == "dreamed") {
			t.Errorf("%s: IsInbox = %v", f.Path, IsInbox(f.Path))
		}
	}
	if _, ok := Find(dir, "2026-10-20"); ok {
		t.Fatal("Find took a review file for a fact")
	}
	// Writes stay in the layer's directories and outside the clone.
	for _, rel := range []string{"claude@host-b/x.md", "../x.md", "/etc/x.md", "curated/../../x.md", "x.md"} {
		if err := Write(dir, store, rel, []byte("x")); err == nil {
			t.Errorf("Write(%q) wrote outside the layer", rel)
		}
	}
	if err := Write(filepath.Join(store, "local"), store, "inbox/dreamer@h/x.md", []byte("x")); !errors.Is(err, ErrInside) {
		t.Fatalf("Write into the clone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store, "local")); !os.IsNotExist(err) {
		t.Fatal("Write created a directory in the clone")
	}
}

func TestLayerOn(t *testing.T) {
	for v, want := range map[string]bool{"": false, "off": false, "false": false, "no": false, "on": true, "ON": true, "true": true, "1": true, "yes": true} {
		t.Setenv("TACIT_LOCAL_LAYER", v)
		if LayerOn() != want {
			t.Errorf("TACIT_LOCAL_LAYER=%q: LayerOn = %v", v, !want)
		}
	}
}

// A write lands where its final path resolves: a curated/ or inbox/ (or an
// agent's writer directory) linked into the clone, or out of the overlay, is
// refused and nothing reaches the target.
func TestLayerWritesResolveTheFinalPath(t *testing.T) {
	t.Setenv("TACIT_LOCAL_LAYER", "on")
	root := t.TempDir()
	store := filepath.Join(root, "store")
	away := filepath.Join(root, "away")
	dir := filepath.Join(root, "local")
	for _, d := range []string{filepath.Join(store, "user"), away, dir} {
		os.MkdirAll(d, 0o755)
	}
	os.Symlink(store, filepath.Join(dir, "curated"))
	os.Symlink(filepath.Join(store, "user"), filepath.Join(dir, "inbox"))
	if err := Write(dir, store, "curated/user/escaped.md", []byte("x")); !errors.Is(err, ErrInside) {
		t.Fatalf("Write through a linked curated/: %v", err)
	}
	if err := Append(dir, store, "inbox/dreamer@h/escaped.md", []byte("x")); !errors.Is(err, ErrInside) {
		t.Fatalf("Append through a linked inbox/: %v", err)
	}
	os.Remove(filepath.Join(dir, "curated"))
	os.Symlink(away, filepath.Join(dir, "curated"))
	if err := Write(dir, store, "curated/user/escaped.md", []byte("x")); err == nil {
		t.Fatal("Write through a curated/ linked out of the overlay")
	}
	// A linked file at the final path.
	os.Remove(filepath.Join(dir, "curated"))
	os.MkdirAll(filepath.Join(dir, "curated", "user"), 0o700)
	os.Symlink(filepath.Join(store, "user", "target.md"), filepath.Join(dir, "curated", "user", "x.md"))
	if err := Write(dir, store, "curated/user/x.md", []byte("x")); !errors.Is(err, ErrInside) {
		t.Fatalf("Write to a file linked into the clone: %v", err)
	}
	// An agent's writer directory linked into the clone.
	os.Symlink(filepath.Join(store, "user"), filepath.Join(dir, "claude@host-b"))
	if _, err := Add(dir, store, "claude@host-b", note("x", "a linked writer dir", "b"), func() string { return "s" }); !errors.Is(err, ErrInside) {
		t.Fatalf("Add through a linked writer dir: %v", err)
	}
	var found []string
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && (strings.HasPrefix(p, store) || strings.HasPrefix(p, away)) {
			found = append(found, p)
		}
		return nil
	})
	if len(found) != 0 {
		t.Fatalf("written outside the overlay: %v", found)
	}
}

// With the layer off the overlay is the agents' notes alone: the layer's
// files are not loaded, found or stamped, so no index holds them.
func TestLayerOffHidesTheLayersFiles(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "store")
	dir := filepath.Join(root, "local")
	os.MkdirAll(store, 0o755)
	t.Setenv("TACIT_LOCAL_LAYER", "on")
	for _, rel := range []string{"curated/user/kept.md", "inbox/dreamer@h/dreamed.md"} {
		b, _ := note(strings.TrimSuffix(filepath.Base(rel), ".md"), "a layer file", "b").Encode()
		if err := Write(dir, store, rel, b); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Add(dir, store, "claude@host-b", note("agents", "an agent's note", "b"), func() string { return "s" }); err != nil {
		t.Fatal(err)
	}
	on := Stamp(dir)
	if facts, _ := Load(dir); len(facts) != 3 {
		t.Fatalf("layer on: %d facts", len(facts))
	}
	t.Setenv("TACIT_LOCAL_LAYER", "")
	facts, _ := Load(dir)
	if len(facts) != 1 || facts[0].Meta.Name != "agents" {
		t.Fatalf("layer off: %v", facts)
	}
	if _, ok := Find(dir, "kept"); ok {
		t.Fatal("layer off: a curated fact is found")
	}
	if Stamp(dir) == on {
		t.Fatal("layer off: the stamp still covers the layer's files")
	}
}

// A store file of the local layer (a folder's record, its index) is never
// read as a local fact.
func TestLoadSkipsStoreFiles(t *testing.T) {
	t.Setenv("TACIT_LOCAL_LAYER", "on")
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "curated", "areas", "ops"), 0o700)
	os.WriteFile(filepath.Join(dir, "curated", "areas", "ops", "_record.md"), []byte("---\nkind: area\nname: ops\n---\nwhy\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "curated", "areas", "ops", "_overview.md"), []byte("x"), 0o600)
	facts, errs := Load(dir)
	if len(facts) != 0 || len(errs) != 0 {
		t.Fatalf("facts %v errs %v", facts, errs)
	}
}
