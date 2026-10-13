// Package search builds and queries the local fact index: SQLite FTS5 over
// name, description and body, keyed by the store's HEAD so a stale index is
// never used. The ranking is bm25 with descriptions weighted over bodies,
// the lexical variant that measured best in eval/VALUE.md, filtered to the
// scopes a situation can use (its repo, its host, system, user).
package search

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	_ "modernc.org/sqlite"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/overlay"
	"github.com/agent-sh/tacit/client/internal/ownerwords"
)

// Hit is one ranked fact.
type Hit struct {
	Path        string
	Name        string
	Scope       string
	Type        string
	Description string
	Body        string
	Inbox       bool
	Kind        string
	Ruling      bool // an owner ruling: their own words (fact.IsRuling); no agent may supersede it
	Words       bool // holds backed words of theirs (fact.OwnerWordsOf); no agent supersedes it without asking them
	Conflicts   []string
	// Local is a note in this machine's overlay (internal/overlay): written
	// by a harness here on a read-only clone, never pushed, never reviewed.
	Local bool
	Score float64
}

// Index is an open FTS index for one store commit.
type Index struct {
	db  *sql.DB
	sha string
}

// IndexDir is where index files live: $TACIT_INDEX or ~/.local/state/tacit/index.
func IndexDir() string {
	if d := os.Getenv("TACIT_INDEX"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "tacit", "index")
}

// schemaVersion is part of the index file name, so a schema change rebuilds
// instead of failing on a column that an older build does not have.
const schemaVersion = 5

