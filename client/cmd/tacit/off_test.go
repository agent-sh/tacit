package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setOff(t *testing.T, reason string) string {
	t.Helper()
	p := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "tacit", "off")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(reason), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// With the off file there, every hook prints nothing, every MCP tool says
// tacit is off and why, the model jobs end with one line, and doctor
// reports it; removing the file turns tacit on again, within a running MCP
// server too.
func TestOffSwitch(t *testing.T) {
	treeRig(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	start := `{"session_id":"s-off","source":"startup"}`
	if out := hookOut(t, start, "session-start", "--mcp"); !strings.Contains(out, "widget-signoff") {
		t.Fatalf("the rig's start block is missing its fact:\n%s", out)
	}
	if out := callTool(t, "tacit_fact", map[string]any{"name": "widget-signoff"}); strings.Contains(out, "tacit is off") {
		t.Fatalf("on: %s", out)
	}
	p := setOff(t, "  failed the weekly scorecard\ntwice \n")
	for _, args := range [][]string{{"session-start", "--mcp"}, {"prompt", "--mcp"}, {"pre-tool"}, {"pre-compact"}, {"questions"}} {
		in := `{"session_id":"s-off2","source":"startup","prompt":"commit the widget change with git commit","tool_name":"Bash","tool_input":{"command":"git commit -m x"}}`
		if out := hookOut(t, in, args...); out != "" {
			t.Errorf("hook %s printed while off:\n%s", args[0], out)
		}
	}
	for _, tool := range []string{"tacit_search", "tacit_fact", "tacit_tree", "tacit_add", "tacit_episode", "tacit_mark"} {
		if out := callTool(t, tool, map[string]any{}); out != "tacit is off: failed the weekly scorecard twice" {
			t.Errorf("%s while off: %q", tool, out)
		}
	}
	for _, job := range []string{"dream", "curate", "maintain", "verify", "rotation", "delivery-judge", "add"} {
		var off bool
		out := captureStdout(t, func() { off = jobOff(job) })
		if !off || out != job+": tacit is off: failed the weekly scorecard twice ("+p+")\n" {
			t.Errorf("job %s: off=%v %q", job, off, out)
		}
	}
	if jobOff("search") || jobOff("doctor") {
		t.Error("the off switch stops a command that is not a job")
	}
	checks, _ := doctorChecks(false, "")
	found := false
	for _, c := range checks {
		if c.Name == "off switch" && c.Status == "warn" && strings.Contains(c.Detail, "failed the weekly scorecard twice") {
			found = true
		}
	}
	if !found {
		t.Errorf("doctor does not report the off switch: %+v", checks)
	}
	// An empty file is off too, with no reason given.
	setOff(t, "")
	if r, off := tacitOff(); !off || r != "no reason given" {
		t.Errorf("empty off file: %q %v", r, off)
	}
	os.Remove(p)
	if _, off := tacitOff(); off {
		t.Fatal("still off without the file")
	}
	if out := callTool(t, "tacit_fact", map[string]any{"name": "widget-signoff"}); strings.Contains(out, "tacit is off") {
		t.Fatalf("on again: %s", out)
	}
}

// tacit_add takes a fact with a description alone: the name and the type
// have defaults in addFact (a name from the text, type unclassified), so
// the tool's schema does not require them, and a call that leaves them out
// is recorded rather than refused by the schema.
func TestAddNeedsOnlyADescription(t *testing.T) {
	treeRig(t)
	out := callTool(t, "tacit_add", map[string]any{"description": "widget builds with make, not cargo", "body": "The widget repo builds with make."})
	if !strings.HasPrefix(out, "recorded ") || !strings.Contains(out, "widget-builds-with-make") {
		t.Fatalf("tacit_add without name and type: %q", out)
	}
}
