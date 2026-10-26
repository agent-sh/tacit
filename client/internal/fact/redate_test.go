package fact

import (
	"bytes"
	"strings"
	"testing"
)

// A statement's day is corrected in its first line and its created date
// only: the words, the context, a check's flag line and every other byte stay;
// updated becomes today. It refuses a day after today, a fact that is no
// statement and a first line code did not write.
func TestRedateStatement(t *testing.T) {
	f := &Fact{Path: "user/s.md", Meta: Meta{Name: "s", Description: "the notes", Type: StatementType, Scope: "user",
		Source:  Source{Harness: "curator", Host: "h", Kind: "observed", OwnerWords: "keep the release notes short", OwnerWordsFrom: "claude:s1"},
		Created: "2026-10-25", Origin: "n1"}, Body: StatementBody("2026-10-25", "keep the release notes short", "the notes", "the owner's reading")}
	b, err := f.Encode()
	if err != nil {
		t.Fatal(err)
	}
	flag := CheckFlagLine("2026-10-25", "a check says something")
	b, err = AppendCheckFlag(b, flag)
	if err != nil {
		t.Fatal(err)
	}
	out, err := RedateStatement(b, "2026-09-03", "2026-10-26")
	if err != nil {
		t.Fatal(err)
	}
	g, err := Parse("user/s.md", out)
	if err != nil {
		t.Fatal(err)
	}
	day, words, ok := StatementLine(g.Body)
	if !ok || day != "2026-09-03" || words != "keep the release notes short" || g.Meta.Created != "2026-09-03" || g.Meta.Updated != "2026-10-26" || g.Meta.Source != f.Meta.Source ||
		g.Meta.Origin != "n1" || StatementContext(g.Body) != "the notes" || len(CheckFlags(g.Body)) != 1 || CheckFlags(g.Body)[0] != flag {
		t.Fatalf("redated:\n%s", out)
	}
	// Only the day changed, in two places, and updated was added.
	want := strings.Replace(strings.Replace(string(b), `created: "2026-10-25"`, `created: "2026-09-03"`, 1), "You said once (2026-10-25)", "You said once (2026-09-03)", 1)
	if got := strings.Replace(string(out), "updated: 2026-10-26\n", "", 1); got != want {
		t.Fatalf("other bytes changed:\n%s\nwant\n%s", got, want)
	}
	// Idempotent: the same day again changes nothing but updated.
	again, err := RedateStatement(out, "2026-09-03", "2026-10-26")
	if err != nil || !bytes.Equal(again, out) {
		t.Fatalf("again: %v\n%s", err, again)
	}
	if _, err := RedateStatement(b, "2026-10-27", "2026-10-26"); err == nil {
		t.Fatal("a day after today")
	}
	if _, err := RedateStatement(b, "2026-9-3", "2026-10-26"); err == nil {
		t.Fatal("not a date")
	}
	other := &Fact{Path: "user/o.md", Meta: Meta{Name: "o", Description: "d", Type: "user_standard", Scope: "user", Source: Source{Kind: "observed"}, Created: "2026-10-25"}, Body: "You said once (2026-10-25): \"x y z\"\n"}
	ob, _ := other.Encode()
	if _, err := RedateStatement(ob, "2026-09-03", "2026-10-26"); err == nil {
		t.Fatal("not a statement")
	}
}
