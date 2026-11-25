package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-sh/tacit/client/internal/fact"
)

// gateRig is a store whose prompt block for gateRequestText holds two lines,
// with a gate set on Bedrock and the redactor stubbed.
func gateRig(t *testing.T) (cwd, state string) {
	t.Helper()
	stubScrump(t)
	return gateStore(t)
}

// gateStore is gateRig with the redactor as installed.
func gateStore(t *testing.T) (cwd, state string) {
	t.Helper()
	_, cwd = repoFirstRig(t,
		fact.Meta{Name: "scheduler-shard-locks", Description: "The scheduler pool keeps one shard lock per tenant and a slot lock per job", Type: "design_decision", Scope: "projects/widget", AppliesTo: widgetRepo},
		fact.Meta{Name: "slot-lock-order", Description: "Take the shard lock before the slot lock in the scheduler pool, never the other way", Type: "past_mistake", Scope: "projects/widget", AppliesTo: widgetRepo},
	)
	state = filepath.Join(t.TempDir(), "state")
	t.Setenv("TACIT_STATE_DIR", state)
	t.Setenv("TACIT_GATE_PROVIDER", "bedrock")
	t.Setenv("TACIT_GATE_MODEL", "demo.gate-model")
	t.Setenv("TACIT_GATE_THRESHOLD", "0.3")
	t.Setenv("TACIT_GATE_FIELDS", `{"thinking":{"type":"off"}}`)
	return cwd, state
}

const gateRequestText = "why does the scheduler pool hold a shard lock for each tenant"

// converseReply is a Converse answer whose text is text.
func converseReply(text string) map[string]any {
	return map[string]any{"output": map[string]any{"message": map[string]any{"content": []map[string]string{{"text": text}}}}, "usage": map[string]int{"inputTokens": 300}}
}

// gateUser is the user turn of a Converse request body.
func gateUser(body map[string]any) string {
	msgs, _ := body["messages"].([]any)
	if len(msgs) == 0 {
		return ""
	}
	content, _ := msgs[0].(map[string]any)["content"].([]any)
	if len(content) == 0 {
		return ""
	}
	s, _ := content[0].(map[string]any)["text"].(string)
	return s
}

