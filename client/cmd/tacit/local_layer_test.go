package main

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/curate"
	"github.com/agent-sh/tacit/client/internal/dream"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/overlay"
	"github.com/agent-sh/tacit/client/internal/platform"
	"github.com/agent-sh/tacit/client/internal/search"
	"github.com/agent-sh/tacit/client/internal/store"
)

// localRig is a machine with a read-only clone of a server (a bare repo
// seeded with the store's facts), its own overlay, index and state, the
// local layer on, and a scrump stand-in that masks "Zq7Vx9".
type localRig struct {
	t                        *testing.T
	root, home, bare, local  string
	s                        *store.Store
	serverHead, cloneHead    string
	cloneFiles               map[string]string
	transcript, statePath    string
	splitAnswer, judgedPairs string
	prompts                  string
}

// sharedFacts are the store's facts on the server. quokka-build-cache is
// the one a local note restates and a local fact claims to replace.
var sharedFacts = map[string]string{
	"quokka-build-cache":  "The quokka build cache lives in /var/cache/quokka on every machine",
	"numbat-release-day":  "Numbat releases are cut on Tuesdays after the nightly run",
	"wombat-lint-command": "Run make lint-wombat before pushing the wombat repo",
	"bilby-ci-runner":     "The bilby CI runner is the self-hosted pool named bilby-large",
	"possum-log-path":     "Possum service logs are under /var/log/possum by default",
	"dingo-port":          "The dingo dev server listens on port 4410",
	"emu-schema-owner":    "The emu schema is owned by the data team and changes need their review",
	"kiwi-cache-flag":     "Pass --no-cache to kiwi builds when the lockfile changed",
}

func newLocalRig(t *testing.T) *localRig {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell CLI fixture")
	}
	r := &localRig{t: t, root: t.TempDir()}
	r.home = filepath.Join(r.root, "home")
	os.MkdirAll(r.home, 0o700)
	for k, v := range map[string]string{
		"HOME": r.home, "USERPROFILE": r.home, "TACIT_STATE_DIR": "", "XDG_CACHE_HOME": filepath.Join(r.root, "cache"),
		"TACIT_INDEX": filepath.Join(r.root, "index"), "TACIT_PROJECTS": filepath.Join(r.root, "projects"),
		"TACIT_DREAM_MAX_RUNTIME": "", "TACIT_HERMES_DB": "off", "TACIT_SETTINGS": "", "TACIT_LOCAL_LAYER": "on",
		"GIT_CONFIG_GLOBAL": "/dev/null", "GIT_CONFIG_NOSYSTEM": "1",
		"TACIT_CURATE_RUNNER": "claude", "TACIT_JUDGE_RUNNER": "claude", "TACIT_DREAM_RUNNER": "claude",
	} {
		t.Setenv(k, v)
	}
	r.local = filepath.Join(r.root, "local")
	t.Setenv("TACIT_LOCAL", r.local)
	// A scrump that masks the fixture's token at the same length (and the
	// canary RedactionReady sends), whatever scrump this machine has.
	sed, err := exec.LookPath("sed")
	if err != nil {
		t.Skip("sed not installed")
	}
	bin := filepath.Join(r.root, "bin")
	os.MkdirAll(bin, 0o755)
	os.WriteFile(filepath.Join(bin, "scrump"), []byte("#!/bin/sh\nexec "+sed+" 's/Zq7Vx9/******/g'\n"), 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	r.bare = filepath.Join(r.root, "server.git")
	git(t, r.root, "init", "-q", "--bare", "-b", "main", r.bare)
	seed := filepath.Join(r.root, "seed")
	git(t, r.root, "clone", "-q", r.bare, seed)
	for name, desc := range sharedFacts {
		writeFactFile(t, seed, &fact.Fact{Path: "user/" + name + ".md", Meta: fact.Meta{
			Name: name, Description: desc, Type: "machine_fact", Scope: "user",
			Source: fact.Source{Harness: "curator", Host: "workstation", Kind: "observed"}, Created: "2026-10-01",
		}, Body: desc + ".\n"})
	}
	markAudited(t, seed)
	git(t, seed, "add", "-A")
	git(t, seed, "commit", "-q", "-m", "seed")
	git(t, seed, "push", "-q", "origin", "HEAD:main")

	r.s = &store.Store{Dir: filepath.Join(r.root, "store")}
	t.Setenv("TACIT_STORE", r.s.Dir)
	if err := r.s.CloneReadOnly(context.Background(), r.bare, ""); err != nil {
		t.Fatal(err)
	}
	r.serverHead = gitIn(t, r.bare, "rev-parse", "main")
	r.cloneHead = gitIn(t, r.s.Dir, "rev-parse", "HEAD")
	r.cloneFiles = treeBytes(t, r.s.Dir)

	// One Claude Code session of the owner's on this machine.
	r.transcript = filepath.Join(r.home, ".claude", "projects", "-home-u", "s9.jsonl")
	os.MkdirAll(filepath.Dir(r.transcript), 0o700)
	os.WriteFile(r.transcript, []byte(strings.Join([]string{
		`{"type":"user","entrypoint":"cli","sessionId":"s9","message":{"content":"never deploy the walrus service on fridays without a rollback plan"}}`,
		`{"type":"assistant","sessionId":"s9","message":{"content":[{"type":"text","text":"The walrus fixture driver listens on port 7790 here."}]}}`,
		strings.Repeat(" ", 5000),
	}, "\n")+"\n"), 0o600)
	r.statePath = filepath.Join(platform.StateDir(r.home), "dreamer.json")
	dream.SaveState(r.statePath, &dream.State{Offsets: map[string]int64{r.transcript: 0}})

	// The curator CLI: the rulings pass finds nothing more, the split answers
	// what the test put in $FIXTURE_SPLIT. Every prompt is logged.
	r.prompts = filepath.Join(r.root, "prompts")
	t.Setenv("FIXTURE_PROMPTS", r.prompts)
	fake := filepath.Join(r.root, "fake-cli")
	os.WriteFile(fake, []byte(`#!/bin/sh
in=$(cat)
printf '%s\n\036\n' "$in" >> "$FIXTURE_PROMPTS"
case "$in" in
*"OWNER RULINGS already extracted"*) printf '%s\n' '{"result":"[]","total_cost_usd":0.01}' ;;
*) printf '%s\n' "$FIXTURE_SPLIT" ;;
esac
`), 0o700)
	t.Setenv("TACIT_CLAUDE_COMMAND", fake)
	t.Cleanup(func(f func(string) bool) func() { return func() { hasKey = f } }(hasKey))
	// The conflict judge: supersedes for the pairs the test names, no else.
	previous := judgeRun
	t.Cleanup(func() { judgeRun = previous })
	return r
}

