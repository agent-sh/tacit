package modelrun

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

var ErrInvalidResult = errors.New("invalid model result")

func str(m map[string]any, k string) string         { s, _ := m[k].(string); return s }
func obj(m map[string]any, k string) map[string]any { v, _ := m[k].(map[string]any); return v }
func num(m map[string]any, k string) float64        { n, _ := m[k].(float64); return n }
func content(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	var out strings.Builder
	if xs, ok := v.([]any); ok {
		for _, x := range xs {
			if m, ok := x.(map[string]any); ok && str(m, "type") == "text" {
				out.WriteString(str(m, "text"))
			}
		}
	}
	return out.String()
}

// parse reads complete result events, never partial reasoning or tool output.
func parse(name string, data []byte) (Result, error) {
	var result Result
	done := false
	piCalls := 0
	var pendingErr error
	decoder := json.NewDecoder(bytes.NewReader(data))
	for {
		var event map[string]any
		err := decoder.Decode(&event)
		if err == io.EOF {
			break
		}
		if err != nil {
			return result, fmt.Errorf("%w: invalid JSON stream: %v", ErrInvalidResult, err)
		}
		switch name {
		case "claude":
			if text := str(event, "result"); text != "" {
				result.Text = text
				done = true
			}
			if text := str(event, "response"); text != "" {
				result.Text = text
				done = true
			}
			if value, ok := event["structured_output"]; ok && value != nil {
				encoded, err := json.Marshal(value)
				if err != nil {
					return result, fmt.Errorf("%w: structured output: %v", ErrInvalidResult, err)
				}
				result.Text, result.Structured, done = string(encoded), true, true
			}
			result.CostUSD, result.CostReported = costValue(event, "total_cost_usd")
			result.SessionID = str(event, "session_id")
			u := obj(event, "usage")
			result.InputTokens = int(num(u, "input_tokens") + num(u, "cache_creation_input_tokens") + num(u, "cache_read_input_tokens"))
			if bad, _ := event["is_error"].(bool); bad {
				return result, eventFailure(event, fmt.Sprintf("%s %s %v", str(event, "result"), str(event, "subtype"), event["errors"]))
			}
		case "grok":
			result.CostUSD, result.CostReported = costValue(event, "total_cost_usd")
			u := obj(event, "usage")
			result.InputTokens = int(num(u, "input_tokens") + num(u, "cache_read_input_tokens") + num(u, "cache_creation_input_tokens"))
			if str(event, "type") == "error" {
				return result, eventFailure(event, str(event, "message"))
			}
			if reason := str(event, "stopReason"); reason != "end_turn" {
				return result, eventFailure(event, "model did not complete: "+reason)
			}
			result.Text = str(event, "text")
			result.SessionID = str(event, "sessionId")
			done = true
		case "codex":
			switch str(event, "type") {
			case "thread.started":
				result.SessionID = str(event, "thread_id")
			case "item.completed":
				item := obj(event, "item")
				if str(item, "type") == "agent_message" {
					result.Text = str(item, "text")
				}
			case "turn.completed":
				done = true
				pendingErr = nil
				u := obj(event, "usage")
				result.InputTokens = int(num(u, "input_tokens"))
			case "turn.failed":
				return result, eventFailure(event, strings.TrimSpace(str(event, "message")+" "+str(obj(event, "error"), "message")))
			case "error":
				// A notice, not the turn's end: Codex prints a stream retry
				// here ("Reconnecting... 1/5 (...)") and goes on. The turn's
				// own end decides; a run whose turn never ends fails with the
				// last notice.
				if !done {
					pendingErr = eventFailure(event, strings.TrimSpace(str(event, "message")+" "+str(obj(event, "error"), "message")))
				}
			}
		case "muse":
			p := obj(event, "payload")
			if str(event, "payload_type") == "run.terminal.completed" {
				result.Text = str(p, "text")
				done = true
			}
			if str(p, "kind") == "run_terminal" && str(p, "terminal") != "completed" {
				return result, eventFailure(p, "model run did not complete")
			}
		case "agy":
			if str(event, "event") == "result" {
				p := obj(event, "result")
				u := obj(p, "usage")
				result.InputTokens = int(num(u, "input_tokens") + num(u, "cache_read_tokens"))
				if str(p, "status") != "SUCCESS" {
					return result, eventFailure(p, "model ended with "+str(p, "status"))
				}
				result.Text = str(p, "response")
				result.SessionID = str(p, "conversation_id")
				done = true
			}
		case "pi":
			if str(event, "type") == "message_end" {
				m := obj(event, "message")
				if str(m, "role") == "assistant" {
					if s := str(m, "stopReason"); s == "error" || s == "aborted" {
						pendingErr = eventFailure(m, str(m, "errorMessage"))
					}
					if str(m, "stopReason") != "error" && str(m, "stopReason") != "aborted" {
						pendingErr = nil
					}
					result.Text = content(m["content"])
					u := obj(m, "usage")
					result.InputTokens += int(num(u, "input") + num(u, "cacheRead") + num(u, "cacheWrite"))
					cost, reported := costValue(obj(u, "cost"), "total")
					result.CostUSD += cost
					result.CostReported = (piCalls == 0 || result.CostReported) && reported
					piCalls++
				}
			}
			if str(event, "type") == "agent_end" {
				retry, _ := event["willRetry"].(bool)
				done = !retry
			}
		case "hermes":
			switch str(event, "type") {
			case "result":
				result.CostUSD, result.CostReported = costValue(event, "total_cost_usd")
				u := obj(event, "tokens")
				result.InputTokens = int(num(u, "input") + num(u, "cache_read") + num(u, "cache_write"))
				if num(event, "exit_code") != 0 {
					return result, eventFailure(event, str(event, "result"))
				}
				if bad, _ := event["is_error"].(bool); bad {
					return result, eventFailure(event, str(event, "result"))
				}
				result.Text = str(event, "result")
				if result.Text == "" {
					result.Text = str(event, "text")
				}
				done = true
			case "assistant":
				m := obj(event, "message")
				result.Text = content(m["content"])
			}
		}
	}
	if pendingErr != nil {
		return result, pendingErr
	}
	if !done || strings.TrimSpace(result.Text) == "" {
		return result, fmt.Errorf("%w: missing completed model result", ErrInvalidResult)
	}
	return result, nil
}

func costValue(values map[string]any, key string) (float64, bool) {
	cost, reported := values[key].(float64)
	if !reported || cost < 0 {
		return 0, false
	}
	return cost, true
}