// lineScores scores each numbered line of the user turn: low for the slot
// lock line, high for the rest.
func lineScores(user string) string {
	var parts []string
	for _, l := range strings.Split(user, "\n") {
		var i int
		var rest string
		if n, _ := fmt.Sscanf(l, "%d: %s", &i, &rest); n == 2 {
			v := 90
			if strings.HasPrefix(rest, "slot-lock-order") {
				v = 10
			}
			parts = append(parts, fmt.Sprintf(`"%d": %d`, i, v))
		}
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// The gate scores the prompt block's lines for the request and leaves out
// the ones under its threshold; the delivery log keeps each line's score,
// the gate as what left it out, and the gate's status.
func TestGateLeavesOutTheLinesItScoresUnderItsThreshold(t *testing.T) {
	cwd, state := gateRig(t)
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "bedrock-placeholder")
	calls := fakeTransport(t, func(r *http.Request, body map[string]any) (int, any) {
		if r.URL.String() != "https://bedrock-runtime.us-east-1.amazonaws.com/model/demo.gate-model/converse" {
			t.Errorf("endpoint %s", r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer bedrock-placeholder" {
			t.Errorf("credential %q", r.Header.Get("Authorization"))
		}
		if f, _ := body["additionalModelRequestFields"].(map[string]any); f["thinking"] == nil {
			t.Errorf("the request fields were not sent: %v", body)
		}
		user := gateUser(body)
		if !strings.Contains(user, "Session repository: widget") || !strings.Contains(user, gateRequestText) || !strings.Contains(user, "slot-lock-order: Take the shard lock") {
			t.Errorf("user turn: %q", user)
		}
		if strings.Contains(user, "Zq7Vx9") { // the stub redactor's canary: redacted before it is sent
			t.Errorf("sent unredacted: %q", user)
		}
		return 200, converseReply(lineScores(user))
	})
	out := hookOut(t, promptPayload(cwd, "g1", gateRequestText+" with the key Zq7Vx9"), "prompt", "--no-mcp")
	if *calls != 1 || !strings.Contains(out, "scheduler-shard-locks") || strings.Contains(out, "slot-lock-order") {
		t.Fatalf("calls %d, block: %s", *calls, out)
	}
	var found bool
	for _, e := range readDeliveryLog(t, state) {
		if e.Hook != "prompt" {
			continue
		}
		found = true
		if e.Gate == nil || e.Gate.Status != "ok" || e.Gate.Threshold != 0.3 {
			t.Fatalf("gate entry: %+v", e.Gate)
		}
		if c := candOf(e, "slot-lock-order"); c == nil || c.Delivered || c.Dropped != "gate" || c.Gate == nil || *c.Gate != 0.1 {
			t.Fatalf("the dropped line: %+v", c)
		}
		if c := candOf(e, "scheduler-shard-locks"); c == nil || !c.Delivered || c.Gate == nil || *c.Gate != 0.9 {
			t.Fatalf("the kept line: %+v", c)
		}
	}
	if !found {
		t.Fatal("no prompt entry")
	}
}

// On the loopback route (a Bedrock endpoint on this machine with the auth
// skipped, read from the key file) the call goes there with no credential;
// a base URL off this machine is no route, and nothing is sent.
func TestGateLoopbackRouteFromTheKeyFile(t *testing.T) {
	cwd, _ := gateRig(t)
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "")
	t.Setenv("ANTHROPIC_BEDROCK_BASE_URL", "")
	t.Setenv("CLAUDE_CODE_SKIP_BEDROCK_AUTH", "")
	keys := filepath.Join(t.TempDir(), "route.env")
	t.Setenv("TACIT_GATE_KEY_FILE", keys)
	os.WriteFile(keys, []byte("CLAUDE_CODE_USE_BEDROCK=1\nANTHROPIC_BEDROCK_BASE_URL=\"http://127.0.0.1:41999/route-path\"\nCLAUDE_CODE_SKIP_BEDROCK_AUTH=\"1\"\n"), 0o600)
	calls := fakeTransport(t, func(r *http.Request, body map[string]any) (int, any) {
		if r.URL.String() != "http://127.0.0.1:41999/route-path/model/demo.gate-model/converse" || r.Header.Get("Authorization") != "" {
			t.Errorf("endpoint %s, credential %q", r.URL, r.Header.Get("Authorization"))
		}
		return 200, converseReply(lineScores(gateUser(body)))
	})
	if out := hookOut(t, promptPayload(cwd, "g2", gateRequestText), "prompt", "--no-mcp"); *calls != 1 || strings.Contains(out, "slot-lock-order") || !strings.Contains(out, "scheduler-shard-locks") {
		t.Fatalf("calls %d, block: %s", *calls, out)
	}
	os.WriteFile(keys, []byte("ANTHROPIC_BEDROCK_BASE_URL=http://gateway.example:8080/x\nCLAUDE_CODE_SKIP_BEDROCK_AUTH=1\n"), 0o600)
	t.Setenv("TACIT_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	if out := hookOut(t, promptPayload(cwd, "g3", gateRequestText), "prompt", "--no-mcp"); *calls != 1 || !strings.Contains(out, "slot-lock-order") {
		t.Fatalf("an endpoint off this machine: calls %d, block: %s", *calls, out)
	}
	if s := gateStatus(t, os.Getenv("TACIT_STATE_DIR")); s != "no_route" {
		t.Fatalf("gate status %q", s)
	}
}

// A gate that fails, answers badly or runs past its deadline leaves the
// block as the selection made it, and says why in the log.
func TestGateFailsOpen(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		reply        func() (int, any)
	}{
		{"server error", "http_500", func() (int, any) { return 500, map[string]string{"message": "boom"} }},
		{"no score for a line", "response_invalid", func() (int, any) { return 200, converseReply(`{"0": 90}`) }},
		{"a score out of range", "response_invalid", func() (int, any) { return 200, converseReply(`{"0": 90, "1": 400}`) }},
		{"past the deadline", "deadline", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cwd, state := gateRig(t)
			t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "bedrock-placeholder")
			t.Setenv("TACIT_GATE_TIMEOUT", "2s")
			if tc.reply == nil {
				t.Setenv("TACIT_GATE_TIMEOUT", "100ms")
			}
			fakeTransport(t, func(r *http.Request, _ map[string]any) (int, any) {
				if tc.reply != nil {
					return tc.reply()
				}
				// A call that answers only after the deadline, and ends when
				// the gate's cancellation reaches it.
				select {
				case <-r.Context().Done():
					return 499, nil
				case <-time.After(5 * time.Second):
					return 200, converseReply(`{"0": 1, "1": 1}`)
				}
			})
			start := time.Now()
			out := hookOut(t, promptPayload(cwd, "g4", gateRequestText), "prompt", "--no-mcp")
			if !strings.Contains(out, "slot-lock-order") || !strings.Contains(out, "scheduler-shard-locks") {
				t.Fatalf("block: %s", out)
			}
			if tc.status == "deadline" && time.Since(start) > 2*time.Second {
				t.Fatalf("the hook waited %s past a 100ms gate", time.Since(start))
			}
			if s := gateStatus(t, state); s != tc.status {
				t.Fatalf("gate status %q, want %s", s, tc.status)
			}
		})
	}
}

