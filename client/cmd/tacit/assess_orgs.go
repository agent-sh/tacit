package main

// Orgs, current forge names and the folds code makes on its own (SPEC.md,
// "Orgs", "Planned renames", "Checked claims"). Code, not the model:
//
//   - A project folder named for a forge organization (projects/agent-sh,
//     the forge says agent-sh is an Organization whose repositories the
//     catalogs' remotes hold), and no machine's checkout or remote of that
//     name (widget/widget is a repository), becomes that org's folder:
//     orgs/<name> gets a record of kind org, and the project folder is
//     folded into it, so the migration moves each fact to the one
//     repository its own paths name, or keeps it in the org.
//   - Every live project record carries its current forge name and its
//     forge organization, read from the forge for its origin's URL (the
//     forge follows renames). An org that two or more of them belong to
//     gets its folder.
//   - A name the forge says was renamed folds into the current name; a name
//     that exists nowhere (no checkout, no remote, a 404 from the forge)
//     whose facts all point at one live repository by their own paths, forge
//     URLs or the checkouts at their paths folds into it, dead.
//   - Among the names one repository goes by, code says which is canonical:
//     the owner's decision, else the main checkout's directory, else the
//     forge's name. A fold the other way is refused with the check output.

import (
	"context"
	"fmt"
	"os"
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/agent-sh/tacit/client/internal/assess"
	"github.com/agent-sh/tacit/client/internal/check"
	"github.com/agent-sh/tacit/client/internal/curate"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/overlay"
	"github.com/agent-sh/tacit/client/internal/record"
	"github.com/agent-sh/tacit/client/internal/store"
)

// format2 reports a store whose marker is format 2 or newer: org folders and
// a project record's org and forge fields may be written (with the local
// layer, into the overlay, whatever the store's format).
func (a *assessor) format2() bool {
	if a.local {
		return true
	}
	f, ok, err := ledger.ReadFormat(a.dir)
	return err == nil && ok && f.Orgs()
}

// newOrgRecord is the record code makes of an org from the forge's answer.
func newOrgRecord(name string, res check.Result, today string) *record.Record {
	return &record.Record{Folder: "orgs/" + name, Kind: record.KindOrg, Name: name,
		Description: fmt.Sprintf("The %s organization on the forge: facts that hold across its repositories.", name),
		Status:      record.StatusActive,
		Was: []record.Entry{{Status: record.StatusActive, Date: today, By: record.ByOpus,
			Why: "code: the forge says " + name + " is an organization (check: " + res.Line() + ")"}},
		Evidence:  []record.Evidence{{Gathered: today, Checks: []check.Result{res}}},
		Created:   today,
		CreatedBy: record.ByOpus,
	}
}

