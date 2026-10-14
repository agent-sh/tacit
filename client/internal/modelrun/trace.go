package modelrun

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// CallRecord contains accounting and route provenance, never prompts, answers
// or provider error text. A nil cost means unknown; zero is a reported zero.
type CallRecord struct {
	Role        string   `json:"role"`
	Route       Route    `json:"route"`
	Fallback    bool     `json:"fallback,omitempty"`
	CostUSD     *float64 `json:"cost_usd"`
	InputTokens int      `json:"input_tokens"`
	Failure     string   `json:"failure,omitempty"`
}
type Trace struct {
	Calls            []CallRecord `json:"calls"`
	UnknownCostCalls int          `json:"unknown_cost_calls"`
	mu               sync.Mutex
}
type traceKey struct{}
type fallbackKey struct{}

func WithTrace(ctx context.Context, trace *Trace) context.Context {
	return context.WithValue(ctx, traceKey{}, trace)
}
func FallbackContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, fallbackKey{}, true)
}
func Record(ctx context.Context, role string, result Result, err error) {
	t, _ := ctx.Value(traceKey{}).(*Trace)
	if t == nil {
		return
	}
	record := CallRecord{Role: role, Route: result.Route, InputTokens: result.InputTokens}
	record.Fallback, _ = ctx.Value(fallbackKey{}).(bool)
	if result.CostReported {
		cost := result.CostUSD
		record.CostUSD = &cost
	}
	if err != nil {
		record.Failure = "runner"
		var failure *Failure
		if errors.As(err, &failure) {
			record.Failure = failure.Kind
		}
		if errors.Is(err, ErrInvalidResult) {
			record.Failure = "invalid_result"
		}
		if errors.Is(err, context.DeadlineExceeded) {
			record.Failure = "timeout"
		}
		if errors.Is(err, context.Canceled) {
			record.Failure = "canceled"
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Calls = append(t.Calls, record)
	if record.CostUSD == nil {
		t.UnknownCostCalls++
	}
}
func (t *Trace) CostSummary(reported float64) string {
	if t != nil && t.UnknownCostCalls > 0 {
		return fmt.Sprintf("reported $%.3f; cost unknown for %d call(s)", reported, t.UnknownCostCalls)
	}
	return fmt.Sprintf("$%.3f", reported)
}
