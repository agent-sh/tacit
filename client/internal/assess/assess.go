// Package assess is the curator's assessment of folders (SPEC.md, "Folder
// records"): code gathers the evidence of what a project, area or topic
// folder is (git, the dreamer's registry, the store's facts, the owner's
// words), the curator's model decides its kind and status and why, and code
// checks the decision (record.Validate) and builds the record. Code never
// judges the evidence itself: a git date or a count is shown, not ruled on.
package assess

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/agent-sh/tacit/client/internal/check"
	"github.com/agent-sh/tacit/client/internal/modelrun"
	"github.com/agent-sh/tacit/client/internal/record"
)

// KindNone is the model's answer for a folder that is no project of the
// owner's (an upstream clone, a tool's checkout, a folder of checkouts):
// its record says so, with status dead, so it is not asked again.
const KindNone = "none"

// NotProject starts the why of a "none" decision's entry, written by code.
const NotProject = "not a project: "

// Candidate is one folder to decide.
type Candidate struct {
	Folder string
	// Reason is why code put it to the model ("a repository of this
	// machine with no record", "new work since the last decision", ...).
	Reason string
	// Old is its record now, nil for none.
	Old *record.Record
	// GitRoot: some machine's evidence says the folder is a git checkout
	// (a none decision is then a repo record, else a folder record).
	GitRoot bool
	// Evidence is what code gathered, written into the record as it is.
	Evidence []record.Evidence
	// Context is what else the model reads, one line each: names repoid
	// proves, the folder's facts, the sessions that worked there, the
	// owner's words naming it. Not written into the record.
	Context []string
	// Proposal: the folder is an area or topic proposed from sessions'
	// notes (Kind is its kind), Sessions the distinct sessions behind it.
	Kind     string
	Sessions []string
	// Proposals are the folder log ids of the proposals behind it, Facts the
	// facts they were made for (which move into the folder when it is made).
	Proposals []string
	Facts     []string

	// Name is what code checked of the folder's name as a repository
	// (projects/ folders; nil for none): every machine's checkouts, the
	// remotes, the forge.
	Name *check.RepoCheck
	// Real are the repositories code proposes this folder belongs to when
	// its name exists nowhere or is an old name: each one exists by code's
	// check, with the evidence that names it.
	Real []RealRepo
	// Pattern is the folder name's pattern (NamePattern); Similar the
	// decisions already made on folders alike (same pattern or evidence
	// shape), one line each, filled in when its batch is rendered.
	Pattern string
	Similar []string
	// Same are code's checks that other names are this repository (another
	// name of its checkout, a clone with its origin): one repository has one
	// live folder. Group is the name of the set of candidates one
	// repository's names make, so they are decided in one batch.
	Same  []check.Result
	Group string
	// Alike are the repositories folders alike (the same name pattern) were
	// folded into, each existing by code's check: proposed with the others,
	// filled in when its batch is rendered.
	Alike []RealRepo
	// ProposedKey are the names Real's stable evidence gave (sessions,
	// applies_to, remotes, a rename), before the cut to the best and the
	// checks of today's checkouts: what a fingerprint reads of Real.
	ProposedKey []string
	// Canonical is the name code makes canonical among the names this
	// repository goes by (the owner's decision, else the main checkout's
	// directory, else the forge's name), CanonicalWhy its evidence; "" when
	// none says.
	Canonical    string
	CanonicalWhy []string
	// Extra is the evidence code gathered beyond the first ask's for a folder
	// left for review (needs-review is not a parking state: each re-attempt
	// searches wider), each item with a stable key; Attempt is which
	// re-attempt this is (0: none), LastAt the day it was last decided, and
	// Seen the keys an earlier attempt already showed (the rest are new).
	Extra   []Extra
	Attempt int
	LastAt  string
	Seen    map[string]bool
}

// Extra is one item of the evidence a re-attempt gathered: Key names what
// it is (a fact, a session, a forge commit, a decision on a folder alike),
// Line is what the model reads; a header (Key "") groups the items under it.
type Extra struct {
	Key, Line string
}

