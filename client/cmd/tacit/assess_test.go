package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/assess"
	"github.com/agent-sh/tacit/client/internal/curate"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/lint"
	"github.com/agent-sh/tacit/client/internal/platform"
	"github.com/agent-sh/tacit/client/internal/record"
	"github.com/agent-sh/tacit/client/internal/repoid"
	"github.com/agent-sh/tacit/client/internal/store"
)

// fakeAssessor answers assessment prompts from decisions by folder; a
// retry prompt gets the fixed decision when there is one. Every prompt is
// kept.
type fakeAssessor struct {
	first, fixed map[string]assess.Decision
	prompts      []string
}

var folderRx = regexp.MustCompile(`(?m)^### folder=(\S+)$`)

func (f *fakeAssessor) run(_ context.Context, _, prompt string) (string, float64, error) {
	f.prompts = append(f.prompts, prompt)
	var out []assess.Decision
	if i := strings.Index(prompt, "CODE REFUSED THESE DECISIONS"); i >= 0 {
		for _, l := range strings.Split(prompt[i:], "\n") {
			if folder, _, ok := strings.Cut(strings.TrimPrefix(l, "- "), ": "); ok && strings.HasPrefix(l, "- ") {
				if d, ok := f.fixed[folder]; ok {
					out = append(out, d)
				} else if d, ok := f.first[folder]; ok {
					out = append(out, d)
				}
			}
		}
	} else {
		for _, m := range folderRx.FindAllStringSubmatch(prompt, -1) {
			if d, ok := f.first[m[1]]; ok {
				out = append(out, d)
			}
		}
	}
	b, _ := json.Marshal(out)
	return string(b), 0.25, nil
}

func (f *fakeAssessor) install(t *testing.T) {
	t.Helper()
	prev := assessRun
	t.Cleanup(func() { assessRun = prev })
	assessRun = f.run
}

// assessHome is a machine with a fresh local store and, in its projects
// directory: widget, a repository with commits under this machine's git
// identity and work from two sessions; upstream, a clone with no commit of
// its own and no work; widget-old, a symlink to widget; notes, a folder
// that is no repository. The store holds facts in projects/widget-old and
// projects/notes.
func assessHome(t *testing.T) (home string, s *store.Store) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	home, _ = filepath.EvalSymlinks(t.TempDir())
	for k, v := range map[string]string{"HOME": home, "TACIT_STATE_DIR": "", "XDG_CACHE_HOME": filepath.Join(home, "cache"),
		"TACIT_STORE": filepath.Join(home, "store"), "TACIT_INDEX": filepath.Join(home, "index"), "TACIT_LOCAL": filepath.Join(home, "overlay"),
		"TACIT_PROJECTS": filepath.Join(home, "projects"), "GIT_CONFIG_GLOBAL": filepath.Join(home, "gitconfig"), "GIT_CONFIG_NOSYSTEM": "1"} {
		t.Setenv(k, v)
	}
	stubScrump(t)
	os.WriteFile(filepath.Join(home, "gitconfig"), []byte("[user]\n\temail = owner@example.com\n\tname = Owner\n"), 0o644)
	p := filepath.Join(home, "projects")
	mkGit := func(name, author string, n int) {
		dir := filepath.Join(p, name)
		os.MkdirAll(dir, 0o755)
		gitIn(t, dir, "init", "-q", "-b", "main")
		for i := 0; i < n; i++ {
			os.WriteFile(filepath.Join(dir, "f.txt"), []byte{byte('a' + i)}, 0o644)
			cmd := exec.Command("git", "-c", "user.email="+author, "-c", "user.name=x", "commit", "-q", "-a", "-m", "c", "--allow-empty")
			if i == 0 {
				gitIn(t, dir, "add", "f.txt")
			}
			cmd.Dir = dir
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("commit: %v %s", err, out)
			}
		}
	}
	mkGit("widget", "owner@example.com", 3)
	mkGit("upstream", "someone@upstream.example", 2)
	if err := os.Symlink("widget", filepath.Join(p, "widget-old")); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(p, "notes"), 0o755)
	if code := runInit(nil); code != 0 {
		t.Fatalf("init %d", code)
	}
	s = &store.Store{Dir: filepath.Join(home, "store")}
	// The dreamer's registry: widget seen through the symlink, two sessions' work.
	reg := filepath.Join(platform.StateDir(home), repoid.RegistryFile)
	r, _ := repoid.Of(filepath.Join(p, "widget-old"), home)
	if err := repoid.Record(reg, r); err != nil {
		t.Fatal(err)
	}
	repoid.RecordWorks(reg, []repoid.WorkItem{{Root: r.Root, Session: "s-one", By: "edit"}, {Root: r.Root, Session: "s-two", By: "commit"}, {Folder: filepath.Join(p, "notes"), Session: "s-three", By: "edit"}})
	for _, f := range []*fact.Fact{
		{Path: "projects/widget-old/old-port.md", Meta: fact.Meta{Name: "old-port", Description: "The widget server listens on port 7781", Type: "machine_fact", Scope: "projects/widget-old", Source: fact.Source{Harness: "curator", Host: "box", Session: "s-one", Kind: "observed"}, Created: "2026-10-10"}, Body: "Port 7781.\n"},
		{Path: "projects/notes/notes-plan.md", Meta: fact.Meta{Name: "notes-plan", Description: "The notes folder holds the launch plan drafts", Type: "history", Scope: "projects/notes", Source: fact.Source{Harness: "curator", Host: "box", Session: "s-three", Kind: "observed"}, Created: "2026-10-11"}, Body: "Drafts.\n"},
	} {
		writeFactFile(t, s.Dir, f)
	}
	facts, _, _ := fact.Walk(s.Dir)
	writeIndexesForTest(t, s.Dir, facts)
	gitIn(t, s.Dir, "add", "-A")
	gitIn(t, s.Dir, "commit", "-q", "-m", "facts")
	t.Cleanup(func(f func(string) bool) func() { return func() { hasKey = f } }(hasKey))
	return home, s
}

