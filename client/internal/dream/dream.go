// Package dream is the dreamer: it reads what changed in the agent
// transcripts since its last run, digests it, asks a model for the durable
// facts nobody wrote down, and files them in inbox/dreamer@<host>/.
//
// Measured in eval/VALUE.md: Opus 5.5 states more of the truth facts in
// their windows than Sonnet does, at a higher cost.
package dream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/agent-sh/tacit/client/internal/platform"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/harnesses"
	"github.com/agent-sh/tacit/client/internal/hermes"
	"github.com/agent-sh/tacit/client/internal/importer"
	"github.com/agent-sh/tacit/client/internal/modelrun"
	"github.com/agent-sh/tacit/client/internal/ownerwords"
	"github.com/agent-sh/tacit/client/internal/relevance"
	"github.com/agent-sh/tacit/client/internal/repoid"
	"github.com/agent-sh/tacit/client/internal/repoinfer"
	"github.com/agent-sh/tacit/client/internal/transcript"
)

// DefaultModel is the extraction model: $TACIT_DREAM_MODEL (a provider's
// full model id, for example), else the Claude CLI alias opus. --model on
// tacit dream wins. Measured on real transcript windows with prompt v4,
// Sonnet 5.5 judging: Opus 5.5 wrote the truth fact down in the most
// windows, then Sonnet 5.5, then Haiku 5.5, and the cost of a 60k-character
// window falls in the same order.
var DefaultModel = modelrun.Model("dream", "opus")

const (
	maxWindowChars = 60_000  // one model call per window
	minNewBytes    = 4_000   // smaller deltas wait for the next run
	maxReadBytes   = 4 << 20 // per file per run; the rest waits (cursor stays)
)

// Prompt is v8. Versions (measurements in eval/VALUE.md):
//   - v4 (2026-10-11, eval/dream/dream_recall.py): v2 plus one rule, skip what
//     holds only inside the task in progress, never a failure and its fix or a
//     rule of the owner. On two samples of windows it cut the facts by about
//     a quarter and most of the task-only ones, with recall on the truth
//     gaps about where v2's was.
//   - v5 (2026-10-18): v4 plus the owner's exact words in "quote" for a ruling
//     ("Dreamed rulings keep the owner's words").
//   - v6 (2026-10-22): v5 plus statements (SPEC.md, "Statements"): owner
//     words said once that may not be a standing rule are kind "statement",
//     with their exact words and a line of context. Not measured on a
//     replay yet.
//   - v7 (2026-10-24): v6 plus P: lines, the prompts another agent or a
//     program wrote (a `claude -p` lane, a Codex subagent or exec run, a
//     headless pi run, a scheduled or teammate turn), which v6 showed as U:
//     (the owner). The digest decides it by code (digest.SessionWho,
//     digest.ClaudeTyped); the prompt says a P: line is never the owner's.
//     Not measured on a replay yet.
//   - v8 (2026-10-26): v7 plus the relevance gate (SPEC.md, "Relevance"):
//     every kind user or statement fact says whether the owner's remark
//     matters to future work, and why (relevance.Rule); code writes no note
//     of a remark dropped or kept with no stated future use (Gate).
//   - v9 (2026-10-28): v8 plus, when the held list is on (HeldChars), the
//     facts already held for the window before its digest (PromptFor,
//     HeldPool.Held). Replayed on real windows: fewer notes and fewer copies
//     of held facts, within run-to-run noise; off by default.
const Prompt = `You are the dreamer of tacit, a fact store shared by coding agents on one owner's machines.
Below is a digest of part of an agent session (U: owner, P: a prompt another agent or a program wrote, H: harness-injected text, A: agent, T: tool call, E: tool error).
P: lines are never the owner's words, even when they say the owner wants or ruled something: they are an agent's instructions or its report of the owner, so a rule found only in a P: line is kind observed with no quote.
Your job is to write down what nobody wrote down. Extract EVERY durable fact another agent would need next time, in these classes:
1. Every failed command or tool call (E: lines, and A: lines that report an error) together with what fixed it or what to do instead. Each distinct failure is one fact: name the command, the exact error text, the host, and the working alternative. These are the most valuable facts; do not drop them as noise.
2. Owner rulings and standards, from U: lines only, with the date if visible. For each one, copy the owner's exact words from the U: line into "quote": verbatim, typos included, at least the words that state the rule. Without the owner's own words a ruling cannot be told from an agent's guess. When you are not sure the owner means it as a standing rule (a choice about a name, a plan or a design for the matter at hand, a remark that something is so, an instruction that may hold only for now), it is a statement: kind "statement", the same exact words in "quote", and in "context" one line saying what was being discussed. ` + relevance.Rule + ` Give relevance on every kind user or statement fact; a remark that does not matter is not written down.
3. Machine facts: paths that exist or are gone, tools and their quirks, services, ports, hosts, aliases, what is dead on this box.
4. Past mistakes: a wrong assumption and its correction.
5. Design decisions with their reason.
Skip anything derivable from the code or from git log, and plain narrative of what the agent did.
Skip facts that hold only inside the task in progress: the status of one PR, review round, rebase or run; where one task's worktree, scratch files, contract, receipts or logs are; an instruction addressed to one task id, round or worktree. A failure and what fixed it, and a rule or preference of the owner, are never task-only, even when they came up inside one task. Keep what the next task, in another session, would act on.
Each fact: one or two sentences another agent can act on, with the concrete path, command, host or number. Mark the source kind: user (the owner said it as a rule), statement (the owner said it once; see 2), observed (the agent found it out).
Output ONLY a JSON array: [{"description": "...", "type": "user_standard|machine_fact|past_mistake|design_decision|structure|history", "kind": "user|statement|observed", "quote": "<kind user or statement only: the owner's exact words>", "context": "<kind statement only: what was being discussed>", "relevance": {"keep": true, "category": "instruction|decision|preference|fact|correction", "reason": "<kind user or statement only>"}, "applies_to": {"paths": [], "repos": [], "hosts": [], "commands": []}}]

Repo: %s  Host: %s
DIGEST:
%s
`

