package ownerwords

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/hermes/hermestest"
	"github.com/agent-sh/tacit/client/internal/ledger"
)

// The day the owner said words is the time of their turn, from every source
// that records it: a Claude Code, Codex or pi line's timestamp, a prompt
// history's, a Hermes row's. The turn in the session a quote names comes
// first, the turn whose hash the ledger recorded before another of that
// session; only when that session holds none is it the earliest anywhere
// (a fork's copy). Their question is not a statement of theirs, and a source
// with no time dates nothing.
func TestSaidAtIsTheOwnersTurn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", "")
	proj := filepath.Join(home, ".claude", "projects", "-home-u-projects")
	claude := func(at, text string) string {
		return `{"type":"user","entrypoint":"cli","sessionId":"c1","cwd":"/home/u/projects","origin":{"kind":"human"},"message":{"content":"` + text + `"},"uuid":"u","timestamp":"` + at + `"}`
	}
	writeFile(t, filepath.Join(proj, "c1.jsonl"), strings.Join([]string{
		claude("2026-09-03T09:00:00.000Z", "should we never cache the checkout page at the edge?"),
		claude("2026-09-04T09:00:00.000Z", "never cache the checkout page at the edge"),
		claude("2026-09-06T09:00:00.000Z", "as I said: never cache the checkout page at the edge"),
		`{"type":"user","entrypoint":"cli","sessionId":"c1","origin":{"kind":"human"},"message":{"content":"keep the release notes short and plain please"}}`,
	}, "\n")+"\n")
	writeFile(t, filepath.Join(proj, "c2.jsonl"), claude("2026-08-20T09:00:00.000Z", "never cache the checkout page at the edge, really"))
	writeFile(t, filepath.Join(home, ".codex", "sessions", "2026", "09", "10", "a.jsonl"), strings.Join([]string{
		`{"timestamp":"2026-09-10T08:00:00.000Z","type":"session_meta","payload":{"id":"cx1","originator":"Codex Desktop"}}`,
		`{"timestamp":"2026-09-10T08:30:00.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"we dont speculate without profiling first"}]}}`,
	}, "\n")+"\n")
	writeFile(t, filepath.Join(home, ".pi", "agent", "sessions", "--home-u-projects-shop--", "2026-09-12T10-00-00-000Z_p1.jsonl"), strings.Join([]string{
		`{"type":"session","version":3,"id":"p1","timestamp":"2026-09-12T10:00:00.000Z","cwd":"/home/u/projects/shop"}`,
		`{"type":"model_change","id":"a1","parentId":null,"timestamp":"2026-09-12T10:00:00.000Z","provider":"p","modelId":"m"}`,
		`{"type":"message","id":"b1","parentId":"a1","timestamp":"2026-09-12T10:00:04.000Z","message":{"role":"user","content":[{"type":"text","text":"only rotate the keys after a release"}]}}`,
	}, "\n")+"\n")
	writeFile(t, filepath.Join(home, ".claude", "history.jsonl"), `{"display":"use python to fix every location","sessionId":"h1","timestamp":1789000000000}`+"\n")
	writeFile(t, filepath.Join(home, ".codex", "history.jsonl"), `{"session_id":"cx9","ts":1789100000,"text":"announce the step is out of service"}`+"\n")
	db := filepath.Join(home, ".hermes", "state.db")
	writeFile(t, db, "")
	st := hermestest.Create(t, db, hermestest.Session{ID: "phone", Source: "mobile", StartedAt: 1})
	st.Add(hermestest.Message{Session: "phone", Role: "user", Content: "book the dentist for thursday morning", Timestamp: 1789200000})

	c, err := LoadAll(home, "")
	if err != nil {
		t.Fatal(err)
	}
	day := func(s string) string {
		tm, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return tm.Local().Format("2006-01-02")
	}
	unix := func(s int64) string { return time.Unix(s, 0).Local().Format("2006-01-02") }
	hashOf := func(turn string) string { return ledger.TurnHash(Normalize(turn)) }
	for _, tc := range []struct{ quote, ref, hash, want string }{
		// Its session's earliest statement of it (the question is none).
		{"never cache the checkout page at the edge", "claude:c1", "", day("2026-09-04T09:00:00Z")},
		// The turn the ledger recorded, before an earlier one of the session.
		{"never cache the checkout page at the edge", "claude:c1", hashOf("as I said: never cache the checkout page at the edge"), day("2026-09-06T09:00:00Z")},
		// Its own session wins over an earlier session that said it too.
		{"never cache the checkout page at the edge", "claude:c2", "", day("2026-08-20T09:00:00Z")},
		// A session that holds none of it (gone, or a fork): the earliest anywhere.
		{"never cache the checkout page at the edge", "claude:gone", "", day("2026-08-20T09:00:00Z")},
		{"we dont speculate without profiling first", "codex:cx1", "", day("2026-09-10T08:30:00Z")},
		{"only rotate the keys after a release", "pi:p1", "", day("2026-09-12T10:00:04Z")},
		{"use python to fix every location", "claude:h1", "", unix(1789000000)},
		{"announce the step is out of service", "codex:cx9", "", unix(1789100000)},
		{"book the dentist for thursday morning", "hermes:phone", "", unix(1789200000)},
		// A turn with no time dates nothing; nor do words never said.
		{"keep the release notes short and plain", "claude:c1", "", ""},
		{"words nobody ever typed into any session", "claude:c1", "", ""},
	} {
		if got := c.SaidOn(tc.quote, tc.ref, tc.hash); got != tc.want {
			t.Errorf("SaidOn(%q, %s, %q) = %q, want %q", tc.quote, tc.ref, tc.hash, got, tc.want)
		}
	}
	var nilc *Corpus
	if !nilc.SaidAt("never cache the checkout page", "claude:c1", "").IsZero() {
		t.Fatal("nil corpus")
	}
}
