package main

// Opt-in hosted utility selection. Retrieval still owns scope, lifecycle and
// applicability. A failed scorer returns the existing local selection.
import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"

	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/platform"
	"github.com/agent-sh/tacit/client/internal/search"
)

const utilityModel = "jev-1.13.0"
const utilityEndpoint = "https://api.typesafe.ai/v1/systemone"
const utilityQuestion = "Does this memory add concrete information or a constraint useful for fulfilling the current request, beyond what the request already states? Mere topic overlap is not sufficient."
const utilityTrue = "The memory provides an additional actionable fact, constraint, preference, or procedure that is relevant to fulfilling the task. It is not already stated in the task."
const utilityFalse = "The memory only repeats or paraphrases the task, shares its topic without helping, or suggests an inappropriate or contradictory action."

// Utility providers. None is the default: the local selection matched Jev in
// live sessions (eval/VALUE.md). jev is the most accurate model measured; the
// others let the owner plug in any model that answers the same rubric.
const (
	providerJev       = "jev"       // TypeSafe Jev, pinned model and origin
	providerSystemOne = "systemone" // any /v1/systemone-compatible endpoint
	providerBedrock   = "bedrock"   // any Amazon Bedrock model via Converse
	providerOpenAI    = "openai"    // any OpenAI-compatible /chat/completions
)

type utilityConfig struct {
	Provider  string
	Model     string
	Endpoint  string // systemone: the POST URL; openai: the base URL; bedrock: derived from Region
	Region    string
	KeyVar    string // the only variable read from the environment or key_file
	KeyNeeded bool
	Threshold float64
	Timeout   time.Duration
	KeyFile   string
	AuditDir  string
}

var bedrockRegion = regexp.MustCompile(`^[a-z]{2}(-gov)?-[a-z]+-[0-9]$`)

func utilityProvider() string {
	switch p := os.Getenv("TACIT_UTILITY_PROVIDER"); p {
	case providerJev, providerSystemOne, providerBedrock, providerOpenAI:
		return p
	}
	return ""
}

func utilityConfiguration() (utilityConfig, error) {
	c := utilityConfig{Provider: utilityProvider(), Threshold: 0.70, Timeout: 2 * time.Second, KeyFile: os.Getenv("TACIT_UTILITY_KEY_FILE"), AuditDir: os.Getenv("TACIT_UTILITY_AUDIT_DIR"),
		Model: os.Getenv("TACIT_UTILITY_MODEL"), Endpoint: os.Getenv("TACIT_UTILITY_ENDPOINT"), Region: os.Getenv("TACIT_UTILITY_REGION")}
	raw := os.Getenv("TACIT_UTILITY_THRESHOLD")
	if raw != "" {
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
			return c, errors.New("invalid_threshold")
		}
		c.Threshold = v
	}
	if raw := os.Getenv("TACIT_UTILITY_TIMEOUT"); raw != "" {
		v, err := time.ParseDuration(raw)
		if err != nil || v <= 0 || v > queryBudget {
			return c, errors.New("invalid_timeout")
		}
		c.Timeout = v
	}
	switch c.Provider {
	case providerJev:
		// Pinned: the TypeSafe key is only ever sent to the official origin.
		c.Model, c.Endpoint, c.KeyVar, c.KeyNeeded = utilityModel, utilityEndpoint, "TYPESAFE_API_KEY", true
		return c, nil
	case providerSystemOne:
		c.KeyVar = "TACIT_UTILITY_API_KEY"
		if !allowedEndpoint(c.Endpoint) {
			return c, errors.New("invalid_endpoint")
		}
	case providerOpenAI:
		c.KeyVar = "TACIT_UTILITY_API_KEY"
		c.Endpoint = strings.TrimRight(c.Endpoint, "/")
		if !allowedEndpoint(c.Endpoint) || c.Model == "" {
			return c, errors.New("invalid_endpoint_or_model")
		}
	case providerBedrock:
		c.KeyVar, c.KeyNeeded = "AWS_BEARER_TOKEN_BEDROCK", true
		if c.Region == "" {
			c.Region = "us-east-1"
		}
		if !bedrockRegion.MatchString(c.Region) || c.Model == "" {
			return c, errors.New("invalid_region_or_model")
		}
		c.Endpoint = "https://bedrock-runtime." + c.Region + ".amazonaws.com/model/" + url.PathEscape(c.Model) + "/converse"
	default:
		return c, errors.New("utility_disabled")
	}
	// Scores from different models are not on one scale: a threshold must be chosen per model.
	if raw == "" {
		return c, errors.New("threshold_required")
	}
	return c, nil
}

