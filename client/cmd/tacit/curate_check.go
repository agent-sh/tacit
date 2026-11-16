package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/agent-sh/tacit/client/internal/curate"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/overlay"
	"github.com/agent-sh/tacit/client/internal/repoid"
	"github.com/agent-sh/tacit/client/internal/store"
	"github.com/agent-sh/tacit/client/internal/validate"
)

// curateEnv is what the curator's metadata checks know (internal/validate):
// this machine's checkouts and the store's folders and writers; layer adds
// the local layer's curated folders when it curates the overlay.
func curateEnv(storeDir string, layer bool) validate.Env {
	home, _ := os.UserHomeDir()
	dir := ""
	if layer {
		dir = filepath.Join(overlay.Dir(), filepath.FromSlash(overlay.CuratedDir))
	}
	return validate.NewEnv(storeDir, dir, knownCatalog(home, storeDir, layer), store.ShortHost())
}

// knownCatalog is every repository the checks may name: this machine's
// checkouts (machineCatalog), every machine's catalog in the store
// (repoid.CatalogFile in inbox/*/), and with the local layer this machine's
// catalog in the overlay.
func knownCatalog(home, storeDir string, layer bool) *repoid.Catalog {
	c := machineCatalog(home)
	c.AddStoreCatalogs(storeDir)
	if layer {
		c.AddSharedFile(filepath.Join(overlay.Dir(), overlay.InboxDir, "dreamer@"+store.ShortHost(), repoid.CatalogFile))
	}
	return c
}

// entryCheck checks a split or rulings answer's entries as they would be
// written: the fact curate.ToFact makes of each, with the ruling evidence
// applied (an entry filed as observed may not say the owner ruled), against
// env. An entry whose origin is no note of the batch is refused too. With no
// owner history on this machine a kind user entry cannot be proved here and
// its batch waits (the caller's unbacked-ruling rule), so its wording is not
// sent back to the model.
func entryCheck(env validate.Env, host string, batch []*fact.Fact, noteByName map[string]*fact.Fact, where curate.Where, noOwnerCorpus bool) curate.Check {
	origins := map[string]bool{}
	for _, n := range batch {
		origins[n.Meta.Name] = true
	}
	return func(entries []curate.Entry) []curate.Problem {
		var out []curate.Problem
		taken := map[string]bool{}
		for i, e := range entries {
			if !origins[e.Origin] {
				out = append(out, curate.Problem{Entry: i + 1, Field: "origin", Rule: "names no note of this batch", Hint: fmt.Sprintf("got %q; use the name= of the note the fact came from", e.Origin)})
				continue
			}
			f := curate.ToFact(e, host, taken)
			env.RedirectFact(f) // a suppressed folder's entries go to its winner
			env.RepoPaths(&f.Meta)
			_, ruling := curate.ApplyEvidence(f, e, noteByName[e.Origin], where)
			if !ruling && e.Kind == "user" && noOwnerCorpus {
				ruling = true // not judged here: the batch waits for the owner's history
			}
			for _, p := range env.Meta(f.Meta, f.Body, false, ruling) {
				out = append(out, curate.Problem{Entry: i + 1, Field: p.Field, Rule: p.Rule, Hint: entryHint(e, p)})
			}
		}
		return out
	}
}

