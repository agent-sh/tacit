package record

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const today = "2026-10-21"

func repoRec(name string) *Record {
	return &Record{
		Kind: KindRepo, Name: name, Description: "the " + name + " service", Status: StatusActive,
		Was:     []Entry{{Status: StatusActive, Date: today, By: ByOpus, Why: "assessed: 2 sessions with edits, claude:s1 claude:s2"}},
		Created: today, CreatedBy: ByOpus, Folder: "projects/" + name, Body: "Why it exists.",
	}
}

func known(c string) bool {
	return c == "claude:s1" || c == "claude:s2" || c == "codex:s3" || c == "note:n1"
}

func env(recs ...*Record) Env {
	m := map[string]*Record{}
	for _, r := range recs {
		m[r.Folder] = r
	}
	return Env{Today: today, Records: m, KnownSession: known}
}

func rules(ps []Problem) string {
	var out []string
	for _, p := range ps {
		out = append(out, p.String())
	}
	return strings.Join(out, "; ")
}

func expect(t *testing.T, ps []Problem, want string) {
	t.Helper()
	if want == "" {
		if len(ps) > 0 {
			t.Fatalf("refused: %s", rules(ps))
		}
		return
	}
	if !strings.Contains(rules(ps), want) {
		t.Fatalf("want a problem with %q, got %q", want, rules(ps))
	}
}

func TestParseEncodeRoundTrip(t *testing.T) {
	r := repoRec("widget")
	n := 3
	r.Evidence = []Evidence{{Host: "box", Gathered: today, GitRoot: true, Checkout: "main", Remotes: []string{"github.com/acme/widget"}, OwnCommits: &n, FirstCommit: "2026-01-02", LastCommit: "2026-10-20", WorkSessions: 2}}
	r.Aliases = []string{"widget-old"}
	b, err := r.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse("projects/widget", b)
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := got.Encode()
	if string(b) != string(b2) || got.Evidence[0].OwnCommits == nil || *got.Evidence[0].OwnCommits != 3 || got.Body != "Why it exists." {
		t.Fatalf("round trip:\n%s\n%s", b, b2)
	}
	if !strings.HasPrefix(string(b), "---\nkind: repo\nname: widget\n") {
		t.Fatalf("key order: %s", b)
	}
	if _, err := Parse("projects/widget", []byte("---\nkind: repo\nname: widget\nsurprise: 1\n---\n")); err == nil {
		t.Fatal("an unknown key was accepted")
	}
	if _, err := Parse("projects/widget", []byte("no frontmatter")); err == nil {
		t.Fatal("no frontmatter accepted")
	}
}

func TestValidateNewRecord(t *testing.T) {
	expect(t, Validate(nil, repoRec("widget"), ByOpus, env()), "")
	r := repoRec("widget")
	r.Kind = KindArea
	expect(t, Validate(nil, r, ByOpus, env()), "kind: does not match the folder")
	r = repoRec("widget")
	r.Name = "gadget"
	expect(t, Validate(nil, r, ByOpus, env()), "name: is not the folder's last part")
	r = repoRec("widget")
	r.Status, r.Was[0].Status = "finished", "finished"
	expect(t, Validate(nil, r, ByOpus, env()), "status: not an allowed status")
	r = repoRec("widget")
	r.Was[0].Why = "last commit 2026-10-20"
	expect(t, Validate(nil, r, ByOpus, env()), "cites no session, note or commit code can find")
	r.Was[0].Why = "worked in claude:unknown-session"
	expect(t, Validate(nil, r, ByOpus, env()), "cites no session, note or commit code can find")
	r = repoRec("widget")
	r.Was[0].Date = "2026-10-01"
	expect(t, Validate(nil, r, ByOpus, env()), "a new entry is dated today")
	r = repoRec("widget")
	r.Was = append(r.Was, Entry{Status: StatusDormant, Date: "2026-01-01", By: ByOpus, Why: "x claude:s1"})
	expect(t, Validate(nil, r, ByOpus, env()), "a new record starts with exactly one entry")
	// The owner's channel only.
	r = repoRec("widget")
	r.CreatedBy, r.Was[0].By = ByOwner, ByOwner
	expect(t, Validate(nil, r, ByOpus, env()), "was[0].by")
	expect(t, Validate(nil, r, ByOwner, env()), "")
	r = repoRec("widget")
	r.Was[0].Why = "the owner made it"
	r.CreatedBy, r.Was[0].By = ByOwner, ByOwner
	expect(t, Validate(nil, r, ByOwner, env()), "") // the owner cites nothing
}

