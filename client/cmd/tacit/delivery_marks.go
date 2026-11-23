package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Marks (SPEC.md, "Marks"): the Stop hook (hooks_stop.go) asks the agent,
// once a turn that was given facts, to say of each one whether it was used,
// not relevant or wrong, in one call (tacit_mark, or `tacit mark`) naming
// the ask's log id. A mark is a log entry of its own, and each verdict also
// goes to the outcomes (delivery_outcomes.go), which move the line's
// delivery in that scope.

// logIDRx is a log entry's id (logID).
var logIDRx = regexp.MustCompile(`^[0-9a-f]{16}$`)

// The verdicts a mark takes.
const (
	verdictUsed        = "used"
	verdictNotRelevant = "not_relevant"
	verdictWrong       = "wrong"
)

// markVerdict is one fact's verdict in a mark.
type markVerdict struct {
	Name    string `json:"name" jsonschema:"the fact's name as the ask listed it"`
	Verdict string `json:"verdict" jsonschema:"used, not_relevant or wrong"`
}

// normVerdict reads a verdict, in the words an agent may use for it.
func normVerdict(v string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(v, "-", "_"), " ", "_"))) {
	case "used", "use", "helped", "relevant", "yes":
		return verdictUsed, true
	case "not_relevant", "irrelevant", "not_used", "unused", "not_helped", "no":
		return verdictNotRelevant, true
	case "wrong", "incorrect", "stale", "false":
		return verdictWrong, true
	}
	return "", false
}

// logMarkStep is a mark entry's marks. Helped is the used lines, NotHelped
// the not relevant ones (the names the first marks had), Wrong the wrong
// ones.
type logMarkStep struct {
	Block     string   `json:"block"`
	Helped    []string `json:"helped,omitempty"`
	NotHelped []string `json:"not_helped,omitempty"`
	Wrong     []string `json:"wrong,omitempty"`
}

// markCap bounds the names one mark keeps (the log's candidate cap).
const markCap = logCandCap

// ownerProv are the provenances of the owner's own lines (OwnerLine), which
// no agent grades in a prompt block's ask; a Stop ask lists them too.
var ownerProv = map[string]bool{"rule": true, "owner": true, "statement": true, "words": true, "confirmed": true}

// markedBlock finds, in the window a mark can follow its block (the UTC
// day of the mark and the one before), the block of this id that asked for
// a mark. A read error answers none: the mark is refused rather than taken
// unchecked.
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

// askedLines are the names a mark of this block may grade: the lines it
// printed; a prompt block's, the agents' only.
func askedLines(block *logEntry) map[string]*logCand {
	lines := map[string]*logCand{}
	for i, c := range block.Cands {
		if c.Delivered && (block.Hook == "stop" || !ownerProv[c.Prov]) {
			lines[c.Name] = &block.Cands[i]
		}
	}
	return lines
}

