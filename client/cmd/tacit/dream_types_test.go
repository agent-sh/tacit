package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/fact"
)

// The quiet types come from the delivery log: a type whose delivered lines
// per live fact are under half the median of the dreamer's types is quiet,
// among the types a measurement may quiet (structure and history; never the
// owner's standards, the failures, machine facts or decisions, however
// rarely delivered); a type the dreamer does not write moves no median;
// this machine's own notes are not counted; with under a week of log, none
// is quiet; TACIT_DREAM_QUIET=off turns it off.
func TestQuietDreamTypesFromTheDeliveryLog(t *testing.T) {
	store := t.TempDir()
	write := func(typ string, n int) {
		for i := 0; i < n; i++ {
			f := &fact.Fact{Path: fmt.Sprintf("user/%s-%d.md", strings.ReplaceAll(typ, "_", "-"), i), Meta: fact.Meta{Name: fmt.Sprintf("%s-%d", strings.ReplaceAll(typ, "_", "-"), i), Description: "d", Type: typ, Scope: "user",
				Source: fact.Source{Harness: "curator", Host: "host-a", Kind: "observed"}, Created: "2026-10-01"}, Body: "b\n"}
			b, _ := f.Encode()
			os.MkdirAll(filepath.Join(store, "user"), 0o755)
			os.WriteFile(filepath.Join(store, f.Path), b, 0o644)
		}
	}
	write("machine_fact", 60)
	write("past_mistake", 60)
	write("design_decision", 60)
	write("structure", 60)
	write("history", 60)
	write("reference", 60)
	state := t.TempDir()
	logDir := filepath.Join(state, "delivery")
	os.MkdirAll(logDir, 0o700)
	line := func(typ string, n int) string {
		var cs []string
		for i := 0; i < n; i++ {
			cs = append(cs, `{"name":"x","type":"`+typ+`","delivered":true}`)
		}
		return `{"v":1,"hook":"prompt","cands":[` + strings.Join(cs, ",") + `]}` + "\n"
	}
	local := func(typ string, n int) string {
		var cs []string
		for i := 0; i < n; i++ {
			cs = append(cs, `{"name":"x","type":"`+typ+`","local":true,"delivered":true}`)
		}
		return `{"v":1,"hook":"prompt","cands":[` + strings.Join(cs, ",") + `]}` + "\n"
	}
	day := func(i int) string { return filepath.Join(logDir, fmt.Sprintf("2026-11-%02d.jsonl", i+1)) }
	for i := 0; i < 6; i++ {
		// Per day: machine facts and past mistakes 10 and 12 (the median),
		// decisions 1 (low, but never quiet), structure 2 (quiet), history 9
		// with 50 more of this machine's own notes (not counted: still not
		// quiet), and a type the dreamer does not write at 0, which would
		// pull the median down if it counted.
		os.WriteFile(day(i), []byte(line("machine_fact", 10)+line("past_mistake", 12)+line("design_decision", 1)+line("structure", 2)+line("history", 9)+local("structure", 50)), 0o600)
	}
	t.Setenv("TACIT_DREAM_QUIET", "")
	if q := quietDreamTypes(store, state, false); len(q) != 0 {
		t.Fatalf("six days of log made quiet types: %v", q)
	}
	os.Remove(filepath.Join(state, "dream-types.json"))
	os.WriteFile(day(6), []byte(line("structure", 0)), 0o600)
	q := quietDreamTypes(store, state, false)
	if len(q) != 1 || !q["structure"] {
		t.Fatalf("quiet = %v, want structure", q)
	}
	// Read again the same day: the kept measurement, for this store only.
	if q := quietDreamTypes(store, state, false); len(q) != 1 || !q["structure"] {
		t.Fatalf("kept: %v", q)
	}
	if _, err := os.Stat(filepath.Join(state, "dream-types.json")); err != nil {
		t.Fatal("the measurement was not kept")
	}
	// A dry run measures and keeps nothing.
	os.Remove(filepath.Join(state, "dream-types.json"))
	if q := quietDreamTypes(store, state, true); !q["structure"] {
		t.Fatalf("dry: %v", q)
	}
	if _, err := os.Stat(filepath.Join(state, "dream-types.json")); err == nil {
		t.Fatal("a dry run kept its measurement")
	}
	t.Setenv("TACIT_DREAM_QUIET", "off")
	if q := quietDreamTypes(store, state, false); len(q) != 0 {
		t.Fatalf("off: %v", q)
	}
	t.Setenv("TACIT_DREAM_QUIET", "history,past_mistake,machine_fact")
	if q := quietDreamTypes(store, state, false); len(q) != 1 || !q["history"] {
		t.Fatalf("a list: %v", q)
	}
}
