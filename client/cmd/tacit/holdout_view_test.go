package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/agentreview"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/platform"
)

// deniedName and deniedText are the denied fact's name and the words of
// its description that no other fact holds.
const (
	deniedName = "demo-zebra-denied"
	deniedText = "zebra vortex compactor"
)

// The structural test of the session view (SPEC.md, Hold-outs): a fact a
// session is denied does not exist for it, on every path that shows the
// store. Each row runs one path in a session with the denial and in a
// session without it: the control must show the fact (so the row reaches
// it), the denied session must show neither its name nor its words. A new
// path that shows facts gets a row here, and fails unless it reads the
// store through the session's view. tacit_fact asked for the fact by name is
// the one exception: the agent's own pull is served (TestHoldOutFactByName).
func TestHoldOutViewOnEveryPath(t *testing.T) {
	r, home, payload := restartRig(t)
	cwd := filepath.Join(r.projects, "demo")
	writeFact(t, r.clone, fact.Meta{Name: deniedName, Description: "the " + deniedText + " in demo flushes every quarter hour", Type: "design_decision", Scope: "projects/demo",
		AppliesTo: fact.AppliesTo{Commands: []string{"make zebra"}}}, "zebra")
	writeFact(t, r.clone, fact.Meta{Name: "demo-yak-loader", Description: "the yak loader in demo reads its table once at boot", Type: "design_decision", Scope: "projects/demo",
		ConflictsWith: []string{deniedName}}, "yak")
	writeFact(t, r.clone, fact.Meta{Name: "demo-old-flusher", Description: "the old flusher in demo ran hourly", Type: "design_decision", Scope: "projects/demo"}, "old")
	os.WriteFile(filepath.Join(r.clone, "projects", "demo", "_overview.md"), []byte("The demo folder flushes with the "+deniedText+" [["+deniedName+"]].\nIts notes start at [[demo-note]].\n"), 0o644)
	r.commitIndex(r.clone, "the denied fact and its neighbours")
	gitIn(t, r.clone, "push", "-q", "origin", "HEAD:main")
	buildIndexForHooks(r.s)

	// The reviewer's question about the denied note, in the demo repository.
	q := &agentreview.Queue{}
	q.Add(agentreview.Item{Note: "inbox/claude@h/" + deniedName + ".md", Name: deniedName, Desc: "the " + deniedText + " in demo flushes every quarter hour", Harness: "claude", Day: "2026-11-01", Repo: "demo"}, time.Now())
	if err := q.Save(agentreview.Path(platform.StateDir(home))); err != nil {
		t.Fatal(err)
	}
	compacted := func(session string) string {
		turns := []string{`{"type":"response_item","payload":{"type":"message","role":"user"}}`}
		line := `{"timestamp":"2026-11-04T10:00:00Z","type":"compacted","payload":{"message":` + jsonString("This session is being continued from a previous conversation that ran out of context. Summary: the session fixed how the "+deniedText+" flushes in the demo repo, alpha bravo charlie delta foxtrot golf hotel india juliet kilo lima mike november oscar papa.") + `,"replacement_history":[]}}`
		return writeTranscript(t, filepath.Join(home, ".codex", "sessions", "2026", "11", "04", "rollout-2026-11-04T10-00-00-"+session+".jsonl"), append(turns, line)...)
	}
	// One Claude Code process runs every session here, one after another:
	// its hooks record the session it runs, and its tacit MCP server and
	// the CLI in its shell read that record (session_pid.go). No session
	// id is in the environment.
	for _, k := range []string{"CLAUDE_CODE_SESSION_ID", "TACIT_SESSION", "PI_SESSION_ID", "CODEX_THREAD_ID", "CODEX_SESSION_ID"} {
		t.Setenv(k, "")
	}
	stubHarness(t, 6161)
	runs := func(session string) {
		writeSmall(filepath.Join(harnessPIDDir(), "6161"), session)
		t.Chdir(cwd)
	}
	tool := func(name string, args map[string]any) func(session string) string {
		return func(session string) string {
			runs(session)
			return callToolAs(t, "claude", name, args)
		}
	}
	cli := func(cmd func([]string) int, args ...string) func(session string) string {
		return func(session string) string {
			runs(session)
			return captureStdout(t, func() { cmd(args) })
		}
	}
	rows := []struct {
		path string
		run  func(session string) string
	}{
		{"start", func(s string) string { return hookOut(t, payload(s, "startup", ""), "session-start") }},
		{"start after a clear", func(s string) string {
			hookOut(t, payload(s, "startup", ""), "session-start")
			forget(s)
			return hookOut(t, payload(s, "clear", ""), "session-start")
		}},
		{"start after a compaction and the summary's selection", func(s string) string {
			hookOut(t, payload(s, "startup", ""), "session-start")
			forget(s)
			return hookOut(t, payload(s, "compact", compacted(s)), "session-start")
		}},
		{"prompt", func(s string) string {
			return hookOut(t, payload(s, "", "why does the "+deniedText+" flush so often"), "prompt")
		}},
		{"prompt's conflict mark", func(s string) string {
			return hookOut(t, payload(s, "", "why is the yak loader slow when demo starts"), "prompt")
		}},
		{"reviewer's question", func(s string) string {
			// The transcript's own cwd is a person's checkout (a test's temp
			// folder reads as an agent's session, which is never asked).
			return promptHook(t, s, claudeSession(t, home, s, "/home/u/projects/demo"), cwd, "check the build output")
		}},
		{"push", func(s string) string {
			return hookOut(t, toolPayload(cwd, s, "Bash", map[string]any{"command": "make zebra"}), "pre-tool")
		}},
		{"tacit_search by words", tool("tacit_search", map[string]any{"query": deniedText})},
		{"tacit_search by scope", tool("tacit_search", map[string]any{"scope": "projects/demo"})},
		{"tacit_tree folder and overview", tool("tacit_tree", map[string]any{"path": "projects/demo", "depth": 0})},
		{"tacit_tree root", tool("tacit_tree", map[string]any{"path": "projects", "depth": 2})},
		{"tacit search", cli(runSearch, "--repo", "demo", deniedText)},
		{"tacit tree", cli(runTree, "projects/demo")},
		{"tacit_fact of a fact that names it", tool("tacit_fact", map[string]any{"name": "demo-yak-loader"})},
		{"tacit fact of a fact that names it", cli(runFact, "demo-yak-loader")},
	}
	for _, row := range rows {
		id := strings.NewReplacer(" ", "-", "'", "", "_", "-").Replace(row.path)
		control, denied := "c-"+id, "d-"+id
		appendNames(denyFile(denied), []string{deniedName})
		if out := row.run(control); !strings.Contains(out, deniedName) && !strings.Contains(out, deniedText) {
			t.Errorf("%s: the control session does not reach the fact, so the row tests nothing:\n%s", row.path, out)
			continue
		}
		if out := row.run(denied); strings.Contains(out, deniedName) || strings.Contains(out, deniedText) {
			t.Errorf("%s: the denied fact shows:\n%s", row.path, out)
		}
	}

	// What a search answer listed is given to the session: its arm is
	// decided.
	if !readNames(givenFile("c-tacit-search-by-words"))[deniedName] {
		t.Error("the search answer's facts are not in the session's given set")
	}

	// Every count the tree shows is the view's: one fact fewer.
	for _, args := range []map[string]any{{"path": "projects/demo", "depth": 0}, {"path": "projects", "depth": 2}} {
		appendNames(denyFile("d-counts"), []string{deniedName})
		c, d := tool("tacit_tree", args)("c-counts"), tool("tacit_tree", args)("d-counts")
		cn, dn := factCounts(c), factCounts(d)
		if len(cn) == 0 || len(cn) != len(dn) {
			t.Fatalf("counts %v / %v:\n%s\n%s", cn, dn, c, d)
		}
		for i := range cn {
			if dn[i] != cn[i]-1 {
				t.Errorf("tree %v: count %d is %d in the control, %d denied", args, i, cn[i], dn[i])
			}
		}
	}

	// A Claude Code call whose session cannot be resolved reads no view
	// and is logged, with its harness process.
	os.Remove(filepath.Join(harnessPIDDir(), "6161"))
	callToolAs(t, "claude", "tacit_search", map[string]any{"query": deniedText})
	found := false
	for _, e := range readDeliveryLog(t, platform.StateDir(home)) {
		found = found || e.Hook == "view-unresolved" && e.Tool == "tacit_search" && e.HarnessPID == 6161
	}
	if !found {
		t.Error("no view-unresolved entry")
	}

	// A resume: a fact the session was given is now replaced by the denied
	// one; the withdrawn line does not name it. Last, since it changes the
	// store for every session.
	for _, s := range []string{"c-resume", "d-resume"} {
		hookOut(t, payload(s, "startup", ""), "session-start")
		rememberNames(s, []string{"demo-old-flusher"})
	}
	appendNames(denyFile("d-resume"), []string{deniedName})
	writeFact(t, r.clone, fact.Meta{Name: "demo-old-flusher", Description: "the old flusher in demo ran hourly", Type: "design_decision", Scope: "projects/demo", SupersededBy: deniedName}, "old")
	r.commitIndex(r.clone, "the old flusher replaced")
	gitIn(t, r.clone, "push", "-q", "origin", "HEAD:main")
	buildIndexForHooks(r.s)
	if out := hookOut(t, payload("c-resume", "resume", ""), "session-start"); !strings.Contains(out, "replaced by "+deniedName) {
		t.Errorf("resume: the control session's withdrawn line does not name the successor:\n%s", out)
	}
	if out := hookOut(t, payload("d-resume", "resume", ""), "session-start"); strings.Contains(out, deniedName) || strings.Contains(out, deniedText) || !strings.Contains(out, "demo-old-flusher: replaced") {
		t.Errorf("resume: the denied session's block:\n%s", out)
	}
}

