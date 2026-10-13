package ownerwords

import (
	"path/filepath"
	"strings"
	"testing"
)

// Each way an agent's text could pass for theirs, from a review of the corpus:
// a turn another agent sent (origin.kind peer, task-notification), a session
// whose lines switch to a headless entrypoint, a paste the owner passed
// along (kept apart in the prompt history), an orchestrator's role prompt, and
// a probe started in /tmp.
func TestCorpusKeepsAgentsTextOut(t *testing.T) {
	home := t.TempDir()
	proj := filepath.Join(home, ".claude", "projects", "-home-u-projects")
	writeFile(t, filepath.Join(proj, "s1.jsonl"), strings.Join([]string{
		`{"type":"user","entrypoint":"cli","cwd":"/home/u/projects","origin":{"kind":"human"},"message":{"content":"please keep every hook quiet at night"}}`,
		`{"type":"user","entrypoint":"cli","origin":{"kind":"peer","from":"a1"},"message":{"content":"Build is still running with eight cargo processes now"}}`,
		`{"type":"user","entrypoint":"cli","origin":{"kind":"task-notification"},"message":{"content":"the subagent finished the migration of every lane"}}`,
		`{"type":"user","entrypoint":"sdk-cli","message":{"content":"Your working directory is the worker tree, merge the lane"}}`,
		`{"type":"user","entrypoint":"cli","origin":{"host":"github.com","remote":"x/y"},"message":{"content":"read the release notes before you tag anything"}}`,
		`{"type":"user","entrypoint":"cli","message":{"content":"look at this: Three tests fail on the upload path, which I cannot reproduce here"}}`,
	}, "\n")+"\n")
	writeFile(t, filepath.Join(home, ".claude", "history.jsonl"), `{"display":"look at this: [Pasted text #1 +3 lines]","sessionId":"s1","pastedContents":{"1":{"id":1,"type":"text","content":"Three tests fail on the upload path, which I cannot reproduce here"}}}`+"\n")
	writeFile(t, filepath.Join(home, ".claude", "projects", "-home-u-projects-web-app", "m.jsonl"),
		`{"type":"user","entrypoint":"claude-desktop","cwd":"/home/u/projects/web-app","message":{"content":"You are coordinator-7 for the Atlas project in Forge. Start every worker now"}}`+"\n")
	writeFile(t, filepath.Join(home, ".claude", "projects", "-tmp", "q.jsonl"),
		`{"type":"user","entrypoint":"claude-desktop","cwd":"/tmp","message":{"content":"Answer with one word, ping, and stop right there"}}`+"\n")
	writeFile(t, filepath.Join(home, ".codex", "sessions", "2026", "10", "02", "c.jsonl"), strings.Join([]string{
		`{"type":"session_meta","payload":{"id":"cx5","originator":"Codex Desktop"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"| cell | tok/s |\n|---|---|\n| a | 1 |\n| b | 2 |\n| c | 3 |\n| cache warmup already matches on both hosts |"}]}}`,
	}, "\n")+"\n")
	writeFile(t, filepath.Join(home, ".claude", "projects", "-tmp-tmpab12", "p.jsonl"),
		`{"type":"user","entrypoint":"claude-desktop","cwd":"/tmp/tmpab12","message":{"content":"Answer with one word, done, and nothing more please"}}`+"\n")

	c, err := LoadAll(home, "")
	if err != nil {
		t.Fatal(err)
	}
	for q, want := range map[string]bool{
		"please keep every hook quiet at night":       true,
		"look at this":                                false, // too short to look up at all
		"Build is still running with eight cargo":     false,
		"the subagent finished the migration":         false,
		"Your working directory is the worker tree":   false,
		"read the release notes before you tag":       true, // a repo origin is not an agent's turn
		"Three tests fail on the upload path, which":  false,
		"You are coordinator-7 for the Atlas project": false,
		"Answer with one word, done, and nothing":     false,
		"Answer with one word, ping, and stop":        false,
		"cache warmup already matches on both hosts":  false,
	} {
		if _, got := c.Find(q, "", true); got != want {
			t.Errorf("Find(%q) = %v, want %v", q, got, want)
		}
	}
}

// A phrase of theirs proves nothing when it is their question or follows their
// negation; a short phrase is theirs but too few words to prove a rule.
func TestMatchContextAndProof(t *testing.T) {
	b := NewBuilder()
	b.Add("transcript", "claude:s1", "pick any stack you like. which one would you start with?")
	b.Add("transcript", "claude:s2", "never pin the old parser for this release, it drifts")
	b.Add("transcript", "claude:s3", "we dont need to rebuild the images on each commit. what do you think?")
	c := b.Build()
	if m, ok := c.Find("which one would you start with", "", true); !ok || !m.Question || m.Clean() {
		t.Fatalf("their question: %+v %v", m, ok)
	}
	if m, ok := c.Find("pin the old parser for this release", "", true); !ok || !m.Negated {
		t.Fatalf("after their negation: %+v %v", m, ok)
	}
	if m, ok := c.Find("we dont need to rebuild the images on each commit", "", true); !ok || !m.Clean() {
		t.Fatalf("a statement before a question: %+v %v", m, ok)
	}
	for q, want := range map[string]bool{
		"which one would you start with":                    false,
		"we dont need to rebuild the images on each commit": true,
		"no release on docs":                                true, // most of its description
	} {
		desc := "Write no code until the owner says go"
		switch q {
		case "no release on docs":
			desc = "No release on docs."
		case "we dont need to rebuild the images on each commit":
			desc = "Rebuild the images only when a commit touches them."
		}
		if got := ProvesRuling(q, desc, ""); got != want {
			t.Errorf("ProvesRuling(%q) = %v", q, got)
		}
	}
}