// NewExtra counts the extra items no earlier attempt showed.
func (c Candidate) NewExtra() int {
	n := 0
	for _, e := range c.Extra {
		if e.Key != "" && !c.Seen[e.Key] {
			n++
		}
	}
	return n
}

// ExtraKeys are the extra items' keys, sorted.
func (c Candidate) ExtraKeys() []string {
	var out []string
	for _, e := range c.Extra {
		if e.Key != "" {
			out = append(out, e.Key)
		}
	}
	sort.Strings(out)
	return out
}

// Proposed are code's candidate repositories for the folder: from its own
// evidence, then where folders alike went.
func (c Candidate) Proposed() []RealRepo {
	out := append([]RealRepo(nil), c.Real...)
	for _, r := range c.Alike {
		dup := false
		for i := range out {
			if out[i].Name == r.Name {
				out[i].Why = append(append([]string(nil), out[i].Why...), r.Why...)
				dup = true
			}
		}
		if !dup {
			out = append(out, r)
		}
	}
	return out
}

// RealRepo is a repository code proposes a folder belongs to, with why.
type RealRepo struct {
	Name string
	Why  []string
	// Score orders the proposals: sessions that worked in its checkout
	// weigh most, then facts that name its paths, then a near name.
	Score int
}

// Absent reports a folder whose name, as a repository, code verified exists
// nowhere: no checkout or other name of one on any machine, no remote, and
// the forge has no repository of that name (or says it was renamed), while
// no folder of that name exists on any machine either.
func (c Candidate) Absent() bool {
	if c.Name == nil || c.Name.Verdict != check.Fails {
		return false
	}
	for _, e := range c.Evidence {
		if len(e.Paths) > 0 || e.GitRoot {
			return false
		}
	}
	return true
}

// Decision is the model's answer for one candidate.
type Decision struct {
	Folder         string        `json:"folder"`
	Kind           string        `json:"kind"`
	Status         string        `json:"status"`
	Why            string        `json:"why"`
	Description    string        `json:"description"`
	Body           string        `json:"body,omitempty"`
	Aliases        []string      `json:"aliases,omitempty"`
	SuppressedBy   string        `json:"suppressed_by,omitempty"`
	Roots          []record.Root `json:"roots,omitempty"`
	Tools          []string      `json:"tools,omitempty"`
	Commands       []string      `json:"commands,omitempty"`
	Repos          []string      `json:"repos,omitempty"`
	OwnerQuote     string        `json:"owner_quote,omitempty"`
	OwnerQuoteFrom string        `json:"owner_quote_from,omitempty"`
	// Reject: for a proposed area or topic, the model's no (Why says why);
	// FoldInto: the existing folder it belongs to instead (it gains the
	// proposed name as an alias).
	Reject   bool   `json:"reject,omitempty"`
	FoldInto string `json:"fold_into,omitempty"`
}

