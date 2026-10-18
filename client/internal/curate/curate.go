// Package curate is the curator: it turns inbox notes into one-fact entries
// under the curated scopes, keeps owner rulings verbatim, checks every new
// fact against the store for contradictions, regenerates the index and
// pushes as curator@<host>. Daily, using the configured model, from a machine that has
// model access (the server has none).
//
// Measured in eval/VALUE.md: the split lifts delivery 6 to 7 points at every
// block size; the two defects it showed (a dropped ruling, an undetected
// contradiction) are what the ruling rule and the conflict check are for.
package curate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/dream"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/importer"
	"github.com/agent-sh/tacit/client/internal/modelrun"
)

// The split model and conflict judge use independently configured routes.
// The split defaults to sonnet: on the same 287 notes (2026-10-18) Opus 5.5
// curated better by a blind judge on 138 of 170 sampled notes, at 2.7 times
// the cost per note ($0.0107 against $0.0040); the owner chose Sonnet with
// an Opus dreamer. TACIT_CURATE_MODEL or --model selects opus.
// Operator configuration selects model versions, effort and provider profiles.
// Model overrides accept the selected CLI's IDs; --model on curate wins.
var (
	DefaultModel = modelrun.Model("curate", "sonnet")
	JudgeModel   = modelrun.Model("judge", "haiku")
)

const batchChars = 45_000

// envOr is the value of the environment variable key, or def when it is
// unset or blank.
func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// SplitPrompt is eval/curate/split.py's prompt plus the ruling rule.
const SplitPrompt = `You are the curator of tacit, a fact store shared by coding agents on one owner's machines.
Below are memory notes written by agents. Each note bundles several facts. Rewrite them as ONE FACT PER ENTRY.

Rules:
- A fact is a conclusion another agent can act on without reading anything else. Never a transcript or a narrative.
- ` + "`description`" + ` (max 200 chars) must STATE the fact itself, including the key path, command, host or number. Not a topic label.
- ` + "`body`" + `: one to three sentences, the fact plus the why if known. Keep dates absolute (YYYY-MM-DD).
- Owner rulings (something the owner said or decided, source kind user) are facts of their own: keep the ruling's wording as closely as the note gives it, with its date, one entry per ruling. Never fold a ruling into a neighbouring fact or drop it because a related procedure exists.
- A ruling is the owner's OWN WORDS. For every kind user entry, ` + "`owner_quote`" + ` is his words exactly as the note quotes them (at least six words, copied character for character, typos included) and the body holds that phrase in double quotes. A note that only describes what he wants, or an agent's reading of a remark of his, gives a kind observed entry, never kind user. An agent's conclusion is never kind user, however sure the note sounds.
- ` + "`type`" + ` is one of: user_standard, design_decision, past_mistake, machine_fact, code_change, hot_spot, structure, history, user, reference.
- ` + "`scope`" + ` is ` + "`user`, `system`, `projects/<repo>`, or `machines/<host>`" + ` (the curator's host is ` + "`%s`" + `; use machines/%s for facts about this machine: paths, tools, shell, hardware).
- ` + "`kind`" + `: user only when the note quotes the owner's own words for it (see owner_quote), observed if an agent found it out or concluded it, web if it came from a fetched page.
- ` + "`applies_to`" + `: lists of repo-relative path globs, repo names, host names and command prefixes that should trigger this fact. Empty lists when nothing specific applies.
- Drop anything derivable from the code or from git log, anything that is only a session log, and anything purely about the past with no present consequence. Facts that mark a thing as obsolete, retired, renamed or dead ARE valuable (keep them).
- If a note states the same fact twice, emit it once. Do not invent facts.
- Output ONLY a JSON array, no prose, no code fence. Each item:
  {"origin": "<note name>", "name": "<slug>", "description": "...", "type": "...", "scope": "...", "kind": "user|observed|web", "owner_quote": "<kind user only: his exact words from the note>", "applies_to": {"paths": [], "repos": [], "hosts": [], "commands": []}, "body": "..."}

NOTES:
%s
`

// ConflictPrompt judges a batch of pairs in one call. The answer is a JSON
// array of one word per pair, in order.
const ConflictPrompt = `For each numbered pair of facts from a shared store, decide:
- "contradicts": in the SAME concrete situation the two facts tell an agent to do incompatible things (one says do X, the other says never do X; one gives value A, the other value B for the same setting). A refinement, an exception, a different scope, a different time, or two steps of one procedure is NOT a contradiction.
- "supersedes": the same subject, and the NEW fact states the current state of something the EXISTING fact describes as it used to be.
- "no": everything else, including related or overlapping facts that can both be followed.
When unsure, answer "no".
Output ONLY a JSON array of strings, one per pair in order, each exactly "contradicts", "supersedes" or "no".

%s`