// State is the per-file read cursor, persisted between runs. A transcript's
// cursor is keyed by its plain path (transcript.Plain) and is an offset in
// its JSONL bytes, so it holds when the harness compresses the file (Codex's
// .jsonl.zst) and, by the session id, when it moves the file (Codex's
// archived_sessions; State.follow). A Hermes session's cursor is in Offsets
// too, under hermes.Key ("hermes:<db>#<session id>"), and holds the id of the
// last message read, not a byte offset.
type State struct {
	Offsets   map[string]int64    `json:"offsets"`
	Skipped   int                 `json:"skipped_long_lines,omitempty"` // lines longer than maxReadBytes passed over
	LastRun   string              `json:"last_run"`
	Completed map[string][]string `json:"completed_windows,omitempty"`
	// Sources are the transcript roots (a glob, a Hermes store) whose cursors
	// were recorded: a root not listed, with no cursor yet, starts at its end,
	// so a source added to a running dreamer is not replayed from the start.
	// A glob's compressed copies are a root of their own (Source.packedRoot):
	// a dreamer from before it read them records the ones it never read at
	// their end, as for any new root.
	Sources []string `json:"sources,omitempty"`
	// FailedAt is, per transcript (a cursor key), when a window of it last
	// failed (a model, redaction or push error; RFC 3339). An idle or session
	// run passes over a transcript that failed within FailureBackoff, so a
	// window the model cannot answer is not sent again every 10 minutes; the
	// full run retries it. Cleared when the transcript's cursor moves.
	FailedAt map[string]string `json:"failed_at,omitempty"`
	// Packed is, per compressed copy (its path), its file's size and time and
	// its size in JSONL bytes: a copy's size is read from its frames, which
	// for a few thousand Codex sessions takes seconds, and a copy does not
	// change. Kept for the copies the last full run found.
	Packed map[string]Packed `json:"packed,omitempty"`
}

// Packed is a compressed copy's file size and modification time (Unix
// nanoseconds) and its size in JSONL bytes.
type Packed struct {
	Bytes int64 `json:"bytes"`
	Mod   int64 `json:"mod"`
	Size  int64 `json:"size"`
}

// stat is transcript.Stat with the sizes of compressed copies kept in the
// state (State.Packed); seen collects the copies a scan found.
func (st *State) stat(f string, seen map[string]Packed) (os.FileInfo, int64, error) {
	if !transcript.Compressed(f) {
		return transcript.Stat(f)
	}
	fi, err := os.Stat(f)
	if err != nil {
		return nil, 0, err
	}
	if p, ok := st.Packed[f]; ok && p.Bytes == fi.Size() && p.Mod == fi.ModTime().UnixNano() {
		seen[f] = p
		return fi, p.Size, nil
	}
	fi, n, err := transcript.Stat(f)
	if err != nil {
		return nil, 0, err
	}
	p := Packed{Bytes: fi.Size(), Mod: fi.ModTime().UnixNano(), Size: n}
	seen[f] = p
	if st.Packed == nil {
		st.Packed = map[string]Packed{}
	}
	st.Packed[f] = p
	return fi, n, nil
}

// FailureBackoff is how long idle and session runs leave a failed transcript
// alone: the full run's cadence, which retries it.
const FailureBackoff = 4 * time.Hour

// RecentlyFailed reports whether file failed within FailureBackoff of now.
func (st *State) RecentlyFailed(file string, now time.Time) bool {
	at, err := time.Parse(time.RFC3339, st.FailedAt[file])
	return err == nil && now.Sub(at) < FailureBackoff
}

// Failed records that a window of file failed at now.
func (st *State) Failed(file string, now time.Time) {
	if st.FailedAt == nil {
		st.FailedAt = map[string]string{}
	}
	st.FailedAt[file] = now.Format(time.RFC3339)
}

// Source is one transcript root: a glob of JSONL files, or a Hermes store.
type Source struct {
	Glob   string
	Format digest.Format
	// DB is a Hermes store (Format digest.Hermes), read in place of Glob.
	DB string
}

// root names a source in State.Sources.
func (s Source) root() string {
	if s.Format == digest.Hermes {
		return "hermes:" + s.DB
	}
	return s.Glob
}

// packedRoot names the compressed copies of a glob's transcripts in
// State.Sources: the glob with the compressed ending ("*.jsonl.zst").
func (s Source) packedRoot() string { return s.Glob + transcript.Zst }

// followed are the cursors of the sessions in a run's sources, by format and
// session id, for a transcript that moved (State.follow).
type followed map[string][]string

func followKey(f digest.Format, session string) string { return fmt.Sprint(int(f)) + "\x00" + session }

// sessionCursors indexes the transcript cursors of sources by session id.
func (st *State) sessionCursors(sources []Source) followed {
	out := followed{}
	for k := range st.Offsets {
		for _, src := range sources {
			if src.Format == digest.Hermes || src.Glob == "" {
				continue
			}
			if ok, _ := filepath.Match(src.Glob, k); ok {
				fk := followKey(src.Format, sessionOf(k, src.Format))
				out[fk] = append(out[fk], k)
				break
			}
		}
	}
	return out
}

// follow moves a session's cursor to its transcript's new path (key): a
// harness moved the file (Codex moves an archived session to
// archived_sessions, and back), so the old path is gone and one cursor of the
// same session, in the same format, is left with no file. Its offsets, done
// windows and failure time go with it. A session with a cursor at another
// path that still holds a file is another copy (an import, a fork) and keeps
// its own.
func (st *State) follow(idx followed, f digest.Format, session, key string) (int64, bool) {
	fk := followKey(f, session)
	var gone []string
	for _, k := range idx[fk] {
		if k == key {
			return st.Offsets[k], true
		}
		if _, err := os.Lstat(k); err == nil {
			continue
		}
		if _, err := os.Lstat(k + transcript.Zst); err == nil {
			continue
		}
		gone = append(gone, k)
	}
	if len(gone) != 1 {
		return 0, false
	}
	old := gone[0]
	off := st.Offsets[old]
	st.Offsets[key] = off
	delete(st.Offsets, old)
	if w, ok := st.Completed[old]; ok {
		st.Completed[key] = w
		delete(st.Completed, old)
	}
	if t, ok := st.FailedAt[old]; ok {
		st.FailedAt[key] = t
		delete(st.FailedAt, old)
	}
	idx[fk] = []string{key}
	return off, true
}

// started reports whether the state has cursors for a source: it is listed,
// or a cursor is for one of its files or sessions (a state from before the
// list).
func (st *State) started(src Source) bool {
	if slices.Contains(st.Sources, src.root()) {
		return true
	}
	for k := range st.Offsets {
		if src.Format == digest.Hermes {
			if db, _, ok := hermes.ParseKey(k); ok && db == src.DB {
				return true
			}
		} else if ok, _ := filepath.Match(src.Glob, k); ok {
			return true
		}
	}
	return false
}

