package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/agent-sh/tacit/client/internal/platform"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/agent-sh/tacit/client/internal/digest"
)

// probe is the only way a verifier reaches git, GitHub, the file tree or
// this machine. Each verb is a fixed read-only program with validated
// arguments: no option the model names is passed through unless it is on
// that verb's short list, so neither a git alias, a textconv, a pager, a
// config override nor an output flag can turn a read into a write or code.
// Paths stay inside a short list of roots (the projects tree, the store,
// the agent rules, the memory directory, the user units), secret-named
// files are excluded, output is capped, and secret-shaped text is redacted
// before it reaches the model, because evidence ends up in the pushed store.
//
//	tacit probe git-log <repo> [-n N] [--grep P] [--author A] [--since D] [--until D] [--stat] [-p] [rev] [-- path...]
//	tacit probe git-show <repo> <rev> [--stat|--name-only] [-- path...]
//	tacit probe git-blame <repo> <path> [-L a,b]
//	tacit probe git-grep <repo> <pattern> [-i] [-w] [-l] [-- path...]
//	tacit probe git-ls <repo> [path...]
//	tacit probe git-tags <repo> [--contains rev]
//	tacit probe grep <dir> <pattern> [-i]      recursive, text files, 200 lines
//	tacit probe find <dir> <name-glob>          200 entries
//	tacit probe ls <path>...
//	tacit probe gh pr|issue view <number> [-R owner/repo]
//	tacit probe gh pr|issue list [-R owner/repo] [--state s] [--search q] [--limit N]
//	tacit probe gh repo view <owner/repo>
//	tacit probe gh release list [-R owner/repo]
//	tacit probe unit <name>          systemctl --user status
//	tacit probe units                systemctl --user list-unit-files
//	tacit probe sys                  uname, df, free, nvidia-smi
const (
	probeMax     = 64 << 10
	probeGHMax   = 8 << 10
	probeTimeout = 30 * time.Second
)

var (
	reRev    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/~^-]{0,200}$`)
	reRange  = regexp.MustCompile(`^\d+,\d+$`)
	reRepoNm = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9_][A-Za-z0-9._-]{0,99}$`)
	reUnit   = regexp.MustCompile(`^[A-Za-z0-9@_.-]+\.(service|timer|path|socket)$`)
	reNum    = regexp.MustCompile(`^\d{1,7}$`)
	reDate   = regexp.MustCompile(`^[A-Za-z0-9 :.-]{1,40}$`)
	reState  = regexp.MustCompile(`^(open|closed|merged|all)$`)
	reGlob   = regexp.MustCompile(`^[A-Za-z0-9*?._-]{1,80}$`)

	// secretGlobs are file names a probe never shows, as git pathspecs
	// and as grep/find exclusions. They mirror the verifier's Read deny list.
	secretGlobs = []string{"*.rejected", "*.env", ".env*", "*.pem", "*.key", "id_*", "*_ed25519", "*_rsa", "*_ecdsa", "*_dsa", "*.ppk", "*.p8", "*.p12", "*.pfx", "*.jks", "*.keystore", "*.gpg", "*.asc", "*.kdbx", "*.secret", "*secrets.y*ml", "*.tfstate", "*.tfstate.*", "*.tfvars", ".npmrc", ".pypirc", ".netrc", "hosts.yml", "credentials*", ".credentials*", ".vault-token", "*.jsonl", "*.sqlite", "*.db", "*.age", ".git-credentials", ".pgpass", ".bash_history", ".zsh_history"}
	// secretDirs are directory names never entered inside a root.
	secretDirs = []string{".git", ".ssh", ".aws", ".gnupg"}
	// noiseDirs are skipped by grep and find only.
	noiseDirs = []string{"node_modules", "target", ".venv", "__pycache__", ".cache"}
)

