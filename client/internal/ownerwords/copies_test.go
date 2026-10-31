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
// either transcript finds its words in the one session that holds them.
// What they type in the fork afterwards is a turn of theirs there.
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
	c, err := LoadAll(home, "")
	if err != nil {
		t.Fatal(err)
	}
	for words, sessions := range map[string][2]string{said: {"s-fork", "s-orig"}, codexSaid: {"cx-a", "cx-b"}} {
		if got := c.SaidIn(words); len(got) != 1 {
			t.Errorf("SaidIn(%q) = %v, want one session", words, got)
		}
		one, _ := c.Find(words, sessions[0], true)
		two, _ := c.Find(words, sessions[1], true)
		if one.Ref == "" || one.Ref != two.Ref {
			t.Errorf("%q is in %q from %s's note and in %q from %s's, want one session", words, one.Ref, sessions[0], two.Ref, sessions[1])
		}
	}
	if m, _ := c.Find("tag the release candidate by hand", "", true); m.Ref != "claude:s-fork" {
		t.Errorf("their own turn in the fork: %+v", m)
	}
	if m, _ := c.Find("keep the changelog in plain words", "", true); m.Ref != "codex:cx-b" {
		t.Errorf("their own message in the resumed thread: %+v", m)
	}
	if st := statsJSON(t, c); st["copied_turns"] != 2.0 || st["owner_turns"] != 4.0 || st["broadcast_turns"] != 0.0 {
		t.Errorf("counts: %v", st)
	}
}

// The same text in two sessions at most 2 s apart is a program's broadcast,
// which may type into a person's sessions as it does into its own: no person
// types one message into two sessions at once, so neither is theirs. The
// same words in two sessions half a minute apart stay in each session (SaidIn
// counts word-for-word copies once anyway), and a prompt history line that
// names no session is no second session.
func TestTheSameTextInTwoSessionsAtOnceIsNoTurnOfTheirs(t *testing.T) {
	home := t.TempDir()
	proj := filepath.Join(home, ".claude", "projects", "-home-u-projects-shop")
	cast := "the subscription login is restored and a live request passed"
	twice := "run the migration on the staging copy first"
	alone := "drop the old feature flag after the release"
	writeFile(t, filepath.Join(proj, "b-one.jsonl"), claudeTurn("b-one", "c1", "2026-10-20T10:00:00.000Z", cast, "")+"\n"+claudeTurn("b-one", "c3", "2026-10-20T11:00:00.000Z", twice, "")+"\n")
	writeFile(t, filepath.Join(proj, "b-two.jsonl"), claudeTurn("b-two", "c2", "2026-10-20T10:00:00.050Z", cast, "")+"\n"+claudeTurn("b-two", "c4", "2026-10-20T11:00:30.000Z", twice, "")+"\n")
	writeFile(t, filepath.Join(proj, "b-three.jsonl"), claudeTurn("b-three", "c5", "2026-10-20T12:00:00.000Z", alone, "")+"\n")
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
	if st := statsJSON(t, c); st["broadcast_turns"] != 2.0 {
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
