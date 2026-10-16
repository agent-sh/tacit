package search

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"testing"
)

func TestEvidenceStatisticsLocationsAndGuards(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 20; i++ {
		write(t, root, fmt.Sprintf("system/padding-%d.md", i), fmt.Sprintf("padding-%d", i), "system", "ordinary unrelated guidance", "", "")
	}
	write(t, root, "machines/host-a/relay-unit.md", "relay-unit", "machines/host-a", "Restart the aster relay service.", "See [[manual]] and [[missing]] and [[retired]].", "applies_to:\n  repos: [capture-repo]\n  paths: [/verified/relay.conf]\n  commands: [systemctl --user restart aster-relay]\n")
	write(t, root, "system/protocol.md", "protocol", "system", "The aster relay protocol is documented.", "", "")
	write(t, root, "system/manual.md", "manual", "system", "Installation manual.", "", "applies_to:\n  paths: [/verified/manual.md]\n")
	write(t, root, "system/retired.md", "retired", "system", "The aster relay service.", "", "retired: replaced\n")
	write(t, root, "derived/capture-repo/map.md", "map", "derived/capture-repo", "The aster relay service.", "", "")
	write(t, root, "inbox/claude@host-a/inbox.md", "inbox", "system", "The aster relay service.", "", "")
	write(t, root, "system/body-only.md", "body-only", "system", "Unrelated sentence.", "aster relay service", "")
	write(t, root, "machines/host-b/foreign.md", "foreign", "machines/host-b", "Aster relay service restart.", "", "")
	t.Setenv("TACIT_INDEX", filepath.Join(t.TempDir(), "index"))
	t.Setenv("TACIT_LOCAL", filepath.Join(t.TempDir(), "local"))
	ix, err := Open(context.Background(), root, "evidence-guards")
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	q := "restart the aster relay service"
	ev, _, err := ix.promptEvidence(context.Background(), Terms(q), q)
	if err != nil {
		t.Fatal(err)
	}
	e := ev["machines/host-a/relay-unit.md"]
	if e == nil || !e.Supported || !e.LocationBound || e.Coverage != 1 {
		t.Fatalf("missing evidence: %+v", e)
	}
	if e.CorpusDocuments != 25 {
		t.Fatalf("inbox/derived/retired contaminated corpus: %d", e.CorpusDocuments)
	}
	for _, term := range e.Terms {
		if term.Term == "aster" && (term.Documents != 3 || math.Abs(term.IDF-math.Log(26.0/4)) > 1e-12) {
			t.Fatalf("wrong IDF: %+v", term)
		}
	}
	if len(e.Locations) != 1 || e.Locations[0].AppliesTo.Paths[0] != "/verified/relay.conf" {
		t.Fatalf("unverified or lost direct location: %+v", e.Locations)
	}
	if ev["system/body-only.md"] != nil {
		t.Fatal("body-only terms became summary evidence")
	}
	for _, repo := range []string{"", "other-repo"} {
		hits, err := ix.Query(context.Background(), Situation{Query: q, Host: "host-a", Repo: repo, RestrictRepos: true, PromptRelevance: true}, 50, false)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, h := range hits {
			if h.Name == "relay-unit" {
				found = true
				if h.Evidence == nil || len(h.Evidence.Locations) != 2 || h.Evidence.Locations[1].Fact != "manual" {
					t.Fatalf("selected link not hydrated: %+v", h.Evidence)
				}
			}
			if h.Name == "retired" || h.Name == "map" || h.Name == "inbox" || h.Name == "foreign" {
				t.Fatalf("guard bypass: %s", h.Name)
			}
		}
		if found != (repo == "") {
			t.Fatalf("repo=%q found=%v", repo, found)
		}
	}
}

func TestEvidenceDoesNotInventRelationships(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 12; i++ {
		write(t, root, fmt.Sprintf("system/common-%d.md", i), fmt.Sprintf("common-%d", i), "system", "common operation ordinary guidance", "", "")
	}
	write(t, root, "system/unique-pair.md", "unique-pair", "system", "unique pair appears once", "", "")
	t.Setenv("TACIT_INDEX", filepath.Join(t.TempDir(), "index"))
	t.Setenv("TACIT_LOCAL", filepath.Join(t.TempDir(), "local"))
	ix, err := Open(context.Background(), root, "no-invented-edges")
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	for _, q := range []string{"unique pair", "common operation"} {
		ev, ids, err := ix.promptEvidence(context.Background(), Terms(q), q)
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) != 0 {
			t.Fatalf("unsupported association %q approved: %v", q, ids)
		}
		for _, e := range ev {
			if len(e.Relations) > 0 {
				t.Fatalf("unsupported relation: %+v", e)
			}
		}
	}
	if evidencePairs("add a new")["add\x00new"] || evidencePairs("add the new")["add\x00new"] {
		t.Fatal("stop-word removal manufactured a phrase")
	}
}