// split sets the curator's answer to entries.
func (r *localRig) split(entries ...map[string]any) {
	b, _ := json.Marshal(entries)
	out, _ := json.Marshal(map[string]any{"result": string(b), "total_cost_usd": .01})
	r.t.Setenv("FIXTURE_SPLIT", string(out))
}

// judge makes the conflict judge answer "supersedes" for a pair whose
// existing fact is one of names, and records every pair it was asked about.
func (r *localRig) judge(names ...string) *[]string {
	var seen []string
	pairRx := regexp.MustCompile(`EXISTING \(([^)]*)\): .*\nNEW \(([^)]*)\)`)
	judgeRun = func(_ context.Context, _, prompt string) (string, float64, error) {
		var out []string
		for _, m := range pairRx.FindAllStringSubmatch(prompt, -1) {
			seen = append(seen, m[1]+" <- "+m[2])
			v := "no"
			for _, n := range names {
				if m[1] == n {
					v = "supersedes"
				}
			}
			out = append(out, v)
		}
		b, _ := json.Marshal(out)
		return string(b), .001, nil
	}
	return &seen
}

func (r *localRig) promptCount() int {
	b, _ := os.ReadFile(r.prompts)
	n := 0
	for _, p := range strings.Split(string(b), "\n\x1e\n") {
		if strings.TrimSpace(p) != "" {
			n++
		}
	}
	return n
}

// treeBytes is every file of a clone's working tree (not .git) and its bytes.
func treeBytes(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			b, _ := os.ReadFile(p)
			rel, _ := filepath.Rel(dir, p)
			out[rel] = string(b)
		}
		return nil
	})
	return out
}

// untouched fails unless the server and the clone are exactly as seeded: the
// server's main, the clone's HEAD, its refs, its working tree byte for byte,
// nothing staged, untracked or ignored.
func (r *localRig) untouched() {
	r.t.Helper()
	if got := gitIn(r.t, r.bare, "rev-parse", "main"); got != r.serverHead {
		r.t.Fatalf("the server moved: %s, was %s", got, r.serverHead)
	}
	if n := gitIn(r.t, r.bare, "rev-list", "--all", "--count"); n != "1" {
		r.t.Fatalf("the server holds %s commits, want the seed alone", n)
	}
	if got := gitIn(r.t, r.s.Dir, "rev-parse", "HEAD"); got != r.cloneHead {
		r.t.Fatalf("the clone's HEAD moved: %s, was %s", got, r.cloneHead)
	}
	if out := gitIn(r.t, r.s.Dir, "status", "--porcelain", "--ignored"); out != "" {
		r.t.Fatalf("the clone changed:\n%s", out)
	}
	if refs := gitIn(r.t, r.s.Dir, "for-each-ref", "--format=%(refname)"); strings.Contains(refs, "tacit/") || strings.Count(refs, "\n") > 2 {
		r.t.Fatalf("the clone has new refs:\n%s", refs)
	}
	got := treeBytes(r.t, r.s.Dir)
	if len(got) != len(r.cloneFiles) {
		r.t.Fatalf("the clone holds %d files, was %d", len(got), len(r.cloneFiles))
	}
	for p, b := range r.cloneFiles {
		if got[p] != b {
			r.t.Fatalf("the clone's %s changed:\n%s", p, got[p])
		}
	}
}

