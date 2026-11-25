package main

// The relevance gate (SPEC.md, "The relevance gate"): after the prompt
// block's own selection, a model scores each of its lines for the request
// and the session's repository, and a line under the threshold is left out.
// Nothing takes its place. A gate that is off, misconfigured, slow or
// answers badly leaves the block as the selection made it.
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/search"
)

// gateSystemPrompt is the question the gate asks. Measured offline on graded
// prompt blocks (eval/DELIVERY.md, "A relevance gate"); a change to it is a
// new gate, measured again.
const gateSystemPrompt = "A memory tool picked these saved lines for the user's message to a coding agent by matching words; some share only words and are about something else. " +
	"Score each line from 0 to 100 for whether the agent, doing what this message asks in this session's repository, could use it. " +
	"70-100: about the task's subject, or a tool, file, service, person, project or kind of work the task involves. " +
	"30-69: plausibly related: the message is short or vague and the line could be about what it refers to. " +
	"0-29: clearly about a different subject, project or task. " +
	"When the message is vague, prefer the middle band to a low score. " +
	`Reply with JSON only, mapping each line number to its score, e.g. {"0": 90, "1": 5}.`

const (
	gateDefaultTimeout = 1500 * time.Millisecond
	gateMaxTimeout     = 2 * time.Second
	gateTextLen        = 800 // runes of the request, and of each line, the model reads
	gateMinTokens      = 200 // the reply's budget: this, or more for a long block (gateTokens)
)

// The Bedrock route's names a gate reads, from the environment or its
// key_file: the bearer token (Bedrock itself), or the loopback route (a
// Bedrock endpoint on this machine's loopback with the auth skipped, as
// the judge's: a machine that keeps the token in a local service).
const (
	gateBedrockToken = "AWS_BEARER_TOKEN_BEDROCK"
	gateOpenAIKey    = "TACIT_GATE_API_KEY"
)

var errGateOff = errors.New("gate_off")

type gateConfig struct {
	Provider  string
	Model     string
	Endpoint  string // openai: the base URL
	Region    string
	Threshold float64
	Timeout   time.Duration
	KeyFile   string
	Fields    map[string]any // extra request fields the model takes (thinking off)
}

// gateConfiguration reads the gate's settings (TACIT_GATE_*, from the
// config file's gate section). errGateOff when no provider is set.
func gateConfiguration() (gateConfig, error) {
	c := gateConfig{Provider: os.Getenv("TACIT_GATE_PROVIDER"), Model: os.Getenv("TACIT_GATE_MODEL"), Endpoint: os.Getenv("TACIT_GATE_ENDPOINT"),
		Region: os.Getenv("TACIT_GATE_REGION"), KeyFile: os.Getenv("TACIT_GATE_KEY_FILE"), Timeout: gateDefaultTimeout}
	switch c.Provider {
	case "":
		return c, errGateOff
	case providerBedrock:
		if c.Region == "" {
			c.Region = "us-east-1"
		}
		if !bedrockRegion.MatchString(c.Region) || c.Model == "" {
			return c, errors.New("invalid_region_or_model")
		}
	case providerOpenAI:
		c.Endpoint = strings.TrimRight(c.Endpoint, "/")
		if !allowedEndpoint(c.Endpoint) || c.Model == "" {
			return c, errors.New("invalid_endpoint_or_model")
		}
	default:
		return c, errors.New("invalid_provider")
	}
	raw := os.Getenv("TACIT_GATE_THRESHOLD")
	v, err := strconv.ParseFloat(raw, 64)
	if raw == "" || err != nil || math.IsNaN(v) || v < 0 || v > 1 {
		return c, errors.New("invalid_threshold")
	}
	c.Threshold = v
	if raw := os.Getenv("TACIT_GATE_TIMEOUT"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 || d > gateMaxTimeout {
			return c, errors.New("invalid_timeout")
		}
		c.Timeout = d
	}
	if raw := os.Getenv("TACIT_GATE_FIELDS"); raw != "" {
		if json.Unmarshal([]byte(raw), &c.Fields) != nil {
			return c, errors.New("invalid_fields")
		}
	}
	return c, nil
}

