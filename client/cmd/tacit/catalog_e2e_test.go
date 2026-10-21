package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/dream"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/overlay"
	"github.com/agent-sh/tacit/client/internal/platform"
	"github.com/agent-sh/tacit/client/internal/repoid"
	"github.com/agent-sh/tacit/client/internal/store"
)

// catalogMachine points every tacit path at one simulated machine under
// root/name (its home, state, clone, index, overlay, projects directory),
// with the local layer on or off.
func catalogMachine(t *testing.T, root, name string, layer bool) string {
	t.Helper()
	home := filepath.Join(root, name)
	os.MkdirAll(home, 0o700)
	onOff := "off"
	if layer {
		onOff = "on"
	}
	for k, v := range map[string]string{
		"HOME": home, "USERPROFILE": home, "TACIT_STATE_DIR": "", "XDG_STATE_HOME": "", "XDG_CACHE_HOME": filepath.Join(home, "cache"),
		"TACIT_STORE": filepath.Join(home, "store"), "TACIT_INDEX": filepath.Join(home, "index"), "TACIT_LOCAL": filepath.Join(home, "local"),
		"TACIT_PROJECTS": filepath.Join(home, "projects"), "TACIT_LOCAL_LAYER": onOff, "TACIT_HERMES_DB": "off", "TACIT_SETTINGS": "",
		"TACIT_DREAM_MAX_RUNTIME": "", "TACIT_DREAM_RUNNER": "claude",
	} {
		t.Setenv(k, v)
	}
	return home
}

// checkout makes dir a git checkout with origin url.
func checkout(t *testing.T, dir, url string) {
	t.Helper()
	gitIn(t, filepath.Dir(dir), "init", "-q", dir)
	gitIn(t, dir, "remote", "add", "origin", url)
}

// session writes one Claude Code session in cwd on the machine at home, its
// cursor at the start, so the next dream reads it.
func session(t *testing.T, home, cwd, id, text string) {
	t.Helper()
	f := filepath.Join(home, ".claude", "projects", "p", id+".jsonl")
	os.MkdirAll(filepath.Dir(f), 0o700)
	os.WriteFile(f, []byte(`{"type":"user","entrypoint":"cli","sessionId":"`+id+`","cwd":"`+filepath.ToSlash(cwd)+`","message":{"content":"`+text+`"}}`+"\n"+
		`{"type":"assistant","sessionId":"`+id+`","message":{"content":[{"type":"text","text":"Noted: `+text+`."}]}}`+"\n"+strings.Repeat(" ", 5000)+"\n"), 0o600)
	if err := dream.SaveState(filepath.Join(platform.StateDir(home), "dreamer.json"), &dream.State{Offsets: map[string]int64{f: 0}}); err != nil {
		t.Fatal(err)
	}
}