// The full, idle and session dreams of a read-only clone with the local
// layer dream this machine's transcripts into its overlay's inbox, redacted,
// with the same cursors and meter as the store's dreamer, and write nothing
// to the clone or the server.
func TestLocalDreamWritesOnlyTheOverlay(t *testing.T) {
	r := newLocalRig(t)
	previous := dreamRun
	defer func() { dreamRun = previous }()
	calls := 0
	dreamRun = func(_ context.Context, _, prompt string) (string, float64, int, error) {
		calls++
		if !strings.Contains(prompt, "walrus") {
			t.Errorf("the dream prompt holds no transcript: %q", prompt)
		}
		return `[{"description":"The walrus fixture driver listens on port 7790, token Zq7Vx9","type":"machine_fact","kind":"observed","applies_to":{}},{"description":"Never deploy the walrus service on fridays without a rollback plan","type":"user_standard","kind":"user","quote":"never deploy the walrus service on fridays without a rollback plan","applies_to":{}}]`, .02, 100, nil
	}
	if code := runDream(nil); code != 0 {
		t.Fatalf("local dream exit %d", code)
	}
	if calls != 1 {
		t.Fatalf("%d dream call(s), want 1", calls)
	}
	r.untouched()
	notes, errs := overlay.Load(r.local)
	if len(errs) != 0 || len(notes) != 2 {
		t.Fatalf("overlay: %d notes, %v", len(notes), errs)
	}
	host := store.ShortHost()
	for _, n := range notes {
		if !strings.HasPrefix(n.Path, "local/inbox/dreamer@"+host+"/") || n.Meta.Source.Harness != "dreamer" || n.Meta.Source.Session == "" {
			t.Fatalf("note %s: %+v", n.Path, n.Meta)
		}
		rel, _ := overlay.Rel(n.Path)
		b, _ := os.ReadFile(filepath.Join(r.local, rel))
		if strings.Contains(string(b), "Zq7Vx9") {
			t.Fatalf("a local note is not redacted:\n%s", b)
		}
	}
	st, _ := dream.LoadState(r.statePath)
	if st.Offsets[r.transcript] == 0 {
		t.Fatal("the cursor did not move")
	}
	b, _ := os.ReadFile(filepath.Join(platform.StateDir(r.home), "dreamer.log"))
	var m dream.Meter
	if json.Unmarshal(b, &m) != nil || !m.Local || m.Calls != 1 {
		t.Fatalf("dreamer.log: %s", b)
	}

	// The idle and the session dreams take new content the same way.
	f, _ := os.OpenFile(r.transcript, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"type":"user","entrypoint":"cli","sessionId":"s9","message":{"content":"the kiwi build now needs node 24 here"}}` + "\n" + `{"type":"assistant","sessionId":"s9","message":{"content":[{"type":"text","text":"The kiwi build failed on node 22 and passed on node 24."}]}}` + "\n" + strings.Repeat(" ", 5000) + "\n")
	f.Close()
	quiet := time.Now().Add(-time.Hour)
	os.Chtimes(r.transcript, quiet, quiet)
	dreamRun = func(context.Context, string, string) (string, float64, int, error) {
		calls++
		return `[{"description":"The kiwi build needs node 24 on this machine","type":"machine_fact","kind":"observed","applies_to":{}}]`, .01, 10, nil
	}
	if code := runDream([]string{"--idle", "1ms"}); code != 0 || calls != 2 {
		t.Fatalf("local idle dream: exit %d, %d call(s)", code, calls)
	}
	f, _ = os.OpenFile(r.transcript, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"type":"user","entrypoint":"cli","sessionId":"s9","message":{"content":"and the emu tests need the vpn"}}` + "\n" + `{"type":"assistant","sessionId":"s9","message":{"content":[{"type":"text","text":"The emu tests time out without the vpn."}]}}` + "\n" + strings.Repeat(" ", 5000) + "\n")
	f.Close()
	if code := runDream([]string{"--session", r.transcript}); code != 0 || calls != 3 {
		t.Fatalf("local session dream: exit %d, %d call(s)", code, calls)
	}
	r.untouched()
	if notes, _ := overlay.Load(r.local); len(notes) != 3 {
		t.Fatalf("overlay holds %d notes after the idle and session dreams, want 3", len(notes))
	}

	// A dreamer note is unreviewed: no hook delivers it, search --inbox finds it.
	ix, err := openIndex(context.Background(), r.s)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	sit := search.Situation{Host: host, Query: "walrus fixture driver port"}
	hits, _ := ix.Query(context.Background(), sit, 10, false)
	for _, h := range hits {
		if h.Local {
			t.Fatalf("a dreamer note was delivered: %+v", h)
		}
	}
	hits, _ = ix.Query(context.Background(), sit, 10, true)
	found := false
	for _, h := range hits {
		found = found || h.Local && h.Inbox
	}
	if !found {
		t.Fatalf("search --inbox does not find the local dreamer note: %+v", hits)
	}
}

