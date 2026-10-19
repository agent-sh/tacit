package platform

import (
	"errors"
	"os"
	"path/filepath"
)

var ErrBusy = errors.New("file is locked by another process")

// StateDir is where tacit keeps its per-machine state (cursors, curator and
// stale state, session marks, the default index): $TACIT_STATE_DIR when set,
// else ~/.local/state/tacit under home. A second store on one machine (a
// shadow rebuild) gets its own state without changing HOME, which the
// dreamer reads repositories against.
func StateDir(home string) string {
	if d := os.Getenv("TACIT_STATE_DIR"); d != "" {
		return filepath.Clean(d)
	}
	return filepath.Join(home, ".local", "state", "tacit")
}
