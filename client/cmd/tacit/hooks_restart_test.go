package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/platform"
	"github.com/agent-sh/tacit/client/internal/search"
)

// restartRig is a store with the demo repo's ruling, two decisions and its
// note, which make the start block, and two history facts the start block
// never shows (tier 2): the wombat cache flusher, which a summary's words
// select, and the otter retry loop, which a prompt selects. The hook runs in
// the demo checkout with a home of its own; index builds run in the test,
// not detached. prompt is the prompt hook's input for a session.
func restartRig(t *testing.T) (r *rig, home string, payload func(session, source, transcript string) string) {
	t.Helper()
	r = newRig(t, rigOpts{repos: []string{"demo"}, filler: 200, rules: 3})
	home = filepath.Join(r.root, "home")
	t.Setenv("TACIT_STORE", r.clone)
	t.Setenv("HOME", home)
	t.Setenv("TACIT_STATE_DIR", "")
	t.Setenv("TACIT_FOLLOW_STATE", filepath.Join(r.root, "no-follower.json"))
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", "")
	prev := startIndexBuild
	startIndexBuild = func() {}
	t.Cleanup(func() { startIndexBuild = prev })
	writeFact(t, r.clone, fact.Meta{Name: "demo-wombat-flusher", Description: "the wombat cache flusher in demo runs every night and drops stale wombat entries",
		Type: "history", Scope: "projects/demo"}, "wombat")
	writeFact(t, r.clone, fact.Meta{Name: "demo-otter-retry", Description: "the otter retry loop in demo backs off twice before it gives up",
		Type: "history", Scope: "projects/demo"}, "otter")
	r.commitIndex(r.clone, "add the history facts")
	gitIn(t, r.clone, "push", "-q", "origin", "HEAD:main")
	cwd := filepath.Join(r.projects, "demo")
	payload = func(session, source, transcript string) string {
		if source == "" { // a prompt: transcript holds "<prompt>" or "<prompt>|<transcript>"
			prompt, path, _ := strings.Cut(transcript, "|")
			return `{"session_id":"` + session + `","cwd":"` + cwd + `","hook_event_name":"UserPromptSubmit","prompt":"` + prompt + `","transcript_path":"` + path + `"}`
		}
		return `{"session_id":"` + session + `","cwd":"` + cwd + `","hook_event_name":"SessionStart","source":"` + source + `","transcript_path":"` + transcript + `"}`
	}
	return r, home, payload
}

// otterPrompt is a request the otter fact answers and the summary does not
// name. It does not say the fact back, which the prompt's echo filter would
// leave out.
const otterPrompt = "why did the otter loop stop retrying in production"

// sessionSet is the session's delivered set as the hooks keep it.
func sessionSet(t *testing.T, home, session string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(platform.StateDir(home), "sessions", session))
	if err != nil {
		t.Fatal(err)
	}
	out := strings.Fields(string(b))
	slices.Sort(out)
	return slices.Compact(out)
}

func writeTranscript(t *testing.T, path string, lines ...string) string {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// compactSummaryText opens as Claude Code's summaries do, with the harness's
// own words and more terms than a query takes before its subject. It names
// the note (the summary holds it) and the wombat fact's subject without
// saying the fact back: a summary that says a fact back is an echo, which the
// prompt's selection leaves out (withoutEchoes).
var compactSummaryText = "This session is being continued from a previous conversation that ran out of context. " +
	"The summary below covers the earlier portion of the conversation. Summary: Primary Request and Intent: " +
	"alpha bravo charlie delta foxtrot golf hotel india juliet kilo lima mike november oscar papa quebec romeo sierra tango uniform victor whiskey yankee zulu. " +
	"The session read demo-note, then fixed a bug in the wombat flusher of the demo repo."

func appendLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.WriteString(strings.Join(lines, "\n") + "\n")
}