// orgFolders are the decisions that make org folders of the project folders
// named for forge organizations: the org's record, and the project folder's,
// folded into it. Code checks each (record.Validate) and skips one it
// refuses (an owner's pointer stays theirs).
func (a *assessor) orgFolders(only map[string]bool) []decided {
	if !a.format2() || a.checker.Forge == nil {
		return nil
	}
	folders := a.projectFolders()
	for f, r := range a.recs {
		if strings.HasPrefix(f, "projects/") && record.Broad(r.Kind) == "project" {
			folders[f] = true
		}
	}
	pending := map[string]*record.Record{}
	var out []decided
	for _, folder := range sortedKeysBool(folders) {
		name := path.Base(folder)
		orgF := "orgs/" + name
		if only != nil && !only[folder] && !only[orgF] {
			continue
		}
		old := a.recs[folder]
		if old != nil && old.SuppressedBy == orgF {
			continue
		}
		// A checkout or a remote of this name makes it a repository's folder
		// (widget/widget): only a name no machine holds as a repository
		// is the org's.
		if rc := a.checker.Repo(a.ctx, name); rc.Checkout != "" || (len(rc.Results) > 1 && rc.Results[1].Verdict == check.Holds) {
			continue
		}
		isOrg, res := a.checker.IsOrg(a.ctx, name)
		if !isOrg {
			continue
		}
		org := pending[orgF]
		if org == nil {
			org = a.recs[orgF]
		}
		if org == nil {
			org = newOrgRecord(name, res, a.today)
			if ps := record.Validate(nil, org, record.ByOpus, a.recordEnv(pending, nil)); len(ps) > 0 {
				fmt.Fprintf(os.Stderr, "assess: %s: refused: %s\n", orgF, strings.Join(messages(ps), "; "))
				continue
			}
			pending[orgF] = org
			out = append(out, decided{c: assess.Candidate{Folder: orgF, Reason: "a forge organization"}, rec: org})
		}
		c := a.candidate(folder, "a project folder named for a forge organization", old)
		why := assess.NotProject + fmt.Sprintf("%s is an organization on the forge, not a repository (check: %s): its folder is %s; each fact goes to the repository its own paths name, the others stay there as the org's", name, res.Line(), orgF)
		var rec *record.Record
		if old == nil {
			kind := record.KindFolder
			if c.GitRoot {
				kind = record.KindRepo
			}
			rec = &record.Record{Kind: kind, Status: record.StatusDead, Created: a.today, CreatedBy: record.ByOpus,
				Was: []record.Entry{{Status: record.StatusDead, Date: a.today, By: record.ByOpus, Why: why}}}
		} else {
			rec = old.Clone()
			if rec.Status != record.StatusDead || len(rec.Was) == 0 || rec.Was[0].Why != why {
				rec.Was = append([]record.Entry{{Status: record.StatusDead, Date: a.today, By: record.ByOpus, Why: why}}, rec.Was...)
				rec.Status = record.StatusDead
			}
		}
		rec.Folder, rec.Name = folder, name
		rec.Description = fmt.Sprintf("The folder named for the %s organization; its facts are the org's (%s) or their repositories'.", name, orgF)
		rec.SuppressedBy = orgF
		rec.Evidence = append(append([]record.Evidence(nil), c.Evidence...), record.Evidence{Gathered: a.today, Checks: []check.Result{res}})
		if ps := record.Validate(old, rec, record.ByOpus, a.recordEnv(pending, nil)); len(ps) > 0 {
			fmt.Fprintf(os.Stderr, "assess: %s: not folded into %s: %s\n", folder, orgF, strings.Join(messages(ps), "; "))
			continue
		}
		pending[folder] = rec
		out = append(out, decided{c: c, rec: rec, old: old})
	}
	return out
}

// forgeFacts are a project's current forge name (a normalized URL) and its
// forge organization, from the forge: the URL of its origin (this machine's
// checkout), else its one remote on the forge, else the forge's own name of
// it; with the checks. ok is false when the forge did not answer (the
// record keeps what it has).
func (a *assessor) forgeFacts(name string) (forge, org string, checks []check.Result, ok bool) {
	if a.checker.Forge == nil {
		return "", "", nil, false
	}
	// The checkout the folder is: its own name's, else the checkout its name
	// is another name of (a symlink's), else that of a folder suppressed into
	// it (the forge's name of a planned rename, say).
	names := []string{name}
	if own := a.catalog.AliasOf(name); own != "" {
		names = append(names, own)
	}
	for _, l := range record.Losers(a.recs, "projects/"+name) {
		names = append(names, path.Base(l))
	}
	origin := ""
	var urls []string
	for _, n := range names {
		if r, has := a.catalog.Repo(n); has && r.Root != "" {
			origin, urls = r.Origin, append([]string(nil), r.URLs...)
		}
		for _, h := range a.shared[n] {
			for _, u := range h.URLs {
				if !slices.Contains(urls, u) {
					urls = append(urls, u)
				}
			}
		}
		if len(urls) > 0 {
			break
		}
	}
	url := a.checker.ForgeURL(a.ctx, origin, urls)
	if url == "" && len(urls) == 0 {
		if rc := a.checker.Repo(a.ctx, name); rc.Forge != "" {
			url = strings.ToLower(a.checker.Forge.Forge.Host() + "/" + rc.Forge)
		}
	}
	if url == "" {
		return "", "", nil, false
	}
	fr := a.checker.ForgeName(a.ctx, url)
	checks = append(checks, fr)
	switch fr.Verdict {
	case check.Holds:
		forge = fr.Value
	case check.Fails:
		return "", "", checks, true // the forge has no such repository now
	default:
		return "", "", checks, false
	}
	org, or := a.checker.OrgOf(a.ctx, forge)
	checks = append(checks, or)
	if or.Verdict == check.Unknown {
		return forge, "", checks, false
	}
	return forge, org, checks, true
}