// allowedEndpoint: HTTPS anywhere, plain HTTP only on loopback (a local model server).
func allowedEndpoint(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	host := u.Hostname()
	return u.Scheme == "http" && (host == "localhost" || host == "127.0.0.1" || host == "::1")
}

// Read only the provider's key variable, without executing a shell or importing
// unrelated variables from an env file into the hook's process.
func providerKey(c utilityConfig) (string, error) {
	key, err := readUtilityKey(c.KeyVar, c.KeyFile)
	if errors.Is(err, errKeyUnavailable) && !c.KeyNeeded {
		return "", nil // a local endpoint may need no key
	}
	return key, err
}

var errKeyUnavailable = errors.New("key_unavailable")

// utilityKey reads the Jev key (TYPESAFE_API_KEY) from the environment or a key file.
func utilityKey(path string) (string, error) { return readUtilityKey("TYPESAFE_API_KEY", path) }

func readUtilityKey(name, path string) (string, error) {
	if k := strings.TrimSpace(os.Getenv(name)); k != "" {
		return k, nil
	}
	if path == "" {
		return "", errKeyUnavailable
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", errKeyUnavailable
	}
	f, err := os.Open(path)
	if err != nil {
		return "", errKeyUnavailable
	}
	defer f.Close()
	scan := bufio.NewScanner(io.LimitReader(f, 1<<20))
	for scan.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(scan.Text(), "\ufeff"))
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != name {
			continue
		}
		value = strings.TrimSpace(value)
		if strings.HasPrefix(value, "\"") || strings.HasPrefix(value, "'") {
			quote := value[0]
			end := strings.LastIndexByte(value[1:], quote)
			if end < 0 {
				return "", errors.New("invalid_key_file")
			}
			end++
			if tail := strings.TrimSpace(value[end+1:]); tail != "" && !strings.HasPrefix(tail, "#") {
				return "", errors.New("invalid_key_file")
			}
			value = value[1:end]
		} else if i := strings.Index(value, " #"); i >= 0 {
			value = strings.TrimSpace(value[:i])
		}
		if value == "" || strings.ContainsAny(value, "\r\n") {
			return "", errors.New("invalid_key_file")
		}
		return value, nil
	}
	return "", errKeyUnavailable
}

type utilityAnswer struct {
	Type  string   `json:"type"`
	Score *float64 `json:"noul"`
}
type utilityQuestionBody struct {
	Type         string            `json:"type"`
	Instructions map[string]string `json:"instructions"`
	Criteria     map[string]string `json:"criteria"`
}
type utilityRequest struct {
	Model     string                         `json:"model,omitempty"`
	State     map[string]string              `json:"state"`
	Questions map[string]utilityQuestionBody `json:"questions"`
}
type utilityResponse struct {
	Model   string                   `json:"model"`
	Answers map[string]utilityAnswer `json:"answers"`
	Usage   struct {
		InputTokens int `json:"input_tokens"`
	} `json:"usage"`
}
type utilityAuditHit struct {
	ID       string   `json:"id"`
	Summary  string   `json:"summary,omitempty"`
	Score    *float64 `json:"score,omitempty"`
	Selected bool     `json:"selected"`
}
type utilityAudit struct {
	Time        time.Time         `json:"time"`
	Model       string            `json:"model"`
	Threshold   float64           `json:"threshold"`
	Status      string            `json:"status"`
	QueryHash   string            `json:"query_sha256"`
	Task        string            `json:"task,omitempty"`
	ElapsedMS   float64           `json:"elapsed_ms"`
	InputTokens int               `json:"input_tokens,omitempty"`
	Candidates  []utilityAuditHit `json:"candidates"`
}

