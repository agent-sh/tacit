package dream

import (
	"context"
	"sort"
	"strings"
)

// Quiet types (SPEC.md, Dreamer, "Quiet types"): types whose facts the
// hooks deliver far less than the others, measured from this machine's
// delivery log. The dreamer is not offered them and writes no agent's fact
// of them; the owner's words (kind user or statement) are written whatever
// their type. Facts of these types already in the store stay, searchable.

// Types are the types the dreamer may write, in the prompt's order.
var Types = []string{"user_standard", "machine_fact", "past_mistake", "design_decision", "structure", "history"}

// typeList is the prompt's list of types.
var typeList = strings.Join(Types, "|")

// schemaEnum is the output schema's enum of types.
var schemaEnum = func() string {
	var qs []string
	for _, t := range Types {
		qs = append(qs, `"`+t+`"`)
	}
	return `"enum":[` + strings.Join(qs, ",") + `]`
}()

// Quiet is a set of quiet types; nil or empty for none.
type Quiet map[string]bool

// List is the quiet types among Types, sorted.
func (q Quiet) List() []string {
	var out []string
	for _, t := range Types {
		if q[t] {
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}

// offered are the dreamer's types less the quiet ones.
func (q Quiet) offered() []string {
	var out []string
	for _, t := range Types {
		if !q[t] {
			out = append(out, t)
		}
	}
	return out
}

// prompt is Prompt with the quiet types out of its list of types, and one
// line that says what to do with such a fact; Prompt itself with none.
func (q Quiet) prompt() string {
	ql := q.List()
	if len(ql) == 0 {
		return Prompt
	}
	p := strings.Replace(Prompt, typeList, strings.Join(q.offered(), "|"), 1)
	note := "Write no fact whose only type would be " + strings.Join(ql, " or ") + ": such facts are almost never needed by later sessions. The owner's words (kind user or statement) are always written, with the type that fits best.\n"
	if i := strings.Index(p, "Output ONLY a JSON array"); i >= 0 {
		return p[:i] + note + p[i:]
	}
	return p
}

// schema is the output schema with the quiet types out of its enum.
func (q Quiet) schema() string {
	if len(q.List()) == 0 {
		return outputSchema
	}
	var qs []string
	for _, t := range q.offered() {
		qs = append(qs, `"`+t+`"`)
	}
	return strings.Replace(outputSchema, schemaEnum, `"enum":[`+strings.Join(qs, ",")+`]`, 1)
}

// Drops reports an extraction the run does not write: an agent's fact of
// a quiet type.
func (q Quiet) Drops(x Extracted) bool {
	return q[x.Type] && x.Kind != "user" && x.Kind != kindStatement
}

type quietKey struct{}

// WithQuiet is ctx carrying the run's quiet types, which RunCLI's schema
// leaves out.
func WithQuiet(ctx context.Context, q Quiet) context.Context {
	return context.WithValue(ctx, quietKey{}, q)
}

func quietOf(ctx context.Context) Quiet {
	q, _ := ctx.Value(quietKey{}).(Quiet)
	return q
}
