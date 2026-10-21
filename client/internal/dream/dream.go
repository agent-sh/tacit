// Package dream is the dreamer: it reads what changed in the agent
// transcripts since its last run, digests it, asks a model for the durable
// facts nobody wrote down, and files them in inbox/dreamer@<host>/.
//
// Measured in eval/VALUE.md: Opus 5.5 states 78 percent of the truth facts
// in their windows against Sonnet's 72, for 2.3 times the cost.
package dream

import (
	"bytes"
	"context"
	"crypto/rand"
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
	"github.com/agent-sh/tacit/client/internal/hermes"
	"github.com/agent-sh/tacit/client/internal/importer"
	"github.com/agent-sh/tacit/client/internal/modelrun"
	"github.com/agent-sh/tacit/client/internal/repoid"
	"github.com/agent-sh/tacit/client/internal/repoinfer"
)

// DefaultModel is the extraction model: $TACIT_DREAM_MODEL (a provider's
// full model id, for example), else the Claude CLI alias opus. --model on
// tacit dream wins. Measured 2026-10-18 on 89 transcript windows (53 owner
// rulings), prompt v4, Sonnet 5.5 judging: the truth fact was written down
// in 64% of windows by Haiku 5.5, 72% by Sonnet 5.5 and 78% by Opus 5.5, at
// about 0.01, 0.12 and 0.27 dollars a 60k-character window.
var DefaultModel = modelrun.Model("dream", "opus")

const (
	maxWindowChars = 60_000  // one model call per window
	minNewBytes    = 4_000   // smaller deltas wait for the next run
	maxReadBytes   = 4 << 20 // per file per run; the rest waits (cursor stays)
)

// Prompt is v5. Versions (measurements in eval/VALUE.md):
//   - v4 (2026-10-11, eval/dream/dream_recall.py): v2 plus one rule, skip what
//     holds only inside the task in progress, never a failure and its fix or a
//     rule of the owner. On two samples of 30 windows it cut the facts 25 and
//     23 percent and the task-only ones 90 and 81 percent; recall on the 80
//     truth gaps 58 and 58 at least partial against v2's 60 and 61.
//   - v5 (2026-10-18): v4 plus the owner's exact words in "quote" for a ruling
//     ("Dreamed rulings keep the owner's words").
const Prompt = `You are the dreamer of tacit, a fact store shared by coding agents on one owner's machines.
Below is a digest of part of an agent session (U: owner, H: harness-injected text, A: agent, T: tool call, E: tool error).
Your job is to write down what nobody wrote down. Extract EVERY durable fact another agent would need next time, in these classes:
1. Every failed command or tool call (E: lines, and A: lines that report an error) together with what fixed it or what to do instead. Each distinct failure is one fact: name the command, the exact error text, the host, and the working alternative. These are the most valuable facts; do not drop them as noise.
2. Owner rulings and standards, from U: lines only, with the date if visible. For each one, copy the owner's exact words from the U: line into "quote": verbatim, typos included, at least the words that state the rule. Without the owner's own words a ruling cannot be told from an agent's guess.
3. Machine facts: paths that exist or are gone, tools and their quirks, services, ports, hosts, aliases, what is dead on this box.
4. Past mistakes: a wrong assumption and its correction.
5. Design decisions with their reason.
Skip anything derivable from the code or from git log, and plain narrative of what the agent did.
Skip facts that hold only inside the task in progress: the status of one PR, review round, rebase or run; where one task's worktree, scratch files, contract, receipts or logs are; an instruction addressed to one task id, round or worktree. A failure and what fixed it, and a rule or preference of the owner, are never task-only, even when they came up inside one task. Keep what the next task, in another session, would act on.
Each fact: one or two sentences another agent can act on, with the concrete path, command, host or number. Mark the source kind: user (the owner said it), observed (the agent found it out).
Output ONLY a JSON array: [{"description": "...", "type": "user_standard|machine_fact|past_mistake|design_decision|structure|history", "kind": "user|observed", "quote": "<kind user only: the owner's exact words>", "applies_to": {"paths": [], "repos": [], "hosts": [], "commands": []}}]

Repo: %s  Host: %s
DIGEST:
%s
`

