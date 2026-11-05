package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// The judge's environment keeps what a process needs and the Bedrock
// route, never a gateway or another config directory; without the route it
// is an error (the judge is skipped there); a refused name the keep list
// would pass is an error too.
func TestJudgeEnvBedrockRoute(t *testing.T) {
	env, err := judgeEnv([]string{"PATH=/bin", "HOME=/h", "AWS_REGION=x", "AWS_BEARER_TOKEN_BEDROCK=t", "CLAUDE_CODE_USE_BEDROCK=1",
		"ANTHROPIC_DEFAULT_OPUS_MODEL=global.x", "ANTHROPIC_BASE_URL=https://gw", "ANTHROPIC_AUTH_TOKEN=t", "ANTHROPIC_API_KEY=k",
		"ANTHROPIC_BEDROCK_BASE_URL=https://gw", "AWS_ENDPOINT_URL=https://gw", "CLAUDE_CODE_SKIP_BEDROCK_AUTH=1", "CLAUDE_CODE_USE_GATEWAY=1",
		"ANTHROPIC_CUSTOM_HEADERS=h", "CLAUDE_CONFIG_DIR=/c", "CLAUDE_CODE_USE_VERTEX=1", "CLAUDE_CODE_PLUGIN_DIRS=/p", "LANG=C.UTF-8"})
	if err != nil || strings.Join(env, " ") != "PATH=/bin HOME=/h AWS_REGION=x AWS_BEARER_TOKEN_BEDROCK=t CLAUDE_CODE_USE_BEDROCK=1 ANTHROPIC_DEFAULT_OPUS_MODEL=global.x LANG=C.UTF-8" {
		t.Fatalf("kept: %v %v", env, err)
	}
	// A gateway's environment (another dreamer's route): no Bedrock, no judge.
	for _, e := range [][]string{
		{"PATH=/bin", "ANTHROPIC_BASE_URL=https://gw", "ANTHROPIC_AUTH_TOKEN=t", "ANTHROPIC_API_KEY=k"},
		{"PATH=/bin", "CLAUDE_CODE_USE_BEDROCK=1"},
		{"PATH=/bin", "CLAUDE_CODE_USE_BEDROCK=0", "AWS_BEARER_TOKEN_BEDROCK=t"},
	} {
		if _, err := judgeEnv(e); !errors.Is(err, errJudgeNoRoute) {
			t.Fatalf("%v: %v", e, err)
		}
	}
	old := judgeEnvKeep
	t.Cleanup(func() { judgeEnvKeep = old })
	for _, name := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_BEDROCK_BASE_URL", "AWS_ENDPOINT_URL_BEDROCK_RUNTIME", "CLAUDE_CODE_SKIP_BEDROCK_AUTH", "CLAUDE_CODE_USE_VERTEX", "HTTPS_PROXY",
		"CLAUDE_CONFIG_DIR", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_CUSTOM_HEADERS", "CLAUDE_CODE_PLUGIN_DIRS"} {
		judgeEnvKeep = regexp.MustCompile(`^(PATH|` + name + `|CLAUDE_CODE_USE_BEDROCK|AWS_BEARER_TOKEN_BEDROCK)$`)
		if _, err := judgeEnv([]string{"PATH=/bin", name + "=x", "CLAUDE_CODE_USE_BEDROCK=1", "AWS_BEARER_TOKEN_BEDROCK=t"}); err == nil || errors.Is(err, errJudgeNoRoute) {
			t.Fatalf("%s was passed: %v", name, err)
		}
	}
}

