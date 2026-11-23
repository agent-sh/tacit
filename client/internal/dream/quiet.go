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

// prompt is Prompt with one line that says an agent's fact of a quiet type
// is not written; Prompt itself with none. The list of types stays whole:
// the owner's words of any type are written.
func (q Quiet) prompt() string {
	ql := q.List()
	if len(ql) == 0 {
		return Prompt
	}
	p := Prompt
	note := "Write no fact of kind observed whose only type would be " + strings.Join(ql, " or ") + ": such facts are almost never needed by later sessions. The owner's words (kind user or statement) are always written, with the type that fits best, " + strings.Join(ql, " or ") + " included.\n"
	if i := strings.Index(p, "Output ONLY a JSON array"); i >= 0 {
		return p[:i] + note + p[i:]
	}
	return p
}

// schema is the output schema. It keeps every type whatever is quiet: one
// schema holds the owner's facts and the agents', and the owner's words of
// a quiet type are written (Drops drops only an agent's fact of one).
func (q Quiet) schema() string { return outputSchema }

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
