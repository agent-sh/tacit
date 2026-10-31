package digest

import (
	"encoding/json"
	"strings"
	"testing"
)

// Real-shaped lines, as Claude Code 2.1.30x and Codex 0.16x write them
// (2026-10), texts synthetic.

// claudeTyped is a user line a person typed in a terminal session.
func claudeTyped(text string) string {
	b, _ := json.Marshal(text)
	return `{"parentUuid":"ca33","isSidechain":false,"promptSource":"typed","promptId":"64c1","turnOrigin":"human","type":"user","message":{"role":"user","content":` + string(b) + `},"uuid":"4192","timestamp":"2026-10-23T12:52:42.499Z","permissionMode":"bypassPermissions","userType":"external","entrypoint":"cli","cwd":"/home/u/projects/shop","sessionId":"s1","version":"2.1.302","gitBranch":"main","origin":{"kind":"human"}}`
}

// claudeQueuedTyped is a message a person typed while the agent worked.
func claudeQueuedTyped(text string) string {
	b, _ := json.Marshal(text)
	return queuedLine("2.1.302", `{"type":"queued_command","prompt":`+string(b)+`,"source_uuid":"8064","delivery_id":"c6b7","commandMode":"prompt","origin":{"kind":"human"},"timestamp":"2026-10-13T23:13:33.073Z","humanTurn":true}`, false)
}

// A note a side agent offers, as Claude Code delivers it: the header, the
// body quoted line by line, and what the person typed in reply below it.
const sideNoteBody = "Here is a note offered by a side agent:\n\n" +
	"> Heads up · A deploy script keeps restarting the build every few minutes, so one stale lock file triggered about 25 builds.\n" +
	">\n" +
	"> **The build loop restarts on a lock change**\n" +
	"> - The deploy script asks for a new build on every push.\n" +
	"> - **Consider capping build retries per change.**"

func TestTurnPiecesSideAgentNote(t *testing.T) {
	reply := "theres no need to rebuild on every lock change, and the agents should tell you when the deploy cant finish"
	ps := TurnPieces(sideNoteBody + "\n\n" + reply)
	if len(ps) != 2 || ps[0].Who != Agent || !strings.HasPrefix(ps[0].Text, "Here is a note") || !strings.Contains(ps[0].Text, "capping build retries") || ps[1].Who != Owner || ps[1].Text != reply {
		t.Fatalf("note with a reply: %+v", ps)
	}
	if own := OwnerPieces(TurnPieces(sideNoteBody)); own != nil {
		t.Errorf("a note with no reply holds nothing of theirs: %q", own)
	}
	// A copy the person pasted from the terminal: the prompt mark and the
	// quote's indent.
	pasted := "❯ Here is a note offered by a side agent:\n\n  > Heads up · The main agent shipped build 74 without the lint step.\n  >\n  > Build 74 shipped without the lint step\n  > - Consider adding it to the deploy script."
	if ps := TurnPieces(pasted); len(ps) != 1 || ps[0].Who != Agent {
		t.Errorf("pasted note: %+v", ps)
	}
	// An old digest line, its newlines collapsed: the note whole, nothing of theirs.
	if own := OwnerPieces(TurnPieces(strings.Join(strings.Fields(sideNoteBody+"\n\nok"), " "))); own != nil {
		t.Errorf("collapsed note: %q", own)
	}
	// The words are the harness marker only at the start of the turn.
	plain := "i saw the line here is a note offered by a side agent: in the log, ignore it"
	if own := OwnerPieces(TurnPieces(plain)); len(own) != 1 || own[0] != plain {
		t.Errorf("the words inside their own sentence: %q", own)
	}
}

