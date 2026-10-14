package curate

import (
	"encoding/json"
	"fmt"
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

// ParseChecked rejects an entire answer if any row is invalid. Empty [] is a
// successful extraction; malformed output must leave its notes pending.
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
	for i, row := range rows {
		var entry Entry
		if !strings.HasPrefix(strings.TrimSpace(string(row)), "{") || json.Unmarshal(row, &entry) != nil || strings.TrimSpace(entry.Origin) == "" || strings.TrimSpace(entry.Description) == "" || strings.TrimSpace(entry.Name) == "" || strings.TrimSpace(entry.Body) == "" ||
			!scopeRx.MatchString(entry.Scope) || !fact.Types[entry.Type] || entry.Type == "unclassified" ||
			!fact.SourceKinds[entry.Kind] || entry.Kind == "derived" {
			return nil, fmt.Errorf("%w: invalid curator entry %d", modelrun.ErrInvalidResult, i+1)
		}
		for _, values := range entry.AppliesTo {
			for _, value := range values {
				if _, ok := value.(string); !ok {
					return nil, fmt.Errorf("%w: invalid applicability in entry %d", modelrun.ErrInvalidResult, i+1)
				}
			}
		}
		entries = append(entries, entry)
	}
	return entries, nil
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
