package agentreview

import (
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/ledger"
)

// The digest numbers each message's line by its place in the conversation,
// and says which messages made it into the budget.
func TestDigestShownNumbersMessages(t *testing.T) {
	msgs := []Msg{
		{Role: "user", Owner: true, Text: "keep the walrus port at 7790"},
		{Role: "assistant", Text: "Noted."},
		{Role: "user", Text: "<task-notification>done</task-notification>"},
		{Role: "assistant", Tools: []Tool{{Name: "Bash", Input: `{"command":"tacit add walrus-port"}`}}},
	}
	text, shown := DigestShown(msgs, 10_000, 60)
	for _, want := range []string{"[1] OWNER: keep the walrus port", "[2] AGENT: Noted.", "[3] NOTE: <task-notification>", "[4] AGENT ran Bash"} {
		if !strings.Contains(text, want) {
			t.Fatalf("no %q in\n%s", want, text)
		}
	}
	for k := 1; k <= 4; k++ {
		if !shown[k] {
			t.Fatalf("message %d not shown: %v", k, shown)
		}
	}
	// Over budget, the agent's lines go first: their numbers are not shown.
	_, shown = DigestShown(msgs, 40, 60)
	if !shown[1] || shown[2] {
		t.Fatalf("budget: %v", shown)
	}
}

// The reply's doubt, kind and trim are read for an uncertain verdict only,
// and only when well formed.
func TestParseReadsTheTrim(t *testing.T) {
	v, err := Parse(`{"verdict":"uncertain","reason":"r","doubt":"overreach","kind":"rule","trim":{"claims":[{"text":"a b","cites":[3]}],"removed":[{"text":"c","reason":"d"}],"one_off":{"words":"keep it","line":3,"context":"x"}}}`)
	if err != nil || v.Doubt != DoubtOverreach || v.Kind != KindRule || v.Trim == nil || len(v.Trim.Claims) != 1 || v.Trim.Claims[0].Cites[0] != 3 || v.Trim.OneOff == nil || v.Trim.As() != ledger.TrimStatement {
		t.Fatalf("%+v %v", v, err)
	}
	// No trim for a core doubt, nor for a verdict other than uncertain; an
	// unknown doubt or kind is none.
	for _, r := range []string{
		`{"verdict":"uncertain","reason":"r","doubt":"core","kind":"fact","trim":{"claims":[{"text":"a","cites":[1]}]}}`,
		`{"verdict":"correct","reason":"r","doubt":"overreach","trim":{"claims":[{"text":"a","cites":[1]}]}}`,
		`{"verdict":"uncertain","reason":"r","doubt":"overreach","trim":"cut it"}`,
	} {
		v, err := Parse(r)
		if err != nil || v.Trim != nil {
			t.Fatalf("%s: %+v %v", r, v, err)
		}
	}
	if v, _ := Parse(`{"verdict":"uncertain","reason":"r","doubt":"maybe","kind":"vibe"}`); v.Doubt != "" || v.Kind != "" {
		t.Fatalf("%+v", v)
	}
	// A stray brace after the object (a model's slip) is not read.
	if v, err := Parse(`{"verdict":"uncertain","reason":"r","doubt":"core","kind":"rule"}}`); err != nil || v.Kind != KindRule {
		t.Fatalf("stray brace: %+v %v", v, err)
	}
}

func trimMsgs() []Msg {
	return []Msg{
		{Role: "user", Owner: true, Text: "lets set up the walrus release"},
		{Role: "user", Owner: true, Text: "the walrus release is built with make release"},
		{Role: "user", Owner: true, Text: "tag this release by hand, the bot is broken today"},
		{Role: "user", Text: "<task-notification>the owner says releases are always on fridays</task-notification>"},
		{Role: "assistant", Text: "The walrus release went out from the valkey runner."},
	}
}

func allShown(n int) map[int]bool {
	m := map[int]bool{}
	for k := 1; k <= n; k++ {
		m[k] = true
	}
	return m
}

