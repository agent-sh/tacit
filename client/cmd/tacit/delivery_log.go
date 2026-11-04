package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/fact"
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
	// Tool is a push's tool. Terms are the query terms of what the block
	// answered (the prompt, the summary's query, the repositories' names, or
	// the tool call's command and files), secret-shaped words left out
	// (logTerms); the text itself is never kept.
	Tool  string   `json:"tool,omitempty"`
	Terms []string `json:"terms,omitempty"`
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
	// floors) or "utility" (the utility provider's scores, when they did),
	// then with the threshold they were held to and the utility audit's
	// query hash.
	Policy           string    `json:"policy,omitempty"`
	UtilityThreshold float64   `json:"utility_threshold,omitempty"`
	UtilityQuery     string    `json:"utility_query,omitempty"`
	Cands            []logCand `json:"cands,omitempty"`
	// Nothing: the block showed no line. It is an option the policy chose
	// like any line, with PNothing its probability.
	Nothing  bool    `json:"nothing"`
	PNothing float64 `json:"p_nothing"`
	// Nav is a nav entry's step through the store (no block: its
	// nothing fields say nothing).
	Nav *logNav `json:"nav,omitempty"`
	// full is the whole request, which the echo share is measured on, and
	// words its words, read once.
	full  string
	words map[string]bool
}

// logCand is one candidate of a block, delivered or not.
type logCand struct {
	Name      string  `json:"name"`
	Path      string  `json:"path,omitempty"`
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
	// utility, meta, action, start, again, area, resume.
	Via string `json:"via,omitempty"`
	// Dropped is the gate that left it out: floor, relative, ceiling,
	// seen, echo, held (a compaction's summary holds it), utility (the
	// provider's scores), withheld (a claim of the owner's authority, or a
	// note the reviewer turned down: withoutClaims), local (this machine's
	// notes past their share of the start block), other (a path that does
	// not say why).
	Dropped   string `json:"dropped,omitempty"`
	Delivered bool   `json:"delivered"`
	// P is the probability the policy had of showing it: 1 or 0 while the
	// policy is deterministic.
	P float64 `json:"p"`
	// Utility is the utility provider's score, when its scores chose.
	Utility *float64 `json:"utility,omitempty"`
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
		Cwd: in.Cwd, Repos: repos, Terms: logTerms(request), Transcript: transcriptSize(in.TranscriptPath), TranscriptPath: in.TranscriptPath}
	if hook == "prompt" || hook == "summary" {
		dlog.full = request // the echo filter runs on these only
	}
	dlog.ID = logID(dlog)
}

// logID names an entry across files: its time and session, hashed short.
// 64 bits of a hash of its time, session, hook and its place among this
// process's entries (a tool call's nav steps share a time).
func logID(e *logEntry) string {
	logSeq++
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%d", e.TS, e.Session, e.Hook, os.Getpid(), logSeq)))
	return hex.EncodeToString(sum[:8])
}

var logSeq int

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
	c := logCand{Name: h.Name, Path: h.Path, Type: h.Type, Scope: h.Scope, Tier: tier(h),
		Prov: h.Provenance(), Created: h.Created, AgeDays: age, Review: h.Review, Local: h.Local, Score: round3(h.Score), Rank: -1,
		Matched: len(h.MatchedTerms), Supported: h.Evidence != nil && h.Evidence.Supported, Tied: repoTied(h, e.Repos), Echo: e.echoOf(h)}
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