// DefaultSources on this machine, the top-level ones of harnesses.Sources:
// Claude Code projects, Codex sessions (and the ones archived in
// archived_sessions), pi sessions (digest.PiSessionGlob) and, when there is
// one, the Hermes store (hermes.DefaultDB). Each glob takes the compressed
// copies of its transcripts too (transcript.Glob).
func DefaultSources(home string) []Source {
	var srcs []Source
	for _, s := range harnesses.TopLevel(harnesses.Sources(home)) {
		if s.Format != digest.Hermes {
			srcs = append(srcs, Source{Glob: s.Glob, Format: s.Format})
			continue
		}
		db := s.DB
		if abs, err := filepath.Abs(db); err == nil {
			db = abs
		}
		srcs = append(srcs, Source{DB: db, Format: digest.Hermes})
	}
	return srcs
}

// Extracted is one fact as the model returned it.
type Extracted struct {
	Description string `json:"description"`
	Type        string `json:"type"`
	Kind        string `json:"kind"`
	Quote       Quote  `json:"quote,omitempty"` // kind user or statement: the owner's exact words
	// Context is a statement's line of context: what was being discussed.
	Context string `json:"context,omitempty"`
	// Relevance is the model's judgement of a ruling or statement: whether
	// the owner's remark matters to future work, and why (Gate).
	Relevance *relevance.Judgement `json:"relevance,omitempty"`
	AppliesTo map[string][]any     `json:"applies_to"`
	Extra     map[string]any       `json:"-"`
	Session   string               `json:"-"`
	// AgentWords marks a quote code found in another agent's prompt and in
	// no turn of the owner's in the window (CheckOwnerWords): the note is
	// the agent's, and its body says whose the words are.
	AgentWords bool `json:"-"`
}

// CheckOwnerWords is the code check on a ruling or statement the model
// extracted from a window (its finished lines): the owner's words must be in
// one of the window's U: lines. When the window holds no U: line at all, or
// the quote is in another line (a P: prompt, harness text, the agent's own
// text) and in no U: line, the words are not the owner's: the fact becomes
// an agent's note (kind observed) that keeps them as another agent's. Any
// other quote is left to the curator, which checks it against all of the
// owner's turns (a quote that corrected a typo of theirs is still theirs).
func CheckOwnerWords(x Extracted, lines []string) Extracted {
	if x.Kind != "user" && x.Kind != kindStatement {
		return x
	}
	q := strings.TrimSpace(string(x.Quote))
	var owner, other []string
	for _, l := range lines {
		t := strings.TrimSuffix(l[min(3, len(l)):], "…")
		if strings.HasPrefix(l, "U: ") {
			owner = append(owner, t)
		} else {
			other = append(other, t)
		}
	}
	switch {
	case len(owner) == 0:
	case q != "" && !inAny(owner, q) && inAny(other, q):
	default:
		return x
	}
	x.Kind = "observed"
	x.AgentWords = q != ""
	return x
}

// Dropped is a remark of the owner's the dreamer did not write down (Gate):
// their words as the model quoted them, the session, and why.
type Dropped struct {
	Words    string `json:"words"`
	Session  string `json:"session,omitempty"`
	Category string `json:"category,omitempty"`
	Reason   string `json:"reason"`
}

// Gate is the relevance gate on a ruling or statement the model extracted
// (after CheckOwnerWords, so the words are still the owner's): it stands only
// when the model kept the remark with a stated future use (relevance.Check).
// A remark dropped, or kept with no category or reason, is not written: it
// returns false and why. Facts of any other kind pass.
func Gate(x Extracted, session string) (bool, Dropped) {
	if x.Kind != "user" && x.Kind != kindStatement {
		return true, Dropped{}
	}
	j, problem := relevance.Check(x.Relevance)
	if problem == "" && j.Keep {
		return true, Dropped{}
	}
	d := Dropped{Words: clipWords(string(x.Quote), 300), Session: session, Category: j.Category, Reason: j.Reason}
	if problem != "" {
		d.Reason = "refused: " + problem
	}
	if d.Words == "" {
		d.Words = clipWords(x.Description, 300)
	}
	return false, d
}

func clipWords(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func inAny(texts []string, q string) bool {
	for _, t := range texts {
		if ownerwords.In(t, q) {
			return true
		}
	}
	return false
}

// Quote is the model's "quote": a string, or a list of strings (parts of the
// owner's turn, kept in order with an ellipsis between them). Any other value
// is no quote, not a failed fact.
type Quote string

func (q *Quote) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*q = Quote(s)
		return nil
	}
	var parts []string
	if json.Unmarshal(b, &parts) == nil {
		*q = Quote(strings.Join(parts, " ... "))
		return nil
	}
	*q = ""
	return nil
}

// maxQuote bounds the quote a note carries, in bytes: the words that state a
// rule, not the owner's whole turn.
const maxQuote = 700

// Select narrows which transcripts a run reads (tacit dream --idle,
// --session). The zero value reads every transcript with new content.
type Select struct {
	// Only is one transcript (its path) or one Hermes session (its id):
	// nothing else is read and no other cursor moves. A source with no
	// cursors yet is not read at all, so a later full run still records it
	// at its end instead of replaying it.
	Only string
	// Idle, when set, takes a transcript only when it has had no write for
	// that long (the file's mtime; a Hermes session's newest message), or
	// when its new content holds a compaction (digest.Compacted; a Hermes
	// session that ended by compression or a reset, hermes.Continued). Any
	// other transcript waits with its cursor unmoved: the session is still
	// being written, and its turns are still in the agent's context.
	Idle time.Duration
	// Now is the time idleness, and the age of a window for Agent, are
	// measured against.
	Now time.Time
	// Agent is what the run does with a window that holds no turn of the
	// owner's (agentwindows.go); the zero value dreams every window.
	Agent AgentWindows
}