func TestTurnPiecesHarnessBlocks(t *testing.T) {
	// A message typed while the agent worked, after the reminder Claude Code
	// put in front of it.
	q := "<system-reminder>\nThe user started your suggested background task task_c187 (\"Fix the render\") in a separate cloud session. It is running independently.\n</system-reminder>\n\nwe are done with the migration for today, no more schema work until monday"
	ps := TurnPieces(q)
	if len(ps) != 2 || ps[0].Who != Harness || ps[1].Who != Owner || ps[1].Text != "we are done with the migration for today, no more schema work until monday" {
		t.Errorf("leading reminder: %+v", ps)
	}
	// A reminder after their words, and one alone.
	if own := OwnerPieces(TurnPieces("keep the cap at 50\n<system-reminder>\nGoal check-in\n</system-reminder>")); len(own) != 1 || own[0] != "keep the cap at 50" {
		t.Errorf("trailing reminder: %q", own)
	}
	if own := OwnerPieces(TurnPieces("<system-reminder>\nnothing else\n</system-reminder>")); own != nil {
		t.Errorf("a reminder alone: %q", own)
	}
	// A reminder that does not close is a frame: nothing of theirs.
	if own := OwnerPieces(TurnPieces("<system-reminder>\nno end of it, then words")); own != nil {
		t.Errorf("open reminder: %q", own)
	}
	// A slash command's echo: its arguments are theirs.
	ps = TurnPieces("<command-message>goal</command-message>\n<command-name>/goal</command-name>\n<command-args>keep going and finish the table</command-args>")
	if own := OwnerPieces(ps); len(own) != 1 || own[0] != "keep going and finish the table" || ps[0].Who != Harness || strings.Contains(ps[0].Text, "keep going") {
		t.Errorf("command echo: %+v", ps)
	}
	for _, s := range []string{
		"<task-notification>\n<task-id>b3ah</task-id>\n<status>completed</status>\n</task-notification>",
		"<codex_delegation>\n  <input>lane coordination</input>\n</codex_delegation>",
		"<recommended_plugins>\nHere is a list of plugins\n</recommended_plugins>",
		"This session is being continued from a previous conversation that ran out of context.",
		"The user moved this existing Relay conversation to you. Continue its task with the same intent.",
		"[Request interrupted by user for tool use]",
		"You are lead-01 of the team",
	} {
		if own := OwnerPieces(TurnPieces(s)); own != nil {
			t.Errorf("%.40q: %q", s, own)
		}
	}
}

// codexReply is a Codex user message replying to an agent's questions.
func codexReply(items string) string {
	b, _ := json.Marshal("<send_user_message_question_reply>\n" + items + "\n</send_user_message_question_reply>")
	return `{"timestamp":"2026-10-21T18:59:24.489Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":` + string(b) + `}]}}`
}

// codexAsk is the request_user_input call that asked them.
func codexAsk(call, args string) string {
	b, _ := json.Marshal(args)
	return `{"timestamp":"2026-10-21T18:58:03.993Z","ordinal":417,"type":"response_item","payload":{"type":"function_call","status":"completed","id":"fc_0e4a","name":"request_user_input_async","arguments":` + string(b) + `,"call_id":"` + call + `"},"metadata":{"client_authored":false}}`
}

