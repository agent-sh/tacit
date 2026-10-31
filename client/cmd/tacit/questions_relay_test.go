package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/agentreview"
	"github.com/agent-sh/tacit/client/internal/fact"
)

// The owner's own answer, typed in a session, relayed by an agent (SPEC.md,
// "Closing questions"): their words, found by code in their turns, said on or
// after the day the question was asked and on its subject, close any open
// question, a name two files hold among them. Words on another subject, or
// said before the question, close nothing an agent could not close alone.

// relayHome readies the questions command against a rig's clone (the rig set
// HOME and TACIT_STORE): the agent's own Claude Code session, no Hermes store.
func relayHome(t *testing.T) string {
	t.Helper()
	t.Setenv("TACIT_STATE_DIR", "")
	t.Setenv("TACIT_HERMES_DB", "off")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "agent-session")
	return os.Getenv("HOME")
}

// relayOnName is `tacit questions close` on a name finding, resting on the
// owner's words in their chat session.
func relayOnName(id, answer, words string) int {
	return runQuestionsClose([]string{id, "--harness", "claude", "--answer", answer, "--proof", "words", "--words", words, "--from", "claude:owner-chat",
		"--evidence", "their turn in the chat, under the question's own words", "--reason", "their answer to this question"})
}

// retiredOnServer is a file's retired day as the server holds it.
func retiredOnServer(t *testing.T, r *rig, rel string) string {
	t.Helper()
	f, err := fact.Parse(rel, []byte(r.onServer(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return f.Meta.Retired
}

// lastRecord is the newest line of the server's decision log for id, as
// written.
func lastRecord(t *testing.T, r *rig, id string) string {
	t.Helper()
	last := ""
	for _, l := range strings.Split(r.onServer("review/decisions.jsonl"), "\n") {
		if strings.Contains(l, `"id":"`+id+`"`) {
			last = l
		}
	}
	return last
}

// A name the owner kept in another answer takes no agent's mark. Their own
// answer does: the agent put its question to them in a list, and they typed a
// terse line after each item. That line alone shares little with the files;
// their whole turn holds the question's words, so it is on the subject. Their
// words on another subject close nothing. Their reopen puts the file back, and
// the words, older than the reopen, do not answer it again; words said after
// it do.
func TestARelayOfTheOwnersAnswerClosesAGuardedNameFinding(t *testing.T) {
	r := nameFixture(t)
	stubScrump(t)
	appendDecisionLine(t, r, decision{ID: "aaaaaaaaaaaa", Action: "right", A: "another-fact", B: "lantern-proxy-port", At: "2026-10-20T00:00:00Z"})
	if code := runDedupe([]string{"--names"}); code != 0 {
		t.Fatalf("dedupe --names exited %d", code)
	}
	it := nameItemFor(t, r, "lantern-proxy-port")
	home := relayHome(t)
	const answer = "keep the machine one, the user copy is noise"
	now := time.Now().UTC().Truncate(time.Second)
	turn := "whether the zebra cron runs nightly; - it does.  whether to keep both lantern-proxy-port files (only you may answer, since you kept that name); - " + answer
	// Words every name question holds ("which one stands") name no file.
	const generic = "which one stands now, keep the files that hold it"
	ownerTurns(t, home, [3]string{"owner-chat", now.Format(time.RFC3339), turn}, [3]string{"owner-other", now.Format(time.RFC3339), releaseNotes},
		[3]string{"owner-third", now.Format(time.RFC3339), generic})
	head := r.serverHead()
	for _, off := range []string{releaseNotes, generic} {
		if c := relayOnName(it.ID, "keep:"+it.A.Path, off); c == 0 || r.serverHead() != head {
			t.Fatalf("their words on another subject (%q) closed the name: exit %d", off, c)
		}
	}
	if c := relayOnName(it.ID, "keep:"+it.A.Path, answer); c != 0 {
		t.Fatalf("their answer, relayed: exit %d", c)
	}
	if got := supersededOnServer(t, r, it.B.Path); got != "lantern-proxy-port" {
		t.Fatalf("%s: superseded_by %q, want the name", it.B.Path, got)
	}
	if got := supersededOnServer(t, r, it.A.Path); got != "" {
		t.Fatalf("the file kept, %s: superseded_by %q", it.A.Path, got)
	}
	line := lastRecord(t, r, it.ID)
	for _, w := range []string{`"action":"left"`, `"by":"agent"`, `"proof":"words"`, `"relayed":true`, `"words":"` + answer + `"`, `"words_from":"claude:owner-chat"`, `"words_on":"` + now.Local().Format("2006-01-02") + `"`} {
		if !strings.Contains(line, w) {
			t.Errorf("the record lacks %s:\n%s", w, line)
		}
	}
	r.assertServerLintsClean()
	rs := &reviewServer{s: r.s}
	if _, errs, err := rs.applyOps(context.Background(), []op{{ID: it.ID, Mark: "reopen"}}); err != nil || errs[0] != nil {
		t.Fatalf("the owner's reopen: %v %v", err, errs)
	}
	if got := supersededOnServer(t, r, it.B.Path); got != "" {
		t.Fatalf("after the reopen %s is still superseded by %q", it.B.Path, got)
	}
	head = r.serverHead()
	if c := relayOnName(it.ID, "keep:"+it.A.Path, answer); c == 0 || r.serverHead() != head {
		t.Fatalf("words older than the owner's reopen answered it again: exit %d", c)
	}
	const again = "keep the machine file, i checked the unit"
	later := time.Now().UTC().Add(2 * time.Second).Format(time.RFC3339)
	ownerTurns(t, home, [3]string{"owner-chat", now.Format(time.RFC3339), turn}, [3]string{"owner-chat", later, "the lantern-proxy-port files again: " + again})
	if c := relayOnName(it.ID, "keep:"+it.A.Path, again); c != 0 {
		t.Fatalf("their answer after the reopen, relayed: exit %d", c)
	}
	if got := supersededOnServer(t, r, it.B.Path); got != "lantern-proxy-port" {
		t.Fatalf("%s: superseded_by %q after their later answer", it.B.Path, got)
	}
}

// An agent's question on a name finding is the owner's to answer. They say
// neither file stands: the relay retires both, an answer a name two files hold
// now takes, and their reopen brings both back. Words they said before the
// question was asked are no answer to it.
func TestARelayRetiresBothFilesOfANameWhenTheOwnerSaysSo(t *testing.T) {
	r := nameFixture(t)
	stubScrump(t)
	if code := runDedupe([]string{"--names"}); code != 0 {
		t.Fatalf("dedupe --names exited %d", code)
	}
	it := nameItemFor(t, r, "lantern-proxy-port")
	appendDecisionLine(t, r, decision{ID: it.ID, Action: "ask", A: it.A.Name, B: it.B.Name, At: "2026-10-20T10:00:00Z", By: "agent", Evidence: "the two files name different hosts",
		Question: "Two files hold lantern-proxy-port and they name different hosts. Which one stands, or are both true?"})
	home := relayHome(t)
	const early, answer = "retire both lantern proxy files once the proxy goes", "the proxy is gone from both hosts, retire both"
	ownerTurns(t, home, [3]string{"owner-chat", "2026-10-15T09:00:00Z", early},
		[3]string{"owner-chat", "2026-10-29T09:00:00Z", "which lantern proxy file stands (they name two hosts); - " + answer})
	head := r.serverHead()
	if c := relayOnName(it.ID, "neither", early); c == 0 || r.serverHead() != head {
		t.Fatalf("words said before the question was asked answered it: exit %d", c)
	}
	if c := relayOnName(it.ID, "neither", answer); c != 0 {
		t.Fatalf("their answer, relayed: exit %d", c)
	}
	for _, p := range []string{it.A.Path, it.B.Path} {
		if retiredOnServer(t, r, p) == "" {
			t.Errorf("%s is not retired", p)
		}
	}
	line := lastRecord(t, r, it.ID)
	for _, w := range []string{`"action":"wrong"`, `"closes":"ask"`, `"relayed":true`, `"words":"` + answer + `"`, `"words_on":"2026-10-29"`} {
		if !strings.Contains(line, w) {
			t.Errorf("the record lacks %s:\n%s", w, line)
		}
	}
	r.assertServerLintsClean()
	if got := nameItemFor(t, r, "lantern-proxy-port"); got.NeedsOwner || !contains(got.Allowed, "reopen") {
		t.Fatalf("the answered name: needs the owner %v, allowed %v", got.NeedsOwner, got.Allowed)
	}
	rs := &reviewServer{s: r.s}
	rs.loadFacts()
	if _, err := rs.decide(context.Background(), it.ID, "reopen", false); err != nil {
		t.Fatalf("the owner's reopen: %v", err)
	}
	for _, p := range []string{it.A.Path, it.B.Path} {
		if got := retiredOnServer(t, r, p); got != "" {
			t.Errorf("after the reopen %s is still retired (%s)", p, got)
		}
	}
	r.assertServerLintsClean()
}

// The owner's own page takes the same answer: Neither is true retires both
// files of a name, each by its path, and their reopen brings both back.
func TestTheOwnerRetiresBothFilesOfANameOnThePage(t *testing.T) {
	r := nameFixture(t)
	stubScrump(t)
	if code := runDedupe([]string{"--names"}); code != 0 {
		t.Fatalf("dedupe --names exited %d", code)
	}
	it := nameItemFor(t, r, "lantern-proxy-port")
	rs := &reviewServer{s: r.s}
	rs.loadFacts()
	if _, err := rs.decide(context.Background(), it.ID, "wrong", false); err != nil {
		t.Fatalf("the owner's neither: %v", err)
	}
	for _, p := range []string{it.A.Path, it.B.Path} {
		if retiredOnServer(t, r, p) == "" {
			t.Errorf("%s is not retired", p)
		}
	}
	if line := lastRecord(t, r, it.ID); !strings.Contains(line, `"action":"wrong"`) || strings.Contains(line, `"by":`) || !strings.Contains(line, `"`+it.B.Path+`":{`) {
		t.Fatalf("the record:\n%s", line)
	}
	r.assertServerLintsClean()
	if _, err := rs.decide(context.Background(), it.ID, "reopen", false); err != nil {
		t.Fatalf("the owner's reopen: %v", err)
	}
	for _, p := range []string{it.A.Path, it.B.Path} {
		if got := retiredOnServer(t, r, p); got != "" {
			t.Errorf("after the reopen %s is still retired (%s)", p, got)
		}
	}
}

// The same-subject rule reads the owner's whole turn: their answer after the
// dash shares no word with the fact's words or its description, the
// question's words above it do. Words in a turn about another subject still
// retire nothing.
func TestTheSameSubjectRuleReadsTheOwnersWholeTurn(t *testing.T) {
	stubScrump(t)
	rs, home, wx := walrusStore(t)
	sid := strings.TrimPrefix(testOwnerSession, "claude:")
	const answer = "it moved last week to the new box"
	ownerTurns(t, home, [3]string{sid, "2026-10-01T09:00:00Z", walrusWords},
		[3]string{"owner-later", "2026-10-29T09:00:00Z", releaseNotes},
		[3]string{"owner-later", "2026-10-29T10:00:00Z", "whether your words on the walrus driver port still hold (only you may answer); - " + answer})
	if agentreview.SharesWords(walrusWords, answer) || agentreview.SharesWords(rs.facts["w"].Meta.Description, answer) {
		t.Fatal("fixture: the answer alone should not be on the fact's subject")
	}
	if c := closeOnWords(wx, releaseNotes); c != 1 {
		t.Fatalf("their words on another subject: exit %d", c)
	}
	rs.loadFacts()
	if m := rs.facts["w"].Meta; m.SupersededBy != "" || m.Retired != "" {
		t.Fatalf("their words on another subject retired the fact with their words: %+v", m)
	}
	if c := closeOnWords(wx, answer); c != 0 {
		t.Fatalf("their answer under the question's words: exit %d", c)
	}
	rs.loadFacts()
	if got := rs.facts["w"].Meta.SupersededBy; got != "x" {
		t.Fatalf("w: superseded_by %q, want x", got)
	}
	if d := decisionsOf(t, rs)[wx]; d.Words != answer || d.WordsFrom != "claude:owner-later" || d.WordsOn != "2026-10-29" {
		t.Fatalf("the record: %+v", d)
	}
}

// An agent's question to the owner about their own ruling takes their answer,
// relayed: the ruling is replaced on their later words on its subject. Words
// they said before the question was asked, or on another subject, change no
// ruling.
func TestARelayAnswersTheOwnersQuestionOnTheirRuling(t *testing.T) {
	stubScrump(t)
	ro := pairID(kindRulingChange, "r", "o")
	rs := questionStore(t, map[string]string{"r": "ruling\n", "o": ""}, "- RULING CHANGE? new [[o]] may supersede owner ruling [[r]]\n",
		askOn(ro, "r", "o", "Does your ruling r still hold? An agent reads o as the current one."))
	for name, desc := range map[string]string{"r": "Run the quokka scheduler on the blue queue", "o": "The quokka scheduler runs on the green queue now"} {
		p := filepath.Join(rs.s.Dir, "system", name+".md")
		b, _ := os.ReadFile(p)
		os.WriteFile(p, []byte(strings.Replace(string(b), `description: "fact `+name+`"`, `description: "`+desc+`"`, 1)), 0o644)
	}
	git(t, rs.s.Dir, "commit", "-qam", "descriptions")
	git(t, rs.s.Dir, "push", "-q", "origin", "HEAD:main")
	rs.loadFacts()
	if !isRuling(rs.facts["r"]) {
		t.Fatal("fixture: r is no ruling")
	}
	home := questionsHome(t, rs)
	const early, answer = "the green queue takes the quokka jobs from now", "the green queue replaced it last week"
	ownerTurns(t, home, [3]string{"owner-early", "2026-10-15T09:00:00Z", early},
		[3]string{"owner-later", "2026-10-29T09:00:00Z", releaseNotes},
		[3]string{"owner-later", "2026-10-29T10:00:00Z", "does your quokka scheduler rule still hold (the blue queue one); - " + answer})
	closeRO := func(words string) int {
		return runQuestionsClose([]string{ro, "--harness", "claude", "--answer", "keep:o", "--proof", "words", "--words", words, "--evidence", "their turn in the chat", "--reason", "their answer to this question"})
	}
	for _, w := range []string{early, releaseNotes} {
		if c := closeRO(w); c == 0 {
			t.Fatalf("words that are no answer of theirs (%q) changed their ruling", w)
		}
		rs.loadFacts()
		if got := rs.facts["r"].Meta.SupersededBy; got != "" {
			t.Fatalf("their ruling was replaced by %q on %q", got, w)
		}
	}
	if c := closeRO(answer); c != 0 {
		t.Fatalf("their answer, relayed: exit %d", c)
	}
	rs.loadFacts()
	if got := rs.facts["r"].Meta.SupersededBy; got != "o" {
		t.Fatalf("their ruling: superseded_by %q, want o", got)
	}
	b, _ := os.ReadFile(filepath.Join(rs.s.Dir, "review", "decisions.jsonl"))
	if l := string(b); !strings.Contains(l, `"relayed":true`) || !strings.Contains(l, `"words":"`+answer+`"`) {
		t.Fatalf("the record:\n%s", l)
	}
}

// The owner's page shows a relayed answer as theirs: their words, the day they
// said them, and that an agent only passed them on, with the way to undo it.
func TestPageShowsARelayedAnswerAsTheOwners(t *testing.T) {
	var got map[string]string
	runPage(t, `
  await new Promise((r) => setTimeout(r, 30));
  const f = (path) => ({ name: "lantern-proxy-port", path, created: "2026-10-08", description: "The lantern proxy listens on port 7781", body: "", scope: "user", type: "machine_fact", kind: "observed" });
  const it = { id: "n1", kind: "name", a: f("machines/host-a/lantern-proxy-port.md"), b: f("user/lantern-proxy-port.md"), allowed: ["reopen"], mark: "wrong", state: "wrong", needs_owner: false,
    decision: { action: "wrong", by: "agent", proof: "words", relayed: true, reason: "their answer to this question", evidence: "their turn in the chat", closes: "name",
      words: "the proxy is gone from both hosts, retire both", words_from: "claude:owner-chat", words_on: "2026-10-29", at: "2026-10-30T10:00:00Z" } };
  return JSON.stringify({ record: recordHTML(it) });
`, &got)
	for _, w := range []string{"Your answer, relayed by an agent", "Neither is true", "the proxy is gone from both hosts, retire both", "29 Oct", "Change this answer"} {
		if !strings.Contains(got["record"], w) {
			t.Errorf("the relayed answer does not show %q:\n%s", w, got["record"])
		}
	}
	if strings.Contains(got["record"], "An agent closed your question") {
		t.Errorf("a relayed answer reads as the agent's own:\n%s", got["record"])
	}
}
