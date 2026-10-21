package search

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// indexTarget is the exact index file Open builds for head, and its lock.
func indexTarget(t *testing.T, root, head string) target {
	t.Helper()
	tg, err := targetFor(root, head)
	if err != nil {
		t.Fatal(err)
	}
	return tg
}

// A change of the overlay builds the layered file from the store-only file
// of the same HEAD: the store is built once, whatever the overlay does.
func TestOverlayChangeNeverRebuildsTheStore(t *testing.T) {
	base := t.TempDir()
	root, local := filepath.Join(base, "store"), filepath.Join(base, "local")
	t.Setenv("TACIT_INDEX", filepath.Join(base, "index"))
	t.Setenv("TACIT_LOCAL", local)
	write(t, root, "user/a.md", "a", "user", "homebrew is pinned by the owner", "body", "")
	write(t, local, "claude@host-a/n0.md", "n0", "user", "first note about homebrew", "body", "")
	var builds []string
	onBuild = func(p string) { builds = append(builds, filepath.Base(p)) }
	defer func() { onBuild = nil }()
	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		ix, err := Open(ctx, root, "abc")
		if err != nil {
			t.Fatal(err)
		}
		hits, _ := ix.Query(ctx, Situation{Host: "host-a", Query: "homebrew note"}, 10, false)
		ix.Close()
		if len(hits) != i+1 {
			t.Fatalf("round %d: %d hits, want the fact and %d notes", i, len(hits), i)
		}
		write(t, local, fmt.Sprintf("claude@host-a/n%d.md", i), fmt.Sprintf("n%d", i), "user", "another note about homebrew", "body", "")
	}
	store := fmt.Sprintf("index-v%d-abc.db", schemaVersion)
	n := 0
	for _, b := range builds {
		if b == store {
			n++
		}
	}
	if n != 1 || len(builds) != 4 {
		t.Fatalf("builds %v: want the store-only file once and three layered files", builds)
	}
}

