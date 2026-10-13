package gitev

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/fact"
)

func sday(s string) time.Time { t, _ := time.Parse("2006-01-02", s); return t }

// The owner's page reads git as a few sentences: what is gone, how much
// changed per repo since the facts were written and the latest change, which
// repos were quiet or unreadable.
func TestSummarizeSaysWhatGitShowsInPlainWords(t *testing.T) {
	now := sday("2026-10-11")
	ev := []RepoEvidence{
		{Name: "api", Head: "5a20f4b", Matched: 0, State: []TermState{{Term: "**/PATCHES.md", Present: true}}},
		{Name: "webapp", Head: "3c1d9e7", Matched: 60, Capped: true, State: []TermState{{Term: "**/PATCHES.md", Present: true}, {Term: "rename_worker.rs", Kind: "path"}},
			Commits: []Commit{{At: sday("2026-09-14"), Subject: "v2: rename the flag"}, {At: sday("2026-09-22"), Subject: "Speed up the long upload path (#118)."}}},
		{Name: "atlas", Notes: []string{"not a git repo"}},
	}
	got := summarize(ev, []string{"webapp", "api", "atlas"}, sday("2026-09-14"), map[string]bool{"**/PATCHES.md": true}, false, false, now)
	want := []string{
		"rename_worker.rs is gone from webapp.",
		"In webapp, 60+ commits since 14 Sep changed what these facts name, the latest on 22 Sep: “Speed up the long upload path (#118)”.",
		"In api, nothing they name changed since 14 Sep.",
		"atlas could not be read.",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("summary:\n%q\nwant\n%q", got, want)
	}
	one := summarize([]RepoEvidence{{Name: "x", Head: "abc", Matched: 1, State: []TermState{{Term: "a", Present: true}}, Commits: []Commit{{At: sday("2025-12-01"), Subject: "drop it"}}}},
		[]string{"x"}, sday("2025-11-30"), map[string]bool{"a": true}, true, true, now)
	if want := []string{"In x, 1 commit since 30 Nov 2025 changed what this fact names, the latest on 1 Dec 2025: “drop it”.", "Part of git could not be read, so this may be missing something."}; !reflect.DeepEqual(one, want) {
		t.Errorf("one fact:\n%q\nwant\n%q", one, want)
	}
	if got := summarize(nil, nil, time.Time{}, nil, true, false, now); len(got) != 1 || got[0] != "This fact names no repo, so git has nothing to add." {
		t.Errorf("no repo: %q", got)
	}
}

// A built timeline carries the summary.
func TestBuildOneCarriesASummary(t *testing.T) {
	root := t.TempDir()
	repo := newRepo(t, root, "x")
	commit(t, repo, "2026-10-06", "Dev", "Dev", "retry five times", map[string]string{"src/engine.rs": "const MAX_RETRIES: u32 = 5;\n"})
	f := &fact.Fact{Path: "projects/x/e.md", Meta: fact.Meta{Name: "e", Scope: "projects/x", Created: "2026-10-01", Source: fact.Source{Harness: "curator"}},
		Body: "`MAX_RETRIES` is three.\n"}
	b := &Builder{G: New(root), Redact: func(in []string) ([]string, error) { return in, nil }}
	tl, err := b.BuildOne(context.Background(), FactRef{Fact: f})
	if err != nil {
		t.Fatal(err)
	}
	if len(tl.Summary) == 0 || !strings.Contains(strings.Join(tl.Summary, " "), "changed what this fact names") || !strings.Contains(strings.Join(tl.Summary, " "), "retry five times") {
		t.Errorf("summary %q", tl.Summary)
	}
}
