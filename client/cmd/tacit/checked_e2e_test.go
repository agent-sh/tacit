package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/assess"
	"github.com/agent-sh/tacit/client/internal/check"
	"github.com/agent-sh/tacit/client/internal/curate"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/lint"
	"github.com/agent-sh/tacit/client/internal/platform"
	"github.com/agent-sh/tacit/client/internal/record"
	"github.com/agent-sh/tacit/client/internal/repoid"
	"github.com/agent-sh/tacit/client/internal/store"
)

// fakeGH installs a gh on PATH that answers `gh api repos/<o>/<r>` from
// repos (owner/name -> full name; any other is a 404), `gh api user` with
// user, and `gh auth status` with success; every call is logged to the
// returned file. The real forge (check.NewGitHub) is the checks' forge.
func fakeGH(t *testing.T, user string, repos map[string]string) (log string) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "bin")
	os.MkdirAll(bin, 0o755)
	log = filepath.Join(bin, "calls.log")
	var cases strings.Builder
	for k, v := range repos {
		fmt.Fprintf(&cases, "  'api repos/%s') echo '{\"full_name\":\"%s\"}'; exit 0;;\n", k, v)
	}
	script := "#!/bin/sh\necho \"$*\" >> '" + log + "'\ncase \"$*\" in\n  'auth status') exit 0;;\n  'api user --jq .login') echo " + user + "; exit 0;;\n" + cases.String() +
		"esac\necho '{\"message\":\"Not Found\",\"status\":\"404\"}'\necho 'gh: Not Found (HTTP 404)' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	prev := newForge
	newForge = func(ctx context.Context) check.Forge { return check.NewGitHub(ctx) }
	t.Cleanup(func() { newForge = prev })
	return log
}

// evidenceModel answers assessment prompts the way the shadow run's model
// did before checks: a repository folder is active, citing the first
// session or own commit its evidence lists; a folder with paths is a folder.
// Sent back, it reads code's refusal: a name that exists nowhere is folded
// into the first candidate code lists. It is told nothing about the right
// answers. stubborn folders are answered active with no citation, always.
type evidenceModel struct {
	prompts  []string
	stubborn map[string]bool
	review   map[string]bool // answered needs-review
}

var (
	blockRx      = regexp.MustCompile(`(?ms)^### folder=(\S+)\n(.*?)(?:\n\n|\z)`)
	citeInRx     = regexp.MustCompile(`\b(session:[A-Za-z0-9._-]+|commit:[0-9a-f]{7,})`)
	candidateRx  = regexp.MustCompile(`(?:code's candidates: |set suppressed_by to )(projects/[a-z0-9-]+)`)
	commitCiteRx = regexp.MustCompile(`\bcommit:[0-9a-f]{7,}`)
)

func (m *evidenceModel) run(_ context.Context, _, prompt string) (string, float64, error) {
	m.prompts = append(m.prompts, prompt)
	blocks := map[string]string{}
	var order []string
	for _, b := range blockRx.FindAllStringSubmatch(prompt, -1) {
		blocks[b[1]] = b[2]
		order = append(order, b[1])
	}
	decide := func(folder string) assess.Decision {
		body := blocks[folder]
		d := assess.Decision{Folder: folder, Kind: "repo", Status: "active", Description: "what " + folder + " is"}
		if m.stubborn[folder] {
			d.Why = "busy lately"
			return d
		}
		if m.review[folder] {
			d.Status, d.Why = "needs-review", "nothing places it, "+citeInRx.FindString(body)
			return d
		}
		if strings.Contains(body, "paths ~/") && !strings.Contains(body, "git_root true") {
			d.Kind = "folder"
		}
		// Its own recent commits when the evidence lists them, else a session.
		cite := commitCiteRx.FindString(body)
		if cite == "" {
			cite = citeInRx.FindString(body)
		}
		d.Why = "worked on lately, " + cite
		return d
	}
	var out []assess.Decision
	if i := strings.Index(prompt, "CODE REFUSED THESE DECISIONS"); i >= 0 {
		for _, l := range strings.Split(prompt[i:], "\n") {
			folder, problems, ok := strings.Cut(strings.TrimPrefix(l, "- "), ": ")
			if !ok || !strings.HasPrefix(l, "- ") {
				continue
			}
			d := decide(folder)
			if c := candidateRx.FindStringSubmatch(problems); c != nil {
				d.SuppressedBy = c[1]
				d.Why = "its name exists nowhere; its notes' sessions worked in " + c[1] + ", " + citeInRx.FindString(blocks[folder])
			}
			out = append(out, d)
		}
	} else {
		for _, f := range order {
			out = append(out, decide(f))
		}
	}
	b, _ := json.Marshal(out)
	return string(b), 0.1, nil
}

