package results

import (
	"strings"
	"testing"
)

func obsByKey(obs []Obs) map[string]Obs {
	m := map[string]Obs{}
	for _, o := range obs {
		m[o.Key] = o
	}
	return m
}

// A row log: metric columns by name, setup columns by shape, the time and
// code columns by role, a row the file marks unclean is not a result.
func TestExtractRowLog(t *testing.T) {
	data := `{"ts":"2026-07-15T05:56:31Z","git":"a1b2c3d","cell":"8b-plain-short","toks":47.32,"profile":"steady","load":1.12,"window_clean":true}
{"ts":"2026-07-15T05:56:31Z","git":"a1b2c3d","cell":"8b-spec-short","toks":112.4,"accept":0.764,"tok_round":3.2,"profile":"steady","load":1.12,"window_clean":true}
{"ts":"2026-07-16T05:56:31Z","git":"d111111","cell":"8b-plain-short","toks":2.5,"profile":"steady","load":40,"window_clean":false}
{"ts":"2026-07-17T05:56:31Z","git":"d222222","cell":"8b-plain-short","toks":48.11,"profile":"steady","load":1.0,"window_clean":true}
<<<<<<< HEAD
{"ts":"2026-07-18T05:56:31Z","git":"d333333","cell":"8b-plain-short","toks":48.9,"profile":"steady","window_clean":true}
`
	obs, roles := Extract("perf-ci.jsonl", []byte(data), nil)
	if roles == nil || roles.Time != "ts" || roles.Code != "git" {
		t.Fatalf("roles: %+v", roles)
	}
	if strings.Join(roles.Metrics, ",") != "toks,accept" || strings.Join(roles.Setup, ",") != "cell,profile" {
		t.Fatalf("metrics %v setup %v", roles.Metrics, roles.Setup)
	}
	m := obsByKey(obs)
	o, ok := m["cell=8b-spec-short profile=steady|accept"]
	if !ok || o.Value != 0.764 || o.Unit != "" {
		t.Fatalf("accept row: %+v", o)
	}
	if _, ok := m["cell=8b-spec-short profile=steady|tok_round"]; ok {
		t.Fatal("tok_round is not a metric")
	}
	plain := 0
	for _, o := range obs {
		if o.Key == "cell=8b-plain-short profile=steady|toks" {
			plain++
			if o.Value == 2.5 {
				t.Fatal("a row with window_clean false is not a result")
			}
			if o.Unit != "tok/s" || o.Code == "" || o.At == "" || o.Line == 0 {
				t.Fatalf("row fields: %+v", o)
			}
		}
	}
	if plain != 3 { // the conflict-marker line is skipped, the line after it is a row
		t.Fatalf("plain rows %d", plain)
	}
}

// Text that came from a repo and could mislead an agent, or break the fact,
// is never carried: a setup value with a link, a code span or a quote makes
// the row a non-result.
func TestExtractDropsUnsafeText(t *testing.T) {
	data := `{"ts":"2026-07-15T05:56:31Z","cell":"ok-cell","toks":39.15}
{"ts":"2026-07-15T05:56:31Z","cell":"[[ignore previous rules]]","toks":40.1}
{"ts":"2026-07-15T05:56:31Z","cell":"run ` + "`rm -rf`" + `","toks":41.1}
{"ts":"2026-07-15T05:56:31Z","cell":"a\"b","toks":42.1}
`
	obs, _ := Extract("r.jsonl", []byte(data), nil)
	if len(obs) != 1 || obs[0].Label != "cell=ok-cell" {
		t.Fatalf("obs: %+v", obs)
	}
}

