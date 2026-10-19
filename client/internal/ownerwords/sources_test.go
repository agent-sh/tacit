package ownerwords

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/hermes/hermestest"
)

// pi and Hermes owner sessions count; a headless pi run, a pi subagent, an
// automation Hermes session, a delegation and the frames Hermes puts around
// their words do not.
func TestCorpusReadsPiAndHermesOwnerTurns(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", "")
	pi := func(id, first, text string) string {
		return strings.Join([]string{
			`{"type":"session","version":3,"id":"` + id + `","timestamp":"2026-10-02T10:00:00.000Z","cwd":"/home/u/projects/shop"}`,
			`{"type":"model_change","id":"a1","parentId":null,"timestamp":"2026-10-02T10:00:00.000Z","provider":"p","modelId":"m"}`,
			`{"type":"message","id":"b1","parentId":"a1","timestamp":"` + first + `","message":{"role":"user","content":[{"type":"text","text":"` + text + `"}]}}`,
			`{"type":"message","id":"b2","parentId":"b1","timestamp":"2026-10-02T10:00:20.000Z","message":{"role":"user","content":"<file name=\"notes.md\">attached rules from a file here</file>"}}`,
		}, "\n") + "\n"
	}
	dir := filepath.Join(home, ".pi", "agent", "sessions", "--home-u-projects-shop--")
	writeFile(t, filepath.Join(dir, "2026-10-02T10-00-00-000Z_p1.jsonl"), pi("p1", "2026-10-02T10:00:04.000Z", "never cache the checkout page at the edge"))
	writeFile(t, filepath.Join(dir, "2026-10-02T11-00-00-000Z_p2.jsonl"), pi("p2", "2026-10-02T10:00:00.030Z", "summarize the open pull requests for me"))
	writeFile(t, filepath.Join(dir, "2026-10-02T10-00-00-000Z_p1", "tasks", "2026-10-02T10-05-00-000Z_p3.jsonl"), pi("p3", "2026-10-02T10:00:09.000Z", "audit the payment module for the parent"))

	db := filepath.Join(home, ".hermes", "state.db")
	writeFile(t, db, "")
	st := hermestest.Create(t, db,
		hermestest.Session{ID: "phone", Source: "mobile", StartedAt: 1},
		hermestest.Session{ID: "nightly", Source: "cron", StartedAt: 2},
		hermestest.Session{ID: "sub", Source: "desktop", CreatedSource: "desktop", Parent: "phone", StartedAt: 3},
	)
	st.Add(
		hermestest.Message{Session: "phone", Role: "user", Content: "[Replying to: \"Agent report: rotate the keys monthly\"]\n\nonly rotate the keys after a release", Timestamp: 1},
		hermestest.Message{Session: "phone", Role: "user", Content: "[The user sent a voice message~ Here's what they said: \"book the dentist for thursday morning\"]", Timestamp: 2},
		hermestest.Message{Session: "phone", Role: "user", Content: "[CONTEXT COMPACTION — REFERENCE ONLY] the agent decided to delete old backups", Timestamp: 3},
		hermestest.Message{Session: "nightly", Role: "user", Content: "check every disk on the workstation tonight", Timestamp: 4},
		hermestest.Message{Session: "sub", Role: "user", Content: "implement the approved website copy change", Timestamp: 5},
	)

	c, err := LoadAll(home, "")
	if err != nil {
		t.Fatal(err)
	}
	for q, want := range map[string]string{
		"never cache the checkout page at the edge":  "pi:p1",
		"summarize the open pull requests for me":    "",
		"audit the payment module for the parent":    "",
		"attached rules from a file here":            "",
		"only rotate the keys after a release":       "hermes:phone",
		"book the dentist for thursday morning":      "hermes:phone",
		"rotate the keys monthly":                    "",
		"the agent decided to delete old backups":    "",
		"check every disk on the workstation":        "",
		"implement the approved website copy change": "",
	} {
		m, _ := c.Find(q, "", true)
		if m.Ref != want {
			t.Errorf("Find(%q) = %+v, want %q", q, m, want)
		}
	}
	if c.Stats.Turns["pi"] != 1 || c.Stats.Turns["hermes"] != 2 {
		t.Errorf("turns by source %v", c.Stats.Turns)
	}
	if m, ok := c.Find("only rotate the keys after a release", "phone", true); !ok || m.Ref != "hermes:phone" {
		t.Errorf("session preference: %+v", m)
	}
	t.Setenv("TACIT_HERMES_DB", "off")
	if c, err := LoadAll(home, ""); err != nil || c.Stats.Turns["hermes"] != 0 {
		t.Errorf("hermes off: %v %v", err, c.Stats.Turns)
	}
}