// OpenRecent never builds while an older index is there: it opens the one
// of the same HEAD with an older overlay, starts one build, and the next
// call after that build gets the exact index.
func TestOpenRecentServesTheOlderIndexAndStartsOneBuild(t *testing.T) {
	base := t.TempDir()
	root, local := filepath.Join(base, "store"), filepath.Join(base, "local")
	t.Setenv("TACIT_INDEX", filepath.Join(base, "index"))
	t.Setenv("TACIT_LOCAL", local)
	write(t, root, "user/a.md", "a", "user", "homebrew is pinned by the owner", "body", "")
	write(t, local, "claude@host-a/n0.md", "n0", "user", "first note about homebrew", "body", "")
	ctx := context.Background()
	ix, err := Open(ctx, root, "abc")
	if err != nil {
		t.Fatal(err)
	}
	ix.Close()
	write(t, local, "claude@host-a/n1.md", "n1", "user", "second note about homebrew", "body", "")

	built := 0
	onBuild = func(string) { built++ }
	defer func() { onBuild = nil }()
	started := 0
	start := func() { started++ }
	ix, err = OpenRecent(ctx, root, "abc", start)
	if err != nil {
		t.Fatal(err)
	}
	hits, _ := ix.Query(ctx, Situation{Host: "host-a", Query: "homebrew note"}, 10, false)
	if !ix.Stale() || len(hits) != 2 || built != 0 || started != 1 {
		t.Fatalf("stale=%v hits=%d built=%d started=%d: want the older layered index, no build, one start", ix.Stale(), len(hits), built, started)
	}
	ix.Close()

	// While a process builds the exact file, nothing more is started.
	lk, err := tryLock(indexTarget(t, root, "abc").path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	ix, err = OpenRecent(ctx, root, "abc", start)
	if err != nil || !ix.Stale() || started != 1 {
		t.Fatalf("with a build running: err=%v started=%d", err, started)
	}
	ix.Close()
	lk.release()

	// The started build (here, in process) makes the next call exact.
	ix, err = OpenRecent(ctx, root, "abc", func() {
		if ix, err := Open(ctx, root, "abc"); err == nil {
			ix.Close()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	ix.Close()
	ix, err = OpenRecent(ctx, root, "abc", start)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	hits, _ = ix.Query(ctx, Situation{Host: "host-a", Query: "homebrew note"}, 10, false)
	if ix.Stale() || len(hits) != 3 || started != 1 {
		t.Fatalf("after the build: stale=%v hits=%d started=%d", ix.Stale(), len(hits), started)
	}
}

// With no index of the new HEAD, OpenRecent opens the newest of an older
// HEAD; with none at all it builds within its deadline, and when that runs
// out it starts the build and returns the error; while another process
// builds it, it returns at once.
func TestOpenRecentFallsBackToAnOlderHeadOrBuilds(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "store")
	t.Setenv("TACIT_INDEX", filepath.Join(base, "index"))
	t.Setenv("TACIT_LOCAL", filepath.Join(base, "local"))
	write(t, root, "user/a.md", "a", "user", "homebrew is pinned by the owner", "body", "")
	ctx := context.Background()
	started := 0
	start := func() { started++ }

	ix, err := OpenRecent(ctx, root, "old", start)
	if err != nil || ix.Stale() || started != 0 {
		t.Fatalf("nothing there: err=%v started=%d; want a build in the call", err, started)
	}
	ix.Close()

	ix, err = OpenRecent(ctx, root, "new", start)
	if err != nil || !ix.Stale() || started != 1 {
		t.Fatalf("new HEAD: err=%v started=%d; want the old HEAD's index and one start", err, started)
	}
	ix.Close()

	os.RemoveAll(IndexDir())
	tg := indexTarget(t, root, "new")
	lk, err := tryLock(tg.path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	if _, err := OpenRecent(ctx, root, "new", start); !errors.Is(err, ErrBuilding) || time.Since(t0) > time.Second || started != 1 {
		t.Fatalf("no index, a build running: err=%v after %v, started %d; want ErrBuilding at once", err, time.Since(t0), started)
	}
	lk.release()

	short, cancel := context.WithTimeout(ctx, time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)
	if _, err := OpenRecent(short, root, "new", start); err == nil || started != 2 {
		t.Fatalf("no index and no time: err=%v started=%d; want an error and a start", err, started)
	}
}

// A lock file left by a builder that died (its pid gone) holds nothing:
// the next build goes ahead at once, and prune removes the file.
func TestDeadBuildersLockIsNoLock(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "store")
	t.Setenv("TACIT_INDEX", filepath.Join(base, "index"))
	t.Setenv("TACIT_LOCAL", filepath.Join(base, "local"))
	write(t, root, "user/a.md", "a", "user", "homebrew is pinned by the owner", "body", "")
	tg := indexTarget(t, root, "abc")
	os.WriteFile(tg.path+".lock", []byte(fmt.Sprintf("%d %d\n", deadPID(t), time.Now().Unix())), 0o644)
	other := filepath.Join(tg.dir, fmt.Sprintf("index-v%d-zzz.db.lock", schemaVersion))
	os.WriteFile(other, []byte(fmt.Sprintf("%d %d\n", deadPID(t), time.Now().Unix())), 0o644)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	t0 := time.Now()
	ix, err := Open(ctx, root, "abc")
	if err != nil {
		t.Fatal(err)
	}
	ix.Close()
	if d := time.Since(t0); d > 2*time.Second {
		t.Fatalf("open took %v behind a dead builder's lock", d)
	}
	for _, p := range []string{tg.path + ".lock", other} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s survived", filepath.Base(p))
		}
	}
}

// A waiter waits for a live builder until its deadline; a builder that has
// held the lock longer than lockStaleAge is taken as stuck and built past.
func TestLiveLockWaitsAndAStuckOneIsBuiltPast(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "store")
	t.Setenv("TACIT_INDEX", filepath.Join(base, "index"))
	t.Setenv("TACIT_LOCAL", filepath.Join(base, "local"))
	write(t, root, "user/a.md", "a", "user", "homebrew is pinned by the owner", "body", "")
	tg := indexTarget(t, root, "abc")
	lk, err := tryLock(tg.path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer lk.release()
	short, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := Open(short, root, "abc"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("behind a live builder: %v, want the deadline", err)
	}
	if _, err := os.Stat(tg.path); err == nil {
		t.Fatal("built behind a live builder's lock")
	}
	lk.f.WriteAt([]byte(fmt.Sprintf("%d %d\n", os.Getpid(), time.Now().Add(-lockStaleAge-time.Minute).Unix())), 0)
	ix, err := Open(context.Background(), root, "abc")
	if err != nil {
		t.Fatalf("behind a stuck builder: %v", err)
	}
	ix.Close()
}

// N processes that open the same missing index at once build it once: one
// store-only build and one layered build, whoever wins, and every process
// gets the index.
func TestRacingProcessesBuildOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("starts processes")
	}
	t.Setenv("TACIT_LOCAL_LAYER", "on")
	root, local := layeredFixture(t, 7, 1500, 30)
	base := t.TempDir()
	index := filepath.Join(base, "index")
	builds := filepath.Join(base, "builds.log")
	gate := filepath.Join(base, "go")
	const n = 8
	cmds := make([]*exec.Cmd, n)
	outs := make([]*strings.Builder, n)
	for i := range cmds {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperIndexBuilder$", "-test.count=1")
		cmd.Env = append(os.Environ(), "TACIT_TEST_INDEX_HELPER=1", "TACIT_INDEX="+index, "TACIT_LOCAL="+local,
			"HELPER_STORE="+root, "HELPER_BUILDS="+builds, "HELPER_GATE="+gate)
		outs[i] = &strings.Builder{}
		cmd.Stdout, cmd.Stderr = outs[i], outs[i]
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds[i] = cmd
	}
	time.Sleep(300 * time.Millisecond)
	os.WriteFile(gate, nil, 0o644)
	for i, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("process %d: %v\n%s", i, err, outs[i])
		}
		if !strings.Contains(outs[i].String(), "helper: opened") {
			t.Fatalf("process %d did not open the index:\n%s", i, outs[i])
		}
	}
	b, _ := os.ReadFile(builds)
	lines := strings.Fields(string(b))
	seen := map[string]int{}
	for _, l := range lines {
		seen[l]++
	}
	if len(lines) != 2 || len(seen) != 2 {
		t.Fatalf("builds %v: want the store-only file and the layered file once each", lines)
	}
	entries, _ := os.ReadDir(index)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") || strings.HasSuffix(e.Name(), ".lock") || strings.HasSuffix(e.Name(), "-journal") {
			t.Errorf("left behind: %s", e.Name())
		}
	}
}

