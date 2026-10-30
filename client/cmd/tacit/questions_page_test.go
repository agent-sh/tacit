package main

import (
	"strings"
	"testing"
)

// The owner's page shows a closed question as the agent closed it: the
// answer, how it was proved, why, what it checked and the question it
// closed, with the way to reopen it; a moot one says it is no longer a
// question; a reviewer question an agent closed opens on its own sheet.
func TestPageShowsAClosedQuestionWithItsProof(t *testing.T) {
	var got map[string]string
	runPage(t, `
  await new Promise((r) => setTimeout(r, 30));
  const f = (name) => ({ name, created: "2026-10-08", description: "fact " + name, body: "", scope: "system", type: "design_decision", kind: "observed" });
  const it = { id: "x1", kind: "contradiction", a: f("a"), b: f("b"), allowed: ["reopen"], mark: "right", needs_owner: false,
    ask: { action: "ask", by: "agent", question: "Retire both obsolete facts?" },
    decision: { action: "right", by: "agent", proof: "git", reason: "the repo dropped the old path", evidence: "demo@abc1234 removed it", closes: "ask", at: "2026-10-30T10:00:00Z" } };
  const moot = Object.assign({}, it, { decision: { action: "moot", by: "curator", closes: "ask", evidence: "moot: a is retired (2026-10-27)", at: "2026-10-30T10:00:00Z" } });
  const q = { id: "q-abc123", kind: "question", single: true, a: f("n"), b: f("n"), question: "[tacit q-abc123] On 2026-10-24 an agent saved this. Right?", allowed: ["reopen"], mark: "moot", needs_owner: false,
    decision: { action: "moot", by: "agent", proof: "gone", reason: "the lab tree is gone", evidence: "ls: no such directory", closes: "question", at: "2026-10-30T10:00:00Z" } };
  items = [q]; view = [q]; cur = 0; sheet();
  return JSON.stringify({ closure: recordHTML(it), moot: recordHTML(moot), question: els["sheet-inner"].innerHTML });
`, &got)
	for _, w := range []string{"An agent closed your question: Kept the second fact", "proved by git history", "the repo dropped the old path", "demo@abc1234 removed it", "Retire both obsolete facts?", "Change this answer"} {
		if !strings.Contains(got["closure"], w) {
			t.Errorf("the closure does not show %q:\n%s", w, got["closure"])
		}
	}
	for _, w := range []string{"The curator closed this: no longer a question", "nothing is left to ask you", "moot: a is retired (2026-10-27)", "Retire both obsolete facts?"} {
		if !strings.Contains(got["moot"], w) {
			t.Errorf("the moot closure does not show %q:\n%s", w, got["moot"])
		}
	}
	for _, w := range []string{"The reviewer had this question for you", "[tacit q-abc123]", "An agent closed your question: No longer a question", "proved by its subject is gone", "the lab tree is gone", "ls: no such directory", "Change this answer"} {
		if !strings.Contains(got["question"], w) {
			t.Errorf("the question's sheet does not show %q:\n%s", w, got["question"])
		}
	}
}
