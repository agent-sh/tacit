package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/repoid"
)

// hookRepoAliases are the names delivery knows the repository of cwd by
// (repoid: a real git root, a worktree's main repository), or none when cwd
// is in no repository: a folder named like a project is not evidence of it.
func hookRepoAliases(cwd string) []string {
	r, ok := sessionRepo(cwd)
	if !ok {
		return nil
	}
	return repoAliases(r)
}

// sessionRepo is the repository cwd is in, under this user's home (a
// separate clone named for its main checkout, repoid.OfMachine).
func sessionRepo(cwd string) (repoid.Repo, bool) {
	home, _ := os.UserHomeDir()
	return repoid.OfMachine(cwd, home)
}

// repoAliases are r's name and the other names repoid knows it by (the
// lexical name of a symlinked checkout the session ran through, origin's
// repository name). With the repo-aliases setting on (the default) a
// checkout also answers to the names its own data gives it: every remote's
// repository name (repoid's Remotes: a fork's upstream, a mirror, the URL a
// renamed repo still has), and the package name of its root manifest
// (package.json, Cargo.toml, pyproject.toml, go.mod; an npm scope is
// dropped) when that name shares a word of three letters or more with a name
// already known, so a generic package name ("agents", "server") never joins
// an unrelated project's facts. A repo renamed or republished keeps the facts
// filed under its old name that way, with no list of renames to maintain.
// Only slugs count; everything is read from files, no git process.
func repoAliases(r repoid.Repo) []string {
	out := append([]string{r.Name}, r.Aliases...)
	if !deliveryOn("REPO_ALIASES", true) {
		return dedupeNames(out)
	}
	out = append(out, r.Remotes...)
	words := map[string]bool{}
	for _, n := range out {
		for _, w := range nameWords(n) {
			words[w] = true
		}
	}
	for _, n := range manifestNames(r.Worktree) {
		for _, w := range nameWords(n) {
			if words[w] {
				out = append(out, n)
				break
			}
		}
	}
	return dedupeNames(out)
}

func nameWords(n string) []string {
	var out []string
	for _, w := range strings.FieldsFunc(strings.ToLower(n), func(r rune) bool { return r == '-' || r == '_' || r == '.' }) {
		if len(w) >= 3 {
			out = append(out, w)
		}
	}
	return out
}

func dedupeNames(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		k := strings.ToLower(x)
		if x != "" && !seen[k] && fact.IsSlug(k) {
			seen[k] = true
			out = append(out, x)
		}
	}
	return out
}

var (
	tomlSection = regexp.MustCompile(`^\s*\[([^\]]+)\]\s*$`)
	tomlName    = regexp.MustCompile(`^\s*name\s*=\s*"([^"]+)"`)
	goModule    = regexp.MustCompile(`^\s*module\s+(\S+)`)
)

// manifestNames reads the package names a repo's root manifests declare.
// Each file is read up to 64 KiB.
func manifestNames(root string) []string {
	read := func(name string) []byte {
		fh, err := os.Open(filepath.Join(root, name))
		if err != nil {
			return nil
		}
		defer fh.Close()
		if fi, err := fh.Stat(); err != nil || !fi.Mode().IsRegular() {
			return nil
		}
		b := make([]byte, 64<<10)
		n, _ := fh.Read(b)
		return b[:n]
	}
	var out []string
	if b := read("package.json"); b != nil {
		var pkg struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(b, &pkg) == nil && pkg.Name != "" {
			out = append(out, pkg.Name[strings.LastIndex(pkg.Name, "/")+1:])
		}
	}
	for file, section := range map[string]string{"Cargo.toml": "package", "pyproject.toml": "project"} {
		b := read(file)
		in := false
		sc := bufio.NewScanner(strings.NewReader(string(b)))
		for sc.Scan() {
			if m := tomlSection.FindStringSubmatch(sc.Text()); m != nil {
				in = strings.TrimSpace(m[1]) == section
				continue
			}
			if m := tomlName.FindStringSubmatch(sc.Text()); in && m != nil {
				out = append(out, m[1])
				break
			}
		}
	}
	if b := read("go.mod"); b != nil {
		sc := bufio.NewScanner(strings.NewReader(string(b)))
		for sc.Scan() {
			if m := goModule.FindStringSubmatch(sc.Text()); m != nil {
				mod := strings.TrimRight(m[1], "/")
				if base := filepath.Base(mod); regexp.MustCompile(`^v\d+$`).MatchString(base) {
					mod = filepath.Dir(mod) // example.com/x/v2 is x
				}
				out = append(out, filepath.Base(mod))
				break
			}
		}
	}
	return out
}
