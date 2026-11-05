package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"github.com/agent-sh/tacit/client/internal/store"
)

// The dreamer's judge (SPEC.md, "The dreamer's judge"): after a full dream
// run that succeeded, a model reads a sample of the moments the prompt and
// push hooks delivered lines at, with hindsight (the request, what the
// session did after), and says per line whether it was relevant to what the
// session was doing, whether it confused or misled the agent, and whether it
// was novel (not already in the context, nor found by the agent on its own a
// moment later). Its labels are judge entries of the private delivery log:
// they label data a ranker trains on, so the call runs on the owner's
// subscription, never Bedrock or another gateway (judgeEnv), and moves no
// score until its agreement with the hand grades clears the rule written
// before it was measured.

const (
	// judgeDaily is the most lines judged a day: three calls of
	// judgeBatch, a small share of a subscription's daily use, which still
	// gathers about a thousand labels a month for the calibration.
	judgeDaily = 36
	judgeBatch = 12
	// judgeAge: a line enters a pass once its session moved on past it
	// (what the agent did after is in the transcript), within judgeDays.
	judgeAge  = 30 * time.Minute
	judgeDays = 2
	// judgePending is the most drawn lines kept waiting for the cap or a
	// refused call; judgeTries the calls a line may fail before it is
	// dropped (both counted in the pass's entry).
	judgePending = 3 * judgeDaily
	judgeTries   = 3
	// judgeAim is the share of the cap the day's rate aims at: below 1, so
	// the waiting lines drain on an ordinary day and none are given up.
	judgeAim = 0.8
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
	Model    string  `json:"model,omitempty"`  // the model that answered, as the CLI reports it
	Prompt   string  `json:"prompt,omitempty"` // judgePromptID: which judge this is
	P        float64 `json:"p"`                // the probability the line had of being drawn
}

// judgePassLog is a pass's own entry: what it drew from, at what rate, and
// what became of the drawn lines, so the analysis sees every draw.
type judgePassLog struct {
	Pool       int     `json:"pool"` // lines that entered this pass's draw
	Rate       float64 `json:"rate"` // each one's probability of being drawn
	Drawn      int     `json:"drawn"`
	Judged     int     `json:"judged"`
	Waiting    int     `json:"waiting"`              // drawn and not judged yet (the cap, a refusal)
	Dropped    int     `json:"dropped,omitempty"`    // drawn and given up: too many waiting, or failed judgeTries times
	Unreadable int     `json:"unreadable,omitempty"` // drawn and left out: the transcript or its redaction failed
	// Lost names each drawn line given up or left out this pass
	// (block/fact), so the analysis knows which draws came to no label;
	// Unread and Failed are the ones of them left out because their moment
	// could not be read or redacted, and the ones whose call failed
	// judgeTries times (not random, unlike the give-ups past the waiting
	// bound, which thin the waiting lines' p).
	Lost    []string `json:"lost,omitempty"`
	Unread  []string `json:"unread,omitempty"`
	Failed  []string `json:"failed,omitempty"`
	Refused bool     `json:"refused,omitempty"`
	// Model and Prompt are the judge this pass asked (the model as asked;
	// each label carries the one that answered) and its prompt digest.
	Model  string `json:"model,omitempty"`
	Prompt string `json:"prompt,omitempty"`
	// From and To bound the moments that entered this pass's pool (after
	// From, up to To); UnreadBlocks are the moments in them left out of the
	// draw because their transcript could not be read.
	From         string   `json:"from,omitempty"`
	To           string   `json:"to,omitempty"`
	UnreadBlocks []string `json:"unread_blocks,omitempty"`
}

// judgeMoment is one drawn line as the judge's case is built from it.
type judgeMoment struct {
	Block      string  `json:"block"`
	Fact       string  `json:"fact"`
	Hook       string  `json:"hook"` // prompt, push
	Tool       string  `json:"tool,omitempty"`
	Transcript string  `json:"transcript"`
	Offset     int64   `json:"offset"`
	FactPath   string  `json:"fact_path,omitempty"`
	Line       string  `json:"line,omitempty"` // the line as delivered, when known (the eval's); else built from the store
	P          float64 `json:"p,omitempty"`
	Tries      int     `json:"tries,omitempty"`
}

