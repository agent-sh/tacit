package fact

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// In an audited store a ruling is kind user with the evidence that the words
// are theirs; the type does not make one. Before the audit the store keeps the
// old definition (kind user, or type user_standard).
func TestIsRuling(t *testing.T) {
	for _, c := range []struct {
		src             Source
		typ             string
		audited, before bool
	}{
		{Source{Kind: "user", OwnerQuote: "q q q q", OwnerQuoteFrom: "claude:s"}, "design_decision", true, true},
		{Source{Kind: "user", Endorsed: "0123456789ab"}, "design_decision", true, true},
		{Source{Kind: "user", Harness: "human"}, "user_standard", true, true},
		{Source{Kind: "user"}, "user_standard", false, true},
		{Source{Kind: "observed"}, "user_standard", false, true},
		{Source{Kind: "observed"}, "design_decision", false, false},
		{Source{Kind: "user", OwnerQuote: "q q q q"}, "user_standard", false, true},
	} {
		f := &Fact{Path: "user/x.md", Meta: Meta{Type: c.typ, Source: c.src}, Audited: true, QuoteBacked: true, EndorsedBacked: true}
		if got := IsRuling(f); got != c.audited {
			t.Errorf("audited %+v %s: %v, want %v", c.src, c.typ, got, c.audited)
		}
		f.Audited = false
		if got := IsRuling(f); got != c.before {
			t.Errorf("before the audit %+v %s: %v, want %v", c.src, c.typ, got, c.before)
		}
	}
	// A claim the store does not back is no evidence.
	forged := &Fact{Path: "user/x.md", Meta: Meta{Type: "user_standard", Source: Source{Kind: "user", OwnerQuote: "q q q q", OwnerQuoteFrom: "claude:s", Endorsed: "0123456789ab"}}, Audited: true}
	if IsRuling(forged) || !ClaimsEvidence(forged) {
		t.Fatal("unbacked evidence made a ruling")
	}
}

// Walk reads the audit marker: every fact of the store is audited. A marker
// an older audit wrote with a list of facts put to the owner protects none of
// them: nothing is pending.
func TestWalkReadsTheAuditMarker(t *testing.T) {
	root := t.TempDir()
	write := func(rel, s string) {
		p := root + "/" + rel
		os.MkdirAll(p[:strings.LastIndex(p, "/")], 0o755)
		os.WriteFile(p, []byte(s), 0o644)
	}
	fm := func(name string) string {
		return "---\nname: " + name + "\ndescription: d\ntype: user_standard\nscope: user\nsource:\n  harness: curator\n  host: h\n  kind: user\ncreated: 2026-10-01\n---\n\nb\n"
	}
	write("user/a.md", fm("a"))
	write("user/b.md", fm("b"))
	fs, _, _ := Walk(root)
	if fs[0].Audited || !IsRuling(fs[0]) {
		t.Fatal("no marker: the old definition")
	}
	write("review/rulings-audit.json", `{"version":1,"audited_head":"x","at":"t","pending":[{"name":"b","reason":"no-words"}]}`)
	fs, _, _ = Walk(root)
	if !fs[0].Audited || IsRuling(fs[0]) || !fs[1].Audited || IsRuling(fs[1]) {
		t.Fatalf("with the marker: %+v %+v", fs[0], fs[1])
	}
	// A fact read on its own is judged the same (WithStore).
	if f := (&Fact{Path: "user/b.md", Meta: fs[1].Meta}); WithStore(root, f) != nil || !f.Audited || IsRuling(f) {
		t.Fatalf("WithStore: %+v", f)
	}
	// A marker that exists but cannot be read is an error, never the old definition.
	write("review/rulings-audit.json", `{"version":1,"audited_head":"x"`)
	if _, _, err := Walk(root); err == nil || !errors.Is(err, ErrMarker) {
		t.Fatalf("a broken marker: %v", err)
	}
	if err := WithStore(root, &Fact{Path: "user/a.md", Meta: fs[0].Meta}); !errors.Is(err, ErrMarker) {
		t.Fatalf("WithStore on a broken marker: %v", err)
	}
}

// A note is theirs only in their own inbox directory and only when the app added
// it (AppNote, from the adding commit).
func TestOwnerWroteNeedsHisDirectoryAndTheApp(t *testing.T) {
	src := Source{Harness: "human", Kind: "user"}
	for _, c := range []struct {
		path string
		app  bool
		want bool
	}{{"inbox/human@host-a/n.md", true, true}, {"inbox/human@host-a/n.md", false, false}, {"inbox/claude@host-a/n.md", true, false}, {"user/rule.md", false, true}} {
		if got := OwnerWrote(&Fact{Path: c.path, Meta: Meta{Source: src}, AppNote: c.app}); got != c.want {
			t.Errorf("%s app %v: %v", c.path, c.app, got)
		}
	}
}

