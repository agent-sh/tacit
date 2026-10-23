package assess

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/record"
)

func TestMaintenancePaths(t *testing.T) {
	for p, want := range map[string]bool{
		"go.mod": true, "go.sum": true, "package.json": true, "web/package-lock.json": true, "Cargo.lock": true,
		"requirements-dev.txt": true, "constraints.txt": true, "poetry.lock": true, "pnpm-lock.yaml": true,
		".github/workflows/ci.yml": true, ".github/dependabot.yml": true, ".gitlab-ci.yml": true, ".circleci/config.yml": true,
		"CHANGELOG.md": true, "docs/CHANGES.rst": true, "VERSION": true, ".release-please-manifest.json": true,
		".claude-plugin/plugin.json": true, ".claude-plugin/marketplace.json": true,
		"main.go": false, "src/history.py": false, "lib/plugin.json": false, "plugin.json": false, "README.md": false,
		"skills/x/SKILL.md": false, "requirements.py": false, "docs/history/2026.md": false,
	} {
		if got := MaintenancePath(p); got != want {
			t.Errorf("MaintenancePath(%q) = %v, want %v", p, got, want)
		}
	}
	if !BotAuthor("dependabot[bot]") || !BotAuthor("", "renovate") || !BotAuthor("github-actions") || !BotAuthor("x", "41898282+github-actions[bot]@users.noreply.github.com") {
		t.Error("a bot was not recognised")
	}
	if BotAuthor("avifenesh", "Avi Fenesh", "a@example.com") || BotAuthor("robotics") {
		t.Error("a person was taken for a bot")
	}
	if fs := RealFiles([]string{"go.mod", "go.sum"}); len(fs) != 0 {
		t.Errorf("a dependency bump is real: %v", fs)
	}
	if fs := RealFiles([]string{"go.mod", "internal/x.go"}); len(fs) != 1 || fs[0] != "internal/x.go" {
		t.Errorf("real files %v", fs)
	}
}

func wordsCand(cs string) Candidate {
	c := Candidate{Folder: "projects/oldtool", Notes: []Note{
		{Name: "plugins-to-delete", Scope: "user", Type: "design_decision", Created: "2026-10-08", Tier: TierDecision,
			Description: "Owner decided on 2026-10-06 to delete the plugins oldtool and gadget.", Body: "Tower Crane replaces them. As of 2026-10-08 both still exist."},
		{Name: "removed-plugins", Scope: "user", Type: "design_decision", Created: "2026-10-08", Tier: TierDecision,
			Description: "Removed for good (2026-09-23): oldtool, gadget.", Body: "They were unused."},
		{Name: "oldtool-howto", Scope: "projects/other", Type: "history", Created: "2026-10-01", Tier: TierOther,
			Description: "oldtool runs with --fast.", Body: ""},
	}, NotesTotal: 3}
	if cs != "" {
		c.Activity = &Activity{Status: cs, Days: []ActivityDay{{Day: "2026-10-06", Source: "commit"}}, Today: "2026-10-23", TypicalGap: 8, Window: 16, Idle: 17}
	}
	return c
}

func TestOwnerWordsChecked(t *testing.T) {
	c := wordsCand(record.StatusActive)
	var d Decision
	if err := json.Unmarshal([]byte(`{"folder":"projects/oldtool","kind":"repo","status":"dead","why":"a plugin repository","owner_status":[
		{"says":"dead","words":"Removed for good (2026-09-23): oldtool","from":"note:removed-plugins"},
		{"says":"dead","words":"decided on 2026-10-06 to delete the plugins oldtool","from":"note:plugins-to-delete","successor":"tower-crane"}]}`), &d); err != nil {
		t.Fatal(err)
	}
	said, ps := CheckOwnerWords(c, d)
	if len(ps) != 0 || len(said) != 2 {
		t.Fatalf("said %+v problems %v", said, ps)
	}
	// The newest words first (they supersede the older), their day code's.
	if said[0].Note.Name != "plugins-to-delete" || said[0].Day != "2026-10-06" || said[0].Successor != "tower-crane" || said[1].Day != "2026-09-23" {
		t.Fatalf("order and days: %+v", said)
	}
	// An object reads as one item; words not in the note, a note of no
	// owner tier, a note not shown, a successor the note does not name, or a
	// successor on a status other than dead are refused.
	var one Decision
	json.Unmarshal([]byte(`{"owner_status":{"says":"dormant","words":"both still exist","from":"note:plugins-to-delete"}}`), &one)
	if len(one.OwnerStatus) != 1 {
		t.Fatalf("object: %+v", one.OwnerStatus)
	}
	for _, bad := range []OwnerWords{
		{Says: "dead", Words: "oldtool is gone forever", From: "note:plugins-to-delete"},
		{Says: "dead", Words: "oldtool runs with", From: "note:oldtool-howto"},
		{Says: "dead", Words: "to delete the plugins", From: "note:elsewhere"},
		{Says: "dead", Words: "to delete the plugins", From: "note:plugins-to-delete", Successor: "pikadon"},
		{Says: "dormant", Words: "to delete the plugins", From: "note:plugins-to-delete", Successor: "tower-crane"},
		{Says: "active", Words: "to delete the plugins", From: "note:plugins-to-delete"},
	} {
		if said, ps := CheckOwnerWords(c, Decision{OwnerStatus: OwnerWordsList{bad}}); len(said) != 0 || len(ps) != 1 {
			t.Errorf("%+v: said %v problems %v", bad, said, ps)
		}
	}
}