// judgeCase is what the judge reads of one line's moment, redacted.
type judgeCase struct {
	Block   string
	Fact    string
	Hook    string
	Line    string
	Request string
	Next    []string
}

// judgeEnvKeep is what the judge's environment keeps of the dreamer's: the
// names a process needs to find its tools, its home and the subscription's
// login. Nothing else passes: no Bedrock or AWS route, no gateway
// (ANTHROPIC_BASE_URL, a token, a custom header), no other Claude config
// directory, no plugin directories.
// The match ignores case: Windows spells them as it likes (Path, SystemRoot).
var judgeEnvKeep = regexp.MustCompile(`(?i)^(PATH|HOME|USER|LOGNAME|SHELL|TERM|TZ|TMPDIR|TEMP|TMP|LANG|LC_[A-Z]+|XDG_RUNTIME_DIR|USERPROFILE|APPDATA|LOCALAPPDATA|HOMEDRIVE|HOMEPATH|SYSTEMROOT|COMSPEC|PATHEXT|WINDIR)$`)

// judgeEnvRefuse is a name that must never reach the judge.
var judgeEnvRefuse = regexp.MustCompile(`(?i)bedrock|vertex|foundry|^(ANTHROPIC_|AWS_|CLAUDE_)`)

// judgeEnv is env reduced to judgeEnvKeep, and an error if a name
// judgeEnvRefuse names is left: the judge runs on the owner's subscription,
// never Bedrock or a gateway (training data is theirs to label on their
// own plan).
func judgeEnv(env []string) ([]string, error) {
	var out []string
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if judgeEnvKeep.MatchString(k) {
			out = append(out, kv)
		}
	}
	for _, kv := range out {
		k, _, _ := strings.Cut(kv, "=")
		if judgeEnvRefuse.MatchString(k) {
			return nil, fmt.Errorf("a route variable is left in the judge's environment: %s", k)
		}
	}
	return out, nil
}

// judgeCommand is the judge's `claude -p` call: the subscription's login,
// judgeEnv's environment, no user settings or hooks (local settings only,
// from a scratch working directory), no MCP server, no tools, no session
// kept on disk.
func judgeCommand(ctx context.Context, model, dir string) (*exec.Cmd, error) {
	env, err := judgeEnv(os.Environ())
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "claude", "-p", "--output-format", "json", "--model", model,
		"--tools", "", "--setting-sources", "local", "--strict-mcp-config", "--no-session-persistence")
	cmd.Dir, cmd.Env = dir, env
	return cmd, nil
}

// errJudgeRefused is a refusal or a rate limit of the subscription: the
// pass stops and a later one tries again.
var errJudgeRefused = errors.New("the subscription refused the judge's call")

var rateLimitRx = regexp.MustCompile(`(?i)rate.?limit|usage limit|hit your .*limit|quota|overloaded|429|too many requests`)

// judgeCall runs one judge prompt and answers its text and the model that
// answered. Tests replace it.
var judgeCall = func(ctx context.Context, model, prompt string) (string, string, error) {
	// One fixed working directory: the CLI keeps a little per directory
	// even with no session kept, so a new one a call would pile up.
	dir := filepath.Join(deliveryLogDir(), "judge-cwd")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	// Local settings are read from here: a .claude left in it could set a
	// route, so the call refuses to run over one.
	if _, err := os.Lstat(filepath.Join(dir, ".claude")); err == nil {
		return "", "", fmt.Errorf("%s holds a .claude directory; the judge does not run over it", dir)
	}
	cmd, err := judgeCommand(ctx, model, dir)
	if err != nil {
		return "", "", err
	}
	cmd.Stdin = strings.NewReader(prompt)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	var res struct {
		Result     string                     `json:"result"`
		IsError    bool                       `json:"is_error"`
		ModelUsage map[string]json.RawMessage `json:"modelUsage"`
	}
	_ = json.Unmarshal(out.Bytes(), &res)
	if err != nil || res.IsError {
		if rateLimitRx.MatchString(errb.String() + res.Result) {
			return "", "", errJudgeRefused
		}
		return "", "", fmt.Errorf("claude -p: %v %s", err, judgeClip(errb.String()+" "+res.Result, 300))
	}
	return res.Result, answeredBy(res.ModelUsage, model), nil
}