// Window is one digest ready for the model. File is the transcript, or a
// Hermes session's cursor key; Start and End are byte offsets in the file, or
// the message ids the window was read after and up to.
type Window struct {
	File    string
	Session string // the session id the facts name (source.session)
	Repo    string
	// RepoRoot is the main checkout of Repo (repoid), "" when Repo is "".
	RepoRoot string
	// Pending is the checkout the session's cwd is in when that is outside
	// home or in a temp directory (repoid.Elsewhere): it names the window's
	// repository only once enough sessions worked there (the caller counts
	// the evidence, AcceptPending). Until then Repo is what the window's
	// paths say (Fallback). Work is this window's evidence of work in
	// Pending (WorkIn): "edit", "commit" or "".
	Pending  repoid.Repo
	Fallback string
	Work     string
	// WorkRoot is the checkout Work is evidence about: Pending's root, or
	// the cwd's repository's (every root's work is kept: the assessment of a
	// project reads it). WorkFolder is, for a cwd in no repository, the
	// folder of the projects directory it is in, and Work then is the
	// evidence of work in that folder ("" for none).
	WorkRoot   string
	WorkFolder string
	// Checkout is the cwd's checkout when that is no main checkout (a linked
	// worktree, a separate clone), as repoid resolved it: the caller records
	// its relation to the repository (repoid.RecordCheckouts) while it exists.
	Checkout repoid.Repo
	Lines    []string
	Start    int64
	End      int64
	Part     int
	// Why is the reason a selecting run took the transcript: "idle" or
	// "compacted" (Select.Idle), empty otherwise.
	Why string
	// At is when the newest of the window's lines was written (zero when
	// none records it).
	At time.Time
	// Send, when not nil, is what the model is sent in place of Lines: the
	// owner's turns with their context (OwnerTurns), when Select.Agent does
	// not take the window's agent content. Lines stays the whole window, so
	// the window's identity (its completed key) does not depend on the
	// policy.
	Send []string
}

// Sent is what the model is sent for the window.
func (w Window) Sent() []string {
	if w.Send != nil {
		return w.Send
	}
	return w.Lines
}

// Runner is the LLM call; tests substitute it.
type Runner func(ctx context.Context, model, prompt string) (result string, costUSD float64, inTokens int, err error)

// RunCLI uses the configured dream runner, retaining Claude as the default.
func RunCLI(ctx context.Context, model, prompt string) (string, float64, int, error) {
	sid := modelrun.NewSessionID()
	_ = Exclude(sid)
	res, err := modelrun.RunWithFallback(modelrun.WithJSONSchema(ctx, outputSchema), "dream", model, prompt, sid)
	if res.SessionID != "" {
		_ = Exclude(res.SessionID)
	}
	return res.Text, res.CostUSD, res.InputTokens, err
}

// ClaudeCLI is retained for callers built against the original runner API.
func ClaudeCLI(ctx context.Context, model, prompt string) (string, float64, int, error) {
	return RunCLI(ctx, model, prompt)
}

// NewSessionID returns a fresh UUID for a `claude -p --session-id` call
// (modelrun.NewSessionID).
func NewSessionID() string { return modelrun.NewSessionID() }

