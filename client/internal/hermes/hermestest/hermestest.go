// Package hermestest writes synthetic Hermes session stores for tests: the
// columns of ~/.hermes/state.db that tacit reads, nothing else.
package hermestest

import (
	"database/sql"
	"encoding/json"
	"testing"

	_ "modernc.org/sqlite"
)

const schema = `create table sessions (id text primary key, source text not null, created_source text, model text, model_config text, parent_session_id text, started_at real not null, ended_at real, end_reason text, cwd text, git_repo_root text, title text);
create table messages (id integer primary key autoincrement, session_id text not null references sessions(id), role text not null, content text, tool_call_id text, tool_calls text, tool_name text, timestamp real not null, finish_reason text, _compressed_summary integer default 0, display_kind text);
create index idx_messages_session on messages(session_id, timestamp);`

// Session is a sessions row. DelegateFrom, when set, is the session a
// delegation came from, in model_config's "_delegate_from" (as Hermes marks
// one); ModelConfig, when set, is model_config as written.
type Session struct {
	ID, Source, CreatedSource, Parent, EndReason, Cwd string
	DelegateFrom, ModelConfig, Model                  string
	StartedAt                                         float64
}

// Message is a messages row; ID 0 lets SQLite number it. ToolCallID is the
// call a tool row answers, Finish an assistant row's finish reason.
type Message struct {
	ID                                                int64
	Session, Role, Content, ToolCalls, ToolName, Kind string
	ToolCallID, Finish                                string
	Timestamp                                         float64
	Summary                                           bool
}

// Store is a writable synthetic store.
type Store struct {
	t  testing.TB
	db *sql.DB
}

// Create makes a store at path with the given sessions.
func Create(t testing.TB, path string, sessions ...Session) *Store {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	s := &Store{t: t, db: db}
	for _, se := range sessions {
		s.AddSession(se)
	}
	return s
}

// AddSession inserts a session.
func (s *Store) AddSession(se Session) {
	s.t.Helper()
	null := func(v string) any {
		if v == "" {
			return nil
		}
		return v
	}
	config := se.ModelConfig
	if se.DelegateFrom != "" {
		b, _ := json.Marshal(map[string]string{"model": "m", "_delegate_from": se.DelegateFrom})
		config = string(b)
	}
	if _, err := s.db.Exec(`insert into sessions (id, source, created_source, model, model_config, parent_session_id, end_reason, cwd, started_at) values (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		se.ID, se.Source, null(se.CreatedSource), null(se.Model), null(config), null(se.Parent), null(se.EndReason), null(se.Cwd), se.StartedAt); err != nil {
		s.t.Fatal(err)
	}
}

// Add inserts messages in order.
func (s *Store) Add(ms ...Message) {
	s.t.Helper()
	for _, m := range ms {
		var id any
		if m.ID != 0 {
			id = m.ID
		}
		null := func(v string) any {
			if v == "" {
				return nil
			}
			return v
		}
		summary := 0
		if m.Summary {
			summary = 1
		}
		if _, err := s.db.Exec(`insert into messages (id, session_id, role, content, tool_calls, tool_name, tool_call_id, finish_reason, display_kind, _compressed_summary, timestamp) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, m.Session, m.Role, m.Content, null(m.ToolCalls), null(m.ToolName), null(m.ToolCallID), null(m.Finish), null(m.Kind), summary, m.Timestamp); err != nil {
			s.t.Fatal(err)
		}
	}
}

// End sets a session's end reason, as Hermes does when it ends one
// (compression, session_reset, ...).
func (s *Store) End(id, reason string) {
	s.t.Helper()
	if _, err := s.db.Exec(`update sessions set end_reason = ? where id = ?`, reason, id); err != nil {
		s.t.Fatal(err)
	}
}