// answeredBy is the model the CLI says answered: modelUsage's one key that
// is not a small helper model, else the name asked for.
func answeredBy(usage map[string]json.RawMessage, asked string) string {
	var names []string
	for k := range usage {
		if !strings.Contains(k, "haiku") {
			names = append(names, k)
		}
	}
	if len(names) == 1 {
		return names[0]
	}
	return asked
}

func judgeModel() string {
	if m := strings.TrimSpace(os.Getenv("TACIT_DELIVERY_JUDGE_MODEL")); m != "" {
		return m
	}
	return "opus"
}

const judgePrompt = `You judge, with hindsight, lines a memory tool gave a coding agent. For each case you get where the line came (the owner's prompt, or a tool call about to run), the request it answered, the line, and what the session did next.

Answer three questions per line, each yes, no or unsure:
- relevant: was the line about what the session was doing at that moment?
- confused: did the line mislead or distract the agent (it acted on it wrongly, or chased it off the task)?
- novel: did the line give something the agent did not already have in its context and would not have found on its own a moment later? A correct line the agent already had, or read itself right after, is not novel.

Judge the line's worth at that moment, not whether it is true. Answer JSON only: {"lines":[{"i":<case number>,"relevant":"yes|no|unsure","confused":"yes|no|unsure","novel":"yes|no|unsure"}]}.

%s`

// judgeCaseVersion is the version of how a case is built from its moment
// (momentOf, judgeText): raise it with any change to what the model reads.
const judgeCaseVersion = 1

// judgePromptID names the judge: a digest of its prompt, the case builder's
// version, its bounds and its words for a hook. A change to any of them is a
// new judge, measured again (eval/DELIVERY.md, step 5).
var judgePromptID = func() string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%d %d %d\x00%s %s", judgePrompt, judgeCaseVersion, judgeBefore, judgeAfter, judgeTextLen, judgeHookWords["prompt"], judgeHookWords["push"])))
	return hex.EncodeToString(h[:6])
}()

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
	text, by, err := judgeCall(ctx, model, judgeText(cases))
	if err != nil {
		return nil, err
	}
	var ans struct {
		Lines []struct {
			I                         int
			Relevant, Confused, Novel string
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
		got[l.I] = judgeVerdict{Block: c.Block, Fact: c.Fact, Relevant: l.Relevant, Confused: l.Confused, Novel: l.Novel, Model: by, Prompt: judgePromptID}
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

// judgeState is the judge's place, kept beside the log: the day's count and
// rate, how far the draws have come, and the drawn lines still waiting.
type judgeState struct {
	Day      string        `json:"day"`
	Count    int           `json:"count"`
	Rate     float64       `json:"rate,omitempty"`      // the day's draw rate
	Seen     int           `json:"seen,omitempty"`      // lines that entered a draw today
	PrevSeen int           `json:"prev_seen,omitempty"` // and on the last day with a pass
	Through  string        `json:"through,omitempty"`   // every line up to here has had its one draw
	Pending  []judgeMoment `json:"pending,omitempty"`
}

func judgeStatePath() string { return filepath.Join(deliveryLogDir(), "judge-state.json") }

func readJudgeState(day string) judgeState {
	var st judgeState
	if b, err := os.ReadFile(judgeStatePath()); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	if st.Day != day {
		if st.Day != "" {
			st.PrevSeen = st.Seen
		}
		st.Day, st.Count, st.Rate, st.Seen = day, 0, 0, 0
	}
	return st
}

func writeJudgeState(st judgeState) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := judgeStatePath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, judgeStatePath())
}