// forgeNames are the records whose current forge name or organization the
// forge's answer changed (a rename updates the name; the folder, the
// intended name, stays) or that go by names code proves and they do not list
// (provenNames), and the org folders that two or more live projects now
// belong to. With the local layer only the layer's own folders: a store
// record's fields are the store's.
func (a *assessor) forgeNames(only map[string]bool) []decided {
	if !a.format2() {
		return nil
	}
	pending := map[string]*record.Record{}
	var out []decided
	members := map[string]int{}
	for _, f := range sortedRecordFolders(a.recs) {
		r := a.recs[f]
		if !strings.HasPrefix(f, "projects/") || record.Broad(r.Kind) != "project" || !r.Live() || notProject(r) {
			continue
		}
		if a.local && a.storeRecs[f] != nil {
			if r.Org != "" {
				members[r.Org]++
			}
			continue
		}
		if only != nil && !only[f] {
			if r.Org != "" {
				members[r.Org]++
			}
			continue
		}
		forge, org, checks, ok := "", "", []check.Result(nil), false
		if a.checker.Forge != nil {
			forge, org, checks, ok = a.forgeFacts(r.Name)
		}
		if !ok {
			forge, org, checks = r.Forge, r.Org, nil
		}
		names, nameChecks := a.provenNames(r, forge, pending)
		if forge == r.Forge && org == r.Org && len(names) == 0 {
			if r.Org != "" {
				members[r.Org]++
			}
			continue
		}
		rec := r.Clone()
		rec.Forge, rec.Org = forge, org
		rec.Aliases = append(rec.Aliases, names...)
		if checks = append(checks, nameChecks...); len(checks) > 0 {
			rec.Evidence = append(append([]record.Evidence(nil), rec.Evidence...), record.Evidence{Gathered: a.today, Checks: checks})
		}
		if ps := record.Validate(r, rec, record.ByOpus, a.recordEnv(pending, nil)); len(ps) > 0 {
			fmt.Fprintf(os.Stderr, "assess: %s: forge name not written: %s\n", f, strings.Join(messages(ps), "; "))
			continue
		}
		if rec.Org != "" {
			members[rec.Org]++
		}
		pending[f] = rec
		out = append(out, decided{c: assess.Candidate{Folder: f, Reason: "its forge name or org changed"}, rec: rec, old: r})
	}
	for _, org := range sortedKeysInt(members) {
		orgF := "orgs/" + org
		if members[org] < 2 || a.recs[orgF] != nil || pending[orgF] != nil || (only != nil && !only[orgF]) || a.checker.Forge == nil {
			continue
		}
		isOrg, res := a.checker.IsOrg(a.ctx, org)
		if !isOrg {
			continue
		}
		rec := newOrgRecord(org, res, a.today)
		if ps := record.Validate(nil, rec, record.ByOpus, a.recordEnv(pending, nil)); len(ps) > 0 {
			continue
		}
		pending[orgF] = rec
		out = append(out, decided{c: assess.Candidate{Folder: orgF, Reason: "a forge organization of two or more projects"}, rec: rec})
	}
	return out
}

// provenNames are the other names code proves r's repository goes by that
// r does not list yet (the owner's rule: dry claims are checked; a name a
// check proves is code's to add, not the model's): the catalogs' other
// names of its checkout and clones of its origin (check.Checker.Same), and
// the forge's current name of it (forge, host/owner/name) when that differs
// from the folder's. A name another folder has (its folder, or its name or
// alias) or that holds facts is left out: that folder has or gets its own
// record, suppressed into r's. The checks are returned for the evidence.
func (a *assessor) provenNames(r *record.Record, forge string, pending map[string]*record.Record) ([]string, []check.Result) {
	taken := func(n string) bool {
		if n == r.Name || slices.Contains(r.Aliases, n) || !fact.IsSlug(n) || a.holdsFacts("projects/"+n) {
			return true
		}
		for f, o := range a.recs {
			if p := pending[f]; p != nil {
				o = p
			}
			if f != r.Folder && (f == "projects/"+n || slices.Contains(o.Names(), n)) {
				return true
			}
		}
		for f, o := range pending {
			if f != r.Folder && (f == "projects/"+n || slices.Contains(o.Names(), n)) {
				return true
			}
		}
		return false
	}
	var out []string
	var checks []check.Result
	for _, res := range a.checker.Same(r.Name) {
		if n := strings.ToLower(res.Value); !taken(n) && !slices.Contains(out, n) {
			out = append(out, n)
			checks = append(checks, res)
		}
	}
	if fn := strings.ToLower(path.Base(forge)); forge != "" && !taken(fn) && !slices.Contains(out, fn) {
		out = append(out, fn)
	}
	return out, checks
}

