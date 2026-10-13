// Package importer brings existing harness memories into inbox/import@<host>/.
// It never writes outside inbox/.
package importer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/ownerwords"
)

// Result counts what one import did.
type Result struct {
	Written, Unchanged, Skipped int
	Errors                      []error
}

func (r Result) String() string {
	return fmt.Sprintf("written %d, unchanged %d, skipped %d, errors %d", r.Written, r.Unchanged, r.Skipped, len(r.Errors))
}

// Options shared by the importers.
type Options struct {
	Store string // store root
	Host  string // short hostname, used in inbox/import@<host>; must be a slug
	Home  string // for ~ shortening in imported_from and for scope mapping

	// DirExists lets tests drive the scope mapping without a filesystem.
	// nil means os.Stat.
	DirExists func(string) bool

	// OwnerWords says where the owner said a phrase (an owner_quote_from ref,
	// "" when nowhere). A memory the source system files as the owner's (a
	// Claude "feedback" memory, a Codex user preference) is imported as kind
	// user only when one of its quoted phrases is theirs; nil imports every
	// memory as observed.
	OwnerWords func(quote, session string) string
}

// ownerKind is the source kind of a memory its system files as the owner's:
// user with the evidence when a quoted phrase of it is their words, else
// observed (an agent's summary of what the owner wants is not their ruling).
func (o Options) ownerKind(src *fact.Source, name, desc, text string) {
	src.Kind = "observed"
	if o.OwnerWords == nil {
		return
	}
	for _, q := range ownerwords.Quotes(text) {
		if !ownerwords.ProvesRuling(q, desc, name) {
			continue // their words, perhaps, but too few to be the rule
		}
		if ref := o.OwnerWords(q, src.Session); ref != "" {
			src.Kind, src.OwnerQuote, src.OwnerQuoteFrom = "user", strings.Join(strings.Fields(q), " "), ref
			return
		}
	}
}

// Validate rejects options that could write outside inbox/.
func (o Options) Validate() error {
	if !fact.IsSlug(o.Host) {
		return fmt.Errorf("host %q is not a slug", o.Host)
	}
	if o.Store == "" {
		return errors.New("store root is empty")
	}
	return nil
}

// inbox returns <store>/inbox/import@<host>/<parts...> and refuses any path
// that resolves outside that directory.
func (o Options) inbox(parts ...string) (string, error) {
	base := filepath.Join(o.Store, "inbox", "import@"+o.Host)
	p := filepath.Join(append([]string{base}, parts...)...)
	rel, err := filepath.Rel(base, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("refusing to write outside %s: %s", base, p)
	}
	return p, nil
}

func (o Options) tilde(p string) string {
	if o.Home != "" && (p == o.Home || strings.HasPrefix(p, o.Home+"/")) {
		return "~" + strings.TrimPrefix(p, o.Home)
	}
	return p
}

func (o Options) dirExists(p string) bool {
	if o.DirExists != nil {
		return o.DirExists(p)
	}
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// writeFact writes f at dst unless an identical file exists. The temp file
// is created in the destination directory so the rename is atomic.
func writeFact(dst string, f *fact.Fact, r *Result) {
	b, err := f.Encode()
	if err != nil {
		r.Errors = append(r.Errors, fmt.Errorf("%s: %w", dst, err))
		return
	}
	if old, err := os.ReadFile(dst); err == nil && bytes.Equal(old, b) {
		r.Unchanged++
		return
	}
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		r.Errors = append(r.Errors, err)
		return
	}
	tmp, err := os.CreateTemp(dir, ".tacit-*.tmp")
	if err != nil {
		r.Errors = append(r.Errors, err)
		return
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(name)
		r.Errors = append(r.Errors, err)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		r.Errors = append(r.Errors, err)
		return
	}
	if err := os.Chmod(name, 0o644); err != nil {
		os.Remove(name)
		r.Errors = append(r.Errors, err)
		return
	}
	if err := os.Rename(name, dst); err != nil {
		os.Remove(name)
		r.Errors = append(r.Errors, err)
		return
	}
	r.Written++
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// Slug lowercases, replaces runs of other characters with `-`, trims, and
// caps length at max (cutting at a hyphen when possible).
func Slug(s string, max int) string {
	s = strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(s) > max {
		cut := strings.LastIndex(s[:max], "-")
		if cut < max/2 {
			cut = max
		}
		s = strings.Trim(s[:cut], "-")
	}
	if s == "" {
		return "fact"
	}
	return s
}

func shortHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])[:8]
}

func today() string { return time.Now().Format("2006-01-02") }

// scopeFromClaudeProject maps a Claude project directory name to a scope.
//
// Claude Code names the directory after the session cwd with `/` replaced by
// `-`, so `-home-u-projects-agent-sh-agnix` can be `~/projects/agent-sh/agnix`
// or `~/projects/agent-sh-agnix`. The name alone cannot say which, so the
// mapping walks the filesystem: at each step it takes the longest run of
// tokens that names an existing directory. The repo is the first directory
// under ~/projects that has a `.git`, else the deepest resolved directory.
// When nothing resolves (another machine, a deleted tree) the whole
// remainder joined by `-` is the repo and the curator decides.
//
// Directories outside ~/projects (home itself, caches, scratch) map to `user`.
func (o Options) scopeFromClaudeProject(dir string) string {
	tokens := strings.Split(strings.TrimPrefix(dir, "-"), "-")
	path := "/"
	i := 0
	for i < len(tokens) {
		best := 0
		// An empty token means a leading dot: `--cache` is `/.cache`.
		prefix := ""
		if tokens[i] == "" {
			prefix = "."
			i++
			if i >= len(tokens) {
				break
			}
		}
		for k := len(tokens) - i; k >= 1; k-- {
			cand := filepath.Join(path, prefix+strings.Join(tokens[i:i+k], "-"))
			if o.dirExists(cand) {
				best = k
				break
			}
		}
		if best == 0 {
			break
		}
		path = filepath.Join(path, prefix+strings.Join(tokens[i:i+best], "-"))
		i += best
	}
	rest := strings.Join(tokens[i:], "-")
	projects := filepath.Join(o.Home, "projects")
	if d := os.Getenv("TACIT_PROJECTS"); d != "" {
		projects = filepath.Clean(d)
	}
	switch {
	case path == projects:
		// cwd was ~/projects itself, or a repo under it that no longer exists.
		if rest == "" {
			return "user"
		}
		return "projects/" + Slug(rest, 60)
	case !strings.HasPrefix(path, projects+"/"):
		// Home, a dotdir, /tmp, or nothing resolved at all.
		return "user"
	}
	segs := strings.Split(strings.TrimPrefix(path, projects+"/"), "/")
	cur := projects
	for _, s := range segs {
		cur = filepath.Join(cur, s)
		if o.dirExists(filepath.Join(cur, ".git")) {
			return "projects/" + Slug(s, 60)
		}
	}
	leaf := segs[len(segs)-1]
	if rest != "" {
		// A deleted worktree under a container dir: keep what the name says.
		leaf += "-" + rest
	}
	return "projects/" + Slug(leaf, 60)
}
