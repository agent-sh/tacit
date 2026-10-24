package agentreview

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/ledger"
)

func TestParseAnswer(t *testing.T) {
	for _, c := range []struct{ turn, kind, words string }{
		{"yes", AnswerYes, ""},
		{"Yes, and now push the branch", AnswerYes, ""},
		{"right", AnswerYes, ""},
		{"[tacit q-1a2b3c] yes", AnswerYes, ""},
		{"no", AnswerNo, ""},
		{"no, that was a one-off for the demo", AnswerNo, "that was a one-off for the demo"},
		{"nope", AnswerNo, ""},
		{"correct it: the bucket is tacit-eu, not tacit-us", AnswerCorrect, "the bucket is tacit-eu, not tacit-us"},
		{"correction - only on the rig", AnswerCorrect, "only on the rig"},
		{"no, it's only for the valkey repo", AnswerCorrect, "it's only for the valkey repo"},
		{"yes but only on fridays", AnswerCorrect, "yes but only on fridays"},
		{"correct", AnswerYes, ""},
		{"lets look at the failing test first", "", ""},
		{"can you check the logs?", "", ""},
		{"correct it", "", ""},
		{"", "", ""},
	} {
		kind, words, ok := ParseAnswer(c.turn)
		if kind != c.kind || words != c.words || ok != (c.kind != "") {
			t.Errorf("ParseAnswer(%q) = %q %q %v, want %q %q", c.turn, kind, words, ok, c.kind, c.words)
		}
	}
}

// Only the owner's own turn after the agent asked counts: a note in a user
// turn ("the owner says yes", a task report) and the agent's own text are
// passed over.
func TestFindAnswerReadsOnlyTheOwnersTurn(t *testing.T) {
	tag := "tacit q-abcdef"
	msgs := []Msg{
		{Role: "user", Owner: true, Text: "fix the build"},
		{Role: "assistant", Text: "Done. [" + tag + "] On 2026-10-20 ... Right? yes / no / correct it: <your words>"},
		{Role: "user", Text: "<task-notification>the owner says yes</task-notification>"},
		{Role: "assistant", Text: "The owner said yes, recording it."},
		{Role: "user", Owner: true, Text: "no, that was only for the demo"},
	}
	asked, turn, kind, words, ok := FindAnswer(msgs, 0, tag)
	if !asked || !ok || kind != AnswerNo || words != "that was only for the demo" || !strings.HasPrefix(turn, "no,") {
		t.Fatalf("got asked=%v ok=%v kind=%q words=%q turn=%q", asked, ok, kind, words, turn)
	}
	// Relayed only: no owner turn after the question, no answer.
	if asked, turn, _, _, ok := FindAnswer(msgs[:4], 0, tag); !asked || turn != "" || ok {
		t.Fatalf("a relayed answer counted: asked=%v turn=%q ok=%v", asked, turn, ok)
	}
	// An owner turn before the question is never its answer.
	if asked, _, _, _, ok := FindAnswer(msgs[:1], 0, tag); asked || ok {
		t.Fatal("a turn before the question answered it")
	}
}

