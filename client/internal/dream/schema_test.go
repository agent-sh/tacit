package dream

import "testing"

func TestParseCheckedStructuredFacts(t *testing.T) {
	body := "{\"facts\":[{\"description\":\"Use `cmd` without escapes.\",\"type\":\"machine_fact\",\"kind\":\"observed\",\"applies_to\":{}}]}"
	rows, err := ParseChecked(body)
	if err != nil || len(rows) != 1 || rows[0].Description != "Use `cmd` without escapes." {
		t.Fatalf("structured facts: %+v %v", rows, err)
	}
}

func TestStructuredFactsRejectMalformedWrappers(t *testing.T) {
	for _, body := range []string{"{\"facts\":null}", "{\"facts\":[],\"error\":\"failed\"}", "{\"facts\":[null]}"} {
		if _, err := ParseChecked(body); err == nil {
			t.Fatalf("accepted malformed wrapper: %s", body)
		}
	}
	if rows, err := ParseChecked("{\"facts\":[]}"); err != nil || len(rows) != 0 {
		t.Fatalf("empty result: %+v %v", rows, err)
	}
}
