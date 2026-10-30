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
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/modelrun"
	"github.com/agent-sh/tacit/client/internal/record"
)

// KindNone is the model's answer for a folder that is no project of the
// owner's (an upstream clone, a tool's checkout, a folder of checkouts):
// its record says so, with status dead, so it is not asked again.
const KindNone = "none"

// NotProject starts the why of a "none" decision's entry, written by code.
const NotProject = record.NotProjectWhy

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
	// Ancestors are the folders code found this one is related to, best
	// first (the owner's rule: each piece of content that is clearly related
	// to another tries to find its ancestor). A related folder is presumed
	// part of one of them; it stands alone only on Independence, the evidence
	// code found of an identity of its own, which its decision cites.
	Ancestors    []Ancestor
	Independence []Independence
	// Depth is how many of this run's candidates stand between the folder
	// and its group's final ancestor (0: the ancestor itself, or no ancestor
	// among them): a group is decided ancestor first.
	Depth int
	// NoEvidence: code found no ancestor, no evidence of an identity of its
	// own and no session that worked in it or filed its facts. Nothing says
	// what it is: code's outcome is needs-review, unless the decision cites
	// evidence code showed (a note, the owner's words).
	NoEvidence bool
	// Activity is the checkable evidence of the folder's life code gathered
	// before its first ask (activity.go): the owner's activity days, the
	// forge's record, and code's status from them. nil: none gathered.
	Activity *Activity
	// Notes are the live facts in other folders that name it, the owner's
	// rulings, statements and decisions first (words.go), at most NotesShown
	// of NotesTotal, gathered before its first ask.
	Notes      []Note
	NotesTotal int
	// Names are the other names code found for the folder (aliases, its
	// forge name, other names of its checkout), filled with Notes: a cited
	// sentence names it by its folder name or one of these.
	Names []string `json:"-"`
	// RealWork is the owner's activity on it after a day, classified as
	// work or maintenance by code (maintenance.go); nil: none known.
	RealWork func(after string) Changes `json:"-"`
}

// Changes is c's RealWork after day (none when it has no way to say).
func (c Candidate) Changes(after string) Changes {
	if c.RealWork == nil {
		return Changes{}
	}
	return c.RealWork(after)
}

// Ancestor is a folder code proposes a candidate is part of, with why.
type Ancestor struct {
	// Name is the repository or project folder's name, Folder the live folder
	// that holds it now.
	Name, Folder string
	Why          []string
	// Sources are the kinds of evidence behind it (SourceGit, ...), sorted.
	Sources []string
	Score   int
}

// Certain reports an ancestor git's own metadata names (a linked worktree or
// a separate clone of it): code folds the folder on its own.
func (an Ancestor) Certain() bool {
	for _, s := range an.Sources {
		if s == SourceGit {
			return true
		}
	}
	return false
}

// Kinds of ancestor evidence.
const (
	// SourceGit: the folder's name is a linked worktree's or a separate
	// clone's directory, recorded from git's metadata while it existed.
	SourceGit = "git"
	// SourcePath: paths in facts put a directory of the folder's name inside
	// the ancestor's tree (or its worktrees directory).
	SourcePath = "path"
	// SourceName: the folder's name contains the ancestor's name as whole
	// words (wt-portfolio-rework-20261004 holds portfolio).
	SourceName = "name"
	// SourceSessions: the sessions that filed the folder's facts filed facts
	// in the ancestor too, and the folder's facts point at it by their own
	// paths, forge URLs or repository-relative paths.
	SourceSessions = "sessions"
	// SourceFacts: at least half of the folder's live facts point at it by
	// their own paths, forge URLs or repository-relative paths.
	SourceFacts = "facts"
	// SourceProposed and SourceNamed rank a related folder's ancestors (they
	// make no folder related on their own): a repository code proposes for
	// it (Real), and one its facts name as a word.
	SourceProposed = "proposed"
	SourceNamed    = "named"
	// SourceMentions: the facts of other project folders that name the
	// folder are mostly the ancestor's, more than the folder holds itself.
	SourceMentions = "mentions"
)