// kvTopics are twelve unrelated task notes (none restates another).
var kvTopics = []string{
	"The lexer drops trailing tabs before tokenizing headers",
	"Benchmarks run pinned to core three with turbo off",
	"The cache warmer skips tenants without a quota row",
	"Retry budgets reset when the circuit breaker closes",
	"Fixture snapshots live beside each golden test file",
	"The scheduler rounds deadlines up to whole seconds",
	"Metrics labels never carry a customer identifier",
	"Migration scripts refuse to run against a primary",
	"The parser rejects nested comments deeper than eight",
	"Upload chunks are hashed with blake3 before sending",
	"The config loader prefers environment over the file",
	"Shutdown drains the queue for at most thirty seconds",
}

// checkedHome is a fresh machine and store with: widget, a repository with
// own commits and a GitHub origin; gadget, the current name of a repository
// the forge renamed from old-gadget; stubborn, a checkout the model never
// answers right; notes, a project folder that is no repository. The store
// holds facts filed under widget's misspelling (widgit), under old-gadget,
// under twelve task worktrees of widget (kv-<n>-g1), under stubborn and
// notes; the dreamer's registry says which checkout each session worked in.
func checkedHome(t *testing.T) (home string, s *store.Store, ghLog string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the shell gh fixture")
	}
	home, _ = filepath.EvalSymlinks(t.TempDir())
	for k, v := range map[string]string{"HOME": home, "TACIT_STATE_DIR": "", "XDG_CACHE_HOME": filepath.Join(home, "cache"),
		"TACIT_STORE": filepath.Join(home, "store"), "TACIT_INDEX": filepath.Join(home, "index"), "TACIT_LOCAL": filepath.Join(home, "overlay"),
		"TACIT_PROJECTS": filepath.Join(home, "projects"), "TACIT_LOCAL_LAYER": "off", "TACIT_FOLLOW_STATE": filepath.Join(home, "no-follower.json"),
		"GIT_CONFIG_GLOBAL": filepath.Join(home, "gitconfig"), "GIT_CONFIG_NOSYSTEM": "1"} {
		t.Setenv(k, v)
	}
	stubScrump(t)
	ghLog = fakeGH(t, "owner", map[string]string{"owner/widget": "owner/widget", "owner/gadget": "owner/gadget", "owner/old-gadget": "owner/gadget"})
	os.WriteFile(filepath.Join(home, "gitconfig"), []byte("[user]\n\temail = owner@example.com\n\tname = Owner\n"), 0o644)
	p := filepath.Join(home, "projects")
	mkGit := func(name, origin string) string {
		dir := filepath.Join(p, name)
		os.MkdirAll(dir, 0o755)
		gitIn(t, dir, "init", "-q", "-b", "main")
		os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644)
		gitIn(t, dir, "add", "main.go")
		// Under this machine's git identity: the owner's own commit.
		cmd := exec.Command("git", "commit", "-q", "-m", "start "+name)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("commit: %v %s", err, out)
		}
		if origin != "" {
			gitIn(t, dir, "remote", "add", "origin", origin)
		}
		return dir
	}
	widget := mkGit("widget", "https://github.com/owner/widget.git")
	gadget := mkGit("gadget", "git@github.com:owner/gadget.git")
	stubborn := mkGit("stubborn", "")
	// widget under two more names: a symlink, and a clone with its origin.
	if err := os.Symlink("widget", filepath.Join(p, "gizmo")); err != nil {
		t.Fatal(err)
	}
	mkGit("widget-pr7", "https://github.com/owner/widget")
	notes := filepath.Join(p, "notes")
	os.MkdirAll(notes, 0o755)
	if code := runInit(nil); code != 0 {
		t.Fatalf("init %d", code)
	}
	s = &store.Store{Dir: filepath.Join(home, "store")}
	reg := filepath.Join(platform.StateDir(home), repoid.RegistryFile)
	var work []repoid.WorkItem
	add := func(scope, name, typ, session, desc string) {
		writeFactFile(t, s.Dir, &fact.Fact{Path: scope + "/" + name + ".md", Meta: fact.Meta{Name: name, Description: desc, Type: typ, Scope: scope,
			Source: fact.Source{Harness: "curator", Host: "box", Session: session, Kind: "observed"}, Created: "2026-10-10"}, Body: desc + ".\n"})
	}
	add("projects/widgit", "widgit-port", "machine_fact", "s-w1", "The widget dev server listens on port 7781")
	work = append(work, repoid.WorkItem{Root: widget, Session: "s-w1", By: "edit"})
	add("projects/old-gadget", "gadget-release", "design_decision", "s-g1", "Gadget releases go out from the release branch")
	work = append(work, repoid.WorkItem{Root: gadget, Session: "s-g1", By: "commit"})
	for i := 1; i <= 12; i++ {
		id := fmt.Sprintf("s-kv%d", i)
		add(fmt.Sprintf("projects/kv-%d-g1", i), fmt.Sprintf("kv-%d-task-note", i), "history", id, kvTopics[i-1])
		work = append(work, repoid.WorkItem{Root: widget, Session: id, By: "edit"})
	}
	// One more task folder whose session the registry never saw: only the
	// folders alike say where it belongs.
	add("projects/kv-13-g1", "kv-13-task-note", "history", "s-kv13", "The importer skips rows whose checksum column is empty")
	// A folder nothing places: the model leaves it for review.
	add("projects/puzzle", "puzzle-note", "history", "s-pz", "The puzzle solver caches boards by their canonical hash")
	add("projects/gizmo", "gizmo-flags", "design_decision", "s-z1", "Feature flags are read once at start from flags.toml")
	add("projects/widget-pr7", "pr7-review", "history", "s-z2", "The review of change seven asked for a smaller diff")
	work = append(work, repoid.WorkItem{Root: widget, Session: "s-z1", By: "edit"}, repoid.WorkItem{Root: filepath.Join(p, "widget-pr7"), Session: "s-z2", By: "edit"})
	add("projects/stubborn", "stubborn-note", "history", "s-st", "The stubborn tool keeps its cache in the build folder")
	work = append(work, repoid.WorkItem{Root: stubborn, Session: "s-st", By: "edit"})
	add("projects/notes", "notes-plan", "design_decision", "s-n1", "The notes folder keeps the launch plan in one page per quarter")
	work = append(work, repoid.WorkItem{Folder: notes, Session: "s-n1", By: "edit"})
	if err := repoid.RecordWorks(reg, work); err != nil {
		t.Fatal(err)
	}
	facts, _, _ := fact.Walk(s.Dir)
	writeIndexesForTest(t, s.Dir, facts)
	gitIn(t, s.Dir, "add", "-A")
	gitIn(t, s.Dir, "commit", "-q", "-m", "an older store")
	t.Cleanup(func(f func(string) bool) func() { return func() { hasKey = f } }(hasKey))
	return home, s, ghLog
}