// Open returns the index for the store at head, building it when missing.
// Index files are index-v<schema>-<sha>.db under IndexDir, or
// index-v<schema>-<sha>-l<stamp>.db when this machine's local overlay
// (internal/overlay) holds notes, which are indexed with the store's facts;
// prune removes the ones nobody should need.
//
// The file is opened here, not at the first query, and the one connection is
// kept until Close: another process may prune the file at any time (a new
// HEAD, a binary of another schema version), and an open file survives its
// unlink while a lazy open does not. The 2026-10-11 curator held an unopened
// handle through a split call, the restarted follower pruned the file, and
// 509 conflict checks failed with "unable to open database file". A file
// pruned between the build and the open, or one that fails to read
// (truncated, corrupt), is built again, up to three times.
func Open(ctx context.Context, storeDir, head string) (*Index, error) {
	dir := IndexDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	local := overlay.Dir()
	if !overlay.Outside(local, storeDir) {
		local = "" // never index a directory inside the clone as this machine's notes
	}
	name := fmt.Sprintf("index-v%d-%s", schemaVersion, head)
	if st := overlay.Stamp(local); st != "" {
		name += "-l" + st
	} else {
		local = ""
	}
	path := filepath.Join(dir, name+".db")
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if _, err := os.Stat(path); err != nil {
			if err := build(ctx, storeDir, local, path); err != nil {
				return nil, err
			}
			prune(dir, path)
		}
		db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&immutable=1")
		if err != nil {
			return nil, err
		}
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
		db.SetConnMaxLifetime(0)
		db.SetConnMaxIdleTime(0)
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM facts`).Scan(&n); err != nil {
			db.Close()
			lastErr = err
			if ctx.Err() != nil {
				return nil, err
			}
			if _, serr := os.Stat(path); serr == nil {
				// The file is there and fails (truncated, corrupt): build it again.
				os.Remove(path)
			}
			continue
		}
		return &Index{db: db, sha: head}, nil
	}
	return nil, fmt.Errorf("index %s could not be opened after %d builds: %w", filepath.Base(path), 3, lastErr)
}

func (ix *Index) Close() error { return ix.db.Close() }

// Invalidate removes the cached index for a store commit. A caller that put
// files in the working tree and then took them out again (a refused or
// interrupted write) uses it, because a hook that fired in between built an
// index from the dirty tree and cached it under the unchanged HEAD.
func Invalidate(head string) {
	base := fmt.Sprintf("index-v%d-%s", schemaVersion, head)
	os.Remove(filepath.Join(IndexDir(), base+".db"))
	if more, err := filepath.Glob(filepath.Join(IndexDir(), base+"-l*.db")); err == nil {
		for _, p := range more {
			os.Remove(p)
		}
	}
}

// build writes the index to a temp file and renames it into place, so a
// crash never leaves a half-built index under the final name.
func build(ctx context.Context, storeDir, localDir, path string) error {
	facts, perrs, err := fact.Walk(storeDir)
	if err != nil {
		return err
	}
	if len(perrs) > 0 {
		fmt.Fprintf(os.Stderr, "tacit index: %d fact file(s) did not parse and are not indexed (run `tacit lint`)\n", len(perrs))
	}
	local, lerrs := overlay.Load(localDir)
	if len(lerrs) > 0 {
		fmt.Fprintf(os.Stderr, "tacit index: %d local note(s) in %s did not parse and are not indexed\n", len(lerrs), localDir)
	}
	nStore := len(facts)
	facts = append(facts, local...)
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	os.Remove(tmp)
	defer os.Remove(tmp)
	db, err := sql.Open("sqlite", "file:"+tmp)
	if err != nil {
		return err
	}
	defer db.Close()
	stmts := []string{
		`CREATE TABLE facts(id INTEGER PRIMARY KEY, path TEXT, name TEXT, scope TEXT, type TEXT, description TEXT, body TEXT, inbox INT, superseded INT, applies TEXT, kind TEXT, conflicts TEXT, ruling INT, words INT, local INT)`,
		`CREATE VIRTUAL TABLE fts USING fts5(name, description, body, content='facts', content_rowid='id', tokenize='unicode61 remove_diacritics 2')`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	ins, err := tx.PrepareContext(ctx, `INSERT INTO facts(path, name, scope, type, description, body, inbox, superseded, applies, kind, conflicts, ruling, words, local) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	for i, f := range facts {
		applies, _ := json.Marshal(f.Meta.AppliesTo)
		isLocal := i >= nStore
		// A local note is an agent's, unreviewed: never a ruling and never
		// their words, whatever kind its writer gave it.
		ruling, words := fact.IsRuling(f), fact.OwnerWordsOf(f) != ""
		if isLocal {
			ruling, words = false, false
		}
		if _, err := ins.ExecContext(ctx, f.Path, f.Meta.Name, f.Meta.Scope, f.Meta.Type, f.Meta.Description, strings.TrimSpace(f.Body), b2i(f.InInbox()), b2i(f.Meta.SupersededBy != "" || f.Meta.Retired != ""), string(applies), f.Meta.Source.Kind, strings.Join(f.Meta.ConflictsWith, ","), b2i(ruling), b2i(words), b2i(isLocal)); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO fts(fts) VALUES('rebuild')`); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if err := db.Close(); err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return nil // another process finished first; its file is identical
	}
	return os.Rename(tmp, path)
}

// prune removes the index files no process should need: other heads of this
// schema version, older schema versions, and temp files whose builder has
// exited. It keeps a newer schema version (a newer binary on this machine
// uses it; pruning it made the two builds delete each other's files on every
// sync) and another process's build in progress (deleting its journal failed
// that build with a disk I/O error).
func prune(dir, keep string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		name := e.Name()
		p := filepath.Join(dir, name)
		if p == keep || !strings.HasPrefix(name, "index-") {
			continue
		}
		if m := indexName.FindStringSubmatch(name); m != nil {
			if v, _ := strconv.Atoi(m[1]); v > schemaVersion {
				continue
			}
			if m[2] != "" {
				if pid, _ := strconv.Atoi(m[2]); pid > 0 && processAlive(pid) {
					continue
				}
			}
		}
		os.Remove(p)
	}
}

// indexName matches index-v<schema>-<sha>.db and its build files
// (.<pid>.tmp, plus SQLite's -journal, -wal or -shm beside it).
var indexName = regexp.MustCompile(`^index-v(\d+)-[^.]+\.db(?:\.(\d+)\.tmp)?(?:-journal|-wal|-shm)?$`)

// Situation is what a hook knows: the repo, the host, and optionally the
// user's prompt or the paths and command in play.
type Situation struct {
	Repo    string
	Host    string
	Query   string
	Paths   []string
	Command string
	// Derived adds the repo's derived/<repo> scope (hot spots, module map).
	// Off by default: derived facts are generated reference material, large
	// next to a ruling, and must reach an agent only when something asks for
	// them. SessionStart never sets it.
	Derived bool
	// OnlyDerived searches the derived/<repo> scope and nothing else, so a
	// caller can rank generated facts on their own and leave the curated
	// ranking untouched.
	OnlyDerived bool
	// Printable leaves out the facts a hook may not print: result facts of
	// type result_ref (numbers nobody has quoted) stay findable by search and
	// the MCP tool, which do not set this.
	Printable bool
}

var tokenRe = regexp.MustCompile(`[\p{L}\p{N}_][\p{L}\p{N}_.-]{1,}`)
var stop = map[string]bool{}

func init() {
	for _, w := range strings.Fields("the and for with that this from into over under about than then there here what when where which who why how use using used should can could would will are was were been being have has had not does did you your our its it's dont don't don doesn isn won please want need like just also") {
		stop[w] = true
	}
}

// Terms turns free text into FTS query tokens: distinct, lowercase, no stop
// words, split on . / - so paths and commands contribute their parts too.
func Terms(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range tokenRe.FindAllString(strings.ToLower(text), -1) {
		for _, piece := range strings.FieldsFunc(t, func(r rune) bool { return r == '.' || r == '/' || r == '-' }) {
			if len([]rune(piece)) < 2 || (len([]rune(piece)) < 3 && !hasNonASCII(piece)) || stop[piece] || seen[piece] {
				continue
			}
			seen[piece] = true
			out = append(out, piece)
		}
		if len(out) >= 24 {
			break
		}
	}
	return out
}

// Query ranks facts for a situation. includeInbox adds unreviewed inbox
// facts (ranked below curated ones at equal score). Superseded facts are
// never returned.
func (ix *Index) Query(ctx context.Context, sit Situation, limit int, includeInbox bool) ([]Hit, error) {
	// The repo is a scope filter, not a query term: a fact that merely names
	// the repo is not more relevant to the situation.
	terms := Terms(strings.Join(append([]string{sit.Query, sit.Command}, sit.Paths...), " "))
	if len(terms) == 0 {
		return nil, nil
	}
	scopes := []string{"system", "user"}
	if sit.Repo != "" {
		scopes = append(scopes, "projects/"+strings.ToLower(sit.Repo))
	}
	if sit.Host != "" {
		scopes = append(scopes, "machines/"+strings.ToLower(sit.Host))
	}
	if sit.OnlyDerived {
		if sit.Repo == "" {
			return nil, nil
		}
		scopes = nil
	}
	if (sit.Derived || sit.OnlyDerived) && sit.Repo != "" {
		scopes = append(scopes, "derived/"+strings.ToLower(sit.Repo))
	}
	// FTS5 OR query; bm25 weights: name 6, description 6, body 1 (descriptions
	// state the fact; bodies carry context that dilutes).
	quoted := make([]string, len(terms))
	for i, t := range terms {
		quoted[i] = `"` + strings.ReplaceAll(t, `"`, "") + `"`
	}
	match := strings.Join(quoted, " OR ")
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(scopes)), ",")
	args := []any{match}
	for _, s := range scopes {
		args = append(args, s)
	}
	inboxClause := "AND f.inbox = 0"
	if includeInbox {
		inboxClause = ""
	}
	if sit.Printable {
		inboxClause += " AND f.type != 'result_ref'"
	}
	q := fmt.Sprintf(`SELECT f.path, f.name, f.scope, f.type, f.description, f.body, f.inbox, f.kind, f.conflicts, f.ruling, f.words, f.local, -bm25(fts, 6.0, 6.0, 1.0) AS score
		FROM fts JOIN facts f ON f.id = fts.rowid
		WHERE fts MATCH ? AND f.superseded = 0 AND f.scope IN (%s) %s
		ORDER BY score DESC LIMIT ?`, placeholders, inboxClause)
	args = append(args, limit*3)
	rows, err := ix.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hits []Hit
	for rows.Next() {
		var h Hit
		var inbox, ruling, words, local int
		var conflicts string
		if err := rows.Scan(&h.Path, &h.Name, &h.Scope, &h.Type, &h.Description, &h.Body, &inbox, &h.Kind, &conflicts, &ruling, &words, &local, &h.Score); err != nil {
			return nil, err
		}
		h.Inbox, h.Ruling, h.Words, h.Local = inbox == 1, ruling == 1, words == 1, local == 1
		if conflicts != "" {
			h.Conflicts = strings.Split(conflicts, ",")
		}
		// Curated facts outrank inbox facts at equal score; a repo or host
		// scope outranks the generic ones a little.
		if h.Inbox {
			h.Score *= 0.8
		}
		if strings.HasPrefix(h.Scope, "projects/") || strings.HasPrefix(h.Scope, "machines/") {
			h.Score *= 1.1
		}
		hits = append(hits, h)
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	hits = dropConflictPartners(hits)
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, rows.Err()
}