// With no Bedrock route the pass draws nothing, sends nothing and logs why.
func TestJudgePassWithoutARoute(t *testing.T) {
	f := newJudgeFixture(t, 2)
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "")
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "")
	t.Setenv("ANTHROPIC_BASE_URL", "https://gw")
	n, err := deliveryJudgePass(context.Background(), nil, f.now)
	if n != 0 || !errors.Is(err, errJudgeNoRoute) || len(f.sent) != 0 {
		t.Fatalf("judged %d, %v, sent %d", n, err, len(f.sent))
	}
	skipped := false
	for _, e := range readDeliveryLog(t, f.state) {
		skipped = skipped || e.Hook == "judge-skip" && e.Reason == "no Bedrock route"
		if e.Hook == "judge-pass" {
			t.Fatalf("a pass without a route: %+v", e)
		}
	}
	if !skipped {
		t.Fatal("no judge-skip entry")
	}
	if _, err := os.Stat(judgeStatePath()); err == nil {
		t.Fatal("a pass without a route moved the judge's state")
	}
}

// The judge's call is `claude -p` with the reduced environment, local
// settings only, no MCP server, no tools and no session kept.
func TestJudgeCommand(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", "https://gw")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
	t.Setenv("AWS_PROFILE", "p")
	for _, k := range []string{"AWS_BEARER_TOKEN_BEDROCK", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "TACIT_DELIVERY_JUDGE_MODEL"} {
		t.Setenv(k, "")
	}
	if judgeModel() != "global.anthropic.claude-opus-5-5" {
		t.Fatalf("default model %q", judgeModel())
	}
	cmd, err := judgeCommand(context.Background(), judgeModel(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cmd.Env, "CLAUDE_CODE_USE_BEDROCK=1") || !slices.Contains(cmd.Env, "AWS_PROFILE=p") {
		t.Fatalf("the route did not reach the call: %v", cmd.Env)
	}
	for _, kv := range cmd.Env {
		if judgeEnvRefused(strings.SplitN(kv, "=", 2)[0]) {
			t.Fatalf("passed %s", kv)
		}
	}
	args := strings.Join(cmd.Args, " ")
	for _, want := range []string{"claude -p", "--model global.anthropic.claude-opus-5-5", "--tools  ", "--setting-sources local", "--strict-mcp-config", "--no-session-persistence"} {
		if !strings.Contains(args, want) {
			t.Errorf("args %q lack %q", args, want)
		}
	}
}

type judgeFixture struct {
	state, tr string
	off       int64
	now       time.Time
	sent      []string
	refuse    bool
}

// newJudgeFixture writes a transcript (the prompt written after the
// block's moment, as the hook can run first; a push's call before its
// moment) and a delivery log of a prompt block of n notes, a push block, a
// start block and a fresh block.
func newJudgeFixture(t *testing.T, n int) *judgeFixture {
	home := t.TempDir()
	t.Setenv("HOME", home)
	f := &judgeFixture{state: filepath.Join(home, "state"), now: time.Now()}
	t.Setenv("TACIT_STATE_DIR", f.state)
	t.Setenv("TACIT_DELIVERY_JUDGE", "on")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "test")
	f.tr = filepath.Join(home, ".claude", "projects", "-demo", "s1.jsonl")
	os.MkdirAll(filepath.Dir(f.tr), 0o755)
	first := `{"type":"user","timestamp":"2026-11-01T09:00:00Z","sessionId":"s1","userType":"external","message":{"role":"user","content":"an earlier request"}}` + "\n"
	rest := `{"type":"user","timestamp":"2026-11-01T10:00:00Z","sessionId":"s1","userType":"external","message":{"role":"user","content":"fix the scheduler lock race SECRET-a"}}` + "\n" +
		`{"type":"assistant","timestamp":"2026-11-01T10:01:00Z","sessionId":"s1","message":{"role":"assistant","content":[{"type":"text","text":"I will take the shard lock first."},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"go test ./sched"}}]}}` + "\n"
	os.WriteFile(f.tr, []byte(first+rest), 0o644)
	f.off = int64(len(first))
	pushOff := int64(len(first + rest))
	os.WriteFile(f.tr, []byte(first+rest+`{"type":"user","timestamp":"2026-11-01T10:02:00Z","sessionId":"s1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok sched"}]}}`+"\n"), 0o644)
	old := f.now.Add(-2 * time.Hour).UTC().Format(time.RFC3339Nano)
	var cands []logCand
	for i := 0; i < n; i++ {
		cands = append(cands, logCand{Name: fmt.Sprintf("note-%d", i), Delivered: true, Prov: "agent"})
	}
	cands = append(cands, logCand{Name: "not-shown", Delivered: false})
	writeLogEntry(&logEntry{V: 1, ID: "00000000000000a1", TS: old, Hook: "prompt", Session: "s1", TranscriptPath: f.tr, Transcript: f.off, Cands: cands}, f.now)
	writeLogEntry(&logEntry{V: 1, ID: "00000000000000a2", TS: old, Hook: "push", Tool: "Bash", Session: "s1", TranscriptPath: f.tr, Transcript: pushOff,
		Cands: []logCand{{Name: "push-note", Delivered: true}}}, f.now)
	writeLogEntry(&logEntry{V: 1, ID: "00000000000000a3", TS: old, Hook: "start", Session: "s1", TranscriptPath: f.tr, Transcript: 0,
		Cands: []logCand{{Name: "start-note", Delivered: true}}}, f.now)
	writeLogEntry(&logEntry{V: 1, ID: "00000000000000a4", TS: f.now.UTC().Format(time.RFC3339Nano), Hook: "prompt", Session: "s1", TranscriptPath: f.tr, Transcript: f.off,
		Cands: []logCand{{Name: "too-fresh", Delivered: true}}}, f.now)
	// A moment whose transcript no harness of this machine writes: never drawn.
	writeLogEntry(&logEntry{V: 1, ID: "00000000000000a6", TS: old, Hook: "prompt", Session: "s1", TranscriptPath: filepath.Join(home, "elsewhere", "x.jsonl"), Transcript: 1,
		Cands: []logCand{{Name: "unreadable-note", Delivered: true}}}, f.now)
	call, red, rnd := judgeCall, judgeRedactTexts, judgeRand
	t.Cleanup(func() { judgeCall, judgeRedactTexts, judgeRand = call, red, rnd })
	judgeRedactTexts = func(ctx context.Context, texts []string) ([]string, error) {
		out := make([]string, len(texts))
		for i, s := range texts {
			out[i] = strings.ReplaceAll(s, "SECRET", "[redacted]")
		}
		return out, nil
	}
	judgeRand = func() float64 { return 0 } // every line drawn
	judgeCall = func(ctx context.Context, model, prompt string) (string, string, error) {
		if f.refuse {
			return "", "", errJudgeRefused
		}
		f.sent = append(f.sent, prompt)
		var ls []string
		for i := 1; i <= strings.Count(prompt, "## Case "); i++ {
			ls = append(ls, fmt.Sprintf(`{"i":%d,"relevant":"yes","confused":"no","novel":"unsure"}`, i))
		}
		return `{"lines":[` + strings.Join(ls, ",") + `]}`, "claude-test-model", nil
	}
	return f
}