// logMark records a mark of a block that asked for one: each named line
// the block printed with its verdict, a name given two verdicts in none;
// one mark a block. It reports why nothing was recorded, and what it left
// out (names the block did not print, verdicts it does not know).
func logMark(block string, marks []markVerdict) (skipped []string, err error) {
	if !deliveryLogOn() {
		return nil, errors.New("the delivery log is off; nothing was recorded")
	}
	now := time.Now()
	asked, err := markedBlock(block, now)
	switch {
	case err != nil:
		return nil, fmt.Errorf("the delivery log could not be read: %w", err)
	case asked == nil:
		return nil, errors.New("no tacit ask has this id")
	}
	lines := askedLines(asked)
	verdict := map[string]string{}
	var order []string
	both := map[string]bool{}
	for _, m := range marks {
		name := strings.TrimSpace(m.Name)
		v, ok := normVerdict(m.Verdict)
		if !ok || lines[name] == nil {
			skipped = append(skipped, name)
			continue
		}
		if old, seen := verdict[name]; seen {
			if old != v {
				both[name] = true
			}
			continue
		}
		verdict[name] = v
		order = append(order, name)
	}
	m := &logMarkStep{Block: block}
	var outs []outcomeLine
	for _, name := range order {
		if both[name] || len(m.Helped)+len(m.NotHelped)+len(m.Wrong) >= markCap {
			continue
		}
		switch verdict[name] {
		case verdictUsed:
			m.Helped = append(m.Helped, name)
		case verdictNotRelevant:
			m.NotHelped = append(m.NotHelped, name)
		case verdictWrong:
			m.Wrong = append(m.Wrong, name)
		}
		in, hook := lines[name].In, lines[name].Via
		if asked.Hook != "stop" {
			in, hook = outcomeScope(asked.Repos), asked.Hook
		}
		outs = append(outs, outcomeLine{TS: now.UTC().Format(time.RFC3339Nano), In: in, Fact: name, Verdict: verdict[name], Src: "mark", Block: block, Hook: hook})
	}
	if len(outs) == 0 {
		return skipped, errors.New("name at least one of the facts the ask listed, with used, not_relevant or wrong")
	}
	switch ok, err := claimMark(block, now); {
	case err != nil:
		return skipped, fmt.Errorf("the mark could not be claimed: %w", err)
	case !ok:
		return skipped, errors.New("this ask is marked already")
	}
	e := &logEntry{V: deliveryLogVersion, TS: now.UTC().Format(time.RFC3339Nano), Hook: "mark", Session: asked.Session, Mark: m}
	e.ID = logID(e)
	if err := appendLogEntry(e, now); err != nil {
		os.Remove(filepath.Join(deliveryLogDir(), "marks", block)) // a retry may claim it again
		return skipped, fmt.Errorf("the mark was not written: %w", err)
	}
	recordOutcomes(outs)
	return skipped, nil
}

// markAnswer is what a mark call says back.
func markAnswer(skipped []string) string {
	if len(skipped) == 0 {
		return "noted"
	}
	return "noted; left out (not in the ask, or no verdict of used, not_relevant, wrong): " + strings.Join(skipped, ", ")
}

// addMarkTool registers tacit_mark on the store's MCP server.
func addMarkTool(server *mcp.Server) {
	type markArgs struct {
		Block string        `json:"block" jsonschema:"the ask id tacit's message named"`
		Marks []markVerdict `json:"marks" jsonschema:"one entry for each fact the ask listed: its name and used, not_relevant or wrong"`
	}
	mcp.AddTool(server, &mcp.Tool{
		Name:        "tacit_mark",
		Description: "When tacit asks for a mark: in this one call, say for each fact it listed whether you used it, it was not relevant, or it was wrong.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, a markArgs) (*mcp.CallToolResult, any, error) {
		fail := func(msg string) (*mcp.CallToolResult, any, error) {
			res := text(msg)
			res.IsError = true
			return res, nil, nil
		}
		if !logIDRx.MatchString(a.Block) {
			return fail("give the ask id tacit's message named")
		}
		skipped, err := logMark(a.Block, a.Marks)
		if err != nil {
			return fail(err.Error())
		}
		return text(markAnswer(skipped)), nil, nil
	})
}

// runMark is `tacit mark <id> <name>=<verdict>...`: the same mark from a
// shell, for a harness without tacit's MCP server.
func runMark(args []string) int {
	if len(args) < 2 || !logIDRx.MatchString(args[0]) {
		fmt.Fprintln(os.Stderr, "usage: tacit mark <id> <fact>=used|not_relevant|wrong ...")
		return 2
	}
	var marks []markVerdict
	for _, a := range args[1:] {
		name, v, ok := strings.Cut(a, "=")
		if !ok {
			fmt.Fprintln(os.Stderr, "tacit mark: give each fact as <name>=<verdict>:", a)
			return 2
		}
		marks = append(marks, markVerdict{Name: name, Verdict: v})
	}
	skipped, err := logMark(args[0], marks)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tacit mark:", err)
		return 1
	}
	fmt.Println(markAnswer(skipped))
	return 0
}
