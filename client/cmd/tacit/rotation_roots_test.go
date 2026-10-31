package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/rotation"
	"github.com/agent-sh/tacit/client/internal/store"
)

// The rotation's two path readers take a path under the same roots: the
// gone-path reader, whose paths put a fact first, and the place reader,
// whose paths the proof stats and the ancestor index reads. A route of an
// API or a forge, a temp directory or root's home is a place for neither.
func TestRotationPathReadersTakeTheSameRoots(t *testing.T) {
	for _, c := range []struct {
		root  string
		place bool
	}{
		{"~", true}, {"/home", true}, {"/Users", true}, {"/data", true}, {"/opt", true}, {"/etc", true}, {"/var", true},
		{"/usr", true}, {"/srv", true}, {"/mnt", true}, {"/Library", true},
		{"/tmp", false}, {"/root", false}, {"/workspace", false}, {"/v1", false}, {"/repos", false}, {"/proc", false},
	} {
		text := "it lives in " + c.root + "/box/models.toml"
		if got := machinePathRx.MatchString(text); got != c.place {
			t.Errorf("%s: the place reader takes it: %v, want %v", c.root, got, c.place)
		}
		if got := gonePathRx.MatchString(text); got != c.place {
			t.Errorf("%s: the gone-path reader takes it: %v, want %v", c.root, got, c.place)
		}
	}
}

// A fact of this machine that names a path under /data gets the path in its
// proof, stated as the stat found it, as for any other root the pick reads.
func TestRotationProofStatsAPathUnderData(t *testing.T) {
	stubScrump(t)
	r := rotationRig(t)
	host := store.ShortHost()
	scope := "machines/" + host
	os.MkdirAll(filepath.Join(r.clone, filepath.FromSlash(scope)), 0o755)
	desc := "The model weights live in /data/tacit-test-no-such-dir/weights"
	os.WriteFile(filepath.Join(r.clone, filepath.FromSlash(scope), "weights-dir.md"), []byte(rotFile("weights-dir", scope, "machine_fact", desc, desc+".", "2026-01-01", "")), 0o644)
	r.commitIndex(r.clone, "a machine fact")
	gitIn(t, r.clone, "push", "-q", "origin", "HEAD:main")
	fr := &fakeRotator{answer: func(it rotation.Item) (rotation.Verdict, bool) {
		return rotation.Verdict{Outcome: rotation.Still}, true
	}}
	prev := rotationRun
	rotationRun = fr.run
	t.Cleanup(func() { rotationRun = prev })
	runRotationStep(t, r, rotationOpts{model: "m", age: 30, batch: 1, only: map[string]bool{"weights-dir": true}})
	want := "/data/tacit-test-no-such-dir/weights does not exist on " + host
	if len(fr.prompts) != 1 || !strings.Contains(fr.prompts[0], want) {
		t.Fatalf("the proof lacks %q in %d prompt(s):\n%s", want, len(fr.prompts), strings.Join(fr.prompts, "\n"))
	}
}

// The ancestor index reads a path under /data as it reads one under the
// other roots: a folder of a known repository's tree there is inside it. The
// root itself is no folder of a tree, so a repository named like a root
// (data) holds nothing by it.
func TestAncestorIndexReadsPathsUnderData(t *testing.T) {
	home, _, _ := checkedHome(t)
	s := &store.Store{Dir: filepath.Join(home, "store")}
	data := filepath.Join(home, "projects", "data")
	os.MkdirAll(data, 0o755)
	gitIn(t, data, "init", "-q", "-b", "main")
	add := func(scope, name, session, desc string) {
		writeFactFile(t, s.Dir, &fact.Fact{Path: scope + "/" + name + ".md", Meta: fact.Meta{Name: name, Description: desc, Type: "history", Scope: scope,
			Source: fact.Source{Harness: "curator", Host: "box", Session: session, Kind: "observed"}, Created: "2026-10-12"}, Body: desc + ".\n"})
	}
	add("projects/kiln-tool", "kiln-runner", "s-k1", "The kiln tool runs from /data/widget/kiln-tool/run.sh")
	add("projects/quill-scratch", "quill-out", "s-q1", "The quill scratch output goes to /data/quill-scratch/out.log")
	facts, _, _ := fact.Walk(s.Dir)
	writeIndexesForTest(t, s.Dir, facts)
	gitIn(t, s.Dir, "add", "-A")
	gitIn(t, s.Dir, "commit", "-q", "-m", "paths under /data")
	got := grouping(t, s)
	if !strings.Contains(got["projects/kiln-tool"], "ancestor=projects/widget ") {
		t.Errorf("kiln-tool, a folder of widget's tree under /data: %q", got["projects/kiln-tool"])
	}
	if strings.Contains(got["projects/quill-scratch"], "ancestor=projects/data ") {
		t.Errorf("quill-scratch is inside the root /data, no repository: %q", got["projects/quill-scratch"])
	}
}