// probeRoots are the only directories a probe may look at. Everything else
// under home (shell history, harness credentials, transcripts, caches) is
// out of reach by construction rather than by a deny list.
// verifyRoots are extra read-only directories the verifier and its probe may
// read, such as a directory of agent rule files: $TACIT_VERIFY_ROOTS, a list
// separated like PATH. None by default.
func verifyRoots() []string {
	var out []string
	for _, d := range filepath.SplitList(os.Getenv("TACIT_VERIFY_ROOTS")) {
		if d = strings.TrimSpace(d); d != "" {
			out = append(out, filepath.Clean(d))
		}
	}
	return out
}

func probeRoots() ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return nil, errors.New("no home directory")
	}
	roots := append([]string{projectsRoot(home), filepath.Join(home, ".config", "systemd", "user"), memoryDir(home)}, verifyRoots()...)
	if st, err := storeDirForProbe(); err == nil {
		roots = append(roots, st)
	}
	var out []string
	for _, r := range roots {
		if abs, err := filepath.EvalSymlinks(r); err == nil {
			out = append(out, abs)
		}
	}
	return out, nil
}

func storeDirForProbe() (string, error) {
	if d := os.Getenv("TACIT_STORE"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "tacit", "store"), nil
}

func runProbe(args []string) int {
	out, err := probe(context.Background(), args)
	if out != "" {
		os.Stdout.WriteString(out)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "probe:", err)
		return 1
	}
	return 0
}

// buildCmds validates a probe and returns the programs to run. It runs
// nothing itself, so tests can cover every argument path.
func buildCmds(args []string) (cmds [][]string, limit int, err error) {
	if len(args) == 0 {
		return nil, 0, errors.New("verb missing: git-log git-show git-blame git-grep git-ls git-tags grep find ls gh unit units sys")
	}
	verb, rest := args[0], args[1:]
	limit = probeMax
	switch verb {
	case "git-log", "git-show", "git-blame", "git-grep", "git-ls", "git-tags":
		if len(rest) == 0 {
			return nil, 0, errors.New(verb + ": repo path missing")
		}
		repo, err := homeDir(rest[0], true)
		if err != nil {
			return nil, 0, err
		}
		gargs, err := gitArgs(verb, rest[1:])
		if err != nil {
			return nil, 0, err
		}
		cmds = [][]string{append(gitBase(repo), gargs...)}
	case "grep":
		if len(rest) < 2 || !noOption(rest[1]) {
			return nil, 0, errors.New("grep: <dir> <pattern> [-i]")
		}
		dir, err := homeDir(rest[0], true)
		if err != nil {
			return nil, 0, err
		}
		a := []string{"grep", "-rnIEZ", "--color=never", "-m", "5"}
		for _, d := range append(append([]string{}, secretDirs...), noiseDirs...) {
			a = append(a, "--exclude-dir="+d)
		}
		for _, g := range secretGlobs {
			a = append(a, "--exclude="+g)
		}
		for _, o := range rest[2:] {
			if o != "-i" && o != "-w" && o != "-l" {
				return nil, 0, fmt.Errorf("grep: option %q is not allowed", o)
			}
			a = append(a, o)
		}
		// dir/. so a root named like an excluded directory still searches.
		cmds = [][]string{append(a, "-e", rest[1], "--", dir+string(filepath.Separator)+".")}
	case "find":
		if len(rest) != 2 || !reGlob.MatchString(rest[1]) {
			return nil, 0, errors.New("find: <dir> <name-glob>")
		}
		dir, err := homeDir(rest[0], true)
		if err != nil {
			return nil, 0, err
		}
		a := []string{"find", dir + string(filepath.Separator) + ".", "-xdev", "-mindepth", "1"}
		for _, d := range append(append([]string{}, secretDirs...), noiseDirs...) {
			a = append(a, "-name", d, "-prune", "-o")
		}
		a = append(a, "-iname", rest[1])
		for _, g := range secretGlobs {
			a = append(a, "!", "-iname", g)
		}
		cmds = [][]string{append(a, "-print")}
	case "ls":
		if len(rest) == 0 {
			return nil, 0, errors.New("ls: path missing")
		}
		a := []string{"ls", "-la", "--"}
		for _, p := range rest {
			d, err := homeDir(p, false)
			if err != nil {
				return nil, 0, err
			}
			a = append(a, d)
		}
		cmds = [][]string{a}
	case "gh":
		gargs, err := ghArgs(rest)
		if err != nil {
			return nil, 0, err
		}
		cmds = [][]string{append([]string{"gh"}, gargs...)}
		limit = probeGHMax
	case "unit":
		if len(rest) != 1 || !reUnit.MatchString(rest[0]) {
			return nil, 0, errors.New("unit: one unit name")
		}
		// show, not status: no process tree with command lines, no journal.
		cmds = [][]string{{"systemctl", "--user", "show", "--no-pager", "-p", "Id,Description,LoadState,ActiveState,SubState,UnitFileState,Result,NRestarts,ExecMainStatus,ActiveEnterTimestamp,InactiveEnterTimestamp,NextElapseUSecRealtime,LastTriggerUSec,FragmentPath", "--", rest[0]}}
	case "units":
		if len(rest) != 0 {
			return nil, 0, errors.New("units takes no arguments")
		}
		cmds = [][]string{{"systemctl", "--user", "list-unit-files", "--no-pager", "--no-legend"}}
	case "sys":
		if len(rest) != 0 {
			return nil, 0, errors.New("sys takes no arguments")
		}
		cmds = [][]string{{"uname", "-a"}, {"df", "-h"}, {"free", "-g"}, {"nvidia-smi"}}
	default:
		return nil, 0, fmt.Errorf("unknown verb %q", verb)
	}
	return cmds, limit, nil
}

