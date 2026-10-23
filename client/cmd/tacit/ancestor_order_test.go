package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/assess"
	"github.com/agent-sh/tacit/client/internal/curate"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/record"
	"github.com/agent-sh/tacit/client/internal/store"
)

var (
	firstAncRx  = regexp.MustCompile(`(?m)^ancestor candidates [^\n]*?: 1\. (projects/[a-z0-9-]+) \[`)
	refusedToRx = regexp.MustCompile(`set suppressed_by to (projects/[a-z0-9-]+)`)
)

// orderModel answers from the folder's block alone, the same block the same
// answer: a folder with ancestor candidates folds into the first; one
// marked with no evidence is answered active with nothing cited (code's
// outcome must replace it); a name that exists nowhere is left for review;
// any other stands alone. Sent back, it folds where code says, or leaves the
// folder for review.
func orderModel(_ context.Context, _, prompt string) (string, float64, error) {
	blocks := map[string]string{}
	var order []string
	for _, b := range blockRx.FindAllStringSubmatch(prompt, -1) {
		blocks[b[1]] = b[2]
		order = append(order, b[1])
	}
	decide := func(folder string) assess.Decision {
		body := blocks[folder]
		cite := citeInRx.FindString(body)
		status := "active"
		if cite == "" {
			status = "dormant"
		}
		kind := "folder"
		if strings.Contains(body, "git_root true") {
			kind = "repo"
		}
		d := assess.Decision{Folder: folder, Kind: kind, Status: status, Description: "what " + folder + " is", Why: "its evidence, " + cite}
		switch {
		case firstAncRx.MatchString(body):
			d.SuppressedBy = firstAncRx.FindStringSubmatch(body)[1]
		case strings.Contains(body, "evidence of what it is: none"):
			d.Status, d.Why = "active", "worked on lately"
		case strings.Contains(body, "as a repository: fails"):
			d.Status = "needs-review"
		}
		return d
	}
	var out []assess.Decision
	if i := strings.Index(prompt, "CODE REFUSED THESE DECISIONS"); i >= 0 {
		for _, l := range folderLRx.FindAllStringSubmatch(prompt[i:], -1) {
			d := decide(l[1])
			d.SuppressedBy, d.Status = "", "needs-review"
			if f := refusedToRx.FindStringSubmatch(l[2]); f != nil {
				d.SuppressedBy, d.Status = f[1], "dormant"
			}
			out = append(out, d)
		}
	} else {
		for _, f := range order {
			out = append(out, decide(f))
		}
	}
	b, _ := json.Marshal(out)
	return string(b), 0.01, nil
}

// orderFixture is checkedHome's store with folders whose ancestors only the
// whole graph finds: a chain (aaa-chain inside bbb-mid, bbb-mid inside
// widget: aaa-chain is widget's), a folder the store talks about in another
// folder with no record yet (alpha-note, written about in zeta-base, whose
// name exists nowhere), a cycle (cyc-one and cyc-two, each inside the
// other), and a folder nothing says anything of (lonely-x).
func orderFixture(t *testing.T) *store.Store {
	home, _, _ := checkedHome(t)
	s := &store.Store{Dir: filepath.Join(home, "store")}
	add := func(scope, name, session, desc string) {
		writeFactFile(t, s.Dir, &fact.Fact{Path: scope + "/" + name + ".md", Meta: fact.Meta{Name: name, Description: desc, Type: "history", Scope: scope,
			Source: fact.Source{Harness: "curator", Host: "box", Session: session, Kind: "observed"}, Created: "2026-10-12"}, Body: desc + ".\n"})
	}
	add("projects/aaa-chain", "aaa-runner", "s-a1", "The chain task runs from ~/projects/bbb-mid/aaa-chain/run.sh")
	add("projects/bbb-mid", "bbb-layer", "s-b1", "The mid layer lives at ~/projects/widget/bbb-mid/mid.py")
	add("projects/alpha-note", "alpha-plan", "s-n2", "The plan keeps one page per milestone")
	for i := 1; i <= 4; i++ {
		add("projects/zeta-base", fmt.Sprintf("zeta-%d", i), "s-zb", fmt.Sprintf("Step %d of the base run is checked against alpha-note's numbers", i))
	}
	add("projects/cyc-one", "cyc-one-data", "s-c1", "Its data sits under ~/projects/cyc-two/cyc-one/data")
	add("projects/cyc-two", "cyc-two-data", "s-c2", "Its data sits under ~/projects/cyc-one/cyc-two/data")
	add("projects/lonely-x", "lonely-colour", "", "The theme colour is teal")
	facts, _, _ := fact.Walk(s.Dir)
	writeIndexesForTest(t, s.Dir, facts)
	gitIn(t, s.Dir, "add", "-A")
	gitIn(t, s.Dir, "commit", "-q", "-m", "ancestors only the graph finds")
	return s
}