// The local curator splits the overlay's dreamer notes into curated facts of
// this machine: a ruling stays the owner's only with their words found in
// their turns here; a note that restates a store fact is set aside before
// the split; a supersedes verdict retires a fact of the local curator and
// only puts a line in the local review file for a fact of the store. The
// store's facts, the clone and the server stay exactly as they were.
func TestLocalCurateWritesTheOverlayAndNeverTheStore(t *testing.T) {
	r := newLocalRig(t)
	host := store.ShortHost()
	// The dreamer's notes, through its own write path.
	notes := []*fact.Fact{
		dream.ToFact(dream.Extracted{Description: "The walrus fixture driver listens on port 7790 on this machine", Type: "machine_fact", Kind: "observed"}, "", host, "s9"),
		dream.ToFact(dream.Extracted{Description: "Never deploy the walrus service on fridays without a rollback plan", Type: "user_standard", Kind: "user", Quote: "never deploy the walrus service on fridays without a rollback plan"}, "", host, "s9"),
		dream.ToFact(dream.Extracted{Description: sharedFacts["quokka-build-cache"], Type: "machine_fact", Kind: "observed"}, "", host, "s9"),
	}
	notes[2].Meta.Scope = "user"
	if _, err := persistLocalDreamWindow(r.s, notes); err != nil {
		t.Fatal(err)
	}
	// An older fact of the local curator, which the new port replaces.
	old := &fact.Fact{Meta: fact.Meta{Name: "old-local-walrus-port", Description: "The walrus fixture driver listens on port 7781 on this machine", Type: "machine_fact", Scope: "user",
		Source: fact.Source{Harness: "curator", Host: host, Kind: "observed"}, Created: "2026-10-01", Origin: "an-older-note"}, Body: "The walrus fixture driver listens on port 7781 on this machine.\n"}
	ob, _ := old.Encode()
	if err := overlay.Write(r.local, r.s.Dir, "curated/user/old-local-walrus-port.md", ob); err != nil {
		t.Fatal(err)
	}
	walrus, ruling := notes[0].Meta.Name, notes[1].Meta.Name
	r.split(
		map[string]any{"origin": walrus, "name": "walrus-driver-port", "description": "The walrus fixture driver listens on port 7790 on this machine", "type": "machine_fact", "scope": "user", "kind": "observed", "body": "The walrus fixture driver listens on port 7790 on this machine; its token is Zq7Vx9.", "applies_to": map[string]any{}},
		map[string]any{"origin": walrus, "name": "quokka-cache-moved", "description": "The quokka build cache lives in /opt/quokka-cache on this machine now, not in /var/cache/quokka", "type": "machine_fact", "scope": "user", "kind": "observed", "body": "Moved to /opt/quokka-cache.", "applies_to": map[string]any{}},
		map[string]any{"origin": ruling, "name": "no-friday-walrus-deploys", "description": "Never deploy the walrus service on fridays without a rollback plan", "type": "user_standard", "scope": "user", "kind": "user", "owner_quote": "never deploy the walrus service on fridays without a rollback plan", "body": `The owner said "never deploy the walrus service on fridays without a rollback plan".`, "applies_to": map[string]any{}},
		map[string]any{"origin": ruling, "name": "walrus-agent-reading", "description": "The owner prefers walrus deploys early in the week", "type": "user_standard", "scope": "user", "kind": "user", "owner_quote": "the owner prefers walrus deploys early in the week", "body": "An agent's reading.", "applies_to": map[string]any{}},
	)
	pairs := r.judge("quokka-build-cache", "old-local-walrus-port")

	if code := runCurate(nil); code != 0 {
		t.Fatalf("local curate exit %d", code)
	}
	r.untouched()

	read := func(rel string) *fact.Fact {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(r.local, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		f, err := fact.Parse(overlay.Prefix+rel, b)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "Zq7Vx9") {
			t.Fatalf("%s is not redacted:\n%s", rel, b)
		}
		return f
	}
	port := read("curated/user/walrus-driver-port.md")
	if port.Meta.Source.Harness != "curator" || port.Meta.Source.Kind != "observed" || port.Meta.Origin != walrus {
		t.Fatalf("walrus-driver-port: %+v", port.Meta)
	}
	rule := read("curated/user/no-friday-walrus-deploys.md")
	if rule.Meta.Source.Kind != "user" || rule.Meta.Source.OwnerQuote == "" || !strings.HasPrefix(rule.Meta.Source.OwnerQuoteFrom, "claude:") {
		t.Fatalf("the owner's ruling lost its evidence: %+v", rule.Meta.Source)
	}
	if reading := read("curated/user/walrus-agent-reading.md"); reading.Meta.Source.Kind != "observed" || reading.Meta.Source.OwnerQuote != "" {
		t.Fatalf("an agent's reading became a ruling: %+v", reading.Meta.Source)
	}
	if got := read("curated/user/old-local-walrus-port.md"); got.Meta.SupersededBy != "walrus-driver-port" {
		t.Fatalf("the older local fact was not superseded: %+v", got.Meta)
	}
	ledgerB, _ := os.ReadFile(filepath.Join(r.local, filepath.FromSlash(overlay.QuotesRel)))
	if !strings.Contains(string(ledgerB), `"no-friday-walrus-deploys"`) || strings.Contains(string(ledgerB), "walrus-agent-reading") {
		t.Fatalf("local quote ledger:\n%s", ledgerB)
	}
	reviewB, _ := os.ReadFile(filepath.Join(r.local, filepath.FromSlash(overlay.ReviewDir), time.Now().Format("2006-01-02")+".md"))
	if !strings.Contains(string(reviewB), "[[quokka-cache-moved]] (this machine) may replace the store's [[quokka-build-cache]]; owner decides") {
		t.Fatalf("local review file:\n%s\npairs judged: %v", reviewB, *pairs)
	}
	var cst curatorState
	b, _ := os.ReadFile(filepath.Join(platform.StateDir(r.home), localCuratorState))
	if json.Unmarshal(b, &cst) != nil || cst.Restated[notes[2].Meta.Name] != "quokka-build-cache" {
		t.Fatalf("the note restating the store's fact was not set aside: %s", b)
	}
	if _, err := os.Stat(filepath.Join(platform.StateDir(r.home), "curator.json")); err == nil {
		t.Fatal("the local curator wrote the store curator's state")
	}
	var logged curate.Report
	if lb, err := os.ReadFile(filepath.Join(platform.StateDir(r.home), "curator.log")); err != nil || json.Unmarshal(lb, &logged) != nil || !logged.Local || logged.Rulings != 1 || logged.Facts != 4 {
		t.Fatalf("curator.log: %+v %v", logged, err)
	}

	// Delivery: the ruling is a rule here, marked (this machine); the agents'
	// facts are plain lines; the superseded one is gone.
	ix, err := openIndex(context.Background(), r.s)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	hits, err := ix.Query(context.Background(), search.Situation{Host: host, Query: "walrus deploy fridays rollback plan driver port"}, 10, false)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]search.Hit{}
	for _, h := range hits {
		byName[h.Name] = h
	}
	if h, ok := byName["no-friday-walrus-deploys"]; !ok || !h.Ruling || !h.Local || !h.Rule() {
		t.Fatalf("the local ruling: %+v (hits %v)", h, hits)
	} else if blk := search.Block([]search.Hit{h}, "#"); !strings.Contains(blk, "- RULE no-friday-walrus-deploys (this machine): ") {
		t.Fatalf("block = %q", blk)
	}
	if h, ok := byName["walrus-driver-port"]; !ok || h.Ruling || !h.Local {
		t.Fatalf("walrus-driver-port: %+v", h)
	}
	if _, ok := byName["old-local-walrus-port"]; ok {
		t.Fatal("a superseded local fact was delivered")
	}
	// With the layer off, nothing the layer wrote is delivered.
	t.Setenv("TACIT_LOCAL_LAYER", "off")
	offIx, err := openIndex(context.Background(), r.s)
	if err != nil {
		t.Fatal(err)
	}
	offHits, _ := offIx.Query(context.Background(), search.Situation{Host: host, Query: "walrus deploy fridays rollback plan driver port"}, 10, true)
	offIx.Close()
	for _, h := range offHits {
		if h.Local {
			t.Fatalf("layer off, a local fact is delivered: %+v", h)
		}
	}
	t.Setenv("TACIT_LOCAL_LAYER", "on")
	// A curated local fact competes like a curated one: not under the cap of
	// this machine's raw notes.
	var many []search.Hit
	for i := 0; i < 6; i++ {
		many = append(many, search.Hit{Name: "curated-" + string(rune('a'+i)), Path: "local/curated/user/x.md", Scope: "user", Local: true, Score: 30})
	}
	if got := startSelection(many, "", startLimit); len(got) != 6 {
		t.Fatalf("start selection capped the local curator's facts: %d", len(got))
	}

	// A second run finds nothing pending and calls no model.
	before := r.promptCount()
	if code := runCurate([]string{"--quick", "--min-interval", "1ns"}); code != 0 || r.promptCount() != before {
		t.Fatalf("second run: exit %d, %d new prompt(s)", code, r.promptCount()-before)
	}
	if code := runCurate(nil); code != 0 || r.promptCount() != before {
		t.Fatalf("third run: exit %d, %d new prompt(s)", code, r.promptCount()-before)
	}
	r.untouched()

	// Another machine of the same server sees none of it: the server never
	// received a byte of this machine's layer.
	other := &store.Store{Dir: filepath.Join(r.root, "other-store")}
	if err := other.CloneReadOnly(context.Background(), r.bare, ""); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TACIT_LOCAL", filepath.Join(r.root, "other-local"))
	t.Setenv("TACIT_INDEX", filepath.Join(r.root, "other-index"))
	oix, err := openIndex(context.Background(), other)
	if err != nil {
		t.Fatal(err)
	}
	defer oix.Close()
	ohits, _ := oix.Query(context.Background(), search.Situation{Host: host, Query: "walrus deploy fridays rollback plan driver port"}, 10, true)
	for _, h := range ohits {
		if h.Local || strings.Contains(h.Name, "walrus") {
			t.Fatalf("another machine sees this machine's fact: %+v", h)
		}
	}
	if all := gitIn(t, r.bare, "log", "--all", "-p"); strings.Contains(all, "walrus") || strings.Contains(all, "local/") {
		t.Fatal("the server holds this machine's local layer")
	}
}

