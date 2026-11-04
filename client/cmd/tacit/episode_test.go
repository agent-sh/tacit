package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/events"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/modelrun"
	"github.com/agent-sh/tacit/client/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const episodeSessionID = "5b1e0c3a-1111-4222-8333-444455556666"

// episodeHome is a home with one Claude Code session transcript: the
// owner's request naming a code review, the agent's command (with a secret
// the redactor masks), its output and the agent's reply. The harness gives
// the session id as Claude Code does.
func episodeHome(t *testing.T) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "home")
	t.Setenv("HOME", home)
	t.Setenv("TACIT_STATE_DIR", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", episodeSessionID)
	t.Setenv("TACIT_SESSION", "")
	stubScrump(t)
	head := `"sessionId":"` + episodeSessionID + `","cwd":"/work/demo","entrypoint":"cli"`
	writeTranscript(t, filepath.Join(home, ".claude", "projects", "-work-demo", episodeSessionID+".jsonl"),
		`{"type":"user",`+head+`,"promptSource":"typed","uuid":"u1","timestamp":"2026-11-01T10:00:00Z","message":{"role":"user","content":"deploy the release candidate CR-123456789 to staging"}}`,
		`{"type":"assistant",`+head+`,"uuid":"a1","timestamp":"2026-11-01T10:00:05Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"./deploy.sh CR-123456789 --token Zq7Vx9"}}]}}`,
		`{"type":"user",`+head+`,"uuid":"u2","timestamp":"2026-11-01T10:00:09Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"deployed CR-123456789 with Zq7Vx9 to staging-2"}]}}`,
		`{"type":"assistant",`+head+`,"uuid":"a2","timestamp":"2026-11-01T10:00:12Z","message":{"role":"assistant","content":[{"type":"text","text":"Deployed to staging-2."}]}}`,
		// An edit that mentions the tool and the id: an event like any other.
		`{"type":"assistant",`+head+`,"uuid":"a4","timestamp":"2026-11-01T10:01:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t3","name":"Edit","input":{"file_path":"/work/demo/NOTES.md","new_string":"tacit episode found CR-123456789"}}]}}`,
		// A recall run in a shell, and its answer: no events of what was recalled.
		`{"type":"assistant",`+head+`,"uuid":"a5","timestamp":"2026-11-01T10:02:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t4","name":"Bash","input":{"command":"cd /work && TACIT_X=1 ~/.local/bin/tacit episode find CR-123456789 | head"}}]}}`,
		`{"type":"user",`+head+`,"uuid":"u4","timestamp":"2026-11-01T10:02:01Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t4","content":"- 1.0 owner owner_text: deploy the release candidate CR-123456789"}]}}`,
		// An earlier recall of the same words and its answer: no event of what was recalled.
		`{"type":"assistant",`+head+`,"uuid":"a3","timestamp":"2026-11-01T10:05:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t2","name":"mcp__tacit__tacit_episode","input":{"find":"CR-123456789"}}]}}`,
		`{"type":"user",`+head+`,"uuid":"u3","timestamp":"2026-11-01T10:05:01Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","content":"- 1.0 11-01 10:00 owner owner_text: deploy the release candidate CR-123456789"}]}}`,
	)
	return home
}

// episodeOut runs `tacit episode args...` and returns what it printed and
// its exit code.
func episodeOut(t *testing.T, args ...string) (string, int) {
	t.Helper()
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = wr
	code := runEpisode(args)
	wr.Close()
	os.Stdout = old
	b, _ := io.ReadAll(rd)
	return string(b), code
}

var refRx = regexp.MustCompile(`(?m)^[->] (\d+\.\d+) `)

