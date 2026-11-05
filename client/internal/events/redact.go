package events

import (
	"context"
	"errors"
	"slices"

	"github.com/agent-sh/tacit/client/internal/digest"
)

// Redact masks secret-shaped text in events' Text, ToolInput and ToolOutput
// with scrump's text profile, the one redactor tacit writes with: the texts
// of all of them in one scrump call, each masked as it would be alone
// (digest.RedactAllContext). It fails closed: when scrump is missing or fails it
// returns nil and the error, never an event it could not redact. Each scrump
// call has 60 s, so a caller redacts a few megabytes at a time.
func Redact(ctx context.Context, evs []Event) ([]Event, error) {
	return redactWith(evs, func(texts []string) ([]string, error) {
		return digest.RedactAllContext(ctx, texts)
	})
}

// Redactor redacts one command's batches of events as Redact does, each in a
// scrump call started ahead of it: the first when the Redactor is made, so
// scrump loads its rules while the caller reads the transcript, and the
// next one while a batch is redacted, when the caller says more may follow.
// Close ends a call started for a batch that never came.
type Redactor struct{ next *digest.Warm }

// NewRedactor starts the first batch's scrump call.
func NewRedactor() *Redactor { return &Redactor{next: digest.StartWarm()} }

// Redact is Redact on the call started ahead; more starts the next batch's.
func (r *Redactor) Redact(ctx context.Context, evs []Event, more bool) ([]Event, error) {
	w := r.next
	r.next = nil
	if w == nil {
		w = digest.StartWarm()
	}
	if more {
		r.next = digest.StartWarm()
	}
	defer w.End()
	return redactWith(evs, func(texts []string) ([]string, error) {
		return w.RedactAll(ctx, texts)
	})
}

// Close ends the call started for a next batch, if any.
func (r *Redactor) Close() {
	if r.next != nil {
		r.next.End()
		r.next = nil
	}
}

func redactWith(evs []Event, redact func([]string) ([]string, error)) ([]Event, error) {
	out := slices.Clone(evs)
	var batch []string
	var where []*string
	for i := range out {
		for _, f := range []*string{&out[i].Text, &out[i].ToolInput, &out[i].ToolOutput} {
			if *f != "" {
				batch, where = append(batch, *f), append(where, f)
			}
		}
	}
	if len(batch) == 0 {
		return out, nil
	}
	red, err := redact(batch)
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
