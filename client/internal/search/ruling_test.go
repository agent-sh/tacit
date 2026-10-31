package search

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A RULE line drops the dated preamble, "Owner" or a configured name.
func TestAsRuleStripsTheOwnerPreamble(t *testing.T) {
	t.Setenv("TACIT_OWNER_NAMES", "")
	if got := asRule("Owner 2026-09-10: ship on green"); got != "Ship on green" { // privacy-lint: allow (synthetic attribution)
		t.Errorf("owner: %q", got)
	}
	if got := asRule("Dana 2026-09-10: ship on green"); got != "Dana 2026-09-10: ship on green" {
		t.Errorf("an unconfigured name was stripped: %q", got)
	}
	t.Setenv("TACIT_OWNER_NAMES", "dana")
	if got := asRule("Dana 2026-09-10: ship on green"); got != "Ship on green" {
		t.Errorf("configured name: %q", got)
	}
}

// In an audited store a hook prints RULE only for their ruling (kind user with
// their words on record); a kind user fact with no words of theirs, and a standard
// an agent filed, are plain facts.
func TestBlockMarksOnlyHisRulingsAsRules(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	src := func(name, s, typ string) string {
		return "---\nname: " + name + "\ndescription: zebra quokka " + name + "\ntype: " + typ + "\nscope: projects/atlas\nsource:\n  harness: curator\n  host: host-a\n" + s + "created: 2026-10-08\n---\n\nOwner: \"never rent a zebra box\" zebra quokka\n"
	}
	for name, body := range map[string]string{
		"owners-rule":    src("owners-rule", "  kind: user\n  owner_quote: never rent a zebra box\n  owner_quote_from: claude:s1\n", "design_decision"),
		"bare-kind-user": src("bare-kind-user", "  kind: user\n", "design_decision"),
		"agents-reading": src("agents-reading", "  kind: observed\n", "user_standard"),
		// The reviewer's forged quote: the fields, but no ledger entry.
		"forged": src("forged", "  kind: user\n  owner_quote: never rent a zebra box\n  owner_quote_from: claude:forged-session\n", "user_standard"),
		// Words of theirs too short for a rule: no RULE, but the index says so.
		"owners-words": src("owners-words", "  kind: observed\n  owner_words: never rent a zebra box\n  owner_words_from: claude:s1\n", "design_decision"),
	} {
		p := filepath.Join(root, "projects", "atlas", name+".md")
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
	}
	os.MkdirAll(filepath.Join(root, "review"), 0o755)
	os.WriteFile(filepath.Join(root, "review", "rulings-audit.json"), []byte(`{"version":1,"audited_head":"x","at":"t"}`), 0o644)
	os.WriteFile(filepath.Join(root, "review", "owner-quotes.jsonl"), []byte(`{"name":"owners-rule","quote":"never rent a zebra box","from":"claude:s1","turn_hash":"h","at":"2026-10-11"}`+"\n"+
		`{"name":"owners-words","quote":"never rent a zebra box","from":"claude:s1","turn_hash":"h","at":"2026-10-11","words":true}`+"\n"), 0o644)
	t.Setenv("TACIT_INDEX", filepath.Join(filepath.Dir(root), "index"))
	t.Setenv("TACIT_LOCAL", filepath.Join(filepath.Dir(root), "local"))
	ix, err := Open(context.Background(), root, "r1")
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	hits, err := ix.Query(context.Background(), Situation{Repo: "atlas", Query: "zebra quokka"}, 5, false)
	if err != nil || len(hits) != 5 {
		t.Fatalf("hits %v %v", hits, err)
	}
	block := Block(hits, "facts:")
	if !strings.Contains(block, "- RULE owners-rule") || strings.Contains(block, "RULE bare-kind-user") || strings.Contains(block, "RULE agents-reading") || strings.Contains(block, "RULE forged") || strings.Contains(block, "RULE owners-words") {
		t.Fatalf("block:\n%s", block)
	}
	// The index carries whether a fact holds backed words of theirs (the
	// curator's supersede guard reads it).
	for _, h := range hits {
		if h.Words != (h.Name == "owners-words") {
			t.Errorf("%s: words %v", h.Name, h.Words)
		}
	}
}