// Find gives one line per event of this session holding the words, newest
// first, each with its ref, who wrote it and its kind; around gives the
// events on each side of a ref; show gives one event whole. The secret in
// the command and its output is masked in every answer.
func TestEpisodeFindAroundShow(t *testing.T) {
	episodeHome(t)
	out, code := episodeOut(t, "find", "CR-123456789")
	if code != 0 || !strings.Contains(out, "# tacit: 4 event(s) of this session (claude) holding") || strings.Contains(out, "tacit_episode") || !strings.Contains(out, "tool_call Edit") || strings.Contains(out, "| head") {
		t.Fatalf("find, the session's own recall left out (%d):\n%s", code, out)
	}
	for _, want := range []string{"owner owner_text", "agent tool_call Bash", "tool_result Bash"} {
		if !strings.Contains(out, want) {
			t.Fatalf("find lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Zq7Vx9") {
		t.Fatalf("find showed the secret:\n%s", out)
	}
	refs := refRx.FindAllStringSubmatch(out, -1)
	if len(refs) < 3 {
		t.Fatalf("no refs:\n%s", out)
	}
	newest, oldest := refs[0][1], refs[len(refs)-1][1]
	around, code := episodeOut(t, "--n", "1", "around", oldest)
	if code != 0 || !strings.Contains(around, "> "+oldest+" ") || strings.Count(around, "\n- ") != 1 {
		t.Fatalf("around %s (%d):\n%s", oldest, code, around)
	}
	var secretRef string // the deploy command's event, which holds the secret
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "agent tool_call Bash") {
			secretRef = strings.Fields(l)[1]
		}
	}
	show, code := episodeOut(t, "show", secretRef)
	if code != 0 || !strings.Contains(show, "## ") || !strings.Contains(show, "\n~~~\n") || strings.Contains(show, "Zq7Vx9") || !strings.Contains(show, "******") {
		t.Fatalf("show %s (%d):\n%s", secretRef, code, show)
	}
	if out, code := episodeOut(t, "show", "1.99"); code != 1 || !strings.Contains(out, "no event 1.99") {
		t.Fatalf("an unknown ref (%d):\n%s", code, out)
	}
	// before pages back: only events before the ref.
	older, code := episodeOut(t, "--before", newest, "find", "CR-123456789")
	if code != 0 || strings.Contains(older, "- "+newest+" ") || len(refRx.FindAllString(older, -1)) != len(refs)-1 {
		t.Fatalf("before %s (%d):\n%s", newest, code, older)
	}
}

// Without a redactor nothing of the session is shown, on stdout or stderr.
func TestEpisodeShowsNothingWithoutRedaction(t *testing.T) {
	episodeHome(t)
	t.Setenv("PATH", t.TempDir())
	rd, wr, _ := os.Pipe()
	old := os.Stderr
	os.Stderr = wr
	out, code := episodeOut(t, "find", "CR-123456789")
	wr.Close()
	os.Stderr = old
	errOut, _ := io.ReadAll(rd)
	for _, s := range []string{out, string(errOut)} {
		if code == 0 || strings.Contains(s, "to staging") || strings.Contains(s, "deploy.sh") || strings.Contains(s, "Zq7Vx9") {
			t.Fatalf("without scrump (%d):\n%s\nstderr:\n%s", code, out, errOut)
		}
	}
}