// gateRoute is where the call goes and the bearer key it carries ("" for
// none). Bedrock: the bearer token to the region's endpoint, else the
// loopback route; nothing else (an endpoint off this machine never gets a
// call without the token's route). OpenAI-compatible: the endpoint, with
// its key when there is one.
func gateRoute(c gateConfig) (string, string, error) {
	if c.Provider == providerOpenAI {
		key, err := readUtilityKey(gateOpenAIKey, c.KeyFile)
		if err != nil && !errors.Is(err, errKeyUnavailable) {
			return "", "", err
		}
		return c.Endpoint + "/chat/completions", key, nil
	}
	path := "/model/" + url.PathEscape(c.Model) + "/converse"
	if key, err := readUtilityKey(gateBedrockToken, c.KeyFile); err == nil {
		return "https://bedrock-runtime." + c.Region + ".amazonaws.com" + path, key, nil
	} else if !errors.Is(err, errKeyUnavailable) {
		return "", "", err
	}
	base, err := readUtilityKey(judgeLoopbackURL, c.KeyFile)
	if err != nil {
		return "", "", errors.New("no_route")
	}
	skip, _ := readUtilityKey(judgeLoopbackSkip, c.KeyFile)
	if !judgeSwitchOn(skip) || !judgeLoopbackHost(base) {
		return "", "", errors.New("no_route")
	}
	return strings.TrimRight(base, "/") + path, "", nil
}

// gateRequest is the user turn: the session's repository, the request and
// the numbered lines, as the offline measurement asked them.
func gateRequest(repo, request string, lines []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Session repository: %s\n\nUser message:\n%s\n\nLines:\n", repo, request)
	for i, l := range lines {
		fmt.Fprintf(&b, "%d: %s\n", i, l)
	}
	return b.String()
}

// gateTokens is the reply's token budget for n lines: a score is a few
// tokens, so a block past the default ceiling still gets its whole answer.
func gateTokens(n int) int { return max(gateMinTokens, 32+16*n) }

// gateBody is the provider's request body for n lines.
func gateBody(c gateConfig, user string, n int) []byte {
	var body map[string]any
	if c.Provider == providerBedrock {
		body = map[string]any{
			"system":          []map[string]string{{"text": gateSystemPrompt}},
			"messages":        []map[string]any{{"role": "user", "content": []map[string]string{{"text": user}}}},
			"inferenceConfig": map[string]int{"maxTokens": gateTokens(n)},
		}
		if len(c.Fields) > 0 {
			body["additionalModelRequestFields"] = c.Fields
		}
	} else {
		body = map[string]any{}
		for k, v := range c.Fields {
			body[k] = v
		}
		body["model"] = c.Model
		body["max_tokens"] = gateTokens(n)
		body["messages"] = []map[string]string{{"role": "system", "content": gateSystemPrompt}, {"role": "user", "content": user}}
	}
	b, _ := json.Marshal(body)
	return b
}

// parseGateScores reads the first JSON object of the reply: every line
// number from 0 must have a number in [0,100]; anything else is a failed
// call, not a guess.
func parseGateScores(text string, count int) ([]float64, error) {
	m := scoreObject.FindString(text)
	if m == "" {
		return nil, errors.New("response_invalid")
	}
	var got map[string]any
	if json.Unmarshal([]byte(m), &got) != nil {
		return nil, errors.New("response_invalid")
	}
	scores := make([]float64, count)
	for i := range scores {
		v, ok := got[strconv.Itoa(i)].(float64)
		if !ok || math.IsNaN(v) || v < 0 || v > 100 {
			return nil, errors.New("response_invalid")
		}
		scores[i] = v / 100
	}
	return scores, nil
}

