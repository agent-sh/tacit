package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/platform"
	"github.com/agent-sh/tacit/client/internal/search"
)

// The delivery log (SPEC.md, "Delivery log"): for each moment a hook chose
// what to deliver, every candidate with the features it was chosen on,
// whether it was shown and the probability the policy had of showing it,
// and whether the block showed nothing, one JSON object per block in
// <state>/delivery/<day>.jsonl. It is this machine's alone: mode 0600 in a
// 0700 directory, never in the store, never sent. Outcome signals join it
// later, and the propensities let a new ranker be estimated on past moments
// (eval/DELIVERY.md, "Scoring each delivery"). Writing it is one append per
// block; any failure leaves the hook as it was.

const (
	// deliveryLogVersion is the entry's schema; a reader skips another.
	deliveryLogVersion = 1
	// deliveryLogDays: a day's file older than this is removed when the
	// next day's file is made. Facts change week by week, so a moment much
	// older describes a store that is no longer there.
	deliveryLogDays = 30
	// deliveryLogDayCap: a day's file stops growing here (the utility
	// audit's cap), so a runaway loop of hook calls cannot fill the disk.
	deliveryLogDayCap = 10 << 20
	// deliveryLogText bounds the request and each description kept.
	deliveryLogText = 2000
	deliveryLogDesc = 400
)

// logEntry is one hook block.
type logEntry struct {
	V       int      `json:"v"`
	ID      string   `json:"id"`
	TS      string   `json:"ts"`
	Hook    string   `json:"hook"` // start, resume, compact, summary, prompt, push, nav
	Session string   `json:"session,omitempty"`
	Cwd     string   `json:"cwd,omitempty"`
	Repos   []string `json:"repos,omitempty"`
	// Request is what the block answered: the prompt, the summary's query,
	// or the tool call (its name, then its command or files).
	Request string   `json:"request,omitempty"`
	Terms   []string `json:"terms,omitempty"` // the query's terms (search.Terms)
	// Transcript is the transcript's size in bytes when the hook ran: the
	// turn's place in the session, and where what came after it begins
	// (-1: none to read).
	Transcript     int64  `json:"transcript"`
	TranscriptPath string `json:"transcript_path,omitempty"`
	// Floor and Relative are the prompt selection's floor and share of the
	// best it ran with.
	Floor    float64 `json:"floor,omitempty"`
	Relative float64 `json:"relative,omitempty"`
	// Policy is what chose the block: "gates" (the deterministic gates and
	// floors) or "utility" (the utility provider's scores).
	Policy string    `json:"policy,omitempty"`
	Cands  []logCand `json:"cands,omitempty"`
	// Nothing: the block showed no line. It is an option the policy chose
	// like any line, with PNothing its probability.
	Nothing  bool    `json:"nothing"`
	PNothing float64 `json:"p_nothing"`
	// Nav is a nav entry's step through the store (no block: its
	// nothing fields say nothing).
	Nav *logNav `json:"nav,omitempty"`
}

// logCand is one candidate of a block, delivered or not.
type logCand struct {
	Name      string  `json:"name"`
	Path      string  `json:"path,omitempty"`
	Desc      string  `json:"desc,omitempty"`
	Type      string  `json:"type,omitempty"`
	Scope     string  `json:"scope,omitempty"`
	Tier      int     `json:"tier"`
	Prov      string  `json:"prov"` // search.Hit.Provenance
	Created   string  `json:"created,omitempty"`
	AgeDays   int     `json:"age_days"` // -1: no created date
	Review    string  `json:"review,omitempty"`
	Local     bool    `json:"local,omitempty"`
	Score     float64 `json:"score"`
	Rank      int     `json:"rank"`  // in the pool, from 0; -1 for a line from no ranked pool
	Share     float64 `json:"share"` // score over the pool's best
	Matched   int     `json:"matched"`
	Supported bool    `json:"supported,omitempty"` // relationship evidence admitted it
	Tied      bool    `json:"tied,omitempty"`      // tied to the session's repositories
	// Echo is the share of the line's words the request holds
	// (withoutEchoes's measure; 0 under its minimum of words).
	Echo float64 `json:"echo"`
	// Via is what chose a delivered line: ranked, rule_slot, derived,
	// utility, meta, action, start, again, summary, area, resume.
	Via string `json:"via,omitempty"`
	// Dropped is the gate that left it out: floor, relative, ceiling,
	// seen, echo, held (a compaction's summary holds it).
	Dropped   string `json:"dropped,omitempty"`
	Delivered bool   `json:"delivered"`
	// P is the probability the policy had of showing it: 1 or 0 while the
	// policy is deterministic.
	P float64 `json:"p"`
}

