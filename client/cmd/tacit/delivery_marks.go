package main

import (
	"bufio"
	"bytes"
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
	dlog.PMark = &p
	if p <= 0 || markDraw() >= p {
		return ""
	}
	dlog.MarkAsked = true
	return fmt.Sprintf("# tacit: when this request is done, say which of these lines helped with `mcp__tacit__tacit_mark` (block %s); it is optional and nothing waits for it\n", dlog.ID)
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

// ownerProv are the provenances of the owner's own lines (OwnerLine), which
// no agent grades.
var ownerProv = map[string]bool{"rule": true, "owner": true, "statement": true, "words": true, "confirmed": true}

// markedBlock finds, in the window a mark can follow its block (the UTC
// day of the mark and the one before), the block of this id that asked for
// a self-mark. A read error answers none: the mark is refused rather than
// taken unchecked.
func markedBlock(id string, now time.Time) (asked *logEntry, err error) {
	needle := []byte(`"` + id + `"`)
	for _, day := range []time.Time{now, now.AddDate(0, 0, -1)} {
		path := filepath.Join(deliveryLogDir(), day.UTC().Format("2006-01-02")+".jsonl")
		if fi, err := os.Lstat(path); err != nil || !fi.Mode().IsRegular() {
			continue // only this log's own regular files
		}
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<26)
		for sc.Scan() {
			if !bytes.Contains(sc.Bytes(), needle) {
				continue // most lines are other blocks
			}
			var e logEntry
			if json.Unmarshal(sc.Bytes(), &e) != nil {
				continue
			}
			if e.ID == id && e.MarkAsked {
				asked = &e
			}
		}
		serr := sc.Err()
		f.Close()
		if serr != nil {
			return nil, serr
		}
	}
	return asked, nil
}

// claimMark claims a block for its one mark: an exclusive create of
// delivery/marks/<id>, which two calls at once cannot both make. Claims
// older than the window are removed as new ones are made.
func claimMark(id string, now time.Time) (bool, error) {
	dir := filepath.Join(deliveryLogDir(), "marks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	f, err := os.OpenFile(filepath.Join(dir, id), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if os.IsExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	f.Close()
	if entries, err := os.ReadDir(dir); err == nil {
		for _, x := range entries {
			if fi, err := x.Info(); err == nil && now.Sub(fi.ModTime()) > 72*time.Hour {
				os.Remove(filepath.Join(dir, x.Name()))
			}
		}
	}
	return true, nil
}

// markNames is xs less what the block did not print as an agent's line,
// less doubles and what drop holds, at most markCap.
func markNames(block *logEntry, xs []string, drop map[string]bool) []string {
	lines := map[string]bool{}
	for _, c := range block.Cands {
		if c.Delivered && !ownerProv[c.Prov] {
			lines[c.Name] = true
		}
	}
	var out []string
	seen := map[string]bool{}
	for _, x := range xs {
		if len(out) >= markCap {
			break
		}
		x = strings.TrimSpace(x)
		if seen[x] || drop[x] || !lines[x] {
			continue
		}
		seen[x] = true
		out = append(out, x)
	}
	return out
}

// logMark records an agent's self-mark of a block that asked for one: the
// agent's lines the block printed, a name in both lists in neither; one
// mark a block. It reports why nothing was recorded.
func logMark(block string, helped, not []string) error {
	if !deliveryLogOn() {
		return errors.New("the delivery log is off; nothing was recorded")
	}
	now := time.Now()
	asked, err := markedBlock(block, now)
	switch {
	case err != nil:
		return fmt.Errorf("the delivery log could not be read: %w", err)
	case asked == nil:
		return errors.New("no tacit block asked for a mark with this id")
	}
	both := map[string]bool{}
	for _, h := range helped {
		for _, n := range not {
			if strings.TrimSpace(h) == strings.TrimSpace(n) {
				both[strings.TrimSpace(h)] = true
			}
		}
	}
	m := &logMarkStep{Block: block, Helped: markNames(asked, helped, both), NotHelped: markNames(asked, not, both)}
	if len(m.Helped)+len(m.NotHelped) == 0 {
		return errors.New("name at least one of the agent's lines the block printed")
	}
	switch ok, err := claimMark(block, now); {
	case err != nil:
		return fmt.Errorf("the mark could not be claimed: %w", err)
	case !ok:
		return errors.New("this block is marked already")
	}
	e := &logEntry{V: deliveryLogVersion, TS: now.UTC().Format(time.RFC3339Nano), Hook: "mark", Mark: m}
	e.ID = logID(e)
	if err := appendLogEntry(e, now); err != nil {
		return fmt.Errorf("the mark was not written: %w", err)
	}
	return nil
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
		fail := func(msg string) (*mcp.CallToolResult, any, error) {
			res := text(msg)
			res.IsError = true
			return res, nil, nil
		}
		if !logIDRx.MatchString(a.Block) {
			return fail("give the block id tacit's line named")
		}
		if err := logMark(a.Block, a.Helped, a.NotHelped); err != nil {
			return fail(err.Error())
		}
		return text("noted"), nil, nil
	})
}
