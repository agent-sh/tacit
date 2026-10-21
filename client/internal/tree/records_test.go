package tree

import (
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