// LoadState reads the cursor file (missing file = fresh state).
func LoadState(path string) (*State, error) {
	st := &State{Offsets: map[string]int64{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	return st, json.Unmarshal(b, st)
}

// SaveState writes the cursor file atomically.
func SaveState(path string, st *State) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(st, "", " ")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Windows finds files with new bytes since the state and digests the delta,
// and Hermes owner sessions with new messages (hermesWindows). Fresh state
// starts at each file's current end: the dreamer never replays history (the
// import and the eval already covered it). So does a source the state has no
// cursor for yet (State.Sources): one added later is not replayed either.
//
// Cursors are advanced here only for deltas with nothing to extract. For a
// file that yields windows the caller advances the cursor to Window.End
// once every window of that file has been processed, so a capped or failed
// run leaves the delta for the next run instead of losing it. At most
// maxReadBytes per file per run are read; a file that shrank (rotated,
// truncated) restarts from its current end. A complete line longer than
// maxReadBytes is passed over: its cursor moves past it with no window.
func Windows(sources []Source, st *State, fresh bool) ([]Window, error) {
	return WindowsFor(sources, st, fresh, Select{})
}

// WindowsFor is Windows for the transcripts sel takes. A transcript it passes
// over keeps its cursor, so a later run reads the same delta.
func WindowsFor(sources []Source, st *State, fresh bool, sel Select) ([]Window, error) {
	sc, err := ScanWindows(sources, st, fresh, sel)
	return sc.Windows, err
}

// Scan is what ScanWindows found: the windows to dream, and what sel.Agent
// passed over.
type Scan struct {
	Windows []Window
	Passed  Passed
}

// ScanWindows is WindowsFor that also counts what sel.Agent passed over. A
// delta whose every window the policy passes over moves its cursor to its
// end, as a delta with nothing to extract does; the transcript's next bytes
// wait for the next run, like any delta beyond maxReadBytes. So a run that
// finds no window to dream may still leave bytes unread.
func ScanWindows(sources []Source, st *State, fresh bool, sel Select) (Scan, error) {
	home, _ := os.UserHomeDir()
	var ws []Window
	var passed Passed
	now := sel.Now
	if now.IsZero() {
		now = time.Now()
	}
	exclude := modelrun.Excluded()
	// A separate clone of a checkout in the projects directory is that
	// repository (repoid.Mains); scanned once per run.
	mains := repoid.LoadMains(home, repoid.ProjectsDir(home))
	var cursors followed // by session, built when a transcript has no cursor at its path
	packed := map[string]Packed{}
	for _, src := range sources {
		if sel.Only != "" && !st.started(src) {
			continue
		}
		started := st.started(src)
		fresh := fresh || !started
		if !slices.Contains(st.Sources, src.root()) {
			st.Sources = append(st.Sources, src.root())
		}
		if src.Format == digest.Hermes {
			hw, hp := hermesWindows(src.DB, st, fresh, exclude, home, mains, sel, now)
			ws = append(ws, hw...)
			passed.add(hp)
			continue
		}
		// Compressed copies are read since v0.9.11: a dreamer that read only
		// plain transcripts records the copies it has no cursor for at their
		// end, as for a new root, so the history they hold is not replayed.
		freshPacked := fresh || !slices.Contains(st.Sources, src.packedRoot())
		files, _ := transcript.Glob(src.Glob)
		for _, f := range files {
			key := transcript.Plain(f)
			if sel.Only != "" && !sameFile(f, sel.Only) && !sameFile(key, sel.Only) {
				continue
			}
			session := sessionOf(f, src.Format)
			if exclude[transcript.Name(f)] || exclude[session] {
				continue
			}
			info, size, err := st.stat(f, packed)
			if err != nil || info.IsDir() {
				if err != nil && transcript.Compressed(f) {
					fmt.Fprintf(os.Stderr, "dream: %s: %v\n", f, err)
				}
				continue
			}
			off, seen := st.Offsets[key]
			if !seen {
				if cursors == nil {
					cursors = st.sessionCursors(sources)
				}
				off, seen = st.follow(cursors, src.Format, session, key)
			}
			if !seen {
				if fresh || transcript.Compressed(f) && freshPacked {
					st.Offsets[key] = size
					continue
				}
				off = 0
			}
			if size < off {
				st.Offsets[key] = size
				continue
			}
			if seen && size <= off+minNewBytes {
				continue
			}
			fw, fp := fileWindows(f, src.Format, session, off, size, info, st, home, mains, sel, now)
			ws = append(ws, fw...)
			passed.add(fp)
		}
		// A run of one transcript (sel.Only) recorded no other copy.
		if sel.Only == "" && !slices.Contains(st.Sources, src.packedRoot()) {
			st.Sources = append(st.Sources, src.packedRoot())
		}
	}
	if sel.Only == "" {
		// The sizes of the copies this run found; the rest are gone.
		st.Packed = packed
		if len(packed) == 0 {
			st.Packed = nil
		}
	}
	return Scan{Windows: ws, Passed: passed}, nil
}

// fileWindows digests a transcript's delta from off (at most maxReadBytes)
// and names its windows; the ones sel.Agent passes over are counted, not
// returned. size is the transcript's size in JSONL bytes (transcript.Stat:
// a compressed copy's decompressed size); the cursor is keyed by its plain
// path.
func fileWindows(f string, format digest.Format, session string, off, size int64, info os.FileInfo, st *State, home string, mains *repoid.Mains, sel Select, now time.Time) ([]Window, Passed) {
	key := transcript.Plain(f)
	fh, err := transcript.OpenAt(f, off)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dream: %s: %v\n", f, err)
		return nil, Passed{}
	}
	want := size - off
	if want > maxReadBytes {
		want = maxReadBytes
	}
	buf := make([]byte, want)
	n, _ := io.ReadFull(fh, buf)
	fh.Close()
	buf = buf[:n]
	// Only complete lines: cut at the last newline.
	last := strings.LastIndexByte(string(buf), '\n')
	if last < 0 {
		// A full read with no newline is one line longer than
		// maxReadBytes (a pasted blob or a huge tool result). Waiting
		// would wait forever: every run would read the same bytes.
		// Skip past that line once it is complete; a line still being
		// written keeps the cursor.
		if int64(n) == maxReadBytes {
			if next, ok := nextLineStart(f, off+int64(n)); ok {
				// Not when the file was replaced or cut meanwhile: the
				// offset would land inside other content.
				if now, err := transcript.Size(f); err == nil && now >= next {
					fmt.Fprintf(os.Stderr, "dream: %s: a line of %d bytes at %d is longer than the read cap; passed over\n", f, next-off, off)
					st.Offsets[key] = next
					st.Skipped++
				}
			}
		}
		// The rest of the file waits for the next run, like any delta
		// beyond maxReadBytes.
		return nil, Passed{}
	}
	buf = buf[:last+1]
	end := off + int64(len(buf))
	why := ""
	if sel.Idle > 0 {
		switch {
		case sel.Now.Sub(info.ModTime()) >= sel.Idle:
			why = "idle"
		case digest.Compacted(buf, format):
			why = "compacted"
		default:
			return nil, Passed{} // still being written: it waits, cursor unmoved
		}
	}
	// Who typed the session's user turns is read from its start
	// (digest.SessionWho): a delta from the middle of an agent's session
	// holds its prompts as P: lines, never U:.
	start := transcript.Head(f, 256<<10)
	who := digest.SessionWho(key, start, format)
	lines, at := digest.LinesAt(bytes.NewReader(buf), format, who.Owner())
	head := buf
	if format == digest.Pi {
		// pi writes the cwd once, at the start.
		head = start
	}
	if !digest.Interesting(lines) {
		st.Offsets[key] = end
		return nil, Passed{}
	}
	// The cwd's repo, else the repo the window's own paths are in. Per
	// window, not per delta: a long session moves between repos, and
	// measured per delta, many inferred repos were wrong.
	cwdRepo, cwd := repoOf(head, home, mains)
	resolver := repoinfer.NewResolver(home).WithMains(mains)
	all := windowsOf(lines, at, cwdRepo, cwd, home, mains, resolver, func(part int, c []string) Window {
		return Window{File: key, Session: session, Lines: c, Start: off, End: end, Part: part, Why: why}
	})
	kept, passed := sel.Agent.apply(all, now)
	if len(kept) == 0 {
		st.Offsets[key] = end // every window passed over: the delta is done
	}
	return kept, passed
}

// sameFile reports whether two paths name one transcript.
func sameFile(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	return err == nil && os.SameFile(fa, fb)
}

// sessionOf is the session id of a transcript file: its name (less
// .jsonl and a compression ending), less the time pi puts before the id
// (<time>_<id>.jsonl).
func sessionOf(file string, f digest.Format) string {
	name := transcript.Name(file)
	if f == digest.Pi {
		if i := strings.LastIndexByte(name, '_'); i >= 0 {
			return name[i+1:]
		}
	}
	return name
}

// hermesTimeout bounds one run's reads of a Hermes store.
const hermesTimeout = 2 * time.Minute

// hermesWindows digests what is new in the owner sessions of a Hermes store
// (hermes.Session.Owner; automation sessions are never read). A session's
// cursor is the id of its last message read: rows after it are read in id
// order, up to maxReadBytes a run, and a row Hermes wrote again when it
// compacted the conversation is passed over (hermes.Message.Copy), so it is
// not read twice. A seen session waits for minNewBytes of new content, like a
// file. A cursor past the session's last id (the store was restored from a
// backup) restarts at its current end, as a truncated file does. The repo is
// the session's git root or cwd when it records one, else the window's paths.
// A store that cannot be read is reported and left for the next run, its
// cursors unmoved.
//
// The windows sel.Agent passes over are counted, not returned; when it passes
// over all of them the cursor moves past what was read.
func hermesWindows(path string, st *State, fresh bool, exclude map[string]bool, home string, mains *repoid.Mains, sel Select, now time.Time) ([]Window, Passed) {
	ctx, cancel := context.WithTimeout(context.Background(), hermesTimeout)
	defer cancel()
	var passed Passed
	db, err := hermes.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dream: hermes %s: %v\n", path, err)
		return nil, passed
	}
	defer db.Close()
	sessions, err := db.Sessions(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dream: hermes %s: %v\n", path, err)
		return nil, passed
	}
	last, err := db.LastIDs(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dream: hermes %s: %v\n", path, err)
		return nil, passed
	}
	var lastAt map[string]float64
	if sel.Idle > 0 {
		if lastAt, err = db.LastTimes(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "dream: hermes %s: %v\n", path, err)
			return nil, passed
		}
	}
	resolver := repoinfer.NewResolver(home).WithMains(mains)
	var ws []Window
	for _, s := range sessions {
		lastID, ok := last[s.ID]
		if !s.Owner || !ok || exclude[s.ID] {
			continue
		}
		if sel.Only != "" && s.ID != sel.Only && hermes.Key(path, s.ID) != sel.Only {
			continue
		}
		key := hermes.Key(path, s.ID)
		after, seen := st.Offsets[key]
		if !seen && fresh {
			st.Offsets[key] = lastID
			continue
		}
		if lastID < after {
			// The store went back (restored from a backup): restart from its
			// current end, as a truncated file does.
			st.Offsets[key] = lastID
			continue
		}
		if lastID == after {
			continue
		}
		if seen {
			if n, err := db.NewBytes(ctx, s.ID, after); err != nil || n <= minNewBytes {
				continue
			}
		}
		why := ""
		if sel.Idle > 0 {
			switch {
			case float64(sel.Now.Unix())-lastAt[s.ID] >= sel.Idle.Seconds():
				why = "idle"
			case hermes.Continued(s.EndReason):
				why = "compacted"
			default:
				continue
			}
		}
		ms, err := db.Messages(ctx, s.ID, after, maxReadBytes)
		if err != nil {
			fmt.Fprintf(os.Stderr, "dream: hermes %s: session %s: %v\n", path, s.ID, err)
			continue
		}
		if len(ms) == 0 {
			continue
		}
		end := ms[len(ms)-1].ID
		var lines []string
		var at []time.Time
		for _, m := range ms {
			if m.Copy {
				continue
			}
			ls := digest.HermesLines([]digest.HermesMessage{m.HermesMessage})
			t := hermesTime(m.Timestamp)
			for range ls {
				at = append(at, t)
			}
			lines = append(lines, ls...)
		}
		if !digest.Interesting(lines) {
			st.Offsets[key] = end
			continue
		}
		var cwdRepo repoid.Repo
		cwd := ""
		for _, d := range []string{s.RepoRoot, s.Cwd} {
			if d != "" {
				ids := repoid.NewCache(home)
				ids.Mains = mains
				cwdRepo, _ = ids.Of(cwdPath(d))
				break
			}
		}
		if s.Cwd != "" {
			cwd = cwdPath(s.Cwd)
		}
		all := windowsOf(lines, at, cwdRepo, cwd, home, mains, resolver, func(part int, c []string) Window {
			return Window{File: key, Session: s.ID, Lines: c, Start: after, End: end, Part: part, Why: why}
		})
		kept, p := sel.Agent.apply(all, now)
		passed.add(p)
		if len(kept) == 0 {
			st.Offsets[key] = end // every window passed over
		}
		ws = append(ws, kept...)
	}
	return ws, passed
}