func judgeEntries(t *testing.T, state string) (verdicts []judgeVerdict, passes []judgePassLog) {
	for _, e := range readDeliveryLog(t, state) {
		switch {
		case e.Hook == "judge" && e.Judge != nil:
			verdicts = append(verdicts, *e.Judge)
		case e.Hook == "judge-pass" && e.JudgePass != nil:
			passes = append(passes, *e.JudgePass)
		}
	}
	return
}

// A pass draws each settled prompt or push line once, at the day's rate,
// judges the drawn lines up to the cap with each verdict's draw probability
// and the model that answered, and leaves the rest waiting; start lines,
// lines not shown and fresh lines are not drawn; every text is redacted.
func TestDeliveryJudgePass(t *testing.T) {
	f := newJudgeFixture(t, 5)
	t.Setenv("TACIT_DELIVERY_JUDGE_DAILY", "3")
	n, err := deliveryJudgePass(context.Background(), nil, f.now)
	if err != nil || n != 3 {
		t.Fatalf("judged %d: %v", n, err)
	}
	p := f.sent[0]
	if len(f.sent) != 1 || !strings.Contains(p, "Request: fix the scheduler lock race [redacted]-a") || strings.Contains(p, "SECRET") ||
		!strings.Contains(p, "take the shard lock first") || strings.Contains(p, "too-fresh") || strings.Contains(p, "start-note") || strings.Contains(p, "not-shown") {
		t.Fatalf("prompt: %s", p)
	}
	verdicts, passes := judgeEntries(t, f.state)
	// The rate aims at 0.8 of the cap over the lines of the first pool: 2.4 of 6.
	if len(verdicts) != 3 || !near(verdicts[0].P, 0.4) || verdicts[0].Model != "claude-test-model" || verdicts[0].Prompt != judgePromptID || verdicts[0].Block != "00000000000000a1" {
		t.Fatalf("verdicts: %+v", verdicts)
	}
	if len(passes) != 1 || passes[0].Pool != 6 || passes[0].Drawn != 6 || passes[0].Judged != 3 || passes[0].Waiting != 3 {
		t.Fatalf("pass: %+v", passes)
	}
	if strings.Join(passes[0].UnreadBlocks, ",") != "00000000000000a6" || passes[0].From == "" || passes[0].To == "" || strings.Join(passes[0].Answered, ",") != "claude-test-model" {
		t.Fatalf("the pass's window: %+v", passes[0])
	}
	// The cap is spent: nothing more today, and no line is drawn twice.
	if n, _ := deliveryJudgePass(context.Background(), nil, f.now.Add(time.Minute)); n != 0 {
		t.Fatalf("past the cap: %d", n)
	}
	// Another day: a refusal keeps the waiting lines and counts nothing.
	f.refuse = true
	if n, err := deliveryJudgePass(context.Background(), nil, f.now.Add(24*time.Hour)); n != 0 || !errors.Is(err, errJudgeRefused) {
		t.Fatalf("refused: %d %v", n, err)
	}
	f.refuse = false
	n, err = deliveryJudgePass(context.Background(), nil, f.now.Add(25*time.Hour))
	if err != nil || n != 3 {
		t.Fatalf("the waiting lines: %d %v", n, err)
	}
	verdicts, _ = judgeEntries(t, f.state)
	var push *judgeVerdict
	seen := map[string]int{}
	for i, v := range verdicts {
		seen[v.Block+"/"+v.Fact]++
		if v.Fact == "push-note" {
			push = &verdicts[i]
		}
	}
	if len(verdicts) != 6 || push == nil || !near(push.P, 0.4) {
		t.Fatalf("verdicts: %+v", verdicts)
	}
	for k, c := range seen {
		if c != 1 {
			t.Fatalf("%s judged %d times", k, c)
		}
	}
	last := f.sent[len(f.sent)-1]
	if !strings.Contains(last, "At: a tool call about to run\nRequest: the tool call Bash: ") || !strings.Contains(last, "go test ./sched") || !strings.Contains(last, "made for the request: fix the scheduler") {
		t.Fatalf("the push's case: %s", last)
	}
	// Off: nothing.
	t.Setenv("TACIT_DELIVERY_JUDGE", "off")
	writeLogEntry(&logEntry{V: 1, ID: "00000000000000a5", TS: f.now.Add(24 * time.Hour).UTC().Format(time.RFC3339Nano), Hook: "prompt", Session: "s1", TranscriptPath: f.tr, Transcript: f.off,
		Cands: []logCand{{Name: "later", Delivered: true}}}, f.now.Add(24*time.Hour))
	if n, _ := deliveryJudgePass(context.Background(), nil, f.now.Add(26*time.Hour)); n != 0 {
		t.Fatal("judged with the judge off")
	}
}

