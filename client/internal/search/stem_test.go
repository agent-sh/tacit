package search

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// The Go stemmer must give SQLite's stems, word for word: the gate counts
// query terms by stem in Go and the index matches them by stem in FTS5.
// (Checked once against /usr/share/dict/words, 236,064 words, no difference.)
func TestStemMatchesFTS5(t *testing.T) {
	words := strings.Fields(`amended amending amend amends rotate rotation rotating rotated tokens token secrets scrub
		scrubbing parity reviewers reviewer approvals approved approve merging merged stacked public publicly agreed
		feed feeds sses ies eed aed yyy happy sky skies flying cry caresses ponies ties caress cats plastered bled
		motoring sing conflated troubled sized hopping tanned falling hissing fizzed failing filing generalization
		oscillators relational conditional rational valency hesitancy digitizer conformably radically differently
		vilely analogously vietnamization predication operator feudalism decisiveness hopefulness callousness
		formality sensitivity sensibility triplicate formative formalize electricity electrical hopeful goodness
		revival allowance inference airliner gyroscopic adjustable defensible irritant replacement adjustment
		dependent adoption homologou communism activate angulariti homologous effective bowdlerize probate rate
		cease controll roll deploys deployment deploying pipelines reviewed abc ab x`)
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, q := range []string{
		`CREATE VIRTUAL TABLE s USING fts5(x, tokenize='porter unicode61 remove_diacritics 2')`,
		`CREATE VIRTUAL TABLE v USING fts5vocab(s, instance)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for i, w := range words {
		if _, err := db.Exec(`INSERT INTO s(rowid, x) VALUES(?, ?)`, i+1, w); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := db.Query(`SELECT doc, term FROM v`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	want := map[int]string{}
	for rows.Next() {
		var doc int
		var term string
		if err := rows.Scan(&doc, &term); err != nil {
			t.Fatal(err)
		}
		want[doc] = term
	}
	for i, w := range words {
		if got := stem(w); got != want[i+1] {
			t.Errorf("stem(%q) = %q, FTS5 %q", w, got, want[i+1])
		}
	}
}

func TestDistinctStems(t *testing.T) {
	got := strings.Join(distinctStems([]string{"amended", "review", "amend", "reviewers", "reviewer", "foo_bars", "foo_bar"}), ",")
	if got != "amended,review,foo_bars" {
		t.Fatalf("distinctStems: %s", got)
	}
}

// The relationship route matches words as written: a second spelling of a
// word in the prompt must not drop the one its pairs were stored under.
func TestQueryKeepsEverySpellingForRelationshipEvidence(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 20; i++ {
		write(t, root, fmt.Sprintf("system/padding-%d.md", i), fmt.Sprintf("padding-%d", i), "system", "ordinary unrelated guidance", "", "")
	}
	write(t, root, "machines/host-a/relay-unit.md", "relay-unit", "machines/host-a", "Restart the aster relay service.", "", "applies_to:\n  repos: [capture-repo]\n  paths: [/verified/relay.conf]\n  commands: [systemctl --user restart aster-relay]\n")
	write(t, root, "system/protocol.md", "protocol", "system", "The aster relay protocol is documented.", "", "")
	t.Setenv("TACIT_INDEX", filepath.Join(t.TempDir(), "index"))
	t.Setenv("TACIT_LOCAL", filepath.Join(t.TempDir(), "local"))
	ix, err := Open(context.Background(), root, "demo")
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	for _, q := range []string{"restart the aster relay service", "the relays: restart the aster relay service"} {
		hits, err := ix.Query(context.Background(), Situation{Query: q, Host: "host-a", RestrictRepos: true, PromptRelevance: true}, 50, false)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, h := range hits {
			found = found || (h.Name == "relay-unit" && h.Evidence != nil && h.Evidence.Supported)
		}
		if !found {
			t.Errorf("%q: relay-unit not delivered with supported evidence", q)
		}
	}
}
