package links

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/fact"
)

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"gpu-notes":     "gpu-notes",
		"Gpu_Notes":     "gpu-notes",
		"gpu-notes.md":  "gpu-notes",
		"GPU__NOTES":    "gpu-notes",
		"a.b_c-d":       "a-b-c-d",
		"_lead":         "lead",
		"trail_":        "trail",
		"v1.2":          "v1-2",
		"codex-desktop": "codex-desktop",
	} {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q)=%q, want %q", in, got, want)
		}
	}
}

// A store tree in a temp dir, one fact per call.
type tree struct {
	t    *testing.T
	root string
}

func (tr *tree) add(scope, name, extra, body string) {
	tr.t.Helper()
	kind := "observed"
	if strings.Contains(extra, "kind: user") {
		kind = "user"
		extra = strings.ReplaceAll(extra, "kind: user\n", "")
	}
	text := "---\nname: " + name + "\ndescription: d\ntype: design_decision\nscope: " + scope + "\nsource:\n  harness: curator\n  host: host-a\n  kind: " + kind + "\ncreated: 2026-10-08\n" + extra + "---\n\n" + body + "\n"
	p := filepath.Join(tr.root, scope, name+".md")
	if strings.HasPrefix(scope, "inbox/") {
		p = filepath.Join(tr.root, scope, name+".md")
	}
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		tr.t.Fatal(err)
	}
}

func (tr *tree) plan(isRuling func(*fact.Fact) bool) *Plan {
	tr.t.Helper()
	facts, errs, err := fact.Walk(tr.root)
	if err != nil || len(errs) != 0 {
		tr.t.Fatalf("walk: %v %v", err, errs)
	}
	if isRuling == nil {
		isRuling = func(*fact.Fact) bool { return false }
	}
	p, err := Compute(tr.root, facts, isRuling)
	if err != nil {
		tr.t.Fatal(err)
	}
	return p
}

func fixOf(p *Plan, name string) *Fix {
	for _, fx := range p.Fixes {
		if fx.Name == name {
			return fx
		}
	}
	return nil
}

func TestSupersededChainsAndWhatIsLeftAlone(t *testing.T) {
	tr := &tree{t: t, root: t.TempDir()}
	tr.add("projects/x", "a", "superseded_by: b\n", "old a")
	tr.add("projects/x", "b", "superseded_by: c\n", "old b")
	tr.add("projects/x", "c", "", "the live one")
	tr.add("projects/x", "p", "superseded_by: q\n", "loop p")
	tr.add("projects/x", "q", "superseded_by: p\n", "loop q")
	tr.add("projects/x", "r", "retired: 2026-10-09\n", "retired, no successor")
	tr.add("inbox/claude@host-a", "ghost", "", "an inbox note")
	tr.add("inbox/claude@host-a", "inbox-dead", "", "points at [[nowhere]]")
	tr.add("projects/x", "chain", "", "two hops [[a]]; a loop [[p]]; retired [[r]]; inbox-only [[ghost]]; live [[c]]; dead via spelling [[Old_Name]]")
	tr.add("projects/x", "old-name", "superseded_by: c\n", "an old name")

	p := tr.plan(nil)
	fx := fixOf(p, "chain")
	if fx == nil {
		t.Fatalf("no fix for chain: %+v", p)
	}
	got := map[string]Edit{}
	for _, e := range fx.Edits {
		got[e.Target] = e
	}
	if len(got) != 2 {
		t.Fatalf("edits %+v, want a and Old_Name only (a loop, a retired fact and an inbox-only name resolve and are left alone)", fx.Edits)
	}
	if e := got["a"]; e.To != "c" || e.Why != Successor {
		t.Errorf("a: %+v", e)
	}
	if e := got["Old_Name"]; e.To != "c" || e.Why != Successor {
		t.Errorf("Old_Name spelled as old-name, superseded by c: %+v", e)
	}
	for _, f := range p.Fixes {
		if strings.HasPrefix(f.Path, "inbox/") {
			t.Errorf("an inbox note was planned: %s", f.Path)
		}
	}
	out := string(fx.Data)
	if !strings.Contains(out, "two hops [[c]]; a loop [[p]]; retired [[r]]; inbox-only [[ghost]]; live [[c]]; dead via spelling [[c]]") {
		t.Errorf("rewritten body:\n%s", out)
	}
}

func TestRepairIsIdempotentAndOnlyTouchesLinks(t *testing.T) {
	tr := &tree{t: t, root: t.TempDir()}
	tr.add("projects/x", "live", "", "live")
	body := "Line one  with   spaces\t[[gone|the label]] and [[gone#sec]] and [[live|kept]].\n\n```toml\n[[keys]]\n```\nInline `[[table]]` too.\n[[Live]]"
	tr.add("projects/x", "f", "updated: 2026-10-09\n", body)
	before, _ := os.ReadFile(filepath.Join(tr.root, "projects/x/f.md"))

	p := tr.plan(nil)
	fx := fixOf(p, "f")
	if fx == nil {
		t.Fatal("no fix")
	}
	if !IsRepair(before, fx.Data) {
		t.Fatalf("more than links changed:\n--- before\n%s--- after\n%s", before, fx.Data)
	}
	want := strings.Replace(string(before), "[[gone|the label]] and [[gone#sec]]", "the label and gone", 1)
	want = strings.Replace(want, "[[Live]]", "[[live]]", 1)
	if string(fx.Data) != want {
		t.Fatalf("got\n%s\nwant\n%s", fx.Data, want)
	}
	if err := os.WriteFile(filepath.Join(tr.root, "projects/x/f.md"), fx.Data, 0o644); err != nil {
		t.Fatal(err)
	}
	if p2 := tr.plan(nil); len(p2.Fixes)+len(p2.Rulings)+len(p2.Generated) != 0 {
		t.Fatalf("a second pass plans %+v", p2)
	}
}

func TestRulingsAndGeneratedFactsAreReportedNotEdited(t *testing.T) {
	tr := &tree{t: t, root: t.TempDir()}
	tr.add("projects/x", "rule", "kind: user\n", "owner says [[gone-a]] and [[gone-a]]")
	tr.add("projects/x", "plain", "", "see [[gone-b]]")
	tr.add("derived/x", "x-map", "derived_from: abc1234\n", "see [[gone-c]]")
	p := tr.plan(func(f *fact.Fact) bool { return f.Meta.Source.Kind == "user" })
	if len(p.Rulings) != 1 || p.Rulings[0].Name != "rule" || p.Rulings[0].Data != nil || p.Rulings[0].Edits[0].Count != 2 {
		t.Fatalf("rulings %+v", p.Rulings)
	}
	if len(p.Generated) != 1 || p.Generated[0].Name != "x-map" || p.Generated[0].Data != nil {
		t.Fatalf("generated %+v", p.Generated)
	}
	if len(p.Fixes) != 1 || p.Fixes[0].Name != "plain" {
		t.Fatalf("fixes %+v", p.Fixes)
	}
}

func TestReviewLineReadsBack(t *testing.T) {
	fx := &Fix{Name: "rule", Scope: "projects/x"}
	for _, e := range []Edit{
		{Target: "gone", Why: NoMatch, Detail: "no fact is named gone"},
		{Target: "old", To: "new", Why: Successor, Detail: "[[old]] is superseded by [[new]]"},
	} {
		l := Line(fx, e)
		if strings.Contains(l, "\u2014") {
			t.Errorf("em dash in %q", l)
		}
		got := Raised("## head\n\n" + l + "\n")
		if len(got) != 1 || got[0] != Key("rule", e.Target) {
			t.Errorf("%q reads back as %q", l, got)
		}
	}
}
