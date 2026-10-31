package agentreview

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/ledger"
)

// Over its budget the digest says how many messages it left out, and where.
func TestDigestCutAndOmission(t *testing.T) {
	msgs := []Msg{
		{Role: "assistant", Text: strings.Repeat("old agent text ", 20)},
		{Role: "assistant", Text: strings.Repeat("older agent text ", 20)},
		{Role: "user", Owner: true, Text: "keep the walrus port at 7790"},
		{Role: "assistant", Text: "Noted."},
	}
	_, shown, cut := DigestCut(msgs, 200, 60)
	if len(cut) != 2 || cut[0] != 1 || cut[1] != 2 || shown[1] || !shown[3] {
		t.Fatalf("cut %v shown %v", cut, shown)
	}
	o := Omission(cut, 0, len(msgs))
	for _, want := range []string{"2 of its 4 messages are left out", "numbers 1-2", `basis "unsupported"`} {
		if !strings.Contains(o, want) {
			t.Fatalf("no %q in %q", want, o)
		}
	}
	if Omission(nil, 0, 4) != "" {
		t.Fatal("an omission notice for a digest that left nothing out")
	}
	if o := Omission(nil, 3, 4); !strings.Contains(o, "3 more are shown cut short") {
		t.Fatalf("no notice of messages cut short: %q", o)
	}
	var many []int
	for k := 1; k < 60; k += 2 {
		many = append(many, k)
	}
	if o := Omission(many, 0, 60); !strings.Contains(o, "and 18 more spans") {
		t.Fatalf("spans not bounded: %q", o)
	}
}

// Code finds a removed claim's distinctive words in the left-out messages;
// claims removed on another basis, and words every message holds, are not
// searched for.
func TestSecondLooksFindTheClaimsWords(t *testing.T) {
	msgs := []Msg{{Role: "assistant", Text: "Lane C's report: the log names Oslo, server 41827."}}
	for i := 0; i < 40; i++ {
		msgs = append(msgs, Msg{Role: "assistant", Text: fmt.Sprintf("working on the mirror report, step %d", i)})
	}
	tr := Trim{Removed: []ledger.Removed{
		{Text: "Lane C's report names Oslo, server 41827", Reason: "not in the conversation", Basis: ledger.BasisUnsupported},
		{Text: "Oslo server 41827 is always blocked", Reason: "a one-time call made standing", Basis: ledger.BasisWidens},
		{Text: "the mirror report was written", Reason: "no message"},
	}}
	hidden, _ := Hidden(msgs, []int{1, 2, 3}, 60)
	looks := SecondLooks(tr, msgs, hidden)
	if len(looks) != 1 || looks[0].Removed != 0 || len(looks[0].Msgs) != 1 || looks[0].Msgs[0] != 1 {
		t.Fatalf("%+v", looks)
	}
	if SecondLooks(tr, msgs, nil) != nil {
		t.Fatal("a second look with nothing left out")
	}
	// A long message shown cut short: its rest is searched, and shown from a
	// little before the words.
	long := []Msg{{Role: "assistant", Text: strings.Repeat("the spring budget plan, ", 80) + "hosting costs about US$4k a month"}, {Role: "assistant", Text: "ok"}}
	hidden, clipped := Hidden(long, nil, 60)
	if clipped != 1 || !strings.Contains(hidden[1], "US$4k") || strings.Contains(hidden[1], strings.Repeat("the spring budget plan, ", 30)) {
		t.Fatalf("hidden %q clipped %d", hidden[1], clipped)
	}
	tr = Trim{Removed: []ledger.Removed{{Text: "when shown a US$4k hosting budget", Reason: "no US$4k figure"}}}
	looks = SecondLooks(tr, long, hidden)
	if len(looks) != 1 {
		t.Fatalf("the cut-short part was not searched: %+v", looks)
	}
	if l := LookLines(long, hidden, looks, []int{1}); len(l) != 1 || !strings.Contains(l[0], "[1] AGENT (not shown before):") || !strings.Contains(l[0], "US$4k") {
		t.Fatalf("%v", l)
	}
	// Old messages' tools are never shown: their input is searched.
	tooled := []Msg{{Role: "assistant", Tools: []Tool{{Name: "Write", Input: `{"content":"plan-baseline.json stays as the runs' record"}`}}}}
	for i := 0; i < 70; i++ {
		tooled = append(tooled, Msg{Role: "assistant", Text: "next"})
	}
	if hidden, _ := Hidden(tooled, nil, 60); !strings.Contains(hidden[1], "plan-baseline.json") {
		t.Fatalf("an old message's tool input is not hidden: %v", hidden[1])
	}
}

