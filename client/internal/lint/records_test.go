package lint

import (
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/record"
)

func recordFile(t *testing.T, r *record.Record) string {
	t.Helper()
	b, err := r.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func rec(kind, folder string) *record.Record {
	name := folder[strings.LastIndex(folder, "/")+1:]
	return &record.Record{Kind: kind, Name: name, Description: "what " + name + " is", Status: record.StatusActive,
		Was:     []record.Entry{{Status: record.StatusActive, Date: "2026-10-21", By: record.ByOpus, Why: "claude:s1 claude:s2"}},
		Created: "2026-10-21", CreatedBy: record.ByOpus, Folder: folder}
}

func l17(fs []Finding) []string {
	var out []string
	for _, f := range fs {
		if f.Rule == "L17" {
			out = append(out, f.Path+": "+f.Msg)
		}
	}
	return out
}

func setFormat(t *testing.T, root string, complete bool) {
	write(t, root, ledger.FormatPath, string(ledger.Format{Version: ledger.FormatVersion, At: "x", RecordsComplete: complete}.Encode()))
}

// A good store of the format: records that hold, facts in an area and a
// topic with their records, a suppressed project.
func TestRecordsCleanStorePasses(t *testing.T) {
	root := t.TempDir()
	setFormat(t, root, true)
	write(t, root, "projects/widget/_record.md", recordFile(t, rec("repo", "projects/widget")))
	old := rec("repo", "projects/widget-old")
	old.SuppressedBy = "projects/widget"
	write(t, root, "projects/widget-old/_record.md", recordFile(t, old))
	// An area that holds a harness's tool pattern from before the MCP-only
	// rule: lint passes it (a write would refuse it as new).
	ops := rec("area", "areas/ops")
	ops.Tools = []string{"Bash"}
	write(t, root, "areas/ops/_record.md", recordFile(t, ops))
	write(t, root, "areas/ops/topics/q4/_record.md", recordFile(t, rec("topic", "areas/ops/topics/q4")))
	write(t, root, "projects/widget/a.md", good("a", "projects/widget"))
	write(t, root, "areas/ops/b.md", good("b", "areas/ops"))
	write(t, root, "areas/ops/topics/q4/c.md", good("c", "areas/ops/topics/q4"))
	fs, err := Run(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 0 {
		t.Fatalf("expected clean, got %v", fs)
	}
}

func TestRecordsBrokenFixturesFail(t *testing.T) {
	for name, c := range map[string]struct {
		setup func(t *testing.T, root string)
		want  string
	}{
		"no format marker": {func(t *testing.T, root string) {
			write(t, root, "projects/widget/_record.md", recordFile(t, rec("repo", "projects/widget")))
		}, "projects/widget/_record.md: a folder record in a store without the format marker"},
		"area fact without marker": {func(t *testing.T, root string) {
			write(t, root, "areas/ops/b.md", good("b", "areas/ops"))
		}, "areas/ops/b.md: a fact in areas/ops in a store without the format marker"},
		"last_checked without marker": {func(t *testing.T, root string) {
			write(t, root, "user/a.md", strings.Replace(good("a", "user"), "created:", "last_checked: 2026-10-21\ncreated:", 1))
		}, "user/a.md: last_checked in a store without the format marker"},
		"broken marker": {func(t *testing.T, root string) {
			write(t, root, ledger.FormatPath, "{")
		}, "review/format.json: the format marker cannot be read"},
		"unparsable record": {func(t *testing.T, root string) {
			setFormat(t, root, false)
			write(t, root, "projects/widget/_record.md", "---\nkind: repo\nsurprise: 1\n---\n")
		}, "projects/widget/_record.md: frontmatter"},
		"wrong kind": {func(t *testing.T, root string) {
			setFormat(t, root, false)
			write(t, root, "areas/ops/_record.md", recordFile(t, rec("repo", "areas/ops")))
		}, "areas/ops/_record.md: kind: does not match the folder"},
		"chain": {func(t *testing.T, root string) {
			setFormat(t, root, false)
			a, b, c := rec("repo", "projects/a"), rec("repo", "projects/b"), rec("repo", "projects/c")
			b.SuppressedBy, c.SuppressedBy = "projects/a", "projects/b"
			for _, r := range []*record.Record{a, b, c} {
				write(t, root, record.Rel(r.Folder), recordFile(t, r))
			}
		}, "projects/c/_record.md: suppressed_by: points at a folder that is itself suppressed"},
		"area on one session": {func(t *testing.T, root string) {
			setFormat(t, root, false)
			r := rec("area", "areas/ops")
			r.Was[0].Why = "claude:s1"
			write(t, root, "areas/ops/_record.md", recordFile(t, r))
		}, "at least 2 distinct sessions"},
		"unbacked owner words": {func(t *testing.T, root string) {
			setFormat(t, root, false)
			r := rec("repo", "projects/widget")
			r.Was[0].By, r.CreatedBy, r.Was[0].Quote, r.Was[0].QuoteFrom = "owner", "owner", "widget is the real name of it", "claude:s1"
			write(t, root, "projects/widget/_record.md", recordFile(t, r))
		}, "the quote ledger does not record these words"},
		"area fact with no record": {func(t *testing.T, root string) {
			setFormat(t, root, false)
			write(t, root, "areas/ops/b.md", good("b", "areas/ops"))
		}, "areas/ops/b.md: a fact in areas/ops, which has no record"},
		"records complete, a folder without one": {func(t *testing.T, root string) {
			setFormat(t, root, true)
			write(t, root, "projects/widget/a.md", good("a", "projects/widget"))
		}, "projects/widget: a folder with facts and no record"},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			c.setup(t, root)
			fs, err := Run(root)
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Join(l17(fs), "\n")
			if !strings.Contains(got, c.want) {
				t.Fatalf("want %q in\n%s", c.want, got)
			}
		})
	}
	// Records complete off: the same folder is fine (the interim rule).
	root := t.TempDir()
	setFormat(t, root, false)
	write(t, root, "projects/widget/a.md", good("a", "projects/widget"))
	if fs, _ := Run(root); len(l17(fs)) != 0 {
		t.Fatalf("interim: %v", l17(fs))
	}
}

