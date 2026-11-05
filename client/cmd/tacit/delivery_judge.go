package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/events"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/modelrun"
	"github.com/agent-sh/tacit/client/internal/store"
)

// The dreamer's judge (SPEC.md, "The dreamer's judge"): after a full dream
// run, a model reads a sample of the moments the hooks delivered lines at,
// with hindsight (the request before, what the agent did after), and says
// per line whether it was relevant to what the session was doing, whether
// it confused or misled the agent, and whether it was novel (not already in
// the context, nor found by the agent on its own a moment later). Its
// labels are judge entries of the private delivery log: they label data a
// ranker trains on, so the call runs on the owner's subscription, never
// Bedrock (judgeEnv), and moves no score until its agreement with the hand
// grades clears the rule written before it was measured.

const (
	// judgeDaily is the most lines judged a day: three calls of
	// judgeBatch, a small share of a subscription's daily use, which still
	// gathers about a thousand labels a month for the calibration.
	judgeDaily = 36
	judgeBatch = 12
	// judgeAge: a line is judged once its session moved on past it (what
	// the agent did after is in the transcript), within judgeDays.
	judgeAge  = 30 * time.Minute
	judgeDays = 2
	// judgeContext bounds what the judge reads of the transcript around a
	// moment: the bytes before it, the events after it, each event's text.
	judgeBefore  = 256 << 10
	judgeAfter   = 12
	judgeTextLen = 800
)

// judgeVerdict is one line's labels.
type judgeVerdict struct {
	Block    string  `json:"block"`
	Fact     string  `json:"fact"`
	Relevant string  `json:"relevant"` // yes, no, unsure
	Confused string  `json:"confused"`
	Novel    string  `json:"novel"`
	Why      string  `json:"why,omitempty"`
	Model    string  `json:"model,omitempty"`
	P        float64 `json:"p"` // the probability the line had of being judged
}

// judgeCase is what the judge reads of one line's moment, redacted.
type judgeCase struct {
	Block   string   `json:"block"`
	Fact    string   `json:"fact"`
	Hook    string   `json:"hook"`
	Line    string   `json:"line"`
	Request string   `json:"request"`
	Next    []string `json:"next"`
}

// bedrockVar is an environment variable that routes a model call through
// Bedrock or AWS.
var bedrockVar = regexp.MustCompile(`^(AWS_|CLAUDE_CODE_USE_BEDROCK|CLAUDE_CODE_SKIP_BEDROCK_AUTH|ANTHROPIC_BEDROCK_|ANTHROPIC_DEFAULT_|ANTHROPIC_MODEL$|ANTHROPIC_SMALL_FAST_MODEL)`)

// judgeEnv is env with every Bedrock and AWS variable removed, and an
// error if one is left: the judge runs on the owner's subscription, never
// Bedrock (training data is theirs to label on their own plan).
func judgeEnv(env []string) ([]string, error) {
	var out []string
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if bedrockVar.MatchString(k) {
			continue
		}
		out = append(out, kv)
	}
	for _, kv := range out {
		k, _, _ := strings.Cut(kv, "=")
		if bedrockVar.MatchString(k) || strings.Contains(strings.ToUpper(k), "BEDROCK") {
			return nil, fmt.Errorf("a Bedrock variable is left in the judge's environment: %s", k)
		}
	}
	return out, nil
}

// errJudgeRefused is a refusal or a rate limit of the subscription: the
// pass stops and a later one tries again.
var errJudgeRefused = errors.New("the subscription refused the judge's call")

var rateLimitRx = regexp.MustCompile(`(?i)rate.?limit|usage limit|quota|overloaded|429|too many requests`)

