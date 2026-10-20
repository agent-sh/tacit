package search

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
)

// liveColumns is the column list the scans below read, in Hit order.
const liveColumns = `path, name, scope, type, description, body, inbox, kind, conflicts, ruling, intask, words, local, coalesce(applies, ''), coalesce(created, '')`

// Tied returns every live curated fact tied to one of the repo names: its
// scope is projects/<name> (and its applies_to.repos, when set, names one of
// them too, as the hooks' repo restriction requires), or its applies_to.repos
// names one of them. No text match: a fact is the repo's because its
// metadata says so, not because its words name the repo. Derived facts,
// inbox notes, superseded or retired facts and unquoted result facts are
// left out. Scores are zero; the caller orders.
func (ix *Index) Tied(ctx context.Context, names []string) ([]Hit, error) {
	var lower []string
	for _, n := range names {
		if n = strings.ToLower(strings.TrimSpace(n)); n != "" {
			lower = append(lower, n)
		}
	}
	if len(lower) == 0 {
		return nil, nil
	}
	b, _ := json.Marshal(lower)
	q := `SELECT ` + liveColumns + ` FROM facts f
		WHERE f.superseded = 0 AND f.inbox = 0 AND f.type != 'result_ref' AND f.scope NOT LIKE 'derived/%'
		AND (
			EXISTS (SELECT 1 FROM json_each(f.applies, '$.Repos') r JOIN json_each(:names) n ON lower(r.value) = n.value)
			OR (EXISTS (SELECT 1 FROM json_each(:names) n WHERE f.scope = 'projects/' || n.value)
				AND coalesce(json_array_length(f.applies, '$.Repos'), 0) = 0)
		)
		ORDER BY f.path`
	return ix.scan(ctx, q, map[string]any{"names": string(b)})
}

// MetaFacts returns every live curated fact whose applies_to names paths or
// commands, for matching against the paths and commands of a prompt or a
// tool call. Derived facts, inbox notes and superseded or retired facts are
// left out.
func (ix *Index) MetaFacts(ctx context.Context) ([]Hit, error) {
	q := `SELECT ` + liveColumns + ` FROM facts f
		WHERE f.superseded = 0 AND f.inbox = 0 AND f.type != 'result_ref' AND f.scope NOT LIKE 'derived/%'
		AND (coalesce(json_array_length(f.applies, '$.Paths'), 0) > 0 OR coalesce(json_array_length(f.applies, '$.Commands'), 0) > 0)
		ORDER BY f.path`
	return ix.scan(ctx, q, nil)
}

func (ix *Index) scan(ctx context.Context, q string, named map[string]any) ([]Hit, error) {
	var args []any
	for k, v := range named {
		args = append(args, sql.Named(k, v))
	}
	rows, err := ix.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Hit
	for rows.Next() {
		var h Hit
		var inbox, ruling, intask, words, local int
		var conflicts, applies string
		if err := rows.Scan(&h.Path, &h.Name, &h.Scope, &h.Type, &h.Description, &h.Body, &inbox, &h.Kind, &conflicts, &ruling, &intask, &words, &local, &applies, &h.Created); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(applies), &h.Applies)
		h.Inbox, h.Ruling, h.InTask, h.Words, h.Local = inbox == 1, ruling == 1, intask == 1, words == 1, local == 1
		if conflicts != "" {
			h.Conflicts = strings.Split(conflicts, ",")
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// List returns the live facts in scopes, of type typ when it is set, owner
// rulings only when rulingsOnly, inbox notes only when inbox; superseded and
// retired facts never. Scores are zero; the caller orders.
func (ix *Index) List(ctx context.Context, scopes []string, typ string, rulingsOnly, inbox bool) ([]Hit, error) {
	if len(scopes) == 0 {
		return nil, nil
	}
	b, _ := json.Marshal(scopes)
	q := `SELECT ` + liveColumns + ` FROM facts f
		WHERE f.superseded = 0 AND f.scope IN (SELECT value FROM json_each(:scopes))`
	named := map[string]any{"scopes": string(b)}
	if !inbox {
		q += ` AND f.inbox = 0`
	}
	if typ != "" {
		q += ` AND f.type = :typ`
		named["typ"] = typ
	}
	if rulingsOnly {
		q += ` AND f.ruling = 1`
	}
	return ix.scan(ctx, q+` ORDER BY f.path`, named)
}

// Counts returns, for each of scopes that holds live curated facts, how
// many it holds of each type.
func (ix *Index) Counts(ctx context.Context, scopes []string) (map[string]map[string]int, error) {
	b, _ := json.Marshal(scopes)
	rows, err := ix.db.QueryContext(ctx, `SELECT scope, type, count(*) FROM facts
		WHERE superseded = 0 AND inbox = 0 AND scope IN (SELECT value FROM json_each(?)) GROUP BY scope, type`, string(b))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]int{}
	for rows.Next() {
		var scope, typ string
		var n int
		if err := rows.Scan(&scope, &typ, &n); err != nil {
			return nil, err
		}
		if out[scope] == nil {
			out[scope] = map[string]int{}
		}
		out[scope][typ] = n
	}
	return out, rows.Err()
}

// Scopes returns every scope that holds a fact.
func (ix *Index) Scopes(ctx context.Context) ([]string, error) {
	rows, err := ix.db.QueryContext(ctx, `SELECT DISTINCT scope FROM facts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