// gitBase is git with every repo-configured program switched off: textconv,
// fsmonitor, pager, external diff, hooks.
func gitBase(repo string) []string {
	return []string{"git", "--no-pager", "-C", repo, "-c", "core.fsmonitor=false", "-c", "core.pager=cat", "-c", "diff.external=cat", "-c", "core.hooksPath=/dev/null", "-c", "diff.noprefix=false"}
}

func probe(ctx context.Context, args []string) (string, error) {
	cmds, limit, err := buildCmds(args)
	if err != nil {
		return "", err
	}
	var env []string
	for _, kv := range os.Environ() {
		if (strings.HasPrefix(kv, "GIT_") || strings.HasPrefix(kv, "GH_")) && !strings.HasPrefix(kv, "GH_TOKEN=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "GIT_PAGER=cat", "PAGER=cat", "GIT_TERMINAL_PROMPT=0", "GH_PAGER=cat", "GH_PROMPT_DISABLED=1", "NO_COLOR=1", "LC_ALL=C.UTF-8")
	roots, err := probeRoots()
	if err != nil {
		return "", err
	}
	var ceilings []string
	for _, r := range roots {
		ceilings = append(ceilings, filepath.Dir(r))
	}
	// Repository discovery must not climb above the roots.
	env = append(env, "GIT_CEILING_DIRECTORIES="+strings.Join(ceilings, ":"))
	if strings.HasPrefix(args[0], "git-") {
		// A .git file or link may point at a repository outside the roots.
		if err := gitDirInside(ctx, cmds[0], env, roots); err != nil {
			return "", err
		}
		if args[0] == "git-blame" {
			// Blame has no -I: refuse a binary file before printing its bytes.
			if err := blameIsText(ctx, cmds[0], env); err != nil {
				return "", err
			}
		}
	}
	var buf bytes.Buffer
	for _, c := range cmds {
		cctx, cancel := context.WithTimeout(ctx, probeTimeout)
		cmd := exec.CommandContext(cctx, c[0], c[1:]...)
		cmd.Env = env
		cmd.Stdin = nil
		platform.Prepare(cmd)
		cmd.WaitDelay = 2 * time.Second
		w := &cappedWriter{w: &buf, left: 2 * limit, stop: cancel}
		cmd.Stdout, cmd.Stderr = w, w
		err := cmd.Run()
		cancel()
		if w.cut {
			buf.WriteString("\n[probe: output cut, narrow the query]\n")
			err = nil
		}
		// grep and git grep exit 1 when nothing matched; that is an answer.
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 && (c[0] == "grep" || (c[0] == "git" && contains(c, "grep"))) {
			buf.WriteString("(no matches)\n")
			err = nil
		}
		if err != nil && len(cmds) == 1 {
			out, rerr := filterOut(buf.String(), args, limit)
			if rerr != nil {
				return "", rerr
			}
			return out, fmt.Errorf("%s: %v", c[0], err)
		}
	}
	return filterOut(buf.String(), args, limit)
}

// filterOut applies the secret-name line filter and the redacting clip to
// probe output on every return path, error or not.
func filterOut(out string, args []string, limit int) (string, error) {
	if args[0] == "grep" || args[0] == "find" {
		out = dropSecretLines(out, args[0] == "grep", contains(args, "-l"))
	}
	return clipOut(out, limit)
}

// gitDirInside refuses a repository whose git directory lives outside the
// probe roots. cmd is the git command about to run; its -C value is the repo.
func gitDirInside(ctx context.Context, cmd []string, env, roots []string) error {
	repo := ""
	for i, a := range cmd {
		if a == "-C" && i+1 < len(cmd) {
			repo = cmd[i+1]
		}
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c := exec.CommandContext(cctx, "git", append(gitBase(repo)[1:], "rev-parse", "--absolute-git-dir", "--git-common-dir")...)
	c.Env = env
	out, err := c.Output()
	if err != nil {
		return fmt.Errorf("%s is not a git repository a probe may read", repo)
	}
	for _, d := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if !filepath.IsAbs(d) {
			d = filepath.Join(repo, d)
		}
		d, err = filepath.EvalSymlinks(d)
		if err != nil {
			return fmt.Errorf("%s: %w", repo, err)
		}
		inside := false
		for _, r := range roots {
			if strings.HasPrefix(d, r+string(filepath.Separator)) {
				inside = true
			}
		}
		if !inside {
			return fmt.Errorf("%s keeps its git directory outside the directories a probe may read", repo)
		}
	}
	return nil
}

// blameIsText reads the blob blame will print (HEAD:path, capped) and
// requires valid UTF-8 text with no NUL byte.
func blameIsText(ctx context.Context, cmd []string, env []string) error {
	repo, path := "", cmd[len(cmd)-1]
	for i, a := range cmd {
		if a == "-C" && i+1 < len(cmd) {
			repo = cmd[i+1]
		}
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// ./ resolves the path from the -C directory, as blame does.
	c := exec.CommandContext(cctx, "git", append(gitBase(repo)[1:], "cat-file", "blob", "HEAD:./"+path)...)
	c.Env = append(env, "GIT_LITERAL_PATHSPECS=1")
	var buf bytes.Buffer
	w := &cappedWriter{w: &buf, left: 4 << 20, stop: cancel}
	c.Stdout = w
	if err := c.Run(); err != nil && !w.cut {
		return fmt.Errorf("%s is not a tracked file", path)
	}
	b := buf.Bytes()
	if w.cut || buf.Len() >= 4<<20 {
		return fmt.Errorf("%s is too large to blame", path)
	}
	if bytes.IndexByte(b, 0) >= 0 || !utf8.Valid(b) {
		return fmt.Errorf("%s is not a text file", path)
	}
	return nil
}

// cappedWriter stops the program once it has written more than anyone will
// read, instead of buffering a whole repository in memory.
type cappedWriter struct {
	w    io.Writer
	left int
	cut  bool
	stop func()
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	if c.left <= 0 {
		if !c.cut {
			c.cut = true
			c.stop()
		}
		return len(p), nil
	}
	n := len(p)
	if n > c.left {
		n = c.left
	}
	c.w.Write(p[:n])
	c.left -= n
	return len(p), nil
}

func clipOut(s string, limit int) (string, error) {
	s, err := digest.RedactErr(s)
	if err != nil {
		return "", fmt.Errorf("probe output withheld, redaction failed: %w", err)
	}
	if len(s) > limit {
		s = s[:limit] + "\n[probe: output cut at " + strconv.Itoa(limit>>10) + " KiB]\n"
	}
	return s, nil
}

func noOption(s string) bool { return s != "" && !strings.HasPrefix(s, "-") }

// homeDir accepts an existing path inside one of the probe roots, resolved
// through symlinks, outside the secret directories. mustDir requires a
// directory.
func homeDir(p string, mustDir bool) (string, error) {
	roots, err := probeRoots()
	if err != nil {
		return "", err
	}
	if !noOption(p) {
		return "", fmt.Errorf("%q is not a path", p)
	}
	if home, _ := os.UserHomeDir(); strings.HasPrefix(p, "~/") {
		p = filepath.Join(home, p[2:])
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("%s: %w", p, err)
	}
	root := ""
	for _, r := range roots {
		if abs == r || strings.HasPrefix(abs, r+string(filepath.Separator)) {
			root = r
			break
		}
	}
	if root == "" {
		return "", fmt.Errorf("%s is outside the directories a probe may read (%s)", p, strings.Join(roots, ", "))
	}
	rel, _ := filepath.Rel(root, abs)
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		for _, d := range secretDirs {
			if part == d {
				return "", fmt.Errorf("%s is inside a %s directory", p, d)
			}
		}
	}
	st, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("%s: %w", p, err)
	}
	if mustDir && !st.IsDir() {
		return "", fmt.Errorf("%s is not a directory", p)
	}
	if secretName(filepath.Base(abs)) {
		return "", fmt.Errorf("%s is a secret-named file", p)
	}
	return abs, nil
}

func secretName(base string) bool {
	base = strings.ToLower(base)
	for _, g := range secretGlobs {
		if ok, _ := filepath.Match(g, base); ok {
			return true
		}
	}
	return false
}

// secretPath says whether any component of a path names a secret file or
// directory, case-insensitively.
func secretPath(p string) bool {
	for _, part := range strings.Split(filepath.ToSlash(p), "/") {
		if secretName(part) {
			return true
		}
		for _, d := range secretDirs {
			if strings.EqualFold(part, d) {
				return true
			}
		}
	}
	return false
}

// dropSecretLines removes grep and find output lines whose path names a
// secret file: those tools match names case-sensitively, this does not.
// grep runs with -Z, so the path ends at a NUL; find lines are whole paths.
func dropSecretLines(out string, grep, names bool) string {
	// grep -Z ends every file name with NUL; with -l that is the whole
	// record (no newline), so records are split on NUL first.
	if grep && names {
		out = strings.ReplaceAll(out, "\x00", "\x00\n")
	}
	lines := strings.Split(out, "\n")
	kept := lines[:0]
	dropped := 0
	for _, l := range lines {
		path := l
		if grep {
			if i := strings.IndexByte(l, 0); i >= 0 {
				path = l[:i]
				l = l[:i] + strings.TrimPrefix(l[i+1:], ":")
				if l[len(path):] != "" {
					l = path + ":" + l[len(path):]
				}
			}
		}
		path = strings.ReplaceAll(path, "/./", "/")
		if secretPath(path) {
			dropped++
			continue
		}
		kept = append(kept, strings.Replace(l, "/./", "/", 1))
	}
	if dropped > 0 {
		kept = append(kept, fmt.Sprintf("[probe: %d line(s) from secret-named files withheld]", dropped))
	}
	return strings.Join(kept, "\n")
}

// paths validates everything after "--": plain relative paths, no options,
// no secret or .git component anywhere in them.
func paths(rest []string) ([]string, error) {
	for _, p := range rest {
		if !noOption(p) || filepath.IsAbs(p) || strings.HasPrefix(p, ":") {
			return nil, fmt.Errorf("%q is not a relative path", p)
		}
		for _, part := range strings.Split(filepath.ToSlash(p), "/") {
			if part == ".." || secretName(part) {
				return nil, fmt.Errorf("%s names a secret or leaves the repo", p)
			}
			for _, d := range secretDirs {
				if part == d {
					return nil, fmt.Errorf("%s is inside a %s directory", p, d)
				}
			}
		}
	}
	return rest, nil
}

// excludes are the pathspecs that keep secret-named files out of git
// output. They go after every positive pathspec, the model's included.
func excludes() []string {
	var out []string
	for _, g := range secretGlobs {
		out = append(out, ":(exclude,glob,icase)**/"+g)
	}
	for _, d := range secretDirs {
		out = append(out, ":(exclude,glob,icase)**/"+d+"/**")
	}
	return out
}

func splitDashDash(rest []string) (opts, ps []string) {
	for i, a := range rest {
		if a == "--" {
			return rest[:i], rest[i+1:]
		}
	}
	return rest, nil
}

func gitArgs(verb string, rest []string) ([]string, error) {
	opts, ps := splitDashDash(rest)
	ps, err := paths(ps)
	if err != nil {
		return nil, err
	}
	withPaths := func(a []string) []string {
		a = append(a, "--")
		if len(ps) > 0 {
			a = append(a, ps...)
		} else {
			a = append(a, ".")
		}
		return append(a, excludes()...)
	}
	// A revision must be a commit: a blob or tree sha would print a file
	// or a listing the pathspec excludes never see.
	commit := func(rev string) string { return rev + "^{commit}" }
	switch verb {
	case "git-log":
		a := []string{"log", "--no-color", "--no-textconv", "--date=short", "-n", "50", "--format=%h %ad %an %s"}
		for i := 0; i < len(opts); i++ {
			switch opts[i] {
			case "-n":
				if i+1 >= len(opts) || !reNum.MatchString(opts[i+1]) {
					return nil, errors.New("git-log: -n needs a number")
				}
				n, _ := strconv.Atoi(opts[i+1])
				if n > 500 {
					n = 500
				}
				a[5] = strconv.Itoa(n)
				i++
			case "--grep", "--author", "--since", "--until":
				if i+1 >= len(opts) || !noOption(opts[i+1]) || ((opts[i] == "--since" || opts[i] == "--until") && !reDate.MatchString(opts[i+1])) {
					return nil, fmt.Errorf("git-log: %s needs a value", opts[i])
				}
				a = append(a, opts[i]+"="+opts[i+1])
				i++
			case "--stat", "--all", "-p":
				a = append(a, opts[i])
			default:
				if reRev.MatchString(opts[i]) {
					a = append(a, commit(opts[i]))
					continue
				}
				return nil, fmt.Errorf("git-log: option %q is not allowed", opts[i])
			}
		}
		return withPaths(a), nil
	case "git-show":
		if len(opts) == 0 || !reRev.MatchString(opts[0]) {
			return nil, errors.New("git-show: a revision is required")
		}
		a := []string{"show", "--no-color", "--no-textconv", "--format=%H%n%ad %an%n%s%n%n%b", "--date=short", commit(opts[0])}
		for _, o := range opts[1:] {
			if o != "--stat" && o != "--name-only" {
				return nil, fmt.Errorf("git-show: option %q is not allowed", o)
			}
			a = append(a, o)
		}
		return withPaths(a), nil
	case "git-blame":
		if len(opts) == 0 {
			return nil, errors.New("git-blame: a relative path is required")
		}
		if _, err := paths(opts[:1]); err != nil {
			return nil, err
		}
		a := []string{"blame", "--date=short", "--no-textconv", "HEAD"}
		for i := 1; i < len(opts); i++ {
			if opts[i] == "-L" && i+1 < len(opts) && reRange.MatchString(opts[i+1]) {
				a = append(a, "-L", opts[i+1])
				i++
				continue
			}
			return nil, fmt.Errorf("git-blame: option %q is not allowed", opts[i])
		}
		return append(a, "--", opts[0]), nil
	case "git-grep":
		if len(opts) == 0 || !noOption(opts[0]) {
			return nil, errors.New("git-grep: a pattern is required")
		}
		// --cached: the index, never the working tree, where a directory
		// symlink could lead outside the roots.
		a := []string{"grep", "--cached", "--no-color", "--no-textconv", "-n", "-I", "-E", "--max-count", "20", "-e", opts[0]}
		for _, o := range opts[1:] {
			if o != "-i" && o != "-w" && o != "-l" {
				return nil, fmt.Errorf("git-grep: option %q is not allowed", o)
			}
			a = append(a, o)
		}
		return withPaths(a), nil
	case "git-ls":
		ps2, err := paths(opts)
		if err != nil {
			return nil, err
		}
		a := []string{"ls-files", "--"}
		if len(ps2) > 0 {
			a = append(a, ps2...)
		} else {
			a = append(a, ".")
		}
		return append(a, excludes()...), nil
	case "git-tags":
		a := []string{"tag", "--list", "--sort=-creatordate"}
		if len(opts) == 2 && opts[0] == "--contains" && reRev.MatchString(opts[1]) {
			a = append(a, "--contains", commit(opts[1]))
		} else if len(opts) != 0 {
			return nil, errors.New("git-tags: only --contains <rev> is allowed")
		}
		return a, nil
	}
	return nil, fmt.Errorf("unknown git verb %s", verb)
}

// ghArgs allows views and lists with fixed JSON fields, so a PR or issue
// body written by a stranger never reaches the model as free text.
func ghArgs(rest []string) ([]string, error) {
	if len(rest) < 2 {
		return nil, errors.New("gh: pr|issue|repo|release plus view|list")
	}
	kind, sub := rest[0], rest[1]
	fields := map[string]string{
		"pr view":      "number,title,state,isDraft,mergedAt,closedAt,createdAt,author,baseRefName,headRefName,url",
		"pr list":      "number,title,state,createdAt,author,url",
		"issue view":   "number,title,state,closedAt,createdAt,author,url",
		"issue list":   "number,title,state,createdAt,author,url",
		"repo view":    "name,owner,visibility,isArchived,pushedAt,defaultBranchRef,url",
		"release list": "",
	}
	f, ok := fields[kind+" "+sub]
	if !ok {
		return nil, fmt.Errorf("gh: %s %s is not allowed", kind, sub)
	}
	a := []string{kind, sub}
	opts := rest[2:]
	if sub == "view" {
		if kind == "repo" {
			if len(opts) == 0 || !reRepoNm.MatchString(opts[0]) {
				return nil, errors.New("gh repo view: owner/repo required")
			}
		} else if len(opts) == 0 || !reNum.MatchString(opts[0]) {
			return nil, fmt.Errorf("gh %s view: a number is required", kind)
		}
		a = append(a, opts[0])
		opts = opts[1:]
	}
	limit := "30"
	for i := 0; i < len(opts); i++ {
		switch opts[i] {
		case "-R", "--repo":
			if i+1 >= len(opts) || !reRepoNm.MatchString(opts[i+1]) {
				return nil, errors.New("gh: -R needs owner/repo")
			}
			a = append(a, "-R", opts[i+1])
			i++
		case "--state":
			if sub != "list" || i+1 >= len(opts) || !reState.MatchString(opts[i+1]) {
				return nil, errors.New("gh: --state open|closed|merged|all on list")
			}
			a = append(a, "--state", opts[i+1])
			i++
		case "--limit", "-L":
			if sub != "list" || i+1 >= len(opts) || !reNum.MatchString(opts[i+1]) {
				return nil, errors.New("gh: --limit needs a number")
			}
			if n, _ := strconv.Atoi(opts[i+1]); n > 200 {
				limit = "200"
			} else {
				limit = opts[i+1]
			}
			i++
		case "--search", "-S":
			if sub != "list" || i+1 >= len(opts) || !noOption(opts[i+1]) {
				return nil, errors.New("gh: --search needs a query")
			}
			a = append(a, "--search", opts[i+1])
			i++
		default:
			return nil, fmt.Errorf("gh: option %q is not allowed", opts[i])
		}
	}
	if sub == "list" {
		a = append(a, "--limit", limit)
	}
	if f != "" {
		a = append(a, "--json", f)
	}
	return a, nil
}