// Independence is one item of evidence that a folder has an identity of its
// own: Key is what a decision cites (in independent), Line what the model
// reads.
type Independence struct {
	Key, Line string
}

// Keys is a list of strings that also reads a lone JSON string.
type Keys []string

// UnmarshalJSON reads a JSON string or an array of strings.
func (k *Keys) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		if strings.TrimSpace(one) == "" {
			*k = nil
		} else {
			*k = Keys{one}
		}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*k = many
	return nil
}

// AncestorNames are the ancestors' live folders, in order.
func (c Candidate) AncestorNames() []string {
	var out []string
	for _, an := range c.Ancestors {
		out = append(out, an.Folder)
	}
	return out
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
// evidence, then where folders alike went, then its ancestors.
func (c Candidate) Proposed() []RealRepo {
	out := append([]RealRepo(nil), c.Real...)
	alike := append([]RealRepo(nil), c.Alike...)
	for _, an := range c.Ancestors {
		if n, ok := strings.CutPrefix(an.Folder, "projects/"); ok {
			alike = append(alike, RealRepo{Name: n, Why: an.Why, Score: an.Score})
		}
	}
	for _, r := range alike {
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

// realAndAlike is Proposed without the ancestors (rendered on their own).
func (c Candidate) realAndAlike() []RealRepo {
	d := c
	d.Ancestors = nil
	return d.Proposed()
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
	// OwnerQuoteTemporal: the words of owner_quote, copied exactly, that
	// limit it in time ("for now", "until ..."), as the model reads them;
	// OwnerQuoteFinal: the words of it that make an end of the folder (drop,
	// kill, delete, replace), which a dead status on the quote needs
	// (CheckQuoteStatus).
	OwnerQuoteTemporal string `json:"owner_quote_temporal,omitempty"`
	OwnerQuoteFinal    string `json:"owner_quote_final,omitempty"`
	// Reject: for a proposed area or topic, the model's no (Why says why);
	// FoldInto: the existing folder it belongs to instead (it gains the
	// proposed name as an alias).
	Reject   bool   `json:"reject,omitempty"`
	FoldInto string `json:"fold_into,omitempty"`
	// Independent: for a folder with ancestors that stands alone (no
	// suppressed_by), the keys of the independence evidence it rests on.
	Independent Keys `json:"independent,omitempty"`
	// OwnerStatus: the owner's word on this folder's status in notes code
	// showed (words.go), each cited verbatim.
	OwnerStatus OwnerWordsList `json:"owner_status,omitempty"`
	// Words and Successor are code's (WordsOutcome), never the model's: the
	// owner's words the status rests on, and the live project replacing it.
	Words     *record.Entry `json:"-"`
	Successor string        `json:"-"`
}

// Prompt is the assessment prompt; %s is the candidates (Render).
const Prompt = `You are the curator of tacit, a fact store shared by coding agents on one owner's machines. Facts are filed in folders: projects/<name> (a git repository, or a project folder that is no repository), areas/<name> (an ongoing system or practice) and topics/<name> (a bounded work stream). Each folder gets a record that says what it is. Code gathered the evidence below for each folder; you decide.

For each folder decide:
- kind: "repo" (a git repository that is the owner's project), "folder" (a project folder of the owner's that is no git repository), or "none" (not a project of the owner's: an upstream or tool clone nobody works in, a folder that only holds other checkouts, the home directory, a scratch or task directory). For a proposed area or topic: "area" or "topic", or set reject or fold_into.
- status: "active" (being worked), "dormant" (real work, nobody on it lately), "shipped" (done and delivered), "dead" (abandoned or gone), or "needs-review" (the evidence does not settle what the folder is). Active or dormant is a question of dates, and code answers it: a folder with a line "status (code's): ..." has the status code set from the owner's activity days (its own commits here and on the forge, other machines' catalogs, the sessions that worked in it or filed its notes), measured against the folder's own rhythm; answer that status, which code writes in any case. Dates never make a folder dead or shipped: answer "dead" only on explicit evidence your why cites (the forge has it archived or deleted, a note shown for this folder that says it was removed or abandoned, as note:<name>, the owner's word on its status in owner_status, or the owner's words in owner_quote), and "shipped" only on a release, package or deploy shown for it together with the owner's statement or decision shown for it, cited by its ref; without that evidence code keeps its own status. Answer needs-review for such a folder only when what it is is not settled; for one with the owner's own commits git settles that (real work, theirs), so code keeps its status there too. A folder with no "status (code's)" line has no activity day code could find: its status comes from the owner's words and the notes; git says whether a folder is real work and whose, never whether it is shipped, dormant or dead.
- why: one or two sentences. For a folder with code's status, say what the folder is (its kind, its ancestor or its own identity, and for dead or shipped the evidence): code writes the reason of its status itself. Otherwise it MUST cite the sessions, notes or commits the decision rests on, as code shows them for THIS folder (session:<id>, claude:<id>, codex:<id>, note:<name>, or commit:<sha> for the folder's own commits under the owner's git identity or by the owner's forge account, which code lists); never cite a session that is not about this folder just to have a citation. Code refuses a why that cites none it can find; a git date alone is no reason. A folder whose evidence shows no session at all (no work sessions, no fact sessions) needs no citation, and its status can then only be dead, dormant or needs-review: say that no session touched it.
- description: one line (at most 200 characters) saying what the folder is.
- body: optional, a few sentences: why the folder exists and what belongs in it.
- aliases: optional, other names the same project is known by, only names no other folder has: never the name of another folder listed here or holding facts (that folder gets its own decision with suppressed_by instead).
- suppressed_by: optional, the folder that replaces this one when its facts belong to another project: the same project under another name (a symlink's or a remote's name, a rename), or a task worktree, clone, scratch or task folder or subfolder of another project. Give such a folder kind repo or folder (not none) with suppressed_by that project's folder, so its facts move there. The target must have or get a record and not itself be replaced.
- independent: for a folder with "ancestor candidates" that stands alone (no suppressed_by), the keys of the "independence" items it rests on, e.g. ["commits"].
- roots: kind folder only, optional: [{"host": ..., "path": ...}] where it lives.
- tools, commands: an area only: MCP tool-name patterns (globs on mcp__ names such as "mcp__*__Ticketing*"; a harness's own tools such as Bash or Edit are every session's, never an area's) and command prefixes its sessions used, only ones the evidence lists.
- repos: a topic only, optional: repositories it is tied to.
- owner_quote, owner_quote_from: only when the owner's own words shown below state the decision for THIS folder (its name or alias in them): copy their words exactly and the ref they came from. Words that only mention the folder are evidence for your why, not a quote here. Read what the words say of its status as the owner meant it: stopping work, banking it, pausing it or putting it off ("stop", "bank what you have", "pause", "for now", "not now") is dormant, not dead: the owner can take it up again. Dead needs permanence: words of theirs that end it (drop it, kill it, delete it, replace it with another) or evidence code shows (the forge has the repository deleted or archived). Code dates the words by when the owner said them (the turn's day, not today) and lets the folder's dates stand when the owner really worked on it after that day.
- owner_quote_temporal: with owner_quote, the words of it, copied exactly, that limit it in time ("for now", "until the release", "this week"), when it has any; "" when nothing in it does. Code refuses dead on words you flag here.
- owner_quote_final: with owner_quote and status dead, the words of it, copied exactly, that make an end of the folder (drop, kill, delete, remove, replace with another), unless the forge shows the repository deleted or archived. Code refuses dead on owner_quote without them.
- reject (true) or fold_into ("<existing folder>"): a proposed area or topic only, when it should not be made, with why.
- owner_status: optional, [{"says": ..., "words": ..., "from": "note:<name>", "successor": ...}], one item for each note code lists for THIS folder ("notes in other folders that name it") that is the owner's ruling, statement, words or decision and states THIS folder's status: says "dead" (obsolete, abandoned, removed, to be deleted, replaced by another), "shipped" (done and delivered) or "dormant" (paused, stopped or banked for now: words that stop or put off work without ending it are dormant, never dead); temporal: the words of the item, copied exactly, that limit it in time ("for now", "until ..."), "" when none (code refuses dead on words you flag here); words copied exactly from that note (the part that says it); successor, for dead only, the project the words say replaces this one, when they name one: the words you copy must name it and THIS folder (its name or another name of it) themselves, in the same words (the rest of the note, a related plan, or a project the same words retire with this folder never names a successor; with no successor in the words, give none, which is dead with no successor); give its folder name as code lists it after the note ("names projects: ..."; a name renamed or suppressed into another leads there). When the words say another project replaces this one, the successor is part of the item. A note that lists several folders states the status of each it names that way. Words that only mention the folder, or say something else of it, are no item. The owner's word beats dates: code takes the newest item's status (and successor) over its dates unless the owner really worked on it after the day of the words (sessions that worked in it, commits changing more than dependency, CI or version files), which code checks; answer status as the newest item says. A successor is another project that took this one's place: this folder stays, dead, its facts kept as its history, and the record links to the successor; it is not suppressed_by, which is for the same project under another name or a part of one (its facts move there).

Find the ancestor. A folder that is clearly related to another is part of it: worktrees, clones, scratch folders, task folders and subfolders almost always belong to a project. Code lists such a folder's "ancestor candidates", best first, with the evidence (git metadata of a worktree or clone, paths that put the folder inside the ancestor's tree, a name that contains the ancestor's, sessions that worked in both with facts that point at it). Such a folder is presumed part of one of them: set suppressed_by to the one its evidence supports. It stands alone (any decision without suppressed_by: a status of its own, needs-review or kind none) only on evidence of an identity of its own, which code lists as "independence" items (its own commits, a remote or forge repository of its own that no ancestor has, work done in a folder of its own name across sessions): cite their keys in independent. Code refuses a folder with ancestor candidates that stands alone without such a citation, and when code lists no independence item it must take one of the candidates. One whose independence shows its own commits and a remote or forge repository of its own is another repository than its ancestors: it stands alone, and code refuses folding it into one of them. Code resolves chains (a part of b, b part of c: c is the ancestor, shown "by way of b"), and the evidence includes where the store talks about the folder: most of the facts of other project folders that name it are the ancestor's. Folders that share an ancestor come in one batch, the ancestor first: decide them together and alike.

No evidence, no guess. A folder marked "evidence of what it is: none" has no ancestor candidate, no candidate real repository, no identity of its own and no session behind it: whether it is a project and whether it is worked is not in the evidence. Answer needs-review (code does so for any other answer), unless a note shown for it, or the owner's words, settles it: then cite that note:<name> in why.

Dry claims are checked by code, not judged. Whether a repository of a name exists and what it is called now has one source of truth: code checked each folder's name against every machine's checkouts, the checkouts' remotes and the forge (lines "check: ..."), and code refuses a decision a check refutes, showing the check:
- A project folder whose name exists nowhere (no checkout or other name of one, no remote, the forge has no such repository) is no repository under that name: it cannot be active, dormant, shipped or dead as it is. Set suppressed_by to the real repository its facts belong to (code lists candidates with their evidence: the checkouts its sessions worked in, repositories whose paths or remotes its facts name, near names; pick the one the evidence supports), or answer needs-review when the evidence picks none. Only when code lists no candidate may it be kind none.
- A name the forge says was renamed is an old name: fold it into the current name, never the current name into it.
- Never set suppressed_by to a repository whose name exists nowhere.
- Names code proved are one repository (another name of a checkout, a symlink's or the origin remote's; clones with one origin) get one live folder: the others set suppressed_by to it, or answer needs-review. They come in one batch, so decide them together. Which name it goes by is code's when a line "check: the canonical name of this repository is X" says so (the owner's decision first, then the main checkout's directory, then the forge's name): X is the live folder and the other names fold into it; code refuses the other direction. Without such a line it is yours to decide from the evidence. The forge's current name is kept on the record and shown beside the folder's name, so a repository whose GitHub name differs needs no folder of that name.
Code checked the forge for each folder's repository before asking ("forge (checked)" lines: last push, archived, latest release, the newest commits with their authors' accounts). Commits the forge lists by the owner's forge account (code says which) are the folder's own commits: cite them as commit:<sha>, as for a checkout's. An archived repository was archived by its owner, and a deleted one is gone: dormant, shipped or dead by the rest of the evidence, never active.
A folder left for review comes back on later runs with "extra evidence" code gathered for it, wider each time: facts in other folders that name it, the sessions behind them, the decisions on folders alike (near names, the same forge owner) and the owner's turns that name it. needs-review is not a place to park a folder: nobody promises the owner will review it. Decide from that evidence whenever it settles the folder; answer needs-review again only when even it does not, and then say in why what evidence would settle it. A session listed there filed a fact naming the folder and may be cited for it.
Folders whose names follow one pattern (kv-013-g1, kv-140-g2) come together, with the decisions already made on folders like them: decide alike folders alike, and when one differs say in why what in its evidence differs.

Output ONLY a JSON array, one object per folder, no prose, no code fence:
  {"folder": "...", "kind": "...", "status": "...", "why": "...", "description": "...", "body": "...", "aliases": [], "suppressed_by": "", "roots": [], "tools": [], "commands": [], "repos": [], "owner_quote": "", "owner_quote_from": "", "owner_quote_temporal": "", "owner_quote_final": "", "reject": false, "fold_into": "", "independent": [], "owner_status": []}

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
		if c.Activity != nil {
			for _, l := range c.Activity.Lines() {
				b.WriteString(l + "\n")
			}
		}
		if c.Canonical != "" && (len(c.Same) > 0 || c.Canonical != c.Folder[strings.LastIndex(c.Folder, "/")+1:]) {
			fmt.Fprintf(&b, "check: the canonical name of this repository is %s (%s)\n", c.Canonical, strings.Join(c.CanonicalWhy, "; "))
		}
		if real := c.realAndAlike(); len(real) > 0 {
			var parts []string
			for _, r := range real {
				parts = append(parts, fmt.Sprintf("%s (%s)", r.Name, strings.Join(r.Why, "; ")))
			}
			b.WriteString("candidate real repositories (code's, each exists by its check): " + strings.Join(parts, ", ") + "\n")
		} else if c.Absent() {
			b.WriteString("candidate real repositories: none found in the evidence\n")
		}
		if len(c.Ancestors) > 0 {
			var parts []string
			for i, an := range c.Ancestors {
				parts = append(parts, fmt.Sprintf("%d. %s [%s] (%s)", i+1, an.Folder, strings.Join(an.Sources, "+"), strings.Join(an.Why, "; ")))
			}
			b.WriteString("ancestor candidates (code's, best first; this folder is presumed part of one of them): " + strings.Join(parts, ", ") + "\n")
			if len(c.Independence) == 0 {
				b.WriteString("independence: none found (no own commits, no remote or forge repository of its own, no work in a folder of its own name): it cannot stand alone; set suppressed_by to an ancestor\n")
			}
			for _, in := range c.Independence {
				fmt.Fprintf(&b, "independence [%s]: %s\n", in.Key, in.Line)
			}
		}
		if c.NoEvidence {
			b.WriteString("evidence of what it is: none (no ancestor candidate, no candidate real repository, no identity of its own: no own commits, no remote or forge repository of its own, no work in a folder of its name; no session worked in it or filed its facts): code's outcome is needs-review, asked again with wider evidence; another decision only when its why cites a note:<name> shown here, or the owner's words, that settles it\n")
		}
		for _, l := range c.noteLines() {
			b.WriteString(l + "\n")
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
	if w := d.Words; w != nil && kind != record.KindArea && kind != record.KindTopic {
		entry.Words, entry.WordsFrom, entry.WordsOn = w.Words, w.WordsFrom, w.WordsOn
	}
	if owner != nil {
		entry.By, entry.Quote, entry.QuoteFrom = record.ByOwner, owner.Quote, owner.From
	}
	var r *record.Record
	if c.Old != nil {
		r = c.Old.Clone()
		if status != r.Status || kind != r.Kind || newBasis(r.Was, entry) {
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
	// The successor goes with the status that names it: a dead project's,
	// set by code from the owner's words, gone with any other status.
	switch {
	case status != record.StatusDead || r.SuppressedBy != "":
		r.Successor = ""
	case d.Successor != "":
		r.Successor = d.Successor
	case d.Words != nil || d.Kind == KindNone:
		r.Successor = ""
	}
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

// WordsDay is the day of the owner's words a record entry's status rests
// on: its words' day (store format 3), or the day in a why code wrote on
// their words (WordsOutcome, QuoteOutcome); "" when it rests on none.
func WordsDay(e record.Entry) string {
	if day, ok := e.RestsOnWords(); ok {
		return day
	}
	pre := WordsWhy + e.Status + " on the owner's words of "
	if rest, ok := strings.CutPrefix(e.Why, pre); ok && len(rest) >= 10 && fact.IsDate(rest[:10]) {
		return rest[:10]
	}
	return ""
}

// newBasis reports a decision whose status is the record's but rests on
// other words of the owner's, or the same words dated now (fresh), than its
// last entry: a new entry says so, so the record never keeps a day or words
// a later check corrected.
func newBasis(was []record.Entry, fresh record.Entry) bool {
	day := WordsDay(fresh)
	if len(was) == 0 || day == "" {
		return false
	}
	old := was[0]
	return WordsDay(old) != day || old.Quote != fresh.Quote || old.QuoteFrom != fresh.QuoteFrom || old.Words != fresh.Words || old.WordsFrom != fresh.WordsFrom
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

// isGroupHead reports the folder a group is named for.
func isGroupHead(c Candidate) bool {
	return c.Group != "" && c.Folder[strings.LastIndex(c.Folder, "/")+1:] == c.Group
}

// SortCandidates puts a name that exists before one that exists nowhere
// (the folders others may be suppressed into first; a group as one), then
// the names of one repository or the folders of one ancestor together (Group) and the folders of one name pattern together,
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
	// A group (one repository's names, the folders of one ancestor) sorts as
	// one: it waits to the end only when every one of them exists nowhere.
	present := map[string]bool{}
	for _, c := range cands {
		if c.Group != "" && !c.Absent() {
			present[key(c)] = true
		}
	}
	absent := func(c Candidate) bool {
		if c.Group != "" {
			return !present[key(c)]
		}
		return c.Absent()
	}
	sort.SliceStable(cands, func(i, j int) bool {
		bi, bj := absent(cands[i]), absent(cands[j])
		if bi != bj {
			return !bi
		}
		ki, kj := key(cands[i]), key(cands[j])
		if ki != kj {
			return ki < kj
		}
		// The ancestor first, then the folders nearest it: the others fold
		// into it, so it is decided in the first batch of the group.
		if di, dj := cands[i].Depth, cands[j].Depth; di != dj {
			return di < dj
		}
		// The group's own folder (the repository the names are of) first.
		if gi, gj := isGroupHead(cands[i]), isGroupHead(cands[j]); gi != gj {
			return gi
		}
		ai, aj := aliasOf(cands[i].Folder) != "", aliasOf(cands[j].Folder) != ""
		if ai != aj {
			return !ai
		}
		return cands[i].Folder < cands[j].Folder
	})
}