func utilityPromptSelection(ctx context.Context, p string, hits, derived []search.Hit, seen map[string]bool, floor float64) []search.Hit {
	fallback := promptSelection(hits, derived, seen, floor)
	if utilityProvider() == "" || len(hits)+len(derived) == 0 {
		return fallback
	}
	cfg, _ := utilityConfiguration()
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	// Even filesystem stalls must not hold a prompt. This hook is a short-lived
	// CLI process; an unresponsive worker cannot keep main alive after fallback.
	copiedSeen := make(map[string]bool, len(seen))
	for key, value := range seen {
		copiedSeen[key] = value
	}
	copiedHits, copiedDerived := append([]search.Hit{}, hits...), append([]search.Hit{}, derived...)
	type outcome struct {
		hits  []search.Hit
		audit utilityAudit
	}
	result := make(chan outcome, 1)
	start := time.Now()
	go func() {
		var audit utilityAudit
		selected := utilityPromptSelectionWork(ctx, p, copiedHits, copiedDerived, copiedSeen, floor, func(a utilityAudit) { audit = a })
		result <- outcome{selected, audit}
	}()
	var out outcome
	select {
	case out = <-result:
	case <-ctx.Done():
		// Give cancellation and pipe cleanup time to acknowledge before the
		// CLI exits. A stalled filesystem still cannot hold the prompt.
		select {
		case out = <-result:
		case <-time.After(250 * time.Millisecond):
		}
		out.hits = fallback
		out.audit.Status = "deadline"
	}
	if out.audit.Model == "" {
		hash := sha256.Sum256([]byte(p))
		out.audit.Model = cfg.Model
		out.audit.Time = time.Now().UTC()
		out.audit.Threshold = cfg.Threshold
		out.audit.QueryHash = hex.EncodeToString(hash[:])
		for _, h := range append(copiedHits, copiedDerived...) {
			out.audit.Candidates = append(out.audit.Candidates, utilityAuditHit{ID: h.Path})
		}
	}
	out.audit.ElapsedMS = float64(time.Since(start).Microseconds()) / 1000
	return finishUtilitySelection(out.hits, out.audit, cfg.AuditDir, appendUtilityAudit)
}

// The selected result is final before best-effort logging starts. Slow audit
// storage may lose a receipt, but must not discard successful scoring.
func finishUtilitySelection(selected []search.Hit, audit utilityAudit, dir string, write func(string, utilityAudit)) []search.Hit {
	names := map[string]bool{}
	for _, h := range selected {
		names[h.Path] = true
	}
	for i := range audit.Candidates {
		audit.Candidates[i].Selected = names[audit.Candidates[i].ID]
	}
	if dir != "" {
		done := make(chan struct{}, 1)
		go func() { write(dir, audit); done <- struct{}{} }()
		select {
		case <-done:
		case <-time.After(100 * time.Millisecond):
		}
	}
	return selected
}

func utilityPromptSelectionWork(ctx context.Context, p string, hits, derived []search.Hit, seen map[string]bool, floor float64, capture func(utilityAudit)) []search.Hit {
	fallback := promptSelection(hits, derived, seen, floor)
	if utilityProvider() == "" {
		return fallback
	}
	all := append(append([]search.Hit{}, hits...), derived...)
	if len(all) == 0 {
		return fallback
	}
	c, configErr := utilityConfiguration()
	hash := sha256.Sum256([]byte(p))
	audit := utilityAudit{Time: time.Now().UTC(), Model: c.Model, Threshold: c.Threshold, Status: "ok", QueryHash: hex.EncodeToString(hash[:])}
	for _, h := range all {
		audit.Candidates = append(audit.Candidates, utilityAuditHit{ID: h.Path})
	}
	start := time.Now()
	selected := fallback
	defer func() {
		audit.ElapsedMS = float64(time.Since(start).Microseconds()) / 1000
		capture(audit)
	}()
	if configErr != nil {
		audit.Status = configErr.Error()
		return selected
	}
	key, err := providerKey(c)
	if err != nil {
		audit.Status = err.Error()
		return selected
	}
	callCtx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	texts := []string{p}
	for _, h := range all {
		texts = append(texts, h.Name+"\n"+h.Description)
	}
	clean, err := digest.RedactAllContext(callCtx, texts)
	if err != nil {
		audit.Status = "redaction_failed"
		return selected
	}
	audit.Task = clean[0]
	for i, memory := range clean[1:] {
		audit.Candidates[i].Summary = memory
	}
	scores, tokens, err := scoreUtility(callCtx, c, key, clean[0], clean[1:])
	if callCtx.Err() != nil {
		audit.Status = "request_failed"
		return selected
	}
	if err != nil {
		audit.Status = err.Error()
		return selected
	}
	audit.InputTokens = tokens
	values := make(map[string]float64, len(all))
	for i, h := range all {
		score := scores[i]
		values[h.Path] = score
		audit.Candidates[i].Score = &score
	}
	selected = utilityRank(hits, derived, seen, values, c.Threshold)
	return selected
}

