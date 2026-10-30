package links

import (
	"strings"
	"testing"
)

func file(body string) []byte {
	return []byte("---\nname: pref\ndescription: d\ntype: user_standard\nscope: user\n---\n\n" + body)
}

// Restore puts back the links a run unwrapped whose target keep names, in
// the line the run wrote, and nothing else: not a link it unwrapped as dead,
// not one it rewrote, and not where the line was edited since.
func TestRestorePutsBackOnlyTheKeptLinks(t *testing.T) {
	keep := func(target string) bool { return target == "stmt-a" || target == "stmt-b" }
	before := file("Made of [[stmt-a]] and [[stmt-b|the second]]; see [[gone-x]].\nReplaced [[old]].\nAlso from [[stmt-a]].\n")
	after := file("Made of stmt-a and the second; see gone-x.\nReplaced [[new]].\nAlso from stmt-a.\n")
	cur := file("Made of stmt-a and the second; see gone-x.\nReplaced [[new]].\nAlso from stmt-a, edited since.\nA new line.\n")
	out, got := Restore(before, after, cur, keep)
	want := file("Made of [[stmt-a]] and [[stmt-b|the second]]; see gone-x.\nReplaced [[new]].\nAlso from stmt-a, edited since.\nA new line.\n")
	if string(out) != string(want) {
		t.Fatalf("restored\n%s\nwant\n%s", out, want)
	}
	if got["stmt-a"] != 1 || got["stmt-b"] != 1 || len(got) != 2 {
		t.Fatalf("restored %v", got)
	}
	if !IsRepair(out, cur) {
		t.Fatal("putting the links back is not the repair the other way round")
	}
	// Nothing to keep, no repair, or the line twice in cur: nothing changes.
	for name, c := range map[string]struct {
		before, after, cur []byte
		keep               func(string) bool
	}{
		"keep nothing":    {before, after, cur, func(string) bool { return false }},
		"not a repair":    {before, file("Made of something else.\nReplaced [[new]].\nAlso from stmt-a.\n"), cur, keep},
		"the line twice":  {before, after, file("Made of stmt-a and the second; see gone-x.\nMade of stmt-a and the second; see gone-x.\n"), keep},
		"no frontmatter":  {before, after, []byte("Made of stmt-a and the second; see gone-x.\n"), keep},
		"line edited now": {before, after, file("Made of stmt-a and the 2nd; see gone-x.\n"), keep},
	} {
		out, got := Restore(c.before, c.after, c.cur, c.keep)
		if len(got) != 0 || string(out) != string(c.cur) {
			t.Errorf("%s: restored %v\n%s", name, got, out)
		}
	}
	if strings.Contains(string(out), "[[gone-x]]") {
		t.Fatal("a dead link came back")
	}
}