func TestValidateOwnerQuote(t *testing.T) {
	backed := func(q, from string) bool { return from == "claude:s1" && strings.HasPrefix(q, "the real name") }
	e := env(repoRec("gadget"))
	e.QuoteBacked = backed
	r := repoRec("widget")
	r.CreatedBy, r.Was[0].By = ByOwner, ByOwner
	r.Was[0].Quote, r.Was[0].QuoteFrom = "the real name of widget is widget for good", "claude:s1"
	expect(t, Validate(nil, r, ByOpus, e), "") // their words, about this folder, backed
	r.Was[0].Quote = "the real name of gadget is gadget for good"
	expect(t, Validate(nil, r, ByOpus, e), "was[0].by") // only mentions another folder
	r.Was[0].Quote, r.Was[0].QuoteFrom = "the real name of widget is widget", "claude:s2"
	expect(t, Validate(nil, r, ByOpus, e), "was[0].by") // not backed
}

func TestValidateHistoryIsAppendOnlyAndOwnerStatusKept(t *testing.T) {
	old := repoRec("widget")
	next := old.Clone()
	next.Status = StatusDormant
	next.Was = append([]Entry{{Status: StatusDormant, Date: today, By: ByOpus, Why: "no work since; claude:s2 was the last"}}, next.Was...)
	expect(t, Validate(old, next, ByOpus, env(old)), "")
	bad := next.Clone()
	bad.Was = bad.Was[:1]
	expect(t, Validate(old, bad, ByOpus, env(old)), "append only")
	bad = next.Clone()
	bad.Was[1].Why = "rewritten"
	expect(t, Validate(old, bad, ByOpus, env(old)), "append only")
	bad = old.Clone()
	bad.Status = StatusDead
	expect(t, Validate(old, bad, ByOpus, env(old)), "status: changes only with a new history entry")
	// The owner set the status: Opus may only flag it.
	owned := old.Clone()
	owned.Was = append([]Entry{{Status: StatusShipped, Date: today, By: ByOwner, Why: "done"}}, owned.Was...)
	owned.Status = StatusShipped
	flag := owned.Clone()
	flag.Status = StatusActive
	flag.Was = append([]Entry{{Status: StatusActive, Date: today, By: ByOpus, Why: "new work claude:s2"}}, flag.Was...)
	expect(t, Validate(owned, flag, ByOpus, env(owned)), "the owner set this folder's status")
	flag.Status, flag.Was[0].Status = StatusNeedsReview, StatusNeedsReview
	expect(t, Validate(owned, flag, ByOpus, env(owned)), "")
	if flag.Was[1].Status != StatusShipped {
		t.Fatal("the owner's value is not kept in was")
	}
	// A status on a quote of theirs moves to code's status from its dates
	// (the owner worked on it after the day of the words), and to nothing else.
	quoted := owned.Clone()
	quoted.Was[0].Quote, quoted.Was[0].QuoteFrom = "we are stopping widget for now", "claude:s1"
	dates := quoted.Clone()
	dates.Status = StatusActive
	dates.Was = append([]Entry{{Status: StatusActive, Date: today, By: ByOpus, Why: "code: active from its dates: the owner's last activity is ... Owner activity after the 2026-09-28 words (claude:s1 ...), so the dates stand"}}, dates.Was...)
	ce := env(quoted)
	ce.CodeStatus = func(string) string { return StatusActive }
	expect(t, Validate(quoted, dates, ByOpus, ce), "")
	ce.CodeStatus = func(string) string { return StatusDormant }
	expect(t, Validate(quoted, dates, ByOpus, ce), "the owner set this folder's status")
	// The owner's pointer stays.
	ptr := owned.Clone()
	ptr.SuppressedBy = "projects/gadget"
	expect(t, Validate(owned, ptr, ByOpus, env(owned, repoRec("gadget"))), "an agent does not change its suppression")
	expect(t, Validate(owned, ptr, ByOwner, env(owned, repoRec("gadget"))), "")
	c := owned.Clone()
	c.Created = "2026-01-01"
	expect(t, Validate(owned, c, ByOwner, env(owned)), "creation never changes")
}