// hermesTime is a Hermes row's time (Unix seconds); zero when it has none.
func hermesTime(ts float64) time.Time {
	if ts <= 0 {
		return time.Time{}
	}
	sec := int64(ts)
	return time.Unix(sec, int64((ts-float64(sec))*1e9))
}

// nextLineStart is the offset just after the first newline at or after from,
// and false when the file ends first (the line is not complete yet).
func nextLineStart(path string, from int64) (int64, bool) {
	fh, err := transcript.OpenAt(path, from)
	if err != nil {
		return 0, false
	}
	defer fh.Close()
	buf := make([]byte, 1<<20)
	pos := from
	for {
		n, err := fh.Read(buf)
		if i := bytes.IndexByte(buf[:n], '\n'); i >= 0 {
			return pos + int64(i) + 1, true
		}
		pos += int64(n)
		if err != nil {
			return 0, false
		}
	}
}

// Exclude appends session ids to the list of sessions the dreamer never
// reads: eval harness runs and tacit's own model calls
// (modelrun.ExcludeFile, ~/.local/state/tacit/dream-exclude, one id per
// line; modelrun.Exclude). Without it the dreamer mined the A/B battery's
// synthetic prompts as if they were the owner's work.
func Exclude(ids ...string) error { return modelrun.Exclude(ids...) }

var cwdRx = regexp.MustCompile(`"cwd":"([^"]+)"`)

// repoOf is the repository of the first cwd in the delta (both formats
// carry one), as repoid names it: a real git root, a worktree for its main
// repository. A transcript without a cwd gets none from here (the window's
// paths decide, windowRepo): its folder's name is a lossy spelling of a
// path, not a git root. A separate clone of a main checkout (mains) is
// that checkout's repository. cwd is the cwd read.
func repoOf(raw []byte, home string, mains *repoid.Mains) (r repoid.Repo, cwd string) {
	if m := cwdRx.FindSubmatch(raw); m != nil {
		ids := repoid.NewCache(home)
		ids.Mains = mains
		cwd = cwdPath(string(m[1]))
		var ok bool
		if r, ok = ids.Of(cwd); !ok {
			r = recordedRepo(ids, cwd, home)
		}
		return r, cwd
	}
	return repoid.Repo{}, ""
}

// recordedRepo is the repository of a cwd whose checkout is gone (a removed
// worktree or clone) by the relation the registry recorded while it existed
// (repoid.RecordCheckouts): its main checkout, while that is still one.
func recordedRepo(ids *repoid.Cache, cwd, home string) repoid.Repo {
	_, c, ok := repoid.RecordedCheckout(filepath.Join(platform.StateDir(home), repoid.RegistryFile), repoid.RealPath(cwd))
	if !ok || c.Main == "" {
		return repoid.Repo{}
	}
	r, ok := ids.Of(c.Main)
	if !ok || r.Slug() != c.Repo {
		return repoid.Repo{}
	}
	return r
}

