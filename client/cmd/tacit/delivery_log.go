package main

import (
	"context"
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
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/platform"
	"github.com/agent-sh/tacit/client/internal/search"
	"github.com/agent-sh/tacit/client/internal/store"
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
	// the tool call's command and files)
	// that the store's index holds (logTerms); the text itself is never
	// kept. NTerms is how many terms it had before that check.
	Tool   string   `json:"tool,omitempty"`
	Terms  []string `json:"terms,omitempty"`
	NTerms int      `json:"n_terms"`
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
	// then with the threshold they were held to.
	Policy           string    `json:"policy,omitempty"`
	UtilityThreshold *float64  `json:"utility_threshold,omitempty"`
	Cands            []logCand `json:"cands,omitempty"`
	// Nothing: the block showed no line. It is an option the policy chose
	// like any line, with PNothing its probability.
	Nothing  bool    `json:"nothing"`
	PNothing float64 `json:"p_nothing"`
	// Omitted is how many candidates of the block's pools the entry leaves
	// out: past the head of the start block's pool, or past logCandCap.
	Omitted int `json:"omitted,omitempty"`
	// HoldoutQ is the share of blocks that withhold one agent's note
	// (Hold-outs), HeldOut the note this block withheld.
	HoldoutQ float64 `json:"holdout_q,omitempty"`
	HeldOut  string  `json:"held_out,omitempty"`
	// PMark is the probability the block had of asking for a self-mark,
	// MarkAsked whether it asked; Mark is a mark entry's marks.
	PMark     *float64     `json:"p_mark,omitempty"`
	MarkAsked bool         `json:"mark_asked,omitempty"`
	Mark      *logMarkStep `json:"mark,omitempty"`
	// Judge is a judge entry's labels (the dreamer's judge).
	Judge *judgeVerdict `json:"judge,omitempty"`
	// JudgePass is a judge-pass entry: one pass's draw and what became of it.
	JudgePass *judgePassLog `json:"judge_pass,omitempty"`
	// Nav is a nav entry's step through the store (no block: its
	// nothing fields say nothing).
	Nav *logNav `json:"nav,omitempty"`
	// full is the whole request, which the echo share is measured on, and
	// words its words, read once. ix and req are the hook's index and the
	// request, whose terms endLog takes, once the answer is printed.
	full  string
	words map[string]bool
	// titled: hookBlock printed the block's agents' notes as titles.
	titled bool
	// propensity is each eligible line's probability of being shown under
	// the hold-out draw, by path (holdOut); endLog puts it in its p.
	propensity map[string]float64
	pNothing   *float64
	ix         *search.Index
	req        string
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
	// Form is how a printed line read: "title" (no description: the agent
	// opens it) or "full".
	Form string `json:"form,omitempty"`
}

// hookBlock is the block of a hook the titles A/B measured (the start, the
// prompt, a shell command's push): with the titles setting on
// (TACIT_DELIVERY_TITLES, the default) an agent's note is a title the agent
// opens, the owner's own lines whole, and note ends the block's first line;
// off, or with no line to title, whole lines (SPEC.md, "Titles"). The
// blocks no A/B measured (after a compaction, the summary's selection, a
// resume, an edit's push) call search.Block.
func hookBlock(hits []search.Hit, title, note string) string {
	if !deliveryTitles() || !hookMCP || !slices.ContainsFunc(hits, func(h search.Hit) bool { return !h.OwnerLine() }) {
		return search.Block(hits, title)
	}
	if dlog != nil {
		dlog.titled = true
	}
	if t, rest, ok := strings.Cut(title, "\n"); ok {
		return search.TitleBlock(hits, t+note+"\n"+rest)
	}
	return search.TitleBlock(hits, title+note)
}

// hookMCP: the hook was told the harness has tacit's MCP server (--mcp,
// which `tacit install claude` writes on the start, prompt and push hooks).
// Titles need it: the A/B measured them there, with tacit_fact to open one.
// A harness without it (Codex, Hermes, --no-mcp) keeps whole lines.
var hookMCP bool

// titlesNote names both ways to open a fact: the MCP tool where the
// harness has tacit's server, the command where it has not (the prompt and
// push hooks do not know which). The start's first line already names the
// way its harness has (openDetail), so it only says the lines are titles.
const (
	titlesNote      = " (titles: `tacit_fact <name>`, or `tacit fact <name>`, opens one)"
	startTitlesNote = " (titles)"
)

func deliveryTitles() bool { return deliveryOn("TITLES", true) }

// dlog is this process's open entry; nil when the log is off or no block
// is being built. A hook is one process and builds its blocks one at a
// time, so one entry at a time is enough.
var dlog *logEntry

// deliveryLogOn: the log is on unless the delivery section turns it off.
func deliveryLogOn() bool { return deliveryOn("LOG", true) }

