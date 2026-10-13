package derive

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/store"
)

// File is one fact file: its store-relative path and its bytes.
type File struct {
	Rel  string
	Data []byte
}

func (f File) String() string { return fmt.Sprintf("%s (%d bytes)", f.Rel, len(f.Data)) }

// Source is where the derived files that already exist are read from. The
// question "has this changed" must be asked of what is committed, not of a
// working tree that an interrupted run may have left dirty, so the curator
// reads them from a git ref of the clone; `--out` reads them from its
// directory.
type Source interface {
	// Read returns the file at a store-relative path.
	Read(ctx context.Context, rel string) ([]byte, bool)
	// List returns the names directly under a store-relative directory,
	// sorted; nil when it does not exist.
	List(ctx context.Context, dir string) []string
}

// FS reads from a directory.
type FS struct{ Root string }

func (f FS) Read(_ context.Context, rel string) ([]byte, bool) {
	b, err := os.ReadFile(filepath.Join(f.Root, filepath.FromSlash(rel)))
	return b, err == nil
}

func (f FS) List(_ context.Context, dir string) []string {
	es, err := os.ReadDir(filepath.Join(f.Root, filepath.FromSlash(dir)))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	slices.Sort(out)
	return out
}

// GitRef reads from a commit of a clone: `git cat-file` and `git ls-tree`,
// so the working tree is never consulted.
type GitRef struct {
	Dir string // the clone
	Ref string // a commit-ish, e.g. origin/main
}

func (g GitRef) Read(ctx context.Context, rel string) ([]byte, bool) {
	out, err := store.Git(ctx, g.Dir, "cat-file", "blob", g.Ref+":"+rel)
	if err != nil {
		return nil, false
	}
	return []byte(out), true
}

func (g GitRef) List(ctx context.Context, dir string) []string {
	out, err := store.Git(ctx, g.Dir, "ls-tree", "--name-only", "-z", g.Ref, dir+"/")
	if err != nil {
		return nil
	}
	var names []string
	for _, p := range strings.Split(out, "\x00") {
		if p != "" {
			names = append(names, filepath.Base(p))
		}
	}
	slices.Sort(names)
	return names
}

// generated parses data as a generated fact: source.kind derived with a
// derived_from. Anything else (a hand-made file, a corrupt file) is not ours
// to overwrite or remove.
func generated(rel string, data []byte) (*fact.Fact, bool) {
	f, err := fact.Parse(rel, data)
	if err != nil || f.Meta.Source.Kind != "derived" || f.Meta.DerivedFrom == "" {
		return nil, false
	}
	return f, true
}

// IsGenerated reports whether data is a generated fact (source.kind derived
// with a derived_from), as opposed to a hand-made or corrupt file.
func IsGenerated(rel string, data []byte) bool {
	_, ok := generated(rel, data)
	return ok
}