func TestValidateAliasesAndSuppression(t *testing.T) {
	a, b := repoRec("alpha"), repoRec("beta")
	a.Aliases = []string{"alpha-old"}
	r := repoRec("gamma")
	r.Aliases = []string{"alpha-old"}
	expect(t, Validate(nil, r, ByOpus, env(a, b)), "is another folder's name or alias")
	r.Aliases = []string{"beta"}
	expect(t, Validate(nil, r, ByOpus, env(a, b)), "is another folder's name or alias")
	r.Aliases = []string{"gamma"}
	expect(t, Validate(nil, r, ByOpus, env(a, b)), "is the folder's own name")
	r.Aliases = []string{"Gamma_X"}
	expect(t, Validate(nil, r, ByOpus, env(a, b)), "not a slug")
	r = repoRec("alpha-old")
	expect(t, Validate(nil, r, ByOpus, env(a, b)), "name: is another folder's alias")

	r = repoRec("gamma")
	r.SuppressedBy = "projects/gamma"
	expect(t, Validate(nil, r, ByOpus, env(a, b)), "points at the folder itself")
	r.SuppressedBy = "projects/nothing"
	expect(t, Validate(nil, r, ByOpus, env(a, b)), "names a folder with no record")
	r.SuppressedBy = "projects/alpha"
	expect(t, Validate(nil, r, ByOpus, env(a, b)), "")
	sup := b.Clone()
	sup.SuppressedBy = "projects/alpha"
	r.SuppressedBy = "projects/beta"
	expect(t, Validate(nil, r, ByOpus, env(a, sup)), "itself suppressed: no chains")
	// Another folder points here: this one cannot fold away.
	a2 := a.Clone()
	a2.SuppressedBy = "projects/gamma"
	expect(t, Validate(a, a2, ByOwner, env(a, sup, repoRec("gamma"))), "another folder is suppressed into this one")
	area := areaRec("ops")
	r.SuppressedBy = "areas/ops"
	expect(t, Validate(nil, r, ByOpus, env(a, area)), "names a folder of another kind")
}

func areaRec(name string) *Record {
	return &Record{
		Kind: KindArea, Name: name, Description: "ticketing and on-call", Status: StatusActive,
		Was:     []Entry{{Status: StatusActive, Date: today, By: ByOpus, Why: "proposed from claude:s1 and codex:s3"}},
		Created: today, CreatedBy: ByOpus, Folder: "areas/" + name,
		Tools:    []string{"mcp__*__Ticketing*"},
		Commands: []string{"tickets list"},
	}
}

func TestValidateAreasAndTopics(t *testing.T) {
	e := env()
	used := map[string]bool{"s1|mcp__*__Ticketing*": true}
	e.ToolUsed = func(s, p string) bool { return used[s+"|"+p] }
	expect(t, Validate(nil, areaRec("ops"), ByOpus, e), "")
	one := areaRec("ops")
	one.Was[0].Why = "seen in claude:s1"
	expect(t, Validate(nil, one, ByOpus, e), "at least 2 distinct sessions")
	one.Evidence = []Evidence{{Sessions: []string{"claude:s1", "codex:s3"}}}
	expect(t, Validate(nil, one, ByOpus, e), "")
	tool := areaRec("ops")
	tool.Tools = []string{"mcp__*__Never*"}
	expect(t, Validate(nil, tool, ByOpus, e), "matched no tool call")
	tool.Tools = []string{"bad tool"}
	expect(t, Validate(nil, tool, ByOpus, e), "not a tool-name pattern")
	tool.Tools = []string{"*"}
	expect(t, Validate(nil, tool, ByOpus, e), "not a tool-name pattern")
	// A harness's own tool is every session's: not an area's, from anyone;
	// a record that already holds one keeps it.
	tool.Tools = []string{"Bash"}
	expect(t, Validate(nil, tool, ByOpus, e), "not an MCP tool-name pattern")
	expect(t, Validate(nil, tool, ByOwner, e), "not an MCP tool-name pattern")
	held := areaRec("ops")
	held.Tools = []string{"Bash"}
	next := held.Clone()
	next.Description = "ticketing, on-call and the pager"
	expect(t, Validate(held, next, ByOwner, e), "")
	tool.Tools, tool.Commands = nil, []string{" padded"}
	expect(t, Validate(nil, tool, ByOpus, e), "not a command prefix")
	// The owner needs no sessions.
	own := areaRec("ops")
	own.CreatedBy, own.Was[0].By, own.Was[0].Why = ByOwner, ByOwner, "I want this"
	expect(t, Validate(nil, own, ByOwner, e), "")

	topic := &Record{Kind: KindTopic, Name: "q4-move", Description: "the Q4 move", Status: StatusActive,
		Was:     []Entry{{Status: StatusActive, Date: today, By: ByOpus, Why: "claude:s1 claude:s2"}},
		Created: today, CreatedBy: ByOpus, Folder: "areas/ops/topics/q4-move", Repos: []string{"widget"}}
	e.KnownRepo = func(n string) bool { return n == "widget" }
	expect(t, Validate(nil, topic, ByOpus, e), "")
	twin := topic.Clone()
	twin.Folder = "topics/q4-move"
	expect(t, Validate(nil, twin, ByOpus, env(topic)), "a topic's name is unique")
	topic.Repos = []string{"nothing"}
	expect(t, Validate(nil, topic, ByOpus, e), "names no known repository")
	topic.Repos, topic.Tools = nil, []string{"x"}
	expect(t, Validate(nil, topic, ByOpus, e), "only an area has tools")
	r := repoRec("widget")
	r.Repos = []string{"widget"}
	expect(t, Validate(nil, r, ByOpus, e), "only a topic has repos")
	if FolderKinds("areas/ops/topics/x/y") != nil || FolderKinds("system") != nil || FolderKinds("projects/A") != nil {
		t.Fatal("FolderKinds accepted a folder that holds no record")
	}
}