// Prompt is the assessment prompt; %s is the candidates (Render).
const Prompt = `You are the curator of tacit, a fact store shared by coding agents on one owner's machines. Facts are filed in folders: projects/<name> (a git repository, or a project folder that is no repository), areas/<name> (an ongoing system or practice) and topics/<name> (a bounded work stream). Each folder gets a record that says what it is. Code gathered the evidence below for each folder; you decide.

For each folder decide:
- kind: "repo" (a git repository that is the owner's project), "folder" (a project folder of the owner's that is no git repository), or "none" (not a project of the owner's: an upstream or tool clone nobody works in, a folder that only holds other checkouts, the home directory, a scratch or task directory). For a proposed area or topic: "area" or "topic", or set reject or fold_into.
- status: "active" (being worked), "dormant" (real work, nobody on it lately), "shipped" (done and delivered), "dead" (abandoned or gone), or "needs-review" (the evidence does not settle it). Status comes from the owner's words and from sessions: git says whether a folder is real work and whose, never whether it is shipped, dormant or dead.
- why: one or two sentences. It MUST cite the sessions, notes or commits the decision rests on, as code shows them for THIS folder (session:<id>, claude:<id>, codex:<id>, note:<name>, or commit:<sha> for the folder's own commits under the owner's git identity, which code lists); never cite a session that is not about this folder just to have a citation. Recent own commits are evidence of work: "active" may rest on them. Code refuses a why that cites none it can find; a git date alone is no reason. A folder whose evidence shows no session at all (no work sessions, no fact sessions) needs no citation, and its status can then only be dead, dormant or needs-review: say that no session touched it.
- description: one line (at most 200 characters) saying what the folder is.
- body: optional, a few sentences: why the folder exists and what belongs in it.
- aliases: optional, other names the same project is known by, only names no other folder has: never the name of another folder listed here or holding facts (that folder gets its own decision with suppressed_by instead).
- suppressed_by: optional, the folder that replaces this one when its facts belong to another project: the same project under another name (a symlink's or a remote's name, a rename), or a task worktree, clone or subfolder of another project. Give such a folder kind repo or folder (not none) with suppressed_by that project's folder, so its facts move there. The target must have or get a record and not itself be replaced. Only when the evidence shows it.
- roots: kind folder only, optional: [{"host": ..., "path": ...}] where it lives.
- tools, commands: an area only: tool-name patterns (globs such as "mcp__*__Ticketing*") and command prefixes its sessions used, only ones the evidence lists.
- repos: a topic only, optional: repositories it is tied to.
- owner_quote, owner_quote_from: only when the owner's own words shown below state the decision for THIS folder (its name or alias in them): copy their words exactly and the ref they came from. Words that only mention the folder are evidence for your why, not a quote here.
- reject (true) or fold_into ("<existing folder>"): a proposed area or topic only, when it should not be made, with why.

Dry claims are checked by code, not judged. Whether a repository of a name exists and what it is called now has one source of truth: code checked each folder's name against every machine's checkouts, the checkouts' remotes and the forge (lines "check: ..."), and code refuses a decision a check refutes, showing the check:
- A project folder whose name exists nowhere (no checkout or other name of one, no remote, the forge has no such repository) is no repository under that name: it cannot be active, dormant, shipped or dead as it is. Set suppressed_by to the real repository its facts belong to (code lists candidates with their evidence: the checkouts its sessions worked in, repositories whose paths or remotes its facts name, near names; pick the one the evidence supports), or answer needs-review when the evidence picks none. Only when code lists no candidate may it be kind none.
- A name the forge says was renamed is an old name: fold it into the current name, never the current name into it.
- Never set suppressed_by to a repository whose name exists nowhere.
- Names code proved are one repository (another name of a checkout, a symlink's or the origin remote's; clones with one origin) get one live folder: the others set suppressed_by to it, or answer needs-review. They come in one batch, so decide them together. Which name it goes by is code's when a line "check: the canonical name of this repository is X" says so (the owner's decision first, then the main checkout's directory, then the forge's name): X is the live folder and the other names fold into it; code refuses the other direction. Without such a line it is yours to decide from the evidence. The forge's current name is kept on the record and shown beside the folder's name, so a repository whose GitHub name differs needs no folder of that name.
A folder left for review comes back on later runs with "extra evidence" code gathered for it, wider each time: facts in other folders that name it, the sessions behind them, the forge's record of the repository (last push, newest commits with their authors, latest release, archived), and the decisions on folders alike (near names, the same forge owner). needs-review is not a place to park a folder: nobody promises the owner will review it. Decide from that evidence whenever it settles the folder; answer needs-review again only when even it does not, and then say in why what evidence would settle it. Commits the forge lists by the owner's forge account (code says which) are the folder's own commits: cite them as commit:<sha>, as for a checkout's; a session listed there filed a fact naming the folder and may be cited for it. An archived repository was archived by its owner: dormant, shipped or dead by the rest of the evidence, never active.
Folders whose names follow one pattern (kv-013-g1, kv-140-g2) come together, with the decisions already made on folders like them: decide alike folders alike, and when one differs say in why what in its evidence differs.

Output ONLY a JSON array, one object per folder, no prose, no code fence:
  {"folder": "...", "kind": "...", "status": "...", "why": "...", "description": "...", "body": "...", "aliases": [], "suppressed_by": "", "roots": [], "tools": [], "commands": [], "repos": [], "owner_quote": "", "owner_quote_from": "", "reject": false, "fold_into": ""}

FOLDERS:
%s
`

