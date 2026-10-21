package search

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var fixtureWords = strings.Fields("deploy pipeline valkey cluster shard failover replica snapshot backup restore homebrew prefix " +
	"pgrep pkill shell exit golang module vendor lint format test flaky timeout retry network proxy dns latency " +
	"memory leak crash core dump signal hook prompt session index build stale cache lock journal sqlite fts " +
	"porter stem ranking bm25 owner ruling quote review curator dreamer overlay layer machine host repo branch")

func phrase(r *rand.Rand, n int) string {
	w := make([]string, n)
	for i := range w {
		w[i] = fixtureWords[r.Intn(len(fixtureWords))]
	}
	return strings.Join(w, " ")
}

// layeredFixture writes a store and an overlay of nStore and nLocal facts
// drawn from seed: every scope a query filters on, inbox and superseded
// facts, and the overlay's three kinds of file (an agent's note, a dreamer
// note, a local curator's fact).
func layeredFixture(t *testing.T, seed int64, nStore, nLocal int) (root, local string) {
	t.Helper()
	base := t.TempDir()
	root, local = filepath.Join(base, "store"), filepath.Join(base, "local")
	r := rand.New(rand.NewSource(seed))
	scopes := []string{"user", "system", "machines/host-a", "machines/host-b", "projects/api", "projects/web", "derived/api"}
	for i := 0; i < nStore; i++ {
		sc := scopes[r.Intn(len(scopes))]
		name := fmt.Sprintf("fact-%d", i)
		rel := sc + "/" + name + ".md"
		extra := ""
		switch r.Intn(10) {
		case 0:
			rel = "inbox/claude@host-a/" + name + ".md"
		case 1:
			extra = "superseded_by: fact-0\n"
		}
		write(t, root, rel, name, sc, phrase(r, 4+r.Intn(6)), phrase(r, 10+r.Intn(30)), extra)
	}
	for i := 0; i < nLocal; i++ {
		name := fmt.Sprintf("note-%d", i)
		sc := scopes[r.Intn(4)]
		rel := "claude@host-a/" + name + ".md"
		switch r.Intn(3) {
		case 0:
			rel = "inbox/dreamer@host-a/" + name + ".md"
		case 1:
			rel = "curated/" + sc + "/" + name + ".md"
		}
		write(t, local, rel, name, sc, phrase(r, 4+r.Intn(6)), phrase(r, 10+r.Intn(30)), "")
	}
	return root, local
}

// The layered index (the store-only file copied, the overlay's notes added)
// answers as a build of everything from nothing: the same rows under the
// same ids, the same evidence tables, the same full-text matches and bm25
// scores for every word, and the same Query and Rank answers.
func TestLayeredIndexAnswersAsAFullBuild(t *testing.T) {
	t.Setenv("TACIT_LOCAL_LAYER", "on")
	ctx := context.Background()
	for _, seed := range []int64{1, 2, 3} {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			root, local := layeredFixture(t, seed, 300, 40)
			dir := t.TempDir()
			full := filepath.Join(dir, "full.db")
			base := filepath.Join(dir, "base.db")
			layered := filepath.Join(dir, "layered.db")
			if err := build(ctx, root, local, full, ""); err != nil {
				t.Fatal(err)
			}
			if err := build(ctx, root, "", base, ""); err != nil {
				t.Fatal(err)
			}
			if err := build(ctx, root, local, layered, base); err != nil {
				t.Fatal(err)
			}
			compareIndexes(t, full, layered, fixtureWords)
		})
	}
}

// compareIndexes fails t where the index files a and b answer differently.
func compareIndexes(t *testing.T, a, b string, words []string) {
	t.Helper()
	ctx := context.Background()
	ia, err := openFile(ctx, a, "h")
	if err != nil {
		t.Fatal(err)
	}
	defer ia.Close()
	ib, err := openFile(ctx, b, "h")
	if err != nil {
		t.Fatal(err)
	}
	defer ib.Close()
	same := func(what string, q string, args ...any) {
		t.Helper()
		ra, rb := dump(t, ia.db, q, args...), dump(t, ib.db, q, args...)
		if !reflect.DeepEqual(ra, rb) {
			t.Fatalf("%s differs: %d vs %d rows\nfull:    %.300v\nlayered: %.300v", what, len(ra), len(rb), ra, rb)
		}
	}
	same("facts", `SELECT * FROM facts ORDER BY id`)
	same("evidence", `SELECT * FROM evidence ORDER BY id`)
	same("evidence_postings", `SELECT * FROM evidence_postings ORDER BY kind, token, id`)
	for _, w := range words {
		same("fts "+w, `SELECT rowid, bm25(fts, 2.0, 4.0, 1.0) FROM fts WHERE fts MATCH ? ORDER BY rank, rowid`, `"`+w+`"`)
		same("fts_words "+w, `SELECT rowid, bm25(fts_words) FROM fts_words WHERE fts_words MATCH ? ORDER BY rank, rowid`, `"`+w+`"*`)
	}
	sits := []Situation{
		{Host: "host-a", Query: "deploy pipeline valkey"},
		{Host: "host-b", Repo: "api", Query: "flaky test timeout retry", Derived: true},
		{Host: "host-a", Repo: "web", Query: "owner ruling quote review curator", PromptRelevance: true},
		{Host: "host-a", Query: "sqlite fts index stale lock", PromptRelevance: true, Printable: true},
		{Host: "host-a", Repo: "api", Paths: []string{"cmd/deploy/main.go"}, Command: "pkill -f shell"},
	}
	hits, locals := 0, 0
	for _, sit := range sits {
		for _, inbox := range []bool{false, true} {
			ha, erra := ia.Query(ctx, sit, 20, inbox)
			for _, h := range ha {
				hits++
				if h.Local {
					locals++
				}
			}
			hb, errb := ib.Query(ctx, sit, 20, inbox)
			if (erra == nil) != (errb == nil) || !reflect.DeepEqual(ha, hb) {
				t.Fatalf("Query %+v inbox=%v differs:\nfull:    %v %+v\nlayered: %v %+v", sit, inbox, erra, ha, errb, hb)
			}
		}
	}
	if hits == 0 || locals == 0 {
		t.Fatalf("the queries found %d hits, %d of them local notes: nothing compared", hits, locals)
	}
	for _, text := range []string{"depl", "valkey clu", "owner", "stale lock jour"} {
		ra, erra := ia.Rank(ctx, text, 200, nil)
		rb, errb := ib.Rank(ctx, text, 200, nil)
		if (erra == nil) != (errb == nil) || !reflect.DeepEqual(ra, rb) {
			t.Fatalf("Rank %q differs:\nfull:    %v %+v\nlayered: %v %+v", text, erra, ra, errb, rb)
		}
	}
}

func dump(t *testing.T, db *sql.DB, q string, args ...any) [][]any {
	t.Helper()
	rows, err := db.Query(q, args...)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out [][]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		out = append(out, vals)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
