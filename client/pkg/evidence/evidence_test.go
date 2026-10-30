package evidence_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/pkg/evidence"
)

// The facade pulls in the readers and the rules, not the dreamer or the
// store packages: a program that imports it compiles these of tacit's
// packages and no others.
func TestFacadeImportsNoDreamerOrStore(t *testing.T) {
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go command to list the imports with")
	}
	out, err := exec.Command(gobin, "list", "-deps", "-f", "{{.ImportPath}}", ".").Output()
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{}
	for _, p := range []string{"digest", "events", "hermes", "ledger", "modelrun", "harnesses", "ownerwords", "platform", "relevance", "repoid", "repoinfer", "transcript"} {
		allowed["github.com/agent-sh/tacit/client/internal/"+p] = true
	}
	for _, p := range strings.Fields(string(out)) {
		if strings.HasPrefix(p, "github.com/agent-sh/tacit/client/") && p != "github.com/agent-sh/tacit/client/pkg/evidence" && !allowed[p] {
			t.Errorf("the facade imports %s", p)
		}
	}
}

// A program reads a machine's sessions through the facade alone: where they
// are, then each transcript's events, with the owner's words told apart.
func TestReadSessionsThroughTheFacade(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", "")
	t.Setenv("TACIT_HERMES_DB", "off")
	path := filepath.Join(home, ".claude", "projects", "-home-u-projects-shop", "s1.jsonl")
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.WriteFile(path, []byte(strings.Join([]string{
		`{"type":"user","uuid":"u0","promptSource":"typed","origin":{"kind":"human"},"message":{"role":"user","content":"keep the cache warm before a release"},"timestamp":"2026-10-20T10:00:00.000Z","entrypoint":"cli","cwd":"/home/u/projects/shop","sessionId":"s1"}`,
		`{"type":"assistant","uuid":"a1","message":{"model":"claude-opus-5-5","role":"assistant","content":[{"type":"tool_use","id":"toolu_01","name":"Bash","input":{"command":"make warm"}}]},"timestamp":"2026-10-20T10:00:05.000Z","entrypoint":"cli","cwd":"/home/u/projects/shop","sessionId":"s1"}`,
		`{"type":"user","uuid":"u2","message":{"role":"user","content":[{"tool_use_id":"toolu_01","type":"tool_result","content":"no target","is_error":true}]},"timestamp":"2026-10-20T10:00:06.000Z","entrypoint":"cli","cwd":"/home/u/projects/shop","sessionId":"s1"}`,
	}, "\n")+"\n"), 0o600)

	var got []evidence.Event
	for _, src := range evidence.Sources(home) {
		files, err := evidence.Files(src)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if _, err := evidence.ReadFile(f, src.Format, 0, func(e evidence.Event) error {
				got = append(got, e)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(got) != 3 || got[0].Kind != evidence.KindOwnerText || got[0].Who != evidence.Owner || got[0].InputKind != evidence.InputTyped ||
		got[1].Kind != evidence.KindToolCall || got[2].Kind != evidence.KindToolResult || got[2].ToolID != got[1].ToolID || !got[2].IsError {
		t.Fatalf("events: %+v", got)
	}
	if v := evidence.SessionWho(path, evidence.Head(path, 256<<10), evidence.Claude); v.Who != evidence.Owner {
		t.Fatalf("session: %+v", v)
	}
}
