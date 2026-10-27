package ownerwords

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/digest"
)

// Real-shaped lines (Claude Code 2.1.30x, Codex 0.16x, as written on the
// author's workstation in 2026-10), texts synthetic.
func typedLine(ts, text string) string {
	b, _ := json.Marshal(text)
	return `{"parentUuid":"ca33","isSidechain":false,"promptSource":"typed","promptId":"64c1","turnOrigin":"human","type":"user","message":{"role":"user","content":` + string(b) + `},"uuid":"4192","timestamp":"` + ts + `","userType":"external","entrypoint":"cli","cwd":"/home/u/projects/shop","sessionId":"own","version":"2.1.302","origin":{"kind":"human"}}`
}

func queuedTyped(ts, text string) string {
	b, _ := json.Marshal(text)
	return `{"parentUuid":"8d5d","isSidechain":false,"attachment":{"type":"queued_command","prompt":` + string(b) + `,"source_uuid":"8064","delivery_id":"c6b7","commandMode":"prompt","origin":{"kind":"human"},"timestamp":"` + ts + `","humanTurn":true},"type":"attachment","uuid":"728b","timestamp":"` + ts + `","entrypoint":"cli","sessionId":"own","version":"2.1.302"}`
}

const note = "Here is a note offered by a side agent:\n\n" +
	"> Heads up · The new plugin lets the session merge with open review findings once the reviewer hits its round limit.\n" +
	">\n" +
	"> **Merging past the review cap**\n" +
	"> - Your rule says every merge waits for a clean review, and the plugin now skips it at the cap.\n" +
	"> - **Consider asking the reviewer for a final round at the cap.**"

// What a side agent's note says is not the owner's; what they typed below it
// is. A reminder Claude Code put in front of their message is the
// harness's; their words after it are theirs.
func TestCorpusSplitsTypedTurns(t *testing.T) {
	home := t.TempDir()
	proj := filepath.Join(home, ".claude", "projects", "-home-u-projects-shop")
	writeFile(t, filepath.Join(proj, "own.jsonl"), strings.Join([]string{
		typedLine("2026-10-25T21:01:30.449Z", note),
		typedLine("2026-10-25T21:02:10.043Z", note+"\n\nas said, the reviewer can be asked by hand, but first run a self review at the cap"),
		queuedTyped("2026-10-13T23:13:33.073Z", "Here is a note offered by a side agent:\n\n> Heads up · The server may restart on its own.\n\ntheres no session running it, but make sure nothing in the system is built to take it up"),
		queuedTyped("2026-09-28T00:19:10.325Z", "<system-reminder>\nThe user started your suggested background task task_c187 in a separate cloud session. It is running independently.\n</system-reminder>\n\nwe are banking our work and releasing boxes, no more shop work for now"),
	}, "\n")+"\n")
	c, err := LoadAll(home, "")
	if err != nil {
		t.Fatal(err)
	}
	for q, want := range map[string]string{
		"the reviewer can be asked by hand":                     "claude:own",
		"make sure nothing in the system is built to take it":   "claude:own",
		"no more shop work for now":                             "claude:own",
		"Your rule says every merge waits for a clean review":   "",
		"Consider asking the reviewer for a final round":        "",
		"The server may restart on its own":                     "",
		"The user started your suggested background task":       "",
		"lets the session merge with open review findings once": "",
	} {
		if m, _ := c.Find(q, "", true); m.Ref != want {
			t.Errorf("Find(%q) = %+v, want %q", q, m, want)
		}
	}
	if got := c.SaidOn("no more shop work for now", "claude:own", ""); got == "" || got > "2026-09-28" {
		t.Errorf("dated by its own turn: %q", got)
	}
	// The reviewer reads the same turn the same way, and keeps the note as
	// what they answered.
	r := &TurnReader{}
	own, other := r.Parts(digest.Claude, []byte(typedLine("2026-10-25T21:02:10.043Z", note+"\n\nits the correct decision")))
	if len(own) != 1 || own[0] != "its the correct decision" || len(other) != 1 || !strings.HasPrefix(other[0], "Here is a note") {
		t.Errorf("reviewer: %q / %q", own, other)
	}
	if own := TurnTexts(digest.Claude, []byte(typedLine("2026-10-25T21:01:30.449Z", note))); own != nil {
		t.Errorf("a note alone: %q", own)
	}
}