// dropConflictPartners never delivers both sides of a known contradiction:
// the higher-ranked side stays and is annotated, the other is dropped.
func dropConflictPartners(hits []Hit) []Hit {
	kept := map[string]bool{}
	var out []Hit
	for _, h := range hits {
		skip := false
		for _, c := range h.Conflicts {
			if kept[c] {
				skip = true
			}
		}
		if skip {
			continue
		}
		kept[h.Name] = true
		out = append(out, h)
	}
	// Also drop a kept fact's partner when the partner declares the conflict.
	for i := 0; i < len(out); i++ {
		for _, c := range out[i].Conflicts {
			for j := i + 1; j < len(out); j++ {
				if out[j].Name == c {
					out = append(out[:j], out[j+1:]...)
					j--
				}
			}
		}
	}
	return out
}

// Block renders hits as the compact context block a hook prints: one line
// per fact, name and description only. Bodies are a tool call away. Owner
// rulings (fact.IsRuling: their own words) are rendered as rules, not as quotes of what the owner
// once said: a dated quote beside "the current request wins" weakens the
// rule (eval/VALUE.md, D7). Known contradictions are marked.
func Block(hits []Hit, title string) string {
	if len(hits) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(title)
	b.WriteString("\n")
	for _, h := range hits {
		desc := strings.Join(strings.Fields(h.Description), " ")
		line := "- " + h.Name
		if h.Ruling {
			line = "- RULE " + h.Name
			desc = asRule(desc)
		}
		if h.Inbox {
			line += " (unreviewed)"
		}
		if h.Local {
			line += " (this machine)"
		}
		if h.Kind == "derived" {
			line += " (computed from git)"
		}
		if len(h.Conflicts) > 0 {
			line += " (contradicted by " + strings.Join(h.Conflicts, ", ") + ", unresolved)"
		}
		b.WriteString(line + ": " + desc + "\n")
	}
	return b.String()
}

