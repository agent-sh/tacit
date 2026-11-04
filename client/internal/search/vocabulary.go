package search

import (
	"context"
	"strings"
)

// InVocabulary is the terms of terms that some fact of the store holds, in
// its name, description or body (FTS match, by stem, as a query term
// matches), in order. Facts of this machine's overlay and local layer are
// not counted: only what the store holds, which every write to it masks
// first (store.Redact), so a term found here is no secret the store did
// not already hold. On an error it returns none.
func (ix *Index) InVocabulary(ctx context.Context, terms []string) []string {
	var out []string
	for _, t := range terms {
		var one int
		q := `"` + strings.ReplaceAll(t, `"`, "") + `"`
		err := ix.db.QueryRowContext(ctx, `SELECT 1 FROM fts JOIN facts f ON f.id = fts.rowid WHERE fts MATCH ? AND f.local = 0 LIMIT 1`, q).Scan(&one)
		if err == nil {
			out = append(out, t)
		} else if ctx.Err() != nil {
			return nil
		}
	}
	return out
}

// HasFact reports whether the store holds a fact of this name (not this
// machine's overlay or local layer).
func (ix *Index) HasFact(ctx context.Context, name string) bool {
	var one int
	return ix.db.QueryRowContext(ctx, `SELECT 1 FROM facts WHERE name = ? AND local = 0 LIMIT 1`, name).Scan(&one) == nil
}
