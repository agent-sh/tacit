package fact

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
)

var rePlainDate = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// PatchRelations rewrites only the updated, superseded_by, retired,
// last_checked and conflicts_with lines of a fact file to match f, leaving
// every other byte as it was. A
// field that is empty in f loses its line; a missing one is added before the
// closing ---. Applying the old values back restores the original bytes when
// they were written in this same layout, which is what tacit writes.
func PatchRelations(data []byte, f *Fact) ([]byte, error) {
	fm, body, ok := Split(data)
	if !ok {
		return nil, errors.New("no frontmatter block")
	}
	// Keep the file's own delimiter lines and line ending.
	nl := strings.IndexByte(string(data), '\n')
	opener := string(data[:nl+1])
	closer := string(data[len(opener)+len(fm) : len(data)-len(body)])
	eol := "\n"
	if strings.HasSuffix(opener, "\r\n") {
		eol = "\r\n"
	}
	lines := strings.SplitAfter(string(fm), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	set := func(key, val string) {
		start, end := -1, -1
		for i, l := range lines {
			if strings.HasPrefix(l, key+":") {
				start, end = i, i+1
				for end < len(lines) && (strings.HasPrefix(lines[end], " ") || strings.HasPrefix(lines[end], "- ")) {
					end++
				}
				break
			}
		}
		var repl []string
		if val != "" {
			repl = []string{key + ": " + val + eol}
		}
		if start < 0 {
			lines = append(lines, repl...)
			return
		}
		lines = append(lines[:start], append(repl, lines[end:]...)...)
	}
	if f.Meta.Updated != "" && !rePlainDate.MatchString(f.Meta.Updated) {
		return nil, errors.New("updated is not a YYYY-MM-DD date")
	}
	if f.Meta.SupersededBy != "" && !IsSlug(f.Meta.SupersededBy) {
		return nil, errors.New("superseded_by is not a slug")
	}
	if f.Meta.Retired != "" && !rePlainDate.MatchString(f.Meta.Retired) {
		return nil, errors.New("retired is not a YYYY-MM-DD date")
	}
	if f.Meta.LastChecked != "" && !rePlainDate.MatchString(f.Meta.LastChecked) {
		return nil, errors.New("last_checked is not a YYYY-MM-DD date")
	}
	conflicts := ""
	if len(f.Meta.ConflictsWith) > 0 {
		for _, n := range f.Meta.ConflictsWith {
			if !IsSlug(n) {
				return nil, errors.New("conflicts_with holds a non-slug")
			}
		}
		b, _ := json.Marshal(f.Meta.ConflictsWith)
		conflicts = strings.ReplaceAll(string(b), `","`, `", "`)
	}
	// A field whose value does not change keeps its line as written.
	cur, err := Parse(f.Path, data)
	if err != nil {
		return nil, err
	}
	if cur.Meta.Updated != f.Meta.Updated {
		set("updated", f.Meta.Updated)
	}
	if cur.Meta.SupersededBy != f.Meta.SupersededBy {
		set("superseded_by", f.Meta.SupersededBy)
	}
	if cur.Meta.Retired != f.Meta.Retired {
		set("retired", f.Meta.Retired)
	}
	if cur.Meta.LastChecked != f.Meta.LastChecked {
		set("last_checked", f.Meta.LastChecked)
	}
	if strings.Join(cur.Meta.ConflictsWith, "\x00") != strings.Join(f.Meta.ConflictsWith, "\x00") {
		set("conflicts_with", conflicts)
	}

	if n := len(lines); n > 0 && !strings.HasSuffix(lines[n-1], "\n") {
		lines[n-1] += eol
	}
	out := opener + strings.Join(lines, "") + closer + string(body)
	// The patch must mean exactly what f says.
	got, err := Parse(f.Path, []byte(out))
	if err != nil {
		return nil, err
	}
	if got.Meta.Updated != f.Meta.Updated || got.Meta.SupersededBy != f.Meta.SupersededBy || got.Meta.Retired != f.Meta.Retired || got.Meta.LastChecked != f.Meta.LastChecked || strings.Join(got.Meta.ConflictsWith, "\x00") != strings.Join(f.Meta.ConflictsWith, "\x00") {
		return nil, errors.New("patched frontmatter does not read back")
	}
	return []byte(out), nil
}
