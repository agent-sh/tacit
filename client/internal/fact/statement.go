package fact

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
)

// A statement (SPEC.md, "Statements") is something the owner said once: their
// words verbatim, the day, the session and one line saying what was being
// discussed. It is strong but soft evidence: never a rule, never a RULE line,
// and an agent never changes it. Code writes it, from words it found as a
// statement of theirs in their own turns:
//
//	type: statement
//	description: <the context: what was being discussed>
//	source.kind: observed
//	source.owner_words: <their words>   (a words entry in the quote ledger)
//	source.owner_words_from: claude:<session>
//
// and the body
//
//	You said once (2026-10-21): "<their words>"
//	Context: <the context>
//	Gloss (the curator's reading, not your words): <the model's reading>
//
// A check that disagrees with it appends one line (CheckFlagLine); the
// words above it never change.

// StatementType is the statement's type.
const StatementType = "statement"

// statementOpen starts a statement body's first line.
const statementOpen = "You said once ("

// GlossLabel starts the line that holds the model's reading of a statement:
// labelled, so it is never read as their words.
const GlossLabel = "Gloss (the curator's reading, not your words): "

// IsStatement reports a statement of the owner's whose words the store backs
// (type statement, owner_words recorded in the quote ledger, no Not mine of
// theirs). It is never a ruling.
func IsStatement(f *Fact) bool {
	return f != nil && f.Meta.Type == StatementType && OwnerWordsOf(f) != ""
}

// StatementBody is the body code writes for a statement said on day (YYYY-MM-DD).
func StatementBody(day, words, context, gloss string) string {
	words = strings.Join(strings.Fields(words), " ")
	var b strings.Builder
	fmt.Fprintf(&b, "%s%s): \"%s\"\n", statementOpen, day, words)
	if c := strings.Join(strings.Fields(context), " "); c != "" {
		b.WriteString("Context: " + c + "\n")
	}
	if g := strings.Join(strings.Fields(gloss), " "); g != "" {
		b.WriteString(GlossLabel + g + "\n")
	}
	return b.String()
}

// StatementLine reads a statement body's first line: the day they said it
// and their words. ok is false for a body that does not open with one.
func StatementLine(body string) (day, words string, ok bool) {
	first, _, _ := strings.Cut(strings.TrimLeft(body, "\r\n"), "\n")
	first = strings.TrimRight(first, "\r")
	rest, found := strings.CutPrefix(first, statementOpen)
	if !found {
		return "", "", false
	}
	d, q, found := strings.Cut(rest, "): \"")
	if !found || !IsDate(d) || !strings.HasSuffix(q, "\"") || len(q) < 2 {
		return "", "", false
	}
	return d, q[:len(q)-1], true
}

// StatementContext is the body's context line ("" for none).
func StatementContext(body string) string {
	for _, l := range strings.Split(body, "\n") {
		if c, ok := strings.CutPrefix(strings.TrimRight(l, "\r"), "Context: "); ok {
			return c
		}
	}
	return ""
}

// CheckFlagPrefix starts the line a check that disagrees with a statement
// appends to it: "a check disagrees, maybe misread".
const CheckFlagPrefix = "> check disagrees "

// CheckFlagLine is the flag line for a check's one-line output on day.
func CheckFlagLine(day, check string) string {
	return CheckFlagPrefix + day + ": " + strings.Join(strings.Fields(check), " ")
}

// CheckFlags are a body's flag lines.
func CheckFlags(body string) []string {
	var out []string
	for _, l := range strings.Split(body, "\n") {
		if l = strings.TrimRight(l, "\r"); strings.HasPrefix(l, CheckFlagPrefix) {
			out = append(out, l)
		}
	}
	return out
}

// AppendCheckFlag appends a flag line to a statement file. It only appends:
// the file as it was is a byte prefix of the result, and the result parses
// with the same frontmatter.
func AppendCheckFlag(data []byte, line string) ([]byte, error) {
	if !strings.HasPrefix(line, CheckFlagPrefix) || strings.ContainsAny(line, "\r\n") {
		return nil, errors.New("not a check flag line")
	}
	if _, _, ok := Split(data); !ok {
		return nil, errors.New("no frontmatter block")
	}
	out := append([]byte(nil), data...)
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	out = append(out, line...)
	out = append(out, '\n')
	if !bytes.HasPrefix(out, data) {
		return nil, errors.New("the flag would change the file")
	}
	return out, nil
}

// RedateStatement sets the day a statement file says the owner said its
// words: the day on its body's first line ("You said once (<day>): ...") and
// its created date, which for a statement is that day. Every other byte stays
// as it was, the words included; updated is set to today. It refuses a file
// that is no statement, a day that is not a date, or one after today.
func RedateStatement(data []byte, day, today string) ([]byte, error) {
	if !IsDate(day) || !IsDate(today) || day > today {
		return nil, errors.New("not a day the owner can have said it")
	}
	fm, body, ok := Split(data)
	if !ok {
		return nil, errors.New("no frontmatter block")
	}
	f, err := Parse("", data)
	if err != nil {
		return nil, err
	}
	old, words, ok := StatementLine(string(body))
	if f.Meta.Type != StatementType || !ok {
		return nil, errors.New("not a statement")
	}
	// The body's first line: only its day changes.
	lead := len(body) - len(bytes.TrimLeft(body, "\r\n"))
	from := []byte(statementOpen + old + "): \"")
	if !bytes.HasPrefix(body[lead:], from) {
		return nil, errors.New("the statement's first line is not as code writes it")
	}
	nb := append([]byte(nil), body[:lead]...)
	nb = append(nb, statementOpen+day+"): \""...)
	nb = append(nb, body[lead+len(from):]...)
	// created: one line, as tacit writes it.
	lines := strings.SplitAfter(string(fm), "\n")
	set := false
	for i, l := range lines {
		if strings.HasPrefix(l, "created:") {
			eol := l[len(strings.TrimRight(l, "\r\n")):]
			lines[i] = "created: \"" + day + "\"" + eol
			set = true
			break
		}
	}
	if !set {
		return nil, errors.New("the statement has no created line")
	}
	nl := bytes.IndexByte(data, '\n')
	opener := data[:nl+1]
	closer := data[len(opener)+len(fm) : len(data)-len(body)]
	out := append(append(append(append([]byte(nil), opener...), strings.Join(lines, "")...), closer...), nb...)
	g, err := Parse(f.Path, out)
	if err != nil {
		return nil, err
	}
	g.Meta.Updated = today
	if out, err = PatchRelations(out, g); err != nil {
		return nil, err
	}
	got, err := Parse(f.Path, out)
	if err != nil {
		return nil, err
	}
	d, w, ok := StatementLine(got.Body)
	if !ok || d != day || w != words || got.Meta.Created != day || got.Meta.Source != f.Meta.Source || got.Meta.Name != f.Meta.Name {
		return nil, errors.New("the redated statement does not read back")
	}
	return out, nil
}
