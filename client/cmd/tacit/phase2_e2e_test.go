package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/assess"
	"github.com/agent-sh/tacit/client/internal/curate"
	"github.com/agent-sh/tacit/client/internal/dream"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/lint"
	"github.com/agent-sh/tacit/client/internal/overview"
	"github.com/agent-sh/tacit/client/internal/platform"
	"github.com/agent-sh/tacit/client/internal/record"
	"github.com/agent-sh/tacit/client/internal/store"
)

// toolSession writes one Claude Code session in cwd whose assistant calls
// tool (with a query) and edits file.
func toolSession(t *testing.T, home, cwd, id, text, tool, file string) string {
	t.Helper()
	f := filepath.Join(home, ".claude", "projects", "p", id+".jsonl")
	os.MkdirAll(filepath.Dir(f), 0o700)
	os.WriteFile(f, []byte(`{"type":"user","entrypoint":"cli","sessionId":"`+id+`","cwd":"`+filepath.ToSlash(cwd)+`","message":{"content":"`+text+`"}}`+"\n"+
		`{"type":"assistant","sessionId":"`+id+`","message":{"content":[{"type":"text","text":"Working on it: `+text+`."},{"type":"tool_use","name":"`+tool+`","input":{"query":"open items"}},{"type":"tool_use","name":"Edit","input":{"file_path":"`+filepath.ToSlash(file)+`"}}]}}`+"\n"+
		strings.Repeat(" ", 5000)+"\n"), 0o600)
	return f
}