// outcome is what a run decided: each folder's kind, status and live winner.
func outcome(t *testing.T, s *store.Store) map[string]string {
	recs, errs := record.Walk(s.Dir)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	out := map[string]string{}
	for f, r := range recs {
		w, _, _ := record.Resolve(recs, f)
		out[f] = r.Kind + " " + r.Status + " -> " + w
		if strings.HasPrefix(r.Was[0].Why, "code: no ancestor candidate") {
			out[f] += " (code: no evidence)"
		}
	}
	return out
}

// grouping is the first run's candidates as code groups them.
func grouping(t *testing.T, s *store.Store) map[string]string {
	a, err := newAssessor(context.Background(), s, false)
	if err != nil {
		t.Fatal(err)
	}
	a.held = loadHeld(filepath.Join(t.TempDir(), "held.json"))
	out := map[string]string{}
	for _, c := range a.candidates(1000, nil) {
		anc := ""
		if len(c.Ancestors) > 0 {
			anc = c.Ancestors[0].Folder
		}
		out[c.Folder] = fmt.Sprintf("group=%s ancestor=%s depth=%d none=%v", c.Group, anc, c.Depth, c.NoEvidence)
	}
	return out
}

func diffMaps(a, b map[string]string) []string {
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	var out []string
	for _, k := range sortedKeysBool(keys) {
		if a[k] != b[k] {
			out = append(out, fmt.Sprintf("%s: %q vs %q", k, a[k], b[k]))
		}
	}
	return out
}

// The same store gives the same ancestors, groups and decisions whatever
// the order the facts are read in and however many folders a run takes (a
// folder decided before or after its ancestor has a record), from a model
// that answers each folder from what it is shown: the graph is computed
// from all the evidence before any decision and again after each batch.
func TestAncestorGraphIsOrderIndependent(t *testing.T) {
	prev := assessRun
	assessRun = orderModel
	t.Cleanup(func() { assessRun = prev; assessFactOrder = nil })
	type variant struct {
		seed int64
		max  int
	}
	var base, baseGroups map[string]string
	for i, v := range []variant{{0, 60}, {1, 60}, {2, 60}, {3, 2}, {4, 1}, {5, 3}} {
		s := orderFixture(t)
		if v.seed == 0 {
			assessFactOrder = func(fs []*fact.Fact) {
				sort.SliceStable(fs, func(i, j int) bool { return fs[i].Path > fs[j].Path })
			}
		} else {
			r := rand.New(rand.NewSource(v.seed))
			assessFactOrder = func(fs []*fact.Fact) { r.Shuffle(len(fs), func(i, j int) { fs[i], fs[j] = fs[j], fs[i] }) }
		}
		groups := grouping(t, s)
		for run := 0; run < 60; run++ {
			rep := &curate.Report{}
			if errs := assessStep(context.Background(), s, assessOpts{model: "opus", max: v.max}, rep); errs != 0 {
				t.Fatalf("variant %v run %d: %d errors", v, run, errs)
			}
			if rep.AssessCandidates == 0 {
				break
			}
		}
		got := outcome(t, s)
		if i == 0 {
			base, baseGroups = got, groups
			for f, want := range map[string]string{
				"projects/aaa-chain":  "-> projects/widget",
				"projects/bbb-mid":    "-> projects/widget",
				"projects/alpha-note": "-> projects/zeta-base",
				"projects/cyc-two":    "-> projects/cyc-one",
				"projects/cyc-one":    "needs-review -> projects/cyc-one",
				"projects/lonely-x":   "folder needs-review -> projects/lonely-x (code: no evidence)",
			} {
				if !strings.Contains(got[f], want) {
					t.Errorf("%s: %q, want %q", f, got[f], want)
				}
			}
			for f, want := range map[string]string{
				"projects/aaa-chain":  "ancestor=projects/widget depth=1",
				"projects/bbb-mid":    "ancestor=projects/widget depth=1",
				"projects/alpha-note": "ancestor=projects/zeta-base depth=1",
				"projects/cyc-two":    "ancestor=projects/cyc-one depth=1",
				"projects/cyc-one":    "ancestor= depth=0",
				"projects/lonely-x":   "none=true",
			} {
				if !strings.Contains(groups[f], want) {
					t.Errorf("grouping %s: %q, want %q", f, groups[f], want)
				}
			}
			continue
		}
		if d := diffMaps(baseGroups, groups); len(d) > 0 {
			t.Errorf("variant %v: the grouping differs:\n%s", v, strings.Join(d, "\n"))
		}
		if d := diffMaps(base, got); len(d) > 0 {
			t.Errorf("variant %v: the decisions differ:\n%s", v, strings.Join(d, "\n"))
		}
	}
}