func TestWordsOutcome(t *testing.T) {
	c := wordsCand(record.StatusActive)
	d := Decision{Folder: c.Folder, Kind: "repo", Status: "active", Why: "the owner's plugin repository",
		OwnerStatus: OwnerWordsList{{Says: "dead", Words: "to delete the plugins oldtool", From: "note:plugins-to-delete", Successor: "tower-crane"}}}
	said, _ := CheckOwnerWords(c, d)
	// No real work after the words: dead, replaced by the successor, the words kept.
	maint := Changes{Maintenance: []Work{{Day: "2026-10-09", Kind: "commit", Cite: "commit:abc1234", Why: "a bot's commit (dependabot[bot])"}}}
	nd, stood := WordsOutcome(c, d, said[0], "projects/tower-crane", maint)
	if !stood || nd.Status != record.StatusDead || nd.Successor != "projects/tower-crane" || nd.Words == nil || nd.Words.WordsOn != "2026-10-06" || nd.Words.WordsFrom != "note:plugins-to-delete" {
		t.Fatalf("stood %v: %+v", stood, nd)
	}
	for _, want := range []string{"code: dead on the owner's words of 2026-10-06", "note:plugins-to-delete", "replaced by projects/tower-crane", "maintenance only: 2026-10-09 commit commit:abc1234", "its dates alone say active", "What it is: the owner's plugin repository"} {
		if !strings.Contains(nd.Why, want) {
			t.Errorf("why lacks %q: %s", want, nd.Why)
		}
	}
	// Real work after the words: the dates stand, the words cited.
	real := Changes{Real: []Work{{Day: "2026-10-12", Kind: "commit", Cite: "commit:def5678", Why: "changes main.go", Real: true}}}
	nd, stood = WordsOutcome(c, d, said[0], "projects/tower-crane", real)
	if stood || nd.Status != record.StatusActive || nd.Successor != "" || nd.Words != nil || !strings.Contains(nd.Why, "Owner activity after the 2026-10-06 statement") || !strings.Contains(nd.Why, "real work: 2026-10-12 commit commit:def5678 (changes main.go)") {
		t.Fatalf("stood %v: %+v", stood, nd)
	}
	// No dates at all: the words stand.
	nd, stood = WordsOutcome(wordsCand(""), d, said[0], "", real)
	if !stood || nd.Status != record.StatusDead {
		t.Fatalf("no dates: %v %+v", stood, nd)
	}
}

// A dead answer resting on a cited note stands only while the owner has not
// worked on the folder since the note's day.
func TestDeadOnANoteAndLaterWork(t *testing.T) {
	c := wordsCand(record.StatusActive)
	c.Name = nil
	shown := Render([]Candidate{c})
	d := Decision{Folder: c.Folder, Kind: "repo", Status: "dead", Why: "removed, note:removed-plugins"}
	if _, changed := ApplyStatus(c, d, shown); changed {
		t.Fatal("a dead answer on a shown note was overridden with no work after it")
	}
	c.RealWork = func(after string) Changes {
		if after != "2026-09-23" {
			t.Errorf("after %s", after)
		}
		return Changes{Real: []Work{{Day: "2026-10-05", Kind: "commit", Real: true}}}
	}
	nd, changed := ApplyStatus(c, d, shown)
	if !changed || nd.Status != record.StatusActive || !strings.Contains(nd.Why, "but the owner worked on it after that day") {
		t.Fatalf("%v %+v", changed, nd)
	}
}

func TestNotesRenderOwnersFirst(t *testing.T) {
	ns := []Note{{Name: "b", Tier: TierOther, Created: "2026-10-20"}, {Name: "a", Tier: TierDecision, Created: "2026-10-01"}, {Name: "c", Tier: TierRuling, Created: "2026-09-01"}}
	SortNotes(ns)
	if ns[0].Name != "c" || ns[1].Name != "a" || ns[2].Name != "b" {
		t.Fatalf("%+v", ns)
	}
	c := wordsCand(record.StatusActive)
	out := Render([]Candidate{c})
	if !strings.Contains(out, "notes in other folders that name it") || !strings.Contains(out, "[[plugins-to-delete]] (user, design_decision, 2026-10-08; a decision): Owner decided on 2026-10-06") {
		t.Fatalf("render:\n%s", out)
	}
	f1 := Fingerprint(c)
	c.Notes = c.Notes[:1]
	if Fingerprint(c) == f1 {
		t.Error("the notes are not in the fingerprint")
	}
}
