package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/assess"
	"github.com/agent-sh/tacit/client/internal/check"
	"github.com/agent-sh/tacit/client/internal/curate"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/lint"
	"github.com/agent-sh/tacit/client/internal/platform"
	"github.com/agent-sh/tacit/client/internal/record"
)

// A project folder named for a forge organization becomes the org's folder
// by code: orgs/agent-sh gets an org record, projects/agent-sh folds into
// it, and the migration sends each fact to the one repository its own paths
// or forge URLs name, keeping the org-wide ones in the org. The members'
// records carry the org and the forge name; the org can never be folded into
// a repository; a session in a member reads the org's standards; a model's
// fact goes up to the org only when it names two of its repositories.
func TestOrgFolderFromAnOrgNamedProjectFolder(t *testing.T) {
	w := newNamesWorld(t, map[string]string{"agent-sh/tower-crane": "agent-sh/tower-crane", "agent-sh/agnix": "agent-sh/agnix"},
		map[string]string{"agent-sh": "Organization", "owner": "User"})
	tc := w.git("tower-crane", "https://github.com/agent-sh/tower-crane.git", "ci/test.sh")
	w.git("agnix", "git@github.com:agent-sh/agnix.git")
	w.fact("projects/agent-sh", "release-from-signed-tags", "user_standard", "s1", "Every agent-sh repository releases from a signed tag")
	w.fact("projects/agent-sh", "tower-crane-ci", "design_decision", "s2", "CI runs ~/projects/tower-crane/ci/test.sh before a merge")
	w.fact("projects/agent-sh", "agnix-issues", "reference", "s3", "Bugs go to https://github.com/agent-sh/agnix/issues")
	w.fact("projects/agent-sh", "both-repos", "design_decision", "s4", "~/projects/tower-crane and ~/projects/agnix share one lint config")
	w.fact("projects/tower-crane", "tc-port", "machine_fact", "s5", "tower-crane listens on 7790")
	w.commit("an older store")
	fa := &fakeAssessor{first: map[string]assess.Decision{
		"projects/tower-crane": aDecision("projects/tower-crane", "repo", "active", "worked in session:s5"),
		"projects/agnix":       aDecision("projects/agnix", "repo", "dormant", "no session lately"),
	}}
	w.assess(fa)
	for _, p := range fa.prompts {
		if strings.Contains(p, "### folder=projects/agent-sh") {
			t.Fatal("the model was asked about the org-named folder")
		}
	}
	recs := w.recs()
	org := recs["orgs/agent-sh"]
	if org == nil || org.Kind != record.KindOrg || org.Status != record.StatusActive || !record.OrgChecked(org) {
		t.Fatalf("the org record: %+v", org)
	}
	if p := recs["projects/agent-sh"]; p == nil || p.SuppressedBy != "orgs/agent-sh" || p.Status != record.StatusDead {
		t.Fatalf("projects/agent-sh: %+v", p)
	}
	for _, f := range []string{"projects/tower-crane", "projects/agnix"} {
		r := recs[f]
		if r == nil || r.Org != "agent-sh" || r.Forge != "github.com/agent-sh/"+r.Name || r.Display() != r.Name {
			t.Errorf("%s: org %q forge %q", f, r.Org, r.Forge)
		}
	}

	if _, err := migrateStep(context.Background(), w.s, migrateBatch, migrateCommits, false, nil); err != nil {
		t.Fatal(err)
	}
	live := map[string][]string{}
	for _, f := range factsByName(t, w.s.Dir) {
		if !f.InInbox() && f.Meta.SupersededBy == "" && f.Meta.Retired == "" {
			live[f.Meta.Scope] = append(live[f.Meta.Scope], f.Meta.Origin+f.Meta.Name)
		}
	}
	want := map[string]int{"orgs/agent-sh": 2, "projects/tower-crane": 2, "projects/agnix": 1, "projects/agent-sh": 0}
	for scope, n := range want {
		if len(live[scope]) != n {
			t.Errorf("%s: %d live facts, want %d: %v", scope, len(live[scope]), n, live)
		}
	}
	if s := strings.Join(live["orgs/agent-sh"], " "); !strings.Contains(s, "release-from-signed-tags") || !strings.Contains(s, "both-repos") {
		t.Errorf("the org keeps its org-wide facts: %v", live["orgs/agent-sh"])
	}

	// An org is never folded into a repository.
	if code := runFolder([]string{"suppress", "--folder", "orgs/agent-sh", "--into", "projects/tower-crane", "--why", "most facts are tower-crane's, session:s2"}); code == 0 {
		t.Fatal("an org was folded into a repository")
	}
	if r := w.recs()["orgs/agent-sh"]; r.SuppressedBy != "" {
		t.Fatalf("org %+v", r)
	}

	// A session in a member reads the org's standard at start; one in
	// another repository does not.
	out := hookOut(t, startPayload(tc, "r1"), "session-start")
	if !strings.Contains(out, "release-from-signed-tags") {
		t.Fatalf("the org's standard is not delivered in its member:\n%s", out)
	}
	other := w.git("elsewhere", "")
	if out := hookOut(t, startPayload(other, "r2"), "session-start"); strings.Contains(out, "release-from-signed-tags") {
		t.Fatalf("delivered outside the org:\n%s", out)
	}
	tr, err := treeText(w.s.Dir, "orgs/agent-sh")
	if err != nil || !strings.Contains(tr, "Member projects (2): agnix, tower-crane") {
		t.Fatalf("the org's index: %v\n%s", err, tr)
	}

	// A model files a fact in the org only when it names two of its
	// repositories, or the org itself.
	env := curateEnv(w.s.Dir, false)
	one := fact.Meta{Name: "x", Scope: "orgs/agent-sh", Type: "design_decision", Description: "tower-crane builds with cargo", Source: fact.Source{Kind: "observed"}}
	if ps := env.Meta(one, "x", false, false); len(ps) == 0 {
		t.Fatal("a one-repository fact went up to the org")
	}
	two := one
	two.Description = "tower-crane and agnix both build with cargo"
	if ps := env.Meta(two, "x", false, false); len(ps) != 0 {
		t.Fatalf("a two-repository fact refused: %v", ps)
	}
	if fs, err := lint.Run(w.s.Dir); err != nil || len(fs) != 0 {
		t.Fatalf("lint %v %v", fs, err)
	}
}