// A line the draw passes over is never drawn again, and a failed redaction
// sends nothing and counts the line unreadable.
func TestJudgeDrawsOnceAndRedactsFirst(t *testing.T) {
	f := newJudgeFixture(t, 2)
	t.Setenv("TACIT_DELIVERY_JUDGE_DAILY", "1") // a rate of 1 in 3
	judgeRand = func() float64 { return 0.99 }  // nothing drawn
	if n, _ := deliveryJudgePass(context.Background(), nil, f.now); n != 0 {
		t.Fatalf("judged %d", n)
	}
	judgeRand = func() float64 { return 0 }
	if n, _ := deliveryJudgePass(context.Background(), nil, f.now.Add(time.Minute)); n != 0 || len(f.sent) != 0 {
		t.Fatalf("a line had a second draw: %d", n)
	}
	f2 := newJudgeFixture(t, 2)
	t.Setenv("TACIT_DELIVERY_JUDGE_DAILY", "36")
	judgeRedactTexts = func(ctx context.Context, texts []string) ([]string, error) { return nil, errors.New("no redactor") }
	if n, _ := deliveryJudgePass(context.Background(), nil, f2.now); n != 0 || len(f2.sent) != 0 {
		t.Fatalf("sent without redaction: %d %q", n, f2.sent)
	}
	_, passes := judgeEntries(t, f2.state)
	if len(passes) != 1 || passes[0].Unreadable != 3 || len(passes[0].Unread) != 3 || passes[0].Prompt != judgePromptID || passes[0].Model == "" {
		t.Fatalf("pass: %+v", passes)
	}
}

