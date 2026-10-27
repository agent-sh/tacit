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
			Description: "Owner decided on 2026-10-06 to delete the plugins oldtool and gadget.", Body: "Tower Crane replaces oldtool and gadget. As of 2026-10-08 both still exist."},
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
		{"says":"dead","words":"Tower Crane replaces oldtool","from":"note:plugins-to-delete","successor":"tower-crane"}]}`), &d); err != nil {
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
		// The note names tower-crane, but not in the words cited.
		{Says: "dead", Words: "decided on 2026-10-06 to delete the plugins oldtool", From: "note:plugins-to-delete", Successor: "tower-crane"},
		// The words name tower-crane but not this folder: whichever sentence
		// is cited, the successor needs words that name both.
		{Says: "dead", Words: "Tower Crane replaces", From: "note:plugins-to-delete", Successor: "tower-crane"},
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
		OwnerStatus: OwnerWordsList{{Says: "dead", Words: "Tower Crane replaces oldtool", From: "note:plugins-to-delete", Successor: "tower-crane"}}}
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

// A successor stands only where the cited words name it, and never one the
// words retire together with the folder (a plan that folds both into one
// new tool names no successor): dead with no successor is what they say.
func TestSuccessorOnlyFromTheCitedWords(t *testing.T) {
	c := Candidate{Folder: "projects/sync-docs", Notes: []Note{
		{Name: "single-lint-first-cleanup-tool", Scope: "user", Type: "design_decision", Created: "2026-10-08", Tier: TierDecision,
			Description: "deslop, sync-docs, drift-detect, simplification and logic dedupe fold into one lint-first cleanup tool, rebuilt from the deslop repo", Body: "Owner 2026-10-06: deslop should use a small agent."},
		{Name: "sync-docs-dead", Scope: "user", Type: "statement", Created: "2026-10-24", Said: "2026-10-24", Tier: TierStatement,
			Description: "sync-docs status", Body: "You said once (2026-10-24): \"sync-docs is dead, no successor\""},
	}, NotesTotal: 2}
	words := "deslop, sync-docs, drift-detect, simplification and logic dedupe fold into one lint-first cleanup tool, rebuilt from the deslop repo"
	if said, ps := CheckOwnerWords(c, Decision{OwnerStatus: OwnerWordsList{{Says: "dead", Words: words, From: "note:single-lint-first-cleanup-tool", Successor: "deslop"}}}); len(said) != 0 || len(ps) != 1 || !strings.Contains(ps[0], "no successor") {
		t.Fatalf("a successor the words retire with the folder: said %+v problems %v", said, ps)
	}
	// The same words with no successor: dead, none.
	said, ps := CheckOwnerWords(c, Decision{OwnerStatus: OwnerWordsList{{Says: "dead", Words: words, From: "note:single-lint-first-cleanup-tool"}}})
	if len(ps) != 0 || len(said) != 1 || said[0].Successor != "" {
		t.Fatalf("dead with no successor: said %+v problems %v", said, ps)
	}
	// The owner's newer words, dead with no successor, supersede an older
	// item; nothing carries a successor over from it.
	said, ps = CheckOwnerWords(c, Decision{OwnerStatus: OwnerWordsList{
		{Says: "dead", Words: words, From: "note:single-lint-first-cleanup-tool"},
		{Says: "dead", Words: "sync-docs is dead, no successor", From: "note:sync-docs-dead"}}})
	if len(ps) != 0 || len(said) != 2 || said[0].Note.Name != "sync-docs-dead" || said[0].Successor != "" {
		t.Fatalf("newest: said %+v problems %v", said, ps)
	}
	nd, _ := WordsOutcome(c, Decision{Folder: c.Folder, Kind: "repo"}, said[0], "", Changes{})
	if nd.Status != "dead" || nd.Successor != "" || strings.Contains(nd.Why, "replaced by") {
		t.Fatalf("outcome %+v", nd)
	}
	if !coListed("delete the plugins next-task, prepare-delivery and audit-project", "audit-project", "next-task") || coListed("next-task is obsolete. Tower Crane replaces them.", "next-task", "tower-crane") {
		t.Error("coListed")
	}
}

// A successor is accepted only where the cited words name both the folder
// (its name or another name of it) and the successor: the same answer
// whichever sentence of the note the model cites.
func TestSuccessorNeedsWordsNamingTheFolder(t *testing.T) {
	c := Candidate{Folder: "projects/audit-project", Names: []string{"auditor"}, Notes: []Note{
		{Name: "pipeline-obsolete", Scope: "user", Type: "design_decision", Created: "2026-10-08", Tier: TierDecision,
			Description: "audit-project and the per-ticket pipeline are obsolete (owner 2026-10-06).", Body: "These were part of the obsolete next-task pipeline that Tower Crane replaces. The auditor goes too; Tower Crane replaces the auditor."},
	}, NotesTotal: 1}
	check := func(words string) ([]Said, []string) {
		return CheckOwnerWords(c, Decision{OwnerStatus: OwnerWordsList{{Says: "dead", Words: words, From: "note:pipeline-obsolete", Successor: "tower-crane"}}})
	}
	if said, ps := check("These were part of the obsolete next-task pipeline that Tower Crane replaces."); len(said) != 0 || len(ps) != 1 || !strings.Contains(ps[0], "do not name this folder") {
		t.Fatalf("words naming only the successor: said %+v problems %v", said, ps)
	}
	if said, ps := check("Tower Crane replaces the auditor"); len(ps) != 0 || len(said) != 1 || said[0].Successor != "tower-crane" {
		t.Fatalf("an alias and the successor: said %+v problems %v", said, ps)
	}
	if said, ps := check("audit-project and the per-ticket pipeline are obsolete (owner 2026-10-06). These were part of the obsolete next-task pipeline that Tower Crane replaces."); len(ps) != 0 || len(said) != 1 {
		t.Fatalf("the folder and the successor: said %+v problems %v", said, ps)
	}
	// With no successor the description's words alone say dead, none.
	said, ps := CheckOwnerWords(c, Decision{OwnerStatus: OwnerWordsList{{Says: "dead", Words: "audit-project and the per-ticket pipeline are obsolete", From: "note:pipeline-obsolete"}}})
	if len(ps) != 0 || len(said) != 1 || said[0].Successor != "" {
		t.Fatalf("no successor: said %+v problems %v", said, ps)
	}
}

// A note that claims the owner's authority without their words is an
// agent's note: never the owner's word on a status, and no dead answer rests
// on it against the dates.
func TestClaimNoteIsNoOwnerWord(t *testing.T) {
	c := wordsCand(record.StatusActive)
	c.Name = nil
	c.Notes[0].Tier, c.Notes[0].Claim = TierOther, true
	if said, ps := CheckOwnerWords(c, Decision{OwnerStatus: OwnerWordsList{{Says: "dead", Words: "to delete the plugins oldtool", From: "note:plugins-to-delete"}}}); len(said) != 0 || len(ps) != 1 || !strings.Contains(ps[0], "claims the owner's authority") {
		t.Fatalf("a claim cited as the owner's word: said %+v problems %v", said, ps)
	}
	shown := Render([]Candidate{c})
	if !strings.Contains(shown, "[[plugins-to-delete]]") || !strings.Contains(shown, "an agent's note that claims the owner's authority without their words") {
		t.Fatalf("render:\n%s", shown)
	}
	nd, changed := ApplyStatus(c, Decision{Folder: c.Folder, Kind: "repo", Status: "dead", Why: "to be deleted, note:plugins-to-delete"}, shown)
	if !changed || nd.Status != record.StatusActive {
		t.Fatalf("dead on a claim stood against the dates: %v %+v", changed, nd)
	}
	if _, _, ok := NoteDay(c, "note:plugins-to-delete"); ok {
		t.Fatal("a claim note dated a status")
	}
	// An agent's note that claims nothing still carries a dead answer.
	if _, changed := ApplyStatus(c, Decision{Folder: c.Folder, Kind: "repo", Status: "dead", Why: "removed, note:removed-plugins"}, shown); changed {
		t.Fatal("a dead answer on another shown note was overridden")
	}
}

// Words the model itself reads as limited in time never make a folder dead:
// a pause is dormant. Flagged words must be the note's.
func TestOwnerWordsPauseIsNotDead(t *testing.T) {
	c := wordsCand(record.StatusActive)
	c.Notes = append(c.Notes, Note{Name: "oldtool-paused", Scope: "user", Type: "design_decision", Created: "2026-10-08", Tier: TierStatement, Said: "2026-09-28",
		Description: "The owner stopped oldtool: \"bank what you have, no more oldtool work for now\"."})
	for _, c2 := range []struct {
		item string
		ok   bool
		want string
	}{
		{`{"says":"dead","words":"no more oldtool work for now","from":"note:oldtool-paused","temporal":"for now"}`, false, "stopping or pausing it for a while is dormant"},
		{`{"says":"dormant","words":"no more oldtool work for now","from":"note:oldtool-paused","temporal":"for now"}`, true, ""},
		{`{"says":"dead","words":"no more oldtool work for now","from":"note:oldtool-paused","temporal":"until spring"}`, false, "copy the words exactly"},
	} {
		var d Decision
		if err := json.Unmarshal([]byte(`{"folder":"projects/oldtool","kind":"repo","status":"dormant","why":"x","owner_status":[`+c2.item+`]}`), &d); err != nil {
			t.Fatal(err)
		}
		said, ps := CheckOwnerWords(c, d)
		if (len(said) == 1) != c2.ok || (!c2.ok && (len(ps) != 1 || !strings.Contains(ps[0], c2.want))) {
			t.Errorf("%s: said %+v problems %q", c2.item, said, ps)
		}
		if c2.ok && said[0].Day != "2026-09-28" {
			t.Errorf("dated by the day said: %+v", said[0])
		}
	}
}

// A status on the owner's quote: dead needs words that end the folder (or
// the forge's word it is gone); the words stand from the day they were said
// unless the owner worked on it after.
func TestQuoteOutcome(t *testing.T) {
	c := wordsCand(record.StatusActive)
	d := Decision{Folder: "projects/oldtool", Kind: "repo", Status: "dead", Why: "the owner's tool", OwnerQuote: "we are stopping development of oldtool, bank it", OwnerQuoteFrom: "claude:s9"}
	if ps := CheckQuoteStatus(c, d); len(ps) != 1 || !strings.Contains(ps[0], "owner_quote_final") {
		t.Errorf("dead without words that end it: %q", ps)
	}
	gone := c
	gone.Activity = &Activity{Status: record.StatusDormant, Deleted: true}
	if ps := CheckQuoteStatus(gone, d); len(ps) != 0 {
		t.Errorf("dead with the forge's word it is gone: %q", ps)
	}
	d.OwnerQuoteFinal = "drop it"
	if ps := CheckQuoteStatus(c, d); len(ps) != 1 || !strings.Contains(ps[0], "copy the words exactly") {
		t.Errorf("final words not in the quote: %q", ps)
	}
	d.Status, d.OwnerQuoteFinal = "dormant", ""
	nd, stood := QuoteOutcome(c, d, "2026-09-28", Changes{})
	if !stood || nd.Status != "dormant" || nd.OwnerQuote == "" || !strings.HasPrefix(nd.Why, "code: dormant on the owner's words of 2026-09-28, claude:s9") || !strings.Contains(nd.Why, "its dates alone say active") {
		t.Errorf("words stand: %+v", nd)
	}
	nd, stood = QuoteOutcome(c, d, "2026-09-28", Changes{Real: []Work{{Day: "2026-10-06", Kind: "commit", Cite: "commit:abc1234", Real: true}}})
	if stood || nd.Status != "active" || nd.OwnerQuote != "" || !strings.Contains(nd.Why, "Owner activity after the 2026-09-28 words") {
		t.Errorf("dates stand: %+v", nd)
	}
	// Work after the words, then the forge deleted the repository: no commit
	// follows that, so dead words stand.
	gone.Activity = &Activity{Status: record.StatusDormant, Deleted: true, Days: []ActivityDay{{Day: "2026-10-06", Source: "commit"}}, Today: "2026-10-27", TypicalGap: 8, Window: 16, Idle: 21}
	dd := d
	dd.Status = "dead"
	work := Changes{Real: []Work{{Day: "2026-10-06", Kind: "commit", Cite: "commit:abc1234", Real: true}}}
	if nd, stood := QuoteOutcome(gone, dd, "2026-09-28", work); !stood || nd.Status != "dead" || !strings.Contains(nd.Why, "the forge has the repository deleted or archived since") {
		t.Errorf("dead on a deleted repository: %+v", nd)
	}
	if nd, stood := QuoteOutcome(gone, d, "2026-09-28", work); stood || nd.Status != "dormant" || nd.OwnerQuote != "" {
		t.Errorf("dormant words, work after, the dates stand: %+v", nd)
	}
	d.Status = "active"
	if nd, stood := QuoteOutcome(c, d, "2026-09-28", Changes{}); !stood || nd.Why != d.Why {
		t.Errorf("active on their words stands as given: %+v", nd)
	}
}

// A record whose status stays but now rests on other words of the owner's,
// or on the same words dated by when they said them, gets a new entry; the
// same basis adds none.
func TestNewBasis(t *testing.T) {
	old := &record.Record{Folder: "projects/oldtool", Name: "oldtool", Kind: record.KindRepo, Status: record.StatusDead, Created: "2026-10-26", CreatedBy: record.ByOpus,
		Was: []record.Entry{{Status: record.StatusDead, Date: "2026-10-26", By: record.ByOpus, Why: "code: dead on the owner's words of 2026-10-25, note:a (2026-10-25): \"x\"", Words: "x y", WordsFrom: "note:a", WordsOn: "2026-10-25"}}}
	c := Candidate{Folder: "projects/oldtool", Old: old}
	d := Decision{Kind: record.KindRepo, Status: record.StatusDead, Why: "code: dead on the owner's words of 2026-10-06, note:b (2026-10-06): \"z w\"",
		Words: &record.Entry{Words: "z w", WordsFrom: "note:b", WordsOn: "2026-10-06"}}
	if r := Build(c, d, "2026-10-27", nil); len(r.Was) != 2 || r.Was[0].WordsOn != "2026-10-06" {
		t.Errorf("new basis: %+v", r.Was)
	}
	d.Words = &record.Entry{Words: "x y", WordsFrom: "note:a", WordsOn: "2026-10-25"}
	d.Why = old.Was[0].Why
	if r := Build(c, d, "2026-10-27", nil); len(r.Was) != 1 {
		t.Errorf("same basis: %+v", r.Was)
	}
}