// A Codex session Relay moved from Claude Code holds that harness's frames
// and notices: none is theirs. Their answer to an agent's question is, in
// its answer field, unless it is an option the question offered.
func TestCorpusCodexImportsAndAnswers(t *testing.T) {
	home := t.TempDir()
	user := func(text string) string {
		b, _ := json.Marshal(text)
		return `{"timestamp":"2026-10-21T18:59:24.489Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":` + string(b) + `}]}}`
	}
	ask, _ := json.Marshal(`{"questions":[{"title":"Which machine were you using around 24 March?","options":["This Linux machine","A Mac or a cloud container"]}]}`)
	writeFile(t, filepath.Join(home, ".codex", "sessions", "2026", "10", "21", "rollout-a.jsonl"), strings.Join([]string{
		`{"timestamp":"2026-10-21T18:00:00.000Z","type":"session_meta","payload":{"id":"cx1","cwd":"/home/u","originator":"Codex Desktop","source":"vscode","thread_source":"user"}}`,
		user("The user moved this existing Relay conversation to you. Continue its task with the same intent.\n\nUSER: always rebase before every merge"),
		user("<task-notification>\n<task-id>b3</task-id>\n<summary>the lane says deploy on friday</summary>\n</task-notification>"),
		user("This session is being continued from a previous conversation that ran out of context. The owner said keep every lane busy overnight."),
		`{"timestamp":"2026-10-21T18:58:03.993Z","type":"response_item","payload":{"type":"function_call","name":"request_user_input_async","arguments":` + string(ask) + `,"call_id":"call_7c2e"}}`,
		user(`<send_user_message_question_reply>` + "\n" + `[{"answer":"maybe the mac, i keep my old keys there","question":"Which machine were you using around 24 March?","questionItemId":"[\"request_user_input_async\",\"call_7c2e\",0]"}]` + "\n" + `</send_user_message_question_reply>`),
		user(`<send_user_message_question_reply>` + "\n" + `[{"answer":"A Mac or a cloud container","question":"Which machine were you using around 24 March?","questionItemId":"[\"request_user_input_async\",\"call_7c2e\",0]"}]` + "\n" + `</send_user_message_question_reply>`),
	}, "\n")+"\n")
	c, err := LoadAll(home, "")
	if err != nil {
		t.Fatal(err)
	}
	for q, want := range map[string]string{
		"maybe the mac, i keep my old keys there": "codex:cx1",
		"always rebase before every merge":        "",
		"the lane says deploy on friday":          "",
		"keep every lane busy overnight":          "",
		"Which machine were you using around":     "",
		"A Mac or a cloud container":              "",
	} {
		if m, _ := c.Find(q, "", true); m.Ref != want {
			t.Errorf("Find(%q) = %+v, want %q", q, m, want)
		}
	}
}

// Mentions are the newest turns first, with their times.
func TestMentionsNewestFirst(t *testing.T) {
	b := NewBuilder()
	b.AddAt("transcript", "claude:a", "tower crane is being set up today", mustTime("2026-10-06T10:00:00Z"))
	b.AddAt("transcript", "claude:a", "drop tower crane, it is the most fragile project", mustTime("2026-10-25T20:35:29Z"))
	b.AddAt("transcript", "claude:b", "tower crane ships tonight", mustTime("2026-10-20T10:00:00Z"))
	b.Add("digest", "claude:c", "tower crane from a digest with no time")
	c := b.Build()
	ms := c.Mentions("tower-crane", 10)
	if len(ms) != 4 || !strings.HasPrefix(ms[0].Text, "drop tower crane") || ms[0].At.Format("2006-01-02") != "2026-10-25" || ms[1].Ref != "claude:b" || !ms[3].At.IsZero() {
		t.Errorf("mentions: %+v", ms)
	}
}

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}
