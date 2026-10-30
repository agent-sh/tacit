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

// A line the run wrote twice is not put back (which one it was is not
// known), and a line that moved into code gets no link: the result must be
// cur with links put back and nothing else.
func TestRestoreRefusesWhatItCannotPlace(t *testing.T) {
	keep := func(target string) bool { return target == "stmt-a" }
	before := file("From [[stmt-a]].\nFrom [[stmt-a]].\n")
	after := file("From stmt-a.\nFrom stmt-a.\n")
	cur := file("From stmt-a.\nAnd more.\n")
	if out, got := Restore(before, after, cur, keep); len(got) != 0 || string(out) != string(cur) {
		t.Errorf("a line the run wrote twice: %v\n%s", got, out)
	}
	before = file("Made of [[stmt-a]].\n")
	after = file("Made of stmt-a.\n")
	cur = file("Example:\n\n```\nMade of stmt-a.\n```\n")
	if out, got := Restore(before, after, cur, keep); len(got) != 0 || string(out) != string(cur) {
		t.Errorf("a line that moved into code: %v\n%s", got, out)
	}
	// A line in code gets no link, and the line beside it still gets its own.
	keep = func(target string) bool { return target == "stmt-a" || target == "stmt-b" }
	before = file("From [[stmt-a]].\nAlso from [[stmt-b]].\n")
	after = file("From stmt-a.\nAlso from stmt-b.\n")
	cur = file("```\nFrom stmt-a.\n```\nAlso from stmt-b.\n")
	out, got := Restore(before, after, cur, keep)
	if want := file("```\nFrom stmt-a.\n```\nAlso from [[stmt-b]].\n"); string(out) != string(want) || got["stmt-b"] != 1 || got["stmt-a"] != 0 {
		t.Errorf("one line in code: %v\n%s", got, out)
	}
}