// dlog is this process's open entry; nil when the log is off or no block
// is being built. A hook is one process and builds its blocks one at a
// time, so one entry at a time is enough.
var dlog *logEntry

// deliveryLogOn: the log is on unless the delivery section turns it off.
func deliveryLogOn() bool { return deliveryOn("LOG", true) }

// beginLog opens an entry for a block of hook, writing the one before it.
func beginLog(hook string, in hookInput, request string, repos []string) {
	endLog(nil)
	if !deliveryLogOn() {
		return
	}
	dlog = &logEntry{V: deliveryLogVersion, TS: time.Now().UTC().Format(time.RFC3339Nano), Hook: hook, Session: in.SessionID, Policy: "gates",
		Cwd: in.Cwd, Repos: repos, Request: clipBytes(request, deliveryLogText), Terms: search.Terms(request), Transcript: transcriptSize(in.TranscriptPath), TranscriptPath: in.TranscriptPath}
	dlog.ID = logID(dlog)
}

// logID names an entry across files: its time and session, hashed short.
func logID(e *logEntry) string { return shortHash(e.TS + "\x00" + e.Session + "\x00" + e.Hook) }

func transcriptSize(p string) int64 {
	if p == "" {
		return -1
	}
	fi, err := os.Stat(p)
	if err != nil || !fi.Mode().IsRegular() {
		return -1
	}
	return fi.Size()
}

func clipBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8Start(s[n]) {
		n--
	}
	return s[:n]
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// cand is the candidate entry for h, or a new one.
func (e *logEntry) cand(h search.Hit) *logCand {
	for i := range e.Cands {
		if e.Cands[i].Name == h.Name && e.Cands[i].Path == h.Path {
			return &e.Cands[i]
		}
	}
	age := -1
	if t, err := time.Parse("2006-01-02", firstN(h.Created, 10)); err == nil {
		age = int(time.Since(t).Hours() / 24)
	}
	c := logCand{Name: h.Name, Path: h.Path, Desc: clipBytes(h.Description, deliveryLogDesc), Type: h.Type, Scope: h.Scope, Tier: tier(h),
		Prov: h.Provenance(), Created: h.Created, AgeDays: age, Review: h.Review, Local: h.Local, Score: round3(h.Score), Rank: -1,
		Matched: len(h.MatchedTerms), Supported: h.Evidence != nil && h.Evidence.Supported, Tied: repoTied(h, e.Repos), Echo: echoOf(h, e.Request)}
	e.Cands = append(e.Cands, c)
	return &e.Cands[len(e.Cands)-1]
}