// TestHelperIndexBuilder is one of TestRacingProcessesBuildOnce's processes.
func TestHelperIndexBuilder(t *testing.T) {
	if os.Getenv("TACIT_TEST_INDEX_HELPER") != "1" {
		t.Skip("helper process")
	}
	t.Setenv("TACIT_LOCAL_LAYER", "on")
	for {
		if _, err := os.Stat(os.Getenv("HELPER_GATE")); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	var mine atomic.Int32
	onBuild = func(p string) {
		mine.Add(1)
		f, err := os.OpenFile(os.Getenv("HELPER_BUILDS"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintln(f, filepath.Base(p))
			f.Close()
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ix, err := Open(ctx, os.Getenv("HELPER_STORE"), "race")
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	var n int
	if err := ix.db.QueryRow(`SELECT count(*) FROM facts`).Scan(&n); err != nil || n == 0 {
		t.Fatalf("count %d err %v", n, err)
	}
	fmt.Printf("helper: opened %d facts, built %d\n", n, mine.Load())
}

// A store-only file that fails to read is built again before the layered
// file is copied from it.
func TestLayeredBuildReplacesACorruptStoreFile(t *testing.T) {
	base := t.TempDir()
	root, local := filepath.Join(base, "store"), filepath.Join(base, "local")
	t.Setenv("TACIT_INDEX", filepath.Join(base, "index"))
	t.Setenv("TACIT_LOCAL", local)
	write(t, root, "user/a.md", "a", "user", "homebrew is pinned by the owner", "body", "")
	write(t, local, "claude@host-a/n0.md", "n0", "user", "a note about homebrew", "body", "")
	tg := indexTarget(t, root, "abc")
	os.WriteFile(tg.base, []byte("not a database"), 0o644)
	ix, err := Open(context.Background(), root, "abc")
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	if hits, err := ix.Query(context.Background(), Situation{Host: "host-a", Query: "homebrew note"}, 5, false); err != nil || len(hits) != 2 {
		t.Fatalf("hits %v err %v", hits, err)
	}
}
