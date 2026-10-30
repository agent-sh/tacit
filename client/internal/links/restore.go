package links

import (
	"sort"
	"strings"

	"github.com/agent-sh/tacit/client/internal/fact"
)

// Restored settles nothing: it is a link to what the fact replaced that a run
// of this step before v0.9.17 unwrapped as dead, put back as it was written
// (Restore, `tacit links --restore`).
const Restored Why = "restored"

// Restore puts back in cur the links that one run of the step unwrapped to
// plain text, for the targets keep says. before and after are the fact file
// before and after that run's commit; cur is the file now. Each line the run
// changed is rebuilt from after with those links back as before wrote them,
// and it replaces that line in cur when the run's text and cur each hold it
// once, word for word. The result must be cur with those links put back and
// nothing else (IsRepair the other way round: a line that moved into code
// gets no link). It returns the new file and how many of each target it put
// back; cur as it is and none when before to after is no repair of this step,
// or nothing can be put back safely.
func Restore(before, after, cur []byte, keep func(target string) bool) ([]byte, map[string]int) {
	if !IsRepair(before, after) {
		return cur, nil
	}
	_, bodyB, _ := fact.Split(before)
	_, bodyA, _ := fact.Split(after)
	spans := fact.LinkSpans(string(bodyB))
	stood, ok := repairChoices(string(bodyB), string(bodyA), spans)
	if !ok {
		return cur, nil
	}
	// after's body with the kept links back, and what each line got back.
	var b strings.Builder
	from := 0
	got := map[int]map[string]int{} // line of the body -> target -> count
	for i, sp := range spans {
		b.WriteString(string(bodyB[from:sp.Start]))
		text := stood[i]
		if orig := string(bodyB[sp.Start:sp.End]); text != orig && text == Replacement(sp, Edit{}) && keep(strings.TrimSpace(sp.Target)) {
			line := strings.Count(b.String(), "\n")
			if got[line] == nil {
				got[line] = map[string]int{}
			}
			got[line][strings.TrimSpace(sp.Target)]++
			text = orig
		}
		b.WriteString(text)
		from = sp.End
	}
	b.WriteString(string(bodyB[from:]))
	restored := strings.Split(b.String(), "\n")
	wrote := strings.Split(string(bodyA), "\n")
	if len(restored) != len(wrote) || len(got) == 0 {
		return cur, nil
	}
	_, bodyC, okC := fact.Split(cur)
	if !okC {
		return cur, nil
	}
	head := string(cur[:len(cur)-len(bodyC)])
	now := strings.Split(string(bodyC), "\n")
	count := func(lines []string, l string) (at, n int) {
		at = -1
		for i, x := range lines {
			if x == l {
				at, n = i, n+1
			}
		}
		return at, n
	}
	lines := make([]int, 0, len(got))
	for line := range got {
		lines = append(lines, line)
	}
	sort.Ints(lines)
	out := map[string]int{}
	for _, line := range lines {
		if _, n := count(wrote, wrote[line]); n != 1 {
			continue // the run wrote that text twice: which one it was is not known
		}
		at, n := count(now, wrote[line])
		if n != 1 {
			continue // edited since, or not one place to put it
		}
		now[at] = restored[line]
		for t, c := range got[line] {
			out[t] += c
		}
	}
	if len(out) == 0 {
		return cur, nil
	}
	res := []byte(head + strings.Join(now, "\n"))
	if !IsRepair(res, cur) {
		return cur, nil // a link would land in code, or change more than the link
	}
	return res, out
}

// repairChoices is, for each link span of bodyB, the text that stands for it
// in bodyA: the link itself, the plain text the step unwraps it to, or a link
// to another target with its label. ok is false when bodyA is not bodyB with
// its links repaired that way (the matching of IsRepair).
func repairChoices(bodyB, bodyA string, spans []fact.LinkSpan) ([]string, bool) {
	segs := make([]string, 0, len(spans)+1)
	from := 0
	for _, sp := range spans {
		segs = append(segs, bodyB[from:sp.Start])
		from = sp.End
	}
	segs = append(segs, bodyB[from:])
	choice := make([]string, len(spans))
	failed := map[[2]int]bool{}
	var match func(i, pos int) bool
	match = func(i, pos int) bool {
		if !strings.HasPrefix(bodyA[pos:], segs[i]) {
			return false
		}
		pos += len(segs[i])
		if i == len(spans) {
			return pos == len(bodyA)
		}
		if failed[[2]int{i, pos}] {
			return false
		}
		rest := bodyA[pos:]
		sp := spans[i]
		opts := []string{bodyB[sp.Start:sp.End], Replacement(sp, Edit{})}
		if nl, isLink := fact.LinkAt(rest); isLink && nl.Suffix == label(sp) {
			opts = append(opts, rest[:nl.End])
		}
		for _, o := range opts {
			if strings.HasPrefix(rest, o) && match(i+1, pos+len(o)) {
				choice[i] = o
				return true
			}
		}
		failed[[2]int{i, pos}] = true
		return false
	}
	if !match(0, 0) {
		return nil, false
	}
	return choice, true
}