// L19: an overview parses, names its folder and cites facts the store has
// in every paragraph and bullet; a cited fact that was retired is no
// finding (the overview step renews it).
func TestOverviewRule(t *testing.T) {
	root := t.TempDir()
	write(t, root, "machines/host-a/a.md", good("a", "machines/host-a"))
	write(t, root, "machines/host-a/r.md", strings.Replace(good("r", "machines/host-a"), "created:", "retired: 2026-10-09\ncreated:", 1))
	ov := func(folder, body string) string {
		return "---\nfolder: " + folder + "\nwritten: 2026-10-21\nfacts: 1\nbasis: x\nrulings: y\nby: opus\n---\n\n" + body + "\n"
	}
	write(t, root, "projects/ok/_overview.md", ov("projects/ok", "It holds a [[a]] and a retired one [[r]]."))
	write(t, root, "projects/bad/_overview.md", ov("projects/other", "No citation here.\n\n- one to a ghost [[ghost]]"))
	write(t, root, "projects/broken/_overview.md", "no frontmatter\n")
	fs, err := Run(root)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range fs {
		if f.Rule == "L19" {
			got = append(got, f.Path+": "+f.Msg)
		}
	}
	all := strings.Join(got, "\n")
	for _, want := range []string{"projects/bad/_overview.md: folder is", "projects/bad/_overview.md: a paragraph or bullet cites no fact", "projects/bad/_overview.md: [[ghost]] names no fact", "projects/broken/_overview.md: no frontmatter"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in\n%s", want, all)
		}
	}
	if strings.Contains(all, "projects/ok/") {
		t.Errorf("a good overview flagged:\n%s", all)
	}
}