// An answer that leaves a line out, or says something but yes, no or
// unsure, is a failure: nothing of it is kept.
func TestJudgeAnswersAreChecked(t *testing.T) {
	call := judgeCall
	t.Cleanup(func() { judgeCall = call })
	cases := []judgeCase{{Block: "b", Fact: "f1"}, {Block: "b", Fact: "f2"}}
	for _, ans := range []string{`{"lines":[{"i":1,"relevant":"yes","confused":"no","novel":"no"}]}`, `{"lines":[{"i":1,"relevant":"maybe","confused":"no","novel":"no"},{"i":2,"relevant":"yes","confused":"no","novel":"no"}]}`, "no json"} {
		judgeCall = func(ctx context.Context, model, prompt string) (string, string, error) { return ans, model, nil }
		if _, err := judgeCases(context.Background(), "m", cases); err == nil {
			t.Errorf("accepted %q", ans)
		}
	}
}

// The eval's command builds each case from its moment as the dream pass
// does, with the line as delivered, redacts it, and sends nothing when a
// moment fails.
func TestDeliveryJudgeCommand(t *testing.T) {
	f := newJudgeFixture(t, 1)
	run := func(in string) (int, string) {
		r, w, _ := os.Pipe()
		w.WriteString(in)
		w.Close()
		out := filepath.Join(t.TempDir(), "out")
		of, _ := os.Create(out)
		stdin, stdout := os.Stdin, os.Stdout
		os.Stdin, os.Stdout = r, of
		code := runDeliveryJudge(nil)
		os.Stdin, os.Stdout = stdin, stdout
		r.Close()
		of.Close()
		b, _ := os.ReadFile(out)
		return code, string(b)
	}
	m, _ := json.Marshal(judgeMoment{Block: "b1", Fact: "f", Hook: "prompt", Transcript: f.tr, Offset: f.off, Line: "f: SECRET-x as delivered"})
	code, out := run(string(m) + "\n")
	if code != 0 || len(f.sent) != 1 || strings.Contains(f.sent[0], "SECRET") || !strings.Contains(f.sent[0], "Line: f: [redacted]-x as delivered") ||
		!strings.Contains(f.sent[0], "Request: fix the scheduler lock race") || !strings.Contains(out, `"fact":"f"`) {
		t.Fatalf("code %d, out %s, sent %q", code, out, f.sent)
	}
	bad, _ := json.Marshal(judgeMoment{Block: "b2", Fact: "g", Hook: "prompt", Transcript: filepath.Join(t.TempDir(), "gone.jsonl")})
	// A moment that cannot be read is answered unreadable and not sent; the
	// rest are judged.
	code, out = run(string(bad) + "\n" + string(m) + "\n")
	if code != 0 || len(f.sent) != 2 || strings.Count(f.sent[1], "## Case ") != 1 ||
		!strings.HasPrefix(out, `{"block":"b2","fact":"g","unreadable":true}`) || !strings.Contains(out, `"fact":"f"`) {
		t.Fatalf("code %d, out %s, sent %d", code, out, len(f.sent))
	}
}