// State is the per-file read cursor, persisted between runs. A Hermes
// session's cursor is in Offsets too, under hermes.Key ("hermes:<db>#<session
// id>"), and holds the id of the last message read, not a byte offset.
type State struct {
	Offsets   map[string]int64    `json:"offsets"`
	Skipped   int                 `json:"skipped_long_lines,omitempty"` // lines longer than maxReadBytes passed over
	LastRun   string              `json:"last_run"`
	Completed map[string][]string `json:"completed_windows,omitempty"`
	// Sources are the transcript roots (a glob, a Hermes store) whose cursors
	// were recorded: a root not listed, with no cursor yet, starts at its end,
	// so a source added to a running dreamer is not replayed from the start.
	Sources []string `json:"sources,omitempty"`
	// FailedAt is, per transcript (a cursor key), when a window of it last
	// failed (a model, redaction or push error; RFC 3339). An idle or session
	// run passes over a transcript that failed within FailureBackoff, so a
	// window the model cannot answer is not sent again every 10 minutes; the
	// full run retries it. Cleared when the transcript's cursor moves.
	FailedAt map[string]string `json:"failed_at,omitempty"`
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

// DefaultSources on this machine: Claude Code projects, Codex sessions, pi
// sessions (digest.PiSessionGlob) and, when there is one, the Hermes store
// (hermes.DefaultDB).
func DefaultSources(home string) []Source {
	srcs := []Source{
		{Glob: filepath.Join(home, ".claude", "projects", "*", "*.jsonl"), Format: digest.Claude},
		{Glob: filepath.Join(home, ".codex", "sessions", "*", "*", "*", "*.jsonl"), Format: digest.Codex},
		{Glob: filepath.Join(home, ".codex", "sessions", "*", "*.jsonl"), Format: digest.Codex},
		{Glob: digest.PiSessionGlob(home), Format: digest.Pi},
	}
	if db := hermes.DefaultDB(home); db != "" {
		if abs, err := filepath.Abs(db); err == nil {
			db = abs
		}
		srcs = append(srcs, Source{DB: db, Format: digest.Hermes})
	}
	return srcs
}

// Extracted is one fact as the model returned it.
type Extracted struct {
	Description string           `json:"description"`
	Type        string           `json:"type"`
	Kind        string           `json:"kind"`
	Quote       Quote            `json:"quote,omitempty"` // kind user: the owner's exact words
	AppliesTo   map[string][]any `json:"applies_to"`
	Extra       map[string]any   `json:"-"`
	Session     string           `json:"-"`
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
	// Now is the time idleness is measured against.
	Now time.Time
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
	Lines      []string
	Start      int64
	End        int64
	Part       int
	// Why is the reason a selecting run took the transcript: "idle" or
	// "compacted" (Select.Idle), empty otherwise.
	Why string
}

// Runner is the LLM call; tests substitute it.
type Runner func(ctx context.Context, model, prompt string) (result string, costUSD float64, inTokens int, err error)

// RunCLI uses the configured dream runner, retaining Claude as the default.
func RunCLI(ctx context.Context, model, prompt string) (string, float64, int, error) {
	sid := newUUID()
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

// NewSessionID returns a fresh UUID for a `claude -p --session-id` call.
func NewSessionID() string { return newUUID() }

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

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
	home, _ := os.UserHomeDir()
	var ws []Window
	exclude := loadExclude()
	// A separate clone of a checkout in the projects directory is that
	// repository (repoid.Mains); scanned once per run.
	mains := repoid.LoadMains(home, repoid.ProjectsDir(home))
	for _, src := range sources {
		if sel.Only != "" && !st.started(src) {
			continue
		}
		fresh := fresh || !st.started(src)
		if !slices.Contains(st.Sources, src.root()) {
			st.Sources = append(st.Sources, src.root())
		}
		if src.Format == digest.Hermes {
			ws = append(ws, hermesWindows(src.DB, st, fresh, exclude, home, mains, sel)...)
			continue
		}
		files, _ := filepath.Glob(src.Glob)
		for _, f := range files {
			if sel.Only != "" && !sameFile(f, sel.Only) {
				continue
			}
			session := sessionOf(f, src.Format)
			if exclude[strings.TrimSuffix(filepath.Base(f), ".jsonl")] || exclude[session] {
				continue
			}
			info, err := os.Stat(f)
			if err != nil || info.IsDir() {
				continue
			}
			off, seen := st.Offsets[f]
			if !seen {
				if fresh {
					st.Offsets[f] = info.Size()
					continue
				}
				off = 0
			}
			if info.Size() < off {
				st.Offsets[f] = info.Size()
				continue
			}
			if seen && info.Size() <= off+minNewBytes {
				continue
			}
			fh, err := os.Open(f)
			if err != nil {
				continue
			}
			if _, err := fh.Seek(off, 0); err != nil {
				fh.Close()
				continue
			}
			want := info.Size() - off
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
						if fi, err := os.Stat(f); err == nil && fi.Size() >= next {
							fmt.Fprintf(os.Stderr, "dream: %s: a line of %d bytes at %d is longer than the read cap; passed over\n", f, next-off, off)
							st.Offsets[f] = next
							st.Skipped++
						}
					}
				}
				// The rest of the file waits for the next run, like any delta
				// beyond maxReadBytes.
				continue
			}
			buf = buf[:last+1]
			end := off + int64(len(buf))
			why := ""
			if sel.Idle > 0 {
				switch {
				case sel.Now.Sub(info.ModTime()) >= sel.Idle:
					why = "idle"
				case digest.Compacted(buf, src.Format):
					why = "compacted"
				default:
					continue // still being written: it waits, cursor unmoved
				}
			}
			var lines []string
			head := buf
			if src.Format == digest.Pi {
				// pi writes the cwd and the first turns once, at the start: a
				// program's session is told from them (digest.PiHeadless).
				head = readHead(f)
				lines = digest.PiLines(bytes.NewReader(buf), digest.PiHeadless(head))
			} else {
				lines = digest.Lines(bytes.NewReader(buf), src.Format)
			}
			if !digest.Interesting(lines) {
				st.Offsets[f] = end
				continue
			}
			// The cwd's repo, else the repo the window's own paths are in. Per
			// window, not per delta: a long session moves between repos, and
			// measured per delta, 10 of 40 inferred repos were wrong.
			cwdRepo, cwd := repoOf(head, home, mains)
			resolver := repoinfer.NewResolver(home).WithMains(mains)
			ws = append(ws, windowsOf(lines, cwdRepo, cwd, home, mains, resolver, func(part int, c []string) Window {
				return Window{File: f, Session: session, Lines: c, Start: off, End: end, Part: part, Why: why}
			})...)
		}
	}
	return ws, nil
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

