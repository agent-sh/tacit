package modelrun

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

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
	var pendingErr error
	decoder := json.NewDecoder(bytes.NewReader(data))
	for {
		var event map[string]any
		err := decoder.Decode(&event)
		if err == io.EOF {
			break
		}
		if err != nil {
			return result, fmt.Errorf("invalid JSON stream: %w", err)
		}
		switch name {
		case "claude":
			if bad, _ := event["is_error"].(bool); bad {
				return result, fmt.Errorf("model reported an error: %s %s %v", str(event, "result"), str(event, "subtype"), event["errors"])
			}
			if text := str(event, "result"); text != "" {
				result.Text = text
				done = true
			}
			if text := str(event, "response"); text != "" {
				result.Text = text
				done = true
			}
			result.CostUSD = num(event, "total_cost_usd")
			_, result.CostReported = event["total_cost_usd"]
			result.SessionID = str(event, "session_id")
			u := obj(event, "usage")
			result.InputTokens = int(num(u, "input_tokens") + num(u, "cache_creation_input_tokens") + num(u, "cache_read_input_tokens"))
		case "grok":
			result.CostUSD = num(event, "total_cost_usd")
			_, result.CostReported = event["total_cost_usd"]
			if str(event, "type") == "error" {
				return result, fmt.Errorf("model reported an error: %s", str(event, "message"))
			}
			if reason := str(event, "stopReason"); reason != "end_turn" {
				return result, fmt.Errorf("model did not complete: %s", reason)
			}
			result.Text = str(event, "text")
			result.SessionID = str(event, "sessionId")
			u := obj(event, "usage")
			result.InputTokens = int(num(u, "input_tokens") + num(u, "cache_read_input_tokens") + num(u, "cache_creation_input_tokens"))
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
				u := obj(event, "usage")
				result.InputTokens = int(num(u, "input_tokens"))
			case "turn.failed", "error":
				return result, errors.New("model reported a failed turn")
			}
		case "muse":
			p := obj(event, "payload")
			if str(event, "payload_type") == "run.terminal.completed" {
				result.Text = str(p, "text")
				done = true
			}
			if str(p, "kind") == "run_terminal" && str(p, "terminal") != "completed" {
				return result, errors.New("model run did not complete")
			}
		case "agy":
			if str(event, "event") == "result" {
				p := obj(event, "result")
				if str(p, "status") != "SUCCESS" {
					return result, fmt.Errorf("model ended with %s", str(p, "status"))
				}
				result.Text = str(p, "response")
				result.SessionID = str(p, "conversation_id")
				done = true
				u := obj(p, "usage")
				result.InputTokens = int(num(u, "input_tokens") + num(u, "cache_read_tokens"))
			}
		case "pi":
			if str(event, "type") == "message_end" {
				m := obj(event, "message")
				if str(m, "role") == "assistant" {
					if s := str(m, "stopReason"); s == "error" || s == "aborted" {
						pendingErr = fmt.Errorf("model did not complete: %s", str(m, "errorMessage"))
					}
					if str(m, "stopReason") != "error" && str(m, "stopReason") != "aborted" {
						pendingErr = nil
					}
					result.Text = content(m["content"])
					u := obj(m, "usage")
					result.InputTokens = int(num(u, "input") + num(u, "cacheRead") + num(u, "cacheWrite"))
					result.CostUSD += num(obj(u, "cost"), "total")
					_, result.CostReported = obj(u, "cost")["total"]
				}
			}
			if str(event, "type") == "agent_end" {
				retry, _ := event["willRetry"].(bool)
				done = !retry
			}
		case "hermes":
			switch str(event, "type") {
			case "result":
				if num(event, "exit_code") != 0 {
					return result, errors.New("model reported an error")
				}
				if bad, _ := event["is_error"].(bool); bad {
					return result, errors.New("model reported an error")
				}
				result.Text = str(event, "result")
				if result.Text == "" {
					result.Text = str(event, "text")
				}
				done = true
				result.CostUSD = num(event, "total_cost_usd")
				_, result.CostReported = event["total_cost_usd"]
				u := obj(event, "tokens")
				result.InputTokens = int(num(u, "input") + num(u, "cache_read") + num(u, "cache_write"))
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
		return result, errors.New("missing completed model result")
	}
	return result, nil
}