// The model a verdict names is the one the CLI says answered, past its small
// helper model; the alias asked for when the answer does not say.
func TestAnsweredBy(t *testing.T) {
	u := func(names ...string) map[string]json.RawMessage {
		m := map[string]json.RawMessage{}
		for _, n := range names {
			m[n] = json.RawMessage(`{}`)
		}
		return m
	}
	for _, c := range []struct {
		usage map[string]json.RawMessage
		want  string
	}{{u("claude-x-1", "claude-haiku-2"), "claude-x-1"}, {u(), "opus"}, {u("a", "b"), "opus"}} {
		if got := answeredBy(c.usage, "opus"); got != c.want {
			t.Errorf("answeredBy(%v) = %q, want %q", c.usage, got, c.want)
		}
	}
}

// A refusal leaves a waiting line's tries alone; another failure counts
// one, and a line that fails judgeTries times is given up and named.
func TestJudgeTriesAndLost(t *testing.T) {
	f := newJudgeFixture(t, 1)
	fail := errors.New("bad answer")
	var calls int
	judgeCall = func(ctx context.Context, model, prompt string) (string, string, error) {
		calls++
		if f.refuse {
			return "", "", errJudgeRefused
		}
		return "", "", fail
	}
	f.refuse = true
	for i := 0; i < judgeTries+1; i++ {
		deliveryJudgePass(context.Background(), nil, f.now.Add(time.Duration(i)*time.Minute))
	}
	st := readJudgeState(f.now.UTC().Format("2006-01-02"))
	if len(st.Pending) != 2 || st.Pending[0].Tries != 0 {
		t.Fatalf("refusals counted tries: %+v", st.Pending)
	}
	f.refuse = false
	for i := 0; i < judgeTries; i++ {
		deliveryJudgePass(context.Background(), nil, f.now.Add(time.Duration(10+i)*time.Minute))
	}
	st = readJudgeState(f.now.UTC().Format("2006-01-02"))
	_, passes := judgeEntries(t, f.state)
	var lost []string
	for _, p := range passes {
		lost = append(lost, p.Lost...)
	}
	var failed []string
	for _, p := range passes {
		failed = append(failed, p.Failed...)
	}
	if len(failed) != 2 {
		t.Fatalf("failed %v", failed)
	}
	if len(st.Pending) != 0 || len(lost) != 2 || calls == 0 {
		t.Fatalf("pending %+v, lost %v", st.Pending, lost)
	}
}

// Past judgePending waiting lines, some are given up at random and named;
// a quiet first day does not draw everything.
func TestJudgeOverflowAndRate(t *testing.T) {
	f := newJudgeFixture(t, 1)
	st := readJudgeState(f.now.UTC().Format("2006-01-02"))
	for i := 0; i < judgePending+5; i++ {
		st.Pending = append(st.Pending, judgeMoment{Block: "old", Fact: fmt.Sprint(i), Hook: "prompt", Transcript: f.tr, Offset: f.off, P: 0.1})
	}
	st.Count = judgeDaily // nothing judged this pass
	writeJudgeState(st)
	deliveryJudgePass(context.Background(), nil, f.now)
	_, passes := judgeEntries(t, f.state)
	st = readJudgeState(f.now.UTC().Format("2006-01-02"))
	if len(passes) != 1 || passes[0].Dropped != 7 || len(passes[0].Lost) != 7 || len(st.Pending) != judgePending {
		t.Fatalf("pass %+v, pending %d", passes, len(st.Pending))
	}
	// The kept lines' p is thinned by the share kept.
	for _, m := range st.Pending {
		if m.Block == "old" && !near(m.P, 0.1*float64(judgePending)/float64(judgePending+7)) {
			t.Fatalf("p %v", m.P)
		}
	}
	// The pool held 2 lines: the rate is 0.8 of the cap over the cap.
	if !near(passes[0].Rate, 0.8) {
		t.Fatalf("rate %v", passes[0].Rate)
	}
}