// The local curator's quick run takes only the notes of the last day, like
// the store's; the full run takes the older ones.
func TestLocalQuickCurateTakesRecentNotes(t *testing.T) {
	r := newLocalRig(t)
	host := store.ShortHost()
	notes := []*fact.Fact{
		dream.ToFact(dream.Extracted{Description: "The dingo fixture proxy uses port 4412 on this machine", Type: "machine_fact", Kind: "observed"}, "", host, "s9"),
		dream.ToFact(dream.Extracted{Description: "The bilby fixture runner needs 8 GB of swap on this machine", Type: "machine_fact", Kind: "observed"}, "", host, "s9"),
	}
	if _, err := persistLocalDreamWindow(r.s, notes); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-72 * time.Hour)
	os.Chtimes(filepath.Join(r.local, "inbox", "dreamer@"+host, notes[1].Meta.Name+".md"), old, old)
	r.split(map[string]any{"origin": notes[0].Meta.Name, "name": "dingo-proxy-port", "description": "The dingo fixture proxy uses port 4412 on this machine", "type": "machine_fact", "scope": "user", "kind": "observed", "body": "Port 4412.", "applies_to": map[string]any{}})
	r.judge()
	if code := runCurate([]string{"--quick"}); code != 0 {
		t.Fatalf("quick exit %d", code)
	}
	b, _ := os.ReadFile(r.prompts)
	if r.promptCount() != 2 || !strings.Contains(string(b), "name="+notes[0].Meta.Name) || strings.Contains(string(b), "name="+notes[1].Meta.Name) {
		t.Fatalf("the quick run sent %d prompt(s):\n%s", r.promptCount(), b)
	}
	if _, err := os.Stat(filepath.Join(r.local, "curated", "user", "dingo-proxy-port.md")); err != nil {
		t.Fatal(err)
	}
	r.untouched()
}