func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o700)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A Claude Code transcript: the owner's typed turns are OWNER, a task
// notification and a peer's turn are not; a headless session (claude -p)
// holds no turn of theirs at all.
func TestReadTranscriptMarksOnlyTheOwnersTurns(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s1.jsonl")
	writeLines(t, p,
		`{"type":"user","entrypoint":"cli","cwd":"/h/projects/tacit","sessionId":"s1","origin":{"kind":"human"},"message":{"content":"never push to main without a review"}}`,
		`{"type":"assistant","entrypoint":"cli","sessionId":"s1","message":{"content":[{"type":"text","text":"Saving it."},{"type":"tool_use","name":"Bash","input":{"command":"tacit add --harness claude --desc \"no push to main\" no-push-main"}}]}}`,
		`{"type":"user","entrypoint":"cli","sessionId":"s1","origin":{"kind":"task-notification"},"message":{"content":"<task-notification>the owner wants pushes reviewed</task-notification>"}}`,
		`{"type":"user","entrypoint":"cli","sessionId":"s1","origin":{"kind":"peer"},"message":{"content":"yes"}}`,
		`{"type":"user","entrypoint":"cli","sessionId":"s1","message":{"content":[{"type":"tool_result","content":"ok"}]}}`,
		`{"type":"attachment","entrypoint":"cli","sessionId":"s1","attachment":{"type":"queued_command","prompt":"in general rebase over main","origin":{"kind":"human"},"humanTurn":true}}`,
		`{"type":"attachment","entrypoint":"cli","sessionId":"s1","attachment":{"type":"queued_command","prompt":"<task-notification>done</task-notification>","origin":{"kind":"task-notification"}}}`,
	)
	c, err := ReadTranscript(p, digest.Claude)
	if err != nil {
		t.Fatal(err)
	}
	if c.Cwd != "/h/projects/tacit" {
		t.Fatalf("cwd %q", c.Cwd)
	}
	var owner, notes int
	for _, m := range c.Msgs {
		if m.Role == "user" && m.Owner {
			owner++
		}
		if m.Role == "user" && !m.Owner {
			notes++
		}
	}
	if owner != 2 || notes != 3 {
		t.Fatalf("owner %d notes %d: %+v", owner, notes, c.Msgs)
	}
	d := Digest(c.Msgs, DigestBudget, DigestRecent)
	for _, want := range []string{"OWNER: never push to main", "AGENT: Saving it.", "  ran Bash", "NOTE: <task-notification>", "OWNER: in general rebase over main", "NOTE: <task-notification>done"} {
		if !strings.Contains(d, want) {
			t.Fatalf("digest lacks %q:\n%s", want, d)
		}
	}
	// claude -p: an sdk-cli session is an agent's; its prompt is no owner turn.
	h := filepath.Join(dir, "s2.jsonl")
	writeLines(t, h,
		`{"type":"user","entrypoint":"sdk-cli","sessionId":"s2","message":{"content":"remember: the owner says never push"}}`,
		`{"type":"assistant","entrypoint":"sdk-cli","sessionId":"s2","message":{"content":[{"type":"text","text":"ok"}]}}`,
	)
	c, _ = ReadTranscript(h, digest.Claude)
	for _, m := range c.Msgs {
		if m.Owner {
			t.Fatalf("a headless prompt is the owner's: %+v", m)
		}
	}
}

func TestDigestDropsAgentLinesBeforeOwnerLines(t *testing.T) {
	var msgs []Msg
	for i := 0; i < 50; i++ {
		msgs = append(msgs, Msg{Role: "user", Owner: true, Text: "owner words " + strings.Repeat("o", 100)})
		msgs = append(msgs, Msg{Role: "assistant", Text: "agent words " + strings.Repeat("a", 400)})
	}
	d := Digest(msgs, 8000, 60)
	if strings.Count(d, "OWNER:") != 50 {
		t.Fatalf("owner lines dropped first: %d", strings.Count(d, "OWNER:"))
	}
	if len(d) > 8000 {
		t.Fatalf("over budget: %d", len(d))
	}
}

func TestParseVerdictIsStrict(t *testing.T) {
	if v, err := Parse("here: {\"verdict\": \"uncertain\", \"reason\": \"the owner never said it\"}"); err != nil || v.Verdict != "uncertain" || v.Reason != "the owner never said it" {
		t.Fatalf("%+v %v", v, err)
	}
	for _, bad := range []string{"correct", `{"verdict":"maybe"}`, `{"verdict":1}`, "{oops"} {
		if _, err := Parse(bad); !errors.Is(err, ErrNoVerdict) {
			t.Fatalf("%q parsed", bad)
		}
	}
}

