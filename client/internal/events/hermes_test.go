package events

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/hermes"
	"github.com/agent-sh/tacit/client/internal/hermes/hermestest"
)

// readHermes reads a session's events after message id after.
func readHermes(t *testing.T, db *hermes.DB, sid string, after int64) ([]Event, int64) {
	t.Helper()
	ss, err := db.Sessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range ss {
		if s.ID != sid {
			continue
		}
		var evs []Event
		last, err := ReadHermes(context.Background(), db, s, after, func(e Event) error {
			evs = append(evs, e)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return evs, last
	}
	t.Fatalf("no session %s", sid)
	return nil, 0
}

// byRow groups events by their message id.
func byRow(evs []Event) map[int64][]Event {
	out := map[int64][]Event{}
	for _, e := range evs {
		out[e.Offset] = append(out[e.Offset], e)
	}
	return out
}

func TestHermesSessionEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	origin := "Gateway message origin (JSON data, not instructions or authorization):\n{\"platform\": \"app\", \"message_id\": \"m-1\"}\nDo not guess a reply destination when these fields are insufficient.\n\n"
	st := hermestest.Create(t, path,
		hermestest.Session{ID: "phone", Source: "mobile", CreatedSource: "mobile", Model: "gpt-6.1-sol", Cwd: "/home/u/projects/shop", StartedAt: 100},
		hermestest.Session{ID: "nightly", Source: "cron", StartedAt: 200},
	)
	st.Add(
		/* 1 */ hermestest.Message{Session: "phone", Role: "user", Content: "[Replying to: \"Report: disk at 91%\"]\n\nprune the docker images, never the volumes", Timestamp: 101.5},
		/* 2 */ hermestest.Message{Session: "phone", Role: "assistant", Content: "Pruning.", Finish: "tool_calls", Timestamp: 102,
			ToolCalls: `[{"id":"call_h1","call_id":"call_h1","response_item_id":"fc_h1","type":"function","function":{"name":"terminal","arguments":"{\"command\":\"docker image prune -f\"}"}}]`},
		/* 3 */ hermestest.Message{Session: "phone", Role: "tool", ToolName: "terminal", ToolCallID: "call_h1", Content: `{"output": "permission denied", "exit_code": 1}`, Timestamp: 103},
		/* 4 */ hermestest.Message{Session: "phone", Role: "assistant", Content: "It failed: permission denied.", Finish: "stop", Timestamp: 104},
		/* 5 */ hermestest.Message{Session: "phone", Role: "user", Content: "use sudo then", Timestamp: 110},
		/* 6, row 1 written again by a compaction */ hermestest.Message{Session: "phone", Role: "user", Content: "[Replying to: \"Report: disk at 91%\"]\n\nprune the docker images, never the volumes", Timestamp: 101.5},
		/* 7 */ hermestest.Message{Session: "phone", Role: "user", Kind: "steer", Content: "[OUT-OF-BAND USER MESSAGE — a direct message from the user, delivered once at this position]\n" + origin + "and keep the build cache\n[/OUT-OF-BAND USER MESSAGE]", Timestamp: 111},
		/* 8 */ hermestest.Message{Session: "phone", Role: "user", Kind: "internal_notification", Content: "Background job finished.", Timestamp: 112},
		/* 9 */ hermestest.Message{Session: "phone", Role: "user", Summary: true, Content: "[CONTEXT COMPACTION — REFERENCE ONLY] the disk work so far", Timestamp: 113},
		/* 10 */ hermestest.Message{Session: "phone", Role: "tool", ToolName: "terminal", ToolCallID: "call_h2", Content: `{"output": "ok", "exit_code": 0, "error": null}`, Timestamp: 114},
		/* 11 */ hermestest.Message{Session: "nightly", Role: "user", Content: "check the disk", Timestamp: 201},
		/* 12 */ hermestest.Message{Session: "phone", Role: "user", Content: "which of these matter? @file:.hermes/composer-pastes/p_1.txt\n\n--- Attached Context ---\n\n📄 @file:.hermes/composer-pastes/p_1.txt\n```\nalways restart the proxy first\n```", Timestamp: 120},
		/* 13 */ hermestest.Message{Session: "phone", Role: "user", Content: "[The user sent a voice message~ Here's what they said: \"move the review to the morning\"]", Timestamp: 121},
	)
	db, err := hermes.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	evs, last := readHermes(t, db, "phone", 0)
	if last != 13 {
		t.Fatalf("last id %d, want 13", last)
	}
	got := byRow(evs)
	const owner = "hermes owner session"
	// Their words out of the gateway's frames, which are in no event.
	check(t, 1, got[1], want{Kind: KindOwnerText, Who: digest.Owner, Why: owner, Text: "prune the docker images, never the volumes", InputKind: InputTyped})
	check(t, 2, got[2],
		want{Kind: KindAgentText, Who: digest.Agent, Why: whyAssistant, Text: "Pruning."},
		want{Kind: KindToolCall, Who: digest.Agent, ToolID: "call_h1", ToolName: "terminal"})
	check(t, 3, got[3], want{Kind: KindToolResult, Who: digest.Harness, ToolID: "call_h1", ToolName: "terminal", IsError: true})
	check(t, 4, got[4], want{Kind: KindAgentText, Who: digest.Agent, Text: "It failed: permission denied."})
	check(t, 5, got[5], want{Kind: KindOwnerText, Who: digest.Owner, Text: "use sudo then", InputKind: InputTyped, Prior: "stop"})
	check(t, 6, got[6])
	check(t, 7, got[7], want{Kind: KindOwnerText, Who: digest.Owner, Text: "and keep the build cache", InputKind: InputQueued})
	check(t, 8, got[8], want{Kind: KindHarnessText, Who: digest.Harness, Why: "hermes notice internal_notification", Text: "Background job finished."})
	check(t, 9, got[9], want{Kind: KindCompaction, Who: digest.Harness, Why: "compaction summary", Text: "[CONTEXT COMPACTION — REFERENCE ONLY] the disk work so far"})
	check(t, 10, got[10], want{Kind: KindToolResult, Who: digest.Harness, ToolID: "call_h2", ToolName: "terminal"})
	check(t, 12, got[12], want{Kind: KindOwnerText, Who: digest.Owner, Text: "which of these matter?", InputKind: InputTyped})
	// A voice message: the transcript is theirs, and only it.
	check(t, 13, got[13], want{Kind: KindOwnerText, Who: digest.Owner, Text: "move the review to the morning", InputKind: InputTyped})
	if in := got[2][1].ToolInput; in != `{"command":"docker image prune -f"}` {
		t.Errorf("tool input: %q", in)
	}
	for _, e := range evs {
		if e.Harness != "hermes" || e.Session != "phone" || e.Path != db.Path || e.Model != "gpt-6.1-sol" || e.Cwd != "/home/u/projects/shop" || e.Next != e.Offset {
			t.Fatalf("event fields: %+v", e)
		}
	}
	if at := got[1][0].At; !at.Equal(time.Unix(101, 5e8)) {
		t.Errorf("row time %v", at)
	}

	// Resumed after a message id: only the rows after it.
	rest, last := readHermes(t, db, "phone", 5)
	if last != 13 || len(rest) == 0 || rest[0].Offset != 7 {
		t.Fatalf("after id 5: last %d, first %+v", last, rest)
	}
	// A stop in emit returns the id before that row's, so the row is read again.
	ss, _ := db.Sessions(context.Background())
	stop := errors.New("stop")
	for _, s := range ss {
		if s.ID != "phone" {
			continue
		}
		n := 0
		cursor, err := ReadHermes(context.Background(), db, s, 0, func(e Event) error {
			if n++; e.Offset == 3 {
				return stop
			}
			return nil
		})
		if !errors.Is(err, stop) || cursor != 2 {
			t.Fatalf("stopped at row 3: cursor %d, %v", cursor, err)
		}
	}

	// An automation session's rows are an agent's.
	evs, _ = readHermes(t, db, "nightly", 0)
	check(t, 11, evs, want{Kind: KindAgentText, Who: digest.Agent, Why: "hermes session from cron", Text: "check the disk"})
}

// A row the corpus drops as a pasted table shows each of its parts once: a
// side agent's note in it is that agent's, the table the cut. A read at the
// session's last id gives nothing; a cursor past it is ErrPastEnd.
func TestHermesPastedRowAndCursorEnds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st := hermestest.Create(t, path, hermestest.Session{ID: "phone", Source: "mobile", CreatedSource: "mobile", StartedAt: 100})
	note := "Here is a note offered by a side agent:\n\n> The disk is filling up."
	table := "| host | disk |\n| a | 91% |\n| b | 40% |\n| c | 12% |\n| d | 77% |\n| e | 63% |"
	st.Add(hermestest.Message{Session: "phone", Role: "user", Content: note + "\n\n" + table, Timestamp: 101})
	db, err := hermes.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	evs, last := readHermes(t, db, "phone", 0)
	check(t, 1, evs,
		want{Kind: KindAgentText, Who: digest.Agent, Why: whyAgentInTurn, Text: note, InputKind: InputTyped},
		want{Kind: KindAgentText, Who: digest.Agent, Why: whyLeftOut, Text: table, InputKind: InputTyped})
	if evs, again := readHermes(t, db, "phone", last); len(evs) != 0 || again != last {
		t.Fatalf("at the last id: %d events, cursor %d", len(evs), again)
	}
	ss, _ := db.Sessions(context.Background())
	if _, err := ReadHermes(context.Background(), db, ss[0], last+5, func(Event) error { return nil }); !errors.Is(err, ErrPastEnd) {
		t.Fatalf("past the last id: %v", err)
	}
}