// gateStatus is the gate's status in the one prompt entry of the log.
func gateStatus(t *testing.T, state string) string {
	t.Helper()
	var got []string
	for _, e := range readDeliveryLog(t, state) {
		if e.Hook == "prompt" {
			if e.Gate == nil {
				got = append(got, "")
			} else {
				got = append(got, e.Gate.Status)
			}
		}
	}
	if len(got) != 1 {
		t.Fatalf("prompt entries' gate statuses: %q", got)
	}
	return got[0]
}

// A redactor that fails sends nothing: the block stands, and the log says
// why.
func TestGateSendsNothingWhenRedactionFails(t *testing.T) {
	cwd, state := gateStore(t)
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "scrump"), []byte("#!/bin/sh\nexit 1\n"), 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "bedrock-placeholder")
	calls := fakeTransport(t, func(*http.Request, map[string]any) (int, any) { return 200, converseReply(`{"0": 1, "1": 1}`) })
	if out := hookOut(t, promptPayload(cwd, "g6", gateRequestText), "prompt", "--no-mcp"); *calls != 0 || !strings.Contains(out, "slot-lock-order") {
		t.Fatalf("calls %d, block: %s", *calls, out)
	}
	if s := gateStatus(t, state); s != "redaction_failed" {
		t.Fatalf("gate status %q", s)
	}
}

// A request of several lines is redacted before it is flattened: a password
// on the line after an open setting's value is masked, as it is alone.
func TestGateRedactsEachLineOfTheRequest(t *testing.T) {
	requireScrump(t)
	cwd, _ := gateStore(t)
	t.Setenv("AWS_BEARER_TOKEN_BEDROCK", "bedrock-placeholder")
	secret := "Zq7Vx9" + "Lm2Kp4"
	request := gateRequestText + "\ndeploy token_url: 'https://auth.example.invalid/oauth/" + strings.Repeat("p", 80) + "\n" + "pass" + "word: '" + secret + "' and the rest"
	var sent string
	fakeTransport(t, func(_ *http.Request, body map[string]any) (int, any) {
		sent = gateUser(body)
		return 200, converseReply(lineScores(sent))
	})
	hookOut(t, promptPayload(cwd, "g5", request), "prompt", "--no-mcp")
	if sent == "" || strings.Contains(sent, secret) {
		t.Fatalf("sent: %q", sent)
	}
}

// Cut with its lines kept, a text clipped after redaction reads as the
// clip of the clip did (judgeClip twice), so the judge's cases and the
// gate's request read the same text, less what redaction masks.
func TestRuneCutReadsAsTheClipDid(t *testing.T) {
	for _, s := range []string{"", "  a  b\n\n c ", strings.Repeat("word \n\t  ", 900), "x" + strings.Repeat(" ", 5000) + "y z", "\u00e4\u00f6  \u00fc\n\u00df " + strings.Repeat("ab ", 2000)} {
		for _, n := range []int{1, 3, 10, 800} {
			if got, want := judgeClip(runeCut(s, 4*n), n), judgeClip(judgeClip(s, 4*n), n); got != want {
				t.Fatalf("n %d: %q, want %q", n, got, want)
			}
		}
	}
}
