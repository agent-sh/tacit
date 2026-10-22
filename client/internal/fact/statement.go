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
