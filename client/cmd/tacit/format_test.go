package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/lint"
)

// A new store starts in this binary's format; a store from before the
// format gets the marker from the owner's one-time command, in one commit,
// and asking again writes nothing.
func TestFormatMarkerCommand(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	t.Setenv("HOME", home)
	t.Setenv("TACIT_STATE_DIR", "")
	t.Setenv("TACIT_INDEX", filepath.Join(home, "index"))
	dir := filepath.Join(home, "store")
	t.Setenv("TACIT_STORE", dir)
	if code := runInit(nil); code != 0 {
		t.Fatalf("init %d", code)
	}
	f, ok, err := ledger.ReadFormat(dir)
	if err != nil || !ok || f.Version != ledger.FormatVersion || !f.Writes() {
		t.Fatalf("a new store's marker: %+v %v %v", f, ok, err)
	}
	if out := gitIn(t, dir, "ls-files", ledger.FormatPath); out != ledger.FormatPath {
		t.Fatalf("the marker is not committed: %q", out)
	}
	// A store from before the format.
	gitIn(t, dir, "rm", "-q", ledger.FormatPath)
	gitIn(t, dir, "commit", "-q", "-m", "older store")
	head := gitIn(t, dir, "rev-parse", "HEAD")
	if code := runFormat(nil); code != 0 {
		t.Fatalf("format (show) %d", code)
	}
	if gitIn(t, dir, "rev-parse", "HEAD") != head {
		t.Fatal("showing the marker wrote")
	}
	if code := runFormat([]string{"--set"}); code != 0 {
		t.Fatalf("format --set %d", code)
	}
	if _, ok, _ := ledger.ReadFormat(dir); !ok {
		t.Fatal("no marker after --set")
	}
	if msg := gitIn(t, dir, "log", "-1", "--format=%s"); !strings.HasPrefix(msg, fmt.Sprintf("curator: format: store format %d", ledger.FormatVersion)) {
		t.Fatalf("commit %q", msg)
	}
	set := gitIn(t, dir, "rev-parse", "HEAD")
	if code := runFormat([]string{"--set"}); code != 0 || gitIn(t, dir, "rev-parse", "HEAD") != set {
		t.Fatalf("a second --set wrote again (%d)", code)
	}
	if code := runFormat([]string{"--records-complete"}); code != 0 {
		t.Fatalf("records-complete %d", code)
	}
	if f, _, _ := ledger.ReadFormat(dir); !f.RecordsComplete {
		t.Fatal("records_complete not recorded")
	}
	if fs, err := lint.Run(dir); err != nil || len(fs) != 0 {
		t.Fatalf("lint: %v %v", fs, err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Fatal(err)
	}
}
