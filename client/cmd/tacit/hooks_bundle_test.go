package main

import (
	"testing"

	"github.com/agent-sh/tacit/client/internal/search"
)

func TestWithoutNearDuplicatesKeepsDistinctAndProtectedFacts(t *testing.T) {
	a := search.Hit{Path: "machines/host-a/a.md", Name: "a", Scope: "machines/host-a", Type: "machine_fact", Description: "On host-a, the job runner resume repeats the last message after transient API error 401, which loops the failure; strip the queued continuation and send it manually"}
	b := search.Hit{Path: "machines/host-a/b.md", Name: "b", Scope: "machines/host-a", Type: "machine_fact", Description: "On host-a, job runner resume repeats the last message after transient API error 401, which loops the failure; strip any appended continuation and send it manually"}
	if got := names(withoutNearDuplicates([]search.Hit{a, b})); got != "a" && got != "b" {
		t.Fatalf("restatements: got %s, want one line", got)
	}

	otherScope := b
	otherScope.Path, otherScope.Name, otherScope.Scope = "machines/host-b/b.md", "other-scope", "machines/host-b"
	if got := names(withoutNearDuplicates([]search.Hit{a, otherScope})); got != "a,other-scope" {
		t.Errorf("different hosts: %s", got)
	}

	changed := b
	changed.Path, changed.Name = "machines/host-a/changed.md", "changed"
	changed.Description = "On host-a, the job runner resume no longer repeats the last message after transient API error 401, which loops the failure; strip the queued continuation and send it manually"
	if got := names(withoutNearDuplicates([]search.Hit{a, changed})); got != "a,changed" {
		t.Errorf("a reversal is not a copy: %s", got)
	}

	protected := b
	protected.Path, protected.Name, protected.Words = "machines/host-a/protected.md", "protected", true
	if got := names(withoutNearDuplicates([]search.Hit{a, protected})); got != "protected" {
		t.Errorf("the agent copy may go, owner evidence never does: %s", got)
	}

	systemA, systemB := a, b
	systemA.Path, systemA.Scope = "system/a.md", "system"
	systemB.Path, systemB.Scope = "system/b.md", "system"
	if got := names(withoutNearDuplicates([]search.Hit{systemA, systemB})); got != "a,b" {
		t.Errorf("system facts keep the curator's protection: %s", got)
	}

	numberedA := search.Hit{Path: "projects/widget/rule-1.md", Name: "widget-rule-1", Scope: "projects/widget", Description: "The scheduler rule applies to the demo repository"}
	numberedB := numberedA
	numberedB.Path, numberedB.Name = "projects/widget/rule-2.md", "widget-rule-2"
	if got := names(withoutNearDuplicates([]search.Hit{numberedA, numberedB})); got != "widget-rule-1,widget-rule-2" {
		t.Errorf("numbers carried only by the names still distinguish facts: %s", got)
	}

	statementA := search.Hit{Path: "user/said-one.md", Name: "said-one", Scope: "user", Type: "statement", Description: "The owner said how the review queue is run in this project"}
	statementB := statementA
	statementB.Path, statementB.Name = "user/said-two.md", "said-two"
	if got := names(withoutNearDuplicates([]search.Hit{statementA, statementB})); got != "said-one,said-two" {
		t.Errorf("statements with different quotes stay: %s", got)
	}
}

func TestWithoutNearDuplicatesCollapsesAWholeRestatementCluster(t *testing.T) {
	hits := []search.Hit{
		{Path: "projects/widget/a.md", Name: "a", Scope: "projects/widget", Description: "The widget queue retries transient errors 401, 429 and 529; strip the queued continuation and send it manually"},
		{Path: "projects/widget/b.md", Name: "b", Scope: "projects/widget", Description: "The widget queue retries transient errors 401, 429 and 529; strip any appended continuation and send it manually"},
		{Path: "projects/widget/c.md", Name: "c", Scope: "projects/widget", Description: "The widget queue retries transient errors 401, 429 and 529; strip the queued continuation and send it manually"},
	}
	if got := names(withoutNearDuplicates(hits)); got != "a" && got != "b" && got != "c" {
		t.Fatalf("got %s, want one representative", got)
	}
}
