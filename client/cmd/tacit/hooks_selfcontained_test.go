package main

import (
	"context"
	"testing"
)

// A pure text operation on a literal the prompt supplies gets no facts, even
// when its words match stored facts; a request that only mentions "literal"
// or an operation still gets them.
func TestPromptHookSkipsSelfContainedTextOperations(t *testing.T) {
	ix := hookStore(t)
	ctx := context.Background()
	for _, p := range []string{
		"Return only the uppercase form of this literal: quokka scheduler zebra rule",
		"Count the characters in the literal string quokka scheduler zebra rule, nothing else",
	} {
		got, err := promptHits(ctx, ix, demo, p, map[string]bool{})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) == 0 {
			t.Fatalf("fixture should match the words of %q", p)
		}
		if promptWorthy(p) {
			t.Errorf("%q needs no memory, but the hook would deliver %s", p, names(got))
		}
	}
	for _, p := range []string{
		"quokka scheduler zebra rule: fix the literal parsing in the worker",
		"quokka scheduler zebra rule: make the job names uppercase",
	} {
		if !promptWorthy(p) {
			t.Errorf("%q is a normal request and must still get facts", p)
		}
	}
}
