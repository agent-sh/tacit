package events

import (
	"context"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/digest"
)

// secret is a secret-shaped value, built from pieces so none sits in the
// source.
var secret = "pass" + "word=" + "Zq7Vx9" + "Lm2Kp4"

// Without scrump nothing comes back: no event with text that was not
// redacted.
func TestRedactFailsClosed(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	evs := []Event{{Kind: KindToolResult, ToolOutput: "the db " + secret}}
	out, err := Redact(context.Background(), evs)
	if err == nil || out != nil {
		t.Fatalf("redacted without scrump: %v %+v", err, out)
	}
	if evs[0].ToolOutput != "the db "+secret {
		t.Fatal("the caller's events were changed")
	}
}

func TestRedactMasksEveryTextField(t *testing.T) {
	if err := digest.RedactionReady(); err != nil {
		t.Skip("needs scrump with the text profile: " + err.Error())
	}
	evs := []Event{
		{Kind: KindOwnerText, Text: "rotate it, the old " + secret},
		{Kind: KindToolCall, ToolInput: `{"command":"export ` + secret + `"}`},
		{Kind: KindToolResult, ToolOutput: "binary\x1eoutput " + secret},
		{Kind: KindAgentText, Text: "nothing secret here"},
	}
	out, err := Redact(context.Background(), evs)
	if err != nil {
		t.Fatal(err)
	}
	for i, e := range out {
		for _, s := range []string{e.Text, e.ToolInput, e.ToolOutput} {
			if strings.Contains(s, "Zq7Vx9") {
				t.Errorf("event %d keeps the secret: %q", i, s)
			}
		}
	}
	if out[3].Text != "nothing secret here" || !strings.HasPrefix(out[0].Text, "rotate it, the old ") || len(out[0].Text) != len(evs[0].Text) {
		t.Errorf("redaction changed more than the secret: %q", out[0].Text)
	}
	if !strings.Contains(evs[0].Text, "Zq7Vx9") {
		t.Fatal("the caller's events were changed")
	}
}
