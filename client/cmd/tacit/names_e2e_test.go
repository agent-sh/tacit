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
	"github.com/agent-sh/tacit/client/internal/overlay"
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
	w := newNamesWorld(t, map[string]string{"agent-sh/task-board": "agent-sh/task-board", "agent-sh/agnix": "agent-sh/agnix", "lumen/lumen": "lumen/lumen", "agent-sh/old-board": "agent-sh/task-board"},
		map[string]string{"agent-sh": "Organization", "owner": "User", "lumen": "Organization"})
	tc := w.git("task-board", "https://github.com/agent-sh/task-board.git", "ci/test.sh")
	w.git("agnix", "git@github.com:agent-sh/agnix.git")
	// A repository named like its org: its folder is the repository's.
	w.git("lumen", "https://github.com/lumen/lumen.git")
	w.fact("projects/lumen", "lumen-proxy", "machine_fact", "s6", "The lumen proxy listens on 8787")
	w.fact("projects/agent-sh", "release-from-signed-tags", "user_standard", "s1", "Every agent-sh repository releases from a signed tag")
	w.fact("projects/agent-sh", "task-board-ci", "design_decision", "s2", "CI runs ~/projects/task-board/ci/test.sh before a merge")
	w.fact("projects/agent-sh", "agnix-issues", "reference", "s3", "Bugs go to https://github.com/agent-sh/agnix/issues")
	w.fact("projects/agent-sh", "both-repos", "design_decision", "s4", "~/projects/task-board and ~/projects/agnix share one lint config")
	// A path under the old name the forge renamed (old-board, folded into
	// task-board by code): the record's pointer places it.
	w.fact("projects/agent-sh", "old-board-lock", "machine_fact", "s7", "The lock file is ~/projects/agent-sh/old-board/.lock")
	w.fact("projects/old-board", "old-board-ci", "design_decision", "s8", "The board's CI runs nightly")
	w.fact("projects/task-board", "tb-port", "machine_fact", "s5", "task-board listens on 7790")
	w.commit("an older store")
	fa := &fakeAssessor{first: map[string]assess.Decision{
		"projects/task-board": aDecision("projects/task-board", "repo", "active", "worked in session:s5"),
		"projects/agnix":      aDecision("projects/agnix", "repo", "dormant", "no session lately"),
		"projects/lumen":      aDecision("projects/lumen", "repo", "dormant", "an upstream clone, session:s6"),
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
	if recs["orgs/lumen"] != nil || recs["projects/lumen"] == nil || !recs["projects/lumen"].Live() || recs["projects/lumen"].Org != "lumen" {
		t.Fatalf("lumen, a repository named like its org: %+v %+v", recs["orgs/lumen"], recs["projects/lumen"])
	}
	for _, f := range []string{"projects/task-board", "projects/agnix"} {
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
	want := map[string]int{"orgs/agent-sh": 2, "projects/task-board": 4, "projects/agnix": 1, "projects/agent-sh": 0, "projects/old-board": 0}
	for scope, n := range want {
		if len(live[scope]) != n {
			t.Errorf("%s: %d live facts, want %d: %v", scope, len(live[scope]), n, live)
		}
	}
	if s := strings.Join(live["orgs/agent-sh"], " "); !strings.Contains(s, "release-from-signed-tags") || !strings.Contains(s, "both-repos") {
		t.Errorf("the org keeps its org-wide facts: %v", live["orgs/agent-sh"])
	}

	// An org is never folded into a repository.
	if code := runFolder([]string{"suppress", "--folder", "orgs/agent-sh", "--into", "projects/task-board", "--why", "most facts are task-board's, session:s2"}); code == 0 {
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
	if err != nil || !strings.Contains(tr, "Member projects (2): agnix, task-board") {
		t.Fatalf("the org's index: %v\n%s", err, tr)
	}

	// A model files a fact in the org only when it names two of its
	// repositories, or the org itself.
	env := curateEnv(w.s.Dir, false)
	one := fact.Meta{Name: "x", Scope: "orgs/agent-sh", Type: "design_decision", Description: "task-board builds with cargo", Source: fact.Source{Kind: "observed"}}
	if ps := env.Meta(one, "x", false, false); len(ps) == 0 {
		t.Fatal("a one-repository fact went up to the org")
	}
	two := one
	two.Description = "task-board and agnix both build with cargo"
	if ps := env.Meta(two, "x", false, false); len(ps) != 0 {
		t.Fatalf("a two-repository fact refused: %v", ps)
	}
	if fs, err := lint.Run(w.s.Dir); err != nil || len(fs) != 0 {
		t.Fatalf("lint %v %v", fs, err)
	}
}

// A planned rename: the checkout's directory is shop-v2 (its GitHub name)
// with a symlink storefront, and the owner's ruling says storefront is the
// name. The statements step reads the ruling's claim once; the assessment
// makes storefront the live folder (the owner's decision before the
// checkout's directory) and refuses the fold the other way with the check;
// the record keeps the forge's current name, shown as "storefront (GitHub:
// shop-v2)" in a session's title. A forge rename later updates the current name; the
// folder, the intended name, stays.
func TestPlannedRenameAndForgeRename(t *testing.T) {
	w := newNamesWorld(t, map[string]string{"owner/shop-v2": "owner/shop-v2"}, map[string]string{"owner": "User"})
	shop := w.git("shop-v2", "https://github.com/owner/shop-v2.git")
	if err := os.Symlink("shop-v2", filepath.Join(w.p, "storefront")); err != nil {
		t.Fatal(err)
	}
	w.fact("projects/storefront", "pk-port", "machine_fact", "s1", "The lab server listens on 7781")
	w.fact("projects/shop-v2", "gz-build", "design_decision", "s2", "The lab builds with make lab")
	// The owner's ruling, backed by the quote ledger.
	writeFactFile(t, w.s.Dir, &fact.Fact{Path: "projects/storefront/ruling-storefront-is-the-name.md", Meta: fact.Meta{Name: "ruling-storefront-is-the-name",
		Description: "2026-09-15 owner ruling \"storefront is the name, rename what you need\": references follow the storefront name.", Type: "user_standard", Scope: "projects/storefront",
		Source: fact.Source{Harness: "curator", Host: "box", Kind: "user", OwnerQuote: "storefront is the name, rename what you need", OwnerQuoteFrom: "claude:s9"}, Created: "2026-09-16"},
		Body: "On 2026-09-15 the owner said \"storefront is the name, rename what you need\".\n"}) // privacy-lint: allow (synthetic attribution)
	// A newer ruling whose words name the repository in passing, with a typo:
	// the model misreads a canonical claim from it, which the checks refute.
	wt := "mark the stale wt that keep piling up on storfront(shop-v2) and clean them weekly"
	writeFactFile(t, w.s.Dir, &fact.Fact{Path: "projects/storefront/ruling-worktrees-stale.md", Meta: fact.Meta{Name: "ruling-worktrees-stale",
		Description: "Owner ruling: stale worktrees are marked and removed, " + wt, Type: "design_decision", Scope: "projects/storefront",
		Source: fact.Source{Harness: "curator", Host: "box", Kind: "user", OwnerQuote: wt, OwnerQuoteFrom: "claude:s8"}, Created: "2026-10-19"},
		Body: "The owner said \"" + wt + "\".\n"})
	ql, _ := os.ReadFile(filepath.Join(w.s.Dir, ledger.QuotesPath))
	ql = append(ql, ledger.Quote{Name: "ruling-storefront-is-the-name", Quote: "storefront is the name, rename what you need", From: "claude:s9", TurnHash: "h9", At: "2026-09-16"}.Line()...)
	ql = append(ql, ledger.Quote{Name: "ruling-worktrees-stale", Quote: wt, From: "claude:s8", TurnHash: "h8", At: "2026-10-19"}.Line()...)
	os.MkdirAll(filepath.Join(w.s.Dir, "review"), 0o755)
	os.WriteFile(filepath.Join(w.s.Dir, ledger.QuotesPath), ql, 0o644)
	w.commit("an older store")

	// The statements step reads the ruling: a canonical name.
	prev := statementsRun
	t.Cleanup(func() { statementsRun = prev })
	var asked []string
	statementsRun = func(_ context.Context, _, prompt string) (string, float64, error) {
		asked = append(asked, prompt)
		return `[{"name":"ruling-storefront-is-the-name","says":"choice","claims":[{"kind":"canonical","name":"storefront"}]},` +
			`{"name":"ruling-worktrees-stale","says":"instruction","claims":[{"kind":"canonical","name":"shop-v2","other":"storfront"}]}]`, 0.01, nil
	}
	if _, errs := statementsStep(context.Background(), w.s, statementsOpts{model: "opus"}, &curate.Report{}); errs != 0 {
		t.Fatalf("statements: %d errors", errs)
	}
	if len(asked) != 1 || !strings.Contains(asked[0], "### ruling=ruling-storefront-is-the-name") || !strings.Contains(asked[0], "### ruling=ruling-worktrees-stale") {
		t.Fatalf("the ruling was not read: %v", asked)
	}
	if d := ownerDecisions(w.s.Dir, factsList(t, w.s.Dir), false, nil); d["storefront"].Canonical != "storefront" {
		t.Fatalf("the owner's decision: %+v", d)
	}
	// Unchecked, the misread claim would name shop-v2; the checks drop it
	// (storfront is no name of the repository).
	if d := ownerDecisions(w.s.Dir, factsList(t, w.s.Dir), false, nil); d["shop-v2"].Canonical != "shop-v2" {
		t.Fatalf("unchecked: %+v", d)
	}
	ch := storeChecker(context.Background(), w.s.Dir, false)
	if d := ownerDecisions(w.s.Dir, factsList(t, w.s.Dir), false, sameRepo(context.Background(), ch)); d["shop-v2"].Fact != "" {
		t.Fatalf("a misread claim counted: %+v", d["shop-v2"])
	}

	// The model first keeps shop-v2 (the checkout's directory); code refuses
	// it with the owner's decision and the model folds shop-v2 into storefront.
	naive := aDecision("projects/shop-v2", "repo", "active", "the checkout is here, session:s2")
	fixed := naive
	fixed.SuppressedBy, fixed.Why = "projects/storefront", "the owner named it storefront, session:s2"
	keepPk := aDecision("projects/storefront", "repo", "active", "session:s1")
	fa := &fakeAssessor{first: map[string]assess.Decision{"projects/shop-v2": naive, "projects/storefront": keepPk}, fixed: map[string]assess.Decision{"projects/shop-v2": fixed}}
	rep := w.assess(fa)
	all := strings.Join(fa.prompts, "\n")
	if !strings.Contains(all, "check: the canonical name of this repository is storefront (the owner's decision [[ruling-storefront-is-the-name]]") {
		t.Fatalf("the prompt lacks the canonical line:\n%s", all)
	}
	if !strings.Contains(strings.Join(rep.InvalidEntries, "\n"), "shop-v2 is another name of storefront, the canonical name of this repository") {
		t.Fatalf("the reverse fold was not refused: %v", rep.InvalidEntries)
	}
	recs := w.recs()
	pk, gz := recs["projects/storefront"], recs["projects/shop-v2"]
	if pk == nil || !pk.Live() || gz == nil || gz.SuppressedBy != "projects/storefront" {
		t.Fatalf("storefront %+v shop-v2 %+v", pk, gz)
	}
	if pk.Forge != "github.com/owner/shop-v2" || pk.Display() != "storefront (GitHub: shop-v2)" || pk.Org != "" {
		t.Fatalf("storefront's forge name %q org %q", pk.Forge, pk.Org)
	}
	if out := hookOut(t, startPayload(shop, "r1"), "session-start"); !strings.Contains(out, "# tacit: facts for storefront (GitHub: shop-v2) on ") {
		t.Fatalf("the title:\n%s", out)
	}

	// The forge renames shop-v2 to storefront-lab: the current name follows,
	// the folder stays storefront.
	w2 := fakeGHAccounts(t, "owner", map[string]string{"owner/shop-v2": "owner/storefront-lab", "owner/storefront-lab": "owner/storefront-lab"}, map[string]string{"owner": "User"})
	_ = w2
	os.Remove(filepath.Join(platform.StateDir(w.home), check.CacheFile))
	w.assess(&fakeAssessor{first: map[string]assess.Decision{}})
	pk = w.recs()["projects/storefront"]
	if pk.Forge != "github.com/owner/storefront-lab" || pk.Display() != "storefront (GitHub: storefront-lab)" || pk.SuppressedBy != "" {
		t.Fatalf("after the forge's rename: %+v", pk)
	}
	if fs, err := lint.Run(w.s.Dir); err != nil || len(fs) != 0 {
		t.Fatalf("lint %v %v", fs, err)
	}
}

// A deleted repository: the forge 404s for billing-reporter, no machine has
// a checkout or a remote of it, and two of its three facts name paths that,
// of the repositories proposed for it, only billing-report-bot's checkout
// holds: code folds it there, dead, with the check's output; the forge's wording says the account sees its own
// private repositories. A name the forge renamed folds into the current
// one the same way.
func TestDeletedRepositoryFoldsByItsFacts(t *testing.T) {
	w := newNamesWorld(t, map[string]string{"acme-io/billing-report-bot": "acme-io/billing-report-bot", "owner/webshop": "owner/storefront", "owner/storefront": "owner/storefront", "acme-io/billing": "acme-io/billing"},
		map[string]string{"owner": "User", "acme-io": "Organization"})
	w.git("billing-report-bot", "https://github.com/acme-io/billing-report-bot.git", "deploy/stack.yaml", "deploy/bin/deploy.sh")
	// Another candidate (a near name), without the paths.
	w.git("billing", "https://github.com/acme-io/billing.git", "src/server.c")
	w.git("storefront", "https://github.com/owner/storefront.git")
	w.fact("projects/billing-reporter", "br-stack", "structure", "", "The deploy stack is one file; deploy/stack.yaml runs the scripts", "deploy/stack.yaml", "deploy/bin/*.sh")
	w.fact("projects/billing-reporter", "br-templates", "reference", "", "The report templates are dated per quarter", "deploy/**")
	w.fact("projects/billing-reporter", "br-review", "design_decision", "", "A second reader checks each report first")
	w.fact("projects/billing-report-bot", "brb-ci", "design_decision", "s1", "The bot's CI runs on every PR")
	w.fact("projects/webshop", "webshop-ci", "design_decision", "s2", "The shop's CI runs nightly")
	w.fact("projects/storefront", "sf-port", "machine_fact", "s3", "storefront listens on 7790")
	w.commit("an older store")
	fa := &fakeAssessor{first: map[string]assess.Decision{
		"projects/billing-report-bot": independent(aDecision("projects/billing-report-bot", "repo", "active", "session:s1"), "commits", "remote:github.com/acme-io/billing-report-bot"),
		"projects/storefront":         aDecision("projects/storefront", "repo", "active", "session:s3"),
		"projects/billing":            aDecision("projects/billing", "repo", "dormant", "an upstream checkout"),
	}}
	w.assess(fa)
	for _, p := range fa.prompts {
		if strings.Contains(p, "### folder=projects/billing-reporter\n") || strings.Contains(p, "### folder=projects/webshop\n") {
			t.Fatalf("the model was asked about a folder the checks settle:\n%s", p)
		}
	}
	recs := w.recs()
	vr := recs["projects/billing-reporter"]
	if vr == nil || vr.SuppressedBy != "projects/billing-report-bot" || vr.Status != record.StatusDead {
		t.Fatalf("billing-reporter: %+v", vr)
	}
	if why := vr.Was[0].Why; !strings.Contains(why, "billing-reporter exists nowhere") || !strings.Contains(why, "asked as owner, who sees their own private repositories") || !strings.Contains(why, "point at billing-report-bot") {
		t.Fatalf("why %q", why)
	}
	if g := recs["projects/webshop"]; g == nil || g.SuppressedBy != "projects/storefront" || !strings.Contains(g.Was[0].Why, "renamed to owner/storefront") {
		t.Fatalf("webshop: %+v", g)
	}
	if tc := recs["projects/storefront"]; tc.Forge != "github.com/owner/storefront" || tc.Display() != "storefront" {
		t.Fatalf("storefront's current forge name %q", tc.Forge)
	}
	if vrb := recs["projects/billing-report-bot"]; vrb.Org != "acme-io" {
		t.Fatalf("billing-report-bot's org %q", vrb.Org)
	}
	// Two live projects of the org acme-io: its folder, by code.
	if o := recs["orgs/acme-io"]; o == nil || o.Kind != record.KindOrg || recs["projects/billing"].Org != "acme-io" {
		t.Fatalf("the org of two members: %+v", o)
	}
	if fs, err := lint.Run(w.s.Dir); err != nil || len(fs) != 0 {
		t.Fatalf("lint %v %v", fs, err)
	}
}

// A work org on a Mac lives only in its overlay: on a read-only clone
// with the local layer, the overlay's project folder named for a forge org
// becomes orgs/<org> in the overlay, its facts go to the repository their
// paths name or stay in the org, all in the overlay; the clone and the
// server are untouched. The fact names the checkout under ~, or by its
// absolute path wherever the projects directory is: here the test's temp
// directory. That form tells routing that reads every absolute path from
// one that reads only the system roots where the temp directory is outside
// them (Linux's /tmp, unless TMPDIR or GOTMPDIR is elsewhere); the route
// and the scratch clone below tell routing apart on any machine.
func TestLocalOrgLivesInTheOverlay(t *testing.T) {
	t.Run("tilde", func(t *testing.T) { testLocalOrg(t, true) })
	t.Run("absolute", func(t *testing.T) { testLocalOrg(t, false) })
}

func testLocalOrg(t *testing.T, underHome bool) {
	r := newLocalRig(t)
	seed := filepath.Join(r.root, "seed2")
	gitIn(t, r.root, "clone", "-q", r.bare, seed)
	os.MkdirAll(filepath.Join(seed, "review"), 0o755)
	os.WriteFile(filepath.Join(seed, filepath.FromSlash(ledger.FormatPath)), ledger.Format{Version: ledger.FormatVersion, At: "x"}.Encode(), 0o644)
	gitIn(t, seed, "add", "-A")
	gitIn(t, seed, "commit", "-q", "-m", "format")
	gitIn(t, seed, "push", "-q", "origin", "HEAD:main")
	if err := r.s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.serverHead = gitIn(t, r.bare, "rev-parse", "main")
	r.cloneHead = gitIn(t, r.s.Dir, "rev-parse", "HEAD")
	r.cloneFiles = treeBytes(t, r.s.Dir)
	fakeGHAccounts(t, "owner", map[string]string{"workco/billing": "workco/billing"}, map[string]string{"workco": "Organization", "owner": "User"})
	billing := filepath.Join(r.root, "projects", "billing") // the fixture's projects directory
	named := filepath.ToSlash(filepath.Join(billing, "config.yaml"))
	if underHome {
		t.Setenv("TACIT_PROJECTS", filepath.Join(r.home, "projects"))
		billing = filepath.Join(r.home, "projects", "billing")
		named = "~/projects/billing/config.yaml"
	}
	os.MkdirAll(billing, 0o755)
	gitIn(t, billing, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(billing, "main.go"), []byte("x"), 0o644)
	gitIn(t, billing, "add", "main.go")
	gitIn(t, billing, "commit", "-q", "-m", "c")
	gitIn(t, billing, "remote", "add", "origin", "https://github.com/workco/billing.git")
	// A task's scratch clone outside home, of no checkout here and with no
	// work in it, is no repository (repoid.Elsewhere): the fact still names
	// billing alone.
	scratch := filepath.Join(r.root, "scratch", "vendor-lib")
	os.MkdirAll(scratch, 0o755)
	gitIn(t, scratch, "init", "-q", "-b", "main")
	gitIn(t, scratch, "remote", "add", "origin", "https://github.com/vendor/lib.git")
	// Another, in a temp directory under home, whose path holds
	// projects/billing: a checkout here, so it names nothing, never another
	// machine's billing checkout. Its fact stays in the org.
	t.Setenv("TMPDIR", filepath.Join(r.home, "tmp"))
	scratchHome := filepath.Join(r.home, "tmp", "projects", "billing", "vendor-lib")
	os.MkdirAll(scratchHome, 0o755)
	gitIn(t, scratchHome, "init", "-q", "-b", "main")
	gitIn(t, scratchHome, "remote", "add", "origin", "https://github.com/vendor/lib.git")
	for name, desc := range map[string]string{
		"vendor-lib-scratch": "A one-off clone of the vendor lib sat in ~/tmp/projects/billing/vendor-lib",
		"workco-reviews":     "Every workco change needs two reviewers",
		"billing-config":     "The billing service reads " + named + "; a one-off copy of the vendor lib was at " + filepath.ToSlash(filepath.Join(scratch, "lib.go")),
		// A route names no checkout, though it holds projects/<a known
		// name>: the fact stays in the org.
		"workco-dashboard-route": "The workco dashboard calls /api/projects/billing/settings",
	} {
		b := rotFile(name, "projects/workco", "design_decision", desc, desc+".", "2026-10-10", "")
		if err := overlay.Write(r.local, r.s.Dir, overlay.CuratedDir+"/projects/workco/"+name+".md", []byte(b)); err != nil {
			t.Fatal(err)
		}
	}
	fa := &fakeAssessor{first: map[string]assess.Decision{
		"projects/billing": aDecision("projects/billing", "repo", "dormant", "no session touched it"),
	}}
	fa.install(t)
	if code := runAssess(nil); code != 0 {
		t.Fatalf("assess %d", code)
	}
	if code := runMigrate(nil); code != 0 {
		t.Fatalf("migrate %d", code)
	}
	if got := gitIn(t, r.bare, "rev-parse", "main"); got != r.serverHead {
		t.Fatal("the server moved")
	}
	if got := gitIn(t, r.s.Dir, "rev-parse", "HEAD"); got != r.cloneHead {
		t.Fatal("the clone's HEAD moved")
	}
	if out := gitIn(t, r.s.Dir, "status", "--porcelain", "--ignored"); out != "" {
		t.Fatalf("the clone changed:\n%s", out)
	}
	cur := filepath.Join(r.local, overlay.CuratedDir)
	b, err := os.ReadFile(filepath.Join(cur, "orgs", "workco", record.FileName))
	if err != nil {
		t.Fatalf("no org record in the overlay: %v", err)
	}
	if o, err := record.Parse("orgs/workco", b); err != nil || o.Kind != record.KindOrg {
		t.Fatalf("org %+v %v", o, err)
	}
	pb, _ := os.ReadFile(filepath.Join(cur, "projects", "workco", record.FileName))
	if p, err := record.Parse("projects/workco", pb); err != nil || p.SuppressedBy != "orgs/workco" {
		t.Fatalf("projects/workco %+v %v", p, err)
	}
	where := map[string]string{}
	lf, _ := overlay.Load(r.local)
	for _, f := range lf {
		if overlay.IsCurated(f.Path) && f.Meta.SupersededBy == "" && f.Meta.Origin != "" {
			where[f.Meta.Origin] = f.Meta.Scope
		}
	}
	if where["workco-reviews"] != "orgs/workco" || where["billing-config"] != "projects/billing" || where["workco-dashboard-route"] != "orgs/workco" || where["vendor-lib-scratch"] != "orgs/workco" {
		t.Fatalf("where the org folder's facts went: %v", where)
	}
}
