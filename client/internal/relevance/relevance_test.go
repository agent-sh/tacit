package relevance

import "testing"

func TestKeepNeedsACategoryAndAReason(t *testing.T) {
	for _, c := range []struct {
		j    *Judgement
		keep bool
		ok   bool
	}{
		{nil, false, false},
		{&Judgement{Keep: true, Category: "instruction", Reason: "how he wants reviews run from now on"}, true, true},
		{&Judgement{Keep: true, Category: " Decision ", Reason: "the repo's name"}, true, true},
		{&Judgement{Keep: true, Category: "instruction"}, false, false},                     // no reason
		{&Judgement{Keep: true, Category: "important", Reason: "it matters"}, false, false}, // no future-use category
		{&Judgement{Keep: true, Category: "steering", Reason: "push now"}, false, false},    // a drop category kept
		{&Judgement{Keep: false, Category: "acknowledgement"}, false, true},
		{&Judgement{Keep: false, Reason: "a one-time go-ahead"}, false, true},
		{&Judgement{Keep: false}, false, false},
	} {
		got, p := Check(c.j)
		if (p == "") != c.ok || (p == "" && got.Keep != c.keep) {
			t.Errorf("Check(%+v) = %+v, %q", c.j, got, p)
		}
		if Kept(c.j) != (c.ok && c.keep) {
			t.Errorf("Kept(%+v) = %v", c.j, Kept(c.j))
		}
	}
	if j, _ := Check(&Judgement{Keep: false, Reason: "nothing to act on"}); j.Category != Other {
		t.Errorf("a drop with no category is other, got %q", j.Category)
	}
}
