package events

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/agent-sh/tacit/client/internal/digest"
)

// Redact masks secret-shaped text in events' Text, ToolInput and ToolOutput
// with scrump's text profile, the one redactor tacit writes with: the texts
// of all of them in one scrump call where it can (digest.RedactAllContext;
// a text holding a record separator alone, digest.RedactErr). It fails
// closed: when scrump is missing or fails it returns nil and the error,
// never an event it could not redact. Each scrump call has 60 s, so a
// caller redacts a few megabytes at a time.
func Redact(ctx context.Context, evs []Event) ([]Event, error) {
	out := slices.Clone(evs)
	var batch []string
	var where []*string
	for i := range out {
		for _, f := range []*string{&out[i].Text, &out[i].ToolInput, &out[i].ToolOutput} {
			switch {
			case *f == "":
			case strings.Contains(*f, "\x1e"):
				s, err := digest.RedactErr(*f)
				if err != nil {
					return nil, err
				}
				*f = s
			default:
				batch, where = append(batch, *f), append(where, f)
			}
		}
	}
	if len(batch) == 0 {
		return out, nil
	}
	red, err := digest.RedactAllContext(ctx, batch)
	if err != nil {
		return nil, err
	}
	if len(red) != len(batch) {
		return nil, errors.New("redaction returned a different number of texts")
	}
	for i, f := range where {
		*f = red[i]
	}
	return out, nil
}