// Two machines share a store through its server; a third reads it with the
// local layer. A repository checked out only on machine A is known on
// machine B through A's catalog, which A's dreamer commits beside its notes
// (inbox/dreamer@<host>/_repos.json): B's tacit add and the curator's checks
// accept it, where before A's catalog arrived they refused it. The local
// layer machine keeps its own catalog in its overlay, its checks read both,
// and nothing of it ever reaches the server.
func TestRepoCatalogTravelsThroughTheStore(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell scrump fixture")
	}
	root, _ := filepath.EvalSymlinks(t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "t")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "t@t")
	}
	sed, err := exec.LookPath("sed")
	if err != nil {
		t.Skip("sed not installed")
	}
	bin := filepath.Join(root, "bin")
	os.MkdirAll(bin, 0o755)
	os.WriteFile(filepath.Join(bin, "scrump"), []byte("#!/bin/sh\nexec "+sed+" 's/Zq7Vx9/******/g'\n"), 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Cleanup(func(f func(string) bool) func() { return func() { hasKey = f } }(hasKey))
	hasKey = func(string) bool { return true }
	previous := dreamRun
	t.Cleanup(func() { dreamRun = previous })
	ctx := context.Background()
	host := store.ShortHost()

	// The server: a bare repository seeded with one fact.
	bare := filepath.Join(root, "server.git")
	gitIn(t, root, "init", "-q", "--bare", "-b", "main", bare)
	seed := filepath.Join(root, "seed")
	gitIn(t, root, "clone", "-q", bare, seed)
	writeFactFile(t, seed, &fact.Fact{Path: "user/tabs-in-go.md", Meta: fact.Meta{Name: "tabs-in-go", Description: "Go code is formatted with gofmt", Type: "user_standard", Scope: "user", Source: fact.Source{Harness: "curator", Host: "seedbox", Kind: "observed"}, Created: "2026-10-01"}, Body: "Go code is formatted with gofmt.\n"})
	markAudited(t, seed)
	gitIn(t, seed, "add", "-A")
	gitIn(t, seed, "commit", "-q", "-m", "seed")
	gitIn(t, seed, "push", "-q", "origin", "HEAD:main")

	// Machine B first: a writer with no checkout of gadget. Before machine
	// A's catalog arrives, a note about gadget names no known repository.
	homeB := catalogMachine(t, root, "b", false)
	sB := &store.Store{Dir: filepath.Join(homeB, "store")}
	gitIn(t, homeB, "clone", "-q", bare, sB.Dir)
	gadgetNote := fact.Meta{Name: "gadget-port", Description: "the gadget server listens on port 8123", Type: "design_decision", Scope: "projects/gadget", Source: fact.Source{Kind: "observed"}}
	if _, err := addFact(ctx, sB, "muse", gadgetNote, "body"); err == nil || !strings.Contains(err.Error(), "names no known repository") {
		t.Fatalf("B accepted gadget before A's catalog: %v", err)
	}

	// Machine A: gadget is checked out in its projects directory, a session
	// works in it, and the dreamer pushes the session's note with A's catalog.
	homeA := catalogMachine(t, root, "a", false)
	sA := &store.Store{Dir: filepath.Join(homeA, "store")}
	gitIn(t, homeA, "clone", "-q", bare, sA.Dir)
	gadget := filepath.Join(homeA, "projects", "gadget")
	os.MkdirAll(filepath.Dir(gadget), 0o755)
	checkout(t, gadget, "https://alice:Zq7Vx9tok@github.com/Acme/gadget.git")
	session(t, homeA, gadget, "sa1", "the gadget scheduler runs every five minutes")
	dreamRun = func(context.Context, string, string) (string, float64, int, error) {
		return `[{"description":"The gadget scheduler runs every five minutes","type":"design_decision","kind":"observed","applies_to":{}}]`, .01, 10, nil
	}
	if code := runDream(nil); code != 0 {
		t.Fatalf("A's dream exit %d", code)
	}
	rel := "inbox/dreamer@" + host + "/" + repoid.CatalogFile
	shared := gitIn(t, bare, "show", "main:"+rel)
	if !strings.Contains(shared, `"name": "gadget"`) || !strings.Contains(shared, `"github.com/acme/gadget"`) {
		t.Fatalf("A's catalog on the server:\n%s", shared)
	}
	for _, leak := range []string{homeA, root, "alice", "Zq7Vx9"} {
		if strings.Contains(shared, leak) {
			t.Fatalf("A's catalog holds %q:\n%s", leak, shared)
		}
	}
	notes := gitIn(t, bare, "ls-tree", "-r", "--name-only", "main", "inbox/dreamer@"+host)
	if !strings.Contains(notes, "the-gadget-scheduler-runs-every-five-minutes.md") {
		t.Fatalf("A's note is not on the server beside its catalog:\n%s", notes)
	}
	// Another window with no change to the catalog commits only its note.
	before := gitIn(t, bare, "rev-parse", "main")
	session(t, homeA, gadget, "sa2", "the gadget cache lives in redis")
	dreamRun = func(context.Context, string, string) (string, float64, int, error) {
		return `[{"description":"The gadget cache lives in redis","type":"design_decision","kind":"observed","applies_to":{}}]`, .01, 10, nil
	}
	if code := runDream(nil); code != 0 {
		t.Fatalf("A's second dream exit %d", code)
	}
	if changed := gitIn(t, bare, "diff", "--name-only", before, "main"); strings.Contains(changed, repoid.CatalogFile) {
		t.Fatalf("an unchanged catalog was committed again:\n%s", changed)
	}

	// Back on B: after a sync, A's catalog makes gadget a known repository
	// for tacit add and for the curator's checks.
	catalogMachine(t, root, "b", false)
	if err := sB.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := addFact(ctx, sB, "muse", gadgetNote, "body"); err != nil {
		t.Fatalf("B refused gadget with A's catalog: %v", err)
	}
	envB := curateEnv(sB.Dir, false)
	if p := envB.Scope("projects/gadget"); p != nil {
		t.Errorf("B's curator refuses projects/gadget: %v", p.Message())
	}
	if ps := envB.Repos([]string{"gadget"}); len(ps) != 0 {
		t.Errorf("B's curator refuses applies_to.repos gadget: %v", ps)
	}

	// Machine C: a read-only clone with the local layer. Its own repository,
	// secretproj, goes to its overlay's catalog; the server never sees it.
	homeC := catalogMachine(t, root, "c", true)
	sC := &store.Store{Dir: filepath.Join(homeC, "store")}
	if err := sC.CloneReadOnly(ctx, bare, ""); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(homeC, "projects", "secretproj")
	os.MkdirAll(filepath.Dir(secret), 0o755)
	checkout(t, secret, "git@github.com:acme/secretproj.git")
	session(t, homeC, secret, "sc1", "the secretproj build needs the vpn")
	dreamRun = func(context.Context, string, string) (string, float64, int, error) {
		return `[{"description":"The secretproj build needs the vpn","type":"design_decision","kind":"observed","applies_to":{}}]`, .01, 10, nil
	}
	cloneHead := gitIn(t, sC.Dir, "rev-parse", "HEAD")
	if code := runDream(nil); code != 0 {
		t.Fatalf("C's local dream exit %d", code)
	}
	local, err := os.ReadFile(filepath.Join(homeC, "local", overlay.InboxDir, "dreamer@"+host, repoid.CatalogFile))
	if err != nil || !strings.Contains(string(local), `"name": "secretproj"`) {
		t.Fatalf("C's overlay catalog: %v\n%s", err, local)
	}
	envC := curateEnv(sC.Dir, true)
	for _, scope := range []string{"projects/gadget", "projects/secretproj"} {
		if p := envC.Scope(scope); p != nil {
			t.Errorf("C refuses %s: %v", scope, p.Message())
		}
	}
	if gitIn(t, sC.Dir, "rev-parse", "HEAD") != cloneHead {
		t.Error("C's clone moved")
	}
	if st := gitIn(t, sC.Dir, "status", "--porcelain"); st != "" {
		t.Errorf("C's clone changed:\n%s", st)
	}
	if out := gitIn(t, bare, "log", "--all", "-p", "--format=%H %s"); strings.Contains(out, "secretproj") {
		t.Fatal("C's repository reached the server")
	}
	// And B, which never saw C's overlay, does not know secretproj.
	catalogMachine(t, root, "b", false)
	if p := curateEnv(sB.Dir, false).Scope("projects/secretproj"); p == nil {
		t.Error("B knows C's local repository")
	}
}