// A session of tacit's own model runs is never read, and a session whose
// transcript is not on this machine is said to be so.
func TestEpisodeRefusesExcludedAndAbsentSessions(t *testing.T) {
	home := episodeHome(t)
	os.MkdirAll(filepath.Dir(modelrun.ExcludeFile(home)), 0o700)
	if err := os.WriteFile(modelrun.ExcludeFile(home), []byte(episodeSessionID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, code := episodeOut(t, "find", "CR-123456789"); code != 1 || !strings.Contains(out, "tacit's own model runs") {
		t.Fatalf("an excluded session (%d):\n%s", code, out)
	}
	// Part of an excluded id finds its transcript by its name's end: refused too.
	if out, code := episodeOut(t, "--session", episodeSessionID[len(episodeSessionID)-12:], "find", "CR-123456789"); code != 1 || !strings.Contains(out, "tacit's own model runs") {
		t.Fatalf("a part of an excluded id (%d):\n%s", code, out)
	}
	if out, code := episodeOut(t, "--session", "9f9f9f9f-0000-4000-8000-000000000000", "find", "x"); code != 1 || !strings.Contains(out, "not on this machine") {
		t.Fatalf("an absent session (%d):\n%s", code, out)
	}
}

// A fact leads to the session its owner's quote came from, at the quote's
// words.
func TestEpisodeFactLeadsToItsSession(t *testing.T) {
	r := newRig(t, rigOpts{repos: []string{"demo"}})
	home := episodeHome(t)
	_ = home
	t.Setenv("TACIT_STORE", r.clone)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	writeFact(t, r.clone, fact.Meta{Name: "demo-release-candidates-go-to-staging", Description: "release candidates are deployed to staging first",
		Type: "user_standard", Scope: "projects/demo", Source: fact.Source{Kind: "user", OwnerQuote: "deploy the release candidate", OwnerQuoteFrom: "claude:" + episodeSessionID}}, "staging first")
	out, code := episodeOut(t, "--fact", "demo-release-candidates-go-to-staging")
	if code != 0 || !strings.Contains(out, "the session demo-release-candidates-go-to-staging came from") || !strings.Contains(out, "\n- ") || !strings.Contains(out, "owner owner_text") {
		t.Fatalf("fact (%d):\n%s", code, out)
	}
}

// A curated fact whose origin is a dreamer note (harness dreamer, which names
// no transcript's harness) leads to the note's session, found by the note's
// frontmatter name though its file took a suffix.
func TestEpisodeFactFromADreamerNote(t *testing.T) {
	r := newRig(t, rigOpts{repos: []string{"demo"}})
	episodeHome(t)
	t.Setenv("TACIT_STORE", r.clone)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	note := &fact.Fact{Path: "inbox/dreamer@host-a/staging-first-2.md", Meta: fact.Meta{Name: "staging-first", Description: "release candidates go to staging first",
		Type: "unclassified", Scope: "projects/demo", Created: "2026-11-01", Source: fact.Source{Harness: "dreamer", Host: "host-a", Kind: "observed", Session: episodeSessionID}}, Body: "staging"}
	b, err := note.Encode()
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(r.clone, "inbox", "dreamer@host-a"), 0o755)
	os.WriteFile(filepath.Join(r.clone, filepath.FromSlash(note.Path)), b, 0o644)
	writeFact(t, r.clone, fact.Meta{Name: "demo-candidates-go-to-staging", Description: "release candidates are deployed to staging first",
		Type: "design_decision", Scope: "projects/demo", Origin: "staging-first"}, "staging first")
	out, code := episodeOut(t, "--fact", "demo-candidates-go-to-staging")
	if code != 0 || !strings.Contains(out, "came from (claude)") || !strings.Contains(out, "\n- ") {
		t.Fatalf("a dreamer note's session, a hit by its words (%d):\n%s", code, out)
	}
}

// A named session is looked up under every harness, the caller's first: a
// Codex rollout from a Claude Code caller.
func TestEpisodeNamedSessionOfAnotherHarness(t *testing.T) {
	home := episodeHome(t)
	id := "019a0000-aaaa-7bbb-8ccc-dddddddddddd"
	writeTranscript(t, filepath.Join(home, ".codex", "sessions", "2026", "11", "04", "rollout-2026-11-04T10-00-00-"+id+".jsonl"),
		`{"timestamp":"2026-11-04T10:00:00Z","type":"session_meta","payload":{"id":"`+id+`","cwd":"/work/demo","originator":"codex_cli_rs","source":"cli"}}`,
		`{"timestamp":"2026-11-04T10:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"look up ticket TK-555123"}]}}`)
	if out, code := episodeOut(t, "--session", id, "find", "TK-555123"); code != 0 || !strings.Contains(out, "(codex)") {
		t.Fatalf("a Codex session from a Claude Code caller (%d):\n%s", code, out)
	}
	if out, code := episodeOut(t, "--harness", "claude", "--session", id, "find", "TK-555123"); code != 1 || !strings.Contains(out, "not on this machine") {
		t.Fatalf("a named harness restricts the lookup (%d):\n%s", code, out)
	}
}

// A read past its budget says so: find, that the newer events were not
// searched; around and show, that it stopped before the ref. What a read
// found is redacted on a clock of its own.
func TestEpisodeTimeLimit(t *testing.T) {
	episodeHome(t)
	prev := episodeBudget
	episodeBudget = time.Nanosecond
	t.Cleanup(func() { episodeBudget = prev })
	if out, _ := episodeOut(t, "find", "CR-123456789"); !strings.Contains(out, "the newer events were not searched") {
		t.Fatalf("find past the budget:\n%s", out)
	}
	if out, code := episodeOut(t, "show", "999.0"); code != 1 || !strings.Contains(out, "time limit before 999.0") {
		t.Fatalf("show past the budget (%d):\n%s", code, out)
	}
	// The redaction runs on its own clock, not the read's: a caller's context
	// whose read budget is spent still redacts.
	got, err := redactFound(context.Background(), []events.Event{{Text: "token Zq7Vx9"}})
	if err != nil || len(got) != 1 || strings.Contains(got[0].Text, "Zq7Vx9") {
		t.Fatalf("redaction after the read's deadline: %v %+v", err, got)
	}
	// A caller that went away gets nothing.
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := redactFound(gone, []events.Event{{Text: "token Zq7Vx9"}}); err == nil || got != nil {
		t.Fatalf("redaction for a caller that went away: %v %+v", err, got)
	}
}

// A rune whose lower case is longer (U+023A) does not throw the clip off.
func TestEpisodeClipKeepsToRunes(t *testing.T) {
	text := strings.Repeat("\u023a", 300) + " needle here"
	if got := clipLine(text, []string{"needle"}, 40); !strings.Contains(got, "needle") {
		t.Fatalf("clip: %q", got)
	}
}

// find matches the redacted text only, so it confirms nothing a secret alone
// holds: part of the masked token finds no event, and words the redactor
// masks are not searched for.
func TestEpisodeFindIsNoOracleForSecrets(t *testing.T) {
	episodeHome(t)
	if out, code := episodeOut(t, "find", "zq7"); code != 0 || !strings.Contains(out, "# tacit: 0 event(s)") {
		t.Fatalf("part of a secret (%d):\n%s", code, out)
	}
	if out, code := episodeOut(t, "find", "Zq7Vx9"); code != 1 || !strings.Contains(out, "masks: not searched") || strings.Contains(out, "Zq7Vx9") {
		t.Fatalf("the secret itself (%d):\n%s", code, out)
	}
}

// A fact whose words are all short still has an anchor, and a curated fact
// whose origin note is in this machine's overlay finds it there.
func TestEpisodeFactShortWordsAndAnOverlayNote(t *testing.T) {
	r := newRig(t, rigOpts{repos: []string{"demo"}})
	episodeHome(t)
	t.Setenv("TACIT_STORE", r.clone)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	local := filepath.Join(r.root, "local")
	t.Setenv("TACIT_LOCAL", local)
	note := &fact.Fact{Path: "inbox/dreamer@host-a/the-candidate.md", Meta: fact.Meta{Name: "the-candidate", Description: "the release candidate",
		Type: "unclassified", Scope: "projects/demo", Created: "2026-11-01", Source: fact.Source{Harness: "dreamer", Host: "host-a", Kind: "observed", Session: episodeSessionID}}, Body: "x"}
	b, err := note.Encode()
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(local, "inbox", "dreamer@host-a"), 0o755)
	os.WriteFile(filepath.Join(local, filepath.FromSlash(note.Path)), b, 0o644)
	writeFact(t, r.clone, fact.Meta{Name: "demo-the-candidate", Description: "the RC to the stage", Type: "design_decision", Scope: "projects/demo", Origin: "the-candidate"}, "x")
	if out, code := episodeOut(t, "--fact", "demo-the-candidate"); code != 0 || !strings.Contains(out, "came from (claude)") {
		t.Fatalf("an overlay note's session, short words (%d):\n%s", code, out)
	}
}