func TestCodexQuestionReplies(t *testing.T) {
	asked := CodexAsked{}
	var o map[string]any
	_ = json.Unmarshal([]byte(codexAsk("call_7c2e", `{"questions":[{"title":"Which branch did you cut the release from?","options":["main","release/1.x"]},{"title":"Is the old build still deployed?","options":["Yes","No"]}]}`)), &o)
	asked.Note(o)
	reply := `[{"answer":"maybe the release branch, i dont remember","question":"Which branch did you cut the release from?","questionItemId":"[\"request_user_input_async\",\"call_7c2e\",0]"},{"answer":"No","question":"Is the old build still deployed?","questionItemId":"[\"request_user_input_async\",\"call_7c2e\",1]"}]`
	var text string
	var line map[string]any
	_ = json.Unmarshal([]byte(codexReply(reply)), &line)
	text = line["payload"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	ps := asked.Chosen(TurnPieces(text))
	if own := OwnerPieces(ps); len(own) != 1 || own[0] != "maybe the release branch, i dont remember" {
		t.Errorf("typed answer theirs, offered option not: %+v", ps)
	}
	// Without the question (a window that starts after it) the option counts as their answer.
	if own := OwnerPieces(CodexAsked(nil).Chosen(TurnPieces(text))); len(own) != 2 {
		t.Errorf("no question seen: %q", own)
	}
	// The digest of the Codex session: the answers, the frames and the
	// notices of a conversation a front end moved here from Claude Code.
	in := codexMeta("Codex Desktop", `"vscode"`, "user", "") + strings.Join([]string{
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"The user moved this existing Relay conversation to you. Continue its task with the same intent.\n\nUSER: fix it"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<task-notification>\n<task-id>b3</task-id>\n<status>completed</status>\n</task-notification>"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"This session is being continued from a previous conversation that ran out of context."}]}}`,
		codexAsk("call_7c2e", `{"questions":[{"title":"Which branch?","options":["main","release/1.x"]},{"title":"Still deployed?","options":["Yes","No"]}]}`),
		codexReply(reply),
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"check the march pushes first"}]}}`,
	}, "\n") + "\n"
	got := Lines(strings.NewReader(in), Codex, SessionWho("r.jsonl", []byte(in), Codex).Owner())
	var us []string
	for _, l := range got {
		if strings.HasPrefix(l, "U: ") {
			us = append(us, l)
		}
	}
	if strings.Join(us, "|") != "U: maybe the release branch, i dont remember|U: check the march pushes first" {
		t.Errorf("codex owner lines:\n%s", strings.Join(got, "\n"))
	}
}

func TestLinesSplitTypedTurns(t *testing.T) {
	assistant := `{"type":"assistant","entrypoint":"cli","message":{"content":[{"type":"text","text":"Noted."}]}}`
	in := strings.Join([]string{
		claudeTyped(sideNoteBody),
		assistant,
		claudeTyped(sideNoteBody + "\n\nits the correct decision"),
		claudeQueuedTyped("<system-reminder>\nThe user started your suggested background task task_c1 in a separate cloud session.\n</system-reminder>\n\nno more shop work for now"),
		claudeTyped("<command-message>goal</command-message>\n<command-name>/goal</command-name>\n<command-args>keep the build green</command-args>"),
		claudeTyped("[Request interrupted by user]"),
		assistant,
	}, "\n") + "\n"
	got := Lines(strings.NewReader(in), Claude, true)
	var kinds []string
	for _, l := range got {
		kinds = append(kinds, l[:1])
	}
	if strings.Join(kinds, "") != "PAPUHUHUHA" {
		t.Errorf("digest:\n%s", strings.Join(got, "\n"))
	}
	for _, want := range []string{"U: its the correct decision", "U: no more shop work for now", "U: keep the build green"} {
		found := false
		for _, l := range got {
			found = found || l == want
		}
		if !found {
			t.Errorf("missing %q in\n%s", want, strings.Join(got, "\n"))
		}
	}
	// In an agent's session nothing changes: its prompts are P: or H: whole.
	if got := Lines(strings.NewReader(claudeTyped("<context>the task</context> do it")+"\n"), Claude, false); len(got) != 1 || !strings.HasPrefix(got[0], "P: <context>") {
		t.Errorf("agent prompt: %q", got)
	}
}

// A paste the desktop app framed is pasted text, not words of theirs: an
// agent's (P: in the digest), the words they typed around it theirs. A wake
// loop's prompt, which a program types into a person's session, is the
// harness's.
func TestTurnPiecesPastesAndWakePrompts(t *testing.T) {
	paste := `<pasted_content id="1">build failed: the cache key is stale</pasted_content>`
	ps := TurnPieces("read this " + paste + " and fix the build")
	if len(ps) != 2 || ps[0].Who != Agent || ps[0].Text != paste || ps[1].Who != Owner || strings.Join(strings.Fields(ps[1].Text), " ") != "read this and fix the build" {
		t.Fatalf("paste in a turn: %+v", ps)
	}
	if own := OwnerPieces(TurnPieces(paste)); own != nil {
		t.Errorf("a paste alone holds nothing of theirs: %q", own)
	}
	lines := Lines(strings.NewReader(claudeTyped("read this "+paste+" and fix the build")+"\n"), Claude, true)
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "P: <pasted_content") || !strings.HasPrefix(lines[1], "U: read this") || strings.Contains(lines[1], "cache key") {
		t.Errorf("digest of a turn with a paste: %q", lines)
	}
	for _, wake := range []string{"Wake nonce=7f3a9c: check the queue and report back", "  Wake nonce=1\nstill there?",
		"Automated nightly supervision wake requested by the scheduler. First write the ack file, then check the queue."} {
		if ps := TurnPieces(wake); len(ps) != 1 || ps[0].Who != Harness {
			t.Errorf("wake prompt %q: %+v", wake, ps)
		}
	}
	for _, said := range []string{"the wake nonce= line in the log is the loop, ignore it", "automated tests fail on the cache path again", "Automated tests must pass before any merge to main"} {
		if own := OwnerPieces(TurnPieces(said)); len(own) != 1 {
			t.Errorf("the words inside their own sentence %q: %q", said, own)
		}
	}
}
