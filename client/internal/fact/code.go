package fact

import (
	"regexp"
	"sort"
	"strings"
)

// A `[[table]]` header in a TOML snippet, a wiki-style example in a fenced
// block or an inline code span is text about code, not a link to a fact. The
// ranges below are the Markdown code of a body (CommonMark): fenced blocks
// opened by three or more backticks or tildes, indented code blocks, and inline
// code spans.

var fenceRe = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})(.*)$")

// codeRanges returns the byte ranges of body that are code, sorted and
// disjoint: each fenced block from its opening line to its closing line (to the
// end of the body when it is never closed), and each inline code span from its
// opening backtick run to its closing one.
func codeRanges(body string) [][2]int {
	var out [][2]int
	// Fenced blocks, line by line.
	type fence struct {
		char byte
		n    int
		from int
	}
	var open *fence
	// Indented code blocks: a run of lines indented by four spaces or a tab that
	// starts after a blank line (or at the start) and is not inside a list, where
	// such a line is a paragraph of the list item.
	prevBlank, inList := true, false
	chunkFrom, chunkTo := -1, 0
	endChunk := func() {
		if chunkFrom >= 0 {
			out = append(out, [2]int{chunkFrom, chunkTo})
			chunkFrom = -1
		}
	}
	for off := 0; off < len(body); {
		end := strings.IndexByte(body[off:], '\n')
		next := len(body)
		line := body[off:]
		if end >= 0 {
			line, next = body[off:off+end], off+end+1
		}
		line = strings.TrimSuffix(line, "\r")
		if open == nil {
			if m := fenceRe.FindStringSubmatch(line); m != nil && !(m[1][0] == '`' && strings.Contains(m[2], "`")) {
				endChunk()
				open = &fence{char: m[1][0], n: len(m[1]), from: off}
				prevBlank = false
			} else {
				indented := strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t")
				switch {
				case strings.TrimSpace(line) == "":
					prevBlank = true
				case indented && (chunkFrom >= 0 || (prevBlank && !inList)):
					if chunkFrom < 0 {
						chunkFrom = off
					}
					chunkTo = next
					prevBlank = false
				default:
					endChunk()
					if listItemRe.MatchString(line) {
						inList = true
					} else if !indented {
						inList = false
					}
					prevBlank = false
				}
			}
		} else if m := fenceRe.FindStringSubmatch(line); m != nil && m[1][0] == open.char && len(m[1]) >= open.n && strings.TrimSpace(m[2]) == "" {
			out = append(out, [2]int{open.from, next})
			open = nil
			prevBlank = false
		}
		off = next
	}
	endChunk()
	if open != nil {
		out = append(out, [2]int{open.from, len(body)})
	}
	sortRanges(out)
	// Inline spans in what the fences leave.
	at := func(p int) int { // end of the fenced range starting at or holding p, or -1
		for _, r := range out {
			if p >= r[0] && p < r[1] {
				return r[1]
			}
		}
		return -1
	}
	nextFence := func(p int) int {
		for _, r := range out {
			if r[0] >= p {
				return r[0]
			}
		}
		return len(body)
	}
	var spans [][2]int
	for i := 0; i < len(body); {
		if e := at(i); e >= 0 {
			i = e
			continue
		}
		if body[i] == '\\' {
			i += 2
			continue
		}
		if body[i] != '`' {
			i++
			continue
		}
		j := i
		for j < len(body) && body[j] == '`' {
			j++
		}
		n := j - i
		// the closing run: exactly n backticks, before a blank line or a fence
		limit := nextFence(j)
		if blank := blankLine(body[j:limit]); blank >= 0 {
			limit = j + blank
		}
		closeAt := -1
		for k := j; k < limit; {
			if body[k] != '`' {
				k++
				continue
			}
			m := k
			for m < limit && body[m] == '`' {
				m++
			}
			if m-k == n {
				closeAt = m
				break
			}
			k = m
		}
		if closeAt < 0 {
			i = j // literal backticks
			continue
		}
		spans = append(spans, [2]int{i, closeAt})
		i = closeAt
	}
	out = append(out, spans...)
	sortRanges(out)
	return out
}

func sortRanges(r [][2]int) {
	sort.Slice(r, func(i, j int) bool { return r[i][0] < r[j][0] })
}

// A list item starts a list context: an indented line after a blank line in it
// is a paragraph of the item, not code.
var listItemRe = regexp.MustCompile(`^\s*([-*+]|\d{1,9}[.)])(\s|$)`)

// blankLine returns the offset in s of the first blank line (a newline, spaces
// and tabs, a newline), or -1: a paragraph ends there and an inline span
// cannot cross it.
func blankLine(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] != '\n' {
			continue
		}
		j := i + 1
		for j < len(s) && (s[j] == ' ' || s[j] == '\t' || s[j] == '\r') {
			j++
		}
		if j < len(s) && s[j] == '\n' {
			return i
		}
	}
	return -1
}

// inCode reports whether the byte range [start, end) touches code.
func inCode(ranges [][2]int, start, end int) bool {
	for _, r := range ranges {
		if start < r[1] && end > r[0] {
			return true
		}
	}
	return false
}
