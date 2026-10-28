package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/curate"
	"github.com/agent-sh/tacit/client/internal/platform"
)

// A second entry of the split that says only what the first says, in other
// words (its description shares too few terms for the description rule), is
// not written: the run's report records it with the fact that covers it.
func TestCurateAbsorbsAParaphraseByContentAndRecordsIt(t *testing.T) {
	home, _ := quickHome(t)
	body := "The quokka fixture driver on the build host uses port 7790 behind the proxy."
	two, _ := json.Marshal([]map[string]any{
		{"origin": "fresh-note", "name": "quokka-driver-port", "description": body, "type": "machine_fact", "scope": "system", "kind": "observed", "body": body},
		{"origin": "fresh-note", "name": "reach-quokka", "description": "Reach the quokka driver at port 7790.", "type": "machine_fact", "scope": "system", "kind": "observed", "body": body},
	})
	r, _ := json.Marshal(map[string]any{"result": string(two), "total_cost_usd": .01})
	t.Setenv("FIXTURE_FRESH", string(r))
	if code := runCurate([]string{"--quick", "--min-interval", "1ns"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	store := filepath.Join(home, "store")
	if _, ok := findFact(store, "quokka-driver-port"); !ok {
		t.Fatal("the first fact was not written")
	}
	if _, ok := findFact(store, "reach-quokka"); ok {
		t.Fatal("the paraphrase was written")
	}
	b, _ := os.ReadFile(filepath.Join(platform.StateDir(home), "curator.log"))
	var rep curate.Report
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if json.Unmarshal([]byte(lines[len(lines)-1]), &rep) != nil || rep.Absorbed != 1 || len(rep.AbsorbedBy) != 1 {
		t.Fatalf("report: %+v", rep)
	}
	ab := rep.AbsorbedBy[0]
	if ab.Rule != "content" || ab.By != "quokka-driver-port" || ab.Note != "fresh-note" || ab.Coverage < curate.ContentCover {
		t.Fatalf("absorption: %+v", ab)
	}
}
