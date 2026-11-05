package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/search"
	"github.com/agent-sh/tacit/client/internal/store"
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
	dlog.PMark = &p
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

// markCap bounds the names one mark keeps (the log's candidate cap).
const markCap = logCandCap

// askedBlock reports whether the log holds a block of this id that asked
// for a self-mark, in the window a mark can follow its block (today and
// the day before).
func askedBlock(id string, now time.Time) bool {
	for _, e := range readLogDays(now.AddDate(0, 0, -1)) {
		if e.ID == id && e.MarkAsked {
			return true
		}
	}
	return false
}

// readLogDays is the log's entries from since's day on.
func readLogDays(since time.Time) []logEntry {
	dir := deliveryLogDir()
	entries, _ := os.ReadDir(dir)
	from := since.UTC().Format("2006-01-02")
	var out []logEntry
	for _, x := range entries {
		m := logDay.FindStringSubmatch(x.Name())
		if m == nil || m[1] < from {
			continue
		}
		f, err := os.Open(filepath.Join(dir, x.Name()))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<26)
		for sc.Scan() {
			var e logEntry
			if json.Unmarshal(sc.Bytes(), &e) == nil {
				out = append(out, e)
			}
		}
		f.Close()
	}
	return out
}

// markNames is xs less what is no fact the store holds (the index's
// facts, as nav steps keep them), less doubles and what drop holds, at
// most markCap.
func markNames(ix *search.Index, xs []string, drop map[string]bool) []string {
	var out []string
	seen := map[string]bool{}
	ctx, cancel := context.WithTimeout(context.Background(), logVocabularyBudget)
	defer cancel()
	for _, x := range xs {
		x = strings.TrimSpace(x)
		if seen[x] || drop[x] || !fact.IsSlug(x) || ix == nil || !ix.HasFact(ctx, x) || len(out) >= markCap {
			continue
		}
		seen[x] = true
		out = append(out, x)
	}
	return out
}

// logMark records an agent's self-mark of a block that asked for one: the
// fact names the store holds, a name in both lists in neither. It reports
// why nothing was recorded.
func logMark(s *store.Store, block string, helped, not []string) error {
	if !deliveryLogOn() {
		return errors.New("the delivery log is off; nothing was recorded")
	}
	if !askedBlock(block, time.Now()) {
		return errors.New("no tacit block asked for a mark with this id")
	}
	var ix *search.Index
	ctx, cancel := context.WithTimeout(context.Background(), logVocabularyBudget)
	defer cancel()
	if s != nil {
		if i, err := openHookIndex(ctx, s); err == nil {
			ix = i
			defer ix.Close()
		}
	}
	both := map[string]bool{}
	for _, h := range helped {
		for _, n := range not {
			if strings.TrimSpace(h) == strings.TrimSpace(n) {
				both[strings.TrimSpace(h)] = true
			}
		}
	}
	m := &logMarkStep{Block: block, Helped: markNames(ix, helped, both), NotHelped: markNames(ix, not, both)}
	if len(m.Helped)+len(m.NotHelped) == 0 {
		return errors.New("name at least one of the block's facts")
	}
	e := &logEntry{V: deliveryLogVersion, TS: time.Now().UTC().Format(time.RFC3339Nano), Hook: "mark", Mark: m}
	e.ID = logID(e)
	writeBounded(e)
	return nil
}

// addMarkTool registers tacit_mark on the store's MCP server.
func addMarkTool(server *mcp.Server, s *store.Store) {
	type markArgs struct {
		Block     string   `json:"block" jsonschema:"the block id tacit's line named"`
		Helped    []string `json:"helped,omitempty" jsonschema:"names of the block's facts that helped with the request"`
		NotHelped []string `json:"not_helped,omitempty" jsonschema:"names of the block's facts that did not help"`
	}
	mcp.AddTool(server, &mcp.Tool{
		Name:        "tacit_mark",
		Description: "When a tacit block asked for it: say which of its lines helped with the request and which did not. Optional; it changes nothing the session sees.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a markArgs) (*mcp.CallToolResult, any, error) {
		fail := func(msg string) (*mcp.CallToolResult, any, error) {
			res := text(msg)
			res.IsError = true
			return res, nil, nil
		}
		if !logIDRx.MatchString(a.Block) {
			return fail("give the block id tacit's line named")
		}
		if err := logMark(s, a.Block, a.Helped, a.NotHelped); err != nil {
			return fail(err.Error())
		}
		return text("noted"), nil, nil
	})
}
