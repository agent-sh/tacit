// Package hermes reads Hermes's session store (~/.hermes/state.db, SQLite)
// for the dreamer and the owner's words. The store belongs to a running
// gateway: it is opened read-only, with a short busy timeout, and nothing is
// ever written to it.
package hermes

import (
	"context"
	"database/sql"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/agent-sh/tacit/client/internal/digest"

	_ "modernc.org/sqlite"
)

// OwnerSources are the sources of sessions a person types into: the phone
// app, Telegram, the desktop app, the CLI, the TUI and the web UI. Every other
// source is automation: cron, oneshot (tacit's own Hermes runner calls are
// oneshot), subagent, grok-cli, kanban, or a scheduled job's own name.
var OwnerSources = map[string]bool{"mobile": true, "telegram": true, "desktop": true, "cli": true, "tui": true, "webui": true}

// continued are the end reasons of a session whose conversation goes on in a
// child session: Hermes compacted it, or the owner reset it. Any other child
// of a session is an agent's (a delegation, a subagent).
var continued = map[string]bool{"compression": true, "session_reset": true}

// Continued reports an end reason whose conversation goes on in a child
// session (compression, session_reset): the agent there no longer holds the
// parent's turns in its context.
func Continued(endReason string) bool { return continued[endReason] }

// fanOut is how close together, in seconds, sessions start when an agent
// starts them: a delegation's subagents began within one second of each other.
const fanOut = 2.0

// DefaultDB is the store the dreamer and the owner's words read: the path in
// $TACIT_HERMES_DB (config hermes_db), else ~/.hermes/state.db when it
// exists; "" when there is none or the setting is "off".
func DefaultDB(home string) string {
	if v, ok := os.LookupEnv("TACIT_HERMES_DB"); ok {
		if v == "" || strings.EqualFold(v, "off") {
			return ""
		}
		return v
	}
	p := filepath.Join(home, ".hermes", "state.db")
	if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
		return p
	}
	return ""
}

// DB is a read-only connection to a Hermes store.
type DB struct {
	Path string
	db   *sql.DB
	// has are the columns read only when the store has them: an older Hermes
	// writes no model, tool call id or finish reason.
	has map[string]bool
}

// optional are the columns DB reads when its store has them, as
// "<table>.<column>".
var optional = []string{"sessions.model", "messages.tool_call_id", "messages.finish_reason"}

// Open opens the store read-only (SQLite's mode=ro, query_only) with a busy
// timeout of 2 s: a gateway writing it never waits on tacit, and tacit gives
// up rather than hang. It fails when the file is missing.
func Open(path string) (*DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(abs); err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs), RawQuery: "mode=ro&_busy_timeout=2000&_query_only=1"}
	if !strings.HasPrefix(u.Path, "/") {
		u.Path = "/" + u.Path // a Windows drive path
	}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	d := &DB{Path: abs, db: db, has: map[string]bool{}}
	for _, tc := range optional {
		table, col, _ := strings.Cut(tc, ".")
		var n int
		if err := db.QueryRow(`select count(*) from pragma_table_info(?) where name = ?`, table, col).Scan(&n); err != nil {
			db.Close()
			return nil, err
		}
		d.has[tc] = n > 0
	}
	return d, nil
}

// column is the expression that reads an optional column of table (named
// as in the query, as): the column as text, or an empty string when the
// store has none.
func (d *DB) column(table, as, col string) string {
	if d.has[table+"."+col] {
		return "coalesce(" + as + "." + col + ", '')"
	}
	return "''"
}

// Close closes the connection.
func (d *DB) Close() error { return d.db.Close() }

