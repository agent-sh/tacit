package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

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

func entryHint(e curate.Entry, p validate.Problem) string {
	h := fmt.Sprintf("entry %q", e.Name)
	if p.Hint != "" {
		h += ": " + p.Hint
	}
	return h
}

// splitChecked is one model pass of the curator (split or rulings) through
// curate.SplitChecked: the first answer's refusal is logged (field and rule,
// or for an answer refused whole the parser's reason; never the text) and
// counted as a retry; the result is the second answer's when there was one.
func splitChecked(ctx context.Context, rep *curate.Report, pass string, run curate.Runner, model, prompt string, check curate.Check) ([]curate.Entry, error) {
	// The rulings pass sends back an answer refused whole too (no JSON array);
	// the split pass, whose answers are long, splits its batch for one.
	entries, cost, first, err := curate.SplitChecked(ctx, run, model, prompt, check, pass == "rulings")
	rep.CostUSD += cost
	if first != nil {
		rep.Retries++
		logInvalid(rep, pass+" (sent back)", first)
	}
	return entries, err
}