// A harness that writes its summary before the start (Codex's compacted
// entry, pi's compaction entry, at the transcript's end): the start block
// comes again from the store as it is, less what the summary still holds
// (the note it names), then the facts the summary's words select (the wombat
// history fact, which no start block shows, named past the summary's first
// terms). The session's delivered set is then what its context holds: a fact
// a prompt gave before the compaction (the otter one) that the summary does
// not hold is in it no more.
func TestStartHookAfterACompaction(t *testing.T) {
	_, home, payload := restartRig(t)
	for _, c := range []struct {
		harness, session, path, line string
	}{
		{"codex", "x1", filepath.Join(home, ".codex", "sessions", "2026", "11", "04", "rollout-2026-11-04T10-00-00-x1.jsonl"),
			`{"timestamp":"2026-11-04T10:00:00Z","type":"compacted","payload":{"message":` + jsonString(compactSummaryText) + `,"replacement_history":[]}}`},
		{"pi", "p1", filepath.Join(home, ".pi", "agent", "sessions", "--demo--", "2026-11-04T10-00-00_p1.jsonl"),
			`{"type":"compaction","id":"a1","summary":` + jsonString(compactSummaryText) + `}`},
	} {
		t.Run(c.harness, func(t *testing.T) {
			start := hookOut(t, payload(c.session, "startup", ""), "session-start")
			for _, want := range []string{"RULE demo-quokka-scheduler-0", "demo-note:"} {
				if !strings.Contains(start, want) {
					t.Fatalf("start block lacks %q:\n%s", want, start)
				}
			}
			if got := hookOut(t, payload(c.session, "", otterPrompt), "prompt"); !strings.Contains(got, "demo-otter-retry:") {
				t.Fatalf("the prompt gave no otter fact:\n%s", got)
			}
			tr := writeTranscript(t, c.path, `{"type":"response_item","payload":{"type":"message","role":"user"}}`, `{"type":"message","message":{"role":"user"}}`, c.line)
			out := hookOut(t, payload(c.session, "compact", tr), "session-start")
			for _, want := range []string{", again after the compaction", "RULE demo-quokka-scheduler-0", "demo-quokka-scheduler-1:",
				summaryTitle, "demo-wombat-flusher:"} {
				if !strings.Contains(out, want) {
					t.Fatalf("after the compaction, no %q:\n%s", want, out)
				}
			}
			if strings.Contains(out, "demo-note:") {
				t.Fatalf("the note the summary holds came again:\n%s", out)
			}
			want := []string{"demo-note", "demo-quokka-scheduler-0", "demo-quokka-scheduler-1", "demo-quokka-scheduler-2", "demo-wombat-flusher"}
			if got := sessionSet(t, home, c.session); !slices.Equal(got, want) {
				t.Fatalf("delivered set %v, want %v", got, want)
			}
		})
	}
}