func requestUtility(ctx context.Context, key string, body []byte, count int) (utilityResponse, error) {
	return requestSystemOne(ctx, utilityConfig{Provider: providerJev, Model: utilityModel, Endpoint: utilityEndpoint}, key, body, count)
}

// requestSystemOne posts a /v1/systemone request. Jev's model is pinned and
// checked; another System One server may report its own model name.
func requestSystemOne(ctx context.Context, c utilityConfig, key string, body []byte, count int) (utilityResponse, error) {
	var result utilityResponse
	raw, err := postJSON(ctx, c.Endpoint, key, body)
	if err != nil {
		return result, err
	}
	if json.Unmarshal(raw, &result) != nil {
		return result, errors.New("response_invalid")
	}
	if (c.Provider == providerJev && result.Model != utilityModel) || len(result.Answers) != count {
		return result, errors.New("response_mismatch")
	}
	for i := 0; i < count; i++ {
		a, ok := result.Answers[fmt.Sprintf("candidate_%d", i)]
		if !ok || a.Type != "noul" || a.Score == nil || math.IsNaN(*a.Score) || math.IsInf(*a.Score, 0) || *a.Score < 0 || *a.Score > 1 {
			return result, errors.New("response_invalid")
		}
	}
	return result, nil
}

// postJSON: one POST, no redirects or retries, a 1 MiB response cap. The key,
// when there is one, goes only to the configured endpoint as a bearer token.
func postJSON(ctx context.Context, endpoint, key string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("request_invalid")
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("request_failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http_%d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return nil, errors.New("response_invalid")
	}
	return raw, nil
}

func utilityRank(hits, derived []search.Hit, seen map[string]bool, values map[string]float64, threshold float64) []search.Hit {
	ranked := append([]search.Hit{}, hits...)
	sort.SliceStable(ranked, func(i, j int) bool {
		if values[ranked[i].Path] == values[ranked[j].Path] {
			return ranked[i].Path < ranked[j].Path
		}
		return values[ranked[i].Path] > values[ranked[j].Path]
	})
	var strong []search.Hit
	for _, h := range ranked {
		if !isDerived(h) && values[h.Path] >= threshold && len(strong) < promptLimit {
			strong = append(strong, h)
		}
	}
	if len(derived) > 0 && isDerived(derived[0]) && values[derived[0].Path] >= threshold {
		strong = append(strong, derived[0])
	}
	var selected []search.Hit
	for _, h := range strong {
		if !seen[h.Name] {
			selected = append(selected, h)
		}
	}
	return selected
}

// Local, private, bounded daily receipts for reviewing false positives later.
// Logging failure never changes selection. No response body or credential logs.
func appendUtilityAudit(dir string, event utilityAudit) {
	if dir == "" || os.MkdirAll(dir, 0700) != nil {
		return
	}
	root, err := platform.OpenPrivateDirectory(dir)
	if err != nil {
		return
	}
	defer root.Close()
	now := time.Now().UTC()
	file := "utility-" + now.Format("2006-01-02") + ".jsonl"
	f, err := platform.OpenPrivateAppend(root, file)
	if err != nil {
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() >= 10<<20 {
		return
	}
	entries, _ := root.ReadDir(-1)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "utility-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		date, err := time.Parse("2006-01-02", strings.TrimSuffix(strings.TrimPrefix(name, "utility-"), ".jsonl"))
		if err == nil && now.Sub(date) > 8*24*time.Hour {
			_ = platform.RemovePrivateEntry(root, name)
		}
	}
	body, err := json.Marshal(event)
	if err == nil {
		_, _ = f.Write(append(body, '\n'))
	}
}