// judgeCall runs one judge prompt on `claude -p` with the subscription's
// login: no Bedrock environment, no user settings or hooks (a scratch
// working directory, local settings only), no MCP server, no tools; the
// session is excluded from dreaming. Tests replace it.
var judgeCall = func(ctx context.Context, model, prompt string) (string, error) {
	env, err := judgeEnv(os.Environ())
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(filepath.Join(deliveryLogDir()), "judge-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	sid := modelrun.NewSessionID()
	_ = modelrun.Exclude(sid)
	cmd := exec.CommandContext(ctx, "claude", "-p", "--output-format", "json", "--model", model, "--session-id", sid,
		"--tools", "", "--setting-sources", "local", "--strict-mcp-config")
	cmd.Dir, cmd.Env, cmd.Stdin = dir, env, strings.NewReader(prompt)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	var res struct {
		Result  string `json:"result"`
		IsError bool   `json:"is_error"`
	}
	_ = json.Unmarshal(out.Bytes(), &res)
	if err != nil || res.IsError {
		if rateLimitRx.MatchString(errb.String() + res.Result) {
			return "", errJudgeRefused
		}
		return "", fmt.Errorf("claude -p: %v %s", err, strings.TrimSpace(errb.String()+" "+res.Result))
	}
	return res.Result, nil
}

func judgeModel() string {
	if m := strings.TrimSpace(os.Getenv("TACIT_DELIVERY_JUDGE_MODEL")); m != "" {
		return m
	}
	return "opus"
}

const judgePrompt = `You judge, with hindsight, lines a memory tool gave a coding agent. For each case you get where the line came (the session's start, the owner's prompt, a tool call about to run), the request it answered, the line, and what the session did next.

Answer three questions per line, each yes, no or unsure:
- relevant: was the line about what the session was doing at that moment?
- confused: did the line mislead or distract the agent (it acted on it wrongly, or chased it off the task)?
- novel: did the line give something the agent did not already have in its context and would not have found on its own a moment later? A correct line the agent already had, or read itself right after, is not novel.

Judge the line's worth at that moment, not whether it is true. Answer JSON only: {"lines":[{"i":<case number>,"relevant":"yes|no|unsure","confused":"yes|no|unsure","novel":"yes|no|unsure","why":"<a few words>"}]}.

%s`

func judgeText(cases []judgeCase) string {
	var b strings.Builder
	for i, c := range cases {
		fmt.Fprintf(&b, "## Case %d\nAt: %s\nRequest: %s\nLine: %s\nNext:\n", i+1, c.Hook, c.Request, c.Line)
		if len(c.Next) == 0 {
			b.WriteString("- (nothing recorded)\n")
		}
		for _, n := range c.Next {
			b.WriteString("- " + n + "\n")
		}
		b.WriteString("\n")
	}
	return fmt.Sprintf(judgePrompt, b.String())
}

var answerWord = map[string]bool{"yes": true, "no": true, "unsure": true}

// judgeCases asks the model for one batch; an answer without three valid
// words for every case is a failure, and nothing of it is kept.
func judgeCases(ctx context.Context, model string, cases []judgeCase) ([]judgeVerdict, error) {
	text, err := judgeCall(ctx, model, judgeText(cases))
	if err != nil {
		return nil, err
	}
	var ans struct {
		Lines []struct {
			I                         int
			Relevant, Confused, Novel string
			Why                       string
		} `json:"lines"`
	}
	if err := json.Unmarshal([]byte(jsonObjectOf(text)), &ans); err != nil {
		return nil, fmt.Errorf("the judge's answer is no JSON: %w", err)
	}
	got := map[int]judgeVerdict{}
	for _, l := range ans.Lines {
		if l.I < 1 || l.I > len(cases) || !answerWord[l.Relevant] || !answerWord[l.Confused] || !answerWord[l.Novel] {
			return nil, fmt.Errorf("the judge's answer has an invalid line: %+v", l)
		}
		c := cases[l.I-1]
		got[l.I] = judgeVerdict{Block: c.Block, Fact: c.Fact, Relevant: l.Relevant, Confused: l.Confused, Novel: l.Novel, Why: judgeClip(l.Why, 200), Model: model}
	}
	if len(got) != len(cases) {
		return nil, fmt.Errorf("the judge answered %d of %d lines", len(got), len(cases))
	}
	out := make([]judgeVerdict, 0, len(got))
	for i := 1; i <= len(cases); i++ {
		out = append(out, got[i])
	}
	return out, nil
}

func jsonObjectOf(s string) string {
	i, j := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if i < 0 || j < i {
		return ""
	}
	return s[i : j+1]
}

func judgeClip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// judgeState is the day's count, kept beside the log.
type judgeState struct {
	Day   string `json:"day"`
	Count int    `json:"count"`
}

func judgeStatePath() string { return filepath.Join(deliveryLogDir(), "judge-state.json") }

func readJudgeState(day string) judgeState {
	var st judgeState
	if b, err := os.ReadFile(judgeStatePath()); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	if st.Day != day {
		st = judgeState{Day: day}
	}
	return st
}

func writeJudgeState(st judgeState) {
	b, _ := json.Marshal(st)
	tmp := judgeStatePath() + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, judgeStatePath())
	}
}

// judgeLine is one delivered line the judge may read.
type judgeLine struct {
	entry logEntry
	cand  logCand
}