// Claude Code runs a compaction's SessionStart hooks before it writes the
// summary. At the start the transcript's end holds an older summary with
// turns after it, which is no summary of this compaction: the block is the
// whole start block again, the note among it, and the delivered set is that
// block (the otter fact a prompt gave is in it no more). The first prompt
// after reads the summary Claude Code wrote since, adds what it holds of the
// facts given before, and gives the facts its words select; no later prompt
// gives them again.
func TestStartHookAfterAClaudeCompaction(t *testing.T) {
	_, home, payload := restartRig(t)
	hookOut(t, payload("c1", "startup", ""), "session-start")
	if got := hookOut(t, payload("c1", "", otterPrompt), "prompt"); !strings.Contains(got, "demo-otter-retry:") {
		t.Fatalf("the prompt gave no otter fact:\n%s", got)
	}
	path := writeTranscript(t, filepath.Join(home, ".claude", "projects", "-demo", "c1.jsonl"),
		`{"type":"user","message":{"role":"user","content":"start"}}`,
		`{"type":"user","isCompactSummary":true,"message":{"role":"user","content":"Summary: the session worked on the otter retry loop."}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`)
	out := hookOut(t, payload("c1", "compact", path), "session-start")
	for _, want := range []string{", again after the compaction", "RULE demo-quokka-scheduler-0", "demo-note:"} {
		if !strings.Contains(out, want) {
			t.Fatalf("after the compaction, no %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, summaryTitle) {
		t.Fatalf("an older summary was taken for this one:\n%s", out)
	}
	if got, want := sessionSet(t, home, "c1"), []string{"demo-note", "demo-quokka-scheduler-0", "demo-quokka-scheduler-1", "demo-quokka-scheduler-2"}; !slices.Equal(got, want) {
		t.Fatalf("delivered set %v, want %v", got, want)
	}
	appendLines(t, path, `{"type":"system","subtype":"compact_boundary"}`,
		`{"type":"user","isCompactSummary":true,"message":{"role":"user","content":[{"type":"text","text":`+jsonString(compactSummaryText)+`}]}}`,
		`{"type":"attachment","attachment":{"type":"hook_success"}}`)
	got := hookOut(t, payload("c1", "", "continue|"+path), "prompt")
	if !strings.Contains(got, summaryTitle) || !strings.Contains(got, "demo-wombat-flusher:") {
		t.Fatalf("the first prompt after gave no summary facts:\n%s", got)
	}
	if set, want := sessionSet(t, home, "c1"), []string{"demo-note", "demo-quokka-scheduler-0", "demo-quokka-scheduler-1", "demo-quokka-scheduler-2", "demo-wombat-flusher"}; !slices.Equal(set, want) {
		t.Fatalf("delivered set %v, want %v", set, want)
	}
	if again := hookOut(t, payload("c1", "", "continue|"+path), "prompt"); strings.Contains(again, summaryTitle) {
		t.Fatalf("a later prompt gave the summary's facts again:\n%s", again)
	}
}

// A fact counts as in the summary by its whole name (for a name of three
// words or more, also its words in order), or, when it is no ruling, by one
// sentence or line that holds most of its description; a ruling only by its
// name.
func TestSummaryHoldsWholeNamesOnly(t *testing.T) {
	sum := summaryOf("We shipped demo-note-v2, ran go testing, read the demo note.\nThen we read the wombat cache flusher notes.\nThe nightly job drops stale entries. The wombat cache is cold.")
	for _, c := range []struct {
		hit  search.Hit
		want bool
	}{
		{search.Hit{Name: "demo-note-v2"}, true},
		{search.Hit{Name: "demo-note"}, false}, // inside a longer name; two words are no name
		{search.Hit{Name: "note-v2"}, false},
		{search.Hit{Name: "go-test"}, false},
		{search.Hit{Name: "wombat-cache-flusher"}, true},
		{search.Hit{Name: "a", Description: "the wombat cache flusher notes"}, true},
		{search.Hit{Name: "b", Description: "the wombat cache flusher notes", Ruling: true}, false},
		{search.Hit{Name: "c", Description: "the nightly wombat flusher purges cold caches"}, false}, // its words across sentences
	} {
		if got := sum.holds(c.hit); got != c.want {
			t.Errorf("%+v: holds %v, want %v", c.hit.Name, got, c.want)
		}
	}
}

// A clear drops a compaction's record for the next prompt, and a second
// compaction before that prompt keeps what the session had been given before
// the first: the otter fact a prompt gave, which the second summary names,
// joins the delivered set at the prompt as held, not given again.
func TestStartHookCompactionRecordAcrossClearAndASecondCompaction(t *testing.T) {
	_, home, payload := restartRig(t)
	path := filepath.Join(home, ".claude", "projects", "-demo", "q1.jsonl")
	turn := `{"type":"user","message":{"role":"user","content":"go on"}}`
	summary := func(text string) string {
		return `{"type":"user","isCompactSummary":true,"message":{"role":"user","content":` + jsonString(text) + `}}`
	}
	hookOut(t, payload("q1", "startup", ""), "session-start")
	hookOut(t, payload("q1", "", otterPrompt), "prompt")
	writeTranscript(t, path, turn)
	hookOut(t, payload("q1", "compact", path), "session-start")
	appendLines(t, path, summary("Summary: the session looked at demo-otter-retry."), turn)
	hookOut(t, payload("q1", "compact", path), "session-start")
	appendLines(t, path, summary(compactSummaryText+" It also kept demo-otter-retry in mind."))
	got := hookOut(t, payload("q1", "", "continue|"+path), "prompt")
	if set := sessionSet(t, home, "q1"); !slices.Contains(set, "demo-otter-retry") || strings.Contains(got, "demo-otter-retry:") {
		t.Fatalf("the fact given before the first compaction, which the summary holds, is not held (set %v):\n%s", set, got)
	}

	hookOut(t, payload("q2", "startup", ""), "session-start")
	p2 := writeTranscript(t, filepath.Join(home, ".claude", "projects", "-demo", "q2.jsonl"), turn)
	hookOut(t, payload("q2", "compact", p2), "session-start")
	hookOut(t, payload("q2", "clear", ""), "session-start")
	appendLines(t, p2, summary(compactSummaryText))
	if got := hookOut(t, payload("q2", "", "continue|"+p2), "prompt"); strings.Contains(got, summaryTitle) {
		t.Fatalf("a clear left the compaction's record:\n%s", got)
	}
}

// The start after a compaction does not sync: the harness holds the
// compaction until the hook answers. A resume does (the hooks read the index
// they have and build the new one after, so its block is the next one's).
func TestStartHookAfterACompactionDoesNotSync(t *testing.T) {
	r, home, payload := restartRig(t)
	hookOut(t, payload("n1", "startup", ""), "session-start")
	other := filepath.Join(r.root, "other")
	gitIn(t, r.root, "clone", "-q", r.bare, other)
	gitIn(t, other, "config", "user.email", "curator@tacit")
	gitIn(t, other, "config", "user.name", "curator")
	writeFact(t, other, fact.Meta{Name: "demo-pelican-gate", Description: "the pelican gate in demo holds a deploy until its canary passes",
		Type: "design_decision", Scope: "projects/demo"}, "pelican")
	r.commitIndex(other, "a fact from another machine")
	gitIn(t, other, "push", "-q", "origin", "HEAD:main")
	tr := writeTranscript(t, filepath.Join(home, ".claude", "projects", "-demo", "n1.jsonl"), `{"type":"user","message":{"role":"user","content":"go"}}`)
	before := gitIn(t, r.clone, "rev-parse", "HEAD")
	hookOut(t, payload("n1", "compact", tr), "session-start")
	if got := gitIn(t, r.clone, "rev-parse", "HEAD"); got != before {
		t.Fatal("the start after a compaction synced")
	}
	hookOut(t, payload("n1", "resume", ""), "session-start")
	if got := gitIn(t, r.clone, "rev-parse", "HEAD"); got != r.serverHead() {
		t.Fatalf("a resume did not sync: clone at %s, server at %s", got, r.serverHead())
	}
}

// On a resume the session's old context is back. The block is what the store
// holds for it now that the session was never given, and a line for each fact
// it was given that is now retired or replaced (naming the new fact), said
// once. A session with no record of what it was given gets a start's block.
func TestStartHookOnResume(t *testing.T) {
	r, _, payload := restartRig(t)
	hookOut(t, payload("r1", "startup", ""), "session-start")

	writeFact(t, r.clone, fact.Meta{Name: "demo-quokka-scheduler-2", Description: "quokka scheduler zebra rule 2 for the demo repo",
		Type: "design_decision", Scope: "projects/demo", Retired: "2026-11-01"}, "quokka scheduler zebra")
	writeFact(t, r.clone, fact.Meta{Name: "demo-note", Description: "a curated note about demo", Type: "design_decision",
		Scope: "projects/demo", SupersededBy: "demo-note-v2"}, "body")
	writeFact(t, r.clone, fact.Meta{Name: "demo-note-v2", Description: "a second curated note about demo", Type: "design_decision",
		Scope: "projects/demo"}, "body")
	writeFact(t, r.clone, fact.Meta{Name: "demo-quokka-scheduler-1", Description: "quokka scheduler zebra rule 1 for the demo repo",
		Type: "design_decision", Scope: "projects/demo", SupersededBy: "demo-scheduler-history"}, "quokka scheduler zebra")
	writeFact(t, r.clone, fact.Meta{Name: "demo-scheduler-history", Description: "the quokka scheduler moved to a pool of workers",
		Type: "history", Scope: "projects/demo"}, "history")
	r.commitIndex(r.clone, "retire and replace")
	gitIn(t, r.clone, "push", "-q", "origin", "HEAD:main")
	buildIndexForHooks(r.s)

	out := hookOut(t, payload("r1", "resume", ""), "session-start")
	for _, want := range []string{
		"# tacit: new for demo on ", "- demo-note-v2:",
		"# tacit: no longer holds, given to this session earlier",
		"- demo-note: replaced by demo-note-v2\n",
		"- demo-quokka-scheduler-1: replaced by demo-scheduler-history\n",
		"- demo-quokka-scheduler-2: retired 2026-11-01",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("resume block lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "RULE demo-quokka-scheduler-0") {
		t.Fatalf("a fact the session has came again:\n%s", out)
	}
	if again := hookOut(t, payload("r1", "resume", ""), "session-start"); strings.Contains(again, "demo-note-v2:") || strings.Contains(again, "no longer holds") {
		t.Fatalf("a second resume repeats what the first gave or said:\n%s", again)
	}
	if fresh := hookOut(t, payload("r2", "resume", ""), "session-start"); !strings.Contains(fresh, "RULE demo-quokka-scheduler-0") || !strings.Contains(fresh, "(read before acting;") {
		t.Fatalf("a resume with no record is not a start's block:\n%s", fresh)
	}
}

// After a clear the context is empty: the start block again, whole, and a
// fact a prompt gave before the clear may come again. A fork, and a source
// tacit does not know, print nothing.
func TestStartHookOnClearAndFork(t *testing.T) {
	_, _, payload := restartRig(t)
	hookOut(t, payload("k1", "startup", ""), "session-start")
	if got := hookOut(t, payload("k1", "", otterPrompt), "prompt"); !strings.Contains(got, "demo-otter-retry:") {
		t.Fatalf("the prompt gave no otter fact:\n%s", got)
	}
	if out := hookOut(t, payload("k1", "clear", ""), "session-start"); !strings.Contains(out, "RULE demo-quokka-scheduler-0") || !strings.Contains(out, "demo-note:") {
		t.Fatalf("after a clear:\n%s", out)
	}
	if got := hookOut(t, payload("k1", "", otterPrompt), "prompt"); !strings.Contains(got, "demo-otter-retry:") {
		t.Fatalf("after a clear the prompt's fact did not come again:\n%s", got)
	}
	for _, source := range []string{"fork", "elsewhere"} {
		if out := hookOut(t, payload("k2", source, ""), "session-start"); out != "" {
			t.Fatalf("source %s printed:\n%s", source, out)
		}
	}
}

// The summary's own selection is tacit's, never the utility provider's: a
// summary holds far more of the session than a prompt. With a provider set
// up, the start after a compaction makes no call to it; a prompt in the same
// session does.
func TestStartHookAfterACompactionKeepsTheSummaryLocal(t *testing.T) {
	_, home, payload := restartRig(t)
	stubScrump(t)
	t.Setenv("TACIT_UTILITY_PROVIDER", "systemone")
	t.Setenv("TACIT_UTILITY_ENDPOINT", "http://localhost:8000/v1/systemone")
	t.Setenv("TACIT_UTILITY_THRESHOLD", "0.5")
	calls := fakeTransport(t, func(r *http.Request, body map[string]any) (int, any) {
		answers := map[string]any{}
		for id := range body["questions"].(map[string]any) {
			answers[id] = map[string]any{"type": "noul", "noul": 0.9}
		}
		return 200, map[string]any{"model": "strands-decider-0.1.0", "answers": answers}
	})
	hookOut(t, payload("u1", "startup", ""), "session-start")
	tr := writeTranscript(t, filepath.Join(home, ".claude", "projects", "-demo", "u1.jsonl"),
		`{"type":"user","isCompactSummary":true,"message":{"role":"user","content":"`+compactSummaryText+`"}}`)
	if out := hookOut(t, payload("u1", "compact", tr), "session-start"); !strings.Contains(out, "demo-wombat-flusher:") {
		t.Fatalf("no summary selection:\n%s", out)
	}
	if *calls != 0 {
		t.Fatalf("the start after a compaction called the provider %d time(s)", *calls)
	}
	hookOut(t, payload("u1", "", otterPrompt), "prompt")
	if *calls == 0 {
		t.Fatal("the prompt did not reach the provider: the test does not show the summary stays local")
	}
}

// The summary's query is its heaviest terms, not its first: a term counts by
// how often the summary says it and how few facts hold it. Thirty rare terms
// said once come before a subject said twice; the subject is in the query.
func TestSummaryQueryTakesItsHeaviestTerms(t *testing.T) {
	r, _, _ := restartRig(t)
	ix, err := openHookIndex(context.Background(), r.s)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	var lorems []string
	for i := range 30 {
		lorems = append(lorems, fmt.Sprintf("lorem%03d", i))
	}
	q := summaryQuery(context.Background(), ix, "Summary: "+strings.Join(lorems, " ")+". The wombat flusher broke; the wombat flusher is fixed.")
	terms := strings.Fields(q)
	if len(terms) != summaryQueryTerms || !slices.Contains(terms, "wombat") || !slices.Contains(terms, "flusher") {
		t.Fatalf("query %q", q)
	}
	// A fact's words in another form, looked up by their forms and stems;
	// one term per stem.
	if q := summaryQuery(context.Background(), ix, "Summary: the wombats and their flushers, and a wombat."); !strings.Contains(q, "wombats") || !strings.Contains(q, "flushers") || strings.Count(q, "wombat") != 1 {
		t.Fatalf("query %q", q)
	}
}