// The second look puts back a claim the left-out part supports, keeps a
// removal on another basis, and never lets a claim go as unsupported while
// code found its words there.
func TestApplySecondLook(t *testing.T) {
	tr := Trim{Claims: []ledger.Claim{{Text: "a", Cites: []int{9}}}, Removed: []ledger.Removed{
		{Text: "b", Reason: "not shown"}, {Text: "c", Reason: "not shown"}, {Text: "d", Reason: "not shown"}, {Text: "e", Reason: "a guess", Basis: ledger.BasisWidens},
	}}
	looks := []Look{{Removed: 0, Msgs: []int{1}}, {Removed: 1, Msgs: []int{2}}, {Removed: 2, Msgs: []int{3}}}
	shown := map[int]bool{1: true, 2: true, 3: true, 9: true}
	got, ps := ApplySecondLook(tr, looks, []Rejudged{
		{Claim: 1, Keep: true, Cites: []int{1, 77}},
		{Claim: 2, Keep: false, Basis: ledger.BasisContradicted, Reason: "message 2 says otherwise"},
		{Claim: 3, Keep: false, Basis: ledger.BasisUnsupported},
	}, shown)
	if len(got.Claims) != 2 || got.Claims[1].Text != "b" || len(got.Claims[1].Cites) != 1 || got.Claims[1].Cites[0] != 1 {
		t.Fatalf("claims %+v", got.Claims)
	}
	if len(got.Removed) != 3 || got.Removed[0].Basis != ledger.BasisContradicted || got.Removed[0].Reason != "message 2 says otherwise" || got.Removed[2].Text != "e" {
		t.Fatalf("removed %+v", got.Removed)
	}
	if len(ps) != 1 || !strings.Contains(ps[0], `"d" as unsupported`) {
		t.Fatalf("problems %v", ps)
	}
	// Unanswered is unsupported too.
	if _, ps := ApplySecondLook(tr, looks[:1], nil, shown); len(ps) != 1 {
		t.Fatalf("an unanswered claim went: %v", ps)
	}
}

// A long session: the claim's support sits in the part the digest leaves
// out. The reviewer is told the digest was cut; its removal as unsupported
// gets a second look with the message code found; kept there, the claim
// stands in the trim, cited.
func TestReviewSecondLookAtTheLeftOutPart(t *testing.T) {
	home := t.TempDir()
	p := filepath.Join(home, ".claude", "projects", "-h", "long-1.jsonl")
	line := func(typ, content string) string {
		b, _ := json.Marshal(map[string]any{"type": typ, "entrypoint": "cli", "sessionId": "long-1", "origin": map[string]any{"kind": "human"}, "message": map[string]any{"content": content}})
		return string(b)
	}
	lines := []string{line("assistant", "Lane C's report: the log names Oslo, server 41827, fixed in commit 7c3e91b20.")}
	for i := 0; i < 400; i++ {
		lines = append(lines, line("assistant", fmt.Sprintf("step %d of the scrub: ", i)+strings.Repeat("checking the tracked files again ", 20)))
	}
	lines = append(lines, line("user", "scrub the export notes of any city name"))
	writeLines(t, p, lines...)
	note := Note{Path: "inbox/claude@h/no-city-names.md", Name: "no-city-names", Desc: "export notes were scrubbed of city names", Body: "Lane C's report names Oslo, server 41827.", Harness: "claude", Session: "long-1", Scope: "user"}
	owner := len(lines) // the owner's message, last
	first := fmt.Sprintf(`{"verdict":"uncertain","reason":"r","doubt":"overreach","kind":"fact","trim":{"claims":[{"text":"export notes were scrubbed of city names","cites":[%d]}],"removed":[{"text":"Lane C's report names Oslo, server 41827","reason":"no line shows it","basis":"unsupported"}]}}`, owner)
	run := func(second string) (Result, []string) {
		var prompts []string
		rv := Reviewer{Sources: DefaultSources(home), Redact: func(s []string) ([]string, error) { return s, nil }, Run: func(_ context.Context, p string) (string, float64, string, error) {
			prompts = append(prompts, p)
			if strings.Contains(p, "<left-out messages>") {
				return second, 0.01, "opus", nil
			}
			return first, 0.1, "opus", nil
		}}
		res, err := rv.Review(context.Background(), note)
		if err != nil {
			t.Fatal(err)
		}
		return res, prompts
	}
	res, prompts := run(`[{"claim": 1, "keep": true, "cites": [1]}]`)
	if len(prompts) != 2 || !strings.Contains(prompts[0], "The conversation is longer than you read") || !strings.Contains(prompts[1], "[1] AGENT (not shown before): Lane C's report: the log names Oslo") {
		t.Fatalf("prompts: %d\n%s", len(prompts), prompts[0][:min(len(prompts[0]), 3000)])
	}
	if res.Trim == nil || len(res.Trim.Claims) != 2 || len(res.Trim.Removed) != 0 || res.Line.Omitted == 0 || len(res.Line.Looked) != 1 || res.Line.Looked[0] != 1 || res.Line.CostUSD < 0.109 {
		t.Fatalf("%+v problems %v", res.Line, res.TrimProblems)
	}
	// Removed again as unsupported: the trim does not stand.
	res, _ = run(`[{"claim": 1, "keep": false, "basis": "unsupported", "reason": "still not there"}]`)
	if res.Trim != nil || !strings.Contains(strings.Join(res.TrimProblems, ";"), "left out holds its words") {
		t.Fatalf("%+v %v", res.Trim, res.TrimProblems)
	}
	// Removed again because it widens: the trim stands without it.
	res, _ = run(`[{"claim": 1, "keep": false, "basis": "widens", "reason": "the report names a city, not a server"}]`)
	if res.Trim == nil || len(res.Trim.Removed) != 1 || res.Trim.Removed[0].Basis != ledger.BasisWidens {
		t.Fatalf("%+v %v", res.Trim, res.TrimProblems)
	}
}
