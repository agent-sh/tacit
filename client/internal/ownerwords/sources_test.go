package ownerwords

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/harnesses"
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

	// The same words in an earlier Claude Code session: a quote names its
	// session, so the pi or Hermes session is the match, not this one.
	claude := func(text string) string {
		return `{"type":"user","entrypoint":"cli","sessionId":"c1","cwd":"/home/u/projects","origin":{"kind":"human"},"message":{"content":"` + text + `"}}` + "\n"
	}
	writeFile(t, filepath.Join(home, ".claude", "projects", "-home-u-projects", "c1.jsonl"), claude("never cache the checkout page at the edge")+claude("only rotate the keys after a release"))

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
		hermestest.Message{Session: "phone", Role: "user", Content: "@file:.hermes/composer-pastes/pasted_content_1.txt\n\n--- Attached Context ---\n\n📄 @file:.hermes/composer-pastes/pasted_content_1.txt\n```\nalways restart the proxy before the cache\n```", Timestamp: 6},
		hermestest.Message{Session: "phone", Role: "user", Content: "<file name=\"plan.md\">the agent works in the payments repo</file>", Timestamp: 7},
		hermestest.Message{Session: "phone", Role: "user", Content: "| host | disk |\n| a | 91% |\n| b | 40% |\n| c | 12% |\n| d | 77% |\n| e | 63% |", Timestamp: 8},
		hermestest.Message{Session: "phone", Role: "user", Content: "[Replying to: \"Agent report: rotate the keys monthly\"]\n\nonly rotate the keys after a release", Timestamp: 1}, // a compaction copy
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
		"always restart the proxy before the cache":  "",
		"the agent works in the payments repo":       "",
		"| a | 91% |":                                "",
	} {
		_, session, _ := strings.Cut(want, ":")
		m, _ := c.Find(q, session, true)
		if m.Ref != want {
			t.Errorf("Find(%q) = %+v, want %q", q, m, want)
		}
	}
	if c.Stats.Turns["pi"] != 1 || c.Stats.Turns["hermes"] != 2 {
		t.Errorf("turns by source %v", c.Stats.Turns)
	}
	if m, ok := c.Find("only rotate the keys after a release", "", true); !ok || m.Ref != "claude:c1" {
		t.Errorf("no session named: %+v", m)
	}
	// A store that exists and cannot be read fails the corpus, as an
	// unreadable transcript does: a corpus without it would miss turns.
	bad := filepath.Join(home, "bad.db")
	writeFile(t, bad, "this is not a sqlite database, only text that fills its first page. "+strings.Repeat("x", 200))
	t.Setenv("TACIT_HERMES_DB", bad)
	if _, err := LoadAll(home, ""); err == nil || !strings.Contains(err.Error(), "hermes") {
		t.Errorf("unreadable store: %v", err)
	}
	t.Setenv("TACIT_HERMES_DB", "off")
	if c, err := LoadAll(home, ""); err != nil || c.Stats.Turns["hermes"] != 0 {
		t.Errorf("hermes off: %v %v", err, c.Stats.Turns)
	}
}

// HermesTurn reads a row as the corpus does: their words out of the
// gateway's frames, nothing of a notice, a pasted table or a tag-framed row.
func TestHermesTurnReadsARowAsTheCorpusDoes(t *testing.T) {
	for _, c := range []struct {
		content, kind, want string
	}{
		{"[Replying to: \"Agent report: rotate the keys monthly\"]\n\nonly rotate the keys after a release", "", "only rotate the keys after a release"},
		{"[OUT-OF-BAND USER MESSAGE — a direct message from the user, delivered once at this position]\nand keep the build cache\n[/OUT-OF-BAND USER MESSAGE]", "steer", "and keep the build cache"},
		{"Background job finished.", "internal_notification", ""},
		{"| host | disk |\n| a | 91% |\n| b | 40% |\n| c | 12% |\n| d | 77% |\n| e | 63% |", "", ""},
		{"<file name=\"plan.md\">the agent works in the payments repo</file>", "", ""},
		{"check the queue\n## Findings\n- **The queue is full.**", "", "check the queue"},
	} {
		got, ok := HermesTurn(c.content, c.kind, false)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("HermesTurn(%.40q) = %q, %v; want %q", c.content, got, ok, c.want)
		}
	}
}

// The corpus reads the top-level sources of the one list (harnesses.Sources),
// the same globs as before it.
func TestDefaultSourcesAreTheTopLevelSources(t *testing.T) {
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", "")
	home := "/home/u"
	want := []Source{
		{Glob: filepath.Join(home, ".claude", "projects", "*", "*.jsonl"), Format: digest.Claude},
		{Glob: filepath.Join(home, ".codex", "sessions", "*", "*", "*", "*.jsonl"), Format: digest.Codex},
		{Glob: filepath.Join(home, ".codex", "sessions", "*", "*.jsonl"), Format: digest.Codex},
		{Glob: filepath.Join(home, ".codex", "archived_sessions", "*.jsonl"), Format: digest.Codex},
		{Glob: filepath.Join(home, ".pi", "agent", "sessions", "*", "*.jsonl"), Format: digest.Pi},
	}
	var top []Source
	for _, s := range harnesses.TopLevel(harnesses.Sources(home)) {
		if s.Format != digest.Hermes {
			top = append(top, Source{Glob: s.Glob, Format: s.Format})
		}
	}
	if got := DefaultSources(home); !reflect.DeepEqual(got, want) || !reflect.DeepEqual(got, top) {
		t.Fatalf("DefaultSources:\n%v\nwant\n%v", got, want)
	}
}
