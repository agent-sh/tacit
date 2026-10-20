package main

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/fact"
)

// repoDataNames are the names a checkout's own data gives it, beyond its
// directory: the repository name of every remote (`git remote -v`: a fork's
// upstream, a mirror, the URL a renamed repo still has), and the package
// name of its root manifest (package.json, Cargo.toml, pyproject.toml,
// go.mod; an npm scope is dropped) when that name shares a word of three
// letters or more with a name already known, so a generic package name
// ("agents", "server") never joins an unrelated project's facts. Only slugs
// count; the reads are bounded by ctx and a read that fails adds nothing.
func repoDataNames(ctx context.Context, cwd, root string, known []string) []string {
	var out []string
	cmd := exec.CommandContext(ctx, "git", "-C", cwd, "remote", "-v")
	cmd.WaitDelay = 100 * time.Millisecond
	if b, err := cmd.Output(); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if f := strings.Fields(line); len(f) >= 2 {
				if n := remoteRepoName(f[1]); n != "" {
					out = append(out, n)
				}
			}
		}
	}
	if root == "" {
		return dedupeNames(out)
	}
	words := map[string]bool{}
	for _, n := range append(append([]string{}, known...), out...) {
		for _, w := range nameWords(n) {
			words[w] = true
		}
	}
	for _, n := range manifestNames(root) {
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
