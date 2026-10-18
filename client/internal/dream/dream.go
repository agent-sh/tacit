// Package dream is the dreamer: it reads what changed in the agent
// transcripts since its last run, digests it, asks a small model for the
// durable facts nobody wrote down, and files them in inbox/dreamer@<host>/.
//
// Measured in eval/VALUE.md: Haiku 5.5 with the failed-command prompt
// recovers 75 percent of never-written facts at least partially for about
// two cents per session window; Opus adds little at 26x the cost.
package dream

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/importer"
	"github.com/agent-sh/tacit/client/internal/modelrun"
	"github.com/agent-sh/tacit/client/internal/repoinfer"
)

// DefaultModel is the extraction model: $TACIT_DREAM_MODEL (a provider's
// full model id, for example), else the Claude CLI alias haiku. --model on
// tacit dream wins.
var DefaultModel = modelrun.Model("dream", "haiku")

const (
	maxWindowChars = 60_000  // one model call per window
	minNewBytes    = 4_000   // smaller deltas wait for the next run
	maxReadBytes   = 4 << 20 // per file per run; the rest waits (cursor stays)
)

// Prompt is v4 of eval/dream/dream_recall.py (2026-10-11): the measured v2
// plus one rule, skip what holds only inside the task in progress, never a
// failure and its fix or a rule of the owner. On two samples of 30 transcript
// windows (Haiku 5.5, labels by Sonnet 5.5) it cut the facts 25 and 23
// percent and the task-only ones 90 and 81 percent, durable facts 77 to 71
// and 151 to 165. Recall on the 80 truth gaps, two runs each: v2 60 and 61
// at least partial, v4 58 and 58 (eval/VALUE.md).
const Prompt = `You are the dreamer of tacit, a fact store shared by coding agents on one owner's machines.
Below is a digest of part of an agent session (U: owner, H: harness-injected text, A: agent, T: tool call, E: tool error).
Your job is to write down what nobody wrote down. Extract EVERY durable fact another agent would need next time, in these classes:
1. Every failed command or tool call (E: lines, and A: lines that report an error) together with what fixed it or what to do instead. Each distinct failure is one fact: name the command, the exact error text, the host, and the working alternative. These are the most valuable facts; do not drop them as noise.
2. Owner rulings and standards, from U: lines only, quoted or closely paraphrased, with the date if visible.
3. Machine facts: paths that exist or are gone, tools and their quirks, services, ports, hosts, aliases, what is dead on this box.
4. Past mistakes: a wrong assumption and its correction.
5. Design decisions with their reason.
Skip anything derivable from the code or from git log, and plain narrative of what the agent did.
Skip facts that hold only inside the task in progress: the status of one PR, review round, rebase or run; where one task's worktree, scratch files, contract, receipts or logs are; an instruction addressed to one task id, round or worktree. A failure and what fixed it, and a rule or preference of the owner, are never task-only, even when they came up inside one task. Keep what the next task, in another session, would act on.
Each fact: one or two sentences another agent can act on, with the concrete path, command, host or number. Mark the source kind: user (the owner said it), observed (the agent found it out).
Output ONLY a JSON array: [{"description": "...", "type": "user_standard|machine_fact|past_mistake|design_decision|structure|history", "kind": "user|observed", "applies_to": {"paths": [], "repos": [], "hosts": [], "commands": []}}]

Repo: %s  Host: %s
DIGEST:
%s
`

// State is the per-file read cursor, persisted between runs.
type State struct {
	Offsets   map[string]int64    `json:"offsets"`
	Skipped   int                 `json:"skipped_long_lines,omitempty"` // lines longer than maxReadBytes passed over
	LastRun   string              `json:"last_run"`
	Completed map[string][]string `json:"completed_windows,omitempty"`
}

// Source is one transcript root.
type Source struct {
	Glob   string
	Format digest.Format
}

// DefaultSources on this machine.
func DefaultSources(home string) []Source {
	return []Source{
		{Glob: filepath.Join(home, ".claude", "projects", "*", "*.jsonl"), Format: digest.Claude},
		{Glob: filepath.Join(home, ".codex", "sessions", "*", "*", "*", "*.jsonl"), Format: digest.Codex},
		{Glob: filepath.Join(home, ".codex", "sessions", "*", "*.jsonl"), Format: digest.Codex},
	}
}

// Extracted is one fact as the model returned it.
type Extracted struct {
	Description string           `json:"description"`
	Type        string           `json:"type"`
	Kind        string           `json:"kind"`
	AppliesTo   map[string][]any `json:"applies_to"`
	Extra       map[string]any   `json:"-"`
	Session     string           `json:"-"`
}

// Window is one digest ready for the model.
type Window struct {
	File  string
	Repo  string
	Lines []string
	Start int64
	End   int64
	Part  int
}

// Runner is the LLM call; tests substitute it.
type Runner func(ctx context.Context, model, prompt string) (result string, costUSD float64, inTokens int, err error)

