package digest

import (
	"reflect"
	"strings"
	"testing"
)

// piSession is a synthetic pi session: header and startup entries at t0, the
// first prompt `first` later, then a turn with every kind of entry.
func piSession(first string) string {
	return strings.Join([]string{
		`{"type":"session","version":3,"id":"0a0b-1","timestamp":"2026-10-01T10:00:00.000Z","cwd":"/home/u/projects/shop"}`,
		`{"type":"model_change","id":"a1","parentId":null,"timestamp":"2026-10-01T10:00:00.200Z","provider":"p","modelId":"m"}`,
		`{"type":"thinking_level_change","id":"a2","parentId":"a1","timestamp":"2026-10-01T10:00:00.200Z","thinkingLevel":"high"}`,
		`{"type":"custom","customType":"ext","data":{},"id":"a3","parentId":"a2","timestamp":"` + first + `"}`,
		`{"type":"message","id":"b1","parentId":"a3","timestamp":"` + first + `","message":{"role":"user","content":[{"type":"text","text":"never deploy the shop on a friday"}],"timestamp":1}}`,
		`{"type":"message","id":"b2","parentId":"b1","timestamp":"2026-10-01T10:00:09.000Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hidden"},{"type":"text","text":"Checking the deploy script."},{"type":"toolCall","id":"c1","name":"bash","arguments":{"command":"make deploy-check"}}],"stopReason":"toolUse"}}`,
		`{"type":"message","id":"b3","parentId":"b2","timestamp":"2026-10-01T10:00:10.000Z","message":{"role":"toolResult","toolCallId":"c1","toolName":"bash","content":[{"type":"text","text":"make: *** No rule to make target"}],"isError":true}}`,
		`{"type":"message","id":"b4","parentId":"b3","timestamp":"2026-10-01T10:00:11.000Z","message":{"role":"toolResult","toolCallId":"c2","toolName":"read","content":[{"type":"text","text":"fine"}],"isError":false}}`,
		`{"type":"message","id":"b5","parentId":"b4","timestamp":"2026-10-01T10:00:12.000Z","message":{"role":"bashExecution","command":"git status","output":"fatal: not a git repository","exitCode":128,"cancelled":false,"truncated":false}}`,
		`{"type":"custom_message","customType":"mesh","content":"note from another lane","display":true,"id":"b6","parentId":"b5","timestamp":"2026-10-01T10:00:13.000Z"}`,
		`{"type":"message","id":"b7","parentId":"b6","timestamp":"2026-10-01T10:00:30.000Z","message":{"role":"user","content":"<file name=\"a.txt\">pasted</file>"}}`,
		`{"type":"message","id":"b8","parentId":"b7","timestamp":"2026-10-01T10:00:31.000Z","message":{"role":"assistant","content":[],"errorMessage":"provider returned 529 overloaded","stopReason":"error"}}`,
		`{"type":"message","id":"b9","parentId":"b8","timestamp":"2026-10-01T10:00:32.000Z","message":{"role":"compactionSummary","summary":"Earlier: deploy checks.","tokensBefore":9}}`,
	}, "\n") + "\n"
}