// beginLog opens an entry for a block of hook, writing the one before it.
func beginLog(hook string, in hookInput, request string, repos []string, ix *search.Index) {
	endLog(nil)
	if !deliveryLogOn() {
		return
	}
	dlog = &logEntry{V: deliveryLogVersion, TS: time.Now().UTC().Format(time.RFC3339Nano), Hook: hook, Session: in.SessionID, Policy: "gates",
		Cwd: in.Cwd, Repos: repos, Transcript: transcriptSize(in.TranscriptPath), TranscriptPath: in.TranscriptPath}
	if hook == "prompt" || hook == "summary" {
		dlog.full = request // the echo filter runs on these only
	}
	dlog.ix, dlog.req = ix, request
	if hook == "prompt" {
		zero := 0.0
		dlog.PMark = &zero // the probability of a self-mark ask (markAsk); 0 when none could be asked
	}
	dlog.ID = logID(dlog)
}

// logID names an entry across files: its time and session, hashed short.
// 64 bits of a hash of its time, session, hook and its place among this
// process's entries (a tool call's nav steps share a time).
func logID(e *logEntry) string {
	n := logSeq.Add(1) // the MCP server writes marks from calls run at once
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%d", e.TS, e.Session, e.Hook, os.Getpid(), n)))
	return hex.EncodeToString(sum[:8])
}

var logSeq atomic.Int64

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

// logCandCap bounds the candidates an entry holds: twice the prompt's
// widest pool (promptRankPool), so a prompt's whole pool with its derived,
// metadata and action lines always fits and only a runaway pool is cut.
// Past it every delivered line stays and the others go by rank, best
// first, counted in Omitted.
const logCandCap = 2 * promptRankPool

// capCands keeps an entry within logCandCap candidates.
func (e *logEntry) capCands() {
	if len(e.Cands) <= logCandCap {
		return
	}
	var rest []int
	keep := map[int]bool{}
	for i, c := range e.Cands {
		if c.Delivered {
			keep[i] = true
		} else {
			rest = append(rest, i)
		}
	}
	rank := func(c logCand) int {
		if c.Rank < 0 {
			return 1 << 30
		}
		return c.Rank
	}
	sort.SliceStable(rest, func(a, b int) bool { return rank(e.Cands[rest[a]]) < rank(e.Cands[rest[b]]) })
	for _, i := range rest {
		if len(keep) >= logCandCap {
			break
		}
		keep[i] = true
	}
	var out []logCand
	for i, c := range e.Cands {
		if keep[i] {
			out = append(out, c)
		}
	}
	e.Omitted += len(e.Cands) - len(out)
	e.Cands = out
}

func logOmitted(n int) {
	if dlog != nil && n > 0 {
		dlog.Omitted += n
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
			c.Dropped, c.P, c.Form = "", 1, "full"
			for _, h := range printed {
				if h.Name == c.Name && h.Path == c.Path && e.titled && !h.OwnerLine() {
					c.Form = "title"
				}
			}
		} else {
			c.P = 0
		}
		// Under the hold-out draw a line was shown, or withheld, with the
		// probability holdOut computed.
		if p, ok := e.propensity[c.Path]; ok && (c.Delivered || c.Dropped == "holdout") {
			c.P = p
		}
	}
	e.capCands()
	e.Terms, e.NTerms = logTerms(e.ix, e.req)
	e.ix = nil
	e.Nothing = len(printed) == 0
	e.PNothing = 0
	if e.Nothing {
		e.PNothing = 1
	}
	if e.pNothing != nil {
		e.PNothing = *e.pNothing // the hold-out could empty the block
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
func writeLogEntry(e *logEntry, now time.Time) { _ = appendLogEntry(e, now) }

// appendLogEntry is writeLogEntry with its failure: the MCP server's marks
// report a line that was not appended.
func appendLogEntry(e *logEntry, now time.Time) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	dir := deliveryLogDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if fi, err := os.Lstat(dir); err != nil {
		return err
	} else if !fi.IsDir() {
		return errors.New("the delivery log's directory is not a directory")
	} else if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o700 {
		os.Chmod(dir, 0o700) // a directory made wider by hand is this log's again
	}
	name := now.UTC().Format("2006-01-02") + ".jsonl"
	n := int64(len(b)) + 1
	var f *os.File
	if d, err := platform.OpenPrivateDirectory(dir); err == nil {
		// Unix: relative to a pinned directory, no link, no special file,
		// no second hard link, this user's own (the utility audit's open).
		f, err = platform.OpenPrivateAppend(d, name)
		d.Close()
		if err != nil {
			return err
		}
		if fi, err := f.Stat(); err != nil || fi.Size()+n > deliveryLogDayCap {
			f.Close()
			return errors.New("the day's delivery log is full")
		} else if fi.Size() == 0 {
			pruneDeliveryLog(dir, now)
		}
	} else {
		path := filepath.Join(dir, name)
		f, err = openDay(path, dir, now, n)
		if os.IsExist(err) {
			f, err = openDay(path, dir, now, n) // another hook made the day's file first
		}
		if err != nil {
			return err
		}
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// openDay is the day's open where the private open is not to be had
// (Windows): it opens a day's file to append n bytes, a new one exclusively
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
	Tool   string   `json:"tool"`            // tree, search, fact
	Path   string   `json:"path,omitempty"`  // the tree's folder, a search's scope
	Terms  []string `json:"terms,omitempty"` // a search's terms in the store's vocabulary (logTerms)
	NTerms int      `json:"n_terms,omitempty"`
	// query is the search's words, read only to take its terms.
	query string
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
		case strings.HasSuffix(in.ToolName, "__tacit_fact") && a.Name != "":
			out = append(out, logNav{Tool: "fact", Names: []string{a.Name}, Levels: -1})
		case strings.HasSuffix(in.ToolName, "__tacit_tree"):
			n := logNav{Tool: "tree", Path: a.Path, Levels: -1}
			if a.Depth != nil {
				n.Levels = *a.Depth
			}
			out = append(out, n)
		case strings.HasSuffix(in.ToolName, "__tacit_search"):
			out = append(out, logNav{Tool: "search", Path: a.Scope, query: a.Query, Levels: -1, Offset: a.Offset})
		}
		return out
	}
	for _, part := range cmdBoundary.Split(command, -1) {
		if n, ok := navCommand(shellWords(part)); ok {
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
		n.Path, n.query = flags["scope"], strings.Join(words, " ")
		n.Offset, _ = strconv.Atoi(flags["offset"])
	}
	return n, true
}

