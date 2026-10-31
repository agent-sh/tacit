package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/agentreview"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/lint"
	"github.com/agent-sh/tacit/client/internal/overlay"
	"github.com/agent-sh/tacit/client/internal/platform"
	"github.com/agent-sh/tacit/client/internal/record"
	"github.com/agent-sh/tacit/client/internal/search"
	"github.com/agent-sh/tacit/client/internal/store"
)

// reviewerCLI is a stand-in for the model CLI: the reviewer's prompt gets the
// reply in $FIXTURE_REPLIES/<note name>.json when there is one, else the
// verdict its note's name asks for (note-wrong: wrong, note-unsure:
// uncertain with its core in doubt, a decision; any other: correct), the
// rulings pass nothing, the split $FIXTURE_SPLIT. Every prompt is logged to
// $FIXTURE_PROMPTS.
func reviewerCLI(t *testing.T, dir string) func() []string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell CLI fixture")
	}
	log := filepath.Join(dir, "prompts")
	t.Setenv("FIXTURE_PROMPTS", log)
	fake := filepath.Join(dir, "fake-cli")
	script := `#!/bin/sh
in=$(cat)
printf '%s\n\036\n' "$in" >> "$FIXTURE_PROMPTS"
case "$in" in
*"You review a note an agent saved"*)
  name=$(printf '%s\n' "$in" | sed -n 's/^where: tacit scope .*, name //p' | head -n 1)
  if [ -n "$FIXTURE_REPLIES" ] && [ -f "$FIXTURE_REPLIES/$name.json" ]; then cat "$FIXTURE_REPLIES/$name.json"; exit 0; fi
  x=""
  case "$in" in
  *"name note-wrong"*) v=wrong ;;
  *"name note-unsure"*) v=uncertain; x=',\"doubt\":\"core\",\"kind\":\"decision\"' ;;
  *) v=correct ;;
  esac
  printf '{"result":"{\\"verdict\\":\\"%s\\",\\"reason\\":\\"fixture says %s\\"%s}","total_cost_usd":0.05}\n' "$v" "$v" "$x" ;;
*"OWNER RULINGS already extracted"*) printf '%s\n' '{"result":"[]","total_cost_usd":0.01}' ;;
*) printf '%s\n' "$FIXTURE_SPLIT" ;;
esac
`
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TACIT_CLAUDE_COMMAND", fake)
	for _, k := range []string{"TACIT_CURATE_RUNNER", "TACIT_JUDGE_RUNNER", "TACIT_REVIEW_RUNNER"} {
		t.Setenv(k, "claude")
	}
	t.Setenv("TACIT_RULEBOOK_LOG", "off")
	t.Setenv("TACIT_HERMES_DB", "off")
	t.Setenv("TACIT_REVIEWER", "")
	return func() []string {
		b, _ := os.ReadFile(log)
		var out []string
		for _, p := range strings.Split(string(b), "\n\x1e\n") {
			if strings.TrimSpace(p) != "" {
				out = append(out, p)
			}
		}
		return out
	}
}