// sessionOf is the session id of a transcript file: its name, less the time
// pi puts before the id (<time>_<id>.jsonl).
func sessionOf(file string, f digest.Format) string {
	name := strings.TrimSuffix(filepath.Base(file), ".jsonl")
	if f == digest.Pi {
		if i := strings.LastIndexByte(name, '_'); i >= 0 {
			return name[i+1:]
		}
	}
	return name
}

// readHead is a transcript's first 256 KiB.
func readHead(path string) []byte {
	fh, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer fh.Close()
	b := make([]byte, 256<<10)
	n, _ := io.ReadFull(fh, b)
	return b[:n]
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
func hermesWindows(path string, st *State, fresh bool, exclude map[string]bool, home string, mains *repoid.Mains, sel Select) []Window {
	ctx, cancel := context.WithTimeout(context.Background(), hermesTimeout)
	defer cancel()
	db, err := hermes.Open(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dream: hermes %s: %v\n", path, err)
		return nil
	}
	defer db.Close()
	sessions, err := db.Sessions(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dream: hermes %s: %v\n", path, err)
		return nil
	}
	last, err := db.LastIDs(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dream: hermes %s: %v\n", path, err)
		return nil
	}
	var lastAt map[string]float64
	if sel.Idle > 0 {
		if lastAt, err = db.LastTimes(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "dream: hermes %s: %v\n", path, err)
			return nil
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
		rows := make([]digest.HermesMessage, 0, len(ms))
		for _, m := range ms {
			if !m.Copy {
				rows = append(rows, m.HermesMessage)
			}
		}
		lines := digest.HermesLines(rows)
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
		ws = append(ws, windowsOf(lines, cwdRepo, cwd, home, mains, resolver, func(part int, c []string) Window {
			return Window{File: key, Session: s.ID, Lines: c, Start: after, End: end, Part: part, Why: why}
		})...)
	}
	return ws
}

// nextLineStart is the offset just after the first newline at or after from,
// and false when the file ends first (the line is not complete yet).
func nextLineStart(path string, from int64) (int64, bool) {
	fh, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer fh.Close()
	if _, err := fh.Seek(from, 0); err != nil {
		return 0, false
	}
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

// loadExclude reads session ids the dreamer must never read: eval harness
// runs and tacit's own model calls (~/.local/state/tacit/dream-exclude,
// one id per line). Without it the dreamer mined the A/B battery's
// synthetic prompts as if they were the owner's work.
func loadExclude() map[string]bool {
	out := map[string]bool{}
	home, _ := os.UserHomeDir()
	b, err := os.ReadFile(filepath.Join(platform.StateDir(home), "dream-exclude"))
	if err != nil {
		return out
	}
	for _, l := range strings.Fields(string(b)) {
		out[l] = true
	}
	return out
}

// Exclude appends session ids to the exclude list.
func Exclude(ids ...string) error {
	home, _ := os.UserHomeDir()
	p := filepath.Join(platform.StateDir(home), "dream-exclude")
	os.MkdirAll(filepath.Dir(p), 0o700)
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, id := range ids {
		fmt.Fprintln(f, id)
	}
	return nil
}

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
		r, _ = ids.Of(cwd)
		return r, cwd
	}
	return repoid.Repo{}, ""
}

