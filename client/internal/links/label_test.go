package links

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/fact"
)

// The label of a link is the text after the first `|`, also after a section:
// it stays on a rewrite and is what an unwrapped link turns into. The section
// belonged to the old target and goes.
func TestLabelSurvivesASection(t *testing.T) {
	tr := &tree{t: t, root: t.TempDir()}
	tr.add("projects/x", "new", "", "live")
	tr.add("projects/x", "old", "superseded_by: new\n", "old")
	tr.add("projects/x", "f", "", "see [[old#setup|the setup guide]], [[old#setup]], [[old|plain label]] and [[gone#part|its label]], [[gone#part]]")
	before, _ := os.ReadFile(filepath.Join(tr.root, "projects/x/f.md"))
	fx := fixOf(tr.plan(nil), "f")
	if fx == nil {
		t.Fatal("no fix")
	}
	_, body, _ := fact.Split(fx.Data)
	want := "see [[new|the setup guide]], [[new]], [[new|plain label]] and its label, gone"
	if got := strings.TrimSpace(string(body)); got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	if !IsRepair(before, fx.Data) {
		t.Error("not a repair of the original")
	}
}