// The fresh-store end to end of folder records, with generic names only: a
// machine with a work repository (commits under its git identity), an
// upstream clone (none), a symlinked checkout of the work repository whose
// name an older dreamer filed facts under, and a non-git project folder;
// three sessions, two of them using one MCP tool family, one another once.
// It runs dream, curate (split with area proposals, assess, migrate), and
// expects a record per folder with the upstream clone recorded as not a
// project, the alias folder suppressed and emptied with every old fact
// pointing at a live winner fact, an area from the two-session tool family
// and none from the one-session one, a PreToolUse push of the area's facts
// for its tool outside any repository (once a session), the rotation with
// K = 2 stamping the two oldest facts, an overview of the folder that grew
// to three facts whose every citation is live, and a second run making no
// commit.
func TestFreshStoreFolderRecordsEndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks and the shell CLI fixture")
	}
	home, _ := filepath.EvalSymlinks(t.TempDir())
	for k, v := range map[string]string{"HOME": home, "USERPROFILE": home, "TACIT_STATE_DIR": "", "XDG_CACHE_HOME": filepath.Join(home, "cache"),
		"TACIT_STORE": filepath.Join(home, "store"), "TACIT_INDEX": filepath.Join(home, "index"), "TACIT_LOCAL": filepath.Join(home, "overlay"),
		"TACIT_PROJECTS": filepath.Join(home, "projects"), "TACIT_LOCAL_LAYER": "off", "TACIT_HERMES_DB": "off", "TACIT_SETTINGS": "",
		"TACIT_DREAM_MAX_RUNTIME": "", "TACIT_DREAM_RUNNER": "claude", "TACIT_CURATE_RUNNER": "claude", "TACIT_JUDGE_RUNNER": "claude",
		"GIT_CONFIG_GLOBAL": filepath.Join(home, "gitconfig"), "GIT_CONFIG_NOSYSTEM": "1"} {
		t.Setenv(k, v)
	}
	stubScrump(t)
	os.WriteFile(filepath.Join(home, "gitconfig"), []byte("[user]\n\temail = owner@example.com\n\tname = Owner\n"), 0o644)
	p := filepath.Join(home, "projects")
	mkRepo := func(name, author string, n int) string {
		dir := filepath.Join(p, name)
		os.MkdirAll(dir, 0o755)
		gitIn(t, dir, "init", "-q", "-b", "main")
		for i := 0; i < n; i++ {
			os.WriteFile(filepath.Join(dir, "main.go"), []byte{byte('a' + i)}, 0o644)
			gitIn(t, dir, "add", "main.go")
			cmd := exec.Command("git", "-c", "user.email="+author, "-c", "user.name=x", "commit", "-q", "-m", "c")
			cmd.Dir = dir
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("commit: %v %s", err, out)
			}
		}
		return dir
	}
	widget := mkRepo("widget", "owner@example.com", 3)
	mkRepo("upstream", "maintainer@upstream.example", 2)
	if err := os.Symlink("widget", filepath.Join(p, "widget-old")); err != nil {
		t.Fatal(err)
	}
	notes := filepath.Join(p, "notes-folder")
	os.MkdirAll(notes, 0o755)
	if code := runInit(nil); code != 0 {
		t.Fatalf("init %d", code)
	}
	s := &store.Store{Dir: filepath.Join(home, "store")}
	// What an older dreamer left: facts under the symlink's name and the
	// non-git folder's.
	for _, f := range []*fact.Fact{
		{Meta: fact.Meta{Name: "widget-old-build", Description: "Widget builds with make release from the repository root", Type: "design_decision", Scope: "projects/widget-old", AppliesTo: fact.AppliesTo{Repos: []string{"widget-old"}}}, Body: "make release.\n"},
		{Meta: fact.Meta{Name: "widget-old-port", Description: "The widget dev server listens on port 7781", Type: "machine_fact", Scope: "projects/widget-old"}, Body: "Port 7781.\n"},
		{Meta: fact.Meta{Name: "notes-plan", Description: "The notes folder holds the launch plan drafts", Type: "history", Scope: "projects/notes-folder"}, Body: "Drafts.\n"},
	} {
		f.Path = f.Meta.Scope + "/" + f.Meta.Name + ".md"
		f.Meta.Source = fact.Source{Harness: "curator", Host: "box", Kind: "observed", Session: "s0"}
		f.Meta.Created = "2026-10-01"
		writeFactFile(t, s.Dir, f)
	}
	facts, _, _ := fact.Walk(s.Dir)
	writeIndexesForTest(t, s.Dir, facts)
	gitIn(t, s.Dir, "add", "-A")
	gitIn(t, s.Dir, "commit", "-q", "-m", "an older store")

	// Three sessions: two use the tracker tools in the work repository (one
	// through the symlink), one the pager tool once in the notes folder.
	f1 := toolSession(t, home, filepath.Join(p, "widget-old"), "s1", "ALPHA triage the tracker queue for widget", "mcp__tracker__TicketRead", filepath.Join(p, "widget-old", "main.go"))
	f2 := toolSession(t, home, widget, "s2", "BETA label the tracker tickets for widget", "mcp__tracker__TicketList", filepath.Join(widget, "main.go"))
	f3 := toolSession(t, home, notes, "s3", "GAMMA page the on-call about the notes", "mcp__pager__PageSend", filepath.Join(notes, "plan.md"))
	if err := dream.SaveState(filepath.Join(platform.StateDir(home), "dreamer.json"), &dream.State{Offsets: map[string]int64{f1: 0, f2: 0, f3: 0}}); err != nil {
		t.Fatal(err)
	}
	prevDream := dreamRun
	t.Cleanup(func() { dreamRun = prevDream })
	dreamRun = func(_ context.Context, _, prompt string) (string, float64, int, error) {
		switch {
		case strings.Contains(prompt, "ALPHA"):
			return `[{"description":"The widget tracker queue is triaged every morning","type":"design_decision","kind":"observed","applies_to":{}}]`, .01, 10, nil
		case strings.Contains(prompt, "BETA"):
			return `[{"description":"Widget tracker tickets need a component label before triage","type":"design_decision","kind":"observed","applies_to":{}}]`, .01, 10, nil
		case strings.Contains(prompt, "GAMMA"):
			return `[{"description":"The on-call is paged through the pager for notes outages","type":"design_decision","kind":"observed","applies_to":{}}]`, .01, 10, nil
		}
		return `[]`, .01, 10, nil
	}
	t.Cleanup(func(f func(string) bool) func() { return func() { hasKey = f } }(hasKey))
	if code := runDream(nil); code != 0 {
		t.Fatalf("dream exit %d", code)
	}
	u := dream.LoadToolUsage(filepath.Join(platform.StateDir(home), dream.ToolsFile))
	if !u.Used("s1", "mcp__tracker__*") || !u.Used("s2", "mcp__tracker__*") || !u.Used("s3", "mcp__pager__*") {
		t.Fatalf("tool usage %+v", u.Sessions)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "tools.json")); err == nil {
		t.Fatal("tool usage in the store")
	}

	// The curator: the split files the notes and proposes areas.
	facts, _, _ = fact.Walk(s.Dir)
	noteOf := map[string]string{}
	for _, f := range facts {
		if f.InInbox() {
			noteOf[f.Meta.Source.Session] = f.Meta.Name
		}
	}
	if len(noteOf) != 3 {
		t.Fatalf("dreamed notes %v", noteOf)
	}
	tracker := map[string]any{"kind": "area", "name": "tracker", "description": "The ticket tracker and its triage"}
	pager := map[string]any{"kind": "area", "name": "pager", "description": "Paging the on-call"}
	e1 := entry(noteOf["s1"], "tracker-triage-morning", "The widget tracker queue is triaged every morning", "design_decision", "projects/widget")
	e1["propose"] = tracker
	e2 := entry(noteOf["s2"], "tracker-component-label", "Widget tracker tickets need a component label before triage", "design_decision", "projects/widget")
	e2["propose"] = tracker
	e3 := entry(noteOf["s3"], "pager-notes-outages", "The on-call is paged through the pager for notes outages", "design_decision", "user")
	e3["propose"] = pager
	b, _ := json.Marshal([]map[string]any{e1, e2, e3})
	r, _ := json.Marshal(map[string]any{"result": string(b), "total_cost_usd": .01})
	t.Setenv("FIXTURE_SPLIT", string(r))
	fake := filepath.Join(home, "fake-cli")
	os.WriteFile(fake, []byte("#!/bin/sh\nin=$(cat)\ncase \"$in\" in\n*\"OWNER RULINGS already extracted\"*) printf '%s\\n' '{\"result\":\"[]\",\"total_cost_usd\":0.01}' ;;\n*) printf '%s\\n' \"$FIXTURE_SPLIT\" ;;\nesac\n"), 0o700)
	t.Setenv("TACIT_CLAUDE_COMMAND", fake)
	prevJudge := judgeRun
	t.Cleanup(func() { judgeRun = prevJudge })
	judgeRun = func(_ context.Context, _, prompt string) (string, float64, error) {
		n := strings.Count(prompt, "\nEXISTING (")
		out := make([]string, n)
		for i := range out {
			out[i] = "no"
		}
		b, _ := json.Marshal(out)
		return string(b), .01, nil
	}
	prevOv := overviewRun
	t.Cleanup(func() { overviewRun = prevOv })
	overviewRun = func(_ context.Context, _, prompt string) (string, float64, error) {
		var lines []string
		for _, m := range promptFactRx.FindAllStringSubmatch(prompt, -1) {
			lines = append(lines, "- A fact of the folder [["+m[1]+"]].")
		}
		return strings.Join(lines, "\n"), 0.01, nil
	}
	prevUpkeep, prevStale := curateUpkeep, curateStaleStep
	t.Cleanup(func() { curateUpkeep, curateStaleStep = prevUpkeep, prevStale })
	curateUpkeep = func(context.Context, *store.Store, string, bool, *curate.Report) {}
	curateStaleStep = func(context.Context, *store.Store, bool) int { return 0 }
	area := assess.Decision{Folder: "areas/tracker", Kind: "area", Status: "active", Why: "two sessions triage and label tracker tickets: session:s1, session:s2", Description: "The ticket tracker and its triage", Tools: []string{"mcp__tracker__*"}}
	old := aDecision("projects/widget-old", "repo", "active", "the symlink's name of the widget checkout; its facts came from session:s0")
	old.SuppressedBy = "projects/widget"
	fa := &fakeAssessor{first: map[string]assess.Decision{
		"projects/widget":       aDecision("projects/widget", "repo", "active", "edits and commits from session:s1 and session:s2"),
		"projects/upstream":     {Folder: "projects/upstream", Kind: "none", Status: "dead", Why: "no commit of the owner's and no session worked there; only session:s0's notes elsewhere", Description: "an upstream clone"},
		"projects/widget-old":   old,
		"projects/notes-folder": aDecision("projects/notes-folder", "folder", "active", "launch drafts edited in session:s3"),
		"areas/tracker":         area,
		"areas/pager":           {Folder: "areas/pager", Kind: "area", Status: "active", Why: "session:s3", Description: "paging"},
	}}
	fa.install(t)
	if code := runCurate(nil); code != 0 {
		t.Fatalf("curate exit %d", code)
	}
	for _, p := range fa.prompts {
		if strings.Contains(p, "folder=areas/pager") {
			t.Error("a one-session proposal was put to the model")
		}
	}

	recs, errs := record.Walk(s.Dir)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	for _, f := range []string{"projects/widget", "projects/upstream", "projects/widget-old", "projects/notes-folder", "areas/tracker"} {
		if recs[f] == nil {
			t.Errorf("no record for %s", f)
		}
	}
	if recs["areas/pager"] != nil {
		t.Error("an area from one session")
	}
	if up := recs["projects/upstream"]; up == nil || up.Status != record.StatusDead || !strings.HasPrefix(up.Was[0].Why, assess.NotProject) {
		t.Errorf("the upstream clone is not recorded as no project: %+v", up)
	}
	if w := recs["projects/widget"]; w == nil || w.Evidence[0].OwnCommits == nil || *w.Evidence[0].OwnCommits != 3 || w.Evidence[0].WorkSessions != 2 {
		t.Errorf("widget's evidence: %+v", w)
	}
	if a := recs["areas/tracker"]; a == nil || a.Tools[0] != "mcp__tracker__*" || len(a.Sessions()) != 2 {
		t.Errorf("tracker %+v", a)
	}
	byName := factsByName(t, s.Dir)
	for _, f := range byName {
		if f.Meta.Scope != "projects/widget-old" || f.InInbox() {
			continue
		}
		w := byName[f.Meta.SupersededBy]
		if w == nil || w.Meta.Scope != "projects/widget" || w.Meta.SupersededBy != "" || w.Meta.Retired != "" {
			t.Errorf("%s in the suppressed folder points at %q", f.Meta.Name, f.Meta.SupersededBy)
		}
	}
	inArea := 0
	for _, f := range byName {
		if f.Meta.Scope == "areas/tracker" && f.Meta.SupersededBy == "" {
			inArea++
		}
	}
	if inArea != 2 {
		t.Errorf("%d facts in the new area, want the 2 its proposals came from", inArea)
	}
	log := readFolderLog(s.Dir)
	closed := closedFolderIDs(log)
	pagerOpen := false
	for _, e := range log {
		if e.Kind == "propose" && e.Name == "pager" && closed[e.ID] == "" {
			pagerOpen = true
		}
		if e.Kind == "propose" && e.Name == "tracker" && closed[e.ID] == "" {
			t.Error("the tracker proposals are not closed")
		}
	}
	if !pagerOpen {
		t.Error("the one-session proposal does not wait")
	}
	if fs, err := lint.Run(s.Dir); err != nil || len(fs) != 0 {
		t.Fatalf("lint %v %v", fs, err)
	}
	if !strings.Contains(gitIn(t, s.Dir, "log", "--format=%s"), "curator: migrate: projects/widget-old -> projects/widget") {
		t.Error("no migrate commit")
	}

	// The area's tool, outside any repository: its facts before the call,
	// once in the session.
	push := hookOut(t, toolPayload(home, "x1", "mcp__tracker__TicketRead", map[string]any{"q": "open"}), "pre-tool")
	if !strings.Contains(push, "tracker-triage-morning") || !strings.Contains(push, "tacit_tree areas/tracker") {
		t.Fatalf("no area push outside a repo:\n%s", push)
	}
	if again := hookOut(t, toolPayload(home, "x1", "mcp__tracker__TicketList", map[string]any{"q": "open"}), "pre-tool"); again != "" {
		t.Fatalf("pushed twice:\n%s", again)
	}
	if other := hookOut(t, toolPayload(home, "x2", "mcp__pager__PageSend", nil), "pre-tool"); other != "" {
		t.Fatalf("a tool of no area pushed:\n%s", other)
	}

	// The rotation with K = 2: the two oldest live curated facts are stamped.
	var live []*fact.Fact
	for _, f := range factsByName(t, s.Dir) {
		if !f.InInbox() && f.Meta.SupersededBy == "" && f.Meta.Retired == "" && !strings.HasPrefix(f.Meta.Scope, "derived/") {
			live = append(live, f)
		}
	}
	sort.Slice(live, func(i, j int) bool {
		if live[i].Meta.Created != live[j].Meta.Created {
			return live[i].Meta.Created < live[j].Meta.Created
		}
		return live[i].Meta.Name < live[j].Meta.Name
	})
	prevRot := rotationRun
	t.Cleanup(func() { rotationRun = prevRot })
	rotationRun = func(_ context.Context, _, prompt string) (string, float64, error) {
		var out []string
		for _, m := range regexp.MustCompile(`"name":"([a-z0-9-]+)"`).FindAllStringSubmatch(prompt, -1) {
			out = append(out, `{"name":"`+m[1]+`","outcome":"still"}`)
		}
		return "[" + strings.Join(out, ",") + "]", 0.01, nil
	}
	if code := runRotation([]string{"--max", "2"}); code != 0 {
		t.Fatalf("rotation exit %d", code)
	}
	today := time.Now().Format("2006-01-02")
	byName = factsByName(t, s.Dir)
	for i, f := range live {
		if got := byName[f.Meta.Name].Meta.LastChecked; (i < 2) != (got == today) {
			t.Errorf("%s (%d oldest): last_checked %q", f.Meta.Name, i+1, got)
		}
	}

	// A third fact in the widget folder: the next full run writes its
	// overview, whose every citation resolves.
	writeFactFile(t, s.Dir, &fact.Fact{Path: "projects/widget/widget-release-notes.md", Meta: fact.Meta{Name: "widget-release-notes", Description: "Widget release notes go in CHANGES.md", Type: "user_standard", Scope: "projects/widget",
		Source: fact.Source{Harness: "curator", Host: "box", Kind: "observed", Session: "s2"}, Created: today}, Body: "CHANGES.md.\n"})
	facts, _, _ = fact.Walk(s.Dir)
	writeIndexesForTest(t, s.Dir, facts)
	gitIn(t, s.Dir, "add", "-A")
	gitIn(t, s.Dir, "commit", "-q", "-m", "a third widget fact")
	if code := runCurate(nil); code != 0 {
		t.Fatalf("curate with an overview exit %d", code)
	}
	ob, err := os.ReadFile(filepath.Join(s.Dir, "projects", "widget", overview.File))
	if err != nil {
		t.Fatalf("no overview of widget: %v", err)
	}
	o, err := overview.Parse(ob)
	if err != nil {
		t.Fatal(err)
	}
	byName = factsByName(t, s.Dir)
	for _, c := range overview.Cites(o.Body) {
		if f := byName[c]; f == nil || f.Meta.SupersededBy != "" || f.Meta.Retired != "" {
			t.Errorf("the overview cites %s, no live fact", c)
		}
	}
	if len(overview.Cites(o.Body)) < 3 {
		t.Errorf("the overview cites %v", overview.Cites(o.Body))
	}
	if fs, err := lint.Run(s.Dir); err != nil || len(fs) != 0 {
		t.Fatalf("lint after the overview %v %v", fs, err)
	}

	// A second full run makes no commit.
	head := gitIn(t, s.Dir, "rev-parse", "HEAD")
	n := len(fa.prompts)
	if code := runCurate(nil); code != 0 {
		t.Fatalf("second curate exit %d", code)
	}
	if gitIn(t, s.Dir, "rev-parse", "HEAD") != head || len(fa.prompts) != n {
		t.Fatalf("the second run wrote %s or asked %d more", gitIn(t, s.Dir, "log", "--format=%s", head+"..HEAD"), len(fa.prompts)-n)
	}
	if _, ok, _ := ledger.ReadFormat(s.Dir); !ok {
		t.Fatal("format marker gone")
	}
}