func aDecision(folder, kind, status, why string) assess.Decision {
	return assess.Decision{Folder: folder, Kind: kind, Status: status, Why: why, Description: "what " + folder + " is"}
}

// independent is d citing independence evidence (keys code may list).
func independent(d assess.Decision, keys ...string) assess.Decision {
	d.Independent = keys
	return d
}

// The assessment asks the model about every repository and project folder
// with no record, shows it code's evidence, sends a refused decision back
// once, writes the records in one commit, and a second run asks nothing.
func TestAssessDecidesFoldersOnCodesEvidence(t *testing.T) {
	_, s := assessHome(t)
	fa := &fakeAssessor{
		first: map[string]assess.Decision{
			"projects/widget":   aDecision("projects/widget", "repo", "active", "worked in session:s-one and session:s-two"),
			"projects/upstream": aDecision("projects/upstream", "none", "dead", "an upstream clone: no own commits, no work"),
			"projects/notes":    aDecision("projects/notes", "folder", "active", "the launch drafts, last 2026-10-11"),
		},
		fixed: map[string]assess.Decision{
			"projects/notes": aDecision("projects/notes", "folder", "active", "the launch drafts, session:s-three"),
		},
	}
	wo := aDecision("projects/widget-old", "repo", "active", "the symlink's name of widget, session:s-one")
	wo.SuppressedBy = "projects/widget"
	fa.first["projects/widget-old"] = wo
	fa.first["projects/upstream"] = assess.Decision{Folder: "projects/upstream", Kind: "none", Status: "dead", Why: "no own commits, no work sessions; nothing filed by session:s-one or session:s-two names it", Description: "an upstream clone"}
	fa.install(t)
	head := gitIn(t, s.Dir, "rev-parse", "HEAD")
	rep := &curate.Report{}
	if errs := assessStep(context.Background(), s, assessOpts{model: "opus"}, rep); errs != 0 {
		t.Fatalf("%d errors", errs)
	}
	if rep.AssessCandidates != 4 || rep.AssessRecords != 4 || rep.AssessRefused != 0 || rep.Retries != 1 {
		t.Fatalf("report %+v", rep)
	}
	p := fa.prompts[0]
	for _, want := range []string{"### folder=projects/widget\n", "git_root true; checkout main", "own_commits 3", "work_sessions 2", "### folder=projects/upstream", "own_commits 0", "upstream_clone", "repoid says widget-old is another name of the checkout widget", "sessions whose notes are filed here (newest first): session:s-one", "those sessions worked in (the dreamer's registry): widget (1 of 1)", "paths ~/projects/notes"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	if i, j := strings.Index(p, "folder=projects/widget\n"), strings.Index(p, "folder=projects/widget-old"); i < 0 || j < i {
		t.Error("the alias is asked before its repository")
	}
	if len(fa.prompts) != 2 || !strings.Contains(fa.prompts[1], "projects/notes: was[0].why: cites no session") {
		t.Fatalf("retry: %d prompts\n%s", len(fa.prompts), fa.prompts[len(fa.prompts)-1])
	}
	recs, errs := record.Walk(s.Dir)
	if len(errs) != 0 || len(recs) != 4 {
		t.Fatalf("records %v %v", recs, errs)
	}
	w := recs["projects/widget"]
	if w.Kind != "repo" || w.Status != "active" || w.CreatedBy != "opus" || len(w.Evidence) == 0 || w.Evidence[0].OwnCommits == nil || *w.Evidence[0].OwnCommits != 3 || w.Evidence[0].WorkSessions != 2 {
		t.Errorf("widget %+v", w)
	}
	u := recs["projects/upstream"]
	if u.Kind != "repo" || u.Status != "dead" || !strings.HasPrefix(u.Was[0].Why, assess.NotProject) || !u.Evidence[0].UpstreamClone {
		t.Errorf("upstream is not recorded as no project: %+v %+v", u, u.Evidence)
	}
	if recs["projects/widget-old"].SuppressedBy != "projects/widget" || recs["projects/notes"].Kind != "folder" {
		t.Errorf("widget-old %+v notes %+v", recs["projects/widget-old"], recs["projects/notes"])
	}
	if n := gitIn(t, s.Dir, "rev-list", "--count", head+"..HEAD"); n != "1" {
		t.Errorf("%s commits, want one per batch", n)
	}
	if msg := gitIn(t, s.Dir, "log", "-1", "--format=%s"); msg != "curator: assess: 4 folder(s): 1 folder, 1 not a project, 2 repo" {
		t.Errorf("message %q", msg)
	}
	if fs, err := lint.Run(s.Dir); err != nil || len(fs) != 0 {
		t.Fatalf("lint %v %v", fs, err)
	}
	idx, _ := os.ReadFile(filepath.Join(s.Dir, "projects", "_index.md"))
	if !strings.Contains(string(idx), "widget-old/ -> projects/widget (suppressed)") || !strings.Contains(string(idx), "[upstream/](upstream/_index.md) repo, dead") {
		t.Errorf("projects index:\n%s", idx)
	}
	// A second run: nothing to decide, no model call, no commit.
	again := gitIn(t, s.Dir, "rev-parse", "HEAD")
	n := len(fa.prompts)
	if errs := assessStep(context.Background(), s, assessOpts{model: "opus"}, &curate.Report{}); errs != 0 || len(fa.prompts) != n || gitIn(t, s.Dir, "rev-parse", "HEAD") != again {
		t.Fatalf("second run: %d errors, %d prompts, head moved: %v", errs, len(fa.prompts)-n, gitIn(t, s.Dir, "rev-parse", "HEAD") != again)
	}
}

// A decision refused twice is left for the next run; an owner's status is
// only flagged by the model; and nothing is written without the marker.
func TestAssessRefusesTwiceAndKeepsTheOwnersStatus(t *testing.T) {
	_, s := assessHome(t)
	bad := aDecision("projects/widget", "repo", "active", "the last commit was yesterday")
	fa := &fakeAssessor{first: map[string]assess.Decision{"projects/widget": bad}}
	fa.install(t)
	rep := &curate.Report{}
	assessStep(context.Background(), s, assessOpts{model: "opus", only: map[string]bool{"projects/widget": true}}, rep)
	if rep.AssessRefused != 1 || rep.AssessRecords != 0 {
		t.Fatalf("report %+v", rep)
	}
	if recs, _ := record.Walk(s.Dir); len(recs) != 0 {
		t.Fatalf("a refused decision was written: %v", recs)
	}
	// The owner's status on a record: Opus may only flag it for review.
	own := testRec(record.KindRepo, "projects/widget")
	own.Status, own.Was[0].Status = record.StatusShipped, record.StatusShipped
	own.Evidence = []record.Evidence{{Host: store.ShortHost(), Gathered: "2026-01-01", GitRoot: true, WorkSessions: 0}}
	putRecordFile(t, s.Dir, own)
	if out, err := exec.Command("git", "-C", s.Dir, "add", "-A").CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	gitIn(t, s.Dir, "commit", "-q", "-m", "owner record")
	c := assess.Candidate{Folder: "projects/widget", Old: own, GitRoot: true}
	a, err := newAssessor(context.Background(), s, false)
	if err != nil {
		t.Fatal(err)
	}
	var lines []byte
	acc, refused := a.check([]assess.Candidate{c}, []assess.Decision{aDecision("projects/widget", "repo", "active", "new work in session:s-one")}, map[string]*record.Record{}, &lines)
	if len(acc) != 0 || len(refused) != 1 || !strings.Contains(strings.Join(refused[0].Problems, " "), "the owner set this folder's status") {
		t.Fatalf("acc %v refused %+v", acc, refused)
	}
	acc, _ = a.check([]assess.Candidate{c}, []assess.Decision{aDecision("projects/widget", "repo", "needs-review", "new work in session:s-one")}, map[string]*record.Record{}, &lines)
	if len(acc) != 1 || acc[0].rec.Was[1].Status != record.StatusShipped {
		t.Fatalf("flag: %+v", acc)
	}
}

func TestAssessNeedsTheFormatMarker(t *testing.T) {
	_, s := assessHome(t)
	gitIn(t, s.Dir, "rm", "-q", "review/format.json")
	gitIn(t, s.Dir, "commit", "-q", "-m", "older store")
	fa := &fakeAssessor{}
	fa.install(t)
	if code := runAssess(nil); code != 2 || len(fa.prompts) != 0 {
		t.Fatalf("exit %d, %d prompts", code, len(fa.prompts))
	}
	if errs := curateAssessStep(context.Background(), s, "opus", false, &curate.Report{}); errs != 0 || len(fa.prompts) != 0 {
		t.Fatal("the curate step asked without the marker")
	}
}

// The owner's own words about a folder, found in their turn, make the
// decision theirs: the entry is by owner with the quote, and the quote
// ledger records it. Words that only mention the folder stay evidence.
func TestAssessOwnerWordsAboutTheFolder(t *testing.T) {
	home, s := assessHome(t)
	tr := filepath.Join(home, ".claude", "projects", "-home-u", "s9.jsonl")
	os.MkdirAll(filepath.Dir(tr), 0o700)
	os.WriteFile(tr, []byte(`{"type":"user","entrypoint":"cli","sessionId":"s9","message":{"content":"the real name of widget-old is widget, fold it into widget for good"}}`+"\n"), 0o600)
	d := aDecision("projects/widget", "repo", "active", "worked in session:s-one")
	wo := aDecision("projects/widget-old", "repo", "active", "the owner said so in claude:s9")
	wo.SuppressedBy, wo.OwnerQuote, wo.OwnerQuoteFrom = "projects/widget", "the real name of widget-old is widget, fold it into widget for good", "claude:s9"
	fa := &fakeAssessor{first: map[string]assess.Decision{"projects/widget": d, "projects/widget-old": wo}}
	fa.install(t)
	rep := &curate.Report{}
	if errs := assessStep(context.Background(), s, assessOpts{model: "opus", only: map[string]bool{"projects/widget": true, "projects/widget-old": true}}, rep); errs != 0 || rep.AssessRecords != 2 {
		t.Fatalf("errs %d report %+v", errs, rep)
	}
	if !strings.Contains(fa.prompts[0], `owner's turn (claude:s9): "the real name of widget-old is widget`) {
		t.Errorf("the owner's turn is not shown:\n%s", fa.prompts[0])
	}
	recs, _ := record.Walk(s.Dir)
	r := recs["projects/widget-old"]
	if r.Was[0].By != record.ByOwner || r.CreatedBy != record.ByOwner || r.Was[0].QuoteFrom != "claude:s9" || r.SuppressedBy != "projects/widget" {
		t.Fatalf("the owner's decision: %+v", r)
	}
	ql, _ := os.ReadFile(filepath.Join(s.Dir, "review", "owner-quotes.jsonl"))
	if !strings.Contains(string(ql), `"name":"projects/widget-old/_record"`) {
		t.Fatalf("ledger:\n%s", ql)
	}
	if fs, err := lint.Run(s.Dir); err != nil || len(fs) != 0 {
		t.Fatalf("lint %v %v", fs, err)
	}
}