// Session is one row of the sessions table.
type Session struct {
	ID, Source, CreatedSource, Parent, EndReason, Cwd, RepoRoot string
	// Model is the model the session runs ("" when the store records none).
	Model     string
	StartedAt float64
	// Delegated is a session Hermes started for a delegation: its
	// model_config names the session it came from ("_delegate_from"). Hermes
	// marks every delegation so, from Telegram, the desktop app or a phone
	// subagent, whether or not it records a parent.
	Delegated bool
	// Owner is a session the owner types into: an owner source, not created
	// by an agent (created_source empty or an owner source); not a
	// delegation (Delegated); a CLI session only when created_source says
	// cli; not one of several started together; and, when it has a parent,
	// the continuation of an owner session (the parent ended by compression
	// or a reset). On a real store each rule caught sessions the others let
	// in: phone sessions that are subagents' (created_source subagent),
	// desktop ones that are delegations from a phone session (created_source
	// desktop, the parent still open), Telegram sessions from an older Hermes
	// that are a delegation's fan-out with no parent recorded (two or three
	// started within a second, one prompt each), and CLI sessions with no
	// created source that are scripted one-query runs (`hermes chat -q`,
	// nearly all a single prompt such as "Reply with exactly: OK"). Every
	// child of an owner session that ended by compression or a reset
	// continues its conversation. A few lone Telegram delegations with no
	// parent and no created source pass the other rules; only the delegation
	// marker tells them apart.
	Owner bool
}

// Sessions lists every session, oldest first, with Owner decided.
func (d *DB) Sessions(ctx context.Context) ([]Session, error) {
	rows, err := d.db.QueryContext(ctx, `select id, source, coalesce(created_source, ''), coalesce(parent_session_id, ''), coalesce(end_reason, ''), coalesce(cwd, ''), coalesce(git_repo_root, ''), `+d.column("sessions", "sessions", "model")+`, started_at,
		json_extract(case when json_valid(model_config) then model_config else '{}' end, '$._delegate_from') is not null
		from sessions order by started_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var s Session
		if err := rows.Scan(&s.ID, &s.Source, &s.CreatedSource, &s.Parent, &s.EndReason, &s.Cwd, &s.RepoRoot, &s.Model, &s.StartedAt, &s.Delegated); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	byID := make(map[string]int, len(out))
	for i, s := range out {
		byID[s.ID] = i
	}
	// Sessions of owner sources started within fanOut of each other: a person
	// opens one at a time.
	together := map[string]bool{}
	var prev *Session
	for i := range out {
		s := &out[i]
		if !OwnerSources[s.Source] {
			continue
		}
		if prev != nil && s.StartedAt-prev.StartedAt < fanOut {
			together[s.ID], together[prev.ID] = true, true
		}
		prev = s
	}
	memo := map[string]bool{}
	var owner func(i, depth int) bool
	owner = func(i, depth int) bool {
		s := out[i]
		if v, ok := memo[s.ID]; ok {
			return v
		}
		v := OwnerSources[s.Source] && (s.CreatedSource == "" || OwnerSources[s.CreatedSource]) && !together[s.ID] && !s.Delegated
		if s.Source == "cli" && s.CreatedSource != "cli" {
			v = false
		}
		if v && s.Parent != "" {
			p, ok := byID[s.Parent]
			v = ok && depth < 64 && continued[out[p].EndReason] && owner(p, depth+1)
		}
		memo[s.ID] = v
		return v
	}
	for i := range out {
		out[i].Owner = owner(i, 0)
	}
	return out, nil
}

// Message is one row of the messages table with its id.
type Message struct {
	ID        int64
	Timestamp float64
	digest.HermesMessage
	// ToolCallID is the call a tool row answers: the id of one of the
	// tool_calls of an assistant row before it.
	ToolCallID string
	// Finish is an assistant row's finish reason (stop, tool_calls, length,
	// incomplete, ...).
	Finish string
	// Copy is a row that repeats an earlier row of the session (same role,
	// time and content): Hermes writes the kept messages again when it
	// compacts a conversation, under new ids.
	Copy bool
}

// LastIDs is every session's last message id.
func (d *DB) LastIDs(ctx context.Context) (map[string]int64, error) {
	rows, err := d.db.QueryContext(ctx, `select session_id, max(id) from messages group by session_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var sid string
		var id int64
		if err := rows.Scan(&sid, &id); err != nil {
			return nil, err
		}
		out[sid] = id
	}
	return out, rows.Err()
}

