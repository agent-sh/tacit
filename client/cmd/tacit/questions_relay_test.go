package main

import (
	"bytes"
	"context"
	"encoding/json"
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
// after the day the question was asked and in an item of their turn on its
// subject, close any open question, a name two files hold among them. Words
// on another subject, said before the question, or older than the owner's
// own last word on it close nothing an agent could not close alone.

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

// relayOn is `tacit questions close` on any question, resting on the owner's
// words wherever code finds them.
func relayOn(id, answer, words string) int {
	return runQuestionsClose([]string{id, "--harness", "claude", "--answer", answer, "--proof", "words", "--words", words,
		"--evidence", "their turn in the chat", "--reason", "their answer to this question"})
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

// recordOf is the newest line of a clone's decision log for id, as written.
func recordOf(t *testing.T, rs *reviewServer, id string) string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(rs.s.Dir, "review", "decisions.jsonl"))
	last := ""
	for _, l := range strings.Split(string(b), "\n") {
		if strings.Contains(l, `"id":"`+id+`"`) {
			last = l
		}
	}
	return last
}

// describe gives the question store's facts their descriptions, committed
// and pushed.
func describe(t *testing.T, rs *reviewServer, descs map[string]string) {
	t.Helper()
	for name, desc := range descs {
		p := filepath.Join(rs.s.Dir, "system", name+".md")
		b, _ := os.ReadFile(p)
		nb := strings.Replace(string(b), `description: "fact `+name+`"`, `description: "`+desc+`"`, 1)
		if nb == string(b) {
			t.Fatalf("fixture: %s has no description to replace", name)
		}
		os.WriteFile(p, []byte(nb), 0o644)
	}
	git(t, rs.s.Dir, "commit", "-qam", "descriptions")
	git(t, rs.s.Dir, "push", "-q", "origin", "HEAD:main")
	rs.loadFacts()
}