// pool records a ranked pool: each hit's rank and share of the best.
// score, when given, is the pool's own order (the start block ranks by the
// repository's names, not by Hit.Score). A nil entry records nothing, as
// do all of its methods.
func (e *logEntry) pool(hits []search.Hit, score map[string]float64) {
	if e == nil {
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
		c := e.cand(h)
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

// chose marks hits as chosen by via (delivered, if the block prints them).
func (e *logEntry) chose(hits []search.Hit, via string) {
	if e == nil {
		return
	}
	for _, h := range hits {
		c := e.cand(h)
		if c.Via == "" {
			c.Via, c.Dropped = via, ""
		}
	}
}

// drop marks h as left out by gate, unless something chose it or an
// earlier gate left it out.
func (e *logEntry) drop(h search.Hit, gate string) {
	if e == nil {
		return
	}
	if c := e.cand(h); c.Via == "" && c.Dropped == "" {
		c.Dropped = gate
	}
}

// dropped marks the hits of before that after no longer holds as left out
// by gate, whatever had chosen them.
func (e *logEntry) dropped(before, after []search.Hit, gate string) {
	if e == nil {
		return
	}
	for _, h := range before {
		if !slices.ContainsFunc(after, func(a search.Hit) bool { return a.Name == h.Name && a.Path == h.Path }) {
			if c := e.cand(h); c.Via != "" || c.Dropped == "" {
				c.Via, c.Dropped = "", gate
			}
		}
	}
}

// The process's open entry, for the hooks' own goroutine. Code that may run
// on another goroutine (the utility provider's worker) takes an entry, or
// nil, as an argument instead.
func logPool(hits []search.Hit, score map[string]float64) { dlog.pool(hits, score) }
func logChose(hits []search.Hit, via string)              { dlog.chose(hits, via) }
func logDrop(h search.Hit, gate string)                   { dlog.drop(h, gate) }
func logDropped(before, after []search.Hit, gate string)  { dlog.dropped(before, after, gate) }

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
		c.Delivered = slices.ContainsFunc(printed, func(h search.Hit) bool { return h.Name == c.Name && h.Path == c.Path })
		switch {
		case c.Delivered && c.Via == "":
			c.Via = "other" // a path that does not say what chose it
		case !c.Delivered && c.Via != "":
			c.Via, c.Dropped = "", "other" // chosen, then left out by a path that does not say why
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
	logPending = append(logPending, e)
}

// logPending are the process's entries not yet written: a hook writes them
// all at once, after its answer is printed (flushLog).
var logPending []*logEntry

// flushLog writes the pending entries in one bounded write.
func flushLog() {
	es := logPending
	logPending = nil
	if len(es) > 0 {
		writeBounded(es...)
	}
}

// logWriteBudget bounds a hook's wait for its log line: a stalled state
// directory must not hold a hook (the utility audit's bound).
const logWriteBudget = 100 * time.Millisecond

// logWriteWait is what writeBounded waits: logWriteBudget, except in tests
// that read the log back, which widen it so a loaded machine cannot drop
// the line they check. logWrite is the write.
var (
	logWriteWait = logWriteBudget
	logWrite     = writeLogEntry
)

// writeBounded writes es, waiting at most logWriteWait for all of them; a
// write still running then is given up when the hook's process exits.
func writeBounded(es ...*logEntry) {
	done := make(chan struct{})
	write := logWrite
	go func() {
		for _, e := range es {
			write(e, time.Now())
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(logWriteWait):
	}
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
	} else if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o700 {
		os.Chmod(dir, 0o700) // a directory made wider by hand is this log's again
	}
	path := filepath.Join(dir, now.UTC().Format("2006-01-02")+".jsonl")
	f, err := openDay(path, dir, now, int64(len(b))+1)
	if os.IsExist(err) {
		f, err = openDay(path, dir, now, int64(len(b))+1) // another hook made the day's file first
	}
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(b, '\n'))
}

// openDay opens a day's file to append n bytes: a new one exclusively
// (O_EXCL never follows a link in its place), an existing one only when it
// is the regular file that was there before the open, under the day's cap.
func openDay(path, dir string, now time.Time, n int64) (*os.File, error) {
	fi, err := os.Lstat(path)
	if os.IsNotExist(err) {
		pruneDeliveryLog(dir, now)
		return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	}
	if err != nil || !fi.Mode().IsRegular() || fi.Size()+n > deliveryLogDayCap {
		return nil, errors.New("not this log's day file, or full")
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return nil, err
	}
	if got, err := f.Stat(); err != nil || !os.SameFile(fi, got) {
		f.Close()
		return nil, errors.New("the day's file changed under the open")
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		f.Chmod(0o600)
	}
	return f, nil
}

var logDay = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})\.jsonl$`)

// logDays is the window the log keeps: log_days, 1 to 366, else 30.
func logDays() int {
	if d := deliveryInt("LOG_DAYS", deliveryLogDays); d >= 1 && d <= 366 {
		return d
	}
	return deliveryLogDays
}

// pruneDeliveryLog removes the days' files older than the window.
func pruneDeliveryLog(dir string, now time.Time) {
	cut := now.UTC().AddDate(0, 0, -logDays()).Format("2006-01-02")
	entries, _ := os.ReadDir(dir)
	for _, x := range entries {
		if m := logDay.FindStringSubmatch(x.Name()); m != nil && m[1] < cut {
			os.Remove(filepath.Join(dir, x.Name()))
		}
	}
}

// echoOf is the share of h's name and description words that the request
// holds, as withoutEchoes counts them (echoCount); 0 when the line has
// fewer words than an echo needs.
func (e *logEntry) echoOf(h search.Hit) float64 {
	if e.full == "" {
		return 0
	}
	if e.words == nil {
		e.words = requestWords(e.full)
	}
	in, n := echoCount(h, e.words)
	if n < echoMinTerms {
		return 0
	}
	return round3(float64(in) / float64(n))
}

// logNav is one step of an agent's own way through the store: a folder of
// the tree it opened, a search it ran, or a fact it read whole. Peeking at
// a line in a listing and leaving, or climbing deep and reading a fact
// whole, is what the steps and what follows them say.
type logNav struct {
	Tool  string   `json:"tool"`            // tree, search, fact
	Path  string   `json:"path,omitempty"`  // the tree's folder, a search's scope
	Terms []string `json:"terms,omitempty"` // a search's terms (logTerms)
	Names []string `json:"names,omitempty"` // the facts read whole
	// Depth is how many folders deep the step is (the tree's folder, a
	// search's scope); Levels the levels below it a tree asked for (-1:
	// its default); Offset a search's page.
	Depth  int `json:"depth"`
	Levels int `json:"levels"`
	Offset int `json:"offset,omitempty"`
}

// navValueFlags are the flags of `tacit tree|search|fact` that take a
// value (client/cmd/tacit: runTree, runSearch); the others are switches.
var navValueFlags = map[string]map[string]bool{
	"search": {"repo": true, "host": true, "limit": true, "scope": true, "type": true, "path": true, "command": true, "offset": true},
	"tree":   {"depth": true, "repo": true, "host": true},
	"fact":   {},
}

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
		case strings.HasSuffix(in.ToolName, "__tacit_fact") && fact.IsSlug(a.Name):
			out = append(out, logNav{Tool: "fact", Names: []string{a.Name}, Levels: -1})
		case strings.HasSuffix(in.ToolName, "__tacit_tree"):
			n := logNav{Tool: "tree", Path: scopePath(a.Path), Depth: folderDepth(scopePath(a.Path)), Levels: -1}
			if a.Depth != nil {
				n.Levels = *a.Depth
			}
			out = append(out, n)
		case strings.HasSuffix(in.ToolName, "__tacit_search"):
			out = append(out, logNav{Tool: "search", Path: scopePath(a.Scope), Terms: logTerms(a.Query), Depth: folderDepth(scopePath(a.Scope)), Levels: -1, Offset: a.Offset})
		}
		return out
	}
	for _, part := range cmdBoundary.Split(command, -1) {
		if n, ok := navCommand(strings.Fields(part)); ok {
			out = append(out, n)
		}
	}
	return out
}

// navCommand reads one part of a shell command that runs tacit's tree,
// search or fact (environment assignments before it skipped, the program
// by base name, `/` or `\` paths): its flags as Go's flag package reads them
// (-k v, --k v, -k=v), the rest its words.
func navCommand(w []string) (logNav, bool) {
	for len(w) > 0 && strings.Contains(w[0], "=") && !strings.HasPrefix(w[0], "-") {
		w = w[1:]
	}
	if len(w) < 2 {
		return logNav{}, false
	}
	prog := w[0][strings.LastIndexAny(w[0], `/\`)+1:]
	sub := w[1]
	values, ok := navValueFlags[sub]
	if (prog != "tacit" && prog != "tacit.exe") || !ok {
		return logNav{}, false
	}
	flags := map[string]string{}
	var words []string
	for i := 2; i < len(w); i++ {
		a := w[i]
		if a == "--" || !strings.HasPrefix(a, "-") || a == "-" {
			// Go's flag package stops at the first word that is no flag.
			if a == "--" {
				i++
			}
			for _, x := range w[i:] {
				words = append(words, strings.Trim(x, `"'`))
			}
			break
		}
		k, v, has := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if values[k] && !has && i+1 < len(w) {
			v, has = w[i+1], true
			i++
		}
		if has {
			flags[k] = strings.Trim(v, `"'`)
		}
	}
	n := logNav{Tool: sub, Levels: -1}
	switch sub {
	case "fact":
		if len(words) != 1 || !fact.IsSlug(words[0]) {
			return logNav{}, false // runFact refuses it
		}
		n.Names = words
	case "tree":
		if len(words) > 1 {
			return logNav{}, false // runTree takes one folder
		}
		if len(words) > 0 {
			n.Path = words[0]
		}
		if d, err := strconv.Atoi(flags["depth"]); err == nil {
			n.Levels = d
		}
	case "search":
		n.Path, n.Terms = flags["scope"], logTerms(strings.Join(words, " "))
		n.Offset, _ = strconv.Atoi(flags["offset"])
	}
	n.Path = scopePath(n.Path)
	n.Depth = folderDepth(n.Path)
	return n, true
}

// scopeShape is a store folder: name segments joined by /.
var scopeShape = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]*(?:/[A-Za-z0-9][A-Za-z0-9._@-]*)*$`)

// scopePath is p when it is shaped as a store folder (whose segments are no
// secret), else nothing: the log keeps no free text.
func scopePath(p string) string {
	p = strings.Trim(p, "/")
	if !scopeShape.MatchString(p) || len(p) > 200 {
		return ""
	}
	for _, seg := range strings.Split(p, "/") {
		if secretWord(seg) || secretShaped(seg) {
			return ""
		}
	}
	return p
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
		logPending = append(logPending, e)
	}
}