// Render writes the candidates section of the prompt.
func Render(cands []Candidate) string {
	var b strings.Builder
	for _, c := range cands {
		fmt.Fprintf(&b, "### folder=%s\nwhy asked: %s\n", c.Folder, c.Reason)
		if c.Kind != "" {
			fmt.Fprintf(&b, "proposed %s, from %d session(s): %s\n", c.Kind, len(c.Sessions), strings.Join(c.Sessions, ", "))
		}
		if r := c.Old; r != nil {
			fmt.Fprintf(&b, "record now: %s, %s, %q", r.Kind, r.Status, r.Description)
			if len(r.Was) > 0 {
				fmt.Fprintf(&b, "; last decided %s by %s: %s", r.Was[0].Date, r.Was[0].By, oneLine(r.Was[0].Why, 300))
			}
			if r.SuppressedBy != "" {
				fmt.Fprintf(&b, "; suppressed by %s", r.SuppressedBy)
			}
			b.WriteString("\n")
		}
		n := 0
		for _, e := range c.Evidence {
			if l := evidenceLine(e); l != "" {
				n++
				fmt.Fprintf(&b, "evidence %d: %s\n", n, l)
			}
		}
		if c.Name != nil {
			for _, l := range c.Name.Lines() {
				b.WriteString("check: " + l + "\n")
			}
			fmt.Fprintf(&b, "check: the name %s as a repository: %s", c.Name.Name, c.Name.Verdict)
			if c.Name.Renamed != "" {
				fmt.Fprintf(&b, " (the forge has it as %s now)", c.Name.Forge)
			}
			b.WriteString("\n")
		}
		for _, r := range c.Same {
			b.WriteString("check: " + r.Line() + "\n")
		}
		if c.Canonical != "" && (len(c.Same) > 0 || c.Canonical != c.Folder[strings.LastIndex(c.Folder, "/")+1:]) {
			fmt.Fprintf(&b, "check: the canonical name of this repository is %s (%s)\n", c.Canonical, strings.Join(c.CanonicalWhy, "; "))
		}
		if real := c.Proposed(); len(real) > 0 {
			var parts []string
			for _, r := range real {
				parts = append(parts, fmt.Sprintf("%s (%s)", r.Name, strings.Join(r.Why, "; ")))
			}
			b.WriteString("candidate real repositories (code's, each exists by its check): " + strings.Join(parts, ", ") + "\n")
		} else if c.Absent() {
			b.WriteString("candidate real repositories: none found in the evidence\n")
		}
		for _, l := range c.Context {
			b.WriteString(l + "\n")
		}
		for _, l := range c.Similar {
			b.WriteString(l + "\n")
		}
		if len(c.Extra) > 0 {
			fmt.Fprintf(&b, "extra evidence code gathered for re-attempt %d (left for review on %s; %d of %d item(s) new since the last attempt):\n", c.Attempt, orDash(c.LastAt), c.NewExtra(), len(c.ExtraKeys()))
			for _, e := range c.Extra {
				l := e.Line
				if e.Key != "" && !c.Seen[e.Key] {
					l += " [new]"
				}
				b.WriteString(l + "\n")
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func evidenceLine(e record.Evidence) string {
	var parts []string
	add := func(k string, v any) { parts = append(parts, fmt.Sprintf("%s %v", k, v)) }
	if e.Host != "" {
		add("host", e.Host)
	}
	if e.Host != "" || e.GitRoot {
		add("git_root", e.GitRoot)
	}
	if e.Checkout != "" {
		add("checkout", e.Checkout)
	}
	if len(e.Remotes) > 0 {
		add("remotes", strings.Join(e.Remotes, " "))
	}
	if e.OwnCommits != nil {
		add("own_commits", *e.OwnCommits)
		if e.FirstCommit != "" {
			add("commits", e.FirstCommit+".."+e.LastCommit)
		}
	} else if e.GitRoot && e.Host != "" {
		parts = append(parts, "own commits unknown (git history not readable here)")
	}
	if e.WorkSessions > 0 || e.Host != "" {
		add("work_sessions", e.WorkSessions)
		if e.FirstWork != "" {
			add("work", e.FirstWork+".."+e.LastWork)
		}
	}
	if e.UpstreamClone {
		parts = append(parts, "upstream_clone (no own commits, no work sessions)")
	}
	if e.FactSessions > 0 {
		add("fact_sessions", e.FactSessions)
		add("facts", e.FirstFact+".."+e.LastFact)
	}
	if len(e.Paths) > 0 {
		add("paths", strings.Join(e.Paths, " "))
	}
	if len(e.Sessions) > 0 {
		add("sessions", strings.Join(e.Sessions, " "))
	}
	return strings.Join(parts, "; ")
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len([]rune(s)) > max {
		s = string([]rune(s)[:max]) + "…"
	}
	return s
}

// Parse reads the model's answer: a JSON array of decisions. An answer
// that is no such array wraps modelrun.ErrInvalidResult.
func Parse(text string) ([]Decision, error) {
	text = strings.TrimSpace(text)
	text = strings.TrimPrefix(strings.TrimPrefix(text, "```json"), "```")
	text = strings.TrimSpace(strings.TrimSuffix(text, "```"))
	if i := strings.Index(text, "["); i > 0 {
		text = text[i:]
	}
	if j := strings.LastIndex(text, "]"); j >= 0 {
		text = text[:j+1]
	}
	var ds []Decision
	if err := json.Unmarshal([]byte(text), &ds); err != nil {
		return nil, fmt.Errorf("%w: the assessment must be a JSON array of decisions", modelrun.ErrInvalidResult)
	}
	return ds, nil
}

// Refusal is a decision code refused, with why, for the retry prompt.
type Refusal struct {
	Folder   string
	Problems []string
}

// RetryPrompt is prompt again with the answer and code's refusals, asking
// for the refused folders only.
func RetryPrompt(prompt, answer string, refused []Refusal) string {
	var b strings.Builder
	b.WriteString(prompt)
	b.WriteString("\nYOUR PREVIOUS ANSWER:\n")
	a := strings.TrimSpace(answer)
	if len(a) > 40_000 {
		a = a[:40_000] + "\n[... cut]"
	}
	b.WriteString(a)
	b.WriteString("\n\nCODE REFUSED THESE DECISIONS. Code checks every value; it does not change your decisions for you. The problems:\n")
	for _, r := range refused {
		fmt.Fprintf(&b, "- %s: %s\n", r.Folder, strings.Join(r.Problems, "; "))
	}
	b.WriteString("Return a JSON array with decisions for these folders only, with the problems fixed. Output ONLY the JSON array.\n")
	return b.String()
}

// ErrNoDecision is a candidate the answer said nothing about.
var ErrNoDecision = errors.New("no decision")

// Owner is the owner's words a decision rests on, when code verified them
// in their turns: the entry is then theirs (by owner, quote cited).
type Owner struct {
	Quote, From string
}

// Build makes the record a decision asks for: the candidate's record with
// the decision applied (a new history entry when the status or kind
// changes), or a new one. Code writes the evidence, the dates and the
// entry's writer; the model's values are taken as given, for
// record.Validate to judge. owner is non-nil when the decision's quote is
// the owner's (verified by the caller).
func Build(c Candidate, d Decision, today string, owner *Owner) *record.Record {
	kind, status, why := d.Kind, d.Status, strings.TrimSpace(d.Why)
	if kind == KindNone {
		kind, status, why = record.KindFolder, record.StatusDead, NotProject+why
		if c.GitRoot {
			kind = record.KindRepo
		}
	}
	entry := record.Entry{Status: status, Date: today, By: record.ByOpus, Why: why}
	if owner != nil {
		entry.By, entry.Quote, entry.QuoteFrom = record.ByOwner, owner.Quote, owner.From
	}
	var r *record.Record
	if c.Old != nil {
		r = c.Old.Clone()
		if status != r.Status || kind != r.Kind {
			r.Was = append([]record.Entry{entry}, r.Was...)
			r.Status, r.Kind = status, kind
		}
		if strings.TrimSpace(d.SuppressedBy) != "" {
			r.SuppressedBy = strings.TrimSpace(d.SuppressedBy)
		}
		r.Aliases = union(r.Aliases, d.Aliases)
	} else {
		r = &record.Record{Kind: kind, Status: status, Was: []record.Entry{entry}, Created: today, CreatedBy: entry.By,
			SuppressedBy: strings.TrimSpace(d.SuppressedBy), Aliases: union(nil, d.Aliases)}
	}
	r.Folder = c.Folder
	r.Name = c.Folder[strings.LastIndex(c.Folder, "/")+1:]
	if desc := strings.TrimSpace(d.Description); desc != "" {
		r.Description = desc
	}
	if body := strings.TrimSpace(d.Body); body != "" {
		r.Body = body
	}
	if kind == record.KindFolder && len(d.Roots) > 0 {
		r.Roots = d.Roots
	} else if kind == record.KindFolder && len(r.Roots) == 0 && d.Kind != KindNone {
		// Where the folder lives is code's evidence: the folders of its name
		// that sessions worked in, or the projects directory's.
		r.Roots = EvidenceRoots(c.Evidence)
	}
	if kind == record.KindArea {
		r.Tools, r.Commands = union(r.Tools, d.Tools), union(r.Commands, d.Commands)
	}
	if kind == record.KindTopic {
		r.Repos = union(r.Repos, d.Repos)
	}
	r.Evidence = c.Evidence
	return r
}

// EvidenceRoots are the roots code's evidence shows for a folder: each
// machine's paths of the folder's name, as {host, path}.
func EvidenceRoots(ev []record.Evidence) []record.Root {
	var out []record.Root
	seen := map[record.Root]bool{}
	for _, e := range ev {
		if e.Host == "" {
			continue
		}
		for _, p := range e.Paths {
			rt := record.Root{Host: e.Host, Path: p}
			if !seen[rt] {
				seen[rt] = true
				out = append(out, rt)
			}
		}
	}
	return out
}

func union(a, b []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, x := range append(append([]string(nil), a...), b...) {
		x = strings.TrimSpace(x)
		if x != "" && !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Same reports whether two records say the same thing (evidence aside):
// writing b over a would change no decision.
func Same(a, b *record.Record) bool {
	if a == nil || b == nil {
		return a == b
	}
	x, y := a.Clone(), b.Clone()
	x.Evidence, y.Evidence = nil, nil
	ea, _ := x.Encode()
	eb, _ := y.Encode()
	return string(ea) == string(eb)
}

// SortCandidates puts a name that exists before one that exists nowhere
// (the folders others may be suppressed into first), then the names of one
// repository together (Group) and the folders of one name pattern together,
// so a batch decides them at once, a repository's own name before a folder
// that is only another name of one (aliasOf names its repository), then by
// folder.
func SortCandidates(cands []Candidate, aliasOf func(folder string) string) {
	count := map[string]int{}
	for _, c := range cands {
		if c.Pattern != "" {
			count[c.Pattern]++
		}
	}
	// Pattern keys are per root (projects/kv-#-g#).
	key := func(c Candidate) string {
		if c.Group != "" {
			return c.Folder[:strings.LastIndex(c.Folder, "/")+1] + c.Group
		}
		if c.Pattern != "" && count[c.Pattern] > 1 {
			return c.Folder[:strings.LastIndex(c.Folder, "/")+1] + c.Pattern
		}
		return c.Folder
	}
	sort.SliceStable(cands, func(i, j int) bool {
		bi, bj := cands[i].Absent(), cands[j].Absent()
		if bi != bj {
			return !bi
		}
		ki, kj := key(cands[i]), key(cands[j])
		if ki != kj {
			return ki < kj
		}
		ai, aj := aliasOf(cands[i].Folder) != "", aliasOf(cands[j].Folder) != ""
		if ai != aj {
			return !ai
		}
		return cands[i].Folder < cands[j].Folder
	})
}