// LastTimes is every session's newest message time (Unix seconds): when the
// session was last written.
func (d *DB) LastTimes(ctx context.Context) (map[string]float64, error) {
	rows, err := d.db.QueryContext(ctx, `select session_id, max(timestamp) from messages group by session_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]float64{}
	for rows.Next() {
		var sid string
		var at sql.NullFloat64
		if err := rows.Scan(&sid, &at); err != nil {
			return nil, err
		}
		out[sid] = at.Float64
	}
	return out, rows.Err()
}

// LastID is a session's newest message id (0 when it has none).
func (d *DB) LastID(ctx context.Context, sid string) (int64, error) {
	var id sql.NullInt64
	err := d.db.QueryRowContext(ctx, `select max(id) from messages where session_id = ?`, sid).Scan(&id)
	return id.Int64, err
}

// NewBytes is the content bytes a session holds after message id after.
func (d *DB) NewBytes(ctx context.Context, sid string, after int64) (int64, error) {
	var n sql.NullInt64
	err := d.db.QueryRowContext(ctx, `select sum(length(coalesce(content, '')) + length(coalesce(tool_calls, ''))) from messages where session_id = ? and id > ?`, sid, after).Scan(&n)
	return n.Int64, err
}

// Messages reads a session's rows after message id after, in id order, until
// they hold maxBytes of content (at least one row).
func (d *DB) Messages(ctx context.Context, sid string, after, maxBytes int64) ([]Message, error) {
	return d.messages(ctx, `session_id = ? and id > ?`, maxBytes, sid, after)
}

// UserMessages reads every user row of a session, in id order.
func (d *DB) UserMessages(ctx context.Context, sid string) ([]Message, error) {
	return d.messages(ctx, `session_id = ? and role = 'user'`, math.MaxInt64, sid)
}

// messages reads rows where cond holds. Each row is marked Copy when an
// earlier row of the session has its role, time and content (and tool calls).
func (d *DB) messages(ctx context.Context, cond string, maxBytes int64, args ...any) ([]Message, error) {
	rows, err := d.db.QueryContext(ctx, `select id, role, coalesce(content, ''), coalesce(tool_calls, ''), coalesce(tool_name, ''), coalesce(display_kind, ''), coalesce(_compressed_summary, 0), timestamp,
		`+d.column("messages", "m", "tool_call_id")+`, `+d.column("messages", "m", "finish_reason")+`,
		exists(select 1 from messages p where p.session_id = m.session_id and p.timestamp = m.timestamp and p.id < m.id and p.role = m.role and coalesce(p.content, '') = coalesce(m.content, '') and coalesce(p.tool_calls, '') = coalesce(m.tool_calls, ''))
		from messages m where `+cond+` order by id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	var size int64
	for rows.Next() {
		var m Message
		var summary, copied int
		if err := rows.Scan(&m.ID, &m.Role, &m.Content, &m.ToolCalls, &m.ToolName, &m.DisplayKind, &summary, &m.Timestamp, &m.ToolCallID, &m.Finish, &copied); err != nil {
			return nil, err
		}
		m.Summary, m.Copy = summary != 0, copied != 0
		out = append(out, m)
		size += int64(len(m.Content) + len(m.ToolCalls))
		if size >= maxBytes {
			break
		}
	}
	return out, rows.Err()
}

// Key is the dreamer's cursor key for a session: "hermes:<db path>#<id>".
// Its value in the cursor file is the last message id read.
func Key(db, sid string) string { return "hermes:" + db + "#" + sid }

// ParseKey splits a cursor key; ok is false for any other key.
func ParseKey(key string) (db, sid string, ok bool) {
	rest, ok := strings.CutPrefix(key, "hermes:")
	if !ok {
		return "", "", false
	}
	i := strings.LastIndexByte(rest, '#')
	if i <= 0 || i == len(rest)-1 {
		return "", "", false
	}
	return rest[:i], rest[i+1:], true
}
