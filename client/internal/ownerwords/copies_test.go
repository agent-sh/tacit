package ownerwords

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// claudeTurn is a Claude Code line of a turn a person typed, in session sid.
func claudeTurn(sid, uuid, at, text, extra string) string {
	b, _ := json.Marshal(text)
	return `{"parentUuid":null,"isSidechain":false,"type":"user","message":{"role":"user","content":` + string(b) + `},"uuid":"` + uuid + `","timestamp":"` + at + `","userType":"external","entrypoint":"cli","cwd":"/home/u/projects/shop","sessionId":"` + sid + `","version":"2.1.312","origin":{"kind":"human"},"promptSource":"typed"` + extra + `}`
}

// statsJSON is the corpus's counts as its reports write them.
func statsJSON(t *testing.T, c *Corpus) map[string]any {
	t.Helper()
	b, err := json.Marshal(c.Stats)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// A fork of a Claude Code session copies the turns it carries over, uuid and
// time and all, into its own transcript. The copy is one turn of theirs, in
// one session, not the same words said again in a second one: the note of
// either transcript finds its words in the one session that holds them, the
// one the prompt history says they typed it in, else the first read. No
// other source of the copy's session (its history line, a digest) holds it
// again. What they type in the fork afterwards is a turn of theirs there.
func TestAForkedCopyOfATurnIsOneTurn(t *testing.T) {
	home := t.TempDir()
	proj := filepath.Join(home, ".claude", "projects", "-home-u-projects-shop")
	said := "keep the build cache warm before every release"
	writeFile(t, filepath.Join(proj, "s-orig.jsonl"), claudeTurn("s-orig", "u1", "2026-10-20T10:00:00.000Z", said, "")+"\n")
	writeFile(t, filepath.Join(proj, "s-fork.jsonl"), strings.Join([]string{
		`{"type":"history-suppression","sessionId":"s-fork","cause":"fork_inherit","ts":"2026-10-21T08:00:00.000Z"}`,
		claudeTurn("s-fork", "u1", "2026-10-20T10:00:00.000Z", said, `,"sessionKind":"bg"`),
		claudeTurn("s-fork", "u2", "2026-10-21T08:00:05.000Z", "now tag the release candidate by hand", ""),
	}, "\n")+"\n")
	// A Codex thread resumed into a new rollout carries its messages over
	// with their times; they have no uuid.
	codexSaid := "rotate the signing keys only after the release ships"
	day := filepath.Join(home, ".codex", "sessions", "2026", "10", "21")
	for _, r := range []struct{ file, id, extra string }{
		{"rollout-2026-10-21T09-00-00-cx-a.jsonl", "cx-a", ""},
		{"rollout-2026-10-21T12-00-00-cx-b.jsonl", "cx-b", `{"type":"response_item","timestamp":"2026-10-21T12:00:09.000Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"and keep the changelog in plain words"}]}}`},
	} {
		lines := []string{
			`{"type":"session_meta","payload":{"id":"` + r.id + `","cwd":"/home/u/projects/shop","originator":"codex-tui","source":"cli","thread_source":"user"}}`,
			`{"type":"response_item","timestamp":"2026-10-21T09:00:07.250Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"` + codexSaid + `"}]}}`,
		}
		if r.extra != "" {
			lines = append(lines, r.extra)
		}
		writeFile(t, filepath.Join(day, r.file), strings.Join(lines, "\n")+"\n")
	}
	// The prompt history's line of the turn, in the session it was typed in.
	writeFile(t, filepath.Join(home, ".claude", "history.jsonl"), `{"display":"`+said+`","timestamp":1792490400000,"project":"/home/u/projects/shop","sessionId":"s-orig"}`+"\n")
	c, err := LoadAll(home, "")
	if err != nil {
		t.Fatal(err)
	}
	for words, want := range map[string]string{said: "claude:s-orig", codexSaid: "codex:cx-a"} {
		if got := c.SaidIn(words); len(got) != 1 {
			t.Errorf("SaidIn(%q) = %v, want one session", words, got)
		}
		for _, from := range []string{"s-fork", "s-orig", "cx-a", "cx-b"} {
			if m, _ := c.Find(words, from, true); m.Ref != want {
				t.Errorf("%q from %s's note is in %q, want %s", words, from, m.Ref, want)
			}
		}
	}
	if m, _ := c.Find("tag the release candidate by hand", "", true); m.Ref != "claude:s-fork" {
		t.Errorf("their own turn in the fork: %+v", m)
	}
	if m, _ := c.Find("keep the changelog in plain words", "", true); m.Ref != "codex:cx-b" {
		t.Errorf("their own message in the resumed thread: %+v", m)
	}
	if st := statsJSON(t, c); st["copied_turns"] != 2.0 || st["owner_turns"] != 5.0 || st["broadcast_turns"] != 0.0 {
		t.Errorf("counts: %v", st)
	}
}

// The prompt history's line of a copied turn can hold other text than the
// turn: it cuts a paste's placeholder out, while the transcript keeps a short
// paste in the turn. The turn still stays in the session the history says it
// was typed in, and neither text is in a second session.
func TestACopyStaysWhereItsHistorySaysItWasTyped(t *testing.T) {
	home := t.TempDir()
	proj := filepath.Join(home, ".claude", "projects", "-home-u-projects-shop")
	said := "keep the build cache warm before every release"
	turn := said + " and keep the workers quiet"
	writeFile(t, filepath.Join(proj, "s-orig.jsonl"), claudeTurn("s-orig", "u1", "2026-10-20T10:00:00.000Z", turn, "")+"\n")
	writeFile(t, filepath.Join(proj, "s-fork.jsonl"), strings.Join([]string{
		`{"type":"history-suppression","sessionId":"s-fork","cause":"fork_inherit","ts":"2026-10-21T08:00:00.000Z"}`,
		claudeTurn("s-fork", "u1", "2026-10-20T10:00:00.000Z", turn, `,"sessionKind":"bg"`),
	}, "\n")+"\n")
	writeFile(t, filepath.Join(home, ".claude", "history.jsonl"), `{"display":"`+said+` [Pasted text #1 +1 lines]","pastedContents":{"1":{"id":1,"type":"text","content":"and keep the workers quiet"}},"timestamp":1792490400000,"project":"/home/u/projects/shop","sessionId":"s-orig"}`+"\n")
	c, err := LoadAll(home, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, words := range []string{said, turn} {
		if got := c.SaidIn(words); len(got) != 1 || got[0] != "claude:s-orig" {
			t.Errorf("SaidIn(%q) = %v, want claude:s-orig alone", words, got)
		}
		for _, from := range []string{"s-fork", "s-orig"} {
			if m, _ := c.Find(words, from, true); m.Ref != "claude:s-orig" {
				t.Errorf("%q from %s's note is in %q, want claude:s-orig", words, from, m.Ref)
			}
		}
	}
}

// A copy's history line is found by its text as well as its moment: another
// prompt the fork's session has in the history a second later is the fork's
// own, and neither makes the fork the session the copy was typed in.
func TestACopysHistoryLineIsItsOwnText(t *testing.T) {
	home := t.TempDir()
	proj := filepath.Join(home, ".claude", "projects", "-home-u-projects-shop")
	turn := "move the weekly report to the new dashboard"
	other := "stop the nightly job before the backup window"
	// The fork sorts first, so the first read alone would keep the copy there.
	writeFile(t, filepath.Join(proj, "a-fork.jsonl"), claudeTurn("a-fork", "v1", "2026-10-20T10:00:00.000Z", turn, `,"sessionKind":"bg"`)+"\n")
	writeFile(t, filepath.Join(proj, "b-orig.jsonl"), claudeTurn("b-orig", "v1", "2026-10-20T10:00:00.000Z", turn, "")+"\n")
	writeFile(t, filepath.Join(home, ".claude", "history.jsonl"), strings.Join([]string{
		`{"display":"` + turn + `","timestamp":1792490400000,"project":"/home/u/projects/shop","sessionId":"b-orig"}`,
		`{"display":"` + other + `","timestamp":1792490401000,"project":"/home/u/projects/shop","sessionId":"a-fork"}`,
	}, "\n")+"\n")
	c, err := LoadAll(home, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, from := range []string{"a-fork", "b-orig"} {
		if m, _ := c.Find(turn, from, true); m.Ref != "claude:b-orig" {
			t.Errorf("the copied turn from %s's note is in %q, want claude:b-orig", from, m.Ref)
		}
	}
	if m, _ := c.Find(other, "", true); m.Ref != "claude:a-fork" {
		t.Errorf("the fork's own prompt a second later: %+v", m)
	}
}

// A copy's history line holds the prompt as the person typed it, which the
// corpus cuts as it cuts the turn: a slash command's name, a side agent's
// note above their reply, agent lines they relayed. It still records the
// turn, so the copy stays in the session it was typed in.
func TestACopysHistoryLineIsCutAsTheTurnIs(t *testing.T) {
	for _, c := range []struct{ name, content, display, words string }{
		{"a slash command", "<command-name>/goal</command-name>\n<command-message>goal</command-message>\n<command-args>keep the workers busy all night long</command-args>", "/goal keep the workers busy all night long", "keep the workers busy all night long"},
		{"a side agent's note", "Here is a note offered by a side agent:\n> the build on main is red\n\nfix the build before the release", "Here is a note offered by a side agent:\n> the build on main is red\n\nfix the build before the release", "fix the build before the release"},
		{"relayed agent lines", "please look at the failing build now\n## Findings\n- The cache key is stale", "please look at the failing build now\n## Findings\n- The cache key is stale", "please look at the failing build now"},
	} {
		home := t.TempDir()
		proj := filepath.Join(home, ".claude", "projects", "-home-u-projects-shop")
		writeFile(t, filepath.Join(proj, "a-fork.jsonl"), claudeTurn("a-fork", "w1", "2026-10-20T10:00:00.000Z", c.content, `,"sessionKind":"bg"`)+"\n")
		writeFile(t, filepath.Join(proj, "b-orig.jsonl"), claudeTurn("b-orig", "w1", "2026-10-20T10:00:00.000Z", c.content, "")+"\n")
		display, _ := json.Marshal(c.display)
		writeFile(t, filepath.Join(home, ".claude", "history.jsonl"), `{"display":`+string(display)+`,"timestamp":1792490400000,"project":"/home/u/projects/shop","sessionId":"b-orig"}`+"\n")
		cc, err := LoadAll(home, "")
		if err != nil {
			t.Fatal(err)
		}
		if got := cc.SaidIn(c.words); len(got) != 1 || got[0] != "claude:b-orig" {
			t.Errorf("%s: SaidIn = %v, want claude:b-orig alone", c.name, got)
		}
		for _, from := range []string{"a-fork", "b-orig"} {
			if m, _ := cc.Find(c.words, from, true); m.Ref != "claude:b-orig" {
				t.Errorf("%s: from %s's note the words are in %q, want claude:b-orig", c.name, from, m.Ref)
			}
		}
	}
}

// A Claude Code fork says it is one (its first line, fork_inherit with the
// fork's moment): a turn older than that was carried over. When the
// original's transcript is gone and its prompt history line is not, the
// copy is still one turn, in the session it was typed in.
func TestAForkCarriesTurnsOverWhenTheOriginalIsGone(t *testing.T) {
	home := t.TempDir()
	proj := filepath.Join(home, ".claude", "projects", "-home-u-projects-shop")
	said := "keep the staging database read only this week"
	writeFile(t, filepath.Join(proj, "s-fork.jsonl"), strings.Join([]string{
		`{"type":"history-suppression","sessionId":"s-fork","cause":"fork_inherit","ts":"2026-10-21T08:00:00.000Z"}`,
		claudeTurn("s-fork", "u1", "2026-10-20T10:00:00.000Z", said, `,"sessionKind":"bg"`),
	}, "\n")+"\n")
	writeFile(t, filepath.Join(home, ".claude", "history.jsonl"), `{"display":"`+said+`","timestamp":1792490400000,"project":"/home/u/projects/shop","sessionId":"s-orig"}`+"\n")
	c, err := LoadAll(home, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := c.SaidIn(said); len(got) != 1 || got[0] != "claude:s-orig" {
		t.Errorf("SaidIn = %v, want claude:s-orig alone", got)
	}
	if m, _ := c.Find(said, "s-fork", true); m.Ref != "claude:s-orig" {
		t.Errorf("from the fork's note: %+v", m)
	}
}

// The same text in two sessions at most 2 s apart is a program's broadcast,
// which may type into a person's sessions as it does into its own: no person
// types one message into two sessions at once, so neither is theirs. Only a
// turn with another session's within 2 s is one: the same words typed again
// in one session reach no further. The same words in two sessions half a
// minute apart stay in each session (SaidIn counts word-for-word copies once
// anyway), and a prompt history line that names no session is no second
// session.
func TestTheSameTextInTwoSessionsAtOnceIsNoTurnOfTheirs(t *testing.T) {
	home := t.TempDir()
	proj := filepath.Join(home, ".claude", "projects", "-home-u-projects-shop")
	cast := "the subscription login is restored and a live request passed"
	twice := "run the migration on the staging copy first"
	alone := "drop the old feature flag after the release"
	writeFile(t, filepath.Join(proj, "b-one.jsonl"), claudeTurn("b-one", "c1", "2026-10-20T10:00:00.000Z", cast, "")+"\n"+claudeTurn("b-one", "c3", "2026-10-20T11:00:00.000Z", twice, "")+"\n")
	writeFile(t, filepath.Join(proj, "b-two.jsonl"), claudeTurn("b-two", "c2", "2026-10-20T10:00:00.050Z", cast, "")+"\n"+claudeTurn("b-two", "c4", "2026-10-20T11:00:30.000Z", twice, "")+"\n")
	writeFile(t, filepath.Join(proj, "b-three.jsonl"), claudeTurn("b-three", "c5", "2026-10-20T12:00:00.000Z", alone, "")+"\n")
	again := "rebuild the search index after the import"
	writeFile(t, filepath.Join(proj, "r-a.jsonl"), strings.Join([]string{
		claudeTurn("r-a", "r1", "2026-10-20T13:00:00.000Z", again, ""),
		claudeTurn("r-a", "r2", "2026-10-20T13:00:01.500Z", again, ""),
		claudeTurn("r-a", "r3", "2026-10-20T13:00:03.000Z", again, ""),
	}, "\n")+"\n")
	writeFile(t, filepath.Join(proj, "r-b.jsonl"), claudeTurn("r-b", "r4", "2026-10-20T13:00:04.500Z", again, "")+"\n")
	// A fork's copy of a turn, and then the same text typed into the fork
	// 1.5 s after the original: the copy is a copy, the later turn a
	// broadcast with the original.
	forked := "pause the nightly sync until the move is done"
	writeFile(t, filepath.Join(proj, "f-a.jsonl"), claudeTurn("f-a", "f1", "2026-10-20T14:00:00.000Z", forked, "")+"\n")
	writeFile(t, filepath.Join(proj, "f-b.jsonl"), claudeTurn("f-b", "f1", "2026-10-20T14:00:00.000Z", forked, "")+"\n"+claudeTurn("f-b", "f2", "2026-10-20T14:00:01.500Z", forked, "")+"\n")
	// Two Claude Code turns of one text at one millisecond with their own
	// uuids are two turns, a broadcast, not one turn copied.
	same := "the deploy queue is drained and the proxy restarted"
	writeFile(t, filepath.Join(proj, "m-a.jsonl"), claudeTurn("m-a", "m1", "2026-10-20T15:00:00.000Z", same, "")+"\n")
	writeFile(t, filepath.Join(proj, "m-b.jsonl"), claudeTurn("m-b", "m2", "2026-10-20T15:00:00.000Z", same, "")+"\n")
	// The prompt history's own copy of a turn, with no session named.
	writeFile(t, filepath.Join(home, ".claude", "history.jsonl"), `{"display":"`+alone+`","timestamp":1792497600400,"project":"/home/u/projects/shop"}`+"\n")
	c, err := LoadAll(home, "")
	if err != nil {
		t.Fatal(err)
	}
	if m, ok := c.Find(cast, "", true); ok {
		t.Errorf("a broadcast is in the corpus: %+v", m)
	}
	if !c.FindIn("claude:b-one", twice) || !c.FindIn("claude:b-two", twice) {
		t.Errorf("said in two sessions half a minute apart: theirs in each, as typed")
	}
	if m, _ := c.Find(alone, "b-three", true); m.Ref != "claude:b-three" {
		t.Errorf("a turn and the history's copy of it: %+v", m)
	}
	if !c.FindIn("claude:r-a", again) || c.FindIn("claude:r-b", again) {
		t.Errorf("typed again in one session, then once in another 1.5 s after the last: the first two stay theirs, the other session's goes")
	}
	if m, ok := c.Find(same, "", true); ok {
		t.Errorf("one millisecond, two uuids: %+v", m)
	}
	if c.FindIn("claude:f-a", forked) || c.FindIn("claude:f-b", forked) {
		t.Errorf("a copy and then the same text in its session 1.5 s later: a broadcast, in neither session")
	}
	if st := statsJSON(t, c); st["broadcast_turns"] != 8.0 || st["copied_turns"] != 1.0 {
		t.Errorf("counts: %v", st)
	}
}

// A program's prompt that says it is automated, typed into a person's Codex
// session, is no turn of theirs; their own message in the session is.
func TestAnAutomatedPromptInTheirSessionIsNotTheirs(t *testing.T) {
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".codex", "sessions", "2026", "10", "21", "rollout-2026-10-21T09-00-00-cx-own.jsonl"), strings.Join([]string{
		`{"type":"session_meta","payload":{"id":"cx-own","cwd":"/home/u/projects/shop","originator":"codex-tui","source":"cli","thread_source":"user"}}`,
		`{"type":"response_item","timestamp":"2026-10-21T09:00:05.000Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Automated nightly supervision wake requested by the scheduler. First write the ack file, then check the queue."}]}}`,
		`{"type":"response_item","timestamp":"2026-10-21T09:10:00.000Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"stop the nightly job before the backup window"}]}}`,
	}, "\n")+"\n")
	c, err := LoadAll(home, "")
	if err != nil {
		t.Fatal(err)
	}
	if m, ok := c.Find("supervision wake requested by the scheduler", "", true); ok {
		t.Errorf("the automated prompt is theirs: %+v", m)
	}
	if m, _ := c.Find("stop the nightly job before the backup window", "", true); m.Ref != "codex:cx-own" {
		t.Errorf("their own message: %+v", m)
	}
}