// The audit changes only the source block; every other byte stays, and the
// old source written back gives the same file.
func TestPatchSourceRoundTrip(t *testing.T) {
	orig := "---\nname: a\ndescription: \"x\"\ntype: user_standard\nscope: system\nsource:\n  harness: curator\n  host: h\n  kind: user\ncreated: 2026-10-01\nupdated: 2026-10-02\n---\n\nOwner: \"never rename public endpoints: ask\".\n"
	f, err := Parse("system/a.md", []byte(orig))
	if err != nil {
		t.Fatal(err)
	}
	old := f.Meta.Source
	f.Meta.Source.OwnerQuote, f.Meta.Source.OwnerQuoteFrom = "never rename public endpoints: ask", "claude:s1"
	b, err := PatchSource([]byte(orig), f)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "  kind: user\n  owner_quote: 'never rename public endpoints: ask'\n  owner_quote_from: claude:s1\ncreated: 2026-10-01\nupdated: 2026-10-02\n") {
		t.Fatalf("patched:\n%s", b)
	}
	f.Meta.Source = old
	f.Meta.Source.Kind = "observed"
	b2, _ := PatchSource(b, f)
	if string(b2) != strings.Replace(orig, "kind: user", "kind: observed", 1) {
		t.Fatalf("demoted:\n%s", b2)
	}
	f.Meta.Source = old
	if b3, _ := PatchSource(b2, f); string(b3) != orig {
		t.Fatalf("round trip:\n%s", b3)
	}
}

// Words of theirs on a fact that is not their rule count only when the quote
// ledger records them as words for that fact, and only while no Not mine of
// their stands. A words entry never backs an owner_quote: an agent cannot turn a
// short phrase of theirs into a ruling by moving it to owner_quote.
func TestOwnerWordsAreBackedByTheLedgerAsWords(t *testing.T) {
	root := t.TempDir()
	write := func(rel, s string) {
		p := root + "/" + rel
		os.MkdirAll(p[:strings.LastIndex(p, "/")], 0o755)
		os.WriteFile(p, []byte(s), 0o644)
	}
	write("review/rulings-audit.json", `{"version":1,"audited_head":"x","at":"t"}`)
	write("projects/x/w.md", "---\nname: w\ndescription: d\ntype: user_standard\nscope: projects/x\nsource:\n  harness: curator\n  host: h\n  kind: observed\n  owner_words: keep the hooks quiet\n  owner_words_from: claude:s\ncreated: 2026-10-01\n---\n\nHe said \"keep the hooks quiet\".\n")
	write("projects/x/q.md", "---\nname: q\ndescription: d\ntype: user_standard\nscope: projects/x\nsource:\n  harness: curator\n  host: h\n  kind: user\n  owner_quote: keep the hooks quiet\n  owner_quote_from: claude:s\ncreated: 2026-10-01\n---\n\nHe said \"keep the hooks quiet\".\n")
	byName := func() map[string]*Fact {
		fs, _, err := Walk(root)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]*Fact{}
		for _, f := range fs {
			m[f.Meta.Name] = f
		}
		return m
	}
	if f := byName()["w"]; OwnerWordsOf(f) != "" || !ClaimsEvidence(f) {
		t.Fatal("words with no ledger entry counted")
	}
	write("review/owner-quotes.jsonl", `{"name":"w","quote":"keep the hooks quiet","from":"claude:s","turn_hash":"h","at":"2026-10-11","words":true}`+"\n"+
		`{"name":"q","quote":"keep the hooks quiet","from":"claude:s","turn_hash":"h","at":"2026-10-11","words":true}`+"\n")
	m := byName()
	if OwnerWordsOf(m["w"]) != "keep the hooks quiet" || IsRuling(m["w"]) {
		t.Fatalf("backed words: %+v", m["w"])
	}
	if IsRuling(m["q"]) || m["q"].QuoteBacked {
		t.Fatal("a words entry backed an owner_quote")
	}
	f := &Fact{Path: "projects/x/w.md", Meta: m["w"].Meta}
	if WithStore(root, f) != nil || OwnerWordsOf(f) == "" {
		t.Fatalf("WithStore: %+v", f)
	}
	write("review/decisions.jsonl", `{"id":"aaaaaaaaaaaa","action":"not-mine","a":"w","at":"2026-10-11T10:00:00Z"}`+"\n")
	if f := byName()["w"]; OwnerWordsOf(f) != "" || !f.NotMine {
		t.Fatal("their Not mine left the words standing")
	}
}