// RunCLI uses the configured dream runner, retaining Claude as the default.
func RunCLI(ctx context.Context, model, prompt string) (string, float64, int, error) {
	sid := newUUID()
	_ = Exclude(sid)
	res, err := modelrun.RunWithFallback(ctx, "dream", model, prompt, sid)
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

// Windows finds files with new bytes since the state and digests the delta.
// Fresh state starts at each file's current end: the dreamer never replays
// history (the import and the eval already covered it).
//
// Cursors are advanced here only for deltas with nothing to extract. For a
// file that yields windows the caller advances the cursor to Window.End
// once every window of that file has been processed, so a capped or failed
// run leaves the delta for the next run instead of losing it. At most
// maxReadBytes per file per run are read; a file that shrank (rotated,
// truncated) restarts from its current end. A complete line longer than
// maxReadBytes is passed over: its cursor moves past it with no window.
func Windows(sources []Source, st *State, fresh bool) ([]Window, error) {
	home, _ := os.UserHomeDir()
	var ws []Window
	exclude := loadExclude()
	for _, src := range sources {
		files, _ := filepath.Glob(src.Glob)
		for _, f := range files {
			if exclude[strings.TrimSuffix(filepath.Base(f), ".jsonl")] {
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
			lines := digest.Lines(strings.NewReader(string(buf)), src.Format)
			if !digest.Interesting(lines) {
				st.Offsets[f] = end
				continue
			}
			// The cwd's repo, else the repo the window's own paths are in. Per
			// window, not per delta: a long session moves between repos, and
			// measured per delta, 10 of 40 inferred repos were wrong.
			cwdRepo := repoOf(f, src.Format, buf)
			resolver := repoinfer.NewResolver(home)
			for part, c := range chunk(lines, maxWindowChars) {
				repo := cwdRepo
				if repo == "" {
					repo = windowRepo(c, resolver)
				}
				ws = append(ws, Window{File: f, Repo: repo, Lines: c, Start: off, End: end, Part: part})
			}
		}
	}
	return ws, nil
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
	b, err := os.ReadFile(filepath.Join(home, ".local", "state", "tacit", "dream-exclude"))
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
	p := filepath.Join(home, ".local", "state", "tacit", "dream-exclude")
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

// repoOf guesses the repo from the first cwd in the delta (both formats carry one).
func repoOf(file string, f digest.Format, raw []byte) string {
	if m := cwdRx.FindSubmatch(raw); m != nil {
		home, _ := os.UserHomeDir()
		return RepoOfCwd(cwdPath(string(m[1])), home, isRepo)
	}
	// No cwd: the transcript folder's name says where the session started
	// (-home-u-projects-memra). It names a repo only when that is a
	// checkout under the projects directory; any other folder (home, a
	// folder of checkouts) is not a project, and the window's paths decide
	// (windowRepo). The folder name's last dash-separated part is the guess,
	// so a repo with a dash in its name (my-repo) or one level down
	// (projects/agent-sh/agnix) is not found here and gets no repo.
	if f == digest.Claude {
		parts := strings.Split(filepath.Base(filepath.Dir(file)), "-")
		name := parts[len(parts)-1]
		home, _ := os.UserHomeDir()
		projects := filepath.Join(home, "projects")
		if d := os.Getenv("TACIT_PROJECTS"); d != "" {
			projects = filepath.Clean(d)
		}
		if name != "" && home != "" && isRepo(filepath.Join(projects, name)) {
			return name
		}
	}
	return ""
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

// RepoOfCwd is the repo a session's working directory belongs to. A
// worktree is its repo's (<repo>/.claude/worktrees/x, <repo>/.local/runners/
// w/worktrees/x, <repo>/.worktrees/x, ~/projects/<repo>-worktrees/x), a
// subdirectory is its repo's (~/projects/tacit/client is tacit), a repo under
// an organisation directory is its own (~/projects/agent-sh/agnix), and the
// home directory and its dot directories (~/.cache, ~/.config) are no repo.
// The basename of the directory was the rule before 2026-10-11: 21 percent
// of the pending dreamer notes were filed under a worktree's name
// (projects/<task>-g1) and 12 percent under projects/<user> or
// projects/client.
func RepoOfCwd(cwd, home string, isRepo func(dir string) bool) string {
	p := filepath.Clean(cwd)
	for _, m := range []string{"/.claude/worktrees/", "/.local/runners/", "/.worktrees/"} {
		if i := strings.Index(p, m); i > 0 {
			p = p[:i]
		}
	}
	if home != "" {
		projects := filepath.Join(home, "projects")
		if d := os.Getenv("TACIT_PROJECTS"); d != "" {
			projects = filepath.Clean(d)
		}
		if p == projects {
			return ""
		}
		if rel, err := filepath.Rel(projects, p); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
			parts := strings.Split(filepath.ToSlash(rel), "/")
			if r, ok := strings.CutSuffix(parts[0], "-worktrees"); ok && r != "" {
				return r
			}
			if len(parts) > 1 && !isRepo(filepath.Join(projects, parts[0])) && isRepo(filepath.Join(projects, parts[0], parts[1])) {
				return parts[1]
			}
			return parts[0]
		}
		if p == home || strings.HasPrefix(p, home+string(filepath.Separator)+".") {
			return ""
		}
		// Elsewhere in home: the repo that holds the directory, else none. A
		// folder of checkouts (~/workplace) or of documents is not a project,
		// and its name made pseudo-projects of where sessions started
		// (projects/workplace). A worktree is its repository's.
		if strings.HasPrefix(p, home+string(filepath.Separator)) {
			for d := p; d != home && d != filepath.Dir(d); d = filepath.Dir(d) {
				if isRepo(d) {
					return filepath.Base(repoinfer.WorktreeRepo(d))
				}
			}
			return ""
		}
	}
	if p == "/" || p == "." {
		return ""
	}
	return filepath.Base(p)
}

func isRepo(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

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
		Body: strings.Join(strings.Fields(x.Description), " ") + "\n",
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
}