// judgePool is the lines that enter this pass's draw: delivered by a
// prompt or push block whose moment is after the last pass's (through) and
// at least judgeAge old, within judgeDays, with a transcript to read. Each
// line enters exactly one draw.
func judgePool(now time.Time, through time.Time) (out []judgeMoment, unread []string, from, cutoff time.Time) {
	cutoff, floor := now.Add(-judgeAge), now.AddDate(0, 0, -judgeDays)
	if through.Before(floor) {
		through = floor
	}
	home, _ := os.UserHomeDir()
	for _, e := range readJudgeDays(through) {
		t, err := time.Parse(time.RFC3339Nano, e.TS)
		if err != nil || !t.After(through) || t.After(cutoff) || e.TranscriptPath == "" || e.Transcript < 0 {
			continue
		}
		if e.Hook != "prompt" && e.Hook != "push" {
			continue
		}
		// A moment whose transcript cannot be read now has no chance of a
		// label: none of its lines enters the draw, and the pass names it.
		if _, ok := transcriptFormat(home, e.TranscriptPath); !ok {
			unread = append(unread, e.ID)
			continue
		}
		if fi, err := os.Stat(e.TranscriptPath); err != nil || !fi.Mode().IsRegular() {
			unread = append(unread, e.ID)
			continue
		}
		for _, c := range e.Cands {
			if c.Delivered {
				out = append(out, judgeMoment{Block: e.ID, Fact: c.Name, Hook: e.Hook, Tool: e.Tool, Transcript: e.TranscriptPath, Offset: e.Transcript, FactPath: c.Path})
			}
		}
	}
	return out, unread, through, cutoff
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

// judgeRedactTexts redacts what the judge reads (the dreamer's redactor,
// which fails closed); tests stand in for it.
var judgeRedactTexts = digest.RedactAllContext

// judgeRand is the draw's source; tests fix it.
var judgeRand = rand.Float64

func judgeShuffle(m []judgeMoment) {
	rand.Shuffle(len(m), func(i, j int) { m[i], m[j] = m[j], m[i] })
}

// judgeHookWords is where a line came, as the judge reads it.
var judgeHookWords = map[string]string{"prompt": "the owner's prompt", "push": "a tool call about to run"}

// momentOf is a drawn line's case: the request (a prompt block's own prompt,
// the owner's turn at the block, written just before or just after the
// moment; a push's tool call and the request it serves), the line (as
// delivered when the moment holds it, else the fact's name and description
// as the store holds them now) and the first events after it. Every text is
// redacted before it is clipped; ok is false when the transcript cannot be
// read or redaction fails, and nothing is sent.
func momentOf(ctx context.Context, s *store.Store, m judgeMoment) (judgeCase, bool) {
	home, _ := os.UserHomeDir()
	format, ok := transcriptFormat(home, m.Transcript)
	if !ok || judgeHookWords[m.Hook] == "" {
		return judgeCase{}, false
	}
	from := alignedLineStart(m.Transcript, m.Offset-judgeBefore)
	var owner, last *events.Event // the owner's last turn, the last event, before the moment
	var after []events.Event
	stop := errors.New("enough")
	_, err := events.ReadFile(m.Transcript, format, from, func(ev events.Event) error {
		if ev.Kind == events.KindHarnessText {
			return nil
		}
		if ev.Offset < m.Offset {
			ev := ev
			last = &ev
			if judgeOwnerTurn(ev) {
				owner = &ev
			}
			return nil
		}
		after = append(after, ev)
		if len(after) > judgeAfter {
			return stop
		}
		return nil
	})
	if err != nil && !errors.Is(err, stop) {
		return judgeCase{}, false
	}
	const pre = 4 * judgeTextLen // clipped to this before redaction, to judgeTextLen after
	var req []string
	switch m.Hook {
	case "prompt":
		// The hook may run before the harness writes the prompt.
		if len(after) > 0 && judgeOwnerTurn(after[0]) {
			req, after = append(req, judgeClip(after[0].Text, pre)), after[1:]
		} else if owner != nil {
			req = append(req, judgeClip(owner.Text, pre))
		}
	case "push":
		// The call is the last event before the block, or the first after
		// it when the harness writes it later.
		var call *events.Event
		if len(after) > 0 && after[0].Kind == events.KindToolCall && after[0].ToolName == m.Tool {
			call, after = &after[0], after[1:]
		} else if last != nil && last.Kind == events.KindToolCall && last.ToolName == m.Tool {
			call = last
		}
		if call != nil {
			req = append(req, "the tool call "+call.ToolName+": "+judgeClip(call.ToolInput, pre))
		} else {
			req = append(req, "a tool call ("+m.Tool+")")
		}
		if owner != nil {
			req = append(req, "made for the request: "+judgeClip(owner.Text, pre))
		}
	}
	if len(after) > judgeAfter {
		after = after[:judgeAfter]
	}
	line := m.Line
	if line == "" {
		line = m.Fact + ": " + factDescription(s, m.FactPath)
	}
	texts := []string{strings.Join(req, "; "), judgeClip(line, pre)}
	for _, ev := range after {
		texts = append(texts, eventWords(ev, pre))
	}
	red, err := judgeRedactTexts(ctx, texts)
	if err != nil || len(red) != len(texts) {
		return judgeCase{}, false // nothing leaves the machine unredacted
	}
	c := judgeCase{Block: m.Block, Fact: m.Fact, Hook: judgeHookWords[m.Hook], Request: judgeClip(red[0], judgeTextLen), Line: judgeClip(red[1], judgeTextLen)}
	for _, t := range red[2:] {
		c.Next = append(c.Next, judgeClip(t, judgeTextLen))
	}
	return c, true
}

func judgeOwnerTurn(ev events.Event) bool {
	return ev.Kind == events.KindOwnerText || ev.Kind == events.KindAgentText && ev.Role == "user"
}

func eventWords(ev events.Event, n int) string {
	switch ev.Kind {
	case events.KindToolCall:
		return "tool call " + ev.ToolName + ": " + judgeClip(ev.ToolInput, n)
	case events.KindToolResult:
		return "tool result: " + judgeClip(ev.ToolOutput, n)
	case events.KindOwnerText:
		return "owner: " + judgeClip(ev.Text, n)
	}
	return "agent: " + judgeClip(ev.Text, n)
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

// factDescription is the description of the fact at a store or overlay
// path, or "".
func factDescription(s *store.Store, rel string) string {
	if strings.Contains(rel, "..") {
		return ""
	}
	if f := factAt(s, rel); f != nil {
		return f.Meta.Description
	}
	return ""
}

// deliveryJudgePass gives every line that settled since the last pass its
// one draw, at the day's rate (judgeDaily over the last day's lines, so
// about judgeDaily are drawn a day), and judges the drawn lines up to what
// is left of the day's cap; the rest wait for a later pass. Each verdict
// carries its line's draw probability, and the pass logs its own entry. A
// refusal of the subscription (a rate limit) ends the pass, the lines wait,
// and a later pass tries again; it never falls back to another route.
func deliveryJudgePass(ctx context.Context, s *store.Store, now time.Time) (int, error) {
	if !deliveryOn("JUDGE", true) || !deliveryLogOn() {
		return 0, nil
	}
	st := readJudgeState(now.UTC().Format("2006-01-02"))
	through, _ := time.Parse(time.RFC3339Nano, st.Through)
	pool, unreadBlocks, poolFrom, poolTo := judgePool(now, through)
	daily := deliveryInt("JUDGE_DAILY", judgeDaily)
	pass := judgePassLog{Pool: len(pool), UnreadBlocks: unreadBlocks,
		From: poolFrom.UTC().Format(time.RFC3339Nano), To: poolTo.UTC().Format(time.RFC3339Nano)}
	if len(pool) > 0 {
		if st.Rate == 0 {
			// Aimed at judgeAim of the cap, so the waiting lines drain on
			// an ordinary day; the base is at least the cap, so a quiet day
			// (or a first pool) cannot set a rate that draws everything.
			base := max(st.PrevSeen, len(pool), daily, 1)
			st.Rate = min(1, judgeAim*float64(daily)/float64(base))
		}
		pass.Rate = st.Rate
		for _, m := range pool {
			if judgeRand() < st.Rate {
				m.P = st.Rate
				st.Pending = append(st.Pending, m)
				pass.Drawn++
			}
		}
		st.Seen += len(pool)
	}
	st.Through = now.Add(-judgeAge).UTC().Format(time.RFC3339Nano)
	if over := len(st.Pending) - judgePending; over > 0 {
		// Given up at random, not by age, and named in the pass's entry. A
		// waiting line is kept with the probability kept/waiting, so its p
		// (the chance it is labelled) is thinned by that share.
		judgeShuffle(st.Pending)
		for _, m := range st.Pending[:over] {
			pass.Lost = append(pass.Lost, m.Block+"/"+m.Fact)
		}
		keep := float64(judgePending) / float64(len(st.Pending))
		st.Pending, pass.Dropped = st.Pending[over:], over
		for i := range st.Pending {
			st.Pending[i].P *= keep
		}
	}
	var err error
	if len(st.Pending) > 0 && st.Count < daily {
		take := st.Pending[:min(len(st.Pending), daily-st.Count)]
		rest := slicesClone(st.Pending[len(take):])
		var cases []judgeCase
		var moments []judgeMoment
		for _, m := range take {
			if c, ok := momentOf(ctx, s, m); ok {
				cases, moments = append(cases, c), append(moments, m)
			} else {
				pass.Unreadable++
				pass.Lost = append(pass.Lost, m.Block+"/"+m.Fact)
				pass.Unread = append(pass.Unread, m.Block+"/"+m.Fact)
			}
		}
		model := judgeModel()
		var waiting []judgeMoment
		for i := 0; i < len(cases); i += judgeBatch {
			j := min(i+judgeBatch, len(cases))
			if err != nil {
				waiting = append(waiting, moments[i:j]...)
				continue
			}
			var verdicts []judgeVerdict
			verdicts, err = judgeCases(ctx, model, cases[i:j])
			if err != nil {
				pass.Refused = errors.Is(err, errJudgeRefused)
				for _, m := range moments[i:j] {
					if !pass.Refused {
						m.Tries++
					}
					if m.Tries >= judgeTries {
						pass.Dropped++
						pass.Lost = append(pass.Lost, m.Block+"/"+m.Fact)
						pass.Failed = append(pass.Failed, m.Block+"/"+m.Fact)
					} else {
						waiting = append(waiting, m)
					}
				}
				continue
			}
			for k, v := range verdicts {
				v.P = moments[i+k].P
				v := v
				e := &logEntry{V: deliveryLogVersion, TS: time.Now().UTC().Format(time.RFC3339Nano), Hook: "judge", Judge: &v}
				e.ID = logID(e)
				if appendLogEntry(e, time.Now()) == nil {
					pass.Judged++
					st.Count++
				}
			}
		}
		st.Pending = append(waiting, rest...)
	}
	pass.Waiting = len(st.Pending)
	// Every pass writes its entry, an empty one too: its window says which
	// moments had their draw (eval/delivery/offline.py's population).
	{
		pass.Model, pass.Prompt = judgeModel(), judgePromptID
		e := &logEntry{V: deliveryLogVersion, TS: time.Now().UTC().Format(time.RFC3339Nano), Hook: "judge-pass", JudgePass: &pass}
		e.ID = logID(e)
		_ = appendLogEntry(e, time.Now())
	}
	if werr := writeJudgeState(st); werr != nil && err == nil {
		err = werr
	}
	return pass.Judged, err
}

func slicesClone(m []judgeMoment) []judgeMoment { return append([]judgeMoment(nil), m...) }

// runDreamJudge is the judge's step of a full dream run that succeeded,
// while the run still holds the dreamer's lock.
func runDreamJudge() {
	s, err := store.Default()
	if err != nil || !s.Exists() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
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

// runDeliveryJudge is `tacit delivery-judge`: judge moments read as JSON
// lines on stdin (judgeMoment: a transcript, the offset of the moment, the
// hook, a push's tool, the line as delivered) and print the verdicts, one
// JSON line each, for the eval that measures the judge against the hand
// grades (eval/delivery/judge_agreement.py). The cases are built, redacted
// and asked exactly as the dream pass does.
func runDeliveryJudge(args []string) int {
	fs := flag.NewFlagSet("delivery-judge", flag.ExitOnError)
	model := fs.String("model", judgeModel(), "the judge's model")
	fs.Parse(args)
	var moments []judgeMoment
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	for sc.Scan() {
		var m judgeMoment
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			fmt.Fprintln(os.Stderr, "delivery-judge:", err)
			return 2
		}
		moments = append(moments, m)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	// A moment whose transcript or redaction fails is not sent: its line is
	// answered {"block", "fact", "unreadable": true}, in its place.
	var cases []judgeCase
	ok := make([]bool, len(moments))
	for i, m := range moments {
		if c, good := momentOf(ctx, nil, m); good {
			cases, ok[i] = append(cases, c), true
		}
	}
	var all []judgeVerdict
	for i := 0; i < len(cases); i += judgeBatch {
		verdicts, err := judgeCases(ctx, *model, cases[i:min(i+judgeBatch, len(cases))])
		if err != nil {
			fmt.Fprintln(os.Stderr, "delivery-judge:", err)
			return 1
		}
		all = append(all, verdicts...)
	}
	enc := json.NewEncoder(os.Stdout)
	k := 0
	for i, m := range moments {
		if !ok[i] {
			enc.Encode(map[string]any{"block": m.Block, "fact": m.Fact, "unreadable": true})
			continue
		}
		enc.Encode(all[k])
		k++
	}
	return 0
}