// RulingsPrompt is the second pass per batch: every owner sentence in the
// notes must have a kind:user entry. The first live split dropped a ruling
// while keeping its surroundings (eval/VALUE.md, D6).
const RulingsPrompt = `Below are agent memory notes and the list of OWNER RULINGS already extracted from them (kind user: something the owner said, decided or standardized).
List every owner ruling in the notes that is NOT yet covered by the extracted list. A ruling is covered when an extracted entry states the same decision. A ruling is the owner's OWN WORDS as the note quotes them: list only those, never an agent's reading of what he wants. ` + "`owner_quote`" + ` is his words exactly as the note gives them (at least six words, character for character, typos included) and the body holds that phrase in double quotes. Keep the date if visible, one entry per ruling, the note name as origin.
Output ONLY a JSON array (possibly empty) of items:
  {"origin": "<note name>", "name": "<slug>", "description": "...", "type": "user_standard|design_decision", "scope": "<same scope as the note>", "kind": "user", "owner_quote": "<his exact words from the note>", "applies_to": {"paths": [], "repos": [], "hosts": [], "commands": []}, "body": "..."}

EXTRACTED RULINGS:
%s

NOTES:
%s
`

// Pair is one conflict check.
type Pair struct {
	ExistingName, ExistingDesc string
	NewName, NewDesc           string
}

// RenderPairs formats pairs for ConflictPrompt.
func RenderPairs(pairs []Pair) string {
	var b strings.Builder
	for i, p := range pairs {
		fmt.Fprintf(&b, "%d.\nEXISTING (%s): %s\nNEW (%s): %s\n\n", i+1, p.ExistingName, p.ExistingDesc, p.NewName, p.NewDesc)
	}
	return b.String()
}

// ParseVerdicts reads the batched judge answer; a short or unparsable answer
// yields NoConflict for the missing pairs (nothing is retired on a guess).
func ParseVerdicts(result string, n int) []Verdict {
	out := make([]Verdict, n)
	for i := range out {
		out[i] = NoConflict
	}
	m := arrayRx.FindString(strings.TrimSpace(result))
	var xs []string
	if m == "" || json.Unmarshal([]byte(m), &xs) != nil {
		return out
	}
	for i := 0; i < n && i < len(xs); i++ {
		out[i] = ParseVerdict(xs[i])
	}
	return out
}

// Entry is one split result.
type Entry struct {
	Origin      string           `json:"origin"`
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Type        string           `json:"type"`
	Scope       string           `json:"scope"`
	Kind        string           `json:"kind"`
	OwnerQuote  string           `json:"owner_quote,omitempty"`
	AppliesTo   map[string][]any `json:"applies_to"`
	Body        string           `json:"body"`
}

// Runner is the model call; tests substitute it.
type Runner func(ctx context.Context, model, prompt string) (string, float64, error)

// RunCLI and JudgeCLI select independent runners for curation and judging.
func RunCLI(ctx context.Context, model, prompt string) (string, float64, error) {
	return runRole(ctx, "curate", model, prompt)
}
func JudgeCLI(ctx context.Context, model, prompt string) (string, float64, error) {
	return runRole(ctx, "judge", model, prompt)
}
func runRole(ctx context.Context, role, model, prompt string) (string, float64, error) {
	sid := dream.NewSessionID()
	_ = dream.Exclude(sid)
	res, err := modelrun.RunWithFallback(ctx, role, model, prompt, sid)
	if res.SessionID != "" {
		_ = dream.Exclude(res.SessionID)
	}
	return res.Text, res.CostUSD, err
}
func ClaudeCLI(ctx context.Context, model, prompt string) (string, float64, error) {
	return RunCLI(ctx, model, prompt)
}