// With the local layer off a read-only clone is what it was in v0.6.0: the
// full dream and curate refuse (exit 2), the idle dream and the quick curate
// do nothing quietly, no hook starts a dream, install dream refuses, and no
// overlay file appears. With the layer on the same commands work; each check
// here fails if the setting is not what turns them on.
func TestLocalLayerOffKeepsTheReadOnlyRefusals(t *testing.T) {
	r := newLocalRig(t)
	t.Setenv("TACIT_LOCAL_LAYER", "")
	previous := dreamRun
	defer func() { dreamRun = previous }()
	calls := 0
	dreamRun = func(context.Context, string, string) (string, float64, int, error) {
		calls++
		return `[]`, 0, 0, nil
	}
	before, _ := os.ReadFile(r.statePath)
	if code := runDream(nil); code != 2 {
		t.Fatalf("full dream exit %d, want 2", code)
	}
	quiet := time.Now().Add(-time.Hour)
	os.Chtimes(r.transcript, quiet, quiet)
	if code := runDream([]string{"--idle", "1ms"}); code != 0 {
		t.Fatalf("idle dream exit %d, want 0", code)
	}
	if code := runDream([]string{"--session", r.transcript}); code != 0 {
		t.Fatalf("session dream exit %d, want 0", code)
	}
	if after, _ := os.ReadFile(r.statePath); calls != 0 || string(after) != string(before) {
		t.Fatalf("the dream ran with the layer off: %d call(s)", calls)
	}
	r.split()
	if code := runCurate(nil); code != 2 {
		t.Fatalf("full curate exit %d, want 2", code)
	}
	if code := runCurate([]string{"--quick"}); code != 0 || r.promptCount() != 0 {
		t.Fatalf("quick curate exit %d, %d prompt(s)", code, r.promptCount())
	}
	if canDream(context.Background(), r.s) {
		t.Fatal("a hook would start a dream with the layer off")
	}
	if code := installDream("darwin", r.home, "/bin/tacit", nil); code != 1 {
		t.Fatalf("install dream exit %d, want 1", code)
	}
	if _, err := os.Stat(r.local); !os.IsNotExist(err) {
		t.Fatal("the overlay was created with the layer off")
	}
	r.untouched()

	t.Setenv("TACIT_LOCAL_LAYER", "on")
	if !canDream(context.Background(), r.s) {
		t.Fatal("no dream with the layer on")
	}
	// An overlay inside the clone: refused before any work, nothing written.
	t.Setenv("TACIT_LOCAL", filepath.Join(r.s.Dir, "local"))
	if canDream(context.Background(), r.s) || runDream(nil) != 2 || runCurate(nil) != 2 || calls != 0 {
		t.Fatal("the local layer ran on an overlay inside the clone")
	}
	r.untouched()
}

// The PreCompact hook of a read-only clone starts the session dream only
// with the local layer on.
func TestPreCompactStartsALocalDream(t *testing.T) {
	r := newLocalRig(t)
	out := filepath.Join(r.root, "args")
	fake := filepath.Join(r.root, "tacit")
	os.WriteFile(fake, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" > '"+out+"'\n"), 0o700)
	previous := hookExe
	defer func() { hookExe = previous }()
	hookExe = func() (string, error) { return fake, nil }
	in := `{"session_id":"s9","transcript_path":"` + r.transcript + `","cwd":"` + r.home + `","hook_event_name":"PreCompact"}`
	wait := func() string {
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
			if b, _ := os.ReadFile(out); len(b) > 0 {
				return string(b)
			}
		}
		return ""
	}
	t.Setenv("TACIT_LOCAL_LAYER", "")
	hookOut(t, in, "pre-compact")
	time.Sleep(time.Second)
	if _, err := os.Stat(out); err == nil {
		t.Fatal("a dream started on a read-only clone with the layer off")
	}
	t.Setenv("TACIT_LOCAL_LAYER", "on")
	hookOut(t, in, "pre-compact")
	if got := wait(); got != "dream --session "+r.transcript+"\n" {
		t.Fatalf("started %q", got)
	}
	r.untouched()
}

// A split answer that fails validation is logged as its entry, field and
// rule, in curator.log and on stderr, never with the entry's text.
func TestInvalidSplitAnswerIsLoggedByFieldAndRule(t *testing.T) {
	r := newLocalRig(t)
	host := store.ShortHost()
	n := dream.ToFact(dream.Extracted{Description: "The dingo fixture proxy uses port 4412 on this machine", Type: "machine_fact", Kind: "observed"}, "", host, "s9")
	if _, err := persistLocalDreamWindow(r.s, []*fact.Fact{n}); err != nil {
		t.Fatal(err)
	}
	r.split(map[string]any{"origin": n.Meta.Name, "name": "dingo-proxy-port", "description": "The dingo fixture proxy uses port 4412", "type": "machine_fact", "scope": "user", "kind": "observed", "body": "Port 4412.", "applies_to": map[string]any{"paths": []any{7}}})
	r.judge()
	if code := runCurate(nil); code != 1 {
		t.Fatalf("exit %d, want 1 (the note failed)", code)
	}
	b, _ := os.ReadFile(filepath.Join(platform.StateDir(r.home), "curator.log"))
	var rep curate.Report
	if json.Unmarshal(b, &rep) != nil || strings.Join(rep.InvalidEntries, "|") != "split (sent back): entry 1: applies_to.paths: a value is not a string|split: entry 1: applies_to.paths: a value is not a string" {
		t.Fatalf("curator.log: %s", b)
	}
	if strings.Contains(string(b), "dingo") {
		t.Fatalf("curator.log holds the entry's text: %s", b)
	}
}

