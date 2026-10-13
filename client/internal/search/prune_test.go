package search

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// The 2026-10-11 curator run lost 509 of 605 conflict checks to "unable to
// open database file (14)": it opened the index for its HEAD, spent minutes
// in the split call, and by its first query another process (the follower,
// restarted on a new build with another schema version) had built its own
// index file and pruned the curator's. An open index must survive that.
func TestOpenIndexSurvivesPrune(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	write(t, root, "machines/host-a/pgrep-self-match.md", "pgrep-self-match", "machines/host-a", "pkill -f and pgrep -f match the Bash tool's own shell", "body", "")
	t.Setenv("TACIT_INDEX", filepath.Join(filepath.Dir(root), "index"))
	ctx := context.Background()

	held, err := Open(ctx, root, "aaa111") // the curator's index, held across the split
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	// Another process moves on: a new HEAD, or an older schema version a
	// stale binary still writes. Both prune every other index file.
	other, err := Open(ctx, root, "bbb222")
	if err != nil {
		t.Fatal(err)
	}
	other.Close()
	stale := filepath.Join(IndexDir(), "index-v2-aaa111.db")
	os.WriteFile(stale, []byte("x"), 0o644)
	prune(IndexDir(), filepath.Join(IndexDir(), "index-v2-ccc333.db"))
	if _, err := os.Stat(filepath.Join(IndexDir(), fmt.Sprintf("index-v%d-aaa111.db", schemaVersion))); err == nil {
		t.Fatal("setup: the held index file should be gone")
	}

	hits, err := held.Query(ctx, Situation{Host: "host-a", Query: "pgrep shell"}, 3, false)
	if err != nil {
		t.Fatalf("query on a held index after its file was pruned: %v", err)
	}
	if len(hits) != 1 || hits[0].Name != "pgrep-self-match" {
		t.Fatalf("hits %v", hits)
	}
	// And a second query on the same handle (one connection, kept).
	if _, err := held.Query(ctx, Situation{Host: "host-a", Query: "pkill"}, 3, false); err != nil {
		t.Fatalf("second query: %v", err)
	}
}

// prune keeps the files a running process still needs: another process's
// build in progress (its temp file and journal; deleting the journal gave
// the follower "disk I/O error (5898)" in the same run) and an index of a
// newer schema version that a newer binary uses. It removes other heads of
// its own version, older versions, and temp files whose builder is gone.
func TestPruneKeepsLiveBuildsAndNewerVersions(t *testing.T) {
	dir := t.TempDir()
	keep := filepath.Join(dir, fmt.Sprintf("index-v%d-head.db", schemaVersion))
	live := fmt.Sprintf("index-v%d-other.db.%d.tmp", schemaVersion, os.Getpid())
	files := map[string]bool{ // name -> survives
		filepath.Base(keep): true,
		live:                true,
		live + "-journal":   true,
		fmt.Sprintf("index-v%d-other.db.%d.tmp", schemaVersion, deadPID(t)):         false,
		fmt.Sprintf("index-v%d-other.db.%d.tmp-journal", schemaVersion, deadPID(t)): false,
		fmt.Sprintf("index-v%d-old.db", schemaVersion):                              false,
		fmt.Sprintf("index-v%d-old.db", schemaVersion-1):                            false,
		fmt.Sprintf("index-v%d-new.db", schemaVersion+1):                            true,
		"index-junk":     false,
		"unrelated.file": true,
	}
	for n := range files {
		os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644)
	}
	prune(dir, keep)
	for n, want := range files {
		_, err := os.Stat(filepath.Join(dir, n))
		if got := err == nil; got != want {
			t.Errorf("%s: survives=%v, want %v", n, got, want)
		}
	}
}

// A truncated or corrupt index file under the workstationht name is built again
// instead of failing every open until HEAD moves.
func TestOpenRebuildsACorruptFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	write(t, root, "machines/host-a/pgrep-self-match.md", "pgrep-self-match", "machines/host-a", "pkill -f and pgrep -f match the Bash tool's own shell", "body", "")
	t.Setenv("TACIT_INDEX", filepath.Join(filepath.Dir(root), "index"))
	os.MkdirAll(IndexDir(), 0o755)
	os.WriteFile(filepath.Join(IndexDir(), fmt.Sprintf("index-v%d-ddd444.db", schemaVersion)), []byte("not a database"), 0o644)
	ix, err := Open(context.Background(), root, "ddd444")
	if err != nil {
		t.Fatalf("open over a corrupt file: %v", err)
	}
	defer ix.Close()
	if hits, err := ix.Query(context.Background(), Situation{Host: "host-a", Query: "pgrep"}, 3, false); err != nil || len(hits) != 1 {
		t.Fatalf("hits %v err %v", hits, err)
	}
}

// deadPID is a pid no process has: past the kernel's pid_max.
func deadPID(t *testing.T) int {
	t.Helper()
	return 1<<22 + 12345
}