// entryRepair repairs a split or rulings answer's entries before entryCheck
// sees them (curate.Repair; SPEC.md, Metadata checks), so a fault code can
// fix without a guess never pays for a second model call:
//
//   - a projects/<repo> scope that names another name of a checkout takes the
//     repository's own name; one that names no known repository takes the
//     project scope of the note the entry came from, when that one is known;
//   - applies_to.repos: each value is read as a known repository's name
//     (lower case, the repo of owner/repo, a forge URL or a path, ".git"
//     cut: validate.RepoName) and a value that names none is dropped, unless
//     that would leave a fact outside a project, machine or org scope with no
//     repository at all, which would widen it to every repository: that is
//     the model's to fix;
//   - applies_to.paths: what stays absolute after RepoPaths (a path outside
//     this machine's checkout of the fact's repository) is dropped, on a fact
//     that is not a machine's;
//   - a label that opens a sentence and claims the owner's authority ("Owner
//     ruling: ...") is cut from an entry that is not their verified words,
//     when that leaves no such claim; in the rulings pass (rulings), an entry
//     still claiming it is dropped: the pass lists only rulings code can
//     verify, and the split has filed the note's content already.
//
// What is left goes to entryCheck and, refused, back to the model.
func entryRepair(env validate.Env, host string, batch []*fact.Fact, noteByName map[string]*fact.Fact, where curate.Where, noOwnerCorpus, rulings bool) curate.Repair {
	origins := map[string]bool{}
	for _, n := range batch {
		origins[n.Meta.Name] = true
	}
	return func(entries []curate.Entry, keep []bool) []curate.Repaired {
		var out []curate.Repaired
		for i := range entries {
			e := &entries[i]
			if rulings {
				e.Kind = "user" // the rulings pass's entries are kind user (curate.go)
			}
			if !origins[e.Origin] {
				continue
			}
			fixed := func(field, action string) {
				out = append(out, curate.Repaired{Entry: i + 1, Field: field, Action: action})
			}
			if rest, ok := strings.CutPrefix(e.Scope, "projects/"); ok && env.Scope(e.Scope) != nil {
				if own := env.Catalog.AliasOf(rest); own != "" && env.Scope("projects/"+own) == nil {
					e.Scope = "projects/" + own
					fixed("scope", "another name of a repository, made its own name")
				} else if n := noteByName[e.Origin]; n != nil && env.UnknownRepo(e.Scope) {
					if ns := strings.ToLower(n.Meta.Scope); strings.HasPrefix(ns, "projects/") && ns != e.Scope && env.Scope(ns) == nil {
						e.Scope = ns
						fixed("scope", "names no known repository, made the note's own project")
					}
				}
			}
			if vs := strings2(e.AppliesTo["repos"]); len(vs) > 0 {
				repos, changed, dropped := env.RepairRepos(vs)
				bounded := strings.HasPrefix(e.Scope, "projects/") || strings.HasPrefix(e.Scope, "machines/") || strings.HasPrefix(e.Scope, "orgs/")
				if changed && (len(repos) > 0 || bounded) {
					e.AppliesTo["repos"] = anys(repos)
					switch {
					case dropped > 0:
						fixed("applies_to.repos", fmt.Sprintf("dropped %d value(s) naming no known repository", dropped))
					default:
						fixed("applies_to.repos", "read as known repository names")
					}
				}
			}
			f := curate.ToFact(*e, host, map[string]bool{})
			env.RedirectFact(f)
			env.RepoPaths(&f.Meta)
			if !strings.HasPrefix(f.Meta.Scope, "machines/") && len(f.Meta.AppliesTo.Paths) > 0 {
				var paths []string
				for _, p := range f.Meta.AppliesTo.Paths {
					if !validate.AbsolutePath(p) {
						paths = append(paths, p)
					}
				}
				if d := len(f.Meta.AppliesTo.Paths) - len(paths); d > 0 {
					e.AppliesTo["paths"] = anys(paths)
					f.Meta.AppliesTo.Paths = paths
					fixed("applies_to.paths", fmt.Sprintf("dropped %d absolute path(s) outside the repository's checkout", d))
				}
			}
			if e.Kind == curate.KindStatement {
				continue // their words verbatim: never rewritten by code
			}
			if !claimsUnverified(f, *e, noteByName[e.Origin], where, noOwnerCorpus) {
				continue
			}
			s := *e
			s.Description, s.Body = validate.StripOwnerLabels(e.Description), validate.StripOwnerLabels(e.Body)
			sf := curate.ToFact(s, host, map[string]bool{})
			env.RedirectFact(sf)
			// A cut that empties a required field is no repair: the entry is
			// left to the drop or the send-back below.
			emptied := strings.TrimSpace(s.Description) == "" || strings.TrimSpace(s.Body) == ""
			if !emptied && (s.Description != e.Description || s.Body != e.Body) && !claimsUnverified(sf, s, noteByName[e.Origin], where, noOwnerCorpus) {
				e.Description, e.Body = s.Description, s.Body
				fixed("description", "cut a label claiming the owner's authority")
				continue
			}
			if rulings {
				keep[i] = false
				fixed("entry", "dropped: claims the owner's authority and code found no words of theirs behind it")
			}
		}
		return out
	}
}

// claimsUnverified reports a fact (as entryCheck sees it) that claims the
// owner's authority while it is not their verified words.
func claimsUnverified(f *fact.Fact, e curate.Entry, note *fact.Fact, where curate.Where, noOwnerCorpus bool) bool {
	_, ruling := curate.ApplyEvidence(f, e, note, where)
	if !ruling && e.Kind == "user" && noOwnerCorpus {
		ruling = true // not judged here: the batch waits for the owner's history
	}
	return validate.Attribution(f.Meta.Description, f.Body, ruling) != nil
}

func strings2(xs []any) []string {
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func anys(xs []string) []any {
	out := make([]any, len(xs))
	for i, x := range xs {
		out[i] = x
	}
	return out
}

func entryHint(e curate.Entry, p validate.Problem) string {
	h := fmt.Sprintf("entry %q", e.Name)
	if p.Hint != "" {
		h += ": " + p.Hint
	}
	return h
}

// logRepaired counts and logs the fixes entryRepair made to an answer
// (entry, field and what code did; never a value).
func logRepaired(rep *curate.Report, pass string, repaired []curate.Repaired) {
	rep.Repaired += len(repaired)
	for _, r := range repaired {
		rep.RepairedEntries = append(rep.RepairedEntries, pass+": "+r.String())
	}
}

// splitChecked is one model pass of the curator (split or rulings) through
// curate.SplitChecked: the first answer's refusal is logged (field and rule,
// or for an answer refused whole the parser's reason; never the text) and
// counted as a retry; the result is the second answer's when there was one.
func splitChecked(ctx context.Context, rep *curate.Report, pass string, run curate.Runner, model, prompt string, repair curate.Repair, check curate.Check) ([]curate.Entry, error) {
	// The rulings pass sends back an answer refused whole too (no JSON array);
	// the split pass, whose answers are long, splits its batch for one.
	entries, cost, repaired, first, err := curate.SplitChecked(ctx, run, model, prompt, repair, check, pass == "rulings")
	rep.CostUSD += cost
	logRepaired(rep, pass, repaired)
	if first != nil {
		rep.Retries++
		logInvalid(rep, pass+" (sent back)", first)
	}
	return entries, err
}
