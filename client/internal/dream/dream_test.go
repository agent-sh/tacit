package dream

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/ownerwords"
)

func TestWindowsCursorAndFreshStart(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir) // the checkout is under home: outside it, it would need work from two sessions
	f := filepath.Join(dir, "s1.jsonl")
	line := func(s string) string { return s + "\n" }
	// The session works in a real checkout: its repo is the git root's name.
	api := filepath.Join(dir, "projects", "api")
	if err := os.MkdirAll(filepath.Join(api, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	initial := line(`{"type":"user","entrypoint":"cli","cwd":"` + filepath.ToSlash(api) + `","message":{"content":"old turn that must never be replayed"}}`)
	os.WriteFile(f, []byte(initial), 0o644)
	src := []Source{{Glob: filepath.Join(dir, "*.jsonl"), Format: digest.Claude}}

	st := &State{Offsets: map[string]int64{}}
	ws, _ := Windows(src, st, true)
	if len(ws) != 0 || st.Offsets[f] != int64(len(initial)) {
		t.Fatalf("fresh start must only record cursors: %v %v", ws, st.Offsets)
	}

	// Append a delta with an owner line, an error and assistant text; plus a
	// trailing partial line that must not be consumed.
	delta := line(`{"type":"user","entrypoint":"cli","cwd":"`+filepath.ToSlash(api)+`","message":{"content":"commit it"}}`) +
		line(`{"type":"assistant","message":{"content":[{"type":"text","text":"Committing."},{"type":"tool_use","name":"Bash","input":{"command":"git commit"}}]}}`) +
		line(`{"type":"user","message":{"content":[{"type":"tool_result","is_error":true,"content":"tools/hooks/pre-push: eslint: not found"}]}}`)
	delta += strings.Repeat(" ", minNewBytes) + "\n" // padding so the delta clears the minimum
	partial := `{"type":"user","message":{"content":"unfinished`
	os.WriteFile(f, []byte(initial+delta+partial), 0o644)
	ws, _ = Windows(src, st, false)
	if len(ws) != 1 {
		t.Fatalf("expected one window, got %d", len(ws))
	}
	w := ws[0]
	if w.Repo != "api" {
		t.Fatalf("repo %q", w.Repo)
	}
	// The checkout is in Go's temp dir. Where only agents start sessions
	// (digest.AgentCwd: GOTMPDIR or TMPDIR under ~/.cache) the session is an
	// agent's and its prompt a P: line; the cursors are the same either way.
	prompt := "U: commit it"
	if digest.AgentCwd(api) {
		prompt = "P: commit it"
	}
	joined := strings.Join(w.Lines, "\n")
	if !strings.Contains(joined, prompt) || !strings.Contains(joined, "E: tools/hooks/pre-push: eslint: not found") || strings.Contains(joined, "old turn") || strings.Contains(joined, "unfinished") {
		t.Fatalf("window lines:\n%s", joined)
	}
	if w.End != int64(len(initial)+len(delta)) {
		t.Fatalf("window end should stop before the partial line: %d", w.End)
	}
	if st.Offsets[f] != int64(len(initial)) {
		t.Fatalf("Windows must not advance the cursor for a window with content: %d", st.Offsets[f])
	}
	// The caller advances the cursor after a successful call; then nothing new.
	st.Offsets[f] = w.End
	ws, _ = Windows(src, st, false)
	if len(ws) != 0 {
		t.Fatal("no new bytes should mean no window")
	}
	// A truncated file restarts from its end.
	os.WriteFile(f, []byte("{}\n"), 0o644)
	Windows(src, st, false)
	if st.Offsets[f] != 3 {
		t.Fatalf("shrunk file cursor %d", st.Offsets[f])
	}
}

func TestParseAndToFact(t *testing.T) {
	xs := Parse("```json\n[{\"description\":\"api's tools/hooks/pre-push fails with eslint: not found; prefix PATH=$HOME/.local/node/bin\",\"type\":\"machine_fact\",\"kind\":\"observed\",\"applies_to\":{\"commands\":[\"git push\"],\"repos\":[\"api\"]}},{\"description\":\"\"}]\n```")
	if len(xs) != 1 {
		t.Fatalf("parsed %d", len(xs))
	}
	f := ToFact(xs[0], "api", "host-a", "sess-1")
	if f.Meta.Scope != "machines/host-a" || f.Meta.Type != "machine_fact" || f.Meta.Source.Harness != "dreamer" || f.Meta.Source.Session != "sess-1" || !fact.IsSlug(f.Meta.Name) {
		t.Fatalf("%+v", f.Meta)
	}
	if _, err := fact.Parse("inbox/dreamer@host-a/"+f.Meta.Name+".md", must(f.Encode())); err != nil {
		t.Fatal(err)
	}
	g := ToFact(Extracted{Description: "The owner wants releases batched", Type: "user_standard", Kind: "user"}, "webapp", "host-a", "")
	if g.Meta.Scope != "projects/webapp" || g.Meta.Source.Kind != "user" {
		t.Fatalf("%+v", g.Meta)
	}
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}

// The dreamer's repo of a cwd is repoid's: a real git root, a worktree for its
// main repository; a folder's name never names one (the rule before
// 2026-10-21 read the first folder under ~/projects and the base name of a
// cwd outside home, which made projects/<scratch dir> scopes).
func TestRepoOfCwd(t *testing.T) {
	h, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	git := func(d string) {
		if err := os.MkdirAll(filepath.Join(h, d, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{"workplace/widget", "projects/tacit", "projects/agent-sh/agnix", "projects/atlas"} {
		git(d)
	}
	for _, d := range []string{"projects/park-predictor", "projects/tacit/client", ".cache/run42/head", "workplace/notes", "srv/memra"} {
		if err := os.MkdirAll(filepath.Join(h, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for cwd, want := range map[string]string{
		"projects/tacit":                     "tacit",
		"projects/tacit/client":              "tacit",
		"projects/tacit/.claude/worktrees/x": "tacit", // gone since: its repo still holds the path
		"projects/agent-sh/agnix":            "agnix",
		"projects/agent-sh/agnix/crates/x":   "agnix",
		"projects/agent-sh":                  "", // a folder of checkouts
		"projects/park-predictor":            "", // under ~/projects but no git root
		"":                                   "",
		"projects":                           "",
		".cache/run42/head":                  "",
		"srv/memra":                          "", // a folder named like a repo
		"workplace":                          "",
		"workplace/widget/pkg":               "widget",
		"workplace/notes":                    "",
	} {
		if got := RepoOfCwd(filepath.Join(h, cwd), h); got != want {
			t.Errorf("%s: %q, want %q", cwd, got, want)
		}
	}
	if got := RepoOfCwd("/", h); got != "" {
		t.Errorf("root: %q", got)
	}
}

func TestRepoOfCwdNamesAWorktreeAfterItsRepository(t *testing.T) {
	home := t.TempDir()
	widget := filepath.Join(home, "w", "widget")
	fix := filepath.Join(home, "w", "widget-fix")
	admin := filepath.Join(widget, ".git", "worktrees", "widget-fix")
	for _, d := range []string{admin, filepath.Join(fix, "pkg")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, body := range map[string]string{
		filepath.Join(fix, ".git"):        "gitdir: " + admin + "\n",
		filepath.Join(admin, "HEAD"):      "ref: refs/heads/fix\n",
		filepath.Join(admin, "commondir"): "../..\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := RepoOfCwd(filepath.Join(fix, "pkg"), home); got != "widget" {
		t.Errorf("worktree cwd: %q, want widget", got)
	}
}

func TestParseCheckedDistinguishesEmptyFromFailure(t *testing.T) {
	for _, body := range []string{"[]", "```json\n[]\n```"} {
		if rows, err := ParseChecked(body); err != nil || len(rows) != 0 {
			t.Fatalf("empty result %q: %v %v", body, rows, err)
		}
	}
	for _, body := range []string{"model unavailable", "{}", "[{\"description\":", "[1]", "[null]", "[{}]"} {
		if _, err := ParseChecked(body); err == nil {
			t.Fatalf("accepted %q", body)
		}
	}
}

func TestWindowsPassOverALineLongerThanTheReadCap(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "s1.jsonl")
	src := []Source{{Glob: filepath.Join(dir, "*.jsonl"), Format: digest.Claude}}
	// One record longer than maxReadBytes, then an ordinary turn after it.
	long := `{"type":"user","message":{"content":"` + strings.Repeat("x", maxReadBytes+1000) + `"}}` + "\n"
	after := `{"type":"user","entrypoint":"cli","cwd":"/home/u/projects/api","message":{"content":"commit it"}}` + "\n" +
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Committing."},{"type":"tool_use","name":"Bash","input":{"command":"git commit"}}]}}` + "\n" +
		`{"type":"user","message":{"content":[{"type":"tool_result","is_error":true,"content":"tools/hooks/pre-push: eslint: not found"}]}}` + "\n" +
		strings.Repeat(" ", minNewBytes) + "\n"
	os.WriteFile(f, []byte(long+after), 0o644)
	st := &State{Offsets: map[string]int64{f: 0}}
	ws, _ := Windows(src, st, false)
	if len(ws) != 0 || st.Offsets[f] != int64(len(long)) || st.Skipped != 1 {
		t.Fatalf("the long line should be passed over: %d windows, cursor %d (want %d), skipped %d", len(ws), st.Offsets[f], len(long), st.Skipped)
	}
	ws, _ = Windows(src, st, false)
	if len(ws) != 1 || !strings.Contains(strings.Join(ws[0].Lines, "\n"), "U: commit it") {
		t.Fatalf("the turn after the long line was not read: %v", ws)
	}
	// A long line still being written (no newline yet) keeps the cursor.
	g := filepath.Join(dir, "s2.jsonl")
	os.WriteFile(g, []byte(`{"type":"user","message":{"content":"`+strings.Repeat("y", maxReadBytes+10)), 0o644)
	st.Offsets[g] = 0
	Windows(src, st, false)
	if st.Offsets[g] != 0 {
		t.Fatalf("an unfinished long line moved the cursor to %d", st.Offsets[g])
	}
}

func TestWindowsPassOverALineEndingRightAtTheReadCap(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "s1.jsonl")
	src := []Source{{Glob: filepath.Join(dir, "*.jsonl"), Format: digest.Claude}}
	// maxReadBytes bytes with no newline, the newline right after them.
	long := strings.Repeat("z", maxReadBytes) + "\n"
	os.WriteFile(f, []byte(long+strings.Repeat(" ", minNewBytes)+"\n"), 0o644)
	st := &State{Offsets: map[string]int64{f: 0}}
	Windows(src, st, false)
	if st.Offsets[f] != int64(len(long)) || st.Skipped != 1 {
		t.Fatalf("cursor %d (want %d), skipped %d", st.Offsets[f], len(long), st.Skipped)
	}
}

func TestWindowsFromHomeTakeTheRepoTheSessionWorksIn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repo := filepath.Join(home, "workplace", "widget")
	os.MkdirAll(filepath.Join(repo, ".git"), 0o755)
	dir := filepath.Join(home, "transcripts")
	os.MkdirAll(dir, 0o755)
	src := []Source{{Glob: filepath.Join(dir, "*.jsonl"), Format: digest.Claude}}
	write := func(name, cmd string) string {
		f := filepath.Join(dir, name)
		body := `{"type":"user","cwd":"` + home + `","message":{"content":"run the tests"}}` + "\n" +
			`{"type":"assistant","message":{"content":[{"type":"text","text":"Running."},{"type":"tool_use","name":"Bash","input":{"command":"` + cmd + `"}}]}}` + "\n" +
			`{"type":"user","message":{"content":[{"type":"tool_result","is_error":true,"content":"tools/hooks/pre-push: eslint: not found"}]}}` + "\n" +
			`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"` + cmd + ` -v"}}]}}` + "\n" +
			strings.Repeat(" ", minNewBytes) + "\n"
		os.WriteFile(f, []byte(body), 0o644)
		return f
	}
	in := write("a.jsonl", "cd "+repo+" && npm test")
	none := write("b.jsonl", "ls "+filepath.Join(home, "Downloads", "x"))
	st := &State{Offsets: map[string]int64{in: 0, none: 0}}
	ws, _ := Windows(src, st, false)
	got := map[string]string{}
	for _, w := range ws {
		got[w.File] = w.Repo
	}
	if got[in] != "widget" || got[none] != "" {
		t.Fatalf("window repos %v, want widget for the session in the repo and none for the other", got)
	}
}

func TestCwdPathReadsAFileURL(t *testing.T) {
	for in, want := range map[string]string{
		"file:///home/u":              "/home/u",
		"file:///home/u/my%20project": "/home/u/my project",
		"/home/u/w":                   "/home/u/w",
	} {
		if got := cwdPath(in); got != want {
			t.Errorf("cwdPath(%q) = %q, want %q", in, got, want)
		}
	}
	if got := RepoOfCwd("file:///h", "/h"); got != "" {
		t.Errorf("a file URL of home named %q", got)
	}
}

func TestToFactKeepsTheOwnersWordsForARuling(t *testing.T) {
	rows, err := ParseChecked(`[{"description":"Release notes are written in the past tense.","type":"user_standard","kind":"user","quote":"release notes say what shipped, past tense, never what will ship","applies_to":{}},
	{"description":"npm ci needs node 24 on this host.","type":"machine_fact","kind":"observed","quote":"ignored for an observation","applies_to":{}},
	{"description":"Two turns make one rule.","type":"user_standard","kind":"user","quote":["keep the changelog short","one line per change"],"applies_to":{}},
	{"description":"A quote of the wrong shape is no quote.","type":"user_standard","kind":"user","quote":{"text":"x"},"applies_to":{}}]`)
	if err != nil || len(rows) != 4 {
		t.Fatalf("parse: %v %d", err, len(rows))
	}
	ruling := ToFact(rows[0], "", "box", "s1")
	if !strings.Contains(ruling.Body, `Quoted from the session: "release notes say what shipped, past tense, never what will ship"`) {
		t.Fatalf("ruling body lost the owner's words: %q", ruling.Body)
	}
	if q := ownerwords.Quotes(ruling.Body); len(q) == 0 || !strings.Contains(q[0], "release notes say") {
		t.Fatalf("curation cannot find the quote: %q", q)
	}
	if obs := ToFact(rows[1], "", "box", "s1"); strings.Contains(obs.Body, "Quoted") {
		t.Fatalf("an observation got an owner quote: %q", obs.Body)
	}
	if b := ToFact(rows[2], "", "box", "s1").Body; !strings.Contains(b, `"keep the changelog short ... one line per change"`) {
		t.Fatalf("a list quote: %q", b)
	}
	if b := ToFact(rows[3], "", "box", "s1").Body; strings.Contains(b, "Quoted") {
		t.Fatalf("an object quote: %q", b)
	}
}

func TestBodyQuote(t *testing.T) {
	long := strings.Repeat("never merge on a red build ", 40)
	for _, c := range []struct {
		name, kind, quote, want string
	}{
		{"plain", "user", "always run the linter first", `x. Quoted from the session: "always run the linter first"` + "\n"},
		{"lines joined", "user", "always run\n  the linter\tfirst", `x. Quoted from the session: "always run the linter first"` + "\n"},
		{"inner double quotes", "user", `the"main"branch stays protected forever`, `x. Quoted from the session: "the”main”branch stays protected forever"` + "\n"},
		{"empty", "user", "  \n ", "x.\n"},
		{"observed", "observed", "always run the linter first", "x.\n"},
	} {
		if got := body(Extracted{Description: "x.", Quote: Quote(c.quote)}, c.kind); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	// The inner quotes stay the owner's words for the match.
	b := body(Extracted{Description: "x.", Quote: `the"main"branch stays protected forever`}, "user")
	if q := ownerwords.Quotes(b); len(q) != 1 || !ownerwords.In(`well: the"main"branch stays protected forever, ok`, q[0]) {
		t.Errorf("inner quotes broke the match: %q", q)
	}
	b = body(Extracted{Description: "x.", Quote: Quote(long)}, "user")
	q := b[strings.Index(b, `"`)+1 : strings.LastIndex(b, `"`)]
	if len(q) > maxQuote+4 || !strings.HasSuffix(q, " ...") || !strings.HasPrefix(q, "never merge") {
		t.Errorf("long quote: %d bytes, %q", len(q), q[len(q)-20:])
	}
}

// A session in a separate full clone of a checkout in the projects
// directory (same origin URL) is filed under the main checkout's name.
func TestWindowsNameASeparateCloneForItsMainCheckout(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	t.Setenv("HOME", home)
	t.Setenv("TACIT_PROJECTS", filepath.Join(home, "projects"))
	mk := func(dir, origin string) {
		os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
		os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644)
		os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("[remote \"origin\"]\n\turl = "+origin+"\n"), 0o644)
	}
	mk(filepath.Join(home, "projects", "pikadon"), "git@github.com:acme/ginza.git")
	clone := filepath.Join(home, "tasks", "t7", "ginza")
	mk(clone, "https://github.com/acme/ginza.git")
	dir := filepath.Join(home, "transcripts")
	os.MkdirAll(dir, 0o755)
	f := filepath.Join(dir, "a.jsonl")
	os.WriteFile(f, []byte(`{"type":"user","cwd":"`+filepath.ToSlash(clone)+`","message":{"content":"fix the build"}}`+"\n"+
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Fixed."}]}}`+"\n"+strings.Repeat(" ", minNewBytes)+"\n"), 0o644)
	st := &State{Offsets: map[string]int64{f: 0}}
	ws, _ := Windows([]Source{{Glob: filepath.Join(dir, "*.jsonl"), Format: digest.Claude}}, st, false)
	if len(ws) != 1 || ws[0].Repo != "pikadon" || ws[0].RepoRoot != filepath.Join(home, "projects", "pikadon") {
		t.Fatalf("windows %+v", ws)
	}
}

// A ruling the model took from another agent's prompt is that agent's note:
// code finds the words in a P: line and in no U: line, or the window has no
// U: line at all. Words in a U: line, or in no line (a typo the model fixed),
// stay for the curator to check.
func TestCheckOwnerWordsKeepsAgentsPromptsOut(t *testing.T) {
	lines := []string{
		"P: Owner ruling: only release from the main branch, never from a fork",
		"A: Releasing from main.",
		"U: keep the changelog to one line per change",
	}
	x := Extracted{Description: "Release only from main.", Type: "user_standard", Kind: "user", Quote: "only release from the main branch, never from a fork"}
	got := CheckOwnerWords(x, lines)
	if got.Kind != "observed" || !got.AgentWords {
		t.Fatalf("a quote from a P: line: %+v", got)
	}
	f := ToFact(got, "", "box", "s1")
	if f.Meta.Source.Kind != "observed" || strings.Contains(f.Body, "Quoted from the session") || !strings.Contains(f.Body, `Another agent's prompt in the session said: "only release from the main branch, never from a fork"`) {
		t.Fatalf("fact %+v body %q", f.Meta.Source, f.Body)
	}
	for _, q := range []string{"keep the changelog to one line per change", "keep the chnagelog to one line per change"} {
		if got := CheckOwnerWords(Extracted{Kind: "user", Quote: Quote(q)}, lines); got.Kind != "user" {
			t.Errorf("%q: %+v", q, got)
		}
	}
	st := CheckOwnerWords(Extracted{Kind: "statement", Quote: "keep the changelog to one line per change"}, lines[:2])
	if st.Kind != "observed" {
		t.Errorf("a statement from a window with no owner line: %+v", st)
	}
	if obs := CheckOwnerWords(Extracted{Kind: "observed"}, lines[:2]); obs.Kind != "observed" || obs.AgentWords {
		t.Errorf("an observation: %+v", obs)
	}
}

// A Codex subagent's session (its parent's front end, codex-tui, in
// session_meta) is read as an agent's: its prompt is a P: line.
func TestWindowsReadACodexSubagentAsAnAgent(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "rollout-sub.jsonl")
	meta := `{"type":"session_meta","payload":{"id":"cx-sub","parent_thread_id":"cx-own","cwd":"/home/u/projects/shop","originator":"codex-tui","source":{"subagent":{"thread_spawn":{"parent_thread_id":"cx-own","depth":1}}},"thread_source":"subagent"}}`
	body := meta + "\n" +
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"the owner wants every endpoint versioned"}]}}` + "\n" +
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Versioned. ` + strings.Repeat("y", minNewBytes) + `"}]}}` + "\n"
	os.WriteFile(f, []byte(body), 0o644)
	st := &State{Offsets: map[string]int64{f: 0}}
	ws, _ := Windows([]Source{{Glob: filepath.Join(dir, "*.jsonl"), Format: digest.Codex}}, st, false)
	if len(ws) != 1 || ws[0].Lines[0] != "P: the owner wants every endpoint versioned" {
		t.Fatalf("windows %d %v", len(ws), ws)
	}
}

// What the owner typed while the agent worked (Claude Code's queued_command
// attachment) is a U: line of the window, in order, and backs their quote.
func TestWindowsReadTheOwnersMidTurnMessages(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "own.jsonl")
	body := strings.Join([]string{
		`{"type":"user","entrypoint":"cli","cwd":"/home/u/projects/shop","sessionId":"own","origin":{"kind":"human"},"promptSource":"typed","turnOrigin":"human","message":{"content":"fix the flaky checkout test"}}`,
		`{"type":"assistant","entrypoint":"cli","sessionId":"own","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"go test ./..."}}]}}`,
		`{"type":"user","entrypoint":"cli","sessionId":"own","message":{"content":[{"type":"tool_result","content":"ok"}]}}`,
		`{"parentUuid":"9c1d","isSidechain":false,"attachment":{"type":"queued_command","prompt":"never skip the race detector in this repo","source_uuid":"a1","commandMode":"prompt","origin":{"kind":"human"},"humanTurn":true},"type":"attachment","uuid":"5b2e","entrypoint":"cli","cwd":"/home/u/projects/shop","sessionId":"own","version":"2.1.302"}`,
		`{"type":"assistant","entrypoint":"cli","sessionId":"own","message":{"content":[{"type":"text","text":"Running with -race. ` + strings.Repeat("y", minNewBytes) + `"}]}}`,
	}, "\n") + "\n"
	os.WriteFile(f, []byte(body), 0o644)
	st := &State{Offsets: map[string]int64{f: 0}}
	ws, _ := Windows([]Source{{Glob: filepath.Join(dir, "*.jsonl"), Format: digest.Claude}}, st, false)
	if len(ws) != 1 || len(ws[0].Lines) < 3 || ws[0].Lines[2] != "U: never skip the race detector in this repo" {
		t.Fatalf("windows %d %v", len(ws), ws)
	}
	for _, kind := range []string{"user", "statement"} {
		if got := CheckOwnerWords(Extracted{Kind: kind, Quote: Quote("never skip the race detector")}, ws[0].Lines); got.Kind != kind {
			t.Errorf("%s from a mid-turn message: %+v", kind, got)
		}
	}
}
