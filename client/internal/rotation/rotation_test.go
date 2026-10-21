package rotation

import (
	"strings"
	"testing"
)

func testItem() Item {
	return Item{Name: "pin", Scope: "system", Type: "design_decision", Description: "the pinned version", Body: "We pin v0.7.17 of the tool.", Created: "2026-09-01",
		Proof: []Proof{
			{ID: "p1", Kind: "git", Text: "repo: the path x is gone"},
			{ID: "p2", Kind: "newer", Fact: "pin-new", Newer: true, Folder: true, Text: "[[pin-new]]"},
			{ID: "p3", Kind: "newer", Fact: "pin-old", Newer: false, Folder: true, Text: "[[pin-old]]"},
			{ID: "p4", Kind: "mention", Fact: "elsewhere", Newer: true, Text: "[[elsewhere]]"},
			{ID: "p5", Kind: "newer", Fact: "rule", Newer: true, Folder: true, Text: "[[rule]]"},
		}}
}

func testEnv() Env {
	live := map[string]Live{
		"pin-new":   {Scope: "system", Created: "2026-10-01"},
		"pin-old":   {Scope: "system", Created: "2026-08-01"},
		"elsewhere": {Scope: "user", Created: "2026-10-02"},
		"rule":      {Scope: "system", Created: "2026-10-03", Ruling: true},
	}
	return Env{Today: "2026-10-21", Taken: func(n string) bool { _, ok := live[n]; return ok || n == "pin" },
		Fact: func(n string) (Live, bool) { l, ok := live[n]; return l, ok }}
}

// Code takes every well-formed verdict and refuses the rest, each with a
// reason the retry can act on.
func TestCheckTakesGoodVerdictsAndRefusesTheRest(t *testing.T) {
	it, env := testItem(), testEnv()
	good := []Verdict{
		{Name: "pin", Outcome: Still},
		{Name: "pin", Outcome: Unknown},
		{Name: "pin", Outcome: Stale, Cites: []string{"p1"}, Evidence: "the path is gone"},
		{Name: "pin", Outcome: Superseded, SupersededBy: "pin-new", Cites: []string{"p2"}, Evidence: "newer"},
		{Name: "pin", Outcome: Fix, Was: "v0.7.17", Now: "v0.7.19", Cites: []string{"p2"}, Evidence: "bumped"},
		{Name: "pin", Outcome: Compress, Compress: []string{"pin-old"}, Cites: []string{"p3"}, Evidence: "same",
			New: &NewFact{Name: "pin-joined", Description: "the pin", Type: "design_decision", Body: "See [[pin]] and [[pin-old]]."}},
	}
	for _, v := range good {
		if ps := Check(v, it, env); len(ps) > 0 {
			t.Errorf("%s refused: %v", v.Outcome, ps)
		}
	}
	bad := map[string]Verdict{
		"not one of":                {Name: "pin", Outcome: "maybe"},
		"cites the proof":           {Name: "pin", Outcome: Stale, Evidence: "x"},
		"no proof item":             {Name: "pin", Outcome: Stale, Cites: []string{"p9"}, Evidence: "x"},
		"says why":                  {Name: "pin", Outcome: Stale, Cites: []string{"p1"}},
		"not itself":                {Name: "pin", Outcome: Superseded, SupersededBy: "pin", Cites: []string{"p1"}, Evidence: "x"},
		"no fact of the proof":      {Name: "pin", Outcome: Superseded, SupersededBy: "ghost", Cites: []string{"p1"}, Evidence: "x"},
		"not newer":                 {Name: "pin", Outcome: Superseded, SupersededBy: "pin-old", Cites: []string{"p3"}, Evidence: "x"},
		"word for word":             {Name: "pin", Outcome: Fix, Was: "v9", Now: "v10", Cites: []string{"p1"}, Evidence: "x"},
		"the note":                  {Name: "pin", Outcome: Fix, Was: "v0.7.17", Now: "", Cites: []string{"p1"}, Evidence: "x"},
		"not a live fact of this":   {Name: "pin", Outcome: Compress, Compress: []string{"elsewhere"}, Cites: []string{"p4"}, Evidence: "x", New: &NewFact{Name: "j", Description: "d", Type: "design_decision", Body: "[[pin]] [[elsewhere]]"}},
		"never compressed":          {Name: "pin", Outcome: Compress, Compress: []string{"rule"}, Cites: []string{"p5"}, Evidence: "x", New: &NewFact{Name: "j", Description: "d", Type: "design_decision", Body: "[[pin]] [[rule]]"}},
		"must cite [[pin-old]]":     {Name: "pin", Outcome: Compress, Compress: []string{"pin-old"}, Cites: []string{"p3"}, Evidence: "x", New: &NewFact{Name: "j", Description: "d", Type: "design_decision", Body: "[[pin]]"}},
		"not an unused slug":        {Name: "pin", Outcome: Compress, Compress: []string{"pin-old"}, Cites: []string{"p3"}, Evidence: "x", New: &NewFact{Name: "pin-new", Description: "d", Type: "design_decision", Body: "[[pin]] [[pin-old]]"}},
		"not a type":                {Name: "pin", Outcome: Compress, Compress: []string{"pin-old"}, Cites: []string{"p3"}, Evidence: "x", New: &NewFact{Name: "j", Description: "d", Type: "hot_spot", Body: "[[pin]] [[pin-old]]"}},
		"writes the fact that repl": {Name: "pin", Outcome: Compress, Compress: []string{"pin-old"}, Cites: []string{"p3"}, Evidence: "x"},
	}
	for want, v := range bad {
		ps := Check(v, it, env)
		if len(ps) == 0 || !strings.Contains(strings.Join(ps, "; "), want) {
			t.Errorf("%q: got %v", want, ps)
		}
	}
}

func TestParseAndRender(t *testing.T) {
	if _, err := Parse("not json"); err == nil {
		t.Fatal("parsed prose")
	}
	vs, err := Parse("```json\n[{\"name\":\"pin\",\"outcome\":\"still\"}]\n```")
	if err != nil || len(vs) != 1 || vs[0].Outcome != Still {
		t.Fatalf("%v %v", vs, err)
	}
	r := Render([]Item{testItem(), testItem()})
	if strings.Count(r, "\n") != 2 || !strings.Contains(r, `"proof":[{"id":"p1"`) {
		t.Fatalf("%s", r)
	}
	rp := RetryPrompt("P", "A", map[string][]string{"pin": {"x"}}, []string{"pin", "other"})
	if !strings.Contains(rp, "- pin: x") || strings.Contains(rp, "- other") {
		t.Fatalf("%s", rp)
	}
}