// judgeable is the log's delivered lines of the window that a judge may
// read: settled (judgeAge), with a transcript to read, not judged yet.
func judgeable(now time.Time) []judgeLine {
	all := readJudgeDays(now.AddDate(0, 0, -judgeDays))
	judged := map[string]bool{}
	for _, e := range all {
		if e.Hook == "judge" && e.Judge != nil {
			judged[e.Judge.Block+"/"+e.Judge.Fact] = true
		}
	}
	var out []judgeLine
	for _, e := range all {
		t, err := time.Parse(time.RFC3339Nano, e.TS)
		if err != nil || now.Sub(t) < judgeAge || e.TranscriptPath == "" || e.Transcript < 0 {
			continue
		}
		switch e.Hook {
		case "start", "prompt", "push":
		default:
			continue
		}
		for _, c := range e.Cands {
			if c.Delivered && !judged[e.ID+"/"+c.Name] {
				out = append(out, judgeLine{entry: e, cand: c})
			}
		}
	}
	return out
}

func readJudgeDays(since time.Time) []logEntry {
	dir := deliveryLogDir()
	entries, _ := os.ReadDir(dir)
	from := since.UTC().Format("2006-01-02")
	var out []logEntry
	for _, x := range entries {
		m := logDay.FindStringSubmatch(x.Name())
		if m == nil || m[1] < from {
			continue
		}
		path := filepath.Join(dir, x.Name())
		if fi, err := os.Lstat(path); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<26)
		for sc.Scan() {
			var e logEntry
			if json.Unmarshal(sc.Bytes(), &e) == nil && e.ID != "" {
				out = append(out, e)
			}
		}
		f.Close()
	}
	return out
}

// judgeRedact redacts what the judge reads (events.Redact, which fails
// closed); tests stand in for it.
var judgeRedact = events.Redact

// judgeRedactTexts redacts plain texts the same way (digest's redactor).
var judgeRedactTexts = digest.RedactAllContext

