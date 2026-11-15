package main

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/agent-sh/tacit/client/internal/search"
)

func TestAutomationQueryKeepsTheActionableTail(t *testing.T) {
	request := "Inspect the amber queue failure and report its root cause."
	prompt := strings.Repeat("scheduled maintenance preamble with shared instructions ", 40) + request
	got := automationQuery(prompt)
	if !strings.HasSuffix(got, request) {
		t.Fatalf("the request was lost: %q", got)
	}
	if strings.Count(got, "scheduled maintenance preamble") >= strings.Count(prompt, "scheduled maintenance preamble") {
		t.Fatalf("the repeated preamble was not reduced: %q", got)
	}
	if utf8.RuneCountInString(got) > automationTail {
		t.Fatalf("query has %d runes, cap %d", utf8.RuneCountInString(got), automationTail)
	}
	short := "  inspect the amber queue  "
	if got := automationQuery(short); got != "inspect the amber queue" {
		t.Fatalf("short query changed unexpectedly: %q", got)
	}
	unicodePrompt := strings.Repeat("🙂", automationTail+20) + request
	if got := automationQuery(unicodePrompt); !utf8.ValidString(got) || !strings.HasSuffix(got, request) {
		t.Fatalf("unicode query was cut incorrectly: %q", got)
	}
}

func TestQueryForPromptLimitsOnlyRepoLessAutomation(t *testing.T) {
	prompt := strings.Repeat("wrapper ", 200) + "inspect the amber queue"
	for _, tc := range []struct {
		name       string
		context    string
		repo       string
		workspaces int
		trim       bool
	}{
		{name: "cron without a repository", context: "cron", trim: true},
		{name: "subagent without a repository", context: "subagent", trim: true},
		{name: "primary", context: "primary"},
		{name: "unknown harness"},
		{name: "repository", context: "cron", repo: "widgets"},
		{name: "workspace", context: "cron", workspaces: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := queryForPrompt(hookInput{AgentContext: tc.context}, search.Situation{Repo: tc.repo}, tc.workspaces, prompt)
			if tc.trim && got != automationQuery(prompt) {
				t.Fatalf("query was not trimmed: %q", got)
			}
			if !tc.trim && got != prompt {
				t.Fatalf("full prompt changed: %q", got)
			}
		})
	}
}
