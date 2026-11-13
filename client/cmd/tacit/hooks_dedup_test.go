package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/search"
)

// A fact whose content words the repository's own AGENTS.md already covers
// this widely (its name and description words, its name's) reaches no prompt
// block and no tool push: the session carries the rule already. The dedup
// setting turns the gate off, and dedup_files adds files beyond the
// repository's own instructions.
func TestDedupHoldsFactsTheInstructionsAlreadyCarry(t *testing.T) {
	_, cwd := repoFirstRig(t,
		fact.Meta{Name: "widget-signoff", Description: "Every commit carries a Signed-off-by trailer", Type: "user_standard", Scope: "projects/widget", AppliesTo: widgetRepo},
		fact.Meta{Name: "widget-pool-order", Description: "Take the shard lock before the slot lock", Type: "past_mistake", Scope: "projects/widget", AppliesTo: fact.AppliesTo{Repos: []string{"widget"}, Paths: []string{"src/scheduler/pool.go"}}},
	)
	// The repository's instructions restate the signoff rule, not the pool one.
	if err := os.WriteFile(filepath.Join(cwd, "AGENTS.md"), []byte("# widget\n\nEvery commit carries a Signed-off-by trailer. The widget signoff bot rejects a commit without one.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TACIT_DELIVERY_REPO_START", "off")
	t.Setenv("TACIT_DELIVERY_PROMPT_ACTION", "off")
	t.Setenv("TACIT_DELIVERY_PROMPT_METADATA", "off")
	if out := hookOut(t, promptPayload(cwd, "s1", "fix the commit signoff trailer check"), "prompt"); strings.Contains(out, "widget-signoff") {
		t.Fatalf("a fact the AGENTS.md already holds was delivered:\n%s", out)
	}
	if out := hookOut(t, promptPayload(cwd, "s2", "the scheduler takes the shard lock before the slot lock where"), "prompt"); !strings.Contains(out, "widget-pool-order") {
		t.Fatalf("a fact the AGENTS.md does not hold was held:\n%s", out)
	}
	edit := toolPayload(cwd, "s3", "Edit", map[string]any{"file_path": filepath.Join(cwd, "src", "scheduler", "pool.go"), "old_string": "a", "new_string": "b"})
	if got := preToolContext(t, hookOut(t, edit, "pre-tool")); !strings.Contains(got, "widget-pool-order") {
		t.Fatalf("a path push was held:\n%s", got)
	}
	t.Setenv("TACIT_DELIVERY_DEDUP", "off")
	if out := hookOut(t, promptPayload(cwd, "s4", "fix the commit signoff trailer check"), "prompt"); !strings.Contains(out, "widget-signoff") {
		t.Fatalf("dedup off still held the fact:\n%s", out)
	}
	t.Setenv("TACIT_DELIVERY_DEDUP", "")
	// dedup_files: a file beyond the repository's own instructions holds the
	// rule instead, and the fact is held through it.
	os.Remove(filepath.Join(cwd, "AGENTS.md"))
	extra := filepath.Join(t.TempDir(), "rulebook.md")
	if err := os.WriteFile(extra, []byte("Every commit carries a Signed-off-by trailer. The signoff widget bot checks commits.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TACIT_DELIVERY_DEDUP_FILES", extra)
	if out := hookOut(t, promptPayload(cwd, "s5", "fix the commit signoff trailer check"), "prompt"); strings.Contains(out, "widget-signoff") {
		t.Fatalf("a fact a dedup file holds was delivered:\n%s", out)
	}
	t.Setenv("TACIT_DELIVERY_DEDUP_FILES", "")
	if out := hookOut(t, promptPayload(cwd, "s6", "fix the commit signoff trailer check"), "prompt"); !strings.Contains(out, "widget-signoff") {
		t.Fatalf("without any corpus the fact was still held:\n%s", out)
	}
}

// The matcher holds a fact only on wide coverage: this share of its name and
// description words, this share of its name's, this many words matched. A
// fact the corpus only talks around stays.
func TestDedupMatcherThresholds(t *testing.T) {
	c := dedupCorpus{}
	for _, w := range dedupWords("Every commit carries a Signed-off-by trailer. The widget signoff bot rejects a commit without one.") {
		c[w] = true
	}
	held := search.Hit{Name: "widget-signoff", Description: "Every commit carries a Signed-off-by trailer"}
	if !c.holdsFact(held) {
		t.Fatal("the restated rule was not held")
	}
	around := search.Hit{Name: "widget-signoff", Description: "The bot posts a comment on every open pull request and links the artifacts"}
	if c.holdsFact(around) {
		t.Fatal("a fact the corpus only talks around was held")
	}
	short := search.Hit{Name: "widget-ci", Description: "CI is green"}
	if c.holdsFact(short) {
		t.Fatal("a fact with fewer words than the minimum was held")
	}
	if (dedupCorpus{}).holdsFact(held) || (dedupCorpus{"every": true}).holdsFact(held) {
		t.Fatal("an empty corpus held a fact")
	}
	// Literal hyphenated identifier in the corpus satisfies the name share.
	lit := dedupCorpus{"widget-signoff": true, "every": true, "commit": true, "carries": true, "signed-off-by": true, "trailer": true}
	if !lit.holdsFact(held) {
		t.Fatal("a corpus naming the hyphenated fact literally did not hold it")
	}
	// Exact threshold boundaries: 0.75 total, 0.60 name, at least 4 matched words.
	threeWords := search.Hit{Name: "widget", Description: "commit trailer"}
	if c.holdsFact(threeWords) {
		t.Fatal("three matched words should stay below dedupMinHit")
	}
	// Name: 3 of 5 matched (0.60); total: 6 of 8 matched (0.75).
	exact := search.Hit{Name: "alpha-bravo-charlie-delta-echo", Description: "foxtrot golf hotel"}
	b := dedupCorpus{"alpha": true, "bravo": true, "charlie": true, "foxtrot": true, "golf": true, "hotel": true}
	if !b.holdsFact(exact) {
		t.Fatal("exact 0.75 total and 0.60 name boundary should hold")
	}
	// 2 of 4 name words (0.50 < 0.60) with 5 of 6 total (0.83) must stay.
	lowName := search.Hit{Name: "alpha-bravo-delta-echo", Description: "foxtrot golf hotel"}
	if b.holdsFact(lowName) {
		t.Fatal("below 0.60 name share should stay")
	}
}

// Only tier 0 and 1 non-derived facts are held; tier-2 and derived facts
// stay even under full corpus coverage, and lazy loading skips corpus I/O
// when no hit is eligible.
func TestDedupTierAndDerivedExemption(t *testing.T) {
	loads := 0
	load := func() dedupCorpus {
		loads++
		return dedupCorpus{"alpha": true, "bravo": true, "charlie": true, "delta": true, "echo": true}
	}
	derived := search.Hit{Name: "alpha-bravo", Description: "charlie delta echo", Type: "user_standard", Scope: "derived/alpha"}
	tier2 := search.Hit{Name: "alpha-bravo", Description: "charlie delta echo", Type: "history", Scope: "projects/widget"}
	got := withoutDuplicates([]search.Hit{derived, tier2}, load)
	if len(got) != 2 || loads != 0 {
		t.Fatalf("expected both ineligible hits kept and 0 loads, got %d hits, %d loads", len(got), loads)
	}
	tier0 := search.Hit{Name: "alpha-bravo", Description: "charlie delta echo", Type: "user_standard", Scope: "projects/widget"}
	got = withoutDuplicates([]search.Hit{tier0, tier2, derived}, load)
	if len(got) != 2 || loads != 1 || got[0].Name != "alpha-bravo" || got[1].Scope != "derived/alpha" {
		t.Fatalf("expected tier-0 held and tier-2/derived kept (loads=%d): %+v", loads, got)
	}
}

// loadDedupCorpus expands ~/ globs, skips ~user paths, and ignores missing files.
func TestDedupFilesGlobAndTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	sub := filepath.Join(home, "rules")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "one.md"), []byte("alpha bravo charlie delta"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TACIT_DELIVERY_DEDUP", "")
	t.Setenv("TACIT_DELIVERY_DEDUP_FILES", "~/rules/*.md:~/rules/*.md:~otheruser/nope.md:~/missing.md")
	c := loadDedupCorpus(nil)
	for _, w := range []string{"alpha", "bravo", "charlie", "delta"} {
		if !c[w] {
			t.Fatalf("missing expected word %q from ~/ glob corpus: %v", w, c)
		}
	}
}