// A harness started from another's shell inherits that one's session id:
// with two set, the caller names its harness. A Hermes caller (whose own
// sessions are not read yet) names another harness's session.
func TestEpisodeSessionOfNestedAndHermesCallers(t *testing.T) {
	episodeHome(t)
	t.Setenv("CODEX_THREAD_ID", "019a0000-aaaa-7bbb-8ccc-eeeeeeeeeeee")
	if out, code := episodeOut(t, "find", "CR-123456789"); code != 1 || !strings.Contains(out, "pass --harness") {
		t.Fatalf("two harnesses' ids (%d):\n%s", code, out)
	}
	if out, code := episodeOut(t, "--harness", "claude", "find", "CR-123456789"); code != 0 || !strings.Contains(out, "(claude)") {
		t.Fatalf("--harness claude (%d):\n%s", code, out)
	}
	s := &store.Store{Dir: t.TempDir()}
	out, err := episodeAnswer(context.Background(), s, episodeQuery{Harness: "hermes", Session: episodeSessionID, Find: "CR-123456789"})
	if err != nil || !strings.Contains(out, "(claude)") {
		t.Fatalf("a Hermes caller naming a Claude Code session: %v\n%s", err, out)
	}
}

// writeClaudeSession writes a Claude Code session of agent text events
// (and an owner's first turn when owner is not empty) under home.
func writeClaudeSession(t *testing.T, home, id, owner string, texts ...string) {
	t.Helper()
	head := `"sessionId":"` + id + `","cwd":"/work/demo","entrypoint":"cli"`
	var lines []string
	if owner != "" {
		lines = append(lines, `{"type":"user",`+head+`,"promptSource":"typed","uuid":"o1","timestamp":"2026-11-01T09:00:00Z","message":{"role":"user","content":"`+owner+`"}}`)
	}
	for i, x := range texts {
		lines = append(lines, fmt.Sprintf(`{"type":"assistant",%s,"uuid":"x%d","timestamp":"2026-11-01T10:%02d:00Z","message":{"role":"assistant","content":[{"type":"text","text":"%s"}]}}`, head, i, i%60, x))
	}
	writeTranscript(t, filepath.Join(home, ".claude", "projects", "-work-demo", id+".jsonl"), lines...)
}

