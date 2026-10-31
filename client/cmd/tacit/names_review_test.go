package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/fact"
)

// nameFixture is a store with two names, each held by two live curated files,
// as a tool outside tacit leaves a copy in a second folder: a machine fact,
// and an owner ruling whose second file is an agent's copy.
func nameFixture(t *testing.T) *rig {
	t.Helper()
	r := newRig(t, rigOpts{})
	writeFact(t, r.clone, fact.Meta{Name: "lantern-proxy-port", Description: "The lantern proxy listens on port 7781 on host-a", Type: "machine_fact", Scope: "machines/host-a"}, "Seen in its unit file.\n")
	writeFact(t, r.clone, fact.Meta{Name: "lantern-proxy-port", Description: "The lantern proxy for the quokka scheduler listens on port 7781", Type: "machine_fact", Scope: "user"}, "Seen in its config.\n")
	writeFact(t, r.clone, fact.Meta{Name: "deploy-by-tool", Description: "Rerun a failed deploy with the deploy tool, never by hand in a loop", Type: "user_standard", Scope: "machines/host-a"}, "An agent's copy of the rule.\n")
	writeFact(t, r.clone, fact.Meta{Name: "deploy-by-tool", Description: "Never retry a failed deploy by hand in a loop; rerun it with the deploy tool", Type: "user_standard", Scope: "user", Source: ownerSource("never retry a failed deploy by hand in a loop")}, "The owner said \"never retry a failed deploy by hand in a loop\".\n")
	r.commitIndex(r.clone, "names fixture")
	gitIn(t, r.clone, "push", "-q", "origin", "HEAD:main")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TACIT_STORE", r.clone)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	return r
}

func nameItemFor(t *testing.T, r *rig, name string) reviewItem {
	t.Helper()
	rs := &reviewServer{s: r.s}
	rs.loadFacts()
	for _, it := range rs.nameItems() {
		if it.A.Name == name {
			return it
		}
	}
	t.Fatalf("no name finding for %s", name)
	return reviewItem{}
}

func applyVerdicts(t *testing.T, vs []map[string]string) int {
	t.Helper()
	b, _ := json.Marshal(vs)
	p := filepath.Join(t.TempDir(), "verdicts.json")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return runReviewApply([]string{p})
}

// tacit dedupe raises a name two live files hold once, as a finding about the
// two files, earlier first; its dry run changes nothing, and a second run
// pushes nothing. The page and tacit questions show it with both files.
func TestDedupeRaisesANameTwoFilesHoldOnce(t *testing.T) {
	r := nameFixture(t)
	head := r.serverHead()
	if code := runDedupe([]string{"--dry-run"}); code != 0 || r.serverHead() != head {
		t.Fatalf("dedupe --dry-run exited %d or moved the server", code)
	}
	r.assertClean("after dedupe --dry-run")
	if code := runDedupe(nil); code != 0 {
		t.Fatalf("dedupe exited %d", code)
	}
	rel := "review/" + time.Now().Format("2006-01-02") + "-names.md"
	page := r.onServer(rel)
	for _, want := range []string{
		nameLine("lantern-proxy-port", "machines/host-a/lantern-proxy-port.md", "user/lantern-proxy-port.md"),
		nameLine("deploy-by-tool", "machines/host-a/deploy-by-tool.md", "user/deploy-by-tool.md"),
	} {
		if strings.Count(page, want) != 1 {
			t.Errorf("%s holds %q %d time(s), want once:\n%s", rel, want, strings.Count(page, want), page)
		}
	}
	if msg := gitIn(t, r.bare, "log", "-1", "--format=%s", "main"); !strings.Contains(msg, "curator: dedupe: 2 name(s) two files hold raised for review") {
		t.Errorf("commit subject %q", msg)
	}
	head = r.serverHead()
	if code := runDedupe(nil); code != 0 || r.serverHead() != head {
		t.Fatalf("a second dedupe exited %d or pushed", code)
	}
	r.assertServerLintsClean()
	it := nameItemFor(t, r, "lantern-proxy-port")
	if it.Kind != kindName || it.A.Path != "machines/host-a/lantern-proxy-port.md" || it.B.Path != "user/lantern-proxy-port.md" || it.A.Missing || it.B.Missing {
		t.Fatalf("item %+v / %+v", it.A, it.B)
	}
	if it.State != "both" || !it.NeedsOwner || !contains(it.Allowed, "left") || !contains(it.Allowed, "right") || contains(it.Allowed, "query") {
		t.Fatalf("state %s, needs owner %v, allowed %v", it.State, it.NeedsOwner, it.Allowed)
	}
	rs := &reviewServer{s: r.s}
	rs.loadFacts()
	found := false
	for _, q := range openQuestions(rs, nil) {
		if q.ID == it.ID {
			found = q.Closes == kindName && strings.Join(q.Facts, " ") == it.A.Path+" "+it.B.Path
		}
	}
	if !found {
		t.Errorf("tacit questions does not list %s with its two files", it.ID)
	}
}