// A name the owner kept in another answer takes no agent's mark. Their own
// answer does: the agent put its question to them in a list, and they typed a
// terse line after each item. That line alone shares little with the files;
// the item of their turn that holds it holds the question's words, so it is
// on the subject. Another item's answer, their words on another subject, and
// words that name no file close nothing. The record keeps their words, and the
// commit reads as an agent's. Their reopen puts the file back, and the words,
// older than the reopen, do not answer it again; words said after it do.
func TestARelayOfTheOwnersAnswerClosesAGuardedNameFinding(t *testing.T) {
	r := nameFixture(t)
	stubScrump(t)
	appendDecisionLine(t, r, decision{ID: "aaaaaaaaaaaa", Action: "right", A: "another-fact", B: "lantern-proxy-port", At: "2026-10-20T00:00:00Z"})
	if code := runDedupe([]string{"--names"}); code != 0 {
		t.Fatalf("dedupe --names exited %d", code)
	}
	it := nameItemFor(t, r, "lantern-proxy-port")
	home := relayHome(t)
	const answer, zebra = "keep the machine one, the user copy is noise", "it does, every night at two"
	now := time.Now().UTC().Truncate(time.Second)
	turn := "whether the zebra cron runs nightly; - " + zebra + ".  whether to keep both lantern-proxy-port files (only you may answer, since you kept that name); - " + answer
	// Words every name question holds ("which one stands"), and folder words
	// of its files, name no subject; nor does an answer in a turn that does
	// not say where its item begins, nor the sentence an answer above it went
	// on with.
	const generic, folders, run, onward = "which one stands now, keep the files that hold it", "the user files hold the old name, rename them", "retire both of them now", "retire both of those please"
	ownerTurns(t, home, [3]string{"owner-chat", now.Format(time.RFC3339), turn}, [3]string{"owner-other", now.Format(time.RFC3339), releaseNotes},
		[3]string{"owner-third", now.Format(time.RFC3339), generic}, [3]string{"owner-fourth", now.Format(time.RFC3339), folders},
		[3]string{"owner-fifth", now.Format(time.RFC3339), "which lantern proxy file stands; - keep the old one; which zebra nightly cron stands; - " + run},
		[3]string{"owner-sixth", now.Format(time.RFC3339), "which lantern proxy port file stands?\n- keep the old one. the lantern proxy port still works.\nwhich zebra nightly cron stands?\n- " + onward})
	head := r.serverHead()
	for _, off := range []string{zebra, releaseNotes, generic, folders, run, onward} {
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
	if subject := gitIn(t, r.bare, "log", "-1", "--format=%s", "main"); !strings.HasSuffix(strings.TrimSpace(subject), " by agent") || !strings.Contains(subject, "relayed") {
		t.Errorf("the commit subject %q: tacit app's history reads a review commit that does not end in by agent as the owner's", subject)
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
// neither file stands, one item to a line: the relay retires both, an answer
// a name two files hold now takes, resting on their newest saying of the
// words; their reopen brings both back, but a file retired again since keeps
// that retirement. Words they said before the question was asked are no
// answer to it.
func TestARelayRetiresBothFilesOfANameWhenTheOwnerSaysSo(t *testing.T) {
	r := nameFixture(t)
	stubScrump(t)
	if code := runDedupe([]string{"--names"}); code != 0 {
		t.Fatalf("dedupe --names exited %d", code)
	}
	it := nameItemFor(t, r, "lantern-proxy-port")
	now := time.Now().UTC().Truncate(time.Second)
	appendDecisionLine(t, r, decision{ID: it.ID, Action: "ask", A: it.A.Name, B: it.B.Name, At: now.Format(time.RFC3339), By: "agent", Evidence: "the two files name different hosts",
		Question: "Two files hold lantern-proxy-port and they name different hosts. Which one stands, or are both true?"})
	home := relayHome(t)
	const early, answer = "retire both lantern proxy files once the proxy goes", "the proxy is gone from both hosts, retire both"
	before, after := now.Add(-48*time.Hour).Format(time.RFC3339), now.Add(time.Minute).Format(time.RFC3339)
	ownerTurns(t, home, [3]string{"owner-chat", before, early}, [3]string{"owner-chat", before, answer},
		[3]string{"owner-chat", after, "whether the zebra cron runs nightly\n- it does, every night at two\nwhich lantern proxy file stands (they name two hosts)\n- " + answer})
	head := r.serverHead()
	if c := relayOnName(it.ID, "neither", early); c == 0 || r.serverHead() != head {
		t.Fatalf("words said before the question was asked answered it: exit %d", c)
	}
	if c := relayOnName(it.ID, "neither", "it does, every night at two"); c == 0 || r.serverHead() != head {
		t.Fatalf("the line under another question answered it: exit %d", c)
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
	for _, w := range []string{`"action":"wrong"`, `"closes":"ask"`, `"relayed":true`, `"words":"` + answer + `"`, `"words_on":"` + now.Add(time.Minute).Local().Format("2006-01-02") + `"`} {
		if !strings.Contains(line, w) {
			t.Errorf("the record lacks %s:\n%s", w, line)
		}
	}
	r.assertServerLintsClean()
	if got := nameItemFor(t, r, "lantern-proxy-port"); got.NeedsOwner || !contains(got.Allowed, "reopen") || got.State != "wrong" {
		t.Fatalf("the answered name: state %s, needs the owner %v, allowed %v", got.State, got.NeedsOwner, got.Allowed)
	}
	// Another tool retires the second file again, on another day: the reopen
	// leaves that retirement.
	p := filepath.Join(r.clone, filepath.FromSlash(it.B.Path))
	b, _ := os.ReadFile(p)
	day := time.Now().Format("2006-01-02")
	if !bytes.Contains(b, []byte("retired: \""+day+"\"")) && !bytes.Contains(b, []byte("retired: "+day)) {
		t.Fatalf("fixture: %s has no retired line of today:\n%s", it.B.Path, b)
	}
	b = bytes.Replace(bytes.Replace(b, []byte("retired: \""+day+"\""), []byte("retired: \"2026-01-02\""), 1), []byte("retired: "+day), []byte("retired: 2026-01-02"), 1)
	os.WriteFile(p, b, 0o644)
	gitIn(t, r.clone, "commit", "-qam", "another tool retires it again")
	gitIn(t, r.clone, "push", "-q", "origin", "HEAD:main")
	rs := &reviewServer{s: r.s}
	rs.loadFacts()
	if _, err := rs.decide(context.Background(), it.ID, "reopen", false); err != nil {
		t.Fatalf("the owner's reopen: %v", err)
	}
	if got := retiredOnServer(t, r, it.A.Path); got != "" {
		t.Errorf("after the reopen %s is still retired (%s)", it.A.Path, got)
	}
	if got := retiredOnServer(t, r, it.B.Path); got != "2026-01-02" {
		t.Errorf("after the reopen %s: retired %q, want the later retirement kept", it.B.Path, got)
	}
	r.assertServerLintsClean()
}

// An agent's answer on a name that the store no longer says waits again, and
// the owner's own answer, relayed, takes it.
func TestARelayAnswersANameWhoseAgentAnswerWentStale(t *testing.T) {
	r := nameFixture(t)
	stubScrump(t)
	if code := runDedupe([]string{"--names"}); code != 0 {
		t.Fatalf("dedupe --names exited %d", code)
	}
	it := nameItemFor(t, r, "lantern-proxy-port")
	if code := applyVerdicts(t, []map[string]string{{"id": it.ID, "a": it.A.Path, "b": it.B.Path, "mark": "keep", "keep": it.A.Path, "evidence": "Both say port 7781."}}); code != 0 {
		t.Fatalf("review apply exited %d", code)
	}
	// Another tool puts the retired copy back.
	p := filepath.Join(r.clone, filepath.FromSlash(it.B.Path))
	b, _ := os.ReadFile(p)
	os.WriteFile(p, bytes.Replace(b, []byte("superseded_by: lantern-proxy-port\n"), nil, 1), 0o644)
	gitIn(t, r.clone, "commit", "-qam", "another tool restores the copy")
	gitIn(t, r.clone, "push", "-q", "origin", "HEAD:main")
	if got := nameItemFor(t, r, "lantern-proxy-port"); !got.Stale || !got.NeedsOwner {
		t.Fatalf("fixture: the agent's answer should be stale and wait on the owner: %+v", got)
	}
	rs := &reviewServer{s: r.s}
	rs.loadFacts()
	listed := false
	for _, q := range openQuestions(rs, nil) {
		listed = listed || q.ID == it.ID
	}
	if !listed {
		t.Error("tacit questions does not list a name whose agent answer went stale")
	}
	home := relayHome(t)
	const answer = "keep the user copy of the lantern proxy, it names the scheduler"
	ownerTurns(t, home, [3]string{"owner-chat", time.Now().UTC().Add(time.Minute).Format(time.RFC3339), answer})
	if c := relayOnName(it.ID, "keep:"+it.B.Path, answer); c != 0 {
		t.Fatalf("their answer on the stale name, relayed: exit %d", c)
	}
	if got := supersededOnServer(t, r, it.A.Path); got != "lantern-proxy-port" {
		t.Fatalf("%s: superseded_by %q, want the name", it.A.Path, got)
	}
}

// The owner's own page takes the same answer: Neither is true retires both
// files of a name, each by its path, in one commit of theirs, and their
// reopen brings both back byte for byte.
func TestTheOwnerRetiresBothFilesOfANameOnThePage(t *testing.T) {
	r := nameFixture(t)
	stubScrump(t)
	if code := runDedupe([]string{"--names"}); code != 0 {
		t.Fatalf("dedupe --names exited %d", code)
	}
	it := nameItemFor(t, r, "lantern-proxy-port")
	was := map[string]string{it.A.Path: r.onServer(it.A.Path), it.B.Path: r.onServer(it.B.Path)}
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
	if subject := gitIn(t, r.bare, "log", "-1", "--format=%s", "main"); !strings.HasPrefix(subject, ownerCommit+"neither file of lantern-proxy-port stands: retire "+it.A.Path+" and "+it.B.Path) {
		t.Errorf("the commit subject %q", subject)
	}
	r.assertServerLintsClean()
	if _, err := rs.decide(context.Background(), it.ID, "reopen", false); err != nil {
		t.Fatalf("the owner's reopen: %v", err)
	}
	for p, b := range was {
		if got := r.onServer(p); got != b {
			t.Errorf("after the reopen %s is not as it was:\n%s\nwas:\n%s", p, got, b)
		}
	}
}

// The same-subject rule reads the item of the owner's turn that holds their
// words: their answer after the dash shares no word with the fact's words or
// its description, the question's words above it do. Words in a turn about
// another subject still retire nothing.
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
// they said before the question was asked, on another subject, or under
// another item of the same turn, change no ruling.
func TestARelayAnswersTheOwnersQuestionOnTheirRuling(t *testing.T) {
	stubScrump(t)
	ro := pairID(kindRulingChange, "r", "o")
	rs := questionStore(t, map[string]string{"r": "ruling\n", "o": ""}, "- RULING CHANGE? new [[o]] may supersede owner ruling [[r]]\n",
		askOn(ro, "r", "o", "Does your ruling r still hold? An agent reads o as the current one."))
	describe(t, rs, map[string]string{"r": "Run the quokka scheduler on the blue queue", "o": "The quokka scheduler runs on the green queue now"})
	if !isRuling(rs.facts["r"]) {
		t.Fatal("fixture: r is no ruling")
	}
	home := questionsHome(t, rs)
	const early, answer, other = "the green queue takes the quokka jobs from now", "the green queue replaced it last week", "drop them all now, they are gone"
	ownerTurns(t, home, [3]string{"owner-early", "2026-10-15T09:00:00Z", early},
		[3]string{"owner-later", "2026-10-29T09:00:00Z", releaseNotes},
		[3]string{"owner-later", "2026-10-29T09:30:00Z", "does your quokka scheduler rule still hold (the blue queue one); - yes it still holds. the old zebra cron jobs; - " + other},
		[3]string{"owner-later", "2026-10-29T10:00:00Z", "does your quokka scheduler rule still hold (the blue queue one); - " + answer})
	for _, w := range []string{early, releaseNotes, other} {
		if c := relayOn(ro, "keep:o", w); c == 0 {
			t.Fatalf("words that are no answer of theirs to it (%q) changed their ruling", w)
		}
		rs.loadFacts()
		if got := rs.facts["r"].Meta.SupersededBy; got != "" {
			t.Fatalf("their ruling was replaced by %q on %q", got, w)
		}
	}
	if c := relayOn(ro, "keep:o", answer); c != 0 {
		t.Fatalf("their answer, relayed: exit %d", c)
	}
	rs.loadFacts()
	if got := rs.facts["r"].Meta.SupersededBy; got != "o" {
		t.Fatalf("their ruling: superseded_by %q, want o", got)
	}
	if l := recordOf(t, rs, ro); !strings.Contains(l, `"relayed":true`) || !strings.Contains(l, `"words":"`+answer+`"`) {
		t.Fatalf("the record:\n%s", l)
	}
}

// A question the owner touched takes their own answer: a pair they left a
// note on, and a pair whose fact they kept in another answer. Their words
// said before their note are no answer over it.
func TestARelayAnswersAPairTheOwnerTouched(t *testing.T) {
	stubScrump(t)
	uv, kl := pairID(kindContradiction, "u", "v"), pairID(kindSupersedeAsk, "k", "l")
	rs := questionStore(t, map[string]string{"u": "", "v": "", "k": "", "l": "", "z": ""},
		"- CONTRADICTION?: [[v]] (system) vs [[u]] (system)\n- supersedes? [[l]] may replace [[k]]\n",
		askOn(uv, "u", "v", "Which walrus port holds, 7781 or 7790?"),
		decision{ID: uv, Action: "note", A: "u", B: "v", At: "2026-10-21T10:00:00Z", Comment: "check the walrus unit first"},
		askOn(kl, "k", "l", "Does the green box note replace the blue box one?"),
		decision{ID: pairID(kindContradiction, "k", "z"), Action: "left", A: "k", B: "z", At: "2026-10-21T10:00:00Z"})
	describe(t, rs, map[string]string{"u": "The walrus driver listens on port 7781", "v": "The walrus driver listens on port 7790",
		"k": "The quokka scheduler runs on the blue box", "l": "The quokka scheduler runs on the green box"})
	home := questionsHome(t, rs)
	const early, onPorts, onBoxes = "keep the walrus driver on port 7781 for now", "the walrus driver moved to port 7790 last week", "the quokka scheduler moved to the green box"
	ownerTurns(t, home, [3]string{"owner-early", "2026-10-20T12:00:00Z", early},
		[3]string{"owner-later", "2026-10-29T10:00:00Z", "which walrus port holds; - " + onPorts},
		[3]string{"owner-later", "2026-10-29T11:00:00Z", "does the green box note replace the blue one; - yes, " + onBoxes})
	if c := relayOn(uv, "keep:u", early); c == 0 {
		t.Fatal("words older than the owner's note answered the pair over it")
	}
	if c := relayOn(uv, "keep:v", onPorts); c != 0 {
		t.Fatalf("their answer on the pair they left a note on: exit %d", c)
	}
	if c := relayOn(kl, "keep:l", onBoxes); c != 0 {
		t.Fatalf("their answer on the pair whose fact they kept: exit %d", c)
	}
	rs.loadFacts()
	if rs.facts["u"].Meta.SupersededBy != "v" || rs.facts["k"].Meta.SupersededBy != "l" {
		t.Fatalf("u %+v k %+v", rs.facts["u"].Meta, rs.facts["k"].Meta)
	}
	for _, id := range []string{uv, kl} {
		if l := recordOf(t, rs, id); !strings.Contains(l, `"relayed":true`) {
			t.Errorf("%s: the record:\n%s", id, l)
		}
	}
}

// The rotation's question about the owner's fact, and an agent's question on
// their ruling, take their own answer: retire, which no agent's own proof
// gives there, also on a fact they kept in a pair and a question they left a
// note on.
func TestARelayRetiresTheOwnersFactOnTheirOwnQuestion(t *testing.T) {
	stubScrump(t)
	sr, cc := pairID(kindStale, "rr", "abc1234"), pairID(kindCheck, "cc", "2026-10-21")
	rs := questionStore(t, map[string]string{"rr": "ruling\n", "cc": "ruling\n", "zz": ""},
		"- stale? [[rr]] (system) @abc1234 n=1 since=2026-10-01\n  2026-10-09 abc1234 demo: drop FOO - FOO (gone at HEAD)\n- check? [[cc]] (system) outcome=stale on=2026-10-21\n  The curator reads it as: the path it names moved\n",
		askOn(sr, "rr", "abc1234", "Does your ruling on the walrus deploy flag still hold?"),
		decision{ID: pairID(kindContradiction, "rr", "zz"), Action: "left", A: "rr", B: "zz", At: "2026-10-21T10:00:00Z"},
		decision{ID: cc, Action: "note", A: "cc", B: "2026-10-21", At: "2026-10-21T10:00:00Z", Comment: "the lab path may come back"})
	describe(t, rs, map[string]string{"rr": "Pass the walrus flag to every deploy", "cc": "The quokka logs live under the old lab path"})
	home := questionsHome(t, rs)
	const onFlag, onLogs = "the walrus deploys dropped that flag, retire it", "the quokka logs moved off the old lab path, that note is done"
	ownerTurns(t, home, [3]string{"owner-later", "2026-10-29T10:00:00Z", "does your ruling on the walrus deploy flag still hold; - " + onFlag},
		[3]string{"owner-later", "2026-10-29T11:00:00Z", onLogs})
	if c := runQuestionsClose([]string{cc, "--harness", "claude", "--answer", "retire", "--proof", "file", "--evidence", "lab/README.md:3 names a new path", "--reason", "the path moved"}); c == 0 {
		t.Fatal("an agent's own proof retired the owner's fact on the rotation's question")
	}
	if c := relayOn(sr, "retire", onFlag); c != 0 {
		t.Fatalf("their answer on their ruling, relayed: exit %d", c)
	}
	if c := relayOn(cc, "retire", onLogs); c != 0 {
		t.Fatalf("their answer on the rotation's question, relayed: exit %d", c)
	}
	rs.loadFacts()
	if rs.facts["rr"].Meta.Retired == "" || rs.facts["cc"].Meta.Retired == "" {
		t.Fatalf("rr %+v cc %+v", rs.facts["rr"].Meta, rs.facts["cc"].Meta)
	}
	for _, id := range []string{sr, cc} {
		if l := recordOf(t, rs, id); !strings.Contains(l, `"relayed":true`) || !strings.Contains(l, `"action":"retire"`) {
			t.Errorf("%s: the record:\n%s", id, l)
		}
	}
}

// A question of the owner's that went moot waits on them, since the moot pass
// leaves their questions: tacit questions says so, and their own answer
// closes it and changes nothing.
func TestARelayClosesTheOwnersQuestionThatWentMoot(t *testing.T) {
	stubScrump(t)
	ab := pairID(kindContradiction, "a", "b")
	rs := questionStore(t, map[string]string{"a": "retired: 2026-10-27\n", "b": ""}, "- CONTRADICTION?: [[b]] (system) vs [[a]] (system)\n",
		askOn(ab, "a", "b", "Which walrus port holds, 7781 or 7790?"),
		decision{ID: ab, Action: "note", A: "a", B: "b", At: "2026-10-21T10:00:00Z", Comment: "check the walrus unit first"})
	describe(t, rs, map[string]string{"a": "The walrus driver listens on port 7781", "b": "The walrus driver listens on port 7790"})
	home := questionsHome(t, rs)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if n, err := closeMootQuestions(ctx, rs, rs.ownerTouched(), nil, false, func(string) {}); err != nil || n != 0 {
		t.Fatalf("the moot pass closed the owner's question: %d %v", n, err)
	}
	var code int
	out := captureStdout(t, func() { code = runQuestions(nil) })
	if code != 0 || !strings.Contains(out, "moot, but the owner's") {
		t.Fatalf("exit %d, the list:\n%s", code, out)
	}
	const answer = "the walrus driver is on port 7790 now"
	ownerTurns(t, home, [3]string{"owner-later", "2026-10-29T10:00:00Z", "which walrus port holds; - " + answer})
	b, _ := os.ReadFile(filepath.Join(rs.s.Dir, "system", "b.md"))
	if c := relayOn(ab, "keep:b", answer); c != 0 {
		t.Fatalf("their answer on their moot question: exit %d", c)
	}
	rs.loadFacts()
	if got, _ := os.ReadFile(filepath.Join(rs.s.Dir, "system", "b.md")); !bytes.Equal(got, b) || rs.facts["a"].Meta.Retired != "2026-10-27" {
		t.Fatalf("a moot closure changed a fact: a %+v\n%s", rs.facts["a"].Meta, got)
	}
	var d map[string]any
	if json.Unmarshal([]byte(recordOf(t, rs, ab)), &d) != nil || d["action"] != mootMark || d["relayed"] != true || d["words"] != answer || !strings.Contains(asString(d["evidence"]), "a is retired") {
		t.Fatalf("the record: %v", d)
	}
}

// The owner's own edit of a fact in tacit app is a newer word of theirs: on a
// question of theirs (one they left a note on), their words said before it
// are no answer over it, and words said after it are.
func TestTheOwnersEditInTheAppOutranksTheirOlderWords(t *testing.T) {
	stubScrump(t)
	uv := pairID(kindContradiction, "u", "v")
	rs := questionStore(t, map[string]string{"u": "", "v": ""}, "- CONTRADICTION?: [[v]] (system) vs [[u]] (system)\n", askOn(uv, "u", "v", "Which walrus port holds, 7781 or 7790?"),
		decision{ID: uv, Action: "note", A: "u", B: "v", At: "2026-10-21T10:00:00Z", Comment: "check the walrus unit first"})
	describe(t, rs, map[string]string{"u": "The walrus driver listens on port 7781", "v": "The walrus driver listens on port 7790"})
	home := questionsHome(t, rs)
	const older, newer = "the walrus driver moved to port 7790 last week", "the walrus driver is on port 7790, keep that one"
	ownerTurns(t, home, [3]string{"owner-later", time.Now().UTC().Add(-time.Hour).Format(time.RFC3339), "which walrus port holds; - " + older})
	d := draftOf(t, rs, "u")
	d.Body = "Checked against the unit file again."
	if res, err := rs.editFact(ctxT(t), "u", d); err != nil || !res.Saved {
		t.Fatalf("the owner's edit: %+v %v", res, err)
	}
	if c := relayOn(uv, "keep:v", older); c == 0 {
		t.Fatal("words older than the owner's own edit of a fact answered its question")
	}
	ownerTurns(t, home, [3]string{"owner-later", time.Now().UTC().Add(-time.Hour).Format(time.RFC3339), "which walrus port holds; - " + older},
		[3]string{"owner-later", time.Now().UTC().Add(time.Minute).Format(time.RFC3339), "which walrus port holds; - " + newer})
	if c := relayOn(uv, "keep:v", newer); c != 0 {
		t.Fatalf("their words after their edit: exit %d", c)
	}
	rs.loadFacts()
	if got := rs.facts["u"].Meta.SupersededBy; got != "v" {
		t.Fatalf("u: superseded_by %q, want v", got)
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
	for _, w := range []string{"Your answer, relayed by an agent", "Neither is true", "the proxy is gone from both hosts, retire both", "29 Oct", "a Claude Code session", "Change this answer"} {
		if !strings.Contains(got["record"], w) {
			t.Errorf("the relayed answer does not show %q:\n%s", w, got["record"])
		}
	}
	if strings.Contains(got["record"], "An agent closed your question") {
		t.Errorf("a relayed answer reads as the agent's own:\n%s", got["record"])
	}
}
