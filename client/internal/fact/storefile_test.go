package fact

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/ledger"
)

// Every underscore file is a store file, never a fact: a folder's record
// and overview (which hold frontmatter a fact's strict decoder refuses) and
// any later one are passed over by Walk.
func TestWalkSkipsEveryStoreFile(t *testing.T) {
	root := t.TempDir()
	good := "---\nname: a\ndescription: d\ntype: machine_fact\nscope: projects/widget\nsource:\n  harness: claude\n  host: host-a\n  kind: observed\ncreated: 2026-10-07\n---\n\nbody\n"
	record := "---\nkind: repo\nname: widget\nstatus: active\n---\nwhy\n"
	files := map[string]string{
		"projects/widget/a.md":           good,
		"projects/widget/_record.md":     record,
		"projects/widget/_overview.md":   "---\nfolder: projects/widget\n---\ntext [[a]]\n",
		"projects/widget/_later-kind.md": "not even frontmatter",
		"projects/_index.md":             "# projects\n",
	}
	for p, b := range files {
		os.MkdirAll(filepath.Join(root, filepath.Dir(p)), 0o755)
		os.WriteFile(filepath.Join(root, p), []byte(b), 0o644)
	}
	facts, errs, err := Walk(root)
	if err != nil || len(errs) != 0 || len(facts) != 1 || facts[0].Path != "projects/widget/a.md" {
		t.Fatalf("facts=%v errs=%v err=%v", facts, errs, err)
	}
	for name, want := range map[string]bool{"_record.md": true, "_overview.md": true, "_index.md": true, "_x": true, "a.md": false, "record.md": false} {
		if IsStoreFile(name) != want {
			t.Errorf("IsStoreFile(%q) = %v", name, !want)
		}
	}
	if !IsIndexFile("_index.md") || IsIndexFile("_record.md") {
		t.Error("IsIndexFile changed meaning")
	}
}

// last_checked is a frontmatter date the rotation stamps through
// PatchRelations: one line added and taken away again, byte for byte, in LF
// and CRLF files, and a stamp that is not a date is refused.
func TestPatchLastCheckedRoundTrip(t *testing.T) {
	for _, eol := range []string{"\n", "\r\n"} {
		orig := strings.ReplaceAll("---\nname: a\ndescription: \"x\"\ntype: reference\nscope: system\nsource:\n  harness: curator\n  host: h\n  kind: observed\ncreated: 2026-10-01\nupdated: 2026-10-02\n---\n\nbody\n", "\n", eol)
		f, err := Parse("system/a.md", []byte(orig))
		if err != nil {
			t.Fatal(err)
		}
		f.Meta.LastChecked = "2026-10-21"
		patched, err := PatchRelations([]byte(orig), f)
		if err != nil {
			t.Fatal(err)
		}
		want := strings.ReplaceAll("updated: 2026-10-02\nlast_checked: 2026-10-21\n---\n", "\n", eol)
		if !strings.Contains(string(patched), want) {
			t.Fatalf("patched %q", patched)
		}
		got, err := Parse("system/a.md", patched)
		if err != nil || got.Meta.LastChecked != "2026-10-21" || got.Meta.Updated != "2026-10-02" {
			t.Fatalf("read back: %v %+v", err, got)
		}
		f.Meta.LastChecked = ""
		back, err := PatchRelations(patched, f)
		if err != nil || string(back) != orig {
			t.Fatalf("round trip: %v\n%q\nwant\n%q", err, back, orig)
		}
		f.Meta.LastChecked = "today"
		if _, err := PatchRelations([]byte(orig), f); err == nil {
			t.Fatal("a stamp that is no date was written")
		}
	}
}

// The format marker is read with the store's state, and writers of the new
// format write only into a store whose marker this binary knows.
func TestFormatMarker(t *testing.T) {
	root := t.TempDir()
	if ok, why := FormatWrites(root); ok || !strings.Contains(why, "no format marker") {
		t.Fatalf("no marker: %v %q", ok, why)
	}
	st, err := LoadState(root)
	if err != nil || st.Format.Version != 0 || st.FormatErr != nil {
		t.Fatalf("state %+v %v", st, err)
	}
	os.MkdirAll(filepath.Join(root, "review"), 0o755)
	write := func(b string) {
		os.WriteFile(filepath.Join(root, filepath.FromSlash(ledger.FormatPath)), []byte(b), 0o644)
	}
	write("{not json")
	if ok, why := FormatWrites(root); ok || !strings.Contains(why, "cannot be read") {
		t.Fatalf("broken marker: %v %q", ok, why)
	}
	if st, err := LoadState(root); err != nil || st.FormatErr == nil {
		t.Fatalf("a broken format marker must be reported, not fail every read: %+v %v", st, err)
	}
	write(`{"version": 99, "at": "x"}`)
	if ok, why := FormatWrites(root); ok || !strings.Contains(why, "newer") {
		t.Fatalf("newer marker: %v %q", ok, why)
	}
	write(string(ledger.Format{Version: ledger.FormatVersion, At: "2026-10-21T00:00:00Z"}.Encode()))
	if ok, why := FormatWrites(root); !ok {
		t.Fatalf("marker set: %q", why)
	}
	if st, err := LoadState(root); err != nil || st.Format.Version != ledger.FormatVersion {
		t.Fatalf("state %+v %v", st, err)
	}
}