// codeSteps writes decisions code makes without the model (org folders, the
// forge's names) in one commit (or the overlay), and keeps them as the
// run's records. It returns the errors.
func (a *assessor) codeSteps(ctx context.Context, s *store.Store, what string, ds []decided, dry bool, rep *curate.Report) int {
	if len(ds) == 0 {
		return 0
	}
	if dry {
		for _, d := range ds {
			fmt.Printf("- %s: %s, %s (%s)\n", d.rec.Folder, d.rec.Kind, d.rec.Status, what)
		}
		return 0
	}
	n, err := a.write(ctx, s, ds)
	rep.AssessRecords += n
	for _, d := range ds {
		a.recs[d.rec.Folder] = d.rec
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "assess: write:", err)
		return 1
	}
	return 0
}

// codeDecision is the decision code makes for a candidate on its own, when
// the checks settle it (ok false: the model decides):
//
//   - a record of the model's whose dates moved it across its window
//     (statusDrift) gets code's status from them (assess.Activity);
//   - a name the forge says was renamed folds into the current name, which
//     exists by its check and has a live record;
//   - a name that exists nowhere (Candidate.Absent) whose live facts point at
//     one repository by their own evidence, and none at another, folds into
//     that repository's live folder, dead: the facts' paths (repoid on this
//     machine, or a projects directory path naming a catalog's checkout),
//     the forge URLs they name, the repository-relative paths they name that
//     exist at HEAD in exactly one of the proposed repositories (a checkout
//     here, else the forge's default branch), else the checkouts their
//     sessions worked in.
//
// recs are the records with this run's pending ones. wait is true when the
// checks settle it but the repository it folds into has no live record yet
// (it may get one later in the run).
func (a *assessor) codeDecision(c assess.Candidate, recs func(string) *record.Record) (d assess.Decision, ok, wait bool) {
	d, ok, wait = a.codeDecisionOf(c, recs)
	return d, ok, wait
}

func (a *assessor) codeDecisionOf(c assess.Candidate, recs func(string) *record.Record) (assess.Decision, bool, bool) {
	// A record whose dates moved it across its window: code's status, the
	// rest of the record as it was (it stood alone on its independence).
	if r := c.Old; r != nil && statusDrift(r, c.CodeStatus()) {
		var ind []string
		for _, in := range c.Independence {
			ind = append(ind, in.Key)
		}
		return assess.Decision{Folder: c.Folder, Kind: r.Kind, Status: c.CodeStatus(), Why: c.Activity.StatusWhy(), Description: r.Description, Independent: ind}, true, false
	}
	if c.Kind != "" || c.Name == nil || !strings.HasPrefix(c.Folder, "projects/") {
		return assess.Decision{}, false, false
	}
	name := path.Base(c.Folder)
	live := func(n string) string {
		w := "projects/" + n
		for i := 0; i < 8; i++ {
			r := recs(w)
			if r == nil {
				return ""
			}
			if r.SuppressedBy == "" {
				if !r.Live() || record.Broad(r.Kind) != "project" || notProject(r) || r.Quiet() {
					return ""
				}
				return w
			}
			w = r.SuppressedBy
		}
		return ""
	}
	// A linked worktree's or a separate clone's directory (git's metadata,
	// recorded while it existed), with nothing that gives it an identity of
	// its own: part of its repository.
	if len(c.Ancestors) > 0 && c.Ancestors[0].Certain() && len(c.Independence) == 0 && len(c.Same) == 0 {
		an := c.Ancestors[0]
		w := live(an.Name)
		if w == "" || w == c.Folder {
			return assess.Decision{}, false, w == "" && recs("projects/"+an.Name) == nil
		}
		kind := record.KindFolder
		if c.GitRoot || c.Absent() {
			kind = record.KindRepo
		}
		why := fmt.Sprintf("code: %s; nothing gives it an identity of its own (no own commits, no remote or forge repository of its own, no work in a folder of its name): part of %s", strings.Join(an.Why, "; "), path.Base(w))
		return assess.Decision{Folder: c.Folder, Kind: kind, Status: record.StatusDead, Why: why + a.factCite(c.Folder, ""),
			Description: fmt.Sprintf("A worktree or clone directory of %s; its facts are %s's.", path.Base(w), path.Base(w)), SuppressedBy: w}, true, false
	}
	if !c.Absent() {
		return assess.Decision{}, false, false
	}
	if to := c.Name.Renamed; to != "" {
		if a.checker.Repo(a.ctx, to).Verdict != check.Holds {
			return assess.Decision{}, false, false
		}
		w := live(to)
		if w == "" || w == c.Folder {
			return assess.Decision{}, false, w == "" && recs("projects/"+to) == nil
		}
		why := fmt.Sprintf("code: the forge says %s was renamed to %s (check: %s): an old name, folded into the current one", name, c.Name.Forge, strings.Join(c.Name.Lines(), " | "))
		return assess.Decision{Folder: c.Folder, Kind: record.KindRepo, Status: record.StatusDead, Why: why + a.factCite(c.Folder, ""),
			Description: fmt.Sprintf("The old name of %s on the forge (renamed to %s).", path.Base(w), c.Name.Forge), SuppressedBy: w}, true, false
	}
	pointed, n, nLive, example := a.factsPoint(c)
	// At least half of its live facts point, and all of them at one
	// repository: a fact or two that name another checkout's path in a big
	// folder of other things is no evidence of what the folder is.
	if len(pointed) != 1 || 2*n < nLive {
		return assess.Decision{}, false, false
	}
	var r string
	for k := range pointed {
		r = k
	}
	if a.checker.Repo(a.ctx, r).Verdict != check.Holds {
		return assess.Decision{}, false, false
	}
	w := live(r)
	if w == "" || w == c.Folder {
		return assess.Decision{}, false, w == "" && recs("projects/"+r) == nil
	}
	why := fmt.Sprintf("code: %s exists nowhere (check: %s); %d of its live facts point at %s by their own paths, forge URLs or sessions, and none at another repository", name, strings.Join(c.Name.Lines(), " | "), n, r)
	return assess.Decision{Folder: c.Folder, Kind: record.KindRepo, Status: record.StatusDead, Why: why + a.factCite(c.Folder, example),
		Description: fmt.Sprintf("%s exists nowhere: a gone or wrong name of %s, whose folder holds its facts.", name, path.Base(w)), SuppressedBy: w}, true, false
}

