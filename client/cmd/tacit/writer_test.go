package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/store"
)

// readOnlyClone is a read-only clone of the review fixture's server, with
// that server's seed checkout for another writer.
func readOnlyClone(t *testing.T) (rs *reviewServer, bare, seed string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	bare, _ = reviewFixture(t)
	seed = filepath.Join(filepath.Dir(bare), "seed")
	s := &store.Store{Dir: filepath.Join(t.TempDir(), "ro")}
	if err := s.CloneReadOnly(context.Background(), bare, ""); err != nil {
		t.Fatal(err)
	}
	rs = &reviewServer{s: s}
	rs.loadFacts()
	return rs, bare, seed
}

func porcelain(t *testing.T, dir string) string {
	t.Helper()
	return strings.TrimSpace(git(t, dir, "status", "--porcelain", "--untracked-files=all"))
}

// The review's repro: a mark on a read-only clone used to write the facts,
// MEMORY.md and decisions.jsonl and keep them when the push refused, so the
// next change to the same files on the server wedged every sync. Now the mark
// refuses before writing, the tree stays clean, and syncs go on.
func TestMarkOnAReadOnlyCloneLeavesTheTreeClean(t *testing.T) {
	rs, _, seed := readOnlyClone(t)
	ctx := context.Background()
	id := itemFor(t, rs, "a", "b").ID
	if err := mark(ctx, rs, id, "right"); !errors.Is(err, errViewOnly) {
		t.Fatalf("mark: %v, want the view-only refusal", err)
	}
	if st := porcelain(t, rs.s.Dir); st != "" {
		t.Fatalf("the refused mark left files:\n%s", st)
	}
	// Another writer changes the same files on the server.
	os.WriteFile(filepath.Join(seed, "system", "a.md"), []byte(factFile("a")+"changed\n"), 0o644)
	os.WriteFile(filepath.Join(seed, "MEMORY.md"), []byte("# moved on\n"), 0o644)
	git(t, seed, "add", "-A")
	git(t, seed, "commit", "-q", "-m", "elsewhere")
	git(t, seed, "push", "-q", "origin", "HEAD:main")
	for i := 0; i < 2; i++ {
		if err := rs.s.Sync(ctx); err != nil {
			t.Fatalf("sync %d after the refused mark: %v", i, err)
		}
	}
	if got, want := strings.TrimSpace(git(t, rs.s.Dir, "rev-parse", "HEAD")), strings.TrimSpace(git(t, seed, "rev-parse", "HEAD")); got != want {
		t.Fatalf("clone at %s, server at %s", got, want)
	}
}

// Every owner write on a read-only clone refuses before it writes.
func TestOwnerWritesRefuseOnAReadOnlyClone(t *testing.T) {
	rs, _, _ := readOnlyClone(t)
	ctx := context.Background()
	for name, call := range map[string]func() (*saveResult, error){
		"edit": func() (*saveResult, error) {
			return rs.editFact(ctx, "a", factDraft{Description: "x", Body: "x", Scope: "system", Type: "design_decision"})
		},
		"retire": func() (*saveResult, error) { return rs.setRetired(ctx, "a", true) },
		"undo":   func() (*saveResult, error) { return rs.undoLast(ctx, "a") },
		"ruling": func() (*saveResult, error) {
			return rs.newRuling(ctx, rulingDraft{Body: "always pin scrump", Scope: "user"})
		},
		"mine":    func() (*saveResult, error) { return rs.endorse(ctx, "a") },
		"notmine": func() (*saveResult, error) { return rs.disown(ctx, "a") },
	} {
		res, err := call()
		if !errors.Is(err, errViewOnly) || (res != nil && res.Saved) {
			t.Errorf("%s: %+v, %v; want the view-only refusal", name, res, err)
		}
	}
	if st := porcelain(t, rs.s.Dir); st != "" {
		t.Fatalf("refused owner writes left files:\n%s", st)
	}
}

