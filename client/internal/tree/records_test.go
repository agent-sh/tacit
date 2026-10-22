package tree

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/record"
)

func rec(kind, folder, status string) *record.Record {
	name := folder[strings.LastIndex(folder, "/")+1:]
	return &record.Record{Kind: kind, Name: name, Description: "what " + name + " is", Status: status,
		Was:     []record.Entry{{Status: status, Date: "2026-10-21", By: record.ByOwner, Why: "x"}},
		Created: "2026-10-21", CreatedBy: record.ByOwner, Folder: folder}
}

// recordTree: projects widget (active, alias widget-old), old-widget
// (suppressed into widget, one fact left), gadget (dead); areas ops
// (active, topics q4 active and q3 shipped) and legacy (dormant); topics
// launch (active) and tied (tied to repo gadget).
func recordTree() ([]*fact.Fact, map[string]*record.Record) {
	recs := map[string]*record.Record{}
	add := func(r *record.Record) *record.Record { recs[r.Folder] = r; return r }
	add(rec("repo", "projects/widget", "active")).Aliases = []string{"widget-old"}
	add(rec("repo", "projects/old-widget", "active")).SuppressedBy = "projects/widget"
	add(rec("repo", "projects/gadget", "dead"))
	add(rec("area", "areas/ops", "active"))
	add(rec("topic", "areas/ops/topics/q4", "active"))
	add(rec("topic", "areas/ops/topics/q3", "shipped"))
	add(rec("area", "areas/legacy", "dormant"))
	add(rec("topic", "topics/launch", "active"))
	add(rec("topic", "topics/tied", "active")).Repos = []string{"gadget"}
	facts := []*fact.Fact{
		mk("projects/widget/widget-port.md", "machine_fact", "The widget server listens on port 7781", "2026-04-01"),
		mk("projects/old-widget/old-left.md", "history", "A fact the migration has not moved yet", "2026-04-01"),
		mk("projects/gadget/gadget-build.md", "design_decision", "Gadget builds with make", "2026-04-03"),
		mk("areas/ops/ops-pager.md", "user_standard", "Page the on-call through the pager tool", "2026-05-01"),
		mk("areas/ops/topics/q4/q4-cutover.md", "design_decision", "The Q4 cutover runs on a Sunday", "2026-05-02"),
		mk("topics/launch/launch-date.md", "history", "The launch moved to November", "2026-05-03"),
	}
	return facts, recs
}

func TestGenerateShowsRecords(t *testing.T) {
	facts, recs := recordTree()
	gen := GenerateWith(facts, recs, "")
	root := gen["_index.md"]
	if i, j, k := strings.Index(root, "[projects/]"), strings.Index(root, "[areas/]"), strings.Index(root, "[topics/]"); i < 0 || j < i || k < j {
		t.Fatalf("root order:\n%s", root)
	}
	p := gen["projects/_index.md"]
	for _, s := range []string{"- [widget/](widget/_index.md) repo, active; 1 fact", "- [gadget/](gadget/_index.md) repo, dead; 1 fact", "- old-widget/ -> projects/widget (suppressed)"} {
		if !strings.Contains(p, s) {
			t.Errorf("projects index lacks %q:\n%s", s, p)
		}
	}
	if strings.Index(p, "widget/](") > strings.Index(p, "gadget/](") || strings.Index(p, "gadget/](") > strings.Index(p, "old-widget/ ->") {
		t.Errorf("live, then quiet, then suppressed:\n%s", p)
	}
	w := gen["projects/widget/_index.md"]
	if !strings.Contains(w, "Record: repo, active: what widget is") || !strings.Contains(w, "Also known as: widget-old") {
		t.Errorf("widget index:\n%s", w)
	}
	if o := gen["projects/old-widget/_index.md"]; !strings.Contains(o, SuppressedLine+"projects/widget") {
		t.Errorf("the suppressed folder's index names its winner:\n%s", o)
	}
	a := gen["areas/ops/_index.md"]
	if !strings.Contains(a, "- [topics/](topics/_index.md) 1 fact") || !strings.Contains(a, "topics: q3, q4") {
		t.Errorf("an area lists its topics:\n%s", a)
	}
	if _, ok := gen["areas/ops/topics/q3/_index.md"]; !ok {
		t.Error("a record with no facts has no index")
	}
	// Without records the tree is what it was.
	if plain := Generate(facts, ""); strings.Contains(plain["projects/_index.md"], "repo, active") {
		t.Error("records shown without records")
	}
}