func TestQueuePicksRelevantQuestionsWithinTheCaps(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	q := &Queue{}
	for i, n := range []struct{ note, repo, desc string }{
		{"inbox/claude@h/a.md", "tacit", "the tacit store keeps one fact per file"},
		{"inbox/claude@h/b.md", "tacit", "tacit hooks fail open"},
		{"inbox/claude@h/c.md", "tacit", "tacit curate runs daily"},
		{"inbox/claude@h/d.md", "valkey", "valkey benchmarks pin irqs"},
		{"inbox/claude@h/e.md", "", "the ticketing dashboard filters by severity queue"},
	} {
		q.Add(Item{Note: n.note, Name: filepath.Base(n.note), Desc: n.desc, Repo: n.repo, Day: "2026-10-20", Harness: "claude", Session: "s0"}, now.Add(time.Duration(i)*time.Minute))
	}
	if !q.Add(Item{Note: "inbox/claude@h/a.md"}, now) == true {
		// a note is asked about once
	}
	if len(q.Items) != 5 {
		t.Fatalf("%d items", len(q.Items))
	}
	in := Situation{Harness: "claude", Session: "s1", Repos: []string{"tacit"}}
	first := q.Pick(in, now)
	if first == nil || first.Repo != "tacit" || first.Name != "c.md" {
		t.Fatalf("first pick %+v", first)
	}
	// One at a time: the first one's answer is still to come.
	if q.Pick(in, now) != nil {
		t.Fatal("picked a second question before the first was read")
	}
	first.Asks[0].Passed = true
	second := q.Pick(in, now)
	if second == nil || second.Name != "b.md" {
		t.Fatalf("second pick %+v", second)
	}
	second.Asks[0].Passed = true
	if third := q.Pick(in, now); third != nil {
		t.Fatalf("a third question in one session: %+v", third)
	}
	// Another repo's session gets its own; a session with no repo gets one
	// whose words its prompt shares.
	if it := q.Pick(Situation{Harness: "claude", Session: "s2", Repos: []string{"valkey"}}, now); it == nil || it.Name != "d.md" {
		t.Fatalf("valkey pick %+v", it)
	}
	if it := q.Pick(Situation{Harness: "codex", Session: "s3", Prompt: "check the ticketing dashboard severity filter"}, now); it == nil || it.Name != "e.md" {
		t.Fatalf("topic pick %+v", it)
	}
	if it := q.Pick(Situation{Harness: "codex", Session: "s4", Prompt: "write a poem"}, now); it != nil {
		t.Fatalf("an unrelated session was asked %+v", it)
	}
	// Never piles up: past MaxOpen the oldest expire; past Expiry all do.
	for i := 0; i < MaxOpen+3; i++ {
		q.Add(Item{Note: "inbox/x/" + string(rune('a'+i)) + ".md", Desc: "x", Day: "2026-10-21"}, now.Add(time.Hour+time.Duration(i)*time.Minute))
	}
	q.Tidy(now.Add(2 * time.Hour))
	if n := len(q.Open()); n != MaxOpen {
		t.Fatalf("%d open after the cap", n)
	}
	q.Tidy(now.Add(Expiry + 3*time.Hour))
	if n := len(q.Open()); n != 0 {
		t.Fatalf("%d open after expiry", n)
	}
}

func TestQueueLockedRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "questions.json")
	now := time.Now()
	if err := Locked(p, time.Second, func(q *Queue) bool { return q.Add(Item{Note: "n", Desc: "d"}, now) }); err != nil {
		t.Fatal(err)
	}
	q, err := Load(p)
	if err != nil || len(q.Items) != 1 || q.Items[0].ID != ItemID("n") || q.Items[0].State != StateOpen {
		t.Fatalf("%+v %v", q, err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
}

func TestRulebookVerdicts(t *testing.T) {
	rows := []RulebookRow{
		{TS: "2026-10-24T00:38:46", Session: "a1", Kind: "tacit", Title: "the lab tree has no commit rule", Name: "lab-tree-commit", Verdict: "uncertain", Reason: "added rules"},
		{TS: "2026-10-24T01:03:27", Session: "a1", Kind: "tacit", Title: "the lab tree has no commit rule; old entries were wrong", Name: "lab-tree-commit", Verdict: "correct", Reason: "quotes the owner"},
		{TS: "2026-10-24T12:43:36", Session: "b2", Kind: "memory", Title: "urgent tasks run on the top rung", Path: "/h/.claude/projects/-h/memory/urgent.md", Verdict: "uncertain", Reason: "a question made a rule"},
		{TS: "2026-10-24T12:50:00", Session: "b2", Kind: "memory", Title: "urgent tasks run on the top rung", Path: "/h/.claude/projects/-h/memory/urgent.md", Verdict: RulebookApproved, By: "owner"},
		{TS: "2026-10-24T13:00:00", Session: "c3", Kind: "memory", Title: "x", Path: "/h/.claude/projects/-h/memory/x.md", Verdict: "failed"},
		{TS: "2026-10-24T13:05:00", Session: "c3", Tacit: "q-abcdef", Verdict: RulebookEdited, By: "owner", Text: "only on the rig"},
	}
	at := "2026-10-25T00:00:00Z"
	l, ok := RulebookVerdict(rows, Note{Path: "inbox/claude@h/lab-tree-commit.md", Name: "lab-tree-commit", Desc: "the lab tree has no commit rule; old entries were wrong"}, at)
	if !ok || l.Verdict != "correct" || l.By != ledger.ByRulebook || l.Session != "claude:a1" || !ledger.ValidReview(l) {
		t.Fatalf("tacit row: %+v %v", l, ok)
	}
	l, ok = RulebookVerdict(rows, Note{Path: "inbox/import@h/-h/urgent.md", Name: "urgent", Desc: "urgent tasks run on the top rung", ImportedFrom: "/h/.claude/projects/-h/memory/urgent.md"}, at)
	if !ok || l.Verdict != ledger.VerdictConfirmed || l.By != ledger.ByOwner || !ledger.ValidReview(l) {
		t.Fatalf("memory row with the owner's approval: %+v %v", l, ok)
	}
	if _, ok := RulebookVerdict(rows, Note{Path: "p", Name: "x", Desc: "x", ImportedFrom: "/h/.claude/projects/-h/memory/x.md"}, at); ok {
		t.Fatal("a failed review is a verdict")
	}
	if _, ok := RulebookVerdict(rows, Note{Path: "p", Name: "other", Desc: "the lab tree has no commit rule"}, at); ok {
		t.Fatal("another note matched")
	}
	a := RulebookAnswers(rows)
	if got := a["q-abcdef"]; got.Kind != AnswerCorrect || got.Words != "only on the rig" || got.From != "rulebook:c3" {
		t.Fatalf("pane answer %+v", got)
	}
}

func TestLedgerOwnerVerdictStands(t *testing.T) {
	var b []byte
	b = append(b, ledger.Review{Started: "2026-10-24"}.Line()...)
	b = append(b, ledger.Review{Note: "inbox/claude@h/a.md", Name: "a", Verdict: "uncertain", By: "tacit", At: "2026-10-24T10:00:00Z"}.Line()...)
	b = append(b, ledger.Review{Note: "inbox/claude@h/a.md", Name: "a", Verdict: "confirmed", By: "owner", From: "claude:s1", At: "2026-10-24T11:00:00Z"}.Line()...)
	b = append(b, ledger.Review{Note: "inbox/claude@h/a.md", Name: "a", Verdict: "wrong", By: "tacit", At: "2026-10-24T12:00:00Z"}.Line()...)
	b = append(b, ledger.Review{Note: "inbox/claude@h/b.md", Name: "b", Verdict: "confirmed", By: "tacit", At: "2026-10-24T12:00:00Z"}.Line()...) // only the owner confirms
	r := ledger.ParseReviews(b)
	if r.Started != "2026-10-24" {
		t.Fatalf("started %q", r.Started)
	}
	if l, _ := r.ForNote("inbox/claude@h/a.md"); l.Verdict != "confirmed" {
		t.Fatalf("the owner's verdict does not stand: %+v", l)
	}
	if l, ok := r.ForOrigin("a"); !ok || l.Verdict != "confirmed" {
		t.Fatalf("by origin: %+v", l)
	}
	if _, ok := r.ForNote("inbox/claude@h/b.md"); ok {
		t.Fatal("a reviewer's confirmed line was read")
	}
}

func TestReviewWithoutASessionIsNoSource(t *testing.T) {
	calls := 0
	rv := Reviewer{Sources: Sources{Claude: []string{filepath.Join(t.TempDir(), "*", "*.jsonl")}}, Run: func(context.Context, string) (string, float64, string, error) {
		calls++
		return "", 0, "", nil
	}, Redact: func(s []string) ([]string, error) { return s, nil }}
	res, err := rv.Review(context.Background(), Note{Path: "local/claude@h/a.md", Name: "a", Desc: "d", Harness: "claude"})
	if err != nil || res.Line.Verdict != ledger.VerdictNoSource || calls != 0 {
		t.Fatalf("%+v %v %d", res, err, calls)
	}
	res, err = rv.Review(context.Background(), Note{Path: "local/claude@h/b.md", Name: "b", Desc: "d", Harness: "claude", Session: "gone-session"})
	if err != nil || res.Line.Verdict != ledger.VerdictNoSource || !strings.Contains(res.Line.Reason, "not on this machine") || calls != 0 {
		t.Fatalf("%+v %v %d", res, err, calls)
	}
}

func TestReviewJudgesAgainstTheSession(t *testing.T) {
	home := t.TempDir()
	p := filepath.Join(home, ".claude", "projects", "-h", "abc-123.jsonl")
	writeLines(t, p,
		`{"type":"user","entrypoint":"cli","cwd":"/h/p","sessionId":"abc-123","origin":{"kind":"human"},"message":{"content":"the bucket for tacit backups is tacit-eu"}}`,
		`{"type":"assistant","entrypoint":"cli","sessionId":"abc-123","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"tacit add --harness claude --desc \"tacit backups go to tacit-eu\" tacit-backup-bucket"}}]}}`,
	)
	var prompt string
	redacted := false
	rv := Reviewer{Sources: DefaultSources(home), Run: func(_ context.Context, p string) (string, float64, string, error) {
		prompt = p
		return `{"verdict":"correct","reason":"the owner named the bucket"}`, 0.12, "opus", nil
	}, Redact: func(s []string) ([]string, error) { redacted = true; return s, nil }}
	res, err := rv.Review(context.Background(), Note{Path: "inbox/claude@h/tacit-backup-bucket.md", Name: "tacit-backup-bucket", Desc: "tacit backups go to tacit-eu", Body: "tacit backups go to tacit-eu", Harness: "claude", Session: "abc-123", Scope: "projects/tacit"})
	if err != nil || res.Line.Verdict != "correct" || res.Line.Session != "claude:abc-123" || res.Line.CostUSD != 0.12 || !res.Called || !redacted || res.Conv.Cwd != "/h/p" {
		t.Fatalf("%+v %v", res, err)
	}
	for _, want := range []string{"OWNER: the bucket for tacit backups is tacit-eu", "kind: tacit note", "where: tacit scope projects/tacit, name tacit-backup-bucket", "Judge the save."} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	// A redaction that fails sends nothing.
	rv.Redact = func([]string) ([]string, error) { return nil, errors.New("no scrump") }
	prompt = ""
	if _, err := rv.Review(context.Background(), Note{Path: "x", Name: "x", Desc: "x", Harness: "claude", Session: "abc-123"}); err == nil || prompt != "" {
		t.Fatalf("reviewed without redaction: %v", err)
	}
}

func TestReadAnswersFromTheTranscriptAfterTheAsk(t *testing.T) {
	home := t.TempDir()
	p := filepath.Join(home, ".claude", "projects", "-h", "s5.jsonl")
	writeLines(t, p, `{"type":"user","entrypoint":"cli","sessionId":"s5","origin":{"kind":"human"},"message":{"content":"yes, earlier talk"}}`)
	fi, _ := os.Stat(p)
	now := time.Now()
	q := &Queue{}
	q.Add(Item{Note: "n1", Desc: "d", Day: "2026-10-20"}, now)
	it := &q.Items[0]
	it.Asks = []Ask{{Harness: "claude", Session: "s5", Transcript: p, Offset: fi.Size(), At: now.Format(time.RFC3339)}}
	src := DefaultSources(home)
	if ReadAnswers(context.Background(), q, src, nil, now) {
		t.Fatal("an answer before anything was asked")
	}
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	asked, _ := json.Marshal("Done.\n" + it.Text())
	f.WriteString(`{"type":"assistant","entrypoint":"cli","sessionId":"s5","message":{"content":[{"type":"text","text":` + string(asked) + `}]}}` + "\n")
	f.WriteString(`{"type":"user","entrypoint":"cli","sessionId":"s5","origin":{"kind":"peer"},"message":{"content":"yes"}}` + "\n")
	f.Close()
	if ReadAnswers(context.Background(), q, src, nil, now); q.Items[0].State != StateOpen {
		t.Fatal("a peer's yes was taken as the owner's")
	}
	f, _ = os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`{"type":"user","entrypoint":"cli","sessionId":"s5","origin":{"kind":"human"},"message":{"content":"correct it: only in the tacit repo"}}` + "\n")
	f.Close()
	if !ReadAnswers(context.Background(), q, src, nil, now) || q.Items[0].State != StateAnswered {
		t.Fatalf("no answer read: %+v", q.Items[0])
	}
	if a := q.Items[0].Answer; a.Kind != AnswerCorrect || a.Words != "only in the tacit repo" || a.From != "claude:s5" || a.TurnHash == "" {
		t.Fatalf("answer %+v", a)
	}
}

