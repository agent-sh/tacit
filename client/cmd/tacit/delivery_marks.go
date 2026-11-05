package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"regexp"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/search"
)

// Sampled self-marks (SPEC.md, "Self-marks"): on a small share of the
// prompt blocks that print an agent's notes, the block asks the agent to say
// later which lines helped, through tacit_mark, naming the block's log id.
// The ask never blocks: nothing waits for the answer. The draw's
// probability is logged with the block, and a mark is a log entry of its
// own. A mark moves no score until it is calibrated against the hand
// grades and the hold-outs.

// markRate is the share of eligible prompt blocks that ask (about 1 in
// 20): enough asks to calibrate within weeks of a working machine's
// traffic, few enough that the ask is rare in any one session.
// TACIT_DELIVERY_MARK_RATE sets it, 0 for none.
const markRate = 0.05

// markDraw is the sampling draw; tests replace it.
var markDraw = func() float64 { return rand.Float64() }

func deliveryMarkRate() float64 { return deliveryFloat("MARK_RATE", markRate) }

// markAsk is the line a sampled block ends with, or "": only a prompt
// block, in a harness with tacit's MCP server (tacit_mark is a tool there),
// that prints at least one agent's note (an owner's rule is not graded by an
// agent). The draw and its probability go in the block's log entry.
func markAsk(hits []search.Hit) string {
	if dlog == nil || !hookMCP || !hasAgentLine(hits) {
		return ""
	}
	p := deliveryMarkRate()
	dlog.PMark = p
	if p <= 0 || markDraw() >= p {
		return ""
	}
	dlog.MarkAsked = true
	return fmt.Sprintf("# tacit: when this request is done, say which of these lines helped with `tacit_mark` (block %s); it is optional and nothing waits for it\n", dlog.ID)
}

// logIDRx is a log entry's id (logID).
var logIDRx = regexp.MustCompile(`^[0-9a-f]{16}$`)

// logMarkStep is a mark entry's marks.
type logMarkStep struct {
	Block     string   `json:"block"`
	Helped    []string `json:"helped,omitempty"`
	NotHelped []string `json:"not_helped,omitempty"`
}

func hasAgentLine(hits []search.Hit) bool {
	for _, h := range hits {
		if !h.OwnerLine() {
			return true
		}
	}
	return false
}

// logMark records an agent's self-mark as a mark entry of the log.
func logMark(block string, helped, not []string) {
	if !deliveryLogOn() {
		return
	}
	keep := func(xs []string) []string {
		var out []string
		for _, x := range xs {
			if x = strings.TrimSpace(x); fact.IsSlug(x) {
				out = append(out, x)
			}
		}
		return out
	}
	e := &logEntry{V: deliveryLogVersion, TS: time.Now().UTC().Format(time.RFC3339Nano), Hook: "mark",
		Mark: &logMarkStep{Block: block, Helped: keep(helped), NotHelped: keep(not)}}
	e.ID = logID(e)
	writeBounded(e)
}

// addMarkTool registers tacit_mark on the store's MCP server.
func addMarkTool(server *mcp.Server) {
	type markArgs struct {
		Block     string   `json:"block" jsonschema:"the block id tacit's line named"`
		Helped    []string `json:"helped,omitempty" jsonschema:"names of the block's facts that helped with the request"`
		NotHelped []string `json:"not_helped,omitempty" jsonschema:"names of the block's facts that did not help"`
	}
	mcp.AddTool(server, &mcp.Tool{
		Name:        "tacit_mark",
		Description: "When a tacit block asked for it: say which of its lines helped with the request and which did not. Optional; it changes nothing the session sees.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a markArgs) (*mcp.CallToolResult, any, error) {
		if !logIDRx.MatchString(a.Block) {
			return text("give the block id tacit's line named"), nil, nil
		}
		logMark(a.Block, a.Helped, a.NotHelped)
		return text("noted"), nil, nil
	})
}