func firstN(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func round3(f float64) float64 { return float64(int64(f*1000+0.5)) / 1000 }

// logPool records a ranked pool: each hit's rank and share of the best.
// score, when given, is the pool's own order (the start block ranks by the
// repository's names, not by Hit.Score).
func logPool(hits []search.Hit, score map[string]float64) {
	if dlog == nil {
		return
	}
	best := 0.0
	for _, h := range hits {
		s := h.Score
		if score != nil {
			s = score[h.Path]
		}
		best = max(best, s)
	}
	for i, h := range hits {
		c := dlog.cand(h)
		if c.Rank < 0 {
			c.Rank = i
		}
		if score != nil {
			c.Score = round3(score[h.Path])
		}
		if best > 0 {
			c.Share = round3(c.Score / best)
		}
	}
}

// logChose marks hits as chosen by via (delivered, if the block prints them).
func logChose(hits []search.Hit, via string) {
	if dlog == nil {
		return
	}
	for _, h := range hits {
		c := dlog.cand(h)
		if c.Via == "" {
			c.Via, c.Dropped = via, ""
		}
	}
}

// logDrop marks h as left out by gate, unless something chose it.
func logDrop(h search.Hit, gate string) {
	if dlog == nil {
		return
	}
	if c := dlog.cand(h); c.Via == "" && c.Dropped == "" {
		c.Dropped = gate
	}
}

// logDropped marks the hits of before that after no longer holds.
func logDropped(before, after []search.Hit, gate string) {
	if dlog == nil {
		return
	}
	for _, h := range before {
		if !slices.ContainsFunc(after, func(a search.Hit) bool { return a.Name == h.Name }) {
			if c := dlog.cand(h); c.Via != "" {
				c.Via, c.Dropped = "", gate
			}
		}
	}
}

// endLog writes the open entry with printed as what the block delivered,
// and closes it. A block that showed nothing is written too: showing
// nothing is a choice the log scores like any other.
func endLog(printed []search.Hit) {
	e := dlog
	dlog = nil
	if e == nil {
		return
	}
	for i := range e.Cands {
		c := &e.Cands[i]
		c.Delivered = slices.ContainsFunc(printed, func(h search.Hit) bool { return h.Name == c.Name })
		if c.Delivered && c.Via == "" {
			c.Via = "utility" // chosen by the utility provider's scores
		}
		if c.Delivered {
			c.Dropped, c.P = "", 1
		} else {
			c.P = 0
		}
	}
	e.Nothing = len(printed) == 0
	e.PNothing = 0
	if e.Nothing {
		e.PNothing = 1
	}
	writeLogEntry(e, time.Now())
}

// deliveryLogDir is <state>/delivery.
func deliveryLogDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(platform.StateDir(home), "delivery")
}

// writeLogEntry appends e to its day's file: one write of one line, so
// hooks running at once do not interleave inside a line.
func writeLogEntry(e *logEntry, now time.Time) {
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	dir := deliveryLogDir()
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		return
	}
	path := filepath.Join(dir, now.UTC().Format("2006-01-02")+".jsonl")
	fi, err := os.Lstat(path)
	switch {
	case err == nil && !fi.Mode().IsRegular():
		return // never follow a link someone put in its place
	case err == nil && fi.Size()+int64(len(b)) > deliveryLogDayCap:
		return
	case err != nil:
		pruneDeliveryLog(dir, now)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(b, '\n'))
}

