// Package tree generates the store's per-folder index files (SPEC.md,
// "Tree index") and reads them back for the tree tool.
//
// Every curated folder of the store (the root, the scope roots system/, user/,
// projects/, machines/, derived/, and each scope folder under them) holds one
// generated file, _index.md: the folder's live facts, one line each (name,
// type, description), and its subfolders, one line each with a count and a
// summary built by code from what is below them (no model). A writer of
// curated facts regenerates them in the same locked write and commit that
// changes the facts, so they never drift; lint L16 checks it. inbox/, review/
// and keys/ hold no index: the inbox is the writers' (unreviewed notes the
// curator has not promoted), review/ and keys/ hold no facts.
//
// The name starts with an underscore, which no fact name can (a fact name is
// a slug), so an index file is never a fact: fact.Walk, the search index,
// lint's fact rules and delivery all pass it over.
//
// Each file is bounded: at most MaxFacts fact lines and MaxFolders folder
// lines, the rest counted with a pointer to search. The tree tool (Read)
// returns the index files from a folder down a number of levels, reading
// only index files: it never walks the store's facts.
package tree

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/agent-sh/tacit/client/internal/fact"
)

// FileName is the index file in every curated folder.
const FileName = fact.IndexFile

// The bounds of one index file. A folder with more facts lists the owner's
// rulings first and then the most recently changed, and counts the rest.
const (
	MaxFacts   = 30
	MaxFolders = 60
	descRunes  = 140
)

// rootOrder is the order of the scope roots in the root index.
var rootOrder = map[string]int{"system": 1, "user": 2, "projects": 3, "machines": 4, "derived": 5}

// Excluded are the top-level folders that hold no index.
var Excluded = map[string]bool{"inbox": true, "review": true, "keys": true}

// Rel is the store-relative path of the index file of folder dir ("" is the
// root).
func Rel(dir string) string {
	if dir == "" || dir == "." {
		return FileName
	}
	return dir + "/" + FileName
}

type folder struct {
	dir     string
	facts   []*fact.Fact // live facts directly in it
	retired int          // superseded or retired facts directly in it
	subs    map[string]bool
}

// rel is where f sits in the tree: its path less prefix ("" for the store;
// the local layer's facts carry local/curated/).
func rel(f *fact.Fact, prefix string) (string, bool) {
	p, ok := strings.CutPrefix(f.Path, prefix)
	if !ok || p == "" {
		return "", false
	}
	top, _, _ := strings.Cut(p, "/")
	if Excluded[top] || !strings.Contains(p, "/") || strings.HasPrefix(top, ".") {
		return "", false
	}
	return path.Dir(p), true
}

// Generate renders every index file of the tree the facts make: path (store
// relative, or relative to the local layer's curated directory with prefix
// "local/curated/") -> content. Inbox facts and facts outside a curated folder
// make no folder. An empty store has the root index alone.
func Generate(facts []*fact.Fact, prefix string) map[string]string {
	folders := map[string]*folder{"": {dir: "", subs: map[string]bool{}}}
	get := func(dir string) *folder {
		fo := folders[dir]
		if fo == nil {
			fo = &folder{dir: dir, subs: map[string]bool{}}
			folders[dir] = fo
		}
		return fo
	}
	for _, f := range facts {
		if f.InInbox() && prefix == "" {
			continue
		}
		dir, ok := rel(f, prefix)
		if !ok {
			continue
		}
		fo := get(dir)
		if f.Meta.SupersededBy != "" || f.Meta.Retired != "" {
			fo.retired++
		} else {
			fo.facts = append(fo.facts, f)
		}
		// Every ancestor holds this folder.
		for d := dir; d != ""; {
			parent := path.Dir(d)
			if parent == "." {
				parent = ""
			}
			get(parent).subs[d] = true
			d = parent
		}
	}
	st := map[string]stats{}
	var total func(dir string) stats
	total = func(dir string) stats {
		if s, ok := st[dir]; ok {
			return s
		}
		fo := folders[dir]
		s := statsOf(fo.facts)
		for sub := range fo.subs {
			s.add(total(sub))
		}
		st[dir] = s
		return s
	}
	out := map[string]string{}
	for dir, fo := range folders {
		out[Rel(dir)] = render(fo, folders, total)
	}
	return out
}

// stats is what a folder's line says about everything under it.
type stats struct {
	facts, rulings int
	types          map[string]int
	latest         string
	words          map[string]int // description words, once per fact
}

