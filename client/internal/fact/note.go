package fact

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// An in-place note (SPEC.md, "Never delete"): when a fact still holds but a
// detail of it changed, the curator does not rewrite its words. It appends
// one line to the body,
//
//	> superseded 2026-10-21: v0.7.17 -> v0.7.19
//
// and sets updated. Nothing before the line changes.

// NotePrefix starts every in-place note line.
const NotePrefix = "> superseded "

// noteLine is the whole form: the prefix, a date, the old detail, the new.
var noteLine = regexp.MustCompile(`^> superseded (\d{4}-\d{2}-\d{2}): (.+?) -> (.+)$`)

// noteSide is the longest an old or new detail may be.
const noteSide = 300

// NoteLine builds the note that says old became new on date. Both sides are
// one line each, non-empty, at most noteSide characters.
func NoteLine(date, old, new string) (string, error) {
	old, new = strings.TrimSpace(old), strings.TrimSpace(new)
	switch {
	case !rePlainDate.MatchString(date):
		return "", errors.New("the date is not YYYY-MM-DD")
	case old == "" || new == "":
		return "", errors.New("a note needs the old detail and the new one")
	case strings.ContainsAny(old+new, "\r\n"):
		return "", errors.New("a note is one line")
	case len([]rune(old)) > noteSide || len([]rune(new)) > noteSide:
		return "", fmt.Errorf("a side of a note is at most %d characters", noteSide)
	case old == new:
		return "", errors.New("the old detail and the new one are the same")
	}
	line := NotePrefix + date + ": " + old + " -> " + new
	if err := CheckNote(line); err != nil {
		return "", err
	}
	return line, nil
}

// CheckNote reports whether a body line that starts with NotePrefix has the
// note's form.
func CheckNote(line string) error {
	m := noteLine.FindStringSubmatch(strings.TrimRight(line, "\r"))
	if m == nil || strings.TrimSpace(m[2]) == "" || strings.TrimSpace(m[3]) == "" {
		return fmt.Errorf("not an in-place note (%q): want \"> superseded YYYY-MM-DD: <old> -> <new>\"", line)
	}
	return nil
}

// Notes are the body's in-place note lines, and the lines that start like
// one but do not have its form.
func Notes(body string) (notes, bad []string) {
	for _, l := range strings.Split(body, "\n") {
		l = strings.TrimRight(l, "\r")
		if !strings.HasPrefix(l, NotePrefix) {
			continue
		}
		if CheckNote(l) != nil {
			bad = append(bad, l)
		} else {
			notes = append(notes, l)
		}
	}
	return notes, bad
}

// AppendNote appends one in-place note line to a fact file's body. It only
// appends: the file as it was is a byte prefix of the result (a blank line
// separates the first note from the text above), and the result parses as a
// fact with the same frontmatter. The caller sets updated (PatchRelations).
func AppendNote(data []byte, line string) ([]byte, error) {
	if err := CheckNote(line); err != nil {
		return nil, err
	}
	if _, _, ok := Split(data); !ok {
		return nil, errors.New("no frontmatter block")
	}
	eol := "\n"
	if bytes.Contains(data, []byte("\r\n")) {
		eol = "\r\n"
	}
	out := append([]byte(nil), data...)
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, eol...)
	}
	_, body, _ := Split(data)
	lines := strings.Split(strings.TrimRight(string(body), "\r\n"), "\n")
	if last := strings.TrimRight(lines[len(lines)-1], "\r"); !strings.HasPrefix(last, NotePrefix) && last != "" {
		out = append(out, eol...)
	}
	out = append(out, line+eol...)
	if !bytes.HasPrefix(out, data) {
		return nil, errors.New("a note must only append")
	}
	before, err := Parse("note.md", data)
	if err != nil {
		return nil, err
	}
	after, err := Parse("note.md", out)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(after.Body, strings.TrimRight(before.Body, "\r\n")) || before.Meta.Name != after.Meta.Name || before.Meta.Updated != after.Meta.Updated {
		return nil, errors.New("the note changed more than the body's end")
	}
	return out, nil
}
