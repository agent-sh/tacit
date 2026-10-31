// Package relevance is the gate on the owner's words (SPEC.md, "Relevance"):
// not every sentence of theirs is a statement. Wherever their words would
// become a statement or a ruling (the dreamer's capture, the curator's split,
// the reviewer's trim to their statement, `tacit rulings one-offs`), the
// deciding model judges whether the remark would matter to future work and
// says why; code keeps it only with
// one of the categories below and a reason, and still checks the words are
// theirs. A remark dropped is not curated: it stays only in the history it
// came from (the inbox note, the reviewer's ledger, the statements log), with
// the reason.
package relevance

import "strings"

// What makes a remark of the owner's matter to future work (a kept remark).
const (
	Instruction = "instruction" // how work should be done
	Decision    = "decision"    // a choice made
	Preference  = "preference"  // what they like or want
	Fact        = "fact"        // something about a project, a machine or the world
	Correction  = "correction"  // they correct something wrong
)

// Why a remark does not (a dropped remark).
const (
	Acknowledgement = "acknowledgement" // "ok", "go", "check", "yes do it"
	Question        = "question"        // a passing question
	Frustration     = "frustration"     // annoyance, with nothing to act on
	Steering        = "steering"        // one-time task steering: "run it again", "now push"
	Other           = "other"           // anything else with no future use
)

// Keeps are the categories of a kept remark; Drops those of a dropped one.
var (
	Keeps = map[string]bool{Instruction: true, Decision: true, Preference: true, Fact: true, Correction: true}
	Drops = map[string]bool{Acknowledgement: true, Question: true, Frustration: true, Steering: true, Other: true}
)

// Judgement is the model's answer on one remark: keep or drop, the
// category, and the reason in plain words.
type Judgement struct {
	Keep     bool   `json:"keep"`
	Category string `json:"category"`
	Reason   string `json:"reason"`
}

// Check is code's check of a judgement: a kept remark needs a keep category
// and a reason (a keep without a stated future-relevance reason is
// refused), a dropped one a drop category or a reason. It returns the
// judgement normalized and the problem, "" when it stands. A refused keep
// is never kept.
func Check(j *Judgement) (Judgement, string) {
	if j == nil {
		return Judgement{}, "no relevance judgement: the remark's future use is not stated"
	}
	out := Judgement{Keep: j.Keep, Category: strings.ToLower(strings.TrimSpace(j.Category)), Reason: clip(strings.Join(strings.Fields(j.Reason), " "), 300)}
	if out.Keep {
		switch {
		case !Keeps[out.Category]:
			return out, "a kept remark needs its future use: instruction, decision, preference, fact or correction, not " + quote(out.Category)
		case out.Reason == "":
			return out, "a kept remark needs a reason it matters to future work"
		}
		return out, ""
	}
	if !Drops[out.Category] && out.Reason == "" {
		return out, "a dropped remark needs why: acknowledgement, question, frustration, steering or other, or a reason"
	}
	if !Drops[out.Category] {
		out.Category = Other
	}
	return out, ""
}

// Kept reports a judgement that keeps the remark and passes Check.
func Kept(j *Judgement) bool {
	c, p := Check(j)
	return p == "" && c.Keep
}

// String is the judgement as one line: "keep instruction: why" or
// "drop steering: why".
func (j Judgement) String() string {
	k := "drop"
	if j.Keep {
		k = "keep"
	}
	s := k + " " + j.Category
	if j.Reason != "" {
		s += ": " + j.Reason
	}
	return s
}

// Rule is the gate as a model is told it, for any prompt that turns the
// owner's words into a statement or a ruling.
const Rule = `Not every sentence of the owner's is a statement. Keep a remark of his only when it would matter to future work, and say which way: "instruction" (how work should be done), "decision" (a choice made), "preference" (what he likes or wants), "fact" (something about a project, a machine or the world) or "correction" (he corrects something wrong). Drop acknowledgements ("ok", "go", "check", "yes do it"): "acknowledgement"; passing questions: "question"; frustration: "frustration"; and one-time task steering ("run it again", "now push"): "steering". Give "relevance": {"keep": true or false, "category": "<one of these>", "reason": "<one plain sentence: why it matters to later work, or why not>"}.`

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func quote(s string) string {
	if s == "" {
		return "none"
	}
	return `"` + s + `"`
}
