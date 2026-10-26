package dream

import (
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/relevance"
)

// Not every sentence of the owner's is a statement: a ruling or statement
// stands only with its future use stated; an observed fact always passes.
func TestGateKeepsOnlyRemarksThatMatter(t *testing.T) {
	keep := &relevance.Judgement{Keep: true, Category: "decision", Reason: "the repo's real name"}
	for _, c := range []struct {
		x    Extracted
		pass bool
		why  string
	}{
		{Extracted{Description: "pikadon is the real name", Kind: "statement", Quote: "the real name is pikadon", Relevance: keep}, true, ""},
		{Extracted{Description: "push now", Kind: "user", Quote: "ok now push it", Relevance: &relevance.Judgement{Keep: false, Category: "steering", Reason: "one push"}}, false, "one push"},
		{Extracted{Description: "x", Kind: "statement", Quote: "go"}, false, "refused: no relevance judgement"},
		{Extracted{Description: "x", Kind: "user", Quote: "always run the linter", Relevance: &relevance.Judgement{Keep: true, Category: "instruction"}}, false, "refused: a kept remark needs a reason"},
		{Extracted{Description: "the port is 7790", Kind: "observed"}, true, ""},
	} {
		ok, d := Gate(c.x, "claude:s1")
		if ok != c.pass || !strings.Contains(d.Reason, c.why) {
			t.Errorf("%+v: %v %+v", c.x, ok, d)
		}
		if !ok && (d.Words == "" || d.Session != "claude:s1") {
			t.Errorf("a drop without its words or session: %+v", d)
		}
	}
	if !strings.Contains(Prompt, `"relevance"`) || !strings.Contains(outputSchema, `"relevance"`) {
		t.Fatal("the prompt or the schema asks for no relevance")
	}
}
