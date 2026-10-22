package fact

import "strings"

// TypeDoc documents one fact type: what it is for and who may write it.
type TypeDoc struct {
	Name string
	Doc  string
	// Generated: only code writes it (the curator's git steps, derived/ and
	// results); a model or an agent may not.
	Generated bool
	// InboxOnly: allowed only on an inbox note, never on a curated fact.
	InboxOnly bool
	// UserScope: allowed only in the user scope.
	UserScope bool
	// Owner: written by code only, from the owner's own words it verified
	// (a statement): a model or an agent never gives the type itself.
	Owner bool
}

// TypeDocs is the defined list of types, in the order prompts show them.
// Types (the set lint L3 checks) holds the same names.
var TypeDocs = []TypeDoc{
	{Name: "user_standard", Doc: "a standard or preference of the owner's: how they want work done"},
	{Name: "design_decision", Doc: "a decision about how something is built, with its reason"},
	{Name: "past_mistake", Doc: "something that went wrong (a command, an assumption) and what to do instead"},
	{Name: "machine_fact", Doc: "a fact about one machine: paths, tools and their quirks, services, ports, hardware, what is dead there"},
	{Name: "code_change", Doc: "a change to code and what it means for the next agent working there"},
	{Name: "structure", Doc: "how code or a system is laid out: where things are and how they connect"},
	{Name: "history", Doc: "what happened and when, where it still matters now"},
	{Name: "user", Doc: "who the owner is (role, background, how they work); user scope only", UserScope: true},
	{Name: "reference", Doc: "a pointer to an external system, document or dashboard and what it holds"},
	{Name: "hot_spot", Doc: "generated from git by the curator: a repo's most-changed files and their co-change partners", Generated: true},
	{Name: "result", Doc: "generated from git: measured numbers a repo's history records, quotable by hooks", Generated: true},
	{Name: "result_ref", Doc: "generated from git: measured numbers only search finds", Generated: true},
	{Name: "unclassified", Doc: "an inbox note not typed yet; the curator assigns a real type", InboxOnly: true},
	{Name: StatementType, Doc: "the owner's own words, said once: verbatim, with the day, the session and what was being discussed; written by code from their verified words, never a rule", Owner: true},
}

// TypeDocOf is the documentation of a type, and whether it is a known one.
func TypeDocOf(name string) (TypeDoc, bool) {
	for _, d := range TypeDocs {
		if d.Name == name {
			return d, true
		}
	}
	return TypeDoc{}, false
}

// ModelTypes are the types a model or an agent may give a curated fact.
func ModelTypes() []string {
	var out []string
	for _, d := range TypeDocs {
		if !d.Generated && !d.InboxOnly && !d.Owner {
			out = append(out, d.Name)
		}
	}
	return out
}

// ModelTypeLines documents ModelTypes, one "- name: doc" line each, for a
// prompt.
func ModelTypeLines() string {
	var b strings.Builder
	for _, d := range TypeDocs {
		if !d.Generated && !d.InboxOnly && !d.Owner {
			b.WriteString("  - " + d.Name + ": " + d.Doc + "\n")
		}
	}
	return b.String()
}