// tacit_fact asked for a denied fact by name serves it: the agent's own
// pull, which the delivery log records (a nav entry naming the fact), so
// the analysis sees the denial broken for that session.
func TestHoldOutFactByName(t *testing.T) {
	r, home, _ := restartRig(t)
	writeFact(t, r.clone, fact.Meta{Name: deniedName, Description: "the " + deniedText + " in demo flushes every quarter hour", Type: "design_decision", Scope: "projects/demo"}, "zebra")
	r.commitIndex(r.clone, "the denied fact")
	gitIn(t, r.clone, "push", "-q", "origin", "HEAD:main")
	buildIndexForHooks(r.s)
	stubHarness(t, 7171)
	appendNames(denyFile("d-fact"), []string{deniedName})
	writeSmall(filepath.Join(harnessPIDDir(), "7171"), "d-fact")
	if out := callToolAs(t, "claude", "tacit_fact", map[string]any{"name": deniedName}); !strings.Contains(out, deniedText) {
		t.Fatalf("tacit_fact by name:\n%s", out)
	}
	// The opening is in the log: the session's denial is broken.
	cwd := filepath.Join(r.projects, "demo")
	hookOut(t, toolPayload(cwd, "d-fact", "mcp__tacit__tacit_fact", map[string]any{"name": deniedName}), "pre-tool", "--mcp")
	broken := false
	for _, e := range readDeliveryLog(t, platform.StateDir(home)) {
		broken = broken || e.Session == "d-fact" && e.Nav != nil && e.Nav.Tool == "fact" && strings.Join(e.Nav.Names, ",") == deniedName
	}
	if !broken {
		t.Fatal("no nav entry names the denied fact")
	}
}

// factCounts is every "N facts" count, and every scope count "(N)" of the
// counts block, of a tree answer, in order.
func factCounts(out string) []int {
	var n []int
	for _, m := range countRx.FindAllStringSubmatch(out, -1) {
		v := m[1] + m[2]
		x, _ := strconv.Atoi(v)
		n = append(n, x)
	}
	return n
}

var countRx = regexp.MustCompile(`(?m)(?:^| )(\d+) facts\b|^- projects/demo \((\d+)\)`)