// A planned rename: the checkout's directory is ginza (its GitHub name)
// with a symlink pikadon, and the owner's ruling says pikadon is the name.
// The statements step reads the ruling's claim once; the assessment makes
// pikadon the live folder (the owner's decision before the checkout's
// directory) and refuses the fold the other way with the check; the record
// keeps the forge's current name, shown as "pikadon (GitHub: ginza)" in a
// session's title. A forge rename later updates the current name; the
// folder, the intended name, stays.
func TestPlannedRenameAndForgeRename(t *testing.T) {
	w := newNamesWorld(t, map[string]string{"owner/ginza": "owner/ginza"}, map[string]string{"owner": "User"})
	ginza := w.git("ginza", "https://github.com/owner/ginza.git")
	if err := os.Symlink("ginza", filepath.Join(w.p, "pikadon")); err != nil {
		t.Fatal(err)
	}
	w.fact("projects/pikadon", "pk-port", "machine_fact", "s1", "The lab server listens on 7781")
	w.fact("projects/ginza", "gz-build", "design_decision", "s2", "The lab builds with make lab")
	// The owner's ruling, backed by the quote ledger.
	writeFactFile(t, w.s.Dir, &fact.Fact{Path: "projects/pikadon/ruling-pikadon-is-the-name.md", Meta: fact.Meta{Name: "ruling-pikadon-is-the-name",
		Description: "2026-10-15 owner ruling \"pikadon is the name, move what needed\": references follow the pikadon name.", Type: "user_standard", Scope: "projects/pikadon",
		Source: fact.Source{Harness: "curator", Host: "box", Kind: "user", OwnerQuote: "pikadon is the name, move what needed", OwnerQuoteFrom: "claude:s9"}, Created: "2026-10-16"},
		Body: "On 2026-10-15 the owner said \"pikadon is the name, move what needed\".\n"})
	ql, _ := os.ReadFile(filepath.Join(w.s.Dir, ledger.QuotesPath))
	ql = append(ql, ledger.Quote{Name: "ruling-pikadon-is-the-name", Quote: "pikadon is the name, move what needed", From: "claude:s9", TurnHash: "h9", At: "2026-10-16"}.Line()...)
	os.MkdirAll(filepath.Join(w.s.Dir, "review"), 0o755)
	os.WriteFile(filepath.Join(w.s.Dir, ledger.QuotesPath), ql, 0o644)
	w.commit("an older store")

	// The statements step reads the ruling: a canonical name.
	prev := statementsRun
	t.Cleanup(func() { statementsRun = prev })
	var asked []string
	statementsRun = func(_ context.Context, _, prompt string) (string, float64, error) {
		asked = append(asked, prompt)
		return `[{"name":"ruling-pikadon-is-the-name","says":"choice","claims":[{"kind":"canonical","name":"pikadon"}]}]`, 0.01, nil
	}
	if _, errs := statementsStep(context.Background(), w.s, statementsOpts{model: "opus"}, &curate.Report{}); errs != 0 {
		t.Fatalf("statements: %d errors", errs)
	}
	if len(asked) != 1 || !strings.Contains(asked[0], "### ruling=ruling-pikadon-is-the-name") {
		t.Fatalf("the ruling was not read: %v", asked)
	}
	if d := ownerDecisions(w.s.Dir, factsList(t, w.s.Dir), false); d["pikadon"].Canonical != "pikadon" {
		t.Fatalf("the owner's decision: %+v", d)
	}

	// The model first keeps ginza (the checkout's directory); code refuses it
	// with the owner's decision and the model folds ginza into pikadon.
	naive := aDecision("projects/ginza", "repo", "active", "the checkout is here, session:s2")
	fixed := naive
	fixed.SuppressedBy, fixed.Why = "projects/pikadon", "the owner named it pikadon, session:s2"
	keepPk := aDecision("projects/pikadon", "repo", "active", "session:s1")
	fa := &fakeAssessor{first: map[string]assess.Decision{"projects/ginza": naive, "projects/pikadon": keepPk}, fixed: map[string]assess.Decision{"projects/ginza": fixed}}
	rep := w.assess(fa)
	all := strings.Join(fa.prompts, "\n")
	if !strings.Contains(all, "check: the canonical name of this repository is pikadon (the owner's decision [[ruling-pikadon-is-the-name]]") {
		t.Fatalf("the prompt lacks the canonical line:\n%s", all)
	}
	if !strings.Contains(strings.Join(rep.InvalidEntries, "\n"), "ginza is another name of pikadon, the canonical name of this repository") {
		t.Fatalf("the reverse fold was not refused: %v", rep.InvalidEntries)
	}
	recs := w.recs()
	pk, gz := recs["projects/pikadon"], recs["projects/ginza"]
	if pk == nil || !pk.Live() || gz == nil || gz.SuppressedBy != "projects/pikadon" {
		t.Fatalf("pikadon %+v ginza %+v", pk, gz)
	}
	if pk.Forge != "github.com/owner/ginza" || pk.Display() != "pikadon (GitHub: ginza)" || pk.Org != "" {
		t.Fatalf("pikadon's forge name %q org %q", pk.Forge, pk.Org)
	}
	if out := hookOut(t, startPayload(ginza, "r1"), "session-start"); !strings.Contains(out, "# tacit: facts for pikadon (GitHub: ginza) on ") {
		t.Fatalf("the title:\n%s", out)
	}

	// The forge renames ginza to pikadon-lab: the current name follows, the
	// folder stays pikadon.
	w2 := fakeGHAccounts(t, "owner", map[string]string{"owner/ginza": "owner/pikadon-lab", "owner/pikadon-lab": "owner/pikadon-lab"}, map[string]string{"owner": "User"})
	_ = w2
	os.Remove(filepath.Join(platform.StateDir(w.home), check.CacheFile))
	w.assess(&fakeAssessor{first: map[string]assess.Decision{}})
	pk = w.recs()["projects/pikadon"]
	if pk.Forge != "github.com/owner/pikadon-lab" || pk.Display() != "pikadon (GitHub: pikadon-lab)" || pk.SuppressedBy != "" {
		t.Fatalf("after the forge's rename: %+v", pk)
	}
	if fs, err := lint.Run(w.s.Dir); err != nil || len(fs) != 0 {
		t.Fatalf("lint %v %v", fs, err)
	}
}

