package search

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"github.com/agent-sh/tacit/client/internal/fact"
)

// buildEvidence persists bounded lexical observations, not inferred edges.
// It shares the FTS transaction and commit/overlay invalidation lifecycle.
func buildEvidence(ctx context.Context, tx *sql.Tx, facts []*fact.Fact, nStore int) error {
	for _, q := range []string{
		`CREATE TABLE evidence(id INTEGER PRIMARY KEY, terms TEXT, pairs TEXT)`,
		`CREATE TABLE evidence_postings(kind INTEGER, token TEXT, id INTEGER, PRIMARY KEY(kind, token, id)) WITHOUT ROWID`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	doc, err := tx.PrepareContext(ctx, `INSERT INTO evidence VALUES(?,?,?)`)
	if err != nil {
		return err
	}
	defer doc.Close()
	posting, err := tx.PrepareContext(ctx, `INSERT INTO evidence_postings VALUES(?,?,?)`)
	if err != nil {
		return err
	}
	defer posting.Close()
	for i, f := range facts {
		if i >= nStore || f.InInbox() || f.Meta.SupersededBy != "" || f.Meta.Retired != "" || strings.HasPrefix(f.Meta.Scope, "derived/") || f.Meta.Type == "result_ref" {
			continue
		}
		terms := map[string]bool{}
		for _, t := range evidenceTokens(f.Meta.Name + " " + f.Meta.Description) {
			terms[t] = true
		}
		pairs := evidencePairs(f.Meta.Name)
		for p := range evidencePairs(f.Meta.Description) {
			pairs[p] = true
		}
		tj, _ := json.Marshal(terms)
		pj, _ := json.Marshal(pairs)
		if _, err := doc.ExecContext(ctx, i+1, string(tj), string(pj)); err != nil {
			return err
		}
		for kind, tokens := range []map[string]bool{terms, pairs} {
			for token := range tokens {
				if _, err := posting.ExecContext(ctx, kind, token, i+1); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (ix *Index) evidenceFrequency(ctx context.Context, kind int, tokens []string) (map[string]int, error) {
	data, _ := json.Marshal(tokens)
	rows, err := ix.db.QueryContext(ctx, `SELECT token,count(*) FROM evidence_postings WHERE kind=? AND token IN (SELECT value FROM json_each(?)) GROUP BY token`, kind, string(data))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var token string
		var count int
		if err := rows.Scan(&token, &count); err != nil {
			return nil, err
		}
		counts[token] = count
	}
	return counts, rows.Err()
}
