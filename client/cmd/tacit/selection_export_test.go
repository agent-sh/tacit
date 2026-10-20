package main

// Offline experiment transport. Compiled with go test -c, never linked into tacit.
// Reuses the actual hook predicates, alias resolution and selection implementation.
import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/search"
	"github.com/agent-sh/tacit/client/internal/store"
)

type selectionRequest struct {
	ID       string   `json:"id"`
	Query    string   `json:"query"`
	Original string   `json:"original"`
	Cwd      string   `json:"cwd"`
	Ceiling  int      `json:"ceiling"`
	Seen     []string `json:"seen"`
}
type selectionCandidate struct {
	search.Hit
	ID        string   `json:"id"`
	OriginIDs []string `json:"origin_ids"`
}
type selectionResponse struct {
	ID           string               `json:"id"`
	Original     string               `json:"original"`
	Query        string               `json:"query"`
	Repo         string               `json:"repo"`
	RepoAliases  []string             `json:"repo_aliases"`
	Ceiling      int                  `json:"ceiling"`
	Candidates   []selectionCandidate `json:"candidates"`
	Derived      []selectionCandidate `json:"derived"`
	Control      []search.Hit         `json:"control"`
	DerivedError string               `json:"derived_error,omitempty"`
}

func selectionExport(ctx context.Context, ix *search.Index, sit search.Situation, req selectionRequest, root string) (selectionResponse, error) {
	out := selectionResponse{ID: req.ID, Original: req.Original, Query: req.Query, Repo: sit.Repo, RepoAliases: sit.RepoAliases, Ceiling: req.Ceiling, Candidates: []selectionCandidate{}, Derived: []selectionCandidate{}}
	p := strings.TrimSpace(req.Query)
	if !promptWorthy(p) {
		return out, nil
	}
	// Query owns explicit repo routing; Paths supplies retrieval-only context.
	// History must not grant a new repo merely by naming it.
	sit.Query = strings.TrimSpace(req.Original)
	if req.Query != req.Original {
		if !strings.HasPrefix(req.Query, req.Original+"\n") {
			return out, fmt.Errorf("context must preserve original prefix")
		}
		sit.Paths = []string{strings.TrimPrefix(req.Query, req.Original+"\n")}
	}
	sit.RestrictRepos, sit.PromptRelevance = true, true
	sit.Derived, sit.OnlyDerived = false, false
	hits, err := ix.Query(ctx, sit, req.Ceiling, false)
	if err != nil {
		return out, err
	}
	sit.OnlyDerived, sit.Printable = true, true
	derived, err := ix.Query(ctx, sit, 1, false)
	if err != nil {
		out.DerivedError = err.Error()
		derived = nil
	}
	wrap := func(hits []search.Hit) ([]selectionCandidate, error) {
		result := []selectionCandidate{}
		for _, h := range hits {
			origins := []string{}
			raw, e := os.ReadFile(filepath.Join(root, h.Path))
			if e != nil {
				return nil, e
			}
			f, e := fact.Parse(h.Path, raw)
			if e != nil {
				return nil, e
			}
			if f.Meta.Origin != "" {
				origins = append(origins, f.Meta.Origin)
			}
			result = append(result, selectionCandidate{Hit: h, ID: h.Path, OriginIDs: origins})
		}
		return result, nil
	}
	out.Candidates, err = wrap(hits)
	if err != nil {
		return out, err
	}
	out.Derived, err = wrap(derived)
	if err != nil {
		return out, err
	}
	seen := map[string]bool{}
	for _, n := range req.Seen {
		seen[n] = true
	}
	// Call the original local selector, not utilityPromptSelection or promptHits:
	// hosted utility may be globally enabled in the caller's shell. Keep the
	// unchanged six-candidate control even for wider pools.
	querySit := sit
	querySit.OnlyDerived, querySit.Printable = false, false
	originalHits, err := ix.Query(ctx, querySit, promptLimit*2, false)
	if err != nil {
		return out, err
	}
	floor := promptFloor
	if sit.Repo == "" {
		floor = repoLessPromptFloor
	}
	out.Control = promptSelection(originalHits, derived, seen, floor)
	return out, nil
}

func TestSelectionExport(t *testing.T) {
	requestPath := os.Getenv("TACIT_SELECTION_REQUEST")
	if requestPath == "" {
		t.Skip("offline transport only")
	}
	selectionOfflineEnv(t)
	// Never fall back to an operator's live store, index or local overlay.
	for _, key := range []string{"TACIT_STORE", "TACIT_INDEX", "TACIT_LOCAL"} {
		if !filepath.IsAbs(os.Getenv(key)) {
			t.Fatalf("%s must explicitly name an absolute offline path", key)
		}
	}
	raw, err := os.ReadFile(requestPath)
	if err != nil {
		t.Fatal(err)
	}
	var req selectionRequest
	if err = json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}
	if req.Ceiling < 1 || req.Ceiling > 128 {
		t.Fatal("ceiling must be 1..128")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := store.Default()
	if err != nil {
		t.Fatal(err)
	}
	ix, err := openIndex(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	sit := search.Situation{Repo: store.RepoName(ctx, req.Cwd), Host: store.ShortHost()}
	if sit.Repo != "" {
		sit.RepoAliases = hookRepoAliases(req.Cwd)
	}
	out, err := selectionExport(ctx, ix, sit, req, s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(os.Getenv("TACIT_SELECTION_RESPONSE"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

// No main or settings.Load. Isolate HOME so helpers cannot read live config.
func selectionOfflineEnv(t *testing.T) {
	t.Helper()
	t.Setenv("TACIT_UTILITY_PROVIDER", "disabled")
	t.Setenv("TACIT_UTILITY_KEY_FILE", "")
	t.Setenv("TACIT_UTILITY_AUDIT_DIR", "")
	t.Setenv("TYPESAFE_API_KEY", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("TACIT_SETTINGS", filepath.Join(home, "offline.yaml"))
	t.Setenv("TACIT_CONFIG", filepath.Join(home, "keys"))
}
