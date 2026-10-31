package main

import (
	"testing"

	"github.com/agent-sh/tacit/client/internal/curate"
	"github.com/agent-sh/tacit/client/internal/dream"
)

// Every deciding call defaults to Opus, the conflict judge too, also when a
// job names the Claude runner for the judge explicitly (the live judge's
// route) and sets no model.
func TestDecidingCallsDefaultToOpus(t *testing.T) {
	prev := []string{curate.DefaultModel, curate.JudgeModel, dream.DefaultModel, verifyModel, verifyFallbackModel}
	t.Cleanup(func() {
		curate.DefaultModel, curate.JudgeModel, dream.DefaultModel, verifyModel, verifyFallbackModel = prev[0], prev[1], prev[2], prev[3], prev[4]
	})
	for _, k := range []string{"TACIT_RUNNER", "TACIT_CURATE_RUNNER", "TACIT_DREAM_RUNNER", "TACIT_VERIFY_RUNNER", "TACIT_CURATE_MODEL", "TACIT_JUDGE_MODEL", "TACIT_DREAM_MODEL", "TACIT_VERIFY_MODEL", "TACIT_VERIFY_FALLBACK_MODEL"} {
		t.Setenv(k, "")
	}
	for _, runner := range []string{"", "claude"} {
		t.Setenv("TACIT_JUDGE_RUNNER", runner)
		refreshModels()
		if curate.DefaultModel != "opus" || curate.JudgeModel != "opus" || dream.DefaultModel != "opus" || verifyModel != "opus" {
			t.Fatalf("judge runner %q: curate %q judge %q dream %q verify %q", runner, curate.DefaultModel, curate.JudgeModel, dream.DefaultModel, verifyModel)
		}
	}
}