// windowsOf chunks a delta's lines into windows (mk fills each one's
// transcript fields) and names each window's repository: the cwd's
// checkout, else what the window's paths say. A cwd checkout outside home or
// in a temp directory is only Pending, with this window's evidence of work
// in it, until the caller accepts it (AcceptPending).
//
// at holds, per line, when it was written (digest.LinesAt; nil or a zero time
// for none): each window's At is its newest line's.
func windowsOf(lines []string, at []time.Time, cwdRepo repoid.Repo, cwd, home string, mains *repoid.Mains, resolver *repoinfer.Resolver, mk func(part int, c []string) Window) []Window {
	var pending repoid.Repo
	if cwdRepo.Root != "" && repoid.Elsewhere(cwdRepo.Root, home, mains) {
		pending, cwdRepo = cwdRepo, repoid.Repo{}
	}
	folder := ""
	if cwdRepo.Root == "" && pending.Root == "" {
		folder = ProjectFolder(cwdPath(cwd), home)
	}
	var out []Window
	for part, r := range chunkRanges(lines, maxWindowChars) {
		c := lines[r[0]:r[1]:r[1]]
		w := mk(part, c)
		for i := r[0]; i < r[1] && i < len(at); i++ {
			if at[i].After(w.At) {
				w.At = at[i]
			}
		}
		for _, r := range []repoid.Repo{cwdRepo, pending} {
			if r.Root != "" && (r.Clone != "" || r.Worktree != r.Root || !mains.In(r.Root)) {
				w.Checkout = r
			}
		}
		w.Repo, w.RepoRoot = cwdRepo.Name, cwdRepo.Root
		if w.Repo == "" {
			w.Repo = windowRepo(c, resolver)
			w.RepoRoot = resolver.Root(w.Repo)
		}
		switch {
		case pending.Root != "":
			w.Pending, w.Fallback = pending, w.Repo
			w.Work, w.WorkRoot = WorkIn(c, pending.Worktree, cwd, home), pending.Root
		case cwdRepo.Root != "":
			w.Work, w.WorkRoot = WorkIn(c, cwdRepo.Worktree, cwd, home), cwdRepo.Root
		case folder != "":
			w.Work, w.WorkFolder = WorkIn(c, folder, cwd, home), folder
		}
		out = append(out, w)
	}
	return out
}

// ProjectFolder is the folder of the projects directory (repoid.ProjectsDir)
// that cwd is in, when cwd is in no repository: the projects directory's
// child on the way, not a dot directory; "" otherwise. A non-git project
// folder's evidence of work is kept by it.
func ProjectFolder(cwd, home string) string {
	if cwd == "" || home == "" {
		return ""
	}
	proj := repoid.RealPath(repoid.ProjectsDir(home))
	c := repoid.RealPath(cwd)
	rel, err := filepath.Rel(proj, c)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	first := strings.Split(rel, string(filepath.Separator))[0]
	if first == "" || strings.HasPrefix(first, ".") {
		return ""
	}
	return filepath.Join(proj, first)
}

// AcceptPending names each window's repository for its Pending checkout
// when work counts that checkout's sessions (by root) at
// repoid.MinWorkSessions or more; the windows of this run count too.
func AcceptPending(ws []Window, work map[string]map[string]bool) {
	for _, w := range ws {
		if w.Pending.Root != "" && w.Work != "" {
			if work[w.Pending.Root] == nil {
				work[w.Pending.Root] = map[string]bool{}
			}
			work[w.Pending.Root][w.Session] = true
		}
	}
	for i, w := range ws {
		if w.Pending.Root != "" && len(work[w.Pending.Root]) >= repoid.MinWorkSessions {
			ws[i].Repo, ws[i].RepoRoot = w.Pending.Name, w.Pending.Root
		}
	}
}

// cwdPath is a recorded cwd as a path: Codex writes some as a file URL
// (file:///home/u/work), which named a repo after the URL's last part.
func cwdPath(cwd string) string {
	rest, ok := strings.CutPrefix(cwd, "file://")
	if !ok {
		return cwd
	}
	if u, err := url.PathUnescape(rest); err == nil {
		return u
	}
	return rest
}

// windowRepo is the repo a window is about when the session's cwd names
// none: the repo most of its paths are in (cd, git -C, file paths in tool
// calls), and "" when its paths are spread over repos or in none.
func windowRepo(lines []string, r *repoinfer.Resolver) string {
	return repoinfer.Dominant(r.Counts(lines, 1, nil), 3)
}

// RepoOfCwd is the repo a session's working directory belongs to, as
// repoid names it ("" for none). A cwd recorded as a file:// URL is read as
// its path.
func RepoOfCwd(cwd, home string) string { return repoid.Name(cwdPath(cwd), home) }

func chunk(lines []string, maxChars int) [][]string {
	var out [][]string
	for _, r := range chunkRanges(lines, maxChars) {
		out = append(out, lines[r[0]:r[1]])
	}
	return out
}

// chunkRanges is chunk as index ranges [start, end) of lines.
func chunkRanges(lines []string, maxChars int) [][2]int {
	var out [][2]int
	start, size := 0, 0
	for i, l := range lines {
		w := windowWidth(l)
		if size+w+1 > maxChars && i > start {
			out = append(out, [2]int{start, i})
			start, size = i, 0
		}
		size += w + 1
	}
	if start < len(lines) {
		out = append(out, [2]int{start, len(lines)})
	}
	return out
}

// windowWidth is a line's size in a window: lines are uncut until Finish, so
// it is sized by what the model will see.
func windowWidth(l string) int { return min(len(l), digest.Width(l)) }

var arrayRx = regexp.MustCompile(`(?s)\[.*\]`)

// ParseChecked distinguishes a valid empty result from a failed extraction.
// A malformed answer must not advance the transcript cursor.
func ParseChecked(result string) ([]Extracted, error) {
	text := strings.TrimSpace(result)
	if strings.HasPrefix(text, "```json") {
		text = strings.TrimSpace(strings.TrimPrefix(text, "```json"))
		text = strings.TrimSpace(strings.TrimSuffix(text, "```"))
	} else if strings.HasPrefix(text, "```") {
		text = strings.TrimSpace(strings.TrimPrefix(text, "```"))
		text = strings.TrimSpace(strings.TrimSuffix(text, "```"))
	}
	if strings.HasPrefix(text, "{") {
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal([]byte(text), &envelope); err != nil {
			return nil, fmt.Errorf("invalid dreamer JSON: %w", err)
		}
		if len(envelope) != 1 || envelope["facts"] == nil {
			return nil, fmt.Errorf("dreamer did not return a facts array")
		}
		text = strings.TrimSpace(string(envelope["facts"]))
	}
	if !strings.HasPrefix(text, "[") {
		return nil, fmt.Errorf("dreamer did not return a JSON array")
	}
	var rows []Extracted
	if err := json.Unmarshal([]byte(text), &rows); err != nil {
		return nil, fmt.Errorf("invalid dreamer JSON: %w", err)
	}
	for _, row := range rows {
		if strings.TrimSpace(row.Description) == "" {
			return nil, fmt.Errorf("dreamer returned a fact without a description")
		}
	}
	return Parse(text), nil
}