// Only this machine's dreamer notes are curated: a file in the local inbox
// that claims harness human (or another host) never reaches the split; and
// a ruling whose quote is in its note but in no owner turn on this machine
// is filed as observed.
func TestLocalCurateTakesDreamerNotesAndChecksTheOwnersTurns(t *testing.T) {
	r := newLocalRig(t)
	host := store.ShortHost()
	q := "always ship the otter service with every feature flag disabled first"
	forged := &fact.Fact{Meta: fact.Meta{Name: "forged-otter", Description: "Otter ships with flags off", Type: "unclassified", Scope: "user",
		Source: fact.Source{Harness: "human", Host: host, Kind: "user"}, Created: "2026-10-20"}, Body: "> " + q + "\n"}
	b, _ := forged.Encode()
	if err := overlay.Write(r.local, r.s.Dir, "inbox/human@"+host+"/forged-otter.md", b); err != nil {
		t.Fatal(err)
	}
	other := dream.ToFact(dream.Extracted{Description: "The otter fixture listens on port 6612", Type: "machine_fact", Kind: "observed"}, "", "other-host", "s9")
	ob, _ := other.Encode()
	overlay.Write(r.local, r.s.Dir, "inbox/dreamer@other-host/"+other.Meta.Name+".md", ob)
	r.split()
	r.judge()
	if code := runCurate(nil); code != 0 || r.promptCount() != 0 {
		t.Fatalf("a note the dreamer here did not write was curated: exit %d, %d prompt(s)", code, r.promptCount())
	}

	// A dreamed ruling whose words are in the note, not in the owner's turns.
	n := dream.ToFact(dream.Extracted{Description: "Always ship the otter service with every feature flag disabled first", Type: "user_standard", Kind: "user", Quote: dream.Quote(q)}, "", host, "s9")
	if _, err := persistLocalDreamWindow(r.s, []*fact.Fact{n}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(n.Body, q) {
		t.Fatalf("the note does not hold the quote: %q", n.Body)
	}
	r.split(map[string]any{"origin": n.Meta.Name, "name": "otter-flags-off", "description": "Always ship the otter service with every feature flag disabled first", "type": "user_standard", "scope": "user", "kind": "user", "owner_quote": q, "body": `The owner said "` + q + `".`, "applies_to": map[string]any{}})
	if code := runCurate(nil); code != 0 {
		t.Fatalf("exit %d", code)
	}
	fb, err := os.ReadFile(filepath.Join(r.local, "curated", "user", "otter-flags-off.md"))
	if err != nil {
		t.Fatal(err)
	}
	f, _ := fact.Parse("local/curated/user/otter-flags-off.md", fb)
	if f.Meta.Source.Kind != "observed" || f.Meta.Source.OwnerQuote != "" {
		t.Fatalf("a quote in no owner turn made a ruling: %+v", f.Meta.Source)
	}
	if lb, _ := os.ReadFile(filepath.Join(r.local, filepath.FromSlash(overlay.QuotesRel))); strings.Contains(string(lb), "otter") {
		t.Fatalf("the local ledger holds it: %s", lb)
	}
	r.untouched()
}

// On a writer clone the store curator never pairs a new fact with this
// machine's overlay: no local fact reaches its judge, its review file or the
// server, also with a local fact the judge would call a contradiction.
func TestStoreCuratorNeverPairsWithTheOverlay(t *testing.T) {
	r := newLocalRig(t)
	host := store.ShortHost()
	secret := "The marmot vault unlock phrase lives in the ops notebook on this laptop"
	lf := &fact.Fact{Meta: fact.Meta{Name: "marmot-vault-phrase", Description: secret, Type: "machine_fact", Scope: "user",
		Source: fact.Source{Harness: "curator", Host: host, Kind: "observed"}, Created: "2026-10-20"}, Body: secret + ".\n"}
	b, _ := lf.Encode()
	if err := overlay.Write(r.local, r.s.Dir, "curated/user/marmot-vault-phrase.md", b); err != nil {
		t.Fatal(err)
	}
	// The machine becomes a writer clone (a path remote needs no key).
	gitIn(t, r.s.Dir, "config", "--unset", "tacit.readonly")
	gitIn(t, r.s.Dir, "config", "--unset", "remote.origin.pushurl")
	gitIn(t, r.s.Dir, "config", "user.name", "curator")
	gitIn(t, r.s.Dir, "config", "user.email", "curator@tacit")
	note := &fact.Fact{Path: "inbox/claude@" + host + "/marmot-note.md", Meta: fact.Meta{Name: "marmot-note", Description: "The marmot vault unlock phrase moved to the shared password manager", Type: "unclassified", Scope: "user",
		Source: fact.Source{Harness: "claude", Host: host, Kind: "observed"}, Created: "2026-10-20"}, Body: "The marmot vault unlock phrase moved to the shared password manager.\n"}
	writeFactFile(t, r.s.Dir, note)
	git(t, r.s.Dir, "add", "-A")
	git(t, r.s.Dir, "commit", "-q", "-m", "note")
	git(t, r.s.Dir, "push", "-q", "origin", "HEAD:main")
	r.split(map[string]any{"origin": "marmot-note", "name": "marmot-phrase-moved", "description": "The marmot vault unlock phrase moved to the shared password manager", "type": "machine_fact", "scope": "user", "kind": "observed", "body": "Moved.", "applies_to": map[string]any{}})
	var asked []string
	judgeRun = func(_ context.Context, _, prompt string) (string, float64, error) {
		var out []string
		for _, m := range regexp.MustCompile(`EXISTING \(([^)]*)\)`).FindAllStringSubmatch(prompt, -1) {
			asked = append(asked, m[1])
			out = append(out, "contradicts")
		}
		b, _ := json.Marshal(out)
		return string(b), 0, nil
	}
	previous := curateUpkeep
	defer func() { curateUpkeep = previous }()
	curateUpkeep = func(context.Context, *store.Store, string, bool, *curate.Report) {}
	prevStale := curateStaleStep
	defer func() { curateStaleStep = prevStale }()
	curateStaleStep = func(context.Context, *store.Store, bool) int { return 0 }
	if code := runCurate([]string{"--max-batches", "1"}); code != 0 {
		t.Fatalf("store curate exit %d", code)
	}
	if _, ok := findFact(r.s.Dir, "marmot-phrase-moved"); !ok {
		t.Fatal("the store curator wrote nothing")
	}
	for _, a := range asked {
		if a == "marmot-vault-phrase" {
			t.Fatal("the judge was asked about a local fact")
		}
	}
	if all := gitIn(t, r.bare, "log", "--all", "-p"); strings.Contains(all, "marmot-vault-phrase") || strings.Contains(all, "ops notebook") {
		t.Fatal("the server holds this machine's local fact")
	}
}

// tacit_search marks a local dreamer note as unreviewed and as this machine's.
func TestMCPTagsALocalDreamerNoteUnreviewed(t *testing.T) {
	if got := mcpTag(search.Hit{Inbox: true, Local: true}); got != " [unreviewed] (this machine)" {
		t.Fatalf("tag %q", got)
	}
	if got := mcpTag(search.Hit{Local: true}); got != " (this machine)" {
		t.Fatalf("tag %q", got)
	}
}

// A ruling whose quote the write's redaction changes is filed as observed
// and its ledger line dropped, while another ruling of the batch keeps its
// own line.
func TestLocalRulingWhoseQuoteIsRedactedIsObserved(t *testing.T) {
	r := newLocalRig(t)
	host := store.ShortHost()
	q := "never rotate the walrus token Zq7Vx9 without telling the oncall first"
	f, _ := os.OpenFile(r.transcript, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"type":"user","entrypoint":"cli","sessionId":"s9","message":{"content":"` + q + `"}}` + "\n")
	f.Close()
	// A note as written before redaction (the write's redaction is what is tested).
	n := &fact.Fact{Meta: fact.Meta{Name: "walrus-rules", Description: "Owner rules about the walrus service", Type: "unclassified", Scope: "user",
		Source: fact.Source{Harness: "dreamer", Host: host, Kind: "user", Session: "s9"}, Created: "2026-10-20"},
		Body: `Quoted from the session: "` + q + `" and "never deploy the walrus service on fridays without a rollback plan"` + "\n"}
	b, _ := n.Encode()
	if err := overlay.Write(r.local, r.s.Dir, "inbox/dreamer@"+host+"/walrus-rules.md", b); err != nil {
		t.Fatal(err)
	}
	r.split(
		map[string]any{"origin": "walrus-rules", "name": "walrus-token-rotation", "description": "Never rotate the walrus token without telling the oncall first", "type": "user_standard", "scope": "user", "kind": "user", "owner_quote": q, "body": `The owner said "` + q + `".`, "applies_to": map[string]any{}},
		map[string]any{"origin": "walrus-rules", "name": "no-friday-walrus-deploys", "description": "Never deploy the walrus service on fridays without a rollback plan", "type": "user_standard", "scope": "user", "kind": "user", "owner_quote": "never deploy the walrus service on fridays without a rollback plan", "body": `The owner said "never deploy the walrus service on fridays without a rollback plan".`, "applies_to": map[string]any{}},
	)
	r.judge()
	if code := runCurate(nil); code != 0 {
		t.Fatalf("exit %d", code)
	}
	fb, err := os.ReadFile(filepath.Join(r.local, "curated", "user", "walrus-token-rotation.md"))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := fact.Parse("local/curated/user/walrus-token-rotation.md", fb)
	if strings.Contains(string(fb), "Zq7Vx9") || got.Meta.Source.Kind != "observed" || got.Meta.Source.OwnerQuote != "" {
		t.Fatalf("a ruling whose quote was redacted:\n%s", fb)
	}
	lb, _ := os.ReadFile(filepath.Join(r.local, filepath.FromSlash(overlay.QuotesRel)))
	if strings.Contains(string(lb), "walrus-token-rotation") || !strings.Contains(string(lb), `"no-friday-walrus-deploys"`) {
		t.Fatalf("local ledger:\n%s", lb)
	}
	r.untouched()
}

// The local dream neither syncs the clone nor recovers notes for a push: a
// server that moved leaves the clone where it was.
func TestLocalDreamDoesNotSync(t *testing.T) {
	r := newLocalRig(t)
	seed := filepath.Join(r.root, "seed")
	os.WriteFile(filepath.Join(seed, "user", "later.md"), []byte(factFile("later")), 0o644)
	git(t, seed, "add", "-A")
	git(t, seed, "commit", "-q", "-m", "later")
	git(t, seed, "push", "-q", "origin", "HEAD:main")
	previous := dreamRun
	defer func() { dreamRun = previous }()
	dreamRun = func(context.Context, string, string) (string, float64, int, error) {
		return `[{"description":"The walrus fixture driver listens on port 7790","type":"machine_fact","kind":"observed","applies_to":{}}]`, .01, 10, nil
	}
	if code := runDream(nil); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got := gitIn(t, r.s.Dir, "rev-parse", "HEAD"); got != r.cloneHead {
		t.Fatal("the local dream synced the clone")
	}
	if refs := gitIn(t, r.s.Dir, "rev-parse", "origin/main"); refs != r.cloneHead {
		t.Fatal("the local dream fetched")
	}
}
