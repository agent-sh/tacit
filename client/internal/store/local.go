package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/agent-sh/tacit/client/internal/platform"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/index"
	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/tree"
)

const localKey = "tacit.local"

// Same token shapes as the server's pre-receive check (secret_re in
// server/priv/hooks/pre-receive; TestLocalSecretsAreTheServers holds the two
// equal). Agent writes still use scrump first; this is the second check when
// there is no server.
var errLocalPolicy = errors.New("local policy refused the write")

var localSecrets = regexp.MustCompile(`(ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|(^|[^A-Za-z0-9])sk-[A-Za-z0-9_-]*([A-Za-z0-9]{20}|[A-Z_][A-Za-z0-9_-]{15}|[A-Za-z0-9_-]{7}[A-Z_][A-Za-z0-9_-]{8}|[A-Za-z0-9_-]{15}[A-Z_]|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})|AKIA[0-9A-Z]{16}|xox[abprs]-[A-Za-z0-9-]{10,}|tskey-[A-Za-z0-9-]{10,}|(^|[^A-Za-z0-9])hf_[A-Za-z0-9]{20,}|-----BEGIN [A-Z ]*PRIVATE KEY-----`)

func (s *Store) checkLocalFiles(rels []string) error {
	for _, rel := range rels {
		if !filepath.IsLocal(filepath.FromSlash(rel)) {
			return fmt.Errorf("local commit requires store-relative paths")
		}
		p := filepath.Join(s.Dir, filepath.FromSlash(rel))
		// Never read or clean up a file through a symlinked parent directory.
		parent := s.Dir
		parts := strings.Split(filepath.FromSlash(rel), string(filepath.Separator))
		for _, part := range parts[:len(parts)-1] {
			parent = filepath.Join(parent, part)
			info, err := os.Lstat(parent)
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("local commit paths must not traverse symlinks")
			}
		}
		st, err := os.Lstat(p)
		if os.IsNotExist(err) {
			continue
		} // a retirement can remove a file
		if err != nil {
			return err
		}
		if !st.Mode().IsRegular() {
			return fmt.Errorf("%w: regular files are required", errLocalPolicy)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if localSecrets.Match(b) {
			return fmt.Errorf("%w: a file looks like it contains a secret", errLocalPolicy)
		}
	}
	return nil
}

// Local is explicit repository state, never inferred from a failed network
// lookup. A server clone cannot silently turn into a local store.
func (s *Store) Local(ctx context.Context) bool {
	out, err := git(ctx, s.Dir, "config", "--local", "--bool", "--get", localKey)
	return err == nil && strings.TrimSpace(out) == "true"
}

// Init creates a standalone store without any remote or enrollment keys.
// Existing directories with content are refused; rerunning on the same local
// store is harmless. An empty store starts with evidence-based ruling rules.
func (s *Store) Init(ctx context.Context) error {
	if s.Exists() {
		if s.Local(ctx) {
			if _, err := s.Head(ctx); err != nil {
				return fmt.Errorf("local store has no committed HEAD: %w", err)
			}
			return nil
		}
		return fmt.Errorf("%s already holds a git repository; not converting it to local mode", s.Dir)
	}
	if entries, err := os.ReadDir(s.Dir); err == nil && len(entries) != 0 {
		return fmt.Errorf("%s is not empty; nothing initialized", s.Dir)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	parent := filepath.Dir(s.Dir)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(parent, ".tacit-init-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	candidate := &Store{Dir: staging}
	if err := candidate.initEmpty(ctx); err != nil {
		return err
	}
	if err := os.Remove(s.Dir); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("initialization destination changed: %w", err)
	}
	if err := os.Rename(staging, s.Dir); err != nil {
		return fmt.Errorf("publish initialized store: %w", err)
	}
	return nil
}

func (s *Store) initEmpty(ctx context.Context) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	if _, err := git(ctx, s.Dir, "init", "--quiet", "-b", "main"); err != nil {
		return err
	}
	for _, setting := range [][2]string{{localKey, "true"}, {"user.name", "tacit"}, {"user.email", "local@tacit"}, {"core.autocrlf", "false"}} {
		if _, err := git(ctx, s.Dir, "config", "--local", setting[0], setting[1]); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Join(s.Dir, "review"), 0o700); err != nil {
		return err
	}
	marker, _ := json.Marshal(ledger.Marker{Version: 1, At: time.Now().UTC().Format(time.RFC3339)})
	if err := os.WriteFile(filepath.Join(s.Dir, filepath.FromSlash(ledger.MarkerPath)), append(marker, '\n'), 0o600); err != nil {
		return err
	}
	// A new store starts in this binary's format (ledger.Format): nothing
	// older ever read it, so its writers write folder records at once.
	format := ledger.Format{Version: ledger.FormatVersion, At: time.Now().UTC().Format(time.RFC3339)}
	if err := os.WriteFile(filepath.Join(s.Dir, filepath.FromSlash(ledger.FormatPath)), format.Encode(), 0o600); err != nil {
		return err
	}
	initial := []string{tree.FileName, ledger.MarkerPath, ledger.FormatPath, ".gitattributes"}
	if index.On() {
		if err := os.WriteFile(filepath.Join(s.Dir, index.File), []byte(index.Generate(nil)), 0o600); err != nil {
			return err
		}
		initial = append([]string{index.File}, initial...)
	}
	// The tree starts with its root index (internal/tree), so a fresh store
	// is checked for the folder indexes from its first fact on (lint L16).
	root := tree.Generate(nil, "")[tree.FileName]
	if err := os.WriteFile(filepath.Join(s.Dir, tree.FileName), []byte(root), 0o600); err != nil {
		return err
	}
	s.ensureAttributes(ctx)
	_, err := git(ctx, s.Dir, append([]string{"add", "--"}, initial...)...)
	if err != nil {
		return err
	}
	_, err = gitEnv(ctx, s.Dir, []string{"GIT_AUTHOR_NAME=tacit", "GIT_AUTHOR_EMAIL=local@tacit", "GIT_COMMITTER_NAME=tacit", "GIT_COMMITTER_EMAIL=local@tacit"}, "commit", "--quiet", "-m", "Initialize local fact store")
	return err
}

// Remove cached indexes of a dirty tree that may have been read before refusal.
func (s *Store) invalidateLocalIndexes(ctx context.Context) {
	head, err := s.Head(ctx)
	if err != nil {
		return
	}
	dir := os.Getenv("TACIT_INDEX")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(platform.StateDir(home), "index")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "index-v*-"+head+"*.db"))
	for _, p := range files {
		os.Remove(p)
	}
}