func writeTree(t *testing.T, facts []*fact.Fact, recs map[string]*record.Record) string {
	t.Helper()
	dir := t.TempDir()
	w, r, _ := Plan(dir, GenerateWith(facts, recs, ""))
	if err := Apply(dir, w, r); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestReadFollowsSuppressionAndReadsAreasAndTopics(t *testing.T) {
	facts, recs := recordTree()
	dir := writeTree(t, facts, recs)
	o := ReadOptions{Store: dir, Session: Session{Repos: []string{"widget"}, Host: "box", Records: recs}}
	out, err := Read(o, "projects/old-widget", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "projects/old-widget is suppressed by projects/widget") || strings.Join(blocks(out), "|") != "projects/widget: projects/widget/_index.md|projects/old-widget: projects/old-widget/_index.md" {
		t.Fatalf("redirect:\n%s", out)
	}
	if !strings.Contains(out, "widget-port") || !strings.Contains(out, "old-left") {
		t.Errorf("both folders' facts:\n%s", out)
	}
	// The session reads live areas and their live topics, and free topics
	// not tied to another repository.
	out, err = Read(o, "", 4)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(blocks(out), "|")
	for _, want := range []string{"areas/ops: areas/ops/_index.md", "areas/ops/topics/q4:", "topics/launch:"} {
		if !strings.Contains(got, want) {
			t.Errorf("default read lacks %s: %s", want, got)
		}
	}
	for _, bad := range []string{"areas/legacy:", "areas/ops/topics/q3:", "topics/tied:", "projects/gadget:"} {
		if strings.Contains(got, bad) {
			t.Errorf("default read has %s: %s", bad, got)
		}
	}
	// Quiet ones are listed, findable when named.
	if !strings.Contains(out, "- [legacy/](legacy/_index.md) area, dormant") {
		t.Errorf("the dormant area is not listed:\n%s", out)
	}
	if out, err := Read(o, "areas/ops/topics/q3", 0); err != nil || !strings.Contains(out, "topic, shipped") {
		t.Errorf("a named shipped topic: %v\n%s", err, out)
	}
	// A topic tied to a repository is read in that repository's sessions.
	g := ReadOptions{Store: dir, Session: Session{Repos: []string{"gadget"}, Host: "box", Records: recs}}
	if out, _ := Read(g, "topics", 1); !strings.Contains(strings.Join(blocks(out), "|"), "topics/tied:") {
		t.Errorf("the tied topic in its repository's session:\n%s", out)
	}
	s := o.Session
	if got := strings.Join(s.Scopes(), ","); got != "system,user,projects/widget,derived/widget,machines/box,areas/ops,areas/ops/topics/q4,topics/launch" {
		t.Errorf("scopes %s", got)
	}
	for p, want := range map[string]string{"areas/ops": "areas/ops", "areas/ops/topics/q4": "areas/ops/topics/q4", "areas/ops/topics": "areas/ops", "topics/launch": "topics/launch", "areas": ""} {
		if Named(p) != want {
			t.Errorf("Named(%q) = %q", p, Named(p))
		}
	}
	if sc, err := Scope("areas/ops/topics/q4"); err != nil || sc != "areas/ops/topics/q4" {
		t.Errorf("Scope: %q %v", sc, err)
	}
}

// The folder's overview follows its index at depth 0 only.
func TestReadShowsTheOverviewAtDepthZero(t *testing.T) {
	facts, recs := recordTree()
	dir := writeTree(t, facts, recs)
	ov := "---\nfolder: projects/widget\nwritten: 2026-10-21\nfacts: 1\nbasis: x\nrulings: y\nby: opus\n---\n\nThe widget server listens on 7781 [[widget-port]].\n"
	if err := os.WriteFile(filepath.Join(dir, "projects", "widget", OverviewFile), []byte(ov), 0o644); err != nil {
		t.Fatal(err)
	}
	o := ReadOptions{Store: dir, Session: Session{Repos: []string{"widget"}, Host: "box", Records: recs}}
	out, err := Read(o, "projects/widget", 0)
	if err != nil || !strings.Contains(out, "=== overview of projects/widget: projects/widget/_overview.md ===\nThe widget server listens on 7781 [[widget-port]].") || strings.Contains(out, "basis: x") {
		t.Fatalf("depth 0: %v\n%s", err, out)
	}
	if out, _ := Read(o, "projects/widget", 1); strings.Contains(out, "overview of") {
		t.Fatalf("depth 1 shows the overview:\n%s", out)
	}
	if out, _ := Read(o, "projects", 0); strings.Contains(out, "overview of") {
		t.Fatalf("the parent shows a child's overview:\n%s", out)
	}
}

// An org's folder lists its member projects (the records' org) and is read
// by default in a session in one of them, and only named elsewhere; a
// project whose forge name differs from the folder's shows both.
func TestOrgFoldersAndForgeNames(t *testing.T) {
	recs := map[string]*record.Record{}
	add := func(r *record.Record) *record.Record { recs[r.Folder] = r; return r }
	add(rec("org", "orgs/agent-sh", "active"))
	add(rec("repo", "projects/tower-crane", "active")).Org = "agent-sh"
	add(rec("repo", "projects/agnix", "active")).Org = "agent-sh"
	p := add(rec("repo", "projects/pikadon", "active"))
	p.Forge = "github.com/avifenesh/ginza"
	facts := []*fact.Fact{
		mk("orgs/agent-sh/release-flow.md", "user_standard", "Every agent-sh repository releases from a signed tag", "2026-10-20"),
		mk("projects/tower-crane/tc-build.md", "design_decision", "tower-crane builds with cargo", "2026-10-20"),
		mk("projects/pikadon/pk-port.md", "machine_fact", "pikadon listens on 7781", "2026-10-20"),
	}
	dir := writeTree(t, facts, recs)
	in := ReadOptions{Store: dir, Session: Session{Repos: []string{"tower-crane"}, Host: "box", Records: recs}}
	out, err := Read(in, "", 3)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "orgs/agent-sh: orgs/agent-sh/_index.md") || !strings.Contains(out, "Member projects (2): agnix, tower-crane") || !strings.Contains(out, "release-flow") {
		t.Fatalf("the org in its member's session:\n%s", out)
	}
	if got := strings.Join(in.Session.Scopes(), ","); !strings.Contains(got, "orgs/agent-sh") {
		t.Fatalf("scopes %s", got)
	}
	away := ReadOptions{Store: dir, Session: Session{Repos: []string{"pikadon"}, Host: "box", Records: recs}}
	out, _ = Read(away, "", 3)
	if strings.Contains(out, "release-flow") {
		t.Fatalf("the org read outside its projects:\n%s", out)
	}
	if out, err := Read(away, "orgs/agent-sh", 0); err != nil || !strings.Contains(out, "release-flow") {
		t.Fatalf("a named org: %v\n%s", err, out)
	}
	if out, _ := Read(away, "projects", 1); !strings.Contains(out, "- [pikadon/](pikadon/_index.md) (GitHub: ginza) repo, active") {
		t.Fatalf("the forge name beside the folder's:\n%s", out)
	}
	if out, _ := Read(away, "projects/pikadon", 0); !strings.Contains(out, "Name: pikadon (GitHub: ginza), https://github.com/avifenesh/ginza") {
		t.Fatalf("the record's name line:\n%s", out)
	}
}