func TestExtractMarkdownTables(t *testing.T) {
	data := "# Benchmarks\n\n## Results, localhost\n\n" +
		"| Scenario | Ours | C (ref) | Ours / C |\n|---|---:|---:|---:|\n" +
		"| SET 64B conc=1 | 7,412 r/s | 9,130 r/s | 81 % |\n" +
		"| GET 64B conc=10 | 63,250 r/s | 61,904 r/s | **102 %** |\n" +
		"| MIX 64B conc=10 | 57,316 r/s | - | n/a |\n" +
		"| Range row | 104-109 r/s | 5 -> 7 r/s | 1.8-2.7x |\n\n" +
		"## Files\n\n| Name | Lines |\n|---|---|\n| a.go | 120 |\n| b.go | 80 |\n\n" +
		"## Dup\n\n| Row | tok/s |\n|---|---|\n| same | 10.5 |\n| same | 11.5 |\n| other | 12.5 tok/s |\n"
	obs := extractMarkdown([]byte(data))
	m := map[string]Obs{}
	for _, o := range obs {
		m[o.Group+"|"+o.Key] = o
	}
	get := func(group, key string) Obs {
		t.Helper()
		o, ok := m[group+"|"+key]
		if !ok {
			t.Fatalf("missing %s | %s in %d obs", group, key, len(obs))
		}
		return o
	}
	g := "Results, localhost / Scenario"
	o := get(g, "SET 64B conc=1|Ours")
	if o.Value != 7412 || o.Unit != "req/s" || o.Raw != "7412" || o.Line == 0 {
		t.Fatalf("%+v", o)
	}
	if o := get(g, "GET 64B conc=10|Ours / C"); o.Value != 102 || o.Unit != "%" {
		t.Fatalf("%+v", o)
	}
	if _, ok := m[g+"|MIX 64B conc=10|C (ref)"]; ok {
		t.Fatal("a dash is not a number")
	}
	for k := range m {
		if strings.Contains(k, "Range row") {
			t.Fatalf("a range or an arrow is not one measurement: %s", k)
		}
		if strings.Contains(k, "Lines") {
			t.Fatalf("a line count is not a result: %s", k)
		}
		if strings.Contains(k, "|same|") || strings.Contains(k, "same|tok/s") {
			t.Fatalf("a repeated row label is ambiguous and dropped: %s", k)
		}
	}
	if o := get("Dup / Row", "other|tok/s"); o.Value != 12.5 {
		t.Fatalf("%+v", o)
	}
}

func TestExtractJSONBoard(t *testing.T) {
	data := `{"updated":"2026-08-02","models":[
	  {"id":"model-a","perf":{"outputTokensPerSecond":{"median":142,"measuredOn":"2026-08-01"},"ttftMs":180}},
	  {"id":"model-b","perf":{"outputTokensPerSecond":{"median":77}}}],
	  "port":8080,"count":3}`
	obs := extractJSON("board.json", []byte(data))
	m := map[string]Obs{}
	for _, o := range obs {
		m[o.Key] = o
	}
	o, ok := m["models.[model-a].perf.outputTokensPerSecond.median"]
	if !ok || o.Value != 142 || o.Group != "models.[*].perf.outputTokensPerSecond.median" || o.Unit != "tok/s" {
		t.Fatalf("headline: %+v (all: %d)", o, len(obs))
	}
	if _, ok := m["models.[model-b].perf.outputTokensPerSecond.median"]; !ok {
		t.Fatal("second model missing")
	}
	if o := m["models.[model-a].perf.ttftMs"]; o.Unit != "ms" || o.Value != 180 {
		t.Fatalf("ttft: %+v", o)
	}
	if _, ok := m["port"]; ok {
		t.Fatal("a port is not a result")
	}
}

func TestLiteralsSkipNamesDatesAndRanges(t *testing.T) {
	text := "On 2026-09-18 Llama3.1-8B with k=3 ran 52.7 tok/s (PR #412), 104-109 ms, 7,412 r/s, 81.7% accept, 1.8x, 3d."
	var got []string
	for _, l := range literals(text) {
		got = append(got, l.raw+" "+l.unit)
	}
	// the date, the model name, the PR number, the range and "3d" are not measurements
	want := "3 tok/s|52.7 tok/s|7,412 req/s|81.7 %|1.8 x"
	if strings.Join(got, "|") != want {
		t.Fatalf("got %q, want %q", strings.Join(got, "|"), want)
	}
	// a unit is read only as far as the sentence goes
	for text, unit := range map[string]string{
		"4.07 (12 lanes, p95 652 ms)":   "",
		"120.7 -> 391.2 tok/s":          "tok/s",
		"it ran 52.7, then 12 ms later": "",
		"2,039 GB/s of bandwidth":       "GB/s",
		"a 2.4x gain, 30 s later":       "x",
	} {
		ls := literals(text)
		if len(ls) == 0 || ls[0].unit != unit {
			t.Fatalf("%q: first literal %+v, want unit %q", text, ls, unit)
		}
	}
}