// gateScores asks the model for one score in [0,1] per line, in order.
// The repository's name, the request and the lines are redacted first, as
// whole texts and line by line, then flattened and cut; a redaction that
// fails sends nothing.
func gateScores(ctx context.Context, c gateConfig, repo, request string, hits []search.Hit) ([]float64, error) {
	endpoint, key, err := gateRoute(c)
	if err != nil {
		return nil, err
	}
	// Lines stay lines until redaction is done: flattened first, a hit
	// that starts on one line could take the next line's secret with it.
	texts := []string{runeCut(repo, gateTextLen), runeCut(request, 4*gateTextLen)}
	for _, h := range hits {
		texts = append(texts, runeCut(h.Name+": "+h.Description, 4*gateTextLen))
	}
	red, err := digest.RedactLinesContext(ctx, texts)
	if err != nil || len(red) != len(texts) {
		return nil, errors.New("redaction_failed")
	}
	lines := make([]string, len(hits))
	for i := range hits {
		lines[i] = judgeClip(red[i+2], gateTextLen)
	}
	raw, err := postJSON(ctx, endpoint, key, gateBody(c, gateRequest(judgeClip(red[0], gateTextLen), judgeClip(red[1], gateTextLen), lines), len(hits)))
	if err != nil {
		return nil, err
	}
	text, _, err := modelReply(c.Provider, raw)
	if err != nil {
		return nil, err
	}
	return parseGateScores(text, len(hits))
}

// runeCut is the head of s that judgeClip(s, n) is drawn from, its lines
// and spaces kept: as many runes as it takes for n runes once each run of
// white space is one space and the ends are trimmed. Redacted and clipped
// to m <= n, it reads as judgeClip(judgeClip(s, n), m) does, less what
// redaction masks: the judge's cases read as before.
func runeCut(s string, n int) string {
	count, space := 0, false
	for i, r := range s {
		if unicode.IsSpace(r) {
			space = count > 0
			continue
		}
		if space {
			count++ // the one space a run of white space becomes
			space = false
		}
		if count >= n {
			return s[:i]
		}
		count++
	}
	return s
}

// gateRepo is what the gate is told of the session's place: its first
// repository, else the folder it works in.
func gateRepo(repos []string, cwd string) string {
	if len(repos) > 0 {
		return repos[0]
	}
	if b := filepath.Base(filepath.Clean(cwd)); cwd != "" && b != "." && b != string(filepath.Separator) {
		return b
	}
	return "none"
}

// gated is hits less the lines the gate scores under its threshold, logged
// as dropped by "gate", with each line's score and the gate's status in
// the block's entry. With the gate off it is hits; on any failure, or past
// the deadline, hits too (fail open), the status saying why.
func gated(ctx context.Context, request, repo string, hits []search.Hit) []search.Hit {
	if len(hits) == 0 {
		return hits
	}
	c, err := gateConfiguration()
	if errors.Is(err, errGateOff) {
		return hits
	}
	rec := &logGate{Model: c.Model, Threshold: c.Threshold}
	if dlog != nil {
		dlog.Gate = rec
	}
	if err != nil {
		rec.Status = err.Error()
		return hits
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	type result struct {
		scores []float64
		err    error
	}
	done := make(chan result, 1)
	copied := append([]search.Hit(nil), hits...)
	go func() {
		s, err := gateScores(ctx, c, repo, request, copied)
		done <- result{s, err}
	}()
	var r result
	select {
	case r = <-done:
	case <-ctx.Done():
		// Cancellation ends the redactor's process and the call; give them
		// a moment to say so before the hook goes on without the scores.
		select {
		case <-done:
		case <-time.After(250 * time.Millisecond):
		}
		r = result{err: errors.New("deadline")}
	}
	rec.MS = time.Since(start).Milliseconds()
	if ctx.Err() != nil {
		r.err = errors.New("deadline") // whatever the late call said: it was cut short
	}
	if r.err != nil {
		rec.Status = r.err.Error()
		return hits
	}
	rec.Status = "ok"
	out := hits[:0:0]
	for i, h := range hits {
		v := round3(r.scores[i])
		if dlog != nil {
			dlog.cand(h).Gate = &v
		}
		if r.scores[i] >= c.Threshold {
			out = append(out, h)
		}
	}
	logDropped(hits, out, "gate")
	return out
}
