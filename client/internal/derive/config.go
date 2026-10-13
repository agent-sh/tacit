package derive

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/agent-sh/tacit/client/internal/fact"
	"go.yaml.in/yaml/v3"
)

// Config is what the curator reads from ~/.config/tacit/derive.yaml (or
// $TACIT_DERIVE_CONFIG): which branch to read a repo at when the remote
// default branch is not where the work is, and which of this machine's repo
// names are also plain words. Absent file, empty config.
//
//	branches:
//	  myrepo: dev
//	ordinary_names: [compass, notebook]
//
// The case that made branches: a fork whose `main` mirrors upstream while
// `dev` is the tree in use (main plus the carried patches), so the facts that
// help an agent working there are dev's. ordinary_names keeps a bare
// "compass" in a fact from tying it to the compass repo (the git evidence still
// counts a path into it or the name in backticks).
type Config struct {
	Branches      map[string]string `yaml:"branches,omitempty"`
	OrdinaryNames []string          `yaml:"ordinary_names,omitempty"`
}

// Ordinary is OrdinaryNames as a set.
func (c Config) Ordinary() map[string]bool {
	m := map[string]bool{}
	for _, n := range c.OrdinaryNames {
		m[n] = true
	}
	return m
}

// ConfigPath is $TACIT_DERIVE_CONFIG, else ~/.config/tacit/derive.yaml.
func ConfigPath() string {
	if p := os.Getenv("TACIT_DERIVE_CONFIG"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "tacit", "derive.yaml")
}

var branchRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

// ValidBranch reports whether name can be a configured branch: no leading
// dash (it ends up as a git argument and in a refspec), no `..`, no trailing
// slash or `.lock`.
func ValidBranch(name string) bool {
	return branchRe.MatchString(name) && !strings.Contains(name, "..") && !strings.HasSuffix(name, "/") && !strings.HasSuffix(name, ".lock")
}

func (c Config) validate() error {
	for repo, b := range c.Branches {
		if repo == "" || strings.Contains(repo, "/") {
			return fmt.Errorf("branches: %q is not a repo name", repo)
		}
		if !ShowableBranch(b) {
			return fmt.Errorf("branches: %s: %q is not a usable branch name", repo, b)
		}
	}
	for _, n := range c.OrdinaryNames {
		if n == "" || strings.ContainsAny(n, "/ \t") {
			return fmt.Errorf("ordinary_names: %q is not a repo name", n)
		}
	}
	return nil
}

// LoadConfig reads the file strictly: an unknown key or a bad branch name is
// an error, because a typo that silently derives the wrong branch is worse
// than a run that says so.
func LoadConfig(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := fact.Decode(b, &c); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.validate(); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// SetBranch records (or, with an empty branch, removes) the branch for a
// repo and rewrites the file. This is how a machine is set up: the curator
// does not guess it and the code does not carry it.
func SetBranch(path, repo, branch string) error {
	c, err := LoadConfig(path)
	if err != nil {
		return err
	}
	if c.Branches == nil {
		c.Branches = map[string]string{}
	}
	if branch == "" {
		delete(c.Branches, repo)
	} else {
		c.Branches[repo] = branch
	}
	if err := c.validate(); err != nil {
		return err
	}
	out, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	if len(c.Branches) == 0 && len(c.OrdinaryNames) == 0 {
		out = nil
	}
	head := "# tacit derive: the branch to read a repo at, when the remote default branch is not where the work is.\n# Set with `tacit derive --set-branch <repo>=<branch>`. ordinary_names: repo names that are also plain words.\n"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append([]byte(head), out...), 0o644)
}