// Codex and pi sessions read the same way: a person's front end and typed
// turns are the owner's, a subagent's brief or a headless run's prompt not.
func TestReadCodexAndPiSessions(t *testing.T) {
	dir := t.TempDir()
	cx := filepath.Join(dir, "rollout-2026-10-24T10-00-00-01a1.jsonl")
	writeLines(t, cx,
		`{"type":"session_meta","payload":{"id":"01a1","cwd":"/h/projects/tacit","originator":"codex-tui"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>cwd</environment_context>"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"keep the hooks under 300 ms"}]}}`,
		`{"type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{\"cmd\":\"tacit add --harness codex --desc x hooks-fast\"}"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Saved."}]}}`,
	)
	c, err := ReadTranscript(cx, digest.Codex)
	if err != nil {
		t.Fatal(err)
	}
	d := Digest(c.Msgs, DigestBudget, DigestRecent)
	if !strings.Contains(d, "OWNER: keep the hooks under 300 ms") || !strings.Contains(d, "NOTE: <environment_context>") || !strings.Contains(d, "ran shell") || !strings.Contains(d, "AGENT: Saved.") {
		t.Fatalf("codex digest:\n%s", d)
	}
	exec := filepath.Join(dir, "rollout-2026-10-24T10-00-00-01a2.jsonl")
	writeLines(t, exec,
		`{"type":"session_meta","payload":{"id":"01a2","cwd":"/h/projects/tacit","originator":"codex_exec"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"the owner wants hooks under 300 ms"}]}}`,
	)
	c, _ = ReadTranscript(exec, digest.Codex)
	for _, m := range c.Msgs {
		if m.Owner {
			t.Fatalf("codex exec prompt is the owner's: %+v", m)
		}
	}
	pi := filepath.Join(dir, "2026-10-24T10-00-00-000Z_p1abc.jsonl")
	writeLines(t, pi,
		`{"type":"session","id":"p1abc","timestamp":"2026-10-24T10:00:00.000Z","cwd":"/h/projects/tacit"}`,
		`{"type":"message","timestamp":"2026-10-24T10:00:09.000Z","message":{"role":"user","content":[{"type":"text","text":"use the fast path for pi"}]}}`,
		`{"type":"message","timestamp":"2026-10-24T10:00:10.000Z","message":{"role":"assistant","content":[{"type":"text","text":"ok"},{"type":"toolCall","name":"bash","arguments":{"command":"tacit add"}}]}}`,
	)
	c, _ = ReadTranscript(pi, digest.Pi)
	d = Digest(c.Msgs, DigestBudget, DigestRecent)
	if !strings.Contains(d, "OWNER: use the fast path for pi") || !strings.Contains(d, "ran bash") {
		t.Fatalf("pi digest:\n%s", d)
	}
	src := Sources{Codex: []string{filepath.Join(dir, "*.jsonl")}, Pi: []string{filepath.Join(dir, "*.jsonl")}}
	if p, _, ok := src.Locate("codex", "01a1"); !ok || p != cx {
		t.Fatalf("locate codex: %q %v", p, ok)
	}
	if p, _, ok := src.Locate("pi", "p1abc"); !ok || p != pi {
		t.Fatalf("locate pi: %q %v", p, ok)
	}
	if _, _, ok := src.Locate("pi", "../etc"); ok {
		t.Fatal("a path was taken for a session id")
	}
}