// factCite is the citation a code decision's why carries: the fact that
// points (note:<name>), else the newest session that filed the folder's
// notes; "" when the folder has none (no session ever touched it).
func (a *assessor) factCite(folder, example string) string {
	if example != "" {
		return " (e.g. note:" + example + ")"
	}
	best, day := "", ""
	for _, f := range a.facts {
		if f.Meta.Scope == folder && f.Meta.Source.Session != "" && (f.Meta.Created > day || f.Meta.Created == day && f.Meta.Source.Session < best) {
			best, day = f.Meta.Source.Session, f.Meta.Created
		}
	}
	if best == "" {
		return ""
	}
	return " (session:" + best + ")"
}

// factsPoint are the repositories the folder's live facts point at by their
// own evidence (see codeDecision), with how many facts point, how many are
// live, and one that points (the first by name).
func (a *assessor) factsPoint(c assess.Candidate) (map[string]bool, int, int, string) {
	if a.refiler == nil {
		a.refiler = newRefiler(a.home, a.catalog, a.recs)
	}
	a.refiler.recs = a.recs
	cache := a.repoCache()
	// The proposed repositories, for the relative paths: each one's checkout
	// here, else its forge repository (read at its default branch).
	roots := map[string]pathSource{}
	for _, p := range c.Proposed() {
		if r, ok := a.catalog.Repo(p.Name); ok && r.Root != "" {
			roots[p.Name] = pathSource{root: r.Root}
		} else if full := a.forgeFullOf(p.Name); full != "" {
			roots[p.Name] = pathSource{forge: full}
		}
	}
	var facts []*fact.Fact
	for _, f := range a.facts {
		if f.Meta.Scope == c.Folder && !f.InInbox() && !overlay.IsInbox(f.Path) && f.Meta.SupersededBy == "" && f.Meta.Retired == "" {
			facts = append(facts, f)
		}
	}
	sort.Slice(facts, func(i, j int) bool { return facts[i].Meta.Name < facts[j].Meta.Name })
	all := map[string]bool{}
	n, example := 0, ""
	for _, f := range facts {
		set := pointedRepos(cache, a.catalog, a.recs, f)
		for repo := range a.relPathRepos(f, roots) {
			set[repo] = true
		}
		if len(set) == 0 {
			set = a.refiler.sessions[f.Meta.Source.Session]
		}
		if len(set) == 0 {
			continue
		}
		n++
		if example == "" {
			example = f.Meta.Name
		}
		for k := range set {
			all[k] = true
		}
	}
	return all, n, len(facts), example
}