// A push whose call the harness writes after the hook ran still has its
// call as its request.
func TestJudgePushCallWrittenAfter(t *testing.T) {
	f := newJudgeFixture(t, 1)
	m := judgeMoment{Block: "b", Fact: "f", Hook: "push", Tool: "Bash", Transcript: f.tr, Offset: f.off + 1, Line: "f: x"}
	// The offset just inside the prompt's line aligns to the next line: the
	// assistant's text and call come after it.
	b, _ := os.ReadFile(f.tr)
	i := strings.Index(string(b), `{"type":"assistant"`)
	m.Offset = int64(i)
	os.WriteFile(f.tr, []byte(strings.Replace(string(b), `{"type":"text","text":"I will take the shard lock first."},`, "", 1)), 0o644)
	c, ok := momentOf(context.Background(), nil, m)
	if !ok || !strings.HasPrefix(c.Request, "the tool call Bash: ") || !strings.Contains(c.Request, "made for the request: fix the scheduler") {
		t.Fatalf("case %+v %v", c, ok)
	}
}

func near(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }

// A .claude in the judge's working directory, whose local settings could
// set a route, stops the call before it runs.
func TestJudgeRefusesLocalSettings(t *testing.T) {
	t.Setenv("TACIT_STATE_DIR", t.TempDir())
	os.MkdirAll(filepath.Join(deliveryLogDir(), "judge-cwd", ".claude"), 0o700)
	if _, _, err := judgeCall(context.Background(), "opus", "x"); err == nil || !strings.Contains(err.Error(), ".claude") {
		t.Fatalf("ran over a .claude: %v", err)
	}
}

// Only the route's own errors end a pass without counting against the
// lines; an error of the request counts.
func TestRouteRefusal(t *testing.T) {
	for text, want := range map[string]bool{
		"API Error: 429 Too Many Requests":                      true,
		"API Error: 403 {\"message\":\"x\"}":                    true,
		"API Error: 401 unauthorized":                           true,
		"rate limit reached":                                    true,
		"Overloaded":                                            true,
		"The security token included in the request is expired": true,
		"ExpiredTokenException: token expired":                  true,
		"UnrecognizedClientException":                           true,
		"AccessDeniedException: no access":                      true,
		`API Error: 400 {"type":"invalid_request_error","message":"prompt is too long: 210000 tokens > 200000 maximum"}`: false,
		"max_tokens: 64000 > 32000":                     false,
		"API Error: 500 request_id req_403abc internal": false,
		"took 14030ms":                                  false,
		"invalid token count in response":               false,
	} {
		if got := routeRefusal(text); got != want {
			t.Errorf("routeRefusal(%q) = %v, want %v", text, got, want)
		}
	}
}

// The AWS SDK's config files: none on the bearer route, and a profile
// route whose config names an endpoint refuses the call.
func TestJudgeCommandAWSConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TACIT_STATE_DIR", t.TempDir())
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "t")
	t.Setenv("AWS_PROFILE", "")
	cmd, err := judgeCommand(context.Background(), "m", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var cfg string
	for _, kv := range cmd.Env {
		if v, ok := strings.CutPrefix(kv, "AWS_CONFIG_FILE="); ok {
			cfg = v
		}
	}
	if b, err := os.ReadFile(cfg); cfg == "" || err != nil || len(b) != 0 {
		t.Fatalf("no empty AWS config: %q %v", cfg, err)
	}
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "")
	t.Setenv("AWS_PROFILE", "p")
	os.MkdirAll(filepath.Join(home, ".aws"), 0o700)
	os.WriteFile(filepath.Join(home, ".aws", "config"), []byte("[profile p]\nendpoint_url = https://gw.example\n"), 0o600)
	if _, err := judgeCommand(context.Background(), "m", t.TempDir()); err == nil {
		t.Fatal("a profile config with an endpoint_url was run over")
	}
}