// The index marks a ruling whose quote is an instruction inside its task
// (ownerwords.InTask). It stays their ruling (Ruling, which the guards read);
// only delivery ranks it as an ordinary fact (Rule). A rule they wrote in
// tacit app is a rule whatever its words, and an agent's fact is no ruling.
func TestIndexMarksInTaskRulings(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	src := func(name, harness, s string) string {
		return "---\nname: " + name + "\ndescription: zebra quokka " + name + "\ntype: user_standard\nscope: user\nsource:\n  harness: " + harness + "\n  host: host-a\n" + s + "created: 2026-10-08\n---\n\nOwner: \"lets rent the zebra box now\" zebra quokka\n"
	}
	for name, body := range map[string]string{
		"standing": src("standing", "curator", "  kind: user\n  owner_quote: never rent a zebra box\n  owner_quote_from: claude:s1\n"),
		"in-task":  src("in-task", "curator", "  kind: user\n  owner_quote: lets rent the zebra box now\n  owner_quote_from: claude:s1\n"),
		"app-rule": src("app-rule", "human", "  kind: user\n  owner_quote: lets rent the zebra box now\n  owner_quote_from: note:app-rule\n"),
		"agents":   src("agents", "curator", "  kind: observed\n"),
		// Their decision proves it, not the quote: a quote no ledger backs
		// does not make it a ruling in its task.
		"endorsed": src("endorsed", "curator", "  kind: user\n  owner_quote: lets rent the zebra box now\n  owner_quote_from: claude:s9\n  endorsed: 0123456789ab\n"),
	} {
		p := filepath.Join(root, "user", name+".md")
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
	}
	os.MkdirAll(filepath.Join(root, "review"), 0o755)
	os.WriteFile(filepath.Join(root, "review", "rulings-audit.json"), []byte(`{"version":1,"audited_head":"x","at":"t"}`), 0o644)
	os.WriteFile(filepath.Join(root, "review", "decisions.jsonl"), []byte(`{"id":"0123456789ab","action":"mine","a":"endorsed","at":"2026-10-11T07:00:00Z"}`+"\n"), 0o644)
	os.WriteFile(filepath.Join(root, "review", "owner-quotes.jsonl"), []byte(`{"name":"standing","quote":"never rent a zebra box","from":"claude:s1","turn_hash":"h","at":"2026-10-11"}`+"\n"+
		`{"name":"in-task","quote":"lets rent the zebra box now","from":"claude:s1","turn_hash":"h","at":"2026-10-11"}`+"\n"+
		`{"name":"app-rule","quote":"lets rent the zebra box now","from":"note:app-rule","turn_hash":"h","at":"2026-10-11"}`+"\n"), 0o644)
	t.Setenv("TACIT_INDEX", filepath.Join(filepath.Dir(root), "index"))
	t.Setenv("TACIT_LOCAL", filepath.Join(filepath.Dir(root), "local"))
	ix, err := Open(context.Background(), root, "r1")
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	hits, err := ix.Query(context.Background(), Situation{Query: "zebra quokka"}, 5, false)
	if err != nil || len(hits) != 5 {
		t.Fatalf("hits %v %v", hits, err)
	}
	want := map[string][3]bool{ // ruling, in task, rule
		"standing": {true, false, true},
		"in-task":  {true, true, false},
		"app-rule": {true, false, true},
		"agents":   {false, false, false},
		"endorsed": {true, false, true},
	}
	for _, h := range hits {
		if got := [3]bool{h.Ruling, h.InTask, h.Rule()}; got != want[h.Name] {
			t.Errorf("%s: ruling, in task, rule = %v, want %v", h.Name, got, want[h.Name])
		}
	}
	if block := Block(hits, "facts:"); strings.Contains(block, "RULE in-task") || !strings.Contains(block, "- in-task") {
		t.Fatalf("block:\n%s", block)
	}
}

// A statement is printed in their words, dated, with its context, never as
// a RULE; a flag of a check that disagrees is said beside it.
func TestBlockPrintsAStatementInTheirWords(t *testing.T) {
	h := Hit{Name: "storefront-name", Type: "statement", Description: "The name of the storefront repository.",
		Body: "You said once (2026-10-21): \"the real name is storefront, shop-v2 is a name for now\"\nContext: The name of the storefront repository.\n"}
	got := Block([]Hit{h}, "# t")
	want := "- storefront-name: you said once (2026-10-21): \"the real name is storefront, shop-v2 is a name for now\"; context: The name of the storefront repository\n"
	if !strings.HasSuffix(got, want) || strings.Contains(got, "RULE") {
		t.Fatalf("%q", got)
	}
	h.Body += "> check disagrees 2026-10-22: storefront is the real name: fails\n"
	if got := Block([]Hit{h}, "# t"); !strings.Contains(got, "(a check disagrees, maybe misread)") {
		t.Fatalf("%q", got)
	}
}