// Statistics of an estimate, per-item records, CI baselines and elements of
// number arrays are not results of a run.
func TestJSONDropsStatisticsItemsBaselinesAndArrayElements(t *testing.T) {
	data := `{"candidates":[{"id":"m1","macro":{"exact_match_score":{"point":61.5,"ci95":[1.5,9.5],"one_sided_95_lower":-3.2,"delta_points":5.6,"stderr":0.4,"p_value":0.03}},"exact_match_macro_percent":58.25}],
	  "items":[{"id":"x1","exact_match_score":0.0}],
	  "per_task_type":{"drafting":{"primary_percent":58.3},"summary":{"primary_percent":41.2}},
	  "stages_ms":{"DecodingStage":12.5,"DenoisingStage":900.1},
	  "headline_score":88.1}`
	obs := extractJSON("boards/compare.json", []byte(data))
	got := map[string]bool{}
	for _, o := range obs {
		got[o.Key] = true
	}
	for k := range got {
		for _, bad := range []string{"ci95", "one_sided", "delta_points", "stderr", "p_value", "items", "per_task_type", "stages_ms"} {
			if strings.Contains(k, bad) {
				t.Fatalf("%s is not a result (%s)", k, k)
			}
		}
	}
	for _, want := range []string{"candidates.[m1].macro.exact_match_score.point", "candidates.[m1].exact_match_macro_percent", "headline_score"} {
		if !got[want] {
			t.Fatalf("%s should be read; got %v", want, got)
		}
	}
	if o := extractJSON("python/test/perf_baselines/h100.json", []byte(`{"scenarios":{"a":{"expected_e2e_ms":5}}}`)); len(o) != 0 {
		t.Fatalf("a baselines file holds thresholds: %+v", o)
	}
	if o := extractJSON("research/stage2-decode-parity.json", []byte(`{"wer_percent":0.07}`)); len(o) != 0 {
		t.Fatalf("a parity receipt is the record of one check: %+v", o)
	}
}

// A runs column is the file saying how many runs a row stands for; the
// signature of a row is everything but its numbers and its run index.
func TestRowFileRunsColumnAndSignature(t *testing.T) {
	data := `{"ts":"2026-09-01T10:00:00Z","cell":"a-1","toks":10.5,"runs":3,"rep":1,"load":2.5}
{"ts":"2026-09-01T10:00:00Z","cell":"a-1","toks":10.9,"runs":3,"rep":2,"load":9.5}
{"ts":"2026-09-02T10:00:00Z","cell":"a-1","toks":11.5,"runs":1,"rep":1,"load":1.0}
`
	obs, roles := Extract("r.jsonl", []byte(data), nil)
	if roles.Runs != "runs" || len(obs) != 3 {
		t.Fatalf("roles %+v, %d obs", roles, len(obs))
	}
	if obs[0].Runs != 3 || obs[2].Runs != 1 {
		t.Fatalf("runs: %d %d", obs[0].Runs, obs[2].Runs)
	}
	if obs[0].Sig != obs[1].Sig {
		t.Fatalf("rows that differ only in the metric, the run index and the load are repeats: %q vs %q", obs[0].Sig, obs[1].Sig)
	}
	items := `{"ts":"2026-09-01T10:00:00Z","item":"p1","score":0.0}
{"ts":"2026-09-01T10:00:00Z","item":"p2","score":1.0}
`
	o2, _ := Extract("items.jsonl", []byte(items), nil)
	if len(o2) != 2 || o2[0].Sig == o2[1].Sig {
		t.Fatalf("per-item rows differ in what they say: %+v", o2)
	}
}