// Events a secret alone matches take no real match's place: 20 newer events
// whose secret holds the words do not hide 5 older ones that say them.
func TestEpisodeFindSecretsTakeNoPlace(t *testing.T) {
	home := episodeHome(t)
	id := "6c6c6c6c-1111-4222-8333-444455556666"
	var texts []string
	for range 5 {
		texts = append(texts, "configured the vxlan bridge")
	}
	for range 20 {
		texts = append(texts, "token Zq7Vx9 used")
	}
	writeClaudeSession(t, home, id, "", texts...)
	out, code := episodeOut(t, "--session", id, "find", "vx")
	if code != 0 || !strings.Contains(out, "# tacit: 5 event(s)") || strings.Contains(out, "# older") {
		t.Fatalf("find past secret-only matches (%d):\n%s", code, out)
	}
	// More real matches than a page: the next page starts at the last shown.
	texts = nil
	for i := range 30 {
		texts = append(texts, fmt.Sprintf("bridge br%d up", i))
	}
	writeClaudeSession(t, home, id, "", texts...)
	out, code = episodeOut(t, "--session", id, "find", "bridge")
	refs := refRx.FindAllStringSubmatch(out, -1)
	if code != 0 || len(refs) != 10 || !strings.Contains(out, "# older: the same find with --before "+refs[9][1]+"\n") {
		t.Fatalf("a page of 10 and the next (%d):\n%s", code, out)
	}
	// The hint followed as written: the flag after the words.
	next, code := episodeOut(t, "--session", id, "find", "bridge", "--before", refs[9][1])
	nrefs := refRx.FindAllStringSubmatch(next, -1)
	if code != 0 || len(nrefs) != 10 || strings.Contains(next, "- "+refs[9][1]+" ") || !strings.Contains(next, "br19 up") {
		t.Fatalf("the next page, --before after the words (%d):\n%s", code, next)
	}
}

// A fact's quote reaches the owner's turn in a long session: its common
// words are not searched, and the events holding the most come first.
func TestEpisodeFactQuoteInALongSession(t *testing.T) {
	r := newRig(t, rigOpts{repos: []string{"demo"}})
	home := episodeHome(t)
	t.Setenv("TACIT_STORE", r.clone)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	id := "7d7d7d7d-1111-4222-8333-444455556666"
	var texts []string
	for i := range 30 {
		texts = append(texts, fmt.Sprintf("step %d: checked the logs", i))
	}
	writeClaudeSession(t, home, id, "deploy the release candidate to staging first", texts...)
	writeFact(t, r.clone, fact.Meta{Name: "demo-rc-staging", Description: "release candidates go to staging first",
		Type: "user_standard", Scope: "projects/demo", Source: fact.Source{Kind: "user", OwnerQuote: "deploy the release candidate to staging first", OwnerQuoteFrom: "claude:" + id}}, "x")
	out, code := episodeOut(t, "--fact", "demo-rc-staging")
	if code != 0 || !strings.Contains(out, "owner owner_text") || strings.Contains(out, "checked the logs") || strings.Contains(out, "# older") {
		t.Fatalf("a quote in a long session (%d):\n%s", code, out)
	}
}

// A recall call is one that runs tacit episode, in the forms the shell tools
// of Claude Code, Codex and pi give it; a mention of the words is not one.
func TestEpisodeRecallCallForms(t *testing.T) {
	for _, c := range []struct {
		tool, input string
		want        bool
	}{
		{"Bash", `{"command":"tacit episode find X"}`, true},
		{"Bash", `{"command":"cd /w && TACIT_X=1 ~/.local/bin/tacit episode find X | head"}`, true},
		{"Bash", `{"command":"env A=1 tacit episode find X"}`, true},
		{"Bash", `{"command":"timeout 30 tacit episode find X"}`, true},
		{"Bash", `{"command":"nice -n 10 tacit episode find X"}`, true},
		{"Bash", `{"command":"bash -c 'tacit episode find X'"}`, true},
		{"shell", `{"command":["bash","-lc","tacit episode find X"]}`, true},
		{"local_shell", `{"command":["bash","-lc","cd /w; tacit episode around 1.0"]}`, true},
		{"exec_command", `{"cmd":"tacit episode show 1.0"}`, true},
		{"exec", `await tools.exec_command({cmd: 'tacit episode find X'})`, true},
		{"bash", `{"command":"C:\\bin\\tacit.exe episode find X"}`, true},
		{"mcp__tacit__tacit_episode", `{"find":"X"}`, true},
		{"Bash", `{"command":"git commit -m \"docs; tacit episode find works\""}`, false},
		{"Bash", `{"command":"rg 'tacit episode' docs"}`, false},
		{"Bash", `{"command":"echo tacit episode"}`, false},
		{"shell", `{"command":["bash","-lc","grep -n episode SPEC.md"]}`, false},
		{"Bash", `{"command":"sudo -u avi tacit episode find X"}`, true},
		{"Bash", `{"command":"env -u FOO tacit episode find X"}`, true},
		{"Bash", `{"command":"sudo --user=avi tacit episode find X"}`, true},
		{"Bash", `{"command":"cat > brief.md <<'EOF'\nrun tacit episode find X\ntacit episode find X\nEOF\ngit add brief.md"}`, false},
		{"Bash", `{"command":"cat <<EOF > x\nbody\nEOF\ntacit episode find X"}`, true},
		{"Bash", `{"command":"echo \"$(tacit episode find X)\""}`, true},
		{"Bash", `{"command":"(tacit episode find X)"}`, true},
		{"Bash", `{"command":"{ tacit episode find X; }"}`, true},
		{"Bash", `{"command":"if tacit episode find X; then :; fi"}`, true},
		{"Bash", `{"command":"for w in a b; do tacit episode find $w; done"}`, true},
		{"Bash", `{"command":"! tacit episode find X"}`, true},
		{"Bash", `{"command":"echo a | xargs tacit episode find"}`, true},
		{"Bash", `{"command":"stdbuf -oL tacit episode find X"}`, true},
		{"Bash", `{"command":"echo $((1<<2))\ntacit episode find X"}`, true},
		{"Bash", `{"command":"grep -c 'episode' notes.md"}`, false},
		{"Bash", "{\"command\":\"git commit -m \\\"$(cat <<'EOF'\\nepisode: add `tacit episode find`\\nEOF\\n)\\\"\"}", false},
		{"Bash", "{\"command\":\"gh pr create --body 'run `tacit episode find X` after a compaction'\"}", false},
		{"Bash", "{\"command\":\"rg -n '`tacit episode' docs\"}", false},
		{"Bash", `{"command":"echo 'see $(tacit episode find X)'"}`, false},
		{"Bash", "{\"command\":\"echo \\\"`tacit episode find X`\\\"\"}", true},
		{"Edit", `{"new_string":"tacit episode find X"}`, false},
	} {
		ev := events.Event{Kind: events.KindToolCall, ToolName: c.tool, ToolInput: c.input}
		if got := recallCall(ev); got != c.want {
			t.Errorf("recallCall(%s %s) = %v, want %v", c.tool, c.input, got, c.want)
		}
	}
}

