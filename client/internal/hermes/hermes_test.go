package hermes

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/agent-sh/tacit/client/internal/hermes/hermestest"
)

func TestSessionsTellTheOwnersFromAutomation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	hermestest.Create(t, path,
		hermestest.Session{ID: "phone", Source: "mobile", CreatedSource: "mobile", StartedAt: 100},
		hermestest.Session{ID: "chat", Source: "telegram", EndReason: "compression", StartedAt: 200},
		hermestest.Session{ID: "chat-2", Source: "telegram", Parent: "chat", StartedAt: 300},
		hermestest.Session{ID: "nightly", Source: "cron", StartedAt: 400},
		hermestest.Session{ID: "runner", Source: "oneshot", StartedAt: 500},
		hermestest.Session{ID: "helper", Source: "mobile", CreatedSource: "subagent", Parent: "phone", StartedAt: 600},
		hermestest.Session{ID: "delegated", Source: "desktop", CreatedSource: "desktop", Parent: "phone", StartedAt: 700},
		hermestest.Session{ID: "after-cron", Source: "webui", Parent: "nightly", StartedAt: 800},
		hermestest.Session{ID: "fan-1", Source: "telegram", StartedAt: 900},
		hermestest.Session{ID: "fan-2", Source: "telegram", StartedAt: 900.6},
		hermestest.Session{ID: "terminal", Source: "cli", CreatedSource: "cli", StartedAt: 1000},
		hermestest.Session{ID: "query", Source: "cli", StartedAt: 1100},
	)
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ss, err := d.Sessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"phone": true, "chat": true, "chat-2": true, "terminal": true}
	for _, s := range ss {
		if s.Owner != want[s.ID] {
			t.Errorf("%s: owner %v, want %v", s.ID, s.Owner, want[s.ID])
		}
	}
}

func TestMessagesMarkCompactionCopiesAndNeverWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st := hermestest.Create(t, path, hermestest.Session{ID: "s", Source: "cli", StartedAt: 1})
	st.Add(
		hermestest.Message{Session: "s", Role: "user", Content: "rotate the logs weekly", Timestamp: 10},
		hermestest.Message{Session: "s", Role: "assistant", Content: "Done.", Timestamp: 11},
		hermestest.Message{Session: "s", Role: "user", Content: "rotate the logs weekly", Timestamp: 10}, // compaction re-insert
		hermestest.Message{Session: "s", Role: "user", Content: "rotate the logs weekly", Timestamp: 12}, // said again
	)
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ms, err := d.Messages(context.Background(), "s", 0, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 4 || ms[0].Copy || ms[1].Copy || !ms[2].Copy || ms[3].Copy {
		t.Fatalf("copies: %+v", ms)
	}
	if ms, _ := d.Messages(context.Background(), "s", ms[1].ID, 1); len(ms) != 1 || ms[0].ID != 3 {
		t.Fatalf("after id 2, capped at one row: %+v", ms)
	}
	us, _ := d.UserMessages(context.Background(), "s")
	if len(us) != 3 {
		t.Fatalf("user rows %d", len(us))
	}
	if _, err := d.db.Exec(`insert into sessions (id, source, started_at) values ('x', 'cli', 1)`); err == nil {
		t.Fatal("the store accepted a write")
	}
	if _, err := Open(filepath.Join(t.TempDir(), "missing.db")); err == nil {
		t.Fatal("opened a missing store")
	}
}

func TestKey(t *testing.T) {
	k := Key("/home/u/.hermes/state.db", "20261001_101010_ab12")
	db, sid, ok := ParseKey(k)
	if !ok || db != "/home/u/.hermes/state.db" || sid != "20261001_101010_ab12" {
		t.Fatalf("%q -> %q %q %v", k, db, sid, ok)
	}
	if _, _, ok := ParseKey("/home/u/.claude/projects/p/s.jsonl"); ok {
		t.Fatal("a file path parsed as a Hermes key")
	}
}

func TestDefaultDB(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TACIT_HERMES_DB", "off")
	if DefaultDB(home) != "" {
		t.Fatal("off still names a store")
	}
	t.Setenv("TACIT_HERMES_DB", "/srv/h.db")
	if DefaultDB(home) != "/srv/h.db" {
		t.Fatal("setting ignored")
	}
}
