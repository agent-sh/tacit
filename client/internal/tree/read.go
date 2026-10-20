package tree

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// DefaultDepth is how many levels below the asked folder the tree tool
// returns when the caller does not say; MaxDepth is the most it accepts.
const (
	DefaultDepth = 3
	MaxDepth     = 8
	// maxOutput bounds one answer; a larger tree is cut with a note.
	maxOutput = 64 << 10
)

// ReadOptions is the session the tree tool answers for.
type ReadOptions struct {
	Store string // the clone
	// Local is the local layer's curated directory (overlay curated/), ""
	// when the layer is off.
	Local string
	// Session is what the tool may read (Session.Reads).
	Session
	// named is the scope the caller asked for by name (Named), set by Read.
	named string
}

var segRx = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Clean validates a folder path the tool was asked for: "" or "." is the
// root; otherwise store-relative, forward slashes, every part a slug (no
// "..", no dot directory, no absolute path), at most a trailing slash.
func Clean(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" || p == "." || p == "/" {
		return "", nil
	}
	if strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || filepath.IsAbs(p) {
		return "", fmt.Errorf("path %q must be relative to the store root, like system or projects/<repo>", p)
	}
	p = strings.TrimSuffix(p, "/")
	for _, seg := range strings.Split(p, "/") {
		if !segRx.MatchString(seg) {
			return "", fmt.Errorf("path %q: each part must be a folder name of the store (lower-case letters, digits and dashes)", p)
		}
	}
	return p, nil
}

// Eligible reports whether folder p is in the session's scope
// (Session.Reads), with the scope the caller named.
func (o ReadOptions) Eligible(p string) bool { return o.Reads(p, o.named) }

// known are the in-scope children of p whether or not p's index lists them
// (a long folder list is cut at MaxFolders).
func (o ReadOptions) known(p string) []string {
	switch p {
	case "":
		return []string{"system", "user", "projects", "machines", "derived"}
	case "projects", "derived":
		var out []string
		for r := range o.repoSet() {
			if segRx.MatchString(r) {
				out = append(out, p+"/"+r)
			}
		}
		return sortStrings(out)
	case "machines":
		if h := strings.ToLower(o.Host); segRx.MatchString(h) {
			return []string{"machines/" + h}
		}
	}
	return nil
}

func sortStrings(xs []string) []string {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j] < xs[j-1]; j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
	return xs
}

var folderLine = regexp.MustCompile(`^- \[([a-z0-9][a-z0-9-]*)/\]\(`)

// readIndex reads the index file of folder p under root, refusing any path
// that resolves outside root (a symlink on the way).
func readIndex(root, p string) (string, bool, error) {
	if root == "" {
		return "", false, nil
	}
	full := filepath.Join(root, filepath.FromSlash(Rel(p)))
	real, err := filepath.EvalSymlinks(full)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	rr, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", false, err
	}
	if rel, err := filepath.Rel(rr, real); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false, fmt.Errorf("%s resolves outside the store", Rel(p))
	}
	b, err := os.ReadFile(real)
	if err != nil {
		return "", false, err
	}
	return string(b), true, nil
}

// filter drops the folder lines of content for folders out of scope and
// returns the in-scope children it lists.
func (o ReadOptions) filter(p, content string) (string, []string) {
	var out []string
	var kids []string
	hidden := 0
	for _, l := range strings.Split(content, "\n") {
		if m := folderLine.FindStringSubmatch(l); m != nil {
			child := m[1]
			if p != "" {
				child = p + "/" + m[1]
			}
			if !o.Eligible(child) {
				hidden++
				continue
			}
			kids = append(kids, child)
		}
		out = append(out, l)
	}
	s := strings.TrimRight(strings.Join(out, "\n"), "\n") + "\n"
	if hidden > 0 {
		s += fmt.Sprintf("\n(%d %s outside this session's scope not shown: another repository's or machine's; name one as the path to read it.)\n", hidden, plural(hidden, "folder", "folders"))
	}
	return s, kids
}

// Read returns the index files of folder p and the folders below it down
// depth levels that are in the session's scope, store first and then the
// local layer's for each folder. p itself names a scope explicitly (Named):
// another repository's or machine's folder is read when asked for by path,
// and never listed otherwise. It reads index files only.
func Read(o ReadOptions, p string, depth int) (string, error) {
	p, err := Clean(p)
	if err != nil {
		return "", err
	}
	if depth < 0 || depth > MaxDepth {
		return "", fmt.Errorf("depth must be between 0 and %d", MaxDepth)
	}
	o.named = Named(p)
	if !o.Eligible(p) {
		return "", fmt.Errorf("%s is not a folder of the store's tree: system, user, projects/<repo>, machines/<host> and derived/<repo> are", p)
	}
	var b strings.Builder
	cut := false
	seen := map[string]bool{}
	found := false
	var visit func(p string, level int) error
	visit = func(p string, level int) error {
		if seen[p] || cut {
			return nil
		}
		seen[p] = true
		var kids []string
		for _, src := range []struct{ root, label string }{{o.Store, ""}, {o.Local, " (this machine's local layer)"}} {
			content, ok, err := readIndex(src.root, p)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			found = true
			text, k := o.filter(p, content)
			kids = append(kids, k...)
			name := p
			if name == "" {
				name = "store root"
			}
			block := fmt.Sprintf("=== %s%s: %s ===\n%s\n", name, src.label, Rel(p), text)
			if b.Len()+len(block) > maxOutput {
				cut = true
				fmt.Fprintf(&b, "... cut at %d KB: ask for a folder further down, or a smaller depth.\n", maxOutput>>10)
				return nil
			}
			b.WriteString(block)
		}
		if level >= depth {
			return nil
		}
		kids = append(kids, o.known(p)...)
		for _, k := range kids {
			if err := visit(k, level+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(p, 0); err != nil {
		return "", err
	}
	if !found {
		name := p
		if name == "" {
			name = "the store root"
		}
		return "", fmt.Errorf("no index file for %s: the folder holds no facts, or the indexes were never generated (`tacit index`)", name)
	}
	return b.String(), nil
}
