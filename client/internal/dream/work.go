package dream

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/agent-sh/tacit/client/internal/repoid"
)

// A checkout outside home or in a temp directory (repoid.Elsewhere) names a
// window's repository only once sessions show real work in it: file edits
// or git commits there, from repoid.MinWorkSessions distinct sessions. The
// evidence is read from the window's tool lines (digest's "T: <tool> <key>"),
// which every harness writes the same way: the tool's name and its file
// path, command or patch. Only tools whose effect is certain from the name
// count, per harness, and a path counts only inside the checkout.

// editTools write the file their key names: Claude Code's Edit, Write,
// MultiEdit and NotebookEdit (an absolute file_path), pi's edit and write,
// Hermes's write_file and patch (a path, absolute or relative to the
// session's cwd).
var editTools = map[string]bool{"Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true, "edit": true, "write": true, "write_file": true, "patch": true}

// shellTools run their key as a shell command in the session's cwd:
// Claude Code's Bash, Codex's shell tools, pi's bash, Hermes's terminal.
var shellTools = map[string]bool{"Bash": true, "shell": true, "exec": true, "exec_command": true, "shell_command": true, "local_shell": true, "bash": true, "terminal": true}

var (
	// patchFileRx is a file of an apply_patch patch (Codex), whose newlines
	// the digest has folded into spaces.
	patchFileRx = regexp.MustCompile(`\*\*\* (?:Add|Update|Delete) File: (\S+)`)
	// commitRx is a git commit in a command, with git's own options before
	// the subcommand (-c k=v, -C dir).
	commitRx = regexp.MustCompile(`(?:^|[;&|(]\s*)git((?:\s+-[cC]\s+\S+)*)\s+commit\b`)
	cdRx     = regexp.MustCompile(`(?:^|[;&|(]\s*)(?:cd|pushd)\s`)
	gitCRx   = regexp.MustCompile(`-C\s+(\S+)`)
)

// WorkIn is the evidence of real work in the checkout worktree that a
// window's lines hold, from a session whose cwd is inside it: "edit" for an
// edit tool's file (or an apply_patch file) inside it, "commit" for a git
// commit run there (in the cwd, with no cd before it, or with git -C a
// directory inside it), "" for none. A relative path is the cwd's, a ~
// path home's.
func WorkIn(lines []string, worktree, cwd, home string) string {
	if worktree == "" {
		return ""
	}
	ids := repoid.NewCache(home)
	inside := func(p string) bool {
		p = strings.Trim(p, `"'`)
		if p == "" {
			return false
		}
		if strings.HasPrefix(p, "~") {
			if p = ids.Expand(p); p == "" {
				return false
			}
		}
		if !filepath.IsAbs(p) {
			if cwd == "" {
				return false
			}
			p = filepath.Join(cwd, p)
		}
		rel, err := filepath.Rel(worktree, repoid.RealPath(filepath.Clean(p)))
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	commit := false
	for _, l := range lines {
		rest, ok := strings.CutPrefix(l, "T: ")
		if !ok {
			continue
		}
		tool, key, _ := strings.Cut(rest, " ")
		key = strings.TrimSpace(key)
		switch {
		case editTools[tool]:
			if inside(key) {
				return "edit"
			}
		case tool == "apply_patch" || shellTools[tool] && strings.HasPrefix(key, "apply_patch"):
			for _, m := range patchFileRx.FindAllStringSubmatch(key, -1) {
				if inside(m[1]) {
					return "edit"
				}
			}
		case shellTools[tool]:
			m := commitRx.FindStringSubmatch(key)
			if m == nil {
				continue
			}
			if c := gitCRx.FindStringSubmatch(m[1]); c != nil {
				commit = commit || inside(c[1])
			} else if !cdRx.MatchString(key) && cwd != "" && inside(cwd) {
				commit = true
			}
		}
	}
	if commit {
		return "commit"
	}
	return ""
}