var logDay = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})\.jsonl$`)

// pruneDeliveryLog removes the days' files older than the window.
func pruneDeliveryLog(dir string, now time.Time) {
	days := deliveryInt("LOG_DAYS", deliveryLogDays)
	if days < 1 {
		days = deliveryLogDays
	}
	cut := now.UTC().AddDate(0, 0, -days).Format("2006-01-02")
	entries, _ := os.ReadDir(dir)
	for _, x := range entries {
		if m := logDay.FindStringSubmatch(x.Name()); m != nil && m[1] < cut {
			os.Remove(filepath.Join(dir, x.Name()))
		}
	}
}

// echoOf is the share of h's name and description words that request
// holds, as withoutEchoes counts them; 0 when the line has fewer words than
// an echo needs.
func echoOf(h search.Hit, request string) float64 {
	if request == "" {
		return 0
	}
	words := map[string]bool{}
	for _, t := range search.AllTerms(request) {
		words[search.TermKey(t)] = true
	}
	keys := map[string]bool{}
	for _, t := range search.AllTerms(h.Name + " " + h.Description) {
		keys[search.TermKey(t)] = true
	}
	if len(keys) < echoMinTerms {
		return 0
	}
	in := 0
	for k := range keys {
		if words[k] {
			in++
		}
	}
	return round3(float64(in) / float64(len(keys)))
}

// logNav is one step of an agent's own way through the store: a folder of
// the tree it opened, a search it ran, or a fact it read whole. Peeking at
// a line in a listing and leaving, or climbing deep and reading a fact
// whole, is what the steps and what follows them say.
type logNav struct {
	Tool  string   `json:"tool"`            // tree, search, fact
	Path  string   `json:"path,omitempty"`  // the tree's folder, a search's scope
	Query string   `json:"query,omitempty"` // a search's words
	Names []string `json:"names,omitempty"` // the facts read whole
	// Depth is how many folders deep the step is (the tree's folder, a
	// search's scope); Levels the levels below it a tree asked for (-1:
	// its default); Offset a search's page.
	Depth  int `json:"depth"`
	Levels int `json:"levels"`
	Offset int `json:"offset,omitempty"`
}

// navCommandRx finds `tacit tree|search|fact ...` in one part of a shell
// command; its arguments run to the part's end.
var navCommandRx = regexp.MustCompile(`(?:^|[\s;&|(])(?:\S*/)?tacit(?:\.exe)?\s+(tree|search|fact)\b([^;&|)\n]*)`)

// navOf reads the store step a tool call takes: tacit's MCP tools (under
// any server name the harness gave tacit), or the CLI in a shell command.
func navOf(in hookInput, command string) []logNav {
	var out []logNav
	if strings.HasPrefix(in.ToolName, "mcp__") {
		var a struct {
			Name   string `json:"name"`
			Path   string `json:"path"`
			Query  string `json:"query"`
			Scope  string `json:"scope"`
			Depth  *int   `json:"depth"`
			Offset int    `json:"offset"`
		}
		json.Unmarshal(in.ToolInput, &a)
		switch {
		case strings.HasSuffix(in.ToolName, "__tacit_fact") && a.Name != "":
			out = append(out, logNav{Tool: "fact", Names: []string{a.Name}, Levels: -1})
		case strings.HasSuffix(in.ToolName, "__tacit_tree"):
			n := logNav{Tool: "tree", Path: a.Path, Depth: folderDepth(a.Path), Levels: -1}
			if a.Depth != nil {
				n.Levels = *a.Depth
			}
			out = append(out, n)
		case strings.HasSuffix(in.ToolName, "__tacit_search"):
			out = append(out, logNav{Tool: "search", Path: a.Scope, Query: clipBytes(a.Query, deliveryLogText), Depth: folderDepth(a.Scope), Levels: -1, Offset: a.Offset})
		}
		return out
	}
	for _, m := range navCommandRx.FindAllStringSubmatch(command, -1) {
		args := strings.Fields(m[2])
		var words []string
		scope := ""
		for i := 0; i < len(args); i++ {
			switch {
			case args[i] == "--scope" && i+1 < len(args):
				scope = args[i+1]
				i++
			case strings.HasPrefix(args[i], "-"):
				if !strings.Contains(args[i], "=") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") && m[1] != "search" {
					i++
				}
			default:
				words = append(words, strings.Trim(args[i], `"'`))
			}
		}
		switch m[1] {
		case "fact":
			if len(words) > 0 {
				out = append(out, logNav{Tool: "fact", Names: words[:1], Levels: -1})
			}
		case "tree":
			path := ""
			if len(words) > 0 {
				path = words[0]
			}
			out = append(out, logNav{Tool: "tree", Path: path, Depth: folderDepth(path), Levels: -1})
		case "search":
			out = append(out, logNav{Tool: "search", Path: scope, Query: clipBytes(strings.Join(words, " "), deliveryLogText), Depth: folderDepth(scope), Levels: -1})
		}
	}
	return out
}

func folderDepth(p string) int {
	p = strings.Trim(p, "/")
	if p == "" {
		return 0
	}
	return strings.Count(p, "/") + 1
}

// logNavigation records the store steps a tool call takes, as one nav entry
// each, read by the PreToolUse hook before the call runs.
func logNavigation(in hookInput, command string) {
	if !deliveryLogOn() {
		return
	}
	for _, n := range navOf(in, command) {
		n := n
		e := &logEntry{V: deliveryLogVersion, TS: time.Now().UTC().Format(time.RFC3339Nano), Hook: "nav", Session: in.SessionID, Cwd: in.Cwd,
			Transcript: transcriptSize(in.TranscriptPath), TranscriptPath: in.TranscriptPath, Nav: &n}
		e.ID = logID(e)
		writeLogEntry(e, time.Now())
	}
}