// Parse extracts the JSON array from a model answer.
func Parse(result string) []Extracted {
	m := arrayRx.FindString(strings.TrimSpace(result))
	if m == "" {
		return nil
	}
	var raws []json.RawMessage
	if json.Unmarshal([]byte(m), &raws) != nil {
		return nil
	}
	var out []Extracted
	for _, r := range raws {
		var x Extracted
		if json.Unmarshal(r, &x) == nil && strings.TrimSpace(x.Description) != "" {
			out = append(out, x)
		}
	}
	return out
}

// body is the note text: the description, and for an owner ruling the words
// the model copied from the owner's turn, which curation checks against their
// turns before it keeps the ruling as theirs (curate.ApplyEvidence). Until
// then they are the model's claim, so the note says where they are from, not
// whose they are. A double quote inside them becomes ” so the quote stays one
// quote; a long one is cut at a word, with an ellipsis.
func body(x Extracted, kind string) string {
	b := strings.Join(strings.Fields(x.Description), " ")
	q := strings.Join(strings.Fields(strings.ReplaceAll(string(x.Quote), `"`, "”")), " ")
	if len(q) > maxQuote {
		cut := strings.LastIndexByte(q[:maxQuote], ' ')
		if cut <= 0 {
			cut = maxQuote
		}
		q = strings.ToValidUTF8(q[:cut], "") + " ..."
	}
	if (kind == "user" || kind == kindStatement) && q != "" {
		b += ` Quoted from the session: "` + q + `"`
	}
	if x.AgentWords && q != "" {
		b += ` Another agent's prompt in the session said: "` + q + `"`
	}
	if kind == kindStatement && q != "" {
		// Said once: the curator keeps it as a statement of theirs, not a rule.
		b += " Said once"
		if c := strings.Join(strings.Fields(x.Context), " "); c != "" {
			b += "; context: " + strings.TrimRight(c, ".")
		}
		b += "."
	}
	return b + "\n"
}

// kindStatement is the model's kind for the owner's words said once
// (SPEC.md, "Statements"). The note is kind user (their words, which the
// curator checks); its text marks them "Said once", and the curator keeps
// them as a statement.
const kindStatement = "statement"

// ToFact converts an extraction into an inbox fact for writer dreamer@host.
func ToFact(x Extracted, repo, host, session string) *fact.Fact {
	typ := x.Type
	if !fact.Types[typ] || typ == "unclassified" {
		typ = "unclassified"
	}
	kind := x.Kind
	said := kind == kindStatement && strings.TrimSpace(string(x.Quote)) != ""
	if said {
		kind = "user"
	} else if !fact.SourceKinds[kind] || kind == "derived" {
		kind = "observed"
	}
	scope := "user"
	switch {
	case typ == "machine_fact" || len(strList(x.AppliesTo["hosts"])) > 0 && repo == "":
		scope = "machines/" + host
	case repo != "" && fact.IsSlug(strings.ToLower(repo)):
		scope = "projects/" + strings.ToLower(repo)
	}
	desc := fact.ClipDescription(x.Description, 200)
	name := importer.Slug(desc, 60)
	return &fact.Fact{
		Meta: fact.Meta{
			Name:        name,
			Description: desc,
			Type:        typ,
			Scope:       scope,
			AppliesTo: fact.AppliesTo{
				Paths:    strList(x.AppliesTo["paths"]),
				Repos:    strList(x.AppliesTo["repos"]),
				Hosts:    strList(x.AppliesTo["hosts"]),
				Commands: strList(x.AppliesTo["commands"]),
			},
			Source:  fact.Source{Harness: "dreamer", Host: host, Kind: kind, Session: session},
			Created: time.Now().Format("2006-01-02"),
		},
		Body: body(x, map[bool]string{true: kindStatement, false: kind}[said]),
	}
}

func strList(xs []any) []string {
	var out []string
	for _, x := range xs {
		if s, ok := x.(string); ok && s != "" {
			out = append(out, s)
		}
		if len(out) == 8 {
			break
		}
	}
	sort.Strings(out)
	return out
}

// Meter is one run's accounting line.
type Meter struct {
	PauseReason      string          `json:"pause_reason,omitempty"`
	RemainingWindows int             `json:"remaining_windows,omitempty"`
	ModelCalls       *modelrun.Trace `json:"model_calls,omitempty"`
	At               string          `json:"at"`
	Windows          int             `json:"windows"`
	Calls            int             `json:"calls"`
	Facts            int             `json:"facts"`
	InTokens         int             `json:"in_tokens"`
	CostUSD          float64         `json:"cost_usd"`
	Errors           int             `json:"errors"`
	// Mode is how the run chose its transcripts: "idle" (--idle), "session"
	// (--session), empty for every transcript with new content.
	Mode string `json:"mode,omitempty"`
	// Local is a run of the local layer: the notes went to this machine's
	// overlay, not the store.
	Local bool `json:"local,omitempty"`
	// AgentWindows is the policy for windows with no turn of the owner's,
	// when it is not the default (all); AgentPassed the windows it passed
	// over and OwnerCut the owner's windows it cut to their turns, with the
	// characters each left out.
	AgentWindows     string `json:"agent_windows,omitempty"`
	AgentPassed      int    `json:"agent_windows_passed,omitempty"`
	AgentPassedChars int    `json:"agent_windows_passed_chars,omitempty"`
	OwnerCut         int    `json:"owner_windows_cut,omitempty"`
	OwnerCutChars    int    `json:"owner_windows_cut_chars,omitempty"`
	// OwnerDropped is the owner's remarks the relevance gate did not write
	// down (Gate), each with why: kept only here and in the transcript.
	OwnerDropped []Dropped `json:"owner_remarks_dropped,omitempty"`
	// BackedOff is a run that paid for no window: earlier runs failed on the
	// route and its probe failed again. Probes is the route probes the run
	// sent; their cost is in CostUSD.
	BackedOff bool `json:"backed_off,omitempty"`
	Probes    int  `json:"probes,omitempty"`
	// Held and HeldChars are the facts already held that the prompts listed
	// (Held), and their characters, over every window of the run.
	Held      int `json:"held,omitempty"`
	HeldChars int `json:"held_chars,omitempty"`
}
