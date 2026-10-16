package search

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestPromptCoverageAndIdentifierRarity(t *testing.T) {
	for _, df := range []int{1, 5, 6} {
		t.Run(fmt.Sprint(df), func(t *testing.T) {
			root := t.TempDir()
			for i := 0; i < df; i++ {
				write(t, root, fmt.Sprintf("system/symbol-%d.md", i), fmt.Sprintf("symbol-%d", i), "system", "tacit_cache cuda12", "detail", "")
			}
			write(t, root, "system/generic.md", "generic", "system", "research endpoint API", "", "")
			write(t, root, "system/body.md", "body", "system", "unrelated", "scheduler worker", "")
			t.Setenv("TACIT_INDEX", filepath.Join(t.TempDir(), "index"))
			t.Setenv("TACIT_LOCAL", filepath.Join(t.TempDir(), "local"))
			ix, err := Open(context.Background(), root, "coverage")
			if err != nil {
				t.Fatal(err)
			}
			defer ix.Close()
			for _, tc := range []struct {
				q     string
				want  int
				terms string
			}{
				{"tacit_cache", df, "tacit_cache"}, {"cuda12", df, "cuda12"},
				{"research", 0, ""}, {"research research", 0, ""}, {"API", 0, ""},
				{"endpoint API", 1, "endpoint,api"}, {"scheduler worker", 0, ""},
			} {
				want := tc.want
				if df > 5 && (tc.q == "tacit_cache" || tc.q == "cuda12") {
					want = 0
				}
				hits, err := ix.Query(context.Background(), Situation{Query: tc.q, PromptRelevance: true}, 20, false)
				if err != nil {
					t.Fatal(err)
				}
				if len(hits) != want {
					t.Errorf("%q df=%d: got %d want %d", tc.q, df, len(hits), want)
				}
				for _, h := range hits {
					if strings.Join(h.MatchedTerms, ",") != tc.terms {
						t.Errorf("%q matched %v", tc.q, h.MatchedTerms)
					}
				}
			}
			// Explicit queries still return a single generic match.
			hits, err := ix.Query(context.Background(), Situation{Query: "research"}, 20, false)
			if err != nil || len(hits) != 1 {
				t.Fatalf("explicit query: %v %v", hits, err)
			}
		})
	}
}

func TestPromptDoesNotJoinIncidentalSummaryWords(t *testing.T) {
	root := t.TempDir()
	write(t, root, "system/company.md", "research-tool-company", "system", "Company pages are unreachable through the research tool; build company facts from official sources.", "", "")
	write(t, root, "system/kernel.md", "new-kernel", "system", "Driver API drift breaks builds. Refresh the source tree and add the new entry.", "", "")
	write(t, root, "system/ingestion.md", "ingestion-facts", "system", "research facts ingestion", "", "")
	write(t, root, "system/research.md", "research-notes", "system", "research facts", "", "")
	write(t, root, "system/endpoint.md", "endpoint-api", "system", "add a new endpoint to the api", "", "")
	write(t, root, "system/changes.md", "changes", "system", "add new API clients", "", "")
	t.Setenv("TACIT_INDEX", filepath.Join(t.TempDir(), "index"))
	t.Setenv("TACIT_LOCAL", filepath.Join(t.TempDir(), "local"))
	ix, err := Open(context.Background(), root, "incidental")
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	for _, q := range []string{"research facts ingestion improvements", "add a new endpoint to the api"} {
		hits, err := ix.Query(context.Background(), Situation{Query: q, PromptRelevance: true}, 10, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) == 0 {
			t.Errorf("lost coherent match for %q", q)
		}
		for _, h := range hits {
			if h.Name == "research-tool-company" || h.Name == "new-kernel" {
				t.Errorf("%q leaked %s", q, h.Name)
			}
		}
	}
}

func TestCoverageGateBeforeCandidateLimit(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 20; i++ {
		write(t, root, fmt.Sprintf("system/noise-%d.md", i), fmt.Sprintf("endpoint-noise-%d", i), "system", strings.Repeat("endpoint ", 10), "", "")
	}
	write(t, root, "system/valid.md", "valid", "system", "endpoint api", strings.Repeat("padding ", 100), "")
	t.Setenv("TACIT_INDEX", filepath.Join(t.TempDir(), "index"))
	t.Setenv("TACIT_LOCAL", filepath.Join(t.TempDir(), "local"))
	ix, err := Open(context.Background(), root, "limit")
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	hits, err := ix.Query(context.Background(), Situation{Query: "endpoint api", PromptRelevance: true}, 1, false)
	if err != nil || len(hits) != 1 || hits[0].Name != "valid" {
		t.Fatalf("eligible hit starved: %v %v", hits, err)
	}
}