// Batches groups inbox notes so each prompt stays under batchChars.
func Batches(notes []*fact.Fact) [][]*fact.Fact {
	var out [][]*fact.Fact
	var cur []*fact.Fact
	size := 0
	for _, n := range notes {
		sz := len(n.Body) + len(n.Meta.Description) + 100
		if len(cur) > 0 && size+sz > batchChars {
			out = append(out, cur)
			cur, size = nil, 0
		}
		cur = append(cur, n)
		size += sz
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// Render writes the notes section of the split prompt.
func Render(batch []*fact.Fact) string {
	var b strings.Builder
	for _, n := range batch {
		body := strings.TrimSpace(n.Body)
		if len(body) > 12_000 {
			body = body[:12_000]
		}
		fmt.Fprintf(&b, "### note name=%s scope=%s kind=%s\ndescription: %s\n%s\n\n", n.Meta.Name, n.Meta.Scope, n.Meta.Source.Kind, n.Meta.Description, body)
	}
	return b.String()
}

var arrayRx = regexp.MustCompile(`(?s)\[.*\]`)

// Parse extracts entries from a model answer.
func Parse(result string) []Entry {
	m := arrayRx.FindString(strings.TrimSpace(result))
	if m == "" {
		return nil
	}
	var raws []json.RawMessage
	if json.Unmarshal([]byte(m), &raws) != nil {
		return nil
	}
	var out []Entry
	for _, r := range raws {
		var x Entry
		if json.Unmarshal(r, &x) == nil && strings.TrimSpace(x.Description) != "" && x.Origin != "" {
			out = append(out, x)
		}
	}
	return out
}

var scopeRx = regexp.MustCompile(`^(user|system|projects/[a-z0-9-]+|machines/[a-z0-9-]+)$`)

// ToFact turns an entry into a curated fact; taken dedupes names.
func ToFact(e Entry, host string, taken map[string]bool) *fact.Fact {
	scope := e.Scope
	if !scopeRx.MatchString(scope) {
		scope = "user"
	}
	name := importer.Slug(e.Name, 80)
	if name == "fact" {
		name = importer.Slug(e.Description, 60)
	}
	if taken[name] {
		name = importer.Slug(name, 70) + "-" + shortHash(e.Description)
	}
	taken[name] = true
	typ := e.Type
	if !fact.Types[typ] || typ == "unclassified" {
		typ = "unclassified"
	}
	kind := e.Kind
	if !fact.SourceKinds[kind] || kind == "derived" {
		kind = "observed"
	}
	desc := fact.ClipDescription(e.Description, 200)
	body := strings.Join(strings.Fields(e.Body), " ")
	if body == "" {
		body = desc
	}
	return &fact.Fact{
		Path: scope + "/" + name + ".md",
		Meta: fact.Meta{
			Name: name, Description: desc, Type: typ, Scope: scope,
			AppliesTo: fact.AppliesTo{Paths: strs(e.AppliesTo["paths"]), Repos: strs(e.AppliesTo["repos"]), Hosts: strs(e.AppliesTo["hosts"]), Commands: strs(e.AppliesTo["commands"])},
			Source:    fact.Source{Harness: "curator", Host: host, Kind: kind},
			Created:   time.Now().Format("2006-01-02"),
			Origin:    e.Origin,
		},
		Body: body + "\n",
	}
}

func strs(xs []any) []string {
	var out []string
	for _, x := range xs {
		if s, ok := x.(string); ok && s != "" && len(out) < 8 {
			out = append(out, s)
		}
	}
	return out
}

func shortHash(s string) string {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return fmt.Sprintf("%06x", h&0xffffff)
}

// Verdict of a conflict check.
type Verdict string

const (
	NoConflict  Verdict = "no"
	Contradicts Verdict = "contradicts"
	Supersedes  Verdict = "supersedes"
)

// ParseVerdict reads the one-word judge answer.
func ParseVerdict(s string) Verdict {
	s = strings.ToLower(strings.TrimSpace(s))
	switch {
	case strings.HasPrefix(s, "contradict"):
		return Contradicts
	case strings.HasPrefix(s, "supersede"):
		return Supersedes
	}
	return NoConflict
}

// Report is one curator run's accounting.
type Report struct {
	ModelCalls       *modelrun.Trace `json:"model_calls,omitempty"`
	DeferredOwner    int             `json:"deferred_owner_notes,omitempty"`
	At               string          `json:"at"`
	Notes            int             `json:"notes"`
	Restated         int             `json:"restated,omitempty"` // pending notes dropped before the split: they restate a curated fact or an earlier note
	Batches          int             `json:"batches"`
	Facts            int             `json:"facts"`
	Rulings          int             `json:"rulings"`
	NotOwnerWords    int             `json:"not_owner_words,omitempty"` // kind user entries with no verified quote of theirs, filed as observed
	RecoveredRulings int             `json:"recovered_rulings"`
	Checked          int             `json:"conflict_checks"`
	JudgeCalls       int             `json:"judge_calls"`
	Contradicts      int             `json:"contradicts"`
	Supersedes       int             `json:"supersedes"`
	CostUSD          float64         `json:"cost_usd"`
	Errors           int             `json:"errors"`
	FailedBatches    int             `json:"failed_batches"`
	BatchSplits      int             `json:"batch_splits,omitempty"`    // batches split in two after an invalid answer
	SetAside         int             `json:"set_aside,omitempty"`       // pending notes set aside after failing alone in maxNoteFailures runs
	JudgeSplits      int             `json:"judge_splits,omitempty"`    // judge chunks split in two after an invalid answer
	JudgeUnchecked   int             `json:"judge_unchecked,omitempty"` // pairs the judge gave no valid verdict for alone; written, sent to review, counted as errors
	DerivedFiles     int             `json:"derived_files,omitempty"`   // derived/ files written or retired by the git pass
	Recheck          bool            `json:"recheck,omitempty"`         // a --recheck-conflicts run: Facts are the facts checked again
}