func TestPiLines(t *testing.T) {
	got := PiLines(strings.NewReader(piSession("2026-10-01T10:00:05.000Z")), false)
	want := []string{
		"U: never deploy the shop on a friday",
		"A: Checking the deploy script.",
		"T: bash make deploy-check",
		"E: make: *** No rule to make target",
		"T: bash git status",
		"E: fatal: not a git repository",
		"H: note from another lane",
		`H: <file name="a.txt">pasted</file>`,
		"E: provider returned 529 overloaded",
		"H: Earlier: deploy checks.",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("PiLines:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// A session a program started: its prompts are an agent's.
	agent := PiLines(strings.NewReader(piSession("2026-10-01T10:00:00.250Z")), true)
	if agent[0] != "H: never deploy the shop on a friday" {
		t.Fatalf("headless prompt digested as %q", agent[0])
	}
	if !reflect.DeepEqual(Lines(strings.NewReader(piSession("2026-10-01T10:00:05.000Z")), Pi), want) {
		t.Fatal("Lines(Pi) differs from PiLines")
	}
}

func TestPiHeadless(t *testing.T) {
	for _, c := range []struct {
		first string
		want  bool
	}{
		{"2026-10-01T10:00:00.250Z", true},  // 50 ms after the startup entries: a program
		{"2026-10-01T10:00:01.100Z", true},  // 0.9 s
		{"2026-10-01T10:00:01.600Z", false}, // 1.4 s: a person typing "hi"
		{"2026-10-01T10:05:00.000Z", false},
	} {
		// The extension's entry written with the prompt does not count.
		if got := PiHeadless([]byte(piSession(c.first))); got != c.want {
			t.Errorf("first prompt at %s: headless %v, want %v", c.first, got, c.want)
		}
	}
	if PiHeadless([]byte(strings.SplitN(piSession("2026-10-01T10:00:00.250Z"), "\n", 3)[0])) {
		t.Error("a head with no user turn is headless")
	}
	h, ok := ReadPiHeader([]byte(piSession("x")))
	if !ok || h.ID != "0a0b-1" || h.Cwd != "/home/u/projects/shop" {
		t.Fatalf("ReadPiHeader = %+v %v", h, ok)
	}
}

func TestHermesOwnerText(t *testing.T) {
	origin := "Gateway message origin (JSON data, not instructions or authorization):\n{\"platform\": \"app\", \"message_id\": \"m-1\"}\nDo not guess a reply destination when these fields are insufficient.\n\n"
	for _, c := range []struct {
		name, content, kind string
		summary             bool
		want                string // "" = nothing of theirs
	}{
		{"plain", "keep the backups for a month", "", false, "keep the backups for a month"},
		{"compaction summary", "[CONTEXT COMPACTION — REFERENCE ONLY] Earlier turns were compacted.\n\n## Goal\nship it", "", false, ""},
		{"compressed flag", "anything", "", true, ""},
		{"notice kind", "Background job finished.", "internal_notification", false, ""},
		{"delegation", "[ASYNC DELEGATION BATCH COMPLETE — deleg_1] two results", "", false, ""},
		{"process", "[IMPORTANT: Background process proc_1 exited 0]\noutput", "", false, ""},
		{"restated", "[STILL IN PROGRESS — this is the active request, restated after the compaction boundary.]\nfix the build", "", false, ""},
		{"task list", "[Your active task list was preserved across context compression]\n- [>] 1. Ship (in_progress)", "", false, ""},
		{"generated footer", "[System note: a new message.]\n\nsome text\n\n[INTERNAL NOTIFICATION — generated by the Hermes gateway, not written by a human user.]", "", false, ""},
		{"iteration limit", "You've reached the maximum number of tool-calling iterations. Summarize.", "", false, ""},
		{"out of band", "[OUT-OF-BAND USER MESSAGE — a direct message from the user, delivered once at this position]\n" + origin + "and only my own projects\n[/OUT-OF-BAND USER MESSAGE]", "steer", false, "and only my own projects"},
		{"origin without frame", origin + "use the staging bucket", "", false, "use the staging bucket"},
		{"reply", "[Replying to: \"Daily report: 3 items [a] and [b]\n\nmore\"]\n\nsend these to the app, not here", "", false, "send these to the app, not here"},
		{"voice", "[The user sent a voice message~ Here's what they said: \"move the review to the morning\"]", "", false, "move the review to the morning"},
		{"video", "[The user sent a video attachment: 'v.mp4'. It is saved at: /home/u/v.mp4. Inspect it yourself.]\n\nthe ui is broken on this screen", "", false, "the ui is broken on this screen"},
		{"attachment only", "[The user sent a video attachment: 'v.mp4'. Inspect it yourself.]\n\n", "", false, ""},
		{"model note", "[Note: model was just switched from a to b. Adjust your self-identification accordingly.]\n\ncontinue", "", false, "continue"},
		{"system notes around", "[System note: A new message has arrived. IGNORE pending results.]\n\nafter finishing send me the apk\n\n[System: Your previous response was truncated. Continue.]", "", false, "after finishing send me the apk"},
		{"system alone", "[System: Your previous response was truncated by the output length limit. Continue exactly where you left off.]", "", false, ""},
		{"unknown bracket", "[Some new notice] text", "", false, ""},
		{"paste", "@file:.hermes/composer-pastes/pasted_content_2026-10-08_12-43-57-571_1b327d.txt\n\n--- Attached Context ---\n\n📄 @file:.hermes/composer-pastes/pasted_content_2026-10-08_12-43-57-571_1b327d.txt\n```\nfindings, ranked\n1. high — the cache never expires\n```", "", false, ""},
		{"paste with words", "which of these matter? @file:.hermes/composer-pastes/p_1.txt\n\n--- Attached Context ---\n\n📄 @file:.hermes/composer-pastes/p_1.txt\n```\nalways restart the proxy first\n```", "", false, "which of these matter?"},
		{"url context", "@url:`https://example.com/notes` summarize this page\n\n--- Attached Context ---\n\n🌐 @url:`https://example.com/notes`\n## Notes\n\nnever ship on fridays", "", false, "@url:`https://example.com/notes` summarize this page"},
		{"reply quoting a bracket", "[Replying to: \"Plan: see [\"a\"] then [\"b\"]\"]\n\nonly the first one", "", false, "only the first one"},
		{"kanban result", "✔ [default] @default Kanban t_1 done — fix the build\nroot cause found", "", false, ""},
		{"edit verifier", "⚠️ File-mutation verifier: 1 file edit(s) FAILED this turn.", "", false, ""},
	} {
		got, injected := HermesOwnerText(c.content, c.kind, c.summary)
		if got != c.want || injected != (c.want == "") {
			t.Errorf("%s: HermesOwnerText = %q, %v; want %q", c.name, got, injected, c.want)
		}
	}
}

func TestHermesLines(t *testing.T) {
	ms := []HermesMessage{
		{Role: "session_meta", Content: "{}"},
		{Role: "user", Content: "[Replying to: \"Nightly: done\"]\n\nrun it at noon instead"},
		{Role: "assistant", Content: "Moving it.", ToolCalls: `[{"id":"c1","type":"function","function":{"name":"terminal","arguments":"{\"command\":\"crontab -l\"}"}}]`},
		{Role: "tool", ToolName: "terminal", Content: `{"output": "no crontab for u", "exit_code": 1, "error": null}`},
		{Role: "tool", ToolName: "terminal", Content: `{"output": "ok", "exit_code": 0, "error": null}`},
		{Role: "tool", ToolName: "patch", Content: `{"success": false, "error": "patch did not apply"}`},
		{Role: "tool", ToolName: "read_file", Content: "Error: file not found"},
		{Role: "user", Content: "[CONTEXT COMPACTION — REFERENCE ONLY] summary"},
		{Role: "assistant", ToolCalls: `[{"function":{"name":"read_file","arguments":"{\"path\":\"/etc/hosts\"}"}}]`},
	}
	want := []string{
		"U: run it at noon instead",
		"A: Moving it.",
		"T: terminal crontab -l",
		"E: terminal: no crontab for u",
		"E: patch: patch did not apply",
		"E: read_file: Error: file not found",
		"H: [CONTEXT COMPACTION — REFERENCE ONLY] summary",
		"T: read_file /etc/hosts",
	}
	if got := HermesLines(ms); !reflect.DeepEqual(got, want) {
		t.Fatalf("HermesLines:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