// An agent's keep, naming the two paths, retires the file it does not keep
// with superseded_by the name, which then reads as the kept file; the mark is
// recorded with both files as they were, a second agent mark is refused, and
// the owner's reopen puts the file back.
func TestAnAgentKeepsOneFileOfANameTwoFilesHold(t *testing.T) {
	r := nameFixture(t)
	stubScrump(t)
	if code := runDedupe(nil); code != 0 {
		t.Fatalf("dedupe exited %d", code)
	}
	it := nameItemFor(t, r, "lantern-proxy-port")
	v := map[string]string{"id": it.ID, "a": it.A.Path, "b": it.B.Path, "mark": "keep", "keep": it.B.Path, "evidence": "Both files say the lantern proxy listens on port 7781; the second names the scheduler it serves."}
	// Names are no answer here: the verdict must name the files.
	if code := applyVerdicts(t, []map[string]string{{"id": it.ID, "a": "lantern-proxy-port", "b": "lantern-proxy-port", "mark": "keep", "keep": "lantern-proxy-port", "evidence": "x"}}); code == 0 {
		t.Fatal("a verdict naming the fact twice was applied")
	}
	if code := applyVerdicts(t, []map[string]string{v}); code != 0 {
		t.Fatalf("review apply exited %d", code)
	}
	if got := supersededOnServer(t, r, it.A.Path); got != "lantern-proxy-port" {
		t.Errorf("%s: superseded_by %q, want the name", it.A.Path, got)
	}
	if got := supersededOnServer(t, r, it.B.Path); got != "" {
		t.Errorf("%s, the file kept: superseded_by %q", it.B.Path, got)
	}
	if p, ok := findFact(r.clone, "lantern-proxy-port"); !ok || !strings.HasSuffix(filepath.ToSlash(p), it.B.Path) {
		t.Errorf("the name reads as %s, want %s", p, it.B.Path)
	}
	dl := r.onServer("review/decisions.jsonl")
	lines := strings.Split(strings.TrimSpace(dl), "\n")
	var d decision
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &d); err != nil {
		t.Fatal(err)
	}
	if d.ID != it.ID || d.Action != "right" || d.By != "agent" || d.Evidence == "" || len(d.Before) != 2 {
		t.Fatalf("decision %+v", d)
	}
	if _, ok := d.Before[it.A.Path]; !ok {
		t.Fatalf("the record keeps the files by path: %v", d.Before)
	}
	if code := applyVerdicts(t, []map[string]string{{"id": it.ID, "a": it.A.Path, "b": it.B.Path, "mark": "keep", "keep": it.A.Path, "evidence": "x"}}); code == 0 {
		t.Fatal("a second agent mark was applied")
	}
	rs := &reviewServer{s: r.s}
	if _, errs, err := rs.applyOps(context.Background(), []op{{ID: it.ID, Mark: "reopen"}}); err != nil || errs[0] != nil {
		t.Fatalf("reopen: %v %v", err, errs)
	}
	if got := supersededOnServer(t, r, it.A.Path); got != "" {
		t.Errorf("after the reopen %s is still superseded by %q", it.A.Path, got)
	}
	r.assertServerLintsClean()
}

// An agent's keep that would retire the file holding the owner's ruling is
// the owner's question: the ruling stays, and tacit questions lists it as
// theirs.
func TestAKeepAgainstTheOwnersRulingOfANameIsTheirQuestion(t *testing.T) {
	r := nameFixture(t)
	stubScrump(t)
	if code := runDedupe(nil); code != 0 {
		t.Fatalf("dedupe exited %d", code)
	}
	it := nameItemFor(t, r, "deploy-by-tool")
	if !it.B.Ruling {
		t.Fatalf("the fixture's second file is no ruling: %+v", it.B)
	}
	if code := applyVerdicts(t, []map[string]string{{"id": it.ID, "a": it.A.Path, "b": it.B.Path, "mark": "keep", "keep": it.A.Path, "evidence": "The two say the same rule."}}); code != 0 {
		t.Fatalf("review apply exited %d", code)
	}
	if got := supersededOnServer(t, r, it.B.Path); got != "" {
		t.Errorf("the ruling's file was retired: superseded_by %q", got)
	}
	it = nameItemFor(t, r, "deploy-by-tool")
	if it.Ask == nil || it.Ask.By != "agent" || !strings.Contains(it.Ask.Question, it.B.Path) || it.Decision != nil {
		t.Fatalf("ask %+v, decision %+v", it.Ask, it.Decision)
	}
	rs := &reviewServer{s: r.s}
	rs.loadFacts()
	for _, q := range openQuestions(rs, nil) {
		if q.ID == it.ID {
			if q.Closes != closesAsk || q.Owners == "" {
				t.Errorf("question %+v", q)
			}
			return
		}
	}
	t.Errorf("tacit questions does not list %s", it.ID)
}