// claudeSession writes a Claude Code transcript of an owner's session (typed
// turns marked human) under home and returns its path.
func claudeSession(t *testing.T, home, id, cwd string, lines ...string) string {
	t.Helper()
	p := filepath.Join(home, ".claude", "projects", "-work", id+".jsonl")
	os.MkdirAll(filepath.Dir(p), 0o700)
	head := `{"type":"user","entrypoint":"cli","cwd":"` + cwd + `","sessionId":"` + id + `","origin":{"kind":"human"},"message":{"content":"lets set up the walrus service"}}`
	if err := os.WriteFile(p, []byte(strings.Join(append([]string{head}, lines...), "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func appendLine(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		f.WriteString(l + "\n")
	}
}

func ownerTurn(session, text string) string {
	b, _ := json.Marshal(text)
	return `{"type":"user","entrypoint":"cli","sessionId":"` + session + `","origin":{"kind":"human"},"message":{"content":` + string(b) + `}}`
}

func relayedTurn(session, text string) string {
	b, _ := json.Marshal(text)
	return `{"type":"user","entrypoint":"cli","sessionId":"` + session + `","origin":{"kind":"task-notification"},"message":{"content":` + string(b) + `}}`
}

func agentTurn(session, text string) string {
	b, _ := json.Marshal(text)
	return `{"type":"assistant","entrypoint":"cli","sessionId":"` + session + `","message":{"content":[{"type":"text","text":` + string(b) + `}]}}`
}

func addCall(session, name, desc string) string {
	b, _ := json.Marshal(map[string]string{"command": "tacit add --harness claude --desc '" + desc + "' " + name})
	return `{"type":"assistant","entrypoint":"cli","sessionId":"` + session + `","message":{"content":[{"type":"tool_use","name":"Bash","input":` + string(b) + `}]}}`
}

func readQueue(t *testing.T, home string) *agentreview.Queue {
	t.Helper()
	q, err := agentreview.Load(agentreview.Path(platform.StateDir(home)))
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func exactIndex(t *testing.T, s *store.Store) *search.Index {
	t.Helper()
	ctx := context.Background()
	head, err := s.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ix, err := search.Open(ctx, s.Dir, head)
	if err != nil {
		t.Fatal(err)
	}
	return ix
}

func promptHook(t *testing.T, session, transcript, cwd, prompt string) string {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"session_id": session, "transcript_path": transcript, "cwd": cwd, "prompt": prompt, "hook_event_name": "UserPromptSubmit"})
	return hookOut(t, string(b), "prompt")
}

// A fresh store on a writer: agents' notes from a Claude Code session are
// judged against it before they are curated. The correct and the uncertain
// ones are curated with the verdict on them (format 4), the wrong one is not
// (it stays in the inbox, its reason in the ledger), one with no session is a
// low-weight note with no model call. The uncertain one is a question in the
// next session in its repository, and nowhere else; the owner's own yes in
// that session confirms it, a relayed yes does not.
func TestReviewerOnAStoreEndToEnd(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	for k, v := range map[string]string{"HOME": home, "USERPROFILE": home, "TACIT_STATE_DIR": "", "XDG_CACHE_HOME": filepath.Join(root, "cache"),
		"TACIT_STORE": filepath.Join(root, "facts"), "TACIT_INDEX": filepath.Join(root, "index"), "TACIT_LOCAL": filepath.Join(root, "overlay"),
		"TACIT_PROJECTS": filepath.Join(root, "projects"), "TACIT_SETTINGS": "", "TACIT_SESSION": "", "CODEX_THREAD_ID": "", "PI_SESSION_ID": ""} {
		t.Setenv(k, v)
	}
	os.MkdirAll(home, 0o700)
	prompts := reviewerCLI(t, root)
	stubScrump(t)
	repo := filepath.Join(root, "projects", "demo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code := runInit(nil); code != 0 {
		t.Fatalf("init: %d", code)
	}
	s := &store.Store{Dir: filepath.Join(root, "facts")}
	ctx := context.Background()
	// demo is an active project: its notes' doubts may be questions.
	putRecordFile(t, s.Dir, testRec(record.KindRepo, "projects/demo"))
	git(t, s.Dir, "add", "-A")
	git(t, s.Dir, "commit", "-q", "-m", "demo's record")

	notes := map[string]string{
		"note-correct": "demo builds its release with make release",
		"note-unsure":  "demo releases always go out on fridays",
		"note-wrong":   "demo installs its packages with npm",
		"note-unsure2": "demo deploys go through the staging bucket",
	}
	var calls []string
	for _, n := range []string{"note-correct", "note-unsure", "note-wrong", "note-unsure2"} {
		calls = append(calls, addCall("sess-a", n, notes[n]))
	}
	claudeSession(t, home, "sess-a", "/home/u/projects/demo", append([]string{ownerTurn("sess-a", "the demo release is built with make release, and we use pnpm not npm")}, calls...)...)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sess-a")
	for _, n := range []string{"note-correct", "note-unsure", "note-wrong", "note-unsure2"} {
		if _, err := addFact(ctx, s, "claude", fact.Meta{Name: n, Description: notes[n], Type: "design_decision", Scope: "projects/demo"}, notes[n]); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	if _, err := addFact(ctx, s, "claude", fact.Meta{Name: "note-nosrc", Description: "demo keeps its docs in the docs folder", Type: "design_decision", Scope: "projects/demo"}, "demo keeps its docs in the docs folder"); err != nil {
		t.Fatal(err)
	}
	var entries []map[string]any
	for _, n := range []string{"note-correct", "note-unsure", "note-nosrc", "note-unsure2"} {
		d := notes[n]
		if d == "" {
			d = "demo keeps its docs in the docs folder"
		}
		entries = append(entries, map[string]any{"origin": n, "name": n + "-fact", "description": d, "type": "design_decision", "scope": "projects/demo", "kind": "observed", "body": d})
	}
	b, _ := json.Marshal(entries)
	out, _ := json.Marshal(map[string]any{"result": string(b), "total_cost_usd": .01})
	t.Setenv("FIXTURE_SPLIT", string(out))

	if code := runCurate([]string{"--quick"}); code != 0 {
		t.Fatalf("curate exit %d", code)
	}
	reviews := ledger.ParseReviews(readBytes(t, filepath.Join(s.Dir, ledger.ReviewsPath)))
	want := map[string]string{"note-correct": "correct", "note-unsure": "uncertain", "note-wrong": "wrong", "note-nosrc": "nosource", "note-unsure2": "uncertain"}
	for n, v := range want {
		l, ok := reviews.ForOrigin(n)
		if !ok || l.Verdict != v || l.By != ledger.ByTacit {
			t.Fatalf("%s: %+v %v, want %s", n, l, ok, v)
		}
		if v != "nosource" && l.Session != "claude:sess-a" {
			t.Fatalf("%s judged against %q", n, l.Session)
		}
	}
	reviewerPrompts, splitPrompts := 0, 0
	for _, p := range prompts() {
		switch {
		case strings.Contains(p, "You review a note an agent saved"):
			reviewerPrompts++
			if !strings.Contains(p, "OWNER: the demo release is built with make release") {
				t.Fatalf("the reviewer did not read the session:\n%s", p)
			}
		case strings.Contains(p, "OWNER RULINGS already extracted"):
		default:
			splitPrompts++
			if strings.Contains(p, "note-wrong") {
				t.Fatalf("a note judged wrong reached the split:\n%s", p)
			}
		}
	}
	if reviewerPrompts != 4 || splitPrompts == 0 {
		t.Fatalf("%d reviewer prompts (want 4: no call for no session), %d split prompts", reviewerPrompts, splitPrompts)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "inbox", "claude@"+store.ShortHost(), "note-wrong.md")); err != nil {
		t.Fatalf("the wrong note left the inbox: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "projects", "demo", "note-wrong-fact.md")); err == nil {
		t.Fatal("a note judged wrong was curated")
	}
	facts, _, err := fact.Walk(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*fact.Fact{}
	for _, f := range facts {
		byName[f.Meta.Name] = f
	}
	for n, v := range map[string]string{"note-correct-fact": "correct", "note-unsure-fact": "uncertain", "note-nosrc-fact": "nosource"} {
		f := byName[n]
		if f == nil || f.Review != v || f.Meta.Source.Review != v || f.Meta.Source.ReviewReason == "" {
			t.Fatalf("%s: %+v", n, f)
		}
	}
	if findings, err := lint.Run(s.Dir); err != nil || len(findings) > 0 {
		t.Fatalf("lint: %v %v", findings, err)
	}

	// Delivery: the checked note is labelled so and outranks the unsure one.
	ix := exactIndex(t, s)
	hits, err := ix.Query(ctx, search.Situation{Repo: "demo", Query: "demo release"}, 10, false)
	ix.Close()
	if err != nil {
		t.Fatal(err)
	}
	pos := map[string]int{}
	for i, h := range hits {
		pos[h.Name] = i
	}
	if pc, okc := pos["note-correct-fact"]; !okc || pc > pos["note-unsure-fact"] {
		t.Fatalf("ranking: %+v", hits)
	}
	block := search.Block(hits, "# t")
	if !strings.Contains(block, "note-correct-fact: agent note (") || !strings.Contains(block, "checked against its session): demo builds") || !strings.Contains(block, "unconfirmed): demo releases") {
		t.Fatalf("labels:\n%s", block)
	}

	// The question: in the next session in the repository, once; not in a
	// session elsewhere about something else.
	q := readQueue(t, home)
	if open := q.Open(); len(open) != 2 || open[0].Repo != "demo" || open[1].Repo != "demo" {
		t.Fatalf("queue: %+v", q.Items)
	}
	other := claudeSession(t, home, "sess-c", "/home/u/elsewhere")
	if got := promptHook(t, "sess-c", other, "/home/u/elsewhere", "summarize what we did yesterday please"); strings.Contains(got, "tacit q-") {
		t.Fatalf("an unrelated session was asked:\n%s", got)
	}
	tb := claudeSession(t, home, "sess-b", "/home/u/projects/demo")
	got := promptHook(t, "sess-b", tb, repo, "check the build output")
	if !strings.Contains(got, questionTitle) || !strings.Contains(got, "Right? yes / no / correct it") {
		t.Fatalf("no question in the repository's session:\n%s", got)
	}
	var first, second agentreview.Item
	for _, it := range readQueue(t, home).Open() {
		if len(it.Asks) == 1 {
			first = it
		} else {
			second = it
		}
	}
	if first.ID == "" || !strings.Contains(got, first.Desc) {
		t.Fatalf("the question asked is not recorded:\n%s\n%+v", got, first)
	}
	tag := first.Text()
	if again := promptHook(t, "sess-b", tb, repo, "and the tests"); strings.Contains(again, "tacit q-") {
		t.Fatalf("asked again before the answer was read:\n%s", again)
	}

	// The agent asks; another agent relays a yes: no answer.
	appendLine(t, tb, agentTurn("sess-b", "The build is green.\n\n"+tag), relayedTurn("sess-b", "yes"), agentTurn("sess-b", "The owner said yes."))
	if code := runReviewer(nil); code != 0 {
		t.Fatalf("reviewer exit %d", code)
	}
	if it := readQueue(t, home).ByID(first.ID); it.State != agentreview.StateOpen || it.Answer != nil {
		t.Fatalf("a relayed answer counted: %+v", it)
	}
	// The owner's own yes.
	appendLine(t, tb, ownerTurn("sess-b", "yes"))
	if code := runReviewer(nil); code != 0 {
		t.Fatalf("reviewer exit %d", code)
	}
	if it := readQueue(t, home).ByID(first.ID); it.State != agentreview.StateApplied || it.Answer == nil || it.Answer.From != "claude:sess-b" {
		t.Fatalf("the owner's answer: %+v", it)
	}
	reviews = ledger.ParseReviews(readBytes(t, filepath.Join(s.Dir, ledger.ReviewsPath)))
	if l, _ := reviews.ForOrigin(first.Name); l.Verdict != ledger.VerdictConfirmed || l.By != ledger.ByOwner || l.From != "claude:sess-b" {
		t.Fatalf("ledger: %+v", l)
	}
	f, _ := fact.Parse("projects/demo/"+first.Name+"-fact.md", readBytes(t, filepath.Join(s.Dir, "projects", "demo", first.Name+"-fact.md")))
	if f.Meta.Source.Review != ledger.VerdictConfirmed {
		t.Fatalf("frontmatter: %+v", f.Meta.Source)
	}
	if findings, err := lint.Run(s.Dir); err != nil || len(findings) > 0 {
		t.Fatalf("lint after the answer: %v %v", findings, err)
	}
	ix = exactIndex(t, s)
	hits, _ = ix.Query(ctx, search.Situation{Repo: "demo", Query: first.Desc}, 10, false)
	ix.Close()
	if b := search.Block(hits, "# t"); !strings.Contains(b, first.Name+"-fact: you confirmed (") {
		t.Fatalf("confirmed label:\n%s", b)
	}
	// The session's second question; the owner says no: its fact is retired.
	next := promptHook(t, "sess-b", tb, repo, "what next")
	if strings.Contains(next, first.ID) || !strings.Contains(next, second.ID) {
		t.Fatalf("the second question:\n%s", next)
	}
	appendLine(t, tb, agentTurn("sess-b", "Next is the docs.\n\n"+second.Text()), ownerTurn("sess-b", "no, that was only for the demo week"))
	if code := runReviewer(nil); code != 0 {
		t.Fatalf("reviewer exit %d", code)
	}
	reviews = ledger.ParseReviews(readBytes(t, filepath.Join(s.Dir, ledger.ReviewsPath)))
	if l, _ := reviews.ForOrigin(second.Name); l.Verdict != ledger.VerdictRejected || l.Words != "that was only for the demo week" {
		t.Fatalf("ledger: %+v", l)
	}
	f, _ = fact.Parse("x.md", readBytes(t, filepath.Join(s.Dir, "projects", "demo", second.Name+"-fact.md")))
	if f.Meta.Retired == "" {
		t.Fatalf("the rejected note's fact is not retired: %+v", f.Meta)
	}
	if findings, err := lint.Run(s.Dir); err != nil || len(findings) > 0 {
		t.Fatalf("lint after the no: %v %v", findings, err)
	}
	// Two asked: the session is asked no more.
	if again := promptHook(t, "sess-b", tb, repo, "and then"); strings.Contains(again, "tacit q-") {
		t.Fatalf("a third question:\n%s", again)
	}
}

func readBytes(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// On a read-only clone with the local layer (a Mac): the reviewer judges
// this machine's agents' notes into the overlay's ledger. The wrong one no
// hook delivers; the checked one is labelled so. The uncertain one is asked
// in a session in the folder its session ran in, and the owner's correction
// in their own turn becomes a statement in their words that replaces it.
// Nothing reaches the clone or the server.
func TestReviewerOnTheLocalLayerEndToEnd(t *testing.T) {
	r := newLocalRig(t)
	prompts := reviewerCLI(t, r.root)
	t.Setenv("TACIT_SESSION", "")
	t.Setenv("CODEX_THREAD_ID", "")
	t.Setenv("PI_SESSION_ID", "")
	ctx := context.Background()
	work := "/home/u/work" // a transcript's cwd in a temp directory is an agent's (ownerwords.AgentCwd)
	// walrus is a checkout here, and an active project in this machine's
	// layer: a doubt about its notes may be a question.
	walrus := filepath.Join(r.root, "projects", "walrus")
	os.MkdirAll(filepath.Join(walrus, ".git"), 0o755)
	putRecordFile(t, filepath.Join(r.local, "curated"), testRec(record.KindRepo, "projects/walrus"))
	notes := map[string]string{
		"note-correct": "the walrus driver listens on port 7790",
		"note-unsure":  "walrus deploys always need the vpn",
		"note-wrong":   "walrus deploys are fine on fridays",
	}
	var calls []string
	for _, n := range []string{"note-correct", "note-unsure", "note-wrong"} {
		calls = append(calls, addCall("sess-m1", n, notes[n]))
	}
	claudeSession(t, r.home, "sess-m1", work, append([]string{ownerTurn("sess-m1", "the walrus driver is on port 7790, and never deploy walrus on fridays")}, calls...)...)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sess-m1")
	for _, n := range []string{"note-correct", "note-unsure", "note-wrong"} {
		scope := "user"
		if n == "note-unsure" {
			scope = "projects/walrus"
		}
		rel, err := addFact(ctx, r.s, "claude", fact.Meta{Name: n, Description: notes[n], Type: "machine_fact", Scope: scope}, notes[n])
		if err != nil || !strings.HasPrefix(rel, overlay.Prefix) {
			t.Fatalf("%s: %s %v", n, rel, err)
		}
	}
	// A save rulebook already reviewed in its session: its verdict is taken,
	// no model call.
	if _, err := addFact(ctx, r.s, "claude", fact.Meta{Name: "note-rb", Description: "the walrus repo uses pnpm", Type: "machine_fact", Scope: "user"}, "the walrus repo uses pnpm"); err != nil {
		t.Fatal(err)
	}
	rb := filepath.Join(r.root, "reviews.jsonl")
	os.WriteFile(rb, []byte(`{"ts":"2026-10-24T09:00:00","session":"sess-m1","cwd":"/home/u/work","model":"opus","kind":"tacit","title":"the walrus repo uses pnpm","name":"note-rb","verdict":"correct","reason":"the owner said pnpm"}`+"\n"), 0o600)
	t.Setenv("TACIT_RULEBOOK_LOG", rb)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	if code := runCurate([]string{"--quick"}); code != 0 {
		t.Fatalf("curate exit %d", code)
	}
	if n := len(prompts()); n != 3 {
		t.Fatalf("%d model calls, want 3 reviews", n)
	}
	if l, ok := ledger.ParseReviews(readBytes(t, filepath.Join(r.local, filepath.FromSlash(overlay.ReviewsRel)))).ForOrigin("note-rb"); !ok || l.By != ledger.ByRulebook || l.Verdict != "correct" {
		t.Fatalf("rulebook's verdict: %+v", l)
	}
	reviews := ledger.ParseReviews(readBytes(t, filepath.Join(r.local, filepath.FromSlash(overlay.ReviewsRel))))
	host := store.ShortHost()
	for n, v := range map[string]string{"note-correct": "correct", "note-unsure": "uncertain", "note-wrong": "wrong"} {
		if l, ok := reviews.ForNote(overlay.Prefix + "claude@" + host + "/" + n + ".md"); !ok || l.Verdict != v {
			t.Fatalf("%s: %+v", n, l)
		}
	}
	r.untouched()

	// The hooks' query holds the wrong note back and labels the checked one;
	// search still finds the wrong one, marked.
	ix := exactIndex(t, r.s)
	hooked, err := ix.Query(ctx, search.Situation{Query: "walrus driver port deploys fridays", RestrictRepos: true}, 10, false)
	if err != nil {
		t.Fatal(err)
	}
	searched, _ := ix.Query(ctx, search.Situation{Query: "walrus driver port deploys fridays"}, 10, false)
	ix.Close()
	hb, sb := search.Block(hooked, "# t"), search.Block(searched, "# t")
	if !strings.Contains(hb, "note-correct (this machine): agent note (") || !strings.Contains(hb, "checked against its session") || strings.Contains(hb, "note-wrong") {
		t.Fatalf("the hooks' lines:\n%s", hb)
	}
	if !strings.Contains(sb, "note-wrong (this machine): agent note (") || !strings.Contains(sb, "the reviewer found it wrong") {
		t.Fatalf("search's lines:\n%s", sb)
	}
	t2 := claudeSession(t, r.home, "sess-m2", work)
	got := promptHook(t, "sess-m2", t2, walrus, "which port does the walrus driver listen on and when do walrus deploys happen")
	if strings.Contains(got, "- note-wrong") {
		t.Fatalf("a note judged wrong was delivered:\n%s", got)
	}
	if !strings.Contains(got, "walrus deploys always need the vpn") || !strings.Contains(got, "tacit q-") {
		t.Fatalf("no question in the folder's session:\n%s", got)
	}
	t3 := claudeSession(t, r.home, "sess-m3", "/home/u/elsewhere")
	if other := promptHook(t, "sess-m3", t3, "/home/u/elsewhere", "summarize the meeting notes please"); strings.Contains(other, "tacit q-") {
		t.Fatalf("an unrelated session was asked:\n%s", other)
	}
	// A headless session (claude -p) is never asked.
	hp := filepath.Join(r.home, ".claude", "projects", "-work", "sess-h.jsonl")
	os.WriteFile(hp, []byte(`{"type":"user","entrypoint":"sdk-cli","cwd":"`+work+`","sessionId":"sess-h","message":{"content":"walrus deploys"}}`+"\n"), 0o600)
	if h := promptHook(t, "sess-h", hp, work, "when do walrus deploys happen and which port"); strings.Contains(h, "tacit q-") {
		t.Fatalf("a headless session was asked:\n%s", h)
	}

	tag := readQueue(t, r.home).Open()[0].Text()
	appendLine(t, t2, agentTurn("sess-m2", "Port 7790.\n\n"+tag), ownerTurn("sess-m2", "correct it: walrus deploys need a rollback plan, not the vpn"))
	if code := runReviewer(nil); code != 0 {
		t.Fatalf("reviewer exit %d", code)
	}
	reviews = ledger.ParseReviews(readBytes(t, filepath.Join(r.local, filepath.FromSlash(overlay.ReviewsRel))))
	l, _ := reviews.ForNote(overlay.Prefix + "claude@" + host + "/note-unsure.md")
	if l.Verdict != ledger.VerdictCorrected || l.Words != "walrus deploys need a rollback plan, not the vpn" || l.Statement != "owner-corrects-note-unsure" {
		t.Fatalf("ledger: %+v", l)
	}
	st := filepath.Join(r.local, "curated", "projects", "walrus", "owner-corrects-note-unsure.md")
	sf, err := fact.Parse("local/curated/projects/walrus/owner-corrects-note-unsure.md", readBytes(t, st))
	if err != nil || sf.Meta.Type != fact.StatementType || sf.Meta.Source.OwnerWords != "walrus deploys need a rollback plan, not the vpn" || sf.Meta.Source.OwnerWordsFrom != "claude:sess-m2" {
		t.Fatalf("statement: %+v %v", sf, err)
	}
	if !strings.Contains(string(readBytes(t, filepath.Join(r.local, filepath.FromSlash(overlay.QuotesRel)))), "owner-corrects-note-unsure") {
		t.Fatal("no words line in the local quote ledger")
	}
	r.untouched()
	ix = exactIndex(t, r.s)
	hits, _ := ix.Query(ctx, search.Situation{Repo: "walrus", Query: "walrus deploys rollback vpn", RestrictRepos: true}, 10, false)
	ix.Close()
	var sawStatement bool
	for _, h := range hits {
		if h.Name == "note-unsure" {
			t.Fatalf("the corrected note reached the hooks: %+v", h)
		}
		if h.Name == "owner-corrects-note-unsure" {
			sawStatement = true
			if b := search.Block([]search.Hit{h}, "# t"); !strings.Contains(b, "you said once (") {
				t.Fatalf("statement line:\n%s", b)
			}
		}
	}
	if !sawStatement {
		t.Fatalf("the statement is not found: %+v", hits)
	}
}