// A Hermes caller's own session is said to be not read yet, and nothing
// about other harnesses, which were not looked in.
func TestEpisodeHermesCallersOwnSession(t *testing.T) {
	episodeHome(t)
	t.Setenv("TACIT_SESSION", "hermes:20261101_100000_abcdef")
	s := &store.Store{Dir: t.TempDir()}
	_, err := episodeAnswer(context.Background(), s, episodeQuery{Harness: "hermes", Find: "x"})
	if err == nil || err.Error() != "Hermes sessions are not read yet" {
		t.Fatalf("a Hermes caller's own session: %v", err)
	}
}

// When the redaction's clock runs out, find answers with what it redacted and
// says how many events it did not check: no failure, and a pager that names
// only an event it showed.
func TestEpisodeFindPartialRedaction(t *testing.T) {
	home := episodeHome(t)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "scrump"), []byte("#!/bin/sh\nsleep 0.3\nexec sed 's/Zq7Vx9/******/g'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	prev := episodeRedactWait
	episodeRedactWait = time.Second
	t.Cleanup(func() { episodeRedactWait = prev })
	id := "8e8e8e8e-1111-4222-8333-444455556666"
	var texts []string
	texts = append(texts, "the vxlan bridge came up")
	for range 1500 {
		texts = append(texts, "token Zq7Vx9 used")
	}
	texts = append(texts, "the vxlan bridge went down")
	writeClaudeSession(t, home, id, "", texts...)
	out, code := episodeOut(t, "--session", id, "find", "vx")
	if code != 0 || !strings.Contains(out, "# partial: not every matching event was checked") || !strings.Contains(out, "went down") {
		t.Fatalf("a partial answer (%d):\n%s", code, out)
	}
	refs := refRx.FindAllStringSubmatch(out, -1)
	if len(refs) != 1 || !strings.Contains(out, "# older: the same find with --before "+refs[0][1]+"\n") {
		t.Fatalf("the pager names the event shown (%d):\n%s", code, out)
	}
}

// A fact's words: one the redactor masks is left out, not the query, and of
// events holding as many words the newest come first, whatever a secret in
// older ones holds.
func TestEpisodeFactTiesAndMaskedWords(t *testing.T) {
	r := newRig(t, rigOpts{repos: []string{"demo"}})
	home := episodeHome(t)
	t.Setenv("TACIT_STORE", r.clone)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	id := "9f9f9f9f-1111-4222-8333-444455556666"
	var texts []string
	for i := range 50 {
		texts = append(texts, fmt.Sprintf("release Zq7Vx9 old%d", i))
	}
	for i := range 12 {
		texts = append(texts, fmt.Sprintf("release done new%d", i))
	}
	writeClaudeSession(t, home, id, "", texts...)
	writeFact(t, r.clone, fact.Meta{Name: "demo-release-token", Description: "release with the token",
		Type: "user_standard", Scope: "projects/demo", Source: fact.Source{Kind: "user", OwnerQuote: "release Zq7Vx9", OwnerQuoteFrom: "claude:" + id}}, "x")
	out, code := episodeOut(t, "--fact", "demo-release-token")
	if code != 0 || strings.Contains(out, " old") || !strings.Contains(out, "new11") || !strings.Contains(out, "new2") || strings.Contains(out, "Zq7Vx9") {
		t.Fatalf("ties by recency, the masked word left out (%d):\n%s", code, out)
	}
}

