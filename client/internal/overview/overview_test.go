package overview

import (
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/fact"
)

func TestFormNeedsACitationInEveryUnit(t *testing.T) {
	exists := func(n string) bool { return n == "a" || n == "b" }
	good := "The service retries three times [[a]].\nIt waits between tries [[a]] [[b]].\n\n### Rules\n\n- Never deploy on Fridays [[b]].\n1. Pin the tool [[a]]."
	if ps := Form(good, exists); len(ps) > 0 {
		t.Fatalf("good refused: %v", ps)
	}
	for body, want := range map[string]string{
		"A paragraph with none.\n\n- a bullet [[a]]": "cites no fact",
		"- a bullet [[a]]\n- another without":        "cites no fact",
		"Cites a ghost [[ghost]].":                   "names no fact",
		"":                                           "empty",
		strings.Repeat("x [[a]]\n\n", MaxBytes/8):    "bytes",
	} {
		if ps := Form(body, exists); !strings.Contains(strings.Join(ps, ";"), want) {
			t.Errorf("%.40q: %v, want %q", body, ps, want)
		}
	}
	if ps := Live("x [[a]] [[b]]", func(n string) bool { return n == "a" }); len(ps) != 1 || !strings.Contains(ps[0], "[[b]]") {
		t.Fatalf("live: %v", ps)
	}
}

func TestEncodeParseAndChange(t *testing.T) {
	f := func(name, created, updated, sup string) *fact.Fact {
		return &fact.Fact{Meta: fact.Meta{Name: name, Created: created, Updated: updated, SupersededBy: sup}}
	}
	live := []*fact.Fact{f("a", "2026-09-01", "", ""), f("b", "2026-09-02", "", "")}
	n, basis, rulings := Basis(live, func(x *fact.Fact) bool { return x.Meta.Name == "a" })
	o := &Overview{Meta: Meta{Folder: "projects/x", Written: "2026-10-01", Facts: n, Basis: basis, Rulings: rulings, Status: "active", By: "opus"}, Body: "It retries [[a]] and waits [[b]]."}
	b, err := o.Encode()
	if err != nil {
		t.Fatal(err)
	}
	back, err := Parse(b)
	if err != nil || back.Meta != o.Meta || back.Body != o.Body+"\n" {
		t.Fatalf("round trip: %+v %v\n%s", back, err, b)
	}
	if _, err := Parse([]byte("---\nfolder: x\nwritten: y\nbasis: z\nextra: 1\n---\n\nbody\n")); err == nil {
		t.Fatal("an unknown field parsed")
	}
	all := func(x ...*fact.Fact) []*fact.Fact { return x }
	isLive := func(string) bool { return true }
	// No change at all.
	if c := Changed(back, all(live...), n, basis, rulings, "active", isLive); c.Share != 0 || len(c.Forced) != 0 {
		t.Fatalf("no change: %+v", c)
	}
	// One fact replaced after it was written, one added: half of two... over the larger count.
	now := all(f("a", "2026-09-01", "", ""), f("b", "2026-09-02", "2026-10-05", "c"), f("c", "2026-10-05", "", ""))
	n2, b2, r2 := Basis(all(now[0], now[2]), func(x *fact.Fact) bool { return x.Meta.Name == "a" })
	c := Changed(back, now, n2, b2, r2, "active", func(n string) bool { return n != "b" })
	if c.Share != 1 || len(c.Forced) != 1 || !strings.Contains(c.Forced[0], "[[b]]") {
		t.Fatalf("change: %+v", c)
	}
	if c := Changed(back, all(live...), n, basis, rulings, "dormant", isLive); len(c.Forced) != 1 {
		t.Fatalf("a status change is forced: %+v", c)
	}
	_, _, r3 := Basis(live, func(*fact.Fact) bool { return false })
	if c := Changed(back, all(live...), n, basis, r3, "active", isLive); len(c.Forced) != 1 {
		t.Fatalf("a ruling removed is forced: %+v", c)
	}
	if got := Clean("```markdown\n---\nfolder: x\n---\nThe body [[a]].\n```"); got != "The body [[a]]." {
		t.Fatalf("clean %q", got)
	}
}