// momentOf is a line's moment as the judge reads it: the last turn before
// the block (the request), the block's line (the fact's name and
// description as the store holds them now), and the first events after it,
// all redacted. ok is false when the transcript cannot be read.
func momentOf(ctx context.Context, s *store.Store, l judgeLine) (judgeCase, bool) {
	home, _ := os.UserHomeDir()
	format, ok := transcriptFormat(home, l.entry.TranscriptPath)
	if !ok {
		return judgeCase{}, false
	}
	from := alignedLineStart(l.entry.TranscriptPath, l.entry.Transcript-judgeBefore)
	var before, after []events.Event
	stop := errors.New("enough")
	_, err := events.ReadFile(l.entry.TranscriptPath, format, from, func(ev events.Event) error {
		switch {
		case ev.Offset < l.entry.Transcript:
			if ev.Kind == events.KindOwnerText || ev.Kind == events.KindAgentText && ev.Role == "user" {
				before = append(before, ev)
			}
		case ev.Kind != events.KindHarnessText:
			after = append(after, ev)
			if len(after) >= judgeAfter {
				return stop
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, stop) {
		return judgeCase{}, false
	}
	evs := append(before[max(0, len(before)-1):], after...)
	evs, err = judgeRedact(ctx, evs)
	if err != nil {
		return judgeCase{}, false // nothing leaves the machine unredacted
	}
	c := judgeCase{Block: l.entry.ID, Fact: l.cand.Name, Hook: l.entry.Hook, Line: l.cand.Name + ": " + factDescription(s, l.cand.Path)}
	if len(before) > 0 {
		c.Request = judgeClip(evs[0].Text, judgeTextLen)
		evs = evs[1:]
	} else if l.entry.Hook == "push" {
		c.Request = "a tool call (" + l.entry.Tool + ")"
	}
	for _, ev := range evs {
		text := ev.Text
		who := "agent"
		switch ev.Kind {
		case events.KindToolCall:
			who, text = "tool call "+ev.ToolName, ev.ToolInput
		case events.KindToolResult:
			who, text = "tool result", ev.ToolOutput
		case events.KindOwnerText:
			who = "owner"
		}
		c.Next = append(c.Next, who+": "+judgeClip(text, judgeTextLen))
	}
	return c, true
}

func alignedLineStart(path string, off int64) int64 {
	if off <= 0 {
		return 0
	}
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return 0
	}
	r := bufio.NewReader(f)
	skipped, err := r.ReadBytes('\n')
	if err != nil {
		return 0
	}
	return off + int64(len(skipped))
}

// factDescription is the description of the fact at a store path, or "".
func factDescription(s *store.Store, rel string) string {
	if s == nil || rel == "" || strings.Contains(rel, "..") {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(s.Dir, filepath.FromSlash(rel)))
	if err != nil {
		return ""
	}
	f, err := fact.Parse(rel, data)
	if err != nil {
		return ""
	}
	return f.Meta.Description
}

// deliveryJudgePass judges a sample of the window's delivered lines, up to
// what is left of the day's cap: each settled line is drawn with the same
// probability, logged with its labels. A refusal of the subscription (a
// rate limit) ends the pass with nothing counted; a later pass tries again.
// It never falls back to another route.
func deliveryJudgePass(ctx context.Context, s *store.Store, now time.Time) (int, error) {
	if !deliveryOn("JUDGE", true) || !deliveryLogOn() {
		return 0, nil
	}
	day := now.UTC().Format("2006-01-02")
	st := readJudgeState(day)
	left := deliveryInt("JUDGE_DAILY", judgeDaily) - st.Count
	lines := judgeable(now)
	if left <= 0 || len(lines) == 0 {
		return 0, nil
	}
	n := min(left, len(lines))
	p := float64(n) / float64(len(lines))
	rand.Shuffle(len(lines), func(i, j int) { lines[i], lines[j] = lines[j], lines[i] })
	var cases []judgeCase
	for _, l := range lines {
		if len(cases) >= n {
			break
		}
		if c, ok := momentOf(ctx, s, l); ok {
			cases = append(cases, c)
		}
	}
	model, judged := judgeModel(), 0
	for i := 0; i < len(cases); i += judgeBatch {
		batch := cases[i:min(i+judgeBatch, len(cases))]
		verdicts, err := judgeCases(ctx, model, batch)
		if err != nil {
			writeJudgeState(st)
			return judged, err
		}
		for _, v := range verdicts {
			v.P = p
			v := v
			e := &logEntry{V: deliveryLogVersion, TS: time.Now().UTC().Format(time.RFC3339Nano), Hook: "judge", Judge: &v}
			e.ID = logID(e)
			if appendLogEntry(e, time.Now()) == nil {
				judged++
				st.Count++
			}
		}
	}
	writeJudgeState(st)
	return judged, nil
}

// runDreamJudge is the judge's step of a full dream run.
func runDreamJudge() {
	s, err := store.Default()
	if err != nil || !s.Exists() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	n, err := deliveryJudgePass(ctx, s, time.Now())
	switch {
	case errors.Is(err, errJudgeRefused):
		fmt.Println("dream: judge: the subscription refused the call; the next run tries again")
	case err != nil:
		fmt.Fprintln(os.Stderr, "dream: judge:", err)
	case n > 0:
		fmt.Printf("dream: judge: %d line(s) judged\n", n)
	}
}

// runDeliveryJudge is `tacit delivery-judge`: judge cases read as JSON
// lines on stdin and print the verdicts, one JSON line each, for the eval
// that measures the judge against the hand grades
// (eval/delivery/judge_agreement.py). It runs on the same route as the
// dream pass.
func runDeliveryJudge(args []string) int {
	fs := flag.NewFlagSet("delivery-judge", flag.ExitOnError)
	model := fs.String("model", judgeModel(), "the judge's model")
	fs.Parse(args)
	var cases []judgeCase
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	for sc.Scan() {
		var c judgeCase
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			fmt.Fprintln(os.Stderr, "delivery-judge:", err)
			return 2
		}
		cases = append(cases, c)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	// Every text that leaves the machine is redacted first; a failure sends
	// nothing.
	var texts []string
	for _, c := range cases {
		texts = append(append(texts, c.Request, c.Line), c.Next...)
	}
	red, err := judgeRedactTexts(ctx, texts)
	if err != nil || len(red) != len(texts) {
		fmt.Fprintln(os.Stderr, "delivery-judge: redaction failed; nothing was sent:", err)
		return 1
	}
	k := 0
	for i := range cases {
		cases[i].Request, cases[i].Line = red[k], red[k+1]
		k += 2
		for j := range cases[i].Next {
			cases[i].Next[j] = red[k]
			k++
		}
	}
	enc := json.NewEncoder(os.Stdout)
	for i := 0; i < len(cases); i += judgeBatch {
		verdicts, err := judgeCases(ctx, *model, cases[i:min(i+judgeBatch, len(cases))])
		if err != nil {
			fmt.Fprintln(os.Stderr, "delivery-judge:", err)
			return 1
		}
		for _, v := range verdicts {
			enc.Encode(v)
		}
	}
	return 0
}