func TestResolveAndChains(t *testing.T) {
	a, b, c := repoRec("alpha"), repoRec("beta"), repoRec("gamma")
	b.SuppressedBy = "projects/alpha"
	m := map[string]*Record{a.Folder: a, b.Folder: b, c.Folder: c}
	if w, ch, err := Resolve(m, "projects/beta"); err != nil || w != "projects/alpha" || len(ch) != 1 {
		t.Fatalf("resolve %s %v %v", w, ch, err)
	}
	if w, _, _ := Resolve(m, "projects/none"); w != "projects/none" {
		t.Fatal("a folder with no record resolves to itself")
	}
	if l := Losers(m, "projects/alpha"); len(l) != 1 || l[0] != "projects/beta" {
		t.Fatalf("losers %v", l)
	}
	if ps := Chains(m); len(ps) != 0 {
		t.Fatalf("chains %v", ps)
	}
	c.SuppressedBy = "projects/beta"
	if ps := Chains(m); len(ps) != 1 || !strings.Contains(ps[0].Rule, "itself suppressed") {
		t.Fatalf("a chain: %v", ps)
	}
	a.SuppressedBy = "projects/gamma"
	if _, _, err := Resolve(m, "projects/alpha"); !errors.Is(err, ErrCycle) {
		t.Fatalf("a loop: %v", err)
	}
	if RepoOwner(map[string]*Record{b.Folder: b, a.Folder: a}, "beta") != "projects/beta" {
		t.Fatal("RepoOwner")
	}
}

func TestWalk(t *testing.T) {
	root := t.TempDir()
	put := func(rel, s string) {
		os.MkdirAll(filepath.Join(root, filepath.Dir(rel)), 0o755)
		os.WriteFile(filepath.Join(root, rel), []byte(s), 0o644)
	}
	b, _ := repoRec("widget").Encode()
	put("projects/widget/_record.md", string(b))
	a, _ := areaRec("ops").Encode()
	put("areas/ops/_record.md", string(a))
	put("areas/ops/topics/q4/_record.md", "---\nkind: topic\nname: q4\nbogus: 1\n---\n")
	put("system/_record.md", string(b)) // no folder of records
	recs, errs := Walk(root)
	if len(recs) != 2 || recs["projects/widget"] == nil || recs["areas/ops"] == nil || len(errs) != 1 || !strings.Contains(errs[0].Error(), "areas/ops/topics/q4/_record.md") {
		t.Fatalf("recs %v errs %v", recs, errs)
	}
	if recs["projects/widget"].Folder != "projects/widget" {
		t.Fatal("folder not set")
	}
}

func TestCites(t *testing.T) {
	got := Cites("worked in claude:abc-1 and codex:d2, see note:n1; https://x.y and claude:abc-1 again")
	if strings.Join(got, ",") != "claude:abc-1,codex:d2,note:n1" {
		t.Fatalf("cites %v", got)
	}
	if !WholeWord("the real name of valkey-reviewer is", "valkey-reviewer") || WholeWord("valkey-reviewers", "valkey-reviewer") || WholeWord("x-valkey", "valkey") {
		t.Fatal("WholeWord")
	}
}