// logNavigation records the store steps a tool call takes, as one nav entry
// each, read by the PreToolUse hook before the call runs. What a step names
// is kept only when the store holds it: a fact name the index has, a folder
// that is a directory of the store, a search's terms in the store's
// vocabulary (logTerms). Without the index, the names and terms are left
// out and their count kept.
func logNavigation(s *store.Store, in hookInput, steps []logNav) {
	if !deliveryLogOn() || len(steps) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), logVocabularyBudget)
	defer cancel()
	ix, err := openHookIndex(ctx, s)
	if err != nil {
		ix = nil
	} else {
		defer ix.Close()
	}
	for _, n := range steps {
		n := n
		var names []string
		for _, name := range n.Names {
			if ix != nil && ix.HasFact(ctx, name) {
				names = append(names, name)
			}
		}
		n.Names = names
		n.Path = storeFolder(s.Dir, n.Path)
		n.Depth = folderDepth(n.Path)
		if n.Tool == "search" {
			n.Terms, n.NTerms = logTerms(ix, n.query)
		}
		e := &logEntry{V: deliveryLogVersion, TS: time.Now().UTC().Format(time.RFC3339Nano), Hook: "nav", Session: in.SessionID, Cwd: in.Cwd,
			Transcript: transcriptSize(in.TranscriptPath), TranscriptPath: in.TranscriptPath, Nav: &n}
		e.ID = logID(e)
		logPending = append(logPending, e)
	}
}

// logVocabularyBudget bounds the index lookups that decide what a nav entry
// may keep.
const logVocabularyBudget = 150 * time.Millisecond

// storeFolder is p when it names a directory of the store (a scope or a
// folder under one), else nothing.
func storeFolder(dir, p string) string {
	p = strings.Trim(filepath.ToSlash(filepath.Clean("/"+p)), "/")
	if p == "" || p == "." {
		return ""
	}
	fi, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(p)))
	if err != nil || !fi.IsDir() || strings.HasPrefix(p, ".") {
		return ""
	}
	return p
}

// logTerms is the query terms of text (search.Terms, as the selection reads
// them) that the store's vocabulary holds, and how many terms text had. A
// term no fact of the store holds is left out, whatever it is: the store
// masks every write (store.Redact), so a term it holds is no secret it did
// not already hold, and a term it does not hold matches no fact. Without an
// index, none is kept.
func logTerms(ix *search.Index, text string) ([]string, int) {
	terms := search.Terms(text)
	if ix == nil || len(terms) == 0 {
		return nil, len(terms)
	}
	ctx, cancel := context.WithTimeout(context.Background(), logVocabularyBudget)
	defer cancel()
	return ix.InVocabulary(ctx, terms), len(terms)
}

// shellWords splits one part of a command line into its words as a shell
// does: quotes (single, or double with backslash escapes) keep a value
// whole and are removed; a backslash before a space, a quote or another
// backslash escapes it, and any other stays (a Windows path).
func shellWords(s string) []string {
	var out []string
	var b strings.Builder
	in, quote := false, byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			} else {
				b.WriteByte(c)
			}
		case quote == '"':
			if c == '"' {
				quote = 0
			} else if c == '\\' && i+1 < len(s) && strings.IndexByte(`"\\$`+"`", s[i+1]) >= 0 {
				i++
				b.WriteByte(s[i])
			} else {
				b.WriteByte(c)
			}
		case c == '\'' || c == '"':
			quote, in = c, true
		case c == '\\' && i+1 < len(s) && strings.IndexByte(" \t\"'\\", s[i+1]) >= 0:
			// An escape; any other backslash is a Windows path's.
			i++
			b.WriteByte(s[i])
			in = true
		case c == ' ' || c == '\t' || c == '\r':
			if in {
				out = append(out, b.String())
				b.Reset()
				in = false
			}
		default:
			b.WriteByte(c)
			in = true
		}
	}
	if in {
		out = append(out, b.String())
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
