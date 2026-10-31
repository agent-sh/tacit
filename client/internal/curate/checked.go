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
	// Hint is the refused value and what to do instead, for the model's
	// retry (RetryPrompt). It may hold the entry's text, so String leaves it
	// out of logs.
	Hint string
}

func (p Problem) String() string { return fmt.Sprintf("entry %d: %s: %s", p.Entry, p.Field, p.Rule) }

// Feedback is the problem for the model: String and the hint.
func (p Problem) Feedback() string {
	if p.Hint == "" {
		return p.String()
	}
	return p.String() + " (" + p.Hint + ")"
}

// InvalidEntries is an answer rejected for its entries; it wraps
// modelrun.ErrInvalidResult and lists every problem, so a caller can log
// one line per entry and field.
type InvalidEntries struct{ Problems []Problem }

func (e *InvalidEntries) Error() string {
	msg := fmt.Sprintf("%v: invalid curator %s", modelrun.ErrInvalidResult, e.Problems[0])
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

// ErrNotArray and ErrMalformedArray are ParseChecked's refusals of an answer
// whole: one that is no JSON array (a sentence before it), and one that does
// not parse. They wrap modelrun.ErrInvalidResult; their text is fixed, never
// the answer's, so a refusal can be logged.
var (
	ErrNotArray       = errors.New("curator must return a JSON array")
	ErrMalformedArray = errors.New("malformed curator array")
)

// WholeRefusal is the fixed reason ParseChecked refused an answer whole, ""
// for any other error (an entry's problems, a runner's error).
func WholeRefusal(err error) string {
	for _, e := range []error{ErrNotArray, ErrMalformedArray} {
		if errors.Is(err, e) {
			return e.Error()
		}
	}
	return ""
}

// ParseChecked rejects an entire answer if any row is invalid. Empty [] is a
// successful extraction; malformed output must leave its notes pending. An
// answer rejected for its entries is an *InvalidEntries naming every
// problem; one rejected whole wraps ErrNotArray or ErrMalformedArray.
func ParseChecked(text string) ([]Entry, error) {
	text = jsonAnswer(text)
	if !strings.HasPrefix(text, "[") {
		return nil, fmt.Errorf("%w: %w", modelrun.ErrInvalidResult, ErrNotArray)
	}
	var rows []json.RawMessage
	if err := json.Unmarshal([]byte(text), &rows); err != nil {
		return nil, fmt.Errorf("%w: %w", modelrun.ErrInvalidResult, ErrMalformedArray)
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
		// A repo or host in capitals is the same scope in lower case: the
		// dreamer and delivery lower-case the repo, so projects/Foo would be a
		// scope no hook asks for. Normalized, not rejected: a scope that names a
		// package in capitals got a whole split answer refused for that one
		// entry (eval/VALUE.md, "Local layer").
		if !scopeRx.MatchString(entry.Scope) && scopeRx.MatchString(asciiLower(entry.Scope)) {
			entry.Scope = asciiLower(entry.Scope)
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
		case entry.Kind == KindStatement:
			if strings.TrimSpace(entry.OwnerQuote) == "" {
				bad("owner_quote", "a statement is the owner's exact words: owner_quote is empty")
			}
		case !fact.SourceKinds[entry.Kind]:
			bad("kind", "not user, observed, web or statement")
		}
		if p := entry.Propose; p != nil {
			switch {
			case p.Kind != "area" && p.Kind != "topic":
				bad("propose.kind", "not area or topic")
			case !fact.IsSlug(p.Name):
				bad("propose.name", "not a slug (a-z, 0-9, -)")
			case p.Area != "" && (p.Kind != "topic" || !fact.IsSlug(p.Area)):
				bad("propose.area", "only a topic has an area, and it is a slug")
			case strings.TrimSpace(p.Description) == "":
				bad("propose.description", "empty")
			}
		}
		for _, key := range sortedKeys(entry.AppliesTo) {
			for _, value := range entry.AppliesTo[key] {
				if _, ok := value.(string); !ok {
					// A key is the model's text: only the known ones are named.
					field := "applies_to.<unknown key>"
					if knownApplies[key] {
						field = "applies_to." + key
					}
					bad(field, "a value is not a string")
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
	case scopeRx.MatchString(asciiLower(scope)):
		return "has upper-case letters"
	case strings.HasPrefix(scope, "projects/") || strings.HasPrefix(scope, "machines/") || strings.HasPrefix(scope, "areas/") || strings.HasPrefix(scope, "topics/") || strings.HasPrefix(scope, "orgs/"):
		return "the repo or host is not a slug (a-z, 0-9, -)"
	case strings.HasPrefix(scope, "derived/"):
		return "derived/ is for generated facts only"
	}
	return "not user, system, projects/<slug>, machines/<slug>, areas/<slug>, areas/<slug>/topics/<slug>, topics/<slug> or orgs/<slug>"
}

// knownApplies are the applies_to keys a fact has (fact.AppliesTo).
var knownApplies = map[string]bool{"paths": true, "symbols": true, "repos": true, "hosts": true, "commands": true}

// asciiLower lower-cases A-Z only: a Unicode letter that lower-cases to an
// ASCII one (the Kelvin sign to k) must not turn into a valid slug.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
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
