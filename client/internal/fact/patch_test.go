package fact

import (
	"strings"
	"testing"
)

// A relation patch touches only its own lines and undoes to the same bytes,
// in LF and CRLF files alike.
func TestPatchRelationsRoundTrip(t *testing.T) {
	for _, eol := range []string{"\n", "\r\n"} {
		orig := strings.ReplaceAll("---\nname: a\ndescription: \"x\"\ntype: reference\nscope: system\napplies_to:\n  paths:\n    - p\nsource:\n  harness: curator\n  host: h\n  kind: observed\ncreated: 2026-10-01\n---\n\nbody\n", "\n", eol)
		f, err := Parse("system/a.md", []byte(orig))
		if err != nil {
			t.Fatal(err)
		}
		f.Meta.SupersededBy, f.Meta.ConflictsWith, f.Meta.Updated = "b", []string{"c", "d"}, "2026-10-08"
		patched, err := PatchRelations([]byte(orig), f)
		if err != nil {
			t.Fatal(err)
		}
		want := strings.ReplaceAll("created: 2026-10-01\nupdated: 2026-10-08\nsuperseded_by: b\nconflicts_with: [\"c\", \"d\"]\n---\n", "\n", eol)
		if !strings.Contains(string(patched), want) || strings.Count(string(patched), "\r\n") != strings.Count(string(patched), "\n")*boolInt(eol == "\r\n") {
			t.Fatalf("patched %q", patched)
		}
		f.Meta.SupersededBy, f.Meta.ConflictsWith, f.Meta.Updated = "", nil, ""
		back, err := PatchRelations(patched, f)
		if err != nil {
			t.Fatal(err)
		}
		if string(back) != orig {
			t.Fatalf("round trip with %q:\n%q\nwant\n%q", eol, back, orig)
		}
	}
}

// A retirement adds two lines (retired, updated) and undoes to the same bytes.
func TestPatchRetiredRoundTrip(t *testing.T) {
	orig := "---\nname: a\ndescription: \"x\"\ntype: reference\nscope: system\nsource:\n  harness: curator\n  host: h\n  kind: observed\ncreated: 2026-10-01\n---\n\nbody\n"
	f, err := Parse("system/a.md", []byte(orig))
	if err != nil {
		t.Fatal(err)
	}
	f.Meta.Retired, f.Meta.Updated = "2026-10-09", "2026-10-09"
	patched, err := PatchRelations([]byte(orig), f)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(patched), "created: 2026-10-01\nupdated: 2026-10-09\nretired: 2026-10-09\n---\n") {
		t.Fatalf("patched %q", patched)
	}
	if got, err := Parse("system/a.md", patched); err != nil || got.Meta.Retired != "2026-10-09" {
		t.Fatalf("read back: %v %+v", err, got)
	}
	f.Meta.Retired, f.Meta.Updated = "", ""
	back, err := PatchRelations(patched, f)
	if err != nil || string(back) != orig {
		t.Fatalf("round trip: %v\n%q\nwant\n%q", err, back, orig)
	}
	f.Meta.Retired = "yesterday"
	if _, err := PatchRelations([]byte(orig), f); err == nil {
		t.Fatal("a retired that is not a date was written")
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
