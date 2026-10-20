package fact

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const sample = `---
name: api-prepush-node-path
description: api pre-push runs eslint; the hook's PATH lacks ~/.local/node/bin
type: past_mistake
scope: projects/api
applies_to:
  paths:
    - tools/hooks/pre-push
  repos:
    - api
  commands:
    - git push
source:
  harness: claude
  host: host-a
  kind: observed
created: 2026-10-07
---

Prefix with PATH=$HOME/.local/node/bin:$PATH. See [[host-a-shell-path]] and [[host-a-shell-path|alias]].
`

func TestParseAndRoundTrip(t *testing.T) {
	f, err := Parse("projects/api/api-prepush-node-path.md", []byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if f.Meta.Name != "api-prepush-node-path" || f.Meta.Type != "past_mistake" || f.Meta.Scope != "projects/api" {
		t.Fatalf("meta: %+v", f.Meta)
	}
	if got := f.Links(); len(got) != 1 || got[0] != "host-a-shell-path" {
		t.Fatalf("links: %v", got)
	}
	if f.Stem() != "api-prepush-node-path" || f.Dir() != "projects/api" || f.InInbox() {
		t.Fatalf("path helpers: stem=%q dir=%q", f.Stem(), f.Dir())
	}
	out, err := f.Encode()
	if err != nil {
		t.Fatal(err)
	}
	g, err := Parse(f.Path, out)
	if err != nil {
		t.Fatalf("re-parse: %v\n%s", err, out)
	}
	if !reflect.DeepEqual(g.Meta, f.Meta) || strings.TrimSpace(g.Body) != strings.TrimSpace(f.Body) {
		t.Fatalf("round trip changed content:\n%s", out)
	}
}

func TestStrictUnknownKey(t *testing.T) {
	bad := strings.Replace(sample, "created: 2026-10-07", "created: 2026-10-07\nmetadata:\n  type: project", 1)
	if _, err := Parse("x.md", []byte(bad)); err == nil || !strings.Contains(err.Error(), "metadata") {
		t.Fatalf("expected unknown-field error, got %v", err)
	}
}

func TestSplit(t *testing.T) {
	cases := map[string]bool{
		"---\na: 1\n---\nbody\n":     true,
		"---\r\na: 1\r\n---\r\nbody": true,
		"no frontmatter\n":           false,
		"---\na: 1\n":                false, // unterminated
		"---\n---\n":                 true,  // empty frontmatter
	}
	for in, want := range cases {
		_, _, ok := Split([]byte(in))
		if ok != want {
			t.Errorf("Split(%q) ok=%v want %v", in, ok, want)
		}
	}
	fm, body, _ := Split([]byte("---\na: 1\n---\nbody\n"))
	if string(fm) != "a: 1\n" || string(body) != "body\n" {
		t.Fatalf("fm=%q body=%q", fm, body)
	}
}

func TestShapes(t *testing.T) {
	for s, want := range map[string]bool{"system": true, "user": true, "projects/api": true, "machines/host-a": true, "derived/x": true, "projects": false, "projects/a/b": false, "system/x": false, "inbox/x": false} {
		if ValidScope(s) != want {
			t.Errorf("ValidScope(%q)=%v", s, !want)
		}
	}
	if !IsSlug("a-b1") || IsSlug("A") || IsSlug("-a") || IsSlug("a_b") {
		t.Error("IsSlug")
	}
	if !IsDate("2026-10-07") || IsDate("2026-10-0x") || IsDate("2026-10-07T10:00") {
		t.Error("IsDate")
	}
}

func TestWalkSkips(t *testing.T) {
	root := t.TempDir()
	good := "---\nname: a\ndescription: d\ntype: machine_fact\nscope: machines/host-a\nsource:\n  harness: claude\n  host: host-a\n  kind: observed\ncreated: 2026-10-07\n---\n\nbody\n"
	for _, p := range []string{"machines/host-a/a.md", "keys/k.md", ".git/x.md", "MEMORY.md", "README.md", "notes.txt"} {
		os.MkdirAll(filepath.Join(root, filepath.Dir(p)), 0o755)
		os.WriteFile(filepath.Join(root, p), []byte(good), 0o644)
	}
	os.Symlink("/etc/hostname", filepath.Join(root, "machines/host-a/link.md"))
	facts, errs, err := Walk(root)
	if err != nil || len(errs) != 0 || len(facts) != 1 || facts[0].Path != "machines/host-a/a.md" {
		t.Fatalf("facts=%v errs=%v err=%v", facts, errs, err)
	}
}

func TestLinksShape(t *testing.T) {
	f := &Fact{Body: "[[a_b]] [[c-d|x]] [[e#s]] [[ $n == 0 ]] [[ ]] [[x\ny]]"}
	if got := strings.Join(f.Links(), ","); got != "a_b,c-d,e" {
		t.Fatalf("links %q", got)
	}
}

// A machine's overlay file is never one the owner wrote, whatever its
// harness and directory: anything on the machine can write the overlay.
func TestOverlayFilesAreNeverOwnerWritten(t *testing.T) {
	for _, p := range []string{"local/curated/user/x.md", "local/inbox/human@host-b/x.md", "local/human@host-b/x.md"} {
		f := &Fact{Path: p, AppNote: true, Meta: Meta{Source: Source{Harness: "human", Kind: "user"}}}
		f.Audited = true
		if OwnerWrote(f) || IsRuling(f) {
			t.Errorf("%s counts as the owner's", p)
		}
	}
	if !OwnerWrote(&Fact{Path: "user/x.md", Meta: Meta{Source: Source{Harness: "human"}}}) {
		t.Fatal("the app's curated rule is no longer the owner's")
	}
}