// The MCP server lists tacit_episode and answers through it: a find in the
// session whose id the server's environment gives, and a refusal as text.
func TestEpisodeMCPTool(t *testing.T) {
	episodeHome(t)
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	ss, err := storeServer(&store.Store{Dir: t.TempDir()}, "claude").Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	listed := false
	for _, tl := range tools.Tools {
		listed = listed || tl.Name == "tacit_episode"
	}
	if !listed {
		t.Fatal("tacit_episode is not listed")
	}
	call := func(args map[string]any) string {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "tacit_episode", Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				b.WriteString(tc.Text)
			}
		}
		return b.String()
	}
	if out := call(map[string]any{"find": "CR-123456789"}); !strings.Contains(out, "event(s) of this session (claude)") || strings.Contains(out, "Zq7Vx9") {
		t.Fatalf("find through MCP:\n%s", out)
	}
	if out := call(map[string]any{"session": "9f9f9f9f-0000-4000-8000-000000000000", "find": "x"}); !strings.HasPrefix(out, "# tacit: the transcript of") {
		t.Fatalf("an absent session through MCP:\n%s", out)
	}
	if out := call(map[string]any{"find": "a", "show": "1.0"}); !strings.HasPrefix(out, "# tacit: give one of") {
		t.Fatalf("two operations through MCP:\n%s", out)
	}
}

// A person's own `!tacit episode ...` in Claude Code's shell mode, and its
// output, are left out like the agent's calls; other shell turns stay.
func TestEpisodeShellModeRecall(t *testing.T) {
	home := episodeHome(t)
	id := "aaaa1111-1111-4222-8333-444455556666"
	head := `"sessionId":"` + id + `","cwd":"/work/demo","entrypoint":"cli"`
	writeTranscript(t, filepath.Join(home, ".claude", "projects", "-work-demo", id+".jsonl"),
		`{"type":"user",`+head+`,"uuid":"u1","timestamp":"2026-11-01T10:00:00Z","message":{"role":"user","content":"ship TK-424242 today"}}`,
		`{"type":"user",`+head+`,"uuid":"u2","timestamp":"2026-11-01T10:01:00Z","message":{"role":"user","content":"<bash-input>tacit episode find TK-424242</bash-input>"}}`,
		`{"type":"user",`+head+`,"uuid":"u3","timestamp":"2026-11-01T10:01:01Z","message":{"role":"user","content":"<bash-stdout>- 1.0 owner owner_text: ship TK-424242 today</bash-stdout><bash-stderr></bash-stderr>"}}`,
		`{"type":"user",`+head+`,"uuid":"u4","timestamp":"2026-11-01T10:02:00Z","message":{"role":"user","content":"<bash-input>echo TK-424242</bash-input>"}}`,
		`{"type":"user",`+head+`,"uuid":"u5","timestamp":"2026-11-01T10:02:01Z","message":{"role":"user","content":"<bash-stdout>TK-424242</bash-stdout><bash-stderr></bash-stderr>"}}`)
	out, code := episodeOut(t, "--session", id, "find", "TK-424242")
	if code != 0 || !strings.Contains(out, "# tacit: 3 event(s)") || strings.Contains(out, "tacit episode") || strings.Contains(out, "- 1.0 owner") {
		t.Fatalf("shell-mode recall (%d):\n%s", code, out)
	}
}

// After the words only --name is a flag: a search for a command keeps its
// short flags.
func TestEpisodeFindWordsWithShortFlags(t *testing.T) {
	home := episodeHome(t)
	id := "bbbb1111-1111-4222-8333-444455556666"
	writeClaudeSession(t, home, id, "", "ran grep -n CR-77 there")
	if out, code := episodeOut(t, "--session", id, "find", "grep", "-n", "CR-77"); code != 0 || !strings.Contains(out, "# tacit: 1 event(s)") {
		t.Fatalf("short flags as words (%d):\n%s", code, out)
	}
}

// A caller that goes away while find redacts gets no answer.
func TestEpisodeCancelledCaller(t *testing.T) {
	episodeHome(t)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "scrump"), []byte("#!/bin/sh\nsleep 0.4\nexec sed 's/Zq7Vx9/******/g'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	out, err := episodeAnswer(ctx, &store.Store{Dir: t.TempDir()}, episodeQuery{Harness: "claude", Find: "CR-123456789"})
	if err == nil || out != "" {
		t.Fatalf("a cancelled caller: %v\n%s", err, out)
	}
}

