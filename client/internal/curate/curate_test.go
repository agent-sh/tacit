package curate

import (
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/fact"
)

func TestParseAndToFact(t *testing.T) {
	res := "Here you go:\n```json\n[{\"origin\":\"note-a\",\"name\":\"Root Stays On Main\",\"description\":\"The root webapp checkout stays on main; agents work in wt-* worktrees\",\"type\":\"user_standard\",\"scope\":\"projects/webapp\",\"kind\":\"user\",\"applies_to\":{\"repos\":[\"webapp\"],\"commands\":[\"git checkout\"]},\"body\":\"Owner 2026-09-04.\"},{\"origin\":\"\",\"description\":\"dropped: no origin\"},{\"origin\":\"note-a\",\"name\":\"\",\"description\":\"x\",\"type\":\"rumor\",\"scope\":\"../etc\",\"kind\":\"derived\"}]\n```"
	es := Parse(res)
	if len(es) != 2 {
		t.Fatalf("parsed %d entries", len(es))
	}
	taken := map[string]bool{}
	f := ToFact(es[0], "host-a", taken)
	if f.Path != "projects/webapp/root-stays-on-main.md" || f.Meta.Type != "user_standard" || f.Meta.Source.Kind != "user" || f.Meta.Origin != "note-a" || f.Meta.AppliesTo.Commands[0] != "git checkout" {
		t.Fatalf("fact %+v", f.Meta)
	}
	g := ToFact(es[1], "host-a", taken)
	if g.Meta.Scope != "user" || g.Meta.Type != "unclassified" || g.Meta.Source.Kind != "observed" || !fact.IsSlug(g.Meta.Name) {
		t.Fatalf("defaults %+v", g.Meta)
	}
	// Same name twice gets a suffix, never an overwrite.
	h := ToFact(es[0], "host-a", taken)
	if h.Meta.Name == f.Meta.Name || !strings.HasPrefix(h.Meta.Name, "root-stays-on-main-") {
		t.Fatalf("collision %q vs %q", f.Meta.Name, h.Meta.Name)
	}
	// A third time, the same description in another folder: the suffix is
	// the description's, so the suffixed name is taken by then too.
	e := es[0]
	e.Scope = "projects/mobile"
	k := ToFact(e, "host-a", taken)
	if k.Meta.Name == h.Meta.Name || k.Meta.Name == f.Meta.Name || !strings.HasPrefix(k.Meta.Name, h.Meta.Name+"-") || !fact.IsSlug(k.Meta.Name) {
		t.Fatalf("a third entry of one name and description is named %q, after %q and %q", k.Meta.Name, f.Meta.Name, h.Meta.Name)
	}
	if _, err := fact.Parse(f.Path, must(f.Encode())); err != nil {
		t.Fatalf("encoded fact does not parse strictly: %v", err)
	}
}

func TestBatchesAndRender(t *testing.T) {
	var notes []*fact.Fact
	for i := 0; i < 5; i++ {
		notes = append(notes, &fact.Fact{Path: "inbox/x/n.md", Meta: fact.Meta{Name: "n" + string(rune('a'+i)), Description: "d", Scope: "user", Source: fact.Source{Kind: "observed"}}, Body: strings.Repeat("x", 20_000)})
	}
	bs := Batches(notes)
	if len(bs) != 3 || len(bs[0]) != 2 {
		t.Fatalf("batches %v", len(bs))
	}
	r := Render(bs[0])
	if !strings.Contains(r, "### note name=na scope=user kind=observed") {
		t.Fatalf("render:\n%s", r[:120])
	}
}

func TestParseVerdicts(t *testing.T) {
	vs := ParseVerdicts("```json\n[\"contradicts\", \"no\", \"Supersedes\"]\n```", 4)
	want := []Verdict{Contradicts, NoConflict, Supersedes, NoConflict}
	for i := range want {
		if vs[i] != want[i] {
			t.Fatalf("verdicts %v", vs)
		}
	}
	if vs := ParseVerdicts("garbage", 2); vs[0] != NoConflict || vs[1] != NoConflict {
		t.Fatal("unparsable answer must retire nothing")
	}
	r := RenderPairs([]Pair{{ExistingName: "a", ExistingDesc: "A", NewName: "b", NewDesc: "B"}})
	if !strings.Contains(r, "1.\nEXISTING (a): A\nNEW (b): B") {
		t.Fatalf("render %q", r)
	}
}

func TestParseVerdict(t *testing.T) {
	for in, want := range map[string]Verdict{"Contradicts.": Contradicts, " supersedes\n": Supersedes, "no": NoConflict, "": NoConflict, "yes": NoConflict} {
		if got := ParseVerdict(in); got != want {
			t.Errorf("%q -> %q want %q", in, got, want)
		}
	}
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}