// windowsOf chunks a delta's lines into windows (mk fills each one's
// transcript fields) and names each window's repository: the cwd's
// checkout, else what the window's paths say. A cwd checkout outside home or
// in a temp directory is only Pending, with this window's evidence of work
// in it, until the caller accepts it (AcceptPending).
func windowsOf(lines []string, cwdRepo repoid.Repo, cwd, home string, mains *repoid.Mains, resolver *repoinfer.Resolver, mk func(part int, c []string) Window) []Window {
	var pending repoid.Repo
	if cwdRepo.Root != "" && repoid.Elsewhere(cwdRepo.Root, home, mains) {
		pending, cwdRepo = cwdRepo, repoid.Repo{}
	}
	folder := ""
	if cwdRepo.Root == "" && pending.Root == "" {
		folder = ProjectFolder(cwdPath(cwd), home)
	}
	var out []Window
	for part, c := range chunk(lines, maxWindowChars) {
		w := mk(part, c)
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
	var cur []string
	size := 0
	for _, l := range lines {
		// Lines are uncut until Finish; size by what the model will see.
		w := min(len(l), digest.Width(l))
		if size+w+1 > maxChars && len(cur) > 0 {
			out = append(out, cur)
			cur, size = nil, 0
		}
		cur = append(cur, l)
		size += w + 1
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

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
	if kind == "user" && q != "" {
		b += ` Quoted from the session: "` + q + `"`
	}
	return b + "\n"
}

// ToFact converts an extraction into an inbox fact for writer dreamer@host.
func ToFact(x Extracted, repo, host, session string) *fact.Fact {
	typ := x.Type
	if !fact.Types[typ] || typ == "unclassified" {
		typ = "unclassified"
	}
	kind := x.Kind
	if !fact.SourceKinds[kind] || kind == "derived" {
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
		Body: body(x, kind),
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
}
