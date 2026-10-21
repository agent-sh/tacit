package repoid

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// MinWorkSessions is how many distinct sessions must have worked in a
// checkout outside home or in a temp directory (Elsewhere) before it counts
// as a repository: one session's scratch clone is not a project, two
// sessions that edited or committed there are.
const MinWorkSessions = 2

// Elsewhere reports a checkout whose root is outside home or in one of the
// system's temp directories (os.TempDir, and /tmp and /var/tmp off
// Windows), and is no main checkout of the projects directory (mains, which
// the owner keeps where they like). Such a checkout is a clone a tool or a
// task made (a scratch dir, a build tree, a toolchain) until sessions show
// real work in it. A temp directory that holds home is not counted: home is
// then a sandbox's, and everything in it would be elsewhere.
func Elsewhere(root, home string, mains *Mains) bool {
	if root == "" {
		return false
	}
	if mains.In(root) {
		return false
	}
	h := realPath(home)
	sep := string(filepath.Separator)
	if h == "" || (root != h && !strings.HasPrefix(root, h+sep)) {
		return true
	}
	for _, t := range tempDirs() {
		if h == t || strings.HasPrefix(h, t+sep) {
			continue
		}
		if root == t || strings.HasPrefix(root, t+sep) {
			return true
		}
	}
	return false
}

func tempDirs() []string {
	cands := []string{os.TempDir()}
	if runtime.GOOS != "windows" {
		cands = append(cands, "/tmp", "/var/tmp")
	}
	var out []string
	seen := map[string]bool{}
	for _, c := range cands {
		if c == "" {
			continue
		}
		r := realPath(c)
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	return out
}

// WorkSessions counts the distinct sessions in ws.
func WorkSessions(ws []Work) int {
	seen := map[string]bool{}
	for _, w := range ws {
		seen[w.Session] = true
	}
	return len(seen)
}
