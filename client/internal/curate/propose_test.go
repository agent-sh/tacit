package curate

import (
	"strings"
	"testing"
)

func TestProposalsAreCheckedAndTheSectionListsFolders(t *testing.T) {
	base := `{"origin":"n","name":"x","description":"d","type":"design_decision","scope":"user","kind":"observed","applies_to":{},"body":"b"%s}`
	for prop, want := range map[string]string{
		`,"propose":{"kind":"area","name":"ops","description":"on-call"}`:               "",
		`,"propose":{"kind":"topic","name":"q4","area":"ops","description":"the move"}`: "",
		`,"propose":{"kind":"team","name":"ops","description":"x"}`:                     "propose.kind",
		`,"propose":{"kind":"area","name":"Ops Team","description":"x"}`:                "propose.name",
		`,"propose":{"kind":"area","name":"ops","area":"x","description":"x"}`:          "propose.area",
		`,"propose":{"kind":"area","name":"ops","description":" "}`:                     "propose.description",
		`,"scope":"areas/ops/topics/q4"`:                                                "",
	} {
		text := "[" + strings.Replace(base, "%s", prop, 1) + "]"
		if strings.Contains(prop, `"scope"`) {
			text = "[" + strings.Replace(strings.Replace(base, `"scope":"user",`, "", 1), "%s", prop, 1) + "]"
		}
		es, err := ParseChecked(text)
		got := ""
		if err != nil {
			got = err.Error()
			for _, p := range Problems(err) {
				got += " " + p.Field
			}
		}
		if (want == "") != (err == nil) || want != "" && !strings.Contains(got, want) {
			t.Errorf("%s: got %q (%v), want %q", prop, got, es, want)
		}
	}
	s := FoldersSection([]Folder{{Scope: "areas/ops", Kind: "area", Description: "on-call and tickets", Tools: []string{"mcp__*__Ticket*"}}})
	if !strings.Contains(s, "- areas/ops (area): on-call and tickets; tools: mcp__*__Ticket*") || !strings.Contains(s, `"propose"`) {
		t.Fatalf("section:\n%s", s)
	}
	if !strings.Contains(FoldersSection(nil), "(none yet)") {
		t.Fatal("empty section")
	}
}