// A deleted repository: the forge 404s for valkey-reviewer, no machine has
// a checkout or a remote of it, and its facts' paths exist only in the live
// repository valkey-review-bot's checkout: code folds it there, dead, with
// the check's output; the forge's wording says the account sees its own
// private repositories. A name the forge renamed folds into the current
// one the same way.
func TestDeletedRepositoryFoldsByItsFacts(t *testing.T) {
	w := newNamesWorld(t, map[string]string{"valkey-io/valkey-review-bot": "valkey-io/valkey-review-bot", "owner/gishra": "owner/tower-crane", "owner/tower-crane": "owner/tower-crane"},
		map[string]string{"owner": "User", "valkey-io": "Organization"})
	w.git("valkey-review-bot", "https://github.com/valkey-io/valkey-review-bot.git", "infra/cdk.json", "infra/bin/infra.ts")
	w.git("tower-crane", "https://github.com/owner/tower-crane.git")
	w.fact("projects/valkey-reviewer", "vr-cdk", "structure", "", "The infra is a CDK app; infra/cdk.json runs ts-node", "infra/cdk.json", "infra/bin/*.ts")
	w.fact("projects/valkey-reviewer", "vr-pins", "reference", "", "The model pins are dated 2026-08-03")
	w.fact("projects/valkey-review-bot", "vrb-ci", "design_decision", "s1", "The bot's CI runs on every PR")
	w.fact("projects/gishra", "gishra-ci", "design_decision", "s2", "The crane's CI runs nightly")
	w.fact("projects/tower-crane", "tc-port", "machine_fact", "s3", "tower-crane listens on 7790")
	w.commit("an older store")
	fa := &fakeAssessor{first: map[string]assess.Decision{
		"projects/valkey-review-bot": aDecision("projects/valkey-review-bot", "repo", "active", "session:s1"),
		"projects/tower-crane":       aDecision("projects/tower-crane", "repo", "active", "session:s3"),
	}}
	w.assess(fa)
	for _, p := range fa.prompts {
		if strings.Contains(p, "### folder=projects/valkey-reviewer\n") || strings.Contains(p, "### folder=projects/gishra\n") {
			t.Fatalf("the model was asked about a folder the checks settle:\n%s", p)
		}
	}
	recs := w.recs()
	vr := recs["projects/valkey-reviewer"]
	if vr == nil || vr.SuppressedBy != "projects/valkey-review-bot" || vr.Status != record.StatusDead {
		t.Fatalf("valkey-reviewer: %+v", vr)
	}
	if why := vr.Was[0].Why; !strings.Contains(why, "valkey-reviewer exists nowhere") || !strings.Contains(why, "asked as owner, who sees their own private repositories") || !strings.Contains(why, "point at valkey-review-bot") {
		t.Fatalf("why %q", why)
	}
	if g := recs["projects/gishra"]; g == nil || g.SuppressedBy != "projects/tower-crane" || !strings.Contains(g.Was[0].Why, "renamed to owner/tower-crane") {
		t.Fatalf("gishra: %+v", g)
	}
	if tc := recs["projects/tower-crane"]; tc.Forge != "github.com/owner/tower-crane" || tc.Display() != "tower-crane" {
		t.Fatalf("tower-crane's current forge name %q", tc.Forge)
	}
	if vrb := recs["projects/valkey-review-bot"]; vrb.Org != "valkey-io" {
		t.Fatalf("valkey-review-bot's org %q", vrb.Org)
	}
	if recs["orgs/valkey-io"] != nil {
		t.Fatal("an org folder for one member")
	}
	if fs, err := lint.Run(w.s.Dir); err != nil || len(fs) != 0 {
		t.Fatalf("lint %v %v", fs, err)
	}
}