// ownerPrefix matches "Owner 2026-09-10:" and the same with a name from
// $TACIT_OWNER_NAMES in place of "Owner"; compiled once per value.
func ownerPrefix() *regexp.Regexp {
	key := os.Getenv("TACIT_OWNER_NAMES")
	ownerPrefixRe.Lock()
	defer ownerPrefixRe.Unlock()
	if ownerPrefixRe.re == nil || ownerPrefixRe.key != key {
		ownerPrefixRe.key = key
		names := strings.Join(append([]string{"owner"}, ownerwords.OwnerNames()...), "|")
		ownerPrefixRe.re = regexp.MustCompile(`(?i)^(` + names + `)('s)?\s*(ruling|rule|said|says|decided|decision|wants|standard)?\s*(\(?\d{4}-\d{2}-\d{2}\)?)?\s*[:,]?\s*`)
	}
	return ownerPrefixRe.re
}

var ownerPrefixRe struct {
	sync.Mutex
	key string
	re  *regexp.Regexp
}
var quoted = regexp.MustCompile(`^['"](.+)['"]$`)

// asRule strips "Owner 2026-09-10:" style preambles and surrounding quotes so
// the line reads as an instruction in force.
func asRule(desc string) string {
	d := ownerPrefix().ReplaceAllString(desc, "")
	if m := quoted.FindStringSubmatch(strings.TrimSpace(d)); m != nil {
		d = m[1]
	}
	d = strings.TrimSpace(d)
	if d == "" {
		return desc
	}
	return strings.ToUpper(d[:1]) + d[1:]
}

func hasNonASCII(s string) bool {
	for _, r := range s {
		if r > 127 {
			return true
		}
	}
	return false
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Ranked is one fact a free-text search found, with its bm25 score.
type Ranked struct {
	Path  string
	Score float64
}

// Rank searches every fact in the index, whatever its scope, inbox or not,
// superseded or retired, for the owner's own search (tacit app): the caller
// filters with keep (nil keeps every fact), and the filter decides the fallback:
// each term matches as a prefix, so a word half typed already finds; all terms
// must match, and only when no fact the caller keeps has them all is any one
// enough. The weights are Query's (name 6, description 6, body 1).
func (ix *Index) Rank(ctx context.Context, text string, limit int, keep func(path string) bool) ([]Ranked, error) {
	terms := Terms(text)
	if len(terms) == 0 {
		return nil, nil
	}
	quoted := make([]string, len(terms))
	for i, t := range terms {
		quoted[i] = `"` + strings.ReplaceAll(t, `"`, "") + `"*`
	}
	// Pages past the first page until limit rows the caller keeps are found
	// or the matches run out, so a filter that drops most of the top hits
	// (inbox notes, retired facts) never leaves the view under-filled.
	q := `SELECT f.path, -bm25(fts, 6.0, 6.0, 1.0) AS score FROM fts JOIN facts f ON f.id = fts.rowid WHERE fts MATCH ? ORDER BY score DESC, f.id LIMIT ? OFFSET ?`
	page := max(limit, 500)
	for _, sep := range []string{" AND ", " OR "} {
		match := strings.Join(quoted, sep)
		var out []Ranked
		for offset := 0; len(out) < limit; offset += page {
			rows, err := ix.db.QueryContext(ctx, q, match, page, offset)
			if err != nil {
				return nil, err
			}
			n := 0
			for rows.Next() {
				n++
				var r Ranked
				if err := rows.Scan(&r.Path, &r.Score); err != nil {
					rows.Close()
					return nil, err
				}
				if (keep == nil || keep(r.Path)) && len(out) < limit {
					out = append(out, r)
				}
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return nil, err
			}
			if n < page {
				break
			}
		}
		if len(out) > 0 || len(terms) == 1 {
			return out, nil
		}
	}
	return nil, nil
}