// A folder no session ever touched needs no citation for a status that
// claims no work; any status that does still needs one.
func TestValidateNoSessionFolder(t *testing.T) {
	r := repoRec("widget")
	r.Was[0].Why = "no session touched it; an upstream clone"
	r.Evidence = []Evidence{{Host: "box", Gathered: today, GitRoot: true}}
	for _, st := range []string{StatusDead, StatusDormant, StatusNeedsReview} {
		r.Status, r.Was[0].Status = st, st
		expect(t, Validate(nil, r, ByOpus, env()), "")
	}
	r.Status, r.Was[0].Status = StatusActive, StatusActive
	expect(t, Validate(nil, r, ByOpus, env()), "cites no session")
	r.Status, r.Was[0].Status = StatusDead, StatusDead
	r.Evidence[0].WorkSessions = 1
	expect(t, Validate(nil, r, ByOpus, env()), "cites no session")
}

// An alias may not be the name of a project folder that holds facts: that
// folder needs a record of its own, suppressed, so its facts move.
func TestValidateAliasOfAFolderWithFacts(t *testing.T) {
	r := repoRec("widget")
	r.Aliases = []string{"widget-old"}
	e := env()
	e.HoldsFacts = func(f string) bool { return f == "projects/widget-old" }
	expect(t, Validate(nil, r, ByOpus, e), "is the name of a project folder that holds facts")
	r.Aliases = []string{"widget-svc"}
	expect(t, Validate(nil, r, ByOpus, e), "")
}

// A dead project's successor is another live project with a record; the
// owner's words an entry rests on go together, dated no later than it.
func TestValidateSuccessorAndWords(t *testing.T) {
	succ := repoRec("crane")
	gone := repoRec("gishra")
	gone.SuppressedBy = "projects/crane"
	dead := func(s string) *Record {
		r := repoRec("oldtool")
		r.Status, r.Was[0].Status, r.Successor = StatusDead, StatusDead, s
		r.Was[0].Why = "code: dead on the owner's words of 2026-10-06, note:n1"
		r.Was[0].Words, r.Was[0].WordsFrom, r.Was[0].WordsOn = "to delete the plugins oldtool", "note:n1", "2026-10-06"
		return r
	}
	expect(t, Validate(nil, dead("projects/crane"), ByOpus, env(succ, gone)), "")
	expect(t, Validate(nil, dead("projects/gishra"), ByOpus, env(succ, gone)), "successor: names a folder that is suppressed")
	expect(t, Validate(nil, dead("projects/nothing"), ByOpus, env(succ, gone)), "successor: names a folder with no record")
	expect(t, Validate(nil, dead("projects/oldtool"), ByOpus, env(succ)), "successor: points at the folder itself")
	expect(t, Validate(nil, dead("areas/ops"), ByOpus, env(succ)), "successor: is no project folder")
	active := dead("projects/crane")
	active.Status, active.Was[0].Status = StatusActive, StatusActive
	expect(t, Validate(nil, active, ByOpus, env(succ)), "successor: only a dead project has a successor")
	w := dead("")
	w.Was[0].WordsOn = "2099-01-01"
	expect(t, Validate(nil, w, ByOpus, env(succ)), "words_on: not a day on or before the entry's")
	w = dead("")
	w.Was[0].WordsFrom = ""
	expect(t, Validate(nil, w, ByOpus, env(succ)), "words, words_from and words_on go together")
	w = dead("")
	w.Was[0].WordsFrom = "note:unknown"
	expect(t, Validate(nil, w, ByOpus, env(succ)), "words_from: names no note or quote code can find")
	if day, ok := dead("").Was[0].RestsOnWords(); !ok || day != "2026-10-06" {
		t.Fatalf("rests on words: %v %s", ok, day)
	}
	// The local layer's status takes its successor with it.
	l := dead("projects/crane")
	m := MergeLocal(map[string]*Record{"projects/oldtool": repoRec("oldtool")}, map[string]*Record{"projects/oldtool": l})
	if m["projects/oldtool"].Successor != "projects/crane" || !m["projects/oldtool"].LocalStatus {
		t.Fatalf("merge %+v", m["projects/oldtool"])
	}
	b, _ := dead("projects/crane").Encode()
	r, err := Parse("projects/oldtool", b)
	if err != nil || r.Successor != "projects/crane" || r.Was[0].WordsOn != "2026-10-06" {
		t.Fatalf("round trip %v %+v", err, r)
	}
}
