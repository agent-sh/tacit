package derive

import (
	"maps"
	"path"
	"slices"
	"strings"
)

// What counts as code. Hot spots answer "which code will I have to touch or
// coordinate with", so data, logs, lockfiles and prose are left out. In a
// research repo nearly every tracked file can be run output under research/
// (.log, .json, .txt, .csv), and many of its most-touched files (any type)
// are ledgers and run logs there. An allowlist keeps those out without naming
// any repo; the cost is that a language missing from the list is invisible,
// so the list is broad.
//
// The same table drives two things: the pathspecs handed to `git log` (so
// the commit window counts only commits that touched code) and IsCode (so
// the tree listing uses the identical rule). A test holds them equal.
var codeExt = set(
	// systems and general purpose
	"rs", "go", "c", "h", "cc", "cpp", "cxx", "hpp", "hh", "cu", "cuh", "zig",
	"java", "kt", "kts", "scala", "clj", "swift", "cs", "dart",
	"py", "rb", "php", "lua", "ex", "exs", "erl", "hrl", "ml", "mli", "hs",
	// web
	"ts", "tsx", "js", "jsx", "mjs", "cjs", "css", "scss", "html", "svelte", "vue",
	// shell, data definition, build and CI configuration
	"sh", "bash", "zsh", "fish", "ps1", "sql", "proto", "graphql",
	"toml", "yaml", "yml", "cmake", "gradle", "nix", "tf", "sol",
)

var codeName = set("Makefile", "Dockerfile", "justfile", "CMakeLists.txt")

// Directories that hold someone else's code or build output. A change there
// is a consequence of a decision made elsewhere, not a hot spot.
var skipDir = set("vendor", "node_modules", "third_party", "dist", "build", "target")

// Generated file suffixes.
var skipSuffix = []string{".min.js", ".min.css", ".pb.go", ".pb.cc", ".pb.h", "_pb2.py"}

func set(xs ...string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// IsCode reports whether a repo-relative path is a code file by the rule
// above. Case matters, as it does in the git pathspecs.
func IsCode(p string) bool {
	parts := strings.Split(p, "/")
	for _, d := range parts[:len(parts)-1] {
		if skipDir[d] {
			return false
		}
	}
	base := parts[len(parts)-1]
	for _, s := range skipSuffix {
		if strings.HasSuffix(base, s) {
			return false
		}
	}
	if codeName[base] {
		return true
	}
	return codeExt[strings.TrimPrefix(path.Ext(base), ".")]
}

// pathspecs is the IsCode rule as git pathspecs: positive globs for the
// extensions and names, `exclude` globs for the skipped directories and
// suffixes. Sorted so the command line is stable.
func pathspecs() []string {
	var out []string
	for _, e := range sortedKeys(codeExt) {
		out = append(out, "*."+e)
	}
	for _, n := range sortedKeys(codeName) {
		out = append(out, ":(glob)**/"+n)
	}
	for _, d := range sortedKeys(skipDir) {
		out = append(out, ":(exclude,glob)**/"+d+"/**")
	}
	for _, s := range skipSuffix {
		out = append(out, ":(exclude,glob)**/*"+s)
	}
	return out
}

func sortedKeys(m map[string]bool) []string { return slices.Sorted(maps.Keys(m)) }
