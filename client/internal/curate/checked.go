package curate

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/modelrun"
)

func jsonAnswer(text string) string {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "```json") {
		text = strings.TrimPrefix(text, "```json")
	} else if strings.HasPrefix(text, "```") {
		text = strings.TrimPrefix(text, "```")
	}
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(text), "```"))
}

// Problem is why one entry of a curator answer failed validation: the
// entry (1-based), the field and the rule it broke. It never holds the
// entry's text, so it can be logged.
type Problem struct {
	Entry int
	Field string
	Rule  string
}

func (p Problem) String() string { return fmt.Sprintf("entry %d: %s: %s", p.Entry, p.Field, p.Rule) }

// InvalidEntries is an answer rejected for its entries; it wraps
// modelrun.ErrInvalidResult and lists every problem, so a caller can log
// one line per entry and field.
type InvalidEntries struct{ Problems []Problem }

func (e *InvalidEntries) Error() string {
	msg := fmt.Sprintf("%v: invalid curator entry %s", modelrun.ErrInvalidResult, e.Problems[0])
	if n := len(e.Problems) - 1; n > 0 {
		msg += fmt.Sprintf(" (and %d more)", n)
	}
	return msg
}

func (e *InvalidEntries) Unwrap() error { return modelrun.ErrInvalidResult }

// Problems lists the entry problems of an error from ParseChecked, or none.
func Problems(err error) []Problem {
	var inv *InvalidEntries
	if errors.As(err, &inv) {
		return inv.Problems
	}
	return nil
}

// ParseChecked rejects an entire answer if any row is invalid. Empty [] is a
// successful extraction; malformed output must leave its notes pending. An
// answer rejected for its entries is an *InvalidEntries naming every
// problem.
func ParseChecked(text string) ([]Entry, error) {
	text = jsonAnswer(text)
	if !strings.HasPrefix(text, "[") {
		return nil, fmt.Errorf("%w: curator must return a JSON array", modelrun.ErrInvalidResult)
	}
	var rows []json.RawMessage
	if err := json.Unmarshal([]byte(text), &rows); err != nil {
		return nil, fmt.Errorf("%w: malformed curator array", modelrun.ErrInvalidResult)
	}
	entries := make([]Entry, 0, len(rows))
	var problems []Problem
	for i, row := range rows {
		bad := func(field, rule string) { problems = append(problems, Problem{Entry: i + 1, Field: field, Rule: rule}) }
		var entry Entry
		if !strings.HasPrefix(strings.TrimSpace(string(row)), "{") {
			bad("entry", "not a JSON object")
			continue
		}
		if err := json.Unmarshal(row, &entry); err != nil {
			bad("entry", "a field has the wrong JSON type")
			continue
		}
		for _, f := range []struct{ name, value string }{{"origin", entry.Origin}, {"name", entry.Name}, {"description", entry.Description}, {"body", entry.Body}} {
			if strings.TrimSpace(f.value) == "" {
				bad(f.name, "empty")
			}
		}
		if rule := scopeRule(entry.Scope); rule != "" {
			bad("scope", rule)
		}
		switch {
		case entry.Type == "unclassified":
			bad("type", "unclassified is for inbox notes only")
		case !fact.Types[entry.Type]:
			bad("type", "not a known type")
		}
		switch {
		case entry.Kind == "derived":
			bad("kind", "derived is for generated facts only")
		case !fact.SourceKinds[entry.Kind]:
			bad("kind", "not user, observed or web")
		}
		for _, key := range sortedKeys(entry.AppliesTo) {
			for _, value := range entry.AppliesTo[key] {
				if _, ok := value.(string); !ok {
					bad("applies_to."+key, "a value is not a string")
					break
				}
			}
		}
		entries = append(entries, entry)
	}
	if len(problems) > 0 {
		return nil, &InvalidEntries{Problems: problems}
	}
	return entries, nil
}

// scopeRule says which rule a scope breaks, "" when it is valid. It never
// repeats the scope.
func scopeRule(scope string) string {
	switch {
	case scopeRx.MatchString(scope):
		return ""
	case scope == "":
		return "empty"
	case scopeRx.MatchString(strings.ToLower(scope)):
		return "has upper-case letters"
	case strings.HasPrefix(scope, "projects/") || strings.HasPrefix(scope, "machines/"):
		return "the repo or host is not a slug (a-z, 0-9, -)"
	case strings.HasPrefix(scope, "derived/"):
		return "derived/ is for generated facts only"
	}
	return "not user, system, projects/<slug> or machines/<slug>"
}

func sortedKeys(m map[string][]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func ParseVerdictsChecked(text string, n int) ([]Verdict, error) {
	text = jsonAnswer(text)
	if !strings.HasPrefix(text, "[") {
		return nil, fmt.Errorf("%w: judge must return a JSON array", modelrun.ErrInvalidResult)
	}
	var labels []string
	if err := json.Unmarshal([]byte(text), &labels); err != nil || len(labels) != n {
		return nil, fmt.Errorf("%w: judge must return exactly %d verdicts", modelrun.ErrInvalidResult, n)
	}
	verdicts := make([]Verdict, n)
	for i, label := range labels {
		switch strings.ToLower(strings.TrimSpace(label)) {
		case "no":
			verdicts[i] = NoConflict
		case "contradicts":
			verdicts[i] = Contradicts
		case "supersedes":
			verdicts[i] = Supersedes
		default:
			return nil, fmt.Errorf("%w: unknown judge verdict at %d", modelrun.ErrInvalidResult, i+1)
		}
	}
	return verdicts, nil
}
