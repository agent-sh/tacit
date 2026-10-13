package links

import (
	"testing"
)

func TestIsRepair(t *testing.T) {
	head := "---\nname: f\n---\n\nA [[x]] b [[y|l]] c [[x]] d\n"
	for name, tc := range map[string]struct {
		cur  string
		want bool
	}{
		"same":                  {head, true},
		"one unwrapped":         {"---\nname: f\n---\n\nA x b [[y|l]] c [[x]] d\n", true},
		"all unwrapped":         {"---\nname: f\n---\n\nA x b l c x d\n", true},
		"one rewritten":         {"---\nname: f\n---\n\nA [[z]] b [[y|l]] c [[x]] d\n", true},
		"label kept":            {"---\nname: f\n---\n\nA [[x]] b [[q|l]] c [[x]] d\n", true},
		"a label added":         {"---\nname: f\n---\n\nA [[z|the engine is broken]] b [[y|l]] c [[x]] d\n", false},
		"a label changed":       {"---\nname: f\n---\n\nA [[x]] b [[q|the engine is broken]] c [[x]] d\n", false},
		"a label dropped":       {"---\nname: f\n---\n\nA [[x]] b [[q]] c [[x]] d\n", false},
		"a sentence for a link": {"---\nname: f\n---\n\nA x. CORRECTION: the opposite b [[y|l]] c [[x]] d\n", false},
		"a link deleted":        {"---\nname: f\n---\n\nA  b [[y|l]] c [[x]] d\n", false},
		"words changed":         {"---\nname: f\n---\n\nA x b l c x D\n", false},
		"frontmatter changed":   {"---\nname: g\n---\n\nA x b l c x d\n", false},
		"text added":            {"---\nname: f\n---\n\nA x b l c x d\nmore\n", false},
		"text dropped":          {"---\nname: f\n---\n\nA x b l c x\n", false},
		"order swapped":         {"---\nname: f\n---\n\nA x c l b x d\n", false},
	} {
		if got := IsRepair([]byte(head), []byte(tc.cur)); got != tc.want {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
	}
	// A section is dropped on a rewrite and never added; the label is the old one.
	sec := "---\nname: f\n---\n\nsee [[old#setup|the guide]] and [[old#part]]\n"
	for name, tc := range map[string]struct {
		cur  string
		want bool
	}{
		"section dropped, label kept": {"---\nname: f\n---\n\nsee [[new|the guide]] and [[new]]\n", true},
		"section kept":                {"---\nname: f\n---\n\nsee [[new#setup|the guide]] and [[new]]\n", false},
		"section added":               {"---\nname: f\n---\n\nsee [[new|the guide]] and [[new#other]]\n", false},
		"label relabelled":            {"---\nname: f\n---\n\nsee [[new|a different guide]] and [[new]]\n", false},
		"unwrapped":                   {"---\nname: f\n---\n\nsee the guide and old\n", true},
	} {
		if got := IsRepair([]byte(sec), []byte(tc.cur)); got != tc.want {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
	}
	plain := "---\nname: f\n---\n\nno links\n"
	if !IsRepair([]byte(plain), []byte(plain)) || IsRepair([]byte(plain), []byte("---\nname: f\n---\n\nno links!\n")) {
		t.Error("a file without links")
	}
}

// What Rewrite writes is always a repair, whatever the shape of the link.
func TestWhatRewriteWritesIsARepair(t *testing.T) {
	body := "a [[old#setup|the guide]] b [[gone]] c [[old|x]] d [[old#sec]] e `[[code]]`"
	old := []byte("---\nname: f\n---\n\n" + body + "\n")
	for _, to := range []string{"", "new"} {
		edits := map[string]Edit{"old": {Target: "old", To: to}, "gone": {Target: "gone"}}
		out, ok := Rewrite(old, edits)
		if !ok || !IsRepair(old, out) {
			t.Errorf("to=%q: not a repair:\n%s", to, out)
		}
	}
}
