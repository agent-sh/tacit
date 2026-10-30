package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/digest"
)

// verifyAttempts is how many verifier runs a finding is sent to without a
// verdict landing; then it is no longer sent until it is requeued.
const verifyAttempts = 3

// verifyFailure is why the last counted attempt on a finding failed. They
// are kept in failures.json beside attempts.json, so the run that stops
// sending a finding can say which one and why.
type verifyFailure struct {
	At  string `json:"at"`
	Why string `json:"why"`
}

func (r *verifyRun) failures() map[string]verifyFailure {
	m := map[string]verifyFailure{}
	if b, err := os.ReadFile(filepath.Join(r.stateDir, "failures.json")); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

// failed records why the attempt on finding id failed; saveAttempts
// redacts and writes it. A dry run records nothing, as it counts nothing.
func (r *verifyRun) failed(id, why string) {
	if r.dry {
		return
	}
	if r.fresh == nil {
		r.fresh = map[string]string{}
	}
	r.fresh[id] = why
}

// saveFailures keeps the failures of the findings that still have attempts
// counted: a verdict that landed, a finding that left the review files and
// a requeue drop the line with the counter. A reason can carry text from a
// model's answer or an error, so the new ones go through the redactor in one
// call first; when it cannot run, the reason says only that.
func (r *verifyRun) saveFailures(tries map[string]int) {
	if r.fails == nil && len(r.fresh) == 0 {
		return
	}
	if r.fails == nil {
		r.fails = r.failures()
	}
	if len(r.fresh) > 0 {
		var ids, texts []string
		for id, why := range r.fresh {
			ids, texts = append(ids, id), append(texts, why)
		}
		red, err := digest.RedactAll(texts)
		now := time.Now().Format(time.RFC3339)
		for i, id := range ids {
			why := "the reason could not be redacted, so it is not kept"
			if err == nil {
				why = capText(red[i], 300)
			}
			r.fails[id] = verifyFailure{At: now, Why: why}
		}
		r.fresh = nil
	}
	for id := range r.fails {
		if tries[id] <= 0 {
			delete(r.fails, id)
		}
	}
	b, _ := json.Marshal(r.fails)
	tmp := filepath.Join(r.stateDir, "failures.json.tmp")
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, filepath.Join(r.stateDir, "failures.json"))
	}
}

// verdictMarks are the marks of the verifier's answer schema: the only text
// of a verdict a failure line repeats.
var verdictMarks = map[string]bool{"both": true, "keep": true, "query": true, "owner": true, "retire": true, "fix": true}

// verdictGap says why a finding of a batch that came back got no mark: the
// verifier gave it no verdict, or one this finding cannot take.
func verdictGap(vs []verifyVerdict, id string, names ...string) string {
	for _, v := range vs {
		if v.ID != id {
			continue
		}
		switch {
		case v.Mark == "keep" && len(names) == 2 && v.Keep != names[0] && v.Keep != names[1]:
			return "the verdict keeps a fact that is neither of the two"
		case v.Mark == "owner" && strings.TrimSpace(v.Question) == "":
			return "the verdict asks the owner without a question"
		case v.Mark == "fix" && len(names) == 0 && strings.TrimSpace(v.Fix) == "":
			return "the verdict is a fix without the fix"
		case verdictMarks[v.Mark]:
			return fmt.Sprintf("the verdict's mark %q is not one this finding takes", v.Mark)
		default:
			return "the verdict's mark is not one this finding takes"
		}
	}
	return "no verdict came back for it"
}

// staleLabel is a stale finding's mark in the verifier's words.
func staleLabel(mark string) string {
	switch mark {
	case "current":
		return "keep"
	case "ask":
		return "owner"
	}
	return mark
}

// markLabel is a mark as the verifier gave it, for a failure line.
func markLabel(o op) string {
	switch o.Mark {
	case "left":
		return "keep " + o.A
	case "right":
		return "keep " + o.B
	case "ask":
		return "owner"
	}
	return o.Mark
}

// reportStranded names each finding that reached the attempt limit, what it
// is about and why its last attempt failed.
func (r *verifyRun) reportStranded(rs *reviewServer, tries map[string]int) {
	var ids []string
	for id, n := range tries {
		if n >= r.attemptsBefore {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return
	}
	sort.Strings(ids)
	fmt.Fprintf(os.Stderr, "verify: %d finding(s) reached %d failed attempts and are no longer sent (tacit verify --requeue all, or their ids, sends them again):\n", len(ids), r.attemptsBefore)
	about := map[string]string{}
	for _, it := range rs.items() {
		about[it.ID] = it.Kind + " " + it.A.Name + " / " + it.B.Name
	}
	for _, it := range rs.staleItems() {
		about[it.ID] = it.Kind + " " + it.A.Name
	}
	fails := r.fails
	if fails == nil {
		fails = r.failures()
	}
	for _, id := range ids {
		why := "no failure recorded (the limit was reached before v0.9.15 kept reasons, or a run was stopped during the attempt)"
		if f, ok := fails[id]; ok {
			why = f.Why + " (" + strings.SplitN(f.At, "T", 2)[0] + ")"
		}
		fmt.Fprintf(os.Stderr, "verify: stranded %s, %s: %s\n", id, about[id], why)
	}
}

// requeueVerify implements `tacit verify --requeue all|ID,...`: the named
// findings (all: every one at the attempt limit) lose their attempt count
// and their recorded failure, so the next run sends them again. It calls no
// model and changes nothing else; a dry run only says what it would do. An
// id with no count here is an error (exit 1), as a typo would be.
func requeueVerify(stateDir, which string, dry bool) int {
	r := &verifyRun{stateDir: stateDir}
	tries := r.attempts()
	r.fails = r.failures()
	var ids []string
	if strings.TrimSpace(which) == "all" {
		for id, n := range tries {
			if n >= verifyAttempts {
				ids = append(ids, id)
			}
		}
	} else {
		seen := map[string]bool{}
		for _, id := range strings.Split(which, ",") {
			if id = strings.TrimSpace(id); id != "" && !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 && strings.TrimSpace(which) != "all" {
		fmt.Fprintln(os.Stderr, "verify: --requeue needs all or finding ids separated by commas")
		return 2
	}
	sort.Strings(ids)
	var done []string
	code := 0
	for _, id := range ids {
		if _, ok := tries[id]; !ok {
			fmt.Fprintf(os.Stderr, "verify: %s has no failed attempts here, nothing to requeue\n", id)
			code = 1
			continue
		}
		delete(tries, id)
		done = append(done, id)
	}
	if len(done) > 0 && !dry {
		r.saveAttempts(tries)
	}
	list, verb := "", "requeued"
	if len(done) > 0 {
		list = ": " + strings.Join(done, ", ")
	}
	if dry {
		verb = "would be requeued (dry run, nothing written)"
	}
	fmt.Printf("verify: %d finding(s) %s%s\n", len(done), verb, list)
	return code
}