// The checks end to end on a fresh store: a misnamed folder with no
// checkout, remote or forge repository is refused a status of its own, with
// the check's output, and folds into the repository its sessions worked in;
// the forge's rename folds the old name into the current one; twelve task
// worktree folders of one name pattern are batched together, the second
// batch shown the first's decisions, and all fold alike; an active status
// rests on the folder's own commits; a non-git project folder gets its roots
// from the evidence and its facts reach a session there; a folder refused in
// two runs on the same evidence waits until it changes; the forge is asked
// once per name (its answers are cached).
func TestChecksResolveFoldersFromEvidence(t *testing.T) {
	home, s, ghLog := checkedHome(t)
	m := &evidenceModel{stubborn: map[string]bool{"projects/stubborn": true}, review: map[string]bool{"projects/puzzle": true}}
	prev := assessRun
	assessRun = m.run
	t.Cleanup(func() { assessRun = prev })

	rep := &curate.Report{}
	if errs := assessStep(context.Background(), s, assessOpts{model: "opus"}, rep); errs != 0 {
		t.Fatalf("%d errors", errs)
	}
	recs, errs := record.Walk(s.Dir)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	for _, f := range []string{"projects/widgit", "projects/kv-1-g1", "projects/kv-7-g1", "projects/kv-12-g1"} {
		if r := recs[f]; r == nil || r.SuppressedBy != "projects/widget" {
			t.Errorf("%s: %+v", f, r)
		}
	}
	folded := 0
	for i := 1; i <= 12; i++ {
		if r := recs[fmt.Sprintf("projects/kv-%d-g1", i)]; r != nil && r.SuppressedBy == "projects/widget" {
			folded++
		}
	}
	if folded != 12 {
		t.Errorf("%d of 12 alike folders folded alike", folded)
	}
	if r := recs["projects/kv-13-g1"]; r == nil || r.SuppressedBy != "projects/widget" {
		t.Errorf("the alike folder with no evidence of its own: %+v", r)
	}
	if r := recs["projects/puzzle"]; r == nil || r.Status != "needs-review" {
		t.Errorf("puzzle: %+v", r)
	}
	if r := recs["projects/old-gadget"]; r == nil || r.SuppressedBy != "projects/gadget" {
		t.Errorf("old-gadget: %+v", r)
	}
	// One repository, one live folder: the symlink's name and the clone fold.
	for _, f := range []string{"projects/gizmo", "projects/widget-pr7"} {
		if r := recs[f]; r == nil || r.SuppressedBy != "projects/widget" {
			t.Errorf("%s: %+v", f, r)
		}
	}
	w := recs["projects/widget"]
	if w == nil || w.Status != "active" || !strings.Contains(w.Was[0].Why, "commit:") {
		t.Fatalf("widget active on its own commits: %+v", w)
	}
	n := recs["projects/notes"]
	if n == nil || n.Kind != "folder" || len(n.Roots) != 1 || n.Roots[0].Path != "~/projects/notes" || n.Roots[0].Host != store.ShortHost() {
		t.Fatalf("notes roots: %+v", n)
	}
	if recs["projects/stubborn"] != nil {
		t.Fatal("a refused decision was written")
	}
	// The record keeps what code checked, source, output and verdict.
	var forge *check.Result
	for _, e := range recs["projects/widgit"].Evidence {
		for i, c := range e.Checks {
			if c.Source == "forge:github" {
				forge = &e.Checks[i]
			}
		}
	}
	if forge == nil || forge.Verdict != check.Fails || !strings.Contains(forge.Output, "not found: owner/widgit") || forge.At == "" {
		t.Fatalf("the forge check in the evidence: %+v", forge)
	}
	all := strings.Join(m.prompts, "\n=====\n")
	for _, want := range []string{
		"check: repository widgit has a checkout: fails",
		"candidate real repositories (code's, each exists by its check): widget (1 of 1 session(s) that filed its notes worked in its checkout",
		"the name widgit exists nowhere as a repository",
		"was renamed: it is owner/gadget now",
		"decided before on", "suppressed into projects/widget",
		"check: gizmo and widget are one repository: holds", "have one origin, github.com/owner/widget",
		"is the same repository as projects/widget, which has a live record",
		"folder(s) of the same name pattern kv-#-g# are suppressed into it",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("the prompts lack %q", want)
		}
	}
	// The twelve alike folders: the second batch with them is shown the
	// decisions on the first's.
	var kvBatches []string
	for _, p := range m.prompts {
		if strings.Contains(p, "### folder=projects/kv-") && !strings.Contains(p, "CODE REFUSED") {
			kvBatches = append(kvBatches, p)
		}
	}
	if len(kvBatches) != 2 || !strings.Contains(kvBatches[1], "alike (the same name pattern kv-#-g#)") {
		t.Fatalf("%d batches with the alike folders; second:\n%s", len(kvBatches), kvBatches[len(kvBatches)-1])
	}
	if rep.CheckRefused == 0 || rep.AssessRefused != 1 {
		t.Fatalf("report %+v", rep)
	}

	// The migration moves the folded folders' facts to their winners.
	if _, err := migrateStep(context.Background(), s, migrateBatch, migrateCommits, false, nil); err != nil {
		t.Fatal(err)
	}
	facts, _, _ := fact.Walk(s.Dir)
	live := map[string]int{}
	for _, f := range facts {
		if f.Meta.SupersededBy == "" && f.Meta.Retired == "" && !f.InInbox() {
			live[f.Meta.Scope]++
		}
	}
	if live["projects/widget"] != 16 || live["projects/gadget"] != 1 || live["projects/widgit"] != 0 || live["projects/gizmo"] != 0 {
		t.Fatalf("live facts by folder: %v", live)
	}

	// The non-git folder's facts reach a session started there, by its roots.
	out := hookOut(t, startPayload(filepath.Join(home, "projects", "notes"), "r1"), "session-start")
	if !strings.Contains(out, "notes-plan") {
		t.Fatalf("the folder's fact is not delivered in its root:\n%s", out)
	}
	if out := hookOut(t, startPayload(filepath.Join(home, "elsewhere"), "r2"), "session-start"); strings.Contains(out, "notes-plan") {
		t.Fatalf("delivered outside the root:\n%s", out)
	}

	// The forge was asked once per name: a second run asks it nothing new.
	calls := func() int { b, _ := os.ReadFile(ghLog); return strings.Count(string(b), "api repos/") }
	before := calls()
	rep2 := &curate.Report{}
	assessStep(context.Background(), s, assessOpts{model: "opus"}, rep2)
	if c := calls(); c != before {
		t.Errorf("the second run asked the forge %d more time(s)", c-before)
	}
	// stubborn was refused in a second run on the same evidence; the third
	// run does not ask it again.
	if rep2.AssessRefused != 1 {
		t.Fatalf("run 2 %+v", rep2)
	}
	asked := len(m.prompts)
	rep3 := &curate.Report{}
	assessStep(context.Background(), s, assessOpts{model: "opus"}, rep3)
	if rep3.AssessHeld != 1 || rep3.AssessCandidates != 0 || len(m.prompts) != asked {
		t.Fatalf("run 3 asked the waiting folder: %+v, %d prompts", rep3, len(m.prompts)-asked)
	}
	// New evidence (a new fact in the folder) asks it again.
	writeFactFile(t, s.Dir, &fact.Fact{Path: "projects/puzzle/puzzle-two.md", Meta: fact.Meta{Name: "puzzle-two", Description: "The puzzle solver moved its cache to the shared disk",
		Type: "history", Scope: "projects/puzzle", Source: fact.Source{Harness: "curator", Host: "box", Session: "s-pz2", Kind: "observed"}, Created: "2026-10-12"}, Body: "y.\n"})
	writeFactFile(t, s.Dir, &fact.Fact{Path: "projects/stubborn/stubborn-two.md", Meta: fact.Meta{Name: "stubborn-two", Description: "The stubborn tool now reads its config from the repo root",
		Type: "history", Scope: "projects/stubborn", Source: fact.Source{Harness: "curator", Host: "box", Session: "s-st2", Kind: "observed"}, Created: "2026-10-12"}, Body: "x.\n"})
	facts, _, _ = fact.Walk(s.Dir)
	writeIndexesForTest(t, s.Dir, facts)
	gitIn(t, s.Dir, "add", "-A")
	gitIn(t, s.Dir, "commit", "-q", "-m", "a new fact")
	rep4 := &curate.Report{}
	assessStep(context.Background(), s, assessOpts{model: "opus"}, rep4)
	if rep4.AssessHeld != 0 || rep4.AssessCandidates != 2 {
		t.Fatalf("run 4 after new evidence: %+v", rep4)
	}
	if fs, err := lint.Run(s.Dir); err != nil || len(fs) != 0 {
		t.Fatalf("lint %v %v", fs, err)
	}
}