// pathSource is where a repository's paths are read: its checkout here,
// else its forge repository (owner/name).
type pathSource struct{ root, forge string }

// relPathRepos are the repositories among roots whose HEAD (a checkout's,
// else the forge's default branch) holds a repository-relative path f
// names (applies_to.paths, backticked paths: a file path with a folder in
// it, or a folder "<dir>/**" of five letters or more, never another glob),
// when two or more of them were checked and exactly one holds it.
func (a *assessor) relPathRepos(f *fact.Fact, roots map[string]pathSource) map[string]bool {
	out := map[string]bool{}
	if len(roots) == 0 {
		return out
	}
	var rels []string
	for _, p := range f.Meta.AppliesTo.Paths {
		rels = append(rels, p)
	}
	for _, ref := range check.Refs(f.Meta.Description + "\n" + f.Body) {
		if ref.Kind == check.KindPath {
			rels = append(rels, ref.Value)
		}
	}
	names := make([]string, 0, len(roots))
	for n := range roots {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, p := range rels {
		p = strings.Trim(strings.TrimPrefix(strings.TrimSpace(p), "./"), "/")
		if d, ok := strings.CutSuffix(p, "/**"); ok && len(d) >= 5 && !strings.ContainsAny(d, "*?[{~$/") {
			p = d // a folder of the repository
		} else if !strings.Contains(p, "/") || len(p) < 4 {
			continue
		}
		if strings.ContainsAny(p, "*?[{~$") || strings.HasPrefix(p, ".") {
			continue
		}
		var hold []string
		checked := 0
		for _, n := range names {
			src := roots[n]
			var r check.Result
			if src.root != "" {
				r = a.checker.Path(a.ctx, n, src.root, p)
			} else {
				r = a.checker.ForgePath(a.ctx, n, src.forge, p)
			}
			if r.Verdict != check.Unknown {
				checked++
			}
			if r.Verdict == check.Holds {
				hold = append(hold, n)
			}
		}
		if len(hold) == 1 && checked >= 2 {
			out[hold[0]] = true
		}
	}
	return out
}

// canonicalName is the name code makes canonical among the names one
// repository goes by (the candidate's name, the names code proved are the
// same repository, the forge's current name of a renamed one), in this
// order: the owner's decision that names one of them (a promoted decision
// or a ruling whose canonical claim the statements step read), else the
// main checkout's directory on the machines that have one (repoid), else
// the forge's current name. "" when none says; lines are the evidence.
func (a *assessor) canonicalName(c assess.Candidate) (string, []string) {
	name, ok := strings.CutPrefix(c.Folder, "projects/")
	if !ok || c.Kind != "" {
		return "", nil
	}
	group := []string{name}
	for _, r := range c.Same {
		group = append(group, strings.ToLower(r.Value))
	}
	if c.Name != nil && c.Name.Renamed != "" {
		group = append(group, c.Name.Renamed)
	}
	if a.decisions == nil {
		a.decisions = ownerDecisions(a.dir, a.facts, a.local, sameRepo(a.ctx, a.checker))
	}
	for _, n := range group {
		if d, ok := a.decisions[n]; ok {
			return d.Canonical, []string{fmt.Sprintf("the owner's decision [[%s]] (%s): %q", d.Fact, d.From, d.Words)}
		}
	}
	// The main checkout's directory, when every name of the group that a
	// catalog has is one directory's (a symlink's or the origin remote's
	// name); two main checkouts (clones with one origin) settle nothing.
	dirs := map[string]bool{}
	var where []string
	for _, n := range group {
		cn, w := a.checker.CheckoutName(n)
		if cn != "" {
			dirs[cn] = true
			where = append(where, w...)
		}
	}
	if len(dirs) == 1 {
		var cn string
		for d := range dirs {
			cn = d
		}
		return cn, append([]string{"the main checkout's directory is " + cn}, dedupeLines(where)...)
	}
	if len(dirs) > 1 {
		return "", nil
	}
	if c.Name != nil && c.Name.Renamed != "" {
		return c.Name.Renamed, []string{"the forge has it as " + c.Name.Forge + " now"}
	}
	return "", nil
}

func dedupeLines(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}