func statsOf(fs []*fact.Fact) stats {
	s := stats{types: map[string]int{}, words: map[string]int{}}
	for _, f := range fs {
		s.facts++
		if fact.IsRuling(f) {
			s.rulings++
		}
		s.types[f.Meta.Type]++
		if d := date(f); d > s.latest {
			s.latest = d
		}
		seen := map[string]bool{}
		for _, w := range words(f.Meta.Description) {
			if !seen[w] {
				seen[w] = true
				s.words[w]++
			}
		}
	}
	return s
}

func (s *stats) add(o stats) {
	s.facts += o.facts
	s.rulings += o.rulings
	for k, v := range o.types {
		s.types[k] += v
	}
	for k, v := range o.words {
		s.words[k] += v
	}
	if o.latest > s.latest {
		s.latest = o.latest
	}
}

func date(f *fact.Fact) string {
	if f.Meta.Updated > f.Meta.Created {
		return f.Meta.Updated
	}
	return f.Meta.Created
}

// summary is a folder's one-line description: its counts, its main types,
// its latest change and the words most of its facts' descriptions share.
func (s stats) summary(name string) string {
	if s.facts == 0 {
		return "no live facts"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d %s", s.facts, plural(s.facts, "fact", "facts"))
	if s.rulings > 0 {
		fmt.Fprintf(&b, " (%d owner %s)", s.rulings, plural(s.rulings, "ruling", "rulings"))
	}
	types := topKeys(s.types, 3, 1, nil)
	if len(types) > 0 {
		parts := make([]string, len(types))
		for i, t := range types {
			parts[i] = fmt.Sprintf("%s %d", t, s.types[t])
		}
		b.WriteString("; " + strings.Join(parts, ", "))
	}
	if s.latest != "" {
		b.WriteString("; latest " + s.latest)
	}
	skip := map[string]bool{}
	for _, w := range words(strings.ReplaceAll(name, "-", " ")) {
		skip[w] = true
	}
	if ws := topKeys(s.words, 5, max(2, s.facts/10), skip); len(ws) > 0 {
		b.WriteString("; about: " + strings.Join(ws, ", "))
	}
	return b.String()
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// topKeys are the n keys with the highest counts of at least min, ties by
// name.
func topKeys(m map[string]int, n, min int, skip map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k, v := range m {
		if v >= min && !skip[k] && k != "" {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	if len(keys) > n {
		keys = keys[:n]
	}
	return keys
}

// words are a description's content words: lower case, four letters or
// more, not a common English word.
func words(s string) []string {
	var out []string
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' }) {
		if len([]rune(w)) >= 4 && !common[w] && !allDigits(w) {
			out = append(out, w)
		}
	}
	return out
}

func allDigits(s string) bool {
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// common are English words too frequent to say what a folder is about.
var common = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`about above after again against also always because been before being below between both cannot could does doing down during each every from further have having here into itself just more most must never only other over same should some such than that their them then there these they this those through under until very were what when where which while will with would your yours instead still uses used using need needs needed without within like make makes made want wants first last next once owner agent agents session sessions fact facts says said each work works working file files true false none note notes rule rules ruling rulings check checks using runs`) {
		m[w] = true
	}
	return m
}()

func render(fo *folder, folders map[string]*folder, total func(string) stats) string {
	var b strings.Builder
	title := fo.dir
	if title == "" {
		title = "store root"
	}
	fmt.Fprintf(&b, "# %s\n\nGenerated by `tacit index` from this folder's facts and subfolders; do not edit.\n", title)
	all := total(fo.dir)
	fmt.Fprintf(&b, "\n%s.\n", all.summary(path.Base(fo.dir)))

	subs := make([]string, 0, len(fo.subs))
	for s := range fo.subs {
		subs = append(subs, s)
	}
	if len(subs) > 0 {
		sort.Slice(subs, func(i, j int) bool {
			// The root lists the scope roots in the store's own order.
			if oi, oj := rootOrder[subs[i]], rootOrder[subs[j]]; fo.dir == "" && oi != oj {
				return oi < oj
			}
			a, c := total(subs[i]).facts, total(subs[j]).facts
			if a != c {
				return a > c
			}
			return subs[i] < subs[j]
		})
		fmt.Fprintf(&b, "\n## Folders (%d)\n\n", len(subs))
		shown := subs
		if len(shown) > MaxFolders {
			shown = shown[:MaxFolders]
		}
		for _, s := range shown {
			name := path.Base(s)
			fmt.Fprintf(&b, "- [%s/](%s/%s) %s\n", name, name, FileName, total(s).summary(name))
		}
		if n := len(subs) - len(shown); n > 0 {
			fmt.Fprintf(&b, "- ... and %d more %s, the smallest, not listed: each has its own %s.\n", n, plural(n, "folder", "folders"), FileName)
		}
	}

	if len(fo.facts) > 0 || fo.retired > 0 {
		fs := append([]*fact.Fact(nil), fo.facts...)
		sort.Slice(fs, func(i, j int) bool {
			ri, rj := fact.IsRuling(fs[i]), fact.IsRuling(fs[j])
			if ri != rj {
				return ri
			}
			if di, dj := date(fs[i]), date(fs[j]); di != dj {
				return di > dj
			}
			return fs[i].Meta.Name < fs[j].Meta.Name
		})
		fmt.Fprintf(&b, "\n## Facts (%d)\n\n", len(fs))
		shown := fs
		if len(shown) > MaxFacts {
			shown = shown[:MaxFacts]
		}
		for _, f := range shown {
			typ := f.Meta.Type
			if fact.IsRuling(f) {
				typ += ", owner ruling"
			}
			fmt.Fprintf(&b, "- `%s` (%s): %s\n", f.Meta.Name, typ, fact.ClipDescription(f.Meta.Description, descRunes))
		}
		if n := len(fs) - len(shown); n > 0 {
			fmt.Fprintf(&b, "- ... and %d more %s here, not listed (rulings and the newest are above): search finds them, and any one reads in full by name.\n", n, plural(n, "fact", "facts"))
		}
		if fo.retired > 0 {
			fmt.Fprintf(&b, "\n%d superseded or retired %s here %s not listed.\n", fo.retired, plural(fo.retired, "fact", "facts"), plural(fo.retired, "is", "are"))
		}
	}
	return b.String()
}

// Plan compares the generated index files with what is on disk under dir:
// the files to write (path -> content) and the stale index files to remove
// (in folders that no longer hold facts). Paths are relative to dir, with
// forward slashes.
func Plan(dir string, gen map[string]string) (write map[string][]byte, remove []string, err error) {
	write = map[string][]byte{}
	for rel, want := range gen {
		cur, rerr := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if rerr != nil || string(cur) != want {
			write[rel] = []byte(want)
		}
	}
	existing, err := Existing(dir)
	if err != nil {
		return nil, nil, err
	}
	for _, rel := range existing {
		if _, ok := gen[rel]; !ok {
			remove = append(remove, rel)
		}
	}
	sort.Strings(remove)
	return write, remove, nil
}

// Existing lists the index files under dir (relative, forward slashes),
// outside the folders that hold none and dot directories.
func Existing(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, werr error) error {
		if werr != nil {
			if p == dir {
				return werr
			}
			return nil
		}
		r, _ := filepath.Rel(dir, p)
		r = filepath.ToSlash(r)
		if d.IsDir() {
			if p != dir && (strings.HasPrefix(d.Name(), ".") || (!strings.Contains(r, "/") && Excluded[r])) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == FileName && d.Type().IsRegular() {
			out = append(out, r)
		}
		return nil
	})
	if os.IsNotExist(err) {
		return nil, nil
	}
	sort.Strings(out)
	return out, err
}

// Apply writes and removes what Plan returned.
func Apply(dir string, write map[string][]byte, remove []string) error {
	for _, rel := range sortedKeys(write) {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := writeAtomic(full, write[rel]); err != nil {
			return err
		}
	}
	for _, rel := range remove {
		if err := os.Remove(filepath.Join(dir, filepath.FromSlash(rel))); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func sortedKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// writeAtomic replaces full whole or not at all (a temporary file beside it,
// renamed), keeping an existing file's mode.
func writeAtomic(full string, data []byte) error {
	mode := os.FileMode(0o644)
	if st, err := os.Stat(full); err == nil {
		mode = st.Mode().Perm()
	}
	f, err := os.CreateTemp(filepath.Dir(full), ".index-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_, werr := f.Write(data)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(name, mode)
	}
	if werr == nil {
		werr = os.Rename(name, full)
	}
	if werr != nil {
		os.Remove(name)
	}
	return werr
}

// Check lists the index files under dir that differ from gen, are missing
// or are stale (lint L16). A store that has no root index yet (made before
// the tree) is checked only for the index files it holds.
func Check(dir string, gen map[string]string) ([]string, error) {
	existing, err := Existing(dir)
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, r := range existing {
		have[r] = true
	}
	tree := have[FileName]
	var out []string
	for _, rel := range sortedKeysS(gen) {
		cur, rerr := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		switch {
		case rerr != nil && tree:
			out = append(out, rel+": missing; run `tacit index`")
		case rerr == nil && string(cur) != gen[rel]:
			out = append(out, rel+": differs from the generated index; run `tacit index`")
		}
	}
	for _, r := range existing {
		if _, ok := gen[r]; !ok {
			out = append(out, r+": in a folder that holds no facts; run `tacit index`")
		}
	}
	return out, nil
}

func sortedKeysS(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