// More raw matches than find keeps (by count, or by bytes) and nothing real
// to page from: a line says older events were not searched, and no ref.
func TestEpisodeFindCandidateBounds(t *testing.T) {
	home := episodeHome(t)
	id := "cccc1111-1111-4222-8333-444455556666"
	var texts []string
	texts = append(texts, "the vxlan bridge came up")
	for range 40 {
		texts = append(texts, "token Zq7Vx9 used")
	}
	writeClaudeSession(t, home, id, "", texts...)
	prevN, prevB := episodeCandidates, episodeCandidateBytes
	t.Cleanup(func() { episodeCandidates, episodeCandidateBytes = prevN, prevB })
	for _, c := range []struct{ n, b int }{{20, 64 << 20}, {5000, 300}} {
		episodeCandidates, episodeCandidateBytes = c.n, c.b
		out, code := episodeOut(t, "--session", id, "find", "vx")
		if code != 0 || !strings.Contains(out, "# tacit: 0 event(s)") || !strings.Contains(out, "older events were not searched") || refRx.MatchString(out) {
			t.Fatalf("bounds %d events, %d bytes (%d):\n%s", c.n, c.b, code, out)
		}
	}
}

// show cuts each part at 8,000 characters and fences it longer than any run
// of ~ it holds; around takes at most 25 on each side.
func TestEpisodeShowCutAndAroundCap(t *testing.T) {
	home := episodeHome(t)
	id := "dddd1111-1111-4222-8333-444455556666"
	texts := []string{"long " + strings.Repeat("~", 5) + strings.Repeat("x", 9000)}
	for i := range 60 {
		texts = append(texts, fmt.Sprintf("step %d", i))
	}
	writeClaudeSession(t, home, id, "", texts...)
	found, _ := episodeOut(t, "--session", id, "find", "long")
	refs := refRx.FindAllStringSubmatch(found, -1)
	if len(refs) != 1 {
		t.Fatalf("find long:\n%s", found)
	}
	show, code := episodeOut(t, "--session", id, "show", refs[0][1])
	if code != 0 || !strings.Contains(show, "\n~~~~~~\nlong ~~~~~x") || !strings.Contains(show, "(cut at 8000 characters)") || strings.Count(show, "x") > 8000 {
		t.Fatalf("show's cut and fence (%d):\n%.300s", code, show)
	}
	mid, _ := episodeOut(t, "--session", id, "find", "step 30")
	mrefs := refRx.FindAllStringSubmatch(mid, -1)
	if len(mrefs) != 1 {
		t.Fatalf("find step 30:\n%s", mid)
	}
	around, code := episodeOut(t, "--session", id, "--n", "40", "around", mrefs[0][1])
	if code != 0 || strings.Count(around, "\n- ") != 50 {
		t.Fatalf("around caps n at 25 (%d): %d lines", code, strings.Count(around, "\n- "))
	}
}

// A fact whose owner's words (too few to prove a ruling) were said in a
// session leads there too.
func TestEpisodeFactOwnerWords(t *testing.T) {
	r := newRig(t, rigOpts{repos: []string{"demo"}})
	episodeHome(t)
	t.Setenv("TACIT_STORE", r.clone)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	writeFact(t, r.clone, fact.Meta{Name: "demo-candidates", Description: "candidates are staged",
		Type: "design_decision", Scope: "projects/demo", Source: fact.Source{OwnerWords: "release candidate", OwnerWordsFrom: "claude:" + episodeSessionID}}, "x")
	out, code := episodeOut(t, "--fact", "demo-candidates")
	if code != 0 || !strings.Contains(out, "owner owner_text") {
		t.Fatalf("owner words (%d):\n%s", code, out)
	}
}

// A word of the redactor's mask is not searched for: it would list where
// secrets are.
func TestEpisodeFindRefusesTheMask(t *testing.T) {
	episodeHome(t)
	if out, code := episodeOut(t, "find", "******"); code != 1 || !strings.Contains(out, "the redactor's mask") {
		t.Fatalf("a find of the mask (%d):\n%s", code, out)
	}
}

// Owner's words of no word that says something: the description's words
// anchor the fact instead.
func TestEpisodeFactShortOwnerWords(t *testing.T) {
	r := newRig(t, rigOpts{repos: []string{"demo"}})
	episodeHome(t)
	t.Setenv("TACIT_STORE", r.clone)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	writeFact(t, r.clone, fact.Meta{Name: "demo-do-it", Description: "release candidates are staged",
		Type: "design_decision", Scope: "projects/demo", Source: fact.Source{OwnerWords: "do it so", OwnerWordsFrom: "claude:" + episodeSessionID}}, "x")
	out, code := episodeOut(t, "--fact", "demo-do-it")
	if code != 0 || !strings.Contains(out, "holding words of \"release candidates staged\"") || !strings.Contains(out, "owner owner_text") {
		t.Fatalf("short owner words (%d):\n%s", code, out)
	}
}