// A machine without the curator key (a Mac) is view only: the page offers
// no mark and no edit, and every write refuses before it writes.
func TestViewOnlyWithoutTheCuratorKey(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	bare, clone := reviewFixture(t)
	_ = bare
	rs := clone("host-b")
	ctx := context.Background()
	// A clone of the server: its origin is http. Only the config is read.
	git(t, rs.s.Dir, "remote", "set-url", "origin", "http://tacit.invalid:7447/git")
	defer func(f func(string) bool) { hasKey = f }(hasKey)
	hasKey = func(h string) bool { return h != "curator" }
	if got := viewOnlyReason(ctx, rs.s); got != viewOnlyLine {
		t.Fatalf("viewOnlyReason without the curator key = %q", got)
	}
	hasKey = func(string) bool { return true }
	if got := viewOnlyReason(ctx, rs.s); got != "" {
		t.Fatalf("viewOnlyReason with the key = %q", got)
	}

	rs.viewOnly = viewOnlyLine
	rs.loadFacts()
	for _, it := range rs.items() {
		if len(it.Allowed) != 0 {
			t.Fatalf("view only, yet %s offers %v", it.ID, it.Allowed)
		}
	}
	m, err := rs.meta(ctx)
	if err != nil || m.ViewOnly != viewOnlyLine {
		t.Fatalf("meta view_only = %+v, %v", m, err)
	}
	d, err := rs.factDetail(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if d.CanEdit || d.CanRetire || d.CanRestore || d.CanMine || d.CanNotMine || d.Undo.Can || d.Why != viewOnlyLine {
		t.Fatalf("view only, yet the fact offers: %+v", d)
	}
	id := itemFor(t, rs, "a", "b").ID
	if err := mark(ctx, rs, id, "right"); !errors.Is(err, errViewOnly) {
		t.Fatalf("mark: %v", err)
	}
	if _, err := rs.setRetired(ctx, "a", true); !errors.Is(err, errViewOnly) {
		t.Fatalf("retire: %v", err)
	}
	if st := porcelain(t, rs.s.Dir); st != "" {
		t.Fatalf("view-only writes left files:\n%s", st)
	}
}

// The writing commands refuse on a read-only clone before any work: nothing
// synced, called, written or committed.
func TestWritingCommandsRefuseUpFront(t *testing.T) {
	rs, _, _ := readOnlyClone(t)
	t.Setenv("TACIT_STORE", rs.s.Dir)
	home := t.TempDir()
	t.Setenv("HOME", home)
	head := strings.TrimSpace(git(t, rs.s.Dir, "rev-parse", "HEAD"))
	verdicts := filepath.Join(t.TempDir(), "v.json")
	os.WriteFile(verdicts, []byte(`[{"id":"x","mark":"left"}]`), 0o644)
	for name, run := range map[string]func() int{
		"curate":       func() int { return runCurate([]string{"--max-batches", "1"}) },
		"derive":       func() int { return runDerive([]string{"--projects", home}) },
		"results":      func() int { return runResults([]string{"--projects", home}) },
		"stale":        func() int { return runStale(nil) },
		"links":        func() int { return runLinks(nil) },
		"verify":       func() int { return runVerify(nil) },
		"resolve":      func() int { return runResolve([]string{"clear", "a"}) },
		"rulings":      func() int { return runRulings([]string{"audit"}) },
		"review apply": func() int { return runReviewApply([]string{verdicts}) },
		"dream":        func() int { return runDream(nil) },
		"import":       func() int { return runImport([]string{"--store", rs.s.Dir, "claude", home}) },
	} {
		if code := run(); code != 2 {
			t.Errorf("%s on a read-only clone exits %d, want 2", name, code)
		}
		if st := porcelain(t, rs.s.Dir); st != "" {
			t.Fatalf("%s left files:\n%s", name, st)
		}
		if now := strings.TrimSpace(git(t, rs.s.Dir, "rev-parse", "HEAD")); now != head {
			t.Fatalf("%s moved the clone", name)
		}
	}
	// The dreamer moved no cursor: no state was written.
	if _, err := os.Stat(filepath.Join(home, ".local", "state", "tacit", "dreamer.json")); !os.IsNotExist(err) {
		t.Fatal("the dreamer wrote its state on a read-only clone")
	}
}

// The dreamer without its key refuses before any model call; with it (and a
// local remote, which needs none) it gets past the check.
func TestDreamNeedsItsKeyUpFront(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	_, clone := reviewFixture(t)
	rs := clone("dreamer")
	git(t, rs.s.Dir, "remote", "set-url", "origin", "http://tacit.invalid:7447/git")
	defer func(f func(string) bool) { hasKey = f }(hasKey)
	hasKey = func(string) bool { return false }
	err := writerCheck(context.Background(), rs.s, "dreamer")
	if err == nil || !strings.Contains(err.Error(), "tacit keygen dreamer") {
		t.Fatalf("writerCheck without the key: %v", err)
	}
	if err := writerCheck(context.Background(), rs.s, "curator"); err == nil || !strings.Contains(err.Error(), "holds the curator key") {
		t.Fatalf("writerCheck curator: %v", err)
	}
}

// install dream refuses a read-only clone and writes no job.
func TestInstallDreamRefusesAReadOnlyClone(t *testing.T) {
	rs, _, _ := readOnlyClone(t)
	t.Setenv("TACIT_STORE", rs.s.Dir)
	home := t.TempDir()
	if code := installDream("darwin", home, "/bin/tacit", nil); code != 1 {
		t.Fatalf("install dream on a read-only clone exits %d", code)
	}
	if _, err := os.Stat(filepath.Join(home, "Library", "LaunchAgents", "ai.tacit.dream.plist")); !os.IsNotExist(err) {
		t.Fatal("a dream job was written for a read-only clone")
	}
}

func TestAuthAnswer(t *testing.T) {
	for _, c := range []struct {
		status int
		body   string
		want   int
	}{
		{200, "", authOK},
		{401, "writer token rejected: unknown_key", authRetry},
		{401, "writer token rejected: host_mismatch", authStop},
		{401, "writer token rejected: bad_signature", authStop},
		{401, "writer token rejected: expired", authStop},
		{401, "writer token rejected: unknown_harness", authStop},
		{403, "tailnet only", authStop},
		{502, "bad gateway", authRetry},
		{404, "not found", authStop},
	} {
		if got := authAnswer("claude@host-b", c.status, c.body); got != c.want {
			t.Errorf("%d %q -> %d, want %d", c.status, c.body, got, c.want)
		}
	}
}

func TestCapLogRotatesToOneOlderFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "follow.log")
	l, err := openCapLog(path, 200)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		l.Printf("stream down %d; reconnecting", i)
	}
	cur, _ := os.ReadFile(path)
	old, _ := os.ReadFile(path + ".1")
	if len(cur) == 0 || len(cur) > 200 || len(old) == 0 || len(old) > 200 {
		t.Fatalf("sizes %d and %d, want each in (0, 200]", len(cur), len(old))
	}
	if !strings.Contains(string(cur), "stream down 39; reconnecting") {
		t.Fatalf("newest line missing:\n%s", cur)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 2 {
		t.Fatalf("%d files, want the log and one .1", len(entries))
	}
	// A restart appends to the current file.
	l2, err := openCapLog(path, 200)
	if err != nil {
		t.Fatal(err)
	}
	l2.Printf("again")
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), "stream down 39") || !strings.Contains(string(b), "again") {
		t.Fatalf("reopen did not append:\n%s", b)
	}
}
