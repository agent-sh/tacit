// Package overlay is the machine-local fact overlay of a read-only clone: the
// notes the harnesses on one machine (Claude Code, Codex, ...) share with each
// other when the machine may not push (SPEC.md, "Local overlay").
//
// Files have the store's fact format and live under
// <Dir>/<harness>@<host>/<name>.md. The overlay is a plain directory outside
// the clone, never a git repository, so no commit or push can carry it: the
// store's push paths only ever stage paths inside the clone.
package overlay

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/agent-sh/tacit/client/internal/fact"
)

// Prefix is the path every overlay fact carries in the index and in search
// hits ("local/<harness>@<host>/<name>.md"): not inbox/, not a store scope
// directory, so nothing that reads store paths takes it for a store file.
const Prefix = "local/"

// Dir is where the overlay lives: $TACIT_LOCAL, else ~/.local/share/tacit/local.
func Dir() string {
	if d := os.Getenv("TACIT_LOCAL"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "tacit", "local")
}

// Outside reports whether dir lies outside storeDir (and is not storeDir).
// The overlay must never be inside a clone, where a git add could reach it.
// Both are compared with symlinks resolved, also for a dir that does not
// exist yet (through its nearest existing ancestor), so a TACIT_LOCAL that
// runs through a link into the clone is caught before it is created.
func Outside(dir, storeDir string) bool {
	a, err1 := resolve(dir)
	b, err2 := resolve(storeDir)
	if err1 != nil || err2 != nil {
		return false
	}
	rel, err := filepath.Rel(b, a)
	if err != nil {
		return false
	}
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolve makes p absolute with every symlink resolved: the longest
// existing prefix through EvalSymlinks, the missing rest appended as is. A
// dangling symlink on the way (x -> <store>/newdir, newdir not there yet) is
// followed by its target, not taken as a missing name, so it cannot hide
// where the path will land once the target is created.
func resolve(p string) (string, error) {
	return resolveDepth(p, 0)
}

func resolveDepth(p string, depth int) (string, error) {
	if depth > 40 {
		return "", errors.New("too many levels of symbolic links: " + p)
	}
	p, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	var rest []string
	join := func(r string) string {
		for i := len(rest) - 1; i >= 0; i-- {
			r = filepath.Join(r, rest[i])
		}
		return r
	}
	for cur := p; ; {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return join(r), nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		if fi, err := os.Lstat(cur); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(cur)
			if err != nil {
				return "", err
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(cur), target)
			}
			return resolveDepth(join(target), depth+1)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p, nil
		}
		rest = append(rest, filepath.Base(cur))
		cur = parent
	}
}

// Load reads every overlay fact. Path is Prefix + the file's path in the
// overlay. A file that does not parse is skipped and returned in errs.
func Load(dir string) (facts []*fact.Fact, errs []error) {
	if dir == "" {
		return nil, nil
	}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != dir && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			errs = append(errs, rerr)
			return nil
		}
		f, perr := fact.Parse(Prefix+filepath.ToSlash(rel), data)
		if perr != nil {
			errs = append(errs, fmt.Errorf("%s: %w", rel, perr))
			return nil
		}
		facts = append(facts, f)
		return nil
	})
	sort.Slice(facts, func(i, j int) bool { return facts[i].Path < facts[j].Path })
	return facts, errs
}

// Stamp is a short hash of the overlay's files (names, sizes, mtimes), or ""
// when it holds none. The search index is keyed by it next to the store's
// HEAD, so a new note is searchable at once and an unchanged overlay costs
// one directory walk.
func Stamp(dir string) string {
	if dir == "" {
		return ""
	}
	h := sha256.New()
	n := 0
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".md") || d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		fmt.Fprintf(h, "%s\x00%d\x00%d\n", rel, info.Size(), info.ModTime().UnixNano())
		n++
		return nil
	})
	if n == 0 {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// Find returns the overlay file of the fact called name.
func Find(dir, name string) (string, bool) {
	if dir == "" || !fact.IsSlug(name) {
		return "", false
	}
	var found string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == name+".md" && d.Type()&os.ModeSymlink == 0 {
			found = p
			return filepath.SkipAll
		}
		return nil
	})
	return found, found != ""
}

// ErrInside is an overlay directory inside the clone: refused, since a
// commit there could carry it to the server.
var ErrInside = errors.New("the local overlay directory is inside the store clone; set TACIT_LOCAL to a directory outside it")

// Add writes f (Meta and Body ready, already redacted) as
// <dir>/<writer>/<name>.md and returns its Prefix path. A fact whose
// description and body any overlay file already holds is not written again
// (whichever harness wrote it), and that file's path is returned. A name
// taken by a different fact gets a short suffix, so nothing is overwritten.
func Add(dir, storeDir, writer string, f *fact.Fact, suffix func() string) (string, error) {
	if dir == "" {
		return "", errors.New("no home directory for the local overlay")
	}
	if !Outside(dir, storeDir) {
		return "", ErrInside
	}
	existing, _ := Load(dir)
	for _, e := range existing {
		if e.Meta.Description == f.Meta.Description && strings.TrimSpace(e.Body) == strings.TrimSpace(f.Body) {
			return e.Path, nil
		}
	}
	rel := filepath.Join(writer, f.Meta.Name+".md")
	if _, err := os.Stat(filepath.Join(dir, rel)); err == nil {
		f.Meta.Name = f.Meta.Name + "-" + suffix()
		rel = filepath.Join(writer, f.Meta.Name+".md")
	}
	f.Path = Prefix + filepath.ToSlash(rel)
	b, err := f.Encode()
	if err != nil {
		return "", err
	}
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return "", err
	}
	tmp := full + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, full); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return f.Path, nil
}