// tacit dedupe --names raises the names alone: the copies a full run would
// retire stay live, and no chain is pointed elsewhere.
func TestDedupeNamesRaisesTheNamesAlone(t *testing.T) {
	r := nameFixture(t)
	const ws = "machines/host-a"
	writeFact(t, r.clone, fact.Meta{Name: "cwd-resets", Description: "The Bash tool resets cwd to /Users/u/work after every call; start every command with cd", Type: "machine_fact", Scope: ws}, "Seen on every call.\n")
	writeFact(t, r.clone, fact.Meta{Name: "cwd-resets-again", Description: "Bash tool resets the cwd to /Users/u/work after each call, so start every command with cd", Type: "machine_fact", Scope: ws}, "Do not rely on an earlier cd.\n")
	r.commitIndex(r.clone, "a copy")
	gitIn(t, r.clone, "push", "-q", "origin", "HEAD:main")
	if code := runDedupe([]string{"--dry-run"}); code != 0 {
		t.Fatalf("dedupe --dry-run exited %d", code)
	}
	if code := runDedupe([]string{"--names"}); code != 0 {
		t.Fatalf("dedupe --names exited %d", code)
	}
	if got := supersededOnServer(t, r, ws+"/cwd-resets-again.md"); got != "" {
		t.Errorf("dedupe --names retired a copy: superseded_by %q", got)
	}
	page := r.onServer("review/" + time.Now().Format("2006-01-02") + "-names.md")
	if strings.Count(page, "- name held twice: ") != 2 {
		t.Errorf("the names were not raised:\n%s", page)
	}
	if code := runDedupe(nil); code != 0 {
		t.Fatalf("dedupe exited %d", code)
	}
	if got := supersededOnServer(t, r, ws+"/cwd-resets-again.md"); got != "cwd-resets" {
		t.Errorf("the full run left the copy: superseded_by %q", got)
	}
	if page = r.onServer("review/" + time.Now().Format("2006-01-02") + "-names.md"); strings.Count(page, "- name held twice: ") != 2 {
		t.Errorf("the full run raised a name again:\n%s", page)
	}
}

// The page asks for a finding's timeline: a name finding gets one built from
// its two files, not "no such finding".
func TestANameFindingHasATimelineOfItsTwoFiles(t *testing.T) {
	r := nameFixture(t)
	if code := runDedupe([]string{"--names"}); code != 0 {
		t.Fatalf("dedupe --names exited %d", code)
	}
	it := nameItemFor(t, r, "lantern-proxy-port")
	rs := &reviewServer{s: r.s, tl: newTimelineBuilder(r.s, time.Time{})}
	rs.loadFacts()
	tl, err := rs.timelineFor(context.Background(), it.ID)
	if err != nil || tl == nil {
		t.Fatalf("timeline: %v", err)
	}
	in, ok := rs.timelineInput(it)
	if !ok || in.A.Fact == nil || in.B.Fact == nil || in.A.Fact.Path != it.A.Path || in.B.Fact.Path != it.B.Path {
		t.Fatalf("the timeline reads %+v and %+v, want the finding's two files", in.A.Fact, in.B.Fact)
	}
}

// The rotation reads a fact by its name, so an open name finding keeps that
// name out of its picks until the finding is answered.
func TestTheRotationLeavesANameTwoFilesHold(t *testing.T) {
	r := nameFixture(t)
	if code := runDedupe([]string{"--names"}); code != 0 {
		t.Fatalf("dedupe --names exited %d", code)
	}
	rot, err := newRotator(context.Background(), r.s, false)
	if err != nil {
		t.Fatal(err)
	}
	if !rot.open["lantern-proxy-port"] || !rot.open["deploy-by-tool"] {
		t.Fatalf("open names %v", rot.open)
	}
}