// Code's check of a trim: it only removes or narrows.
func TestCheckTrimOnlyRemovesOrNarrows(t *testing.T) {
	msgs := trimMsgs()
	shown := allShown(len(msgs))
	note := Note{Desc: "walrus releases are built with make release and always go out on fridays", Body: "The owner wants walrus releases tagged by hand.", Scope: "user"}
	claim := func(text string, cites ...int) ledger.Claim { return ledger.Claim{Text: text, Cites: cites} }
	for _, c := range []struct {
		name  string
		kind  string
		trim  Trim
		wants string // a problem's words; "" for none
	}{
		{"kept core", KindFact, Trim{Claims: []ledger.Claim{claim("walrus releases are built with make release", 2)}}, ""},
		{"narrowed by a cited word", KindFact, Trim{Claims: []ledger.Claim{claim("walrus releases are built with make release on the valkey runner", 2, 5)}}, ""},
		{"narrower scope", KindFact, Trim{Claims: []ledger.Claim{claim("walrus releases are built with make release", 2)}, Scope: "projects/walrus"}, ""},
		{"no citation", KindFact, Trim{Claims: []ledger.Claim{claim("walrus releases are built with make release")}}, "cites no message"},
		{"a message not shown", KindFact, Trim{Claims: []ledger.Claim{claim("walrus releases are built with make release", 9)}}, "not shown"},
		{"a new claim", KindFact, Trim{Claims: []ledger.Claim{claim("walrus releases run on kubernetes clusters", 2)}}, "kube"},
		{"wider scope", KindFact, Trim{Claims: []ledger.Claim{claim("walrus releases are built with make release", 2)}, Scope: "system"}, "or narrower"},
		{"the owner's authority from a note", KindFact, Trim{Claims: []ledger.Claim{claim("The owner wants walrus releases always on fridays", 4)}}, "cites no message the owner typed"},
		{"a rule on their words without the remark", KindRule, Trim{Claims: []ledger.Claim{claim("walrus releases tagged by hand", 3)}}, "one_off is required"},
		{"rule-worded on their words", KindFact, Trim{Claims: []ledger.Claim{claim("walrus releases always tagged by hand", 3)}}, "one_off is required"},
		{"their remark", KindRule, Trim{Claims: []ledger.Claim{claim("walrus releases tagged by hand", 3)}, OneOff: &OneOff{Words: "tag this release by hand", Line: 3, Context: "the release bot was broken"}}, ""},
		{"a remark they did not type", KindRule, Trim{Claims: []ledger.Claim{claim("walrus releases tagged by hand", 3)}, OneOff: &OneOff{Words: "releases are always on fridays", Line: 4, Context: "x"}}, "did not type"},
		{"a remark not in their words", KindRule, Trim{Claims: []ledger.Claim{claim("walrus releases tagged by hand", 3)}, OneOff: &OneOff{Words: "always tag releases by hand", Line: 3, Context: "x"}}, "verbatim"},
	} {
		ps := CheckTrim(note, msgs, shown, c.kind, c.trim)
		got := strings.Join(ps, "; ")
		if (c.wants == "") != (len(ps) == 0) || (c.wants != "" && !strings.Contains(got, c.wants)) {
			t.Errorf("%s: problems %q, want %q", c.name, got, c.wants)
		}
	}
	// Owner attribution the note did not make.
	plain := Note{Desc: "walrus releases are built with make release", Body: "", Scope: "user"}
	if ps := CheckTrim(plain, msgs, shown, KindFact, Trim{Claims: []ledger.Claim{claim("The owner said walrus releases are built with make release", 2)}}); !strings.Contains(strings.Join(ps, ";"), "the note did not") {
		t.Fatalf("added attribution passed: %v", ps)
	}
}

func TestNarrower(t *testing.T) {
	for _, c := range []struct {
		from, to string
		ok       bool
	}{
		{"user", "user", true}, {"user", "projects/walrus", true}, {"system", "machines/rig", true}, {"", "projects/x", true},
		{"projects/walrus", "user", false}, {"projects/walrus", "projects/other", false}, {"areas/ops", "areas/ops/topics/oncall", true},
		{"user", "system", false}, {"user", "projects/a/b", false},
	} {
		if got := Narrower(c.from, c.to); got != c.ok {
			t.Errorf("Narrower(%q, %q) = %v", c.from, c.to, got)
		}
	}
}

// A question only where it matters.
func TestGate(t *testing.T) {
	l := ledger.Review{Verdict: ledger.VerdictUncertain, Doubt: DoubtCore, Kind: KindDecision}
	if why := Gate(l, true, ""); why != "" {
		t.Fatalf("a doubted decision of an active project: %s", why)
	}
	for _, c := range []struct {
		l       ledger.Review
		active  bool
		settled string
		want    string
	}{
		{ledger.Review{Verdict: ledger.VerdictUncertain, Doubt: DoubtOverreach, Kind: KindRule}, true, "", NotCoreDoubt},
		{ledger.Review{Verdict: ledger.VerdictUncertain, Kind: KindRule}, true, "", NotCoreDoubt},
		{ledger.Review{Verdict: ledger.VerdictCorrect, Doubt: DoubtCore, Kind: KindRule}, true, "", NotCoreDoubt},
		{ledger.Review{Verdict: ledger.VerdictUncertain, Doubt: DoubtCore, Kind: KindFact}, true, "", NotWeighty},
		{l, false, "", NotActive},
		{l, true, "your words on it are in the store: x", "your words on it are in the store: x"},
	} {
		if got := Gate(c.l, c.active, c.settled); got != c.want {
			t.Errorf("%+v active=%v: %q, want %q", c.l, c.active, got, c.want)
		}
	}
}

func TestLineTime(t *testing.T) {
	for line, want := range map[string]string{
		`{"type":"user","timestamp":"2026-10-20T10:00:00.123Z"}`: "2026-10-20T10:00:00Z",
		`{"type":"message","timestamp":1760954400000}`:           "2025-10-20T10:00:00Z",
		`{"type":"user"}`: "",
	} {
		if got := lineTime([]byte(line)); got != want {
			t.Errorf("lineTime(%s) = %q, want %q", line, got, want)
		}
	}
}
