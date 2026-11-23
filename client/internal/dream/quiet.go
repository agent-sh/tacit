package dream

import (
	"sort"
	"strings"
)

// Quiet types (SPEC.md, Dreamer, "Quiet types"): types whose facts the
// hooks deliver far less than the others, measured from this machine's
// delivery log. The dreamer is not offered them and writes no agent's fact
// of them; the owner's words (kind user or statement) are written whatever
// their type. Facts of these types already in the store stay, searchable.

// dreamTypes are the types the dreamer may write, in the prompt's order.
var dreamTypes = []string{"user_standard", "machine_fact", "past_mistake", "design_decision", "structure", "history"}

// typeList is the prompt's and the schema's list of types.
const typeList = "user_standard|machine_fact|past_mistake|design_decision|structure|history"

// Quiet is the run's quiet types; empty for none (the prompt as before).
var Quiet = map[string]bool{}

// Offered are the dreamer's types less the quiet ones.
func Offered() []string {
	var out []string
	for _, t := range dreamTypes {
		if !Quiet[t] {
			out = append(out, t)
		}
	}
	return out
}

// QuietList is the quiet types, sorted.
func QuietList() []string {
	var out []string
	for t, ok := range Quiet {
		if ok {
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}

// withOffered is the prompt with the quiet types out of its list of types,
// and one line that says what to do with such a fact.
func withOffered(prompt string) string {
	q := QuietList()
	if len(q) == 0 {
		return prompt
	}
	p := strings.Replace(prompt, typeList, strings.Join(Offered(), "|"), 1)
	note := "Write no fact whose only type would be " + strings.Join(q, " or ") + ": such facts are almost never needed by later sessions. The owner's words (kind user or statement) are always written, with the type that fits best.\n"
	if i := strings.Index(p, "Output ONLY a JSON array"); i >= 0 {
		return p[:i] + note + p[i:]
	}
	return p
}

// schema is the output schema with the quiet types out of its enum.
func schema() string {
	if len(QuietList()) == 0 {
		return outputSchema
	}
	old := `"enum":["user_standard","machine_fact","past_mistake","design_decision","structure","history"]`
	var qs []string
	for _, t := range Offered() {
		qs = append(qs, `"`+t+`"`)
	}
	return strings.Replace(outputSchema, old, `"enum":[`+strings.Join(qs, ",")+`]`, 1)
}

// DropQuiet reports an extraction the run does not write: an agent's
// fact of a quiet type.
func DropQuiet(x Extracted) bool {
	return Quiet[x.Type] && x.Kind != "user" && x.Kind != kindStatement
}
