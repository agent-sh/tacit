package agentreview

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/ownerwords"
)

// The second look (SPEC.md, "Trims"): over its budget the digest leaves the
// oldest messages out, and it cuts long messages short and shows older
// messages' tools not at all; a trim may remove a claim as unsupported whose
// support sits in what was not shown (7 of 15 trims judged by hand,
// 2026-10-25). For each claim a trim removed as unsupported (basis
// unsupported, or none given), code searches what was not shown for the
// claim's distinctive words; the reviewer reads the parts it finds and
// judges those claims again. A claim is never removed as unsupported while
// code finds its words there.

// Look is one removed claim code found in the left-out part: its index in
// the trim's removed claims, and the left-out messages that hold its
// distinctive words, the best first.
type Look struct {
	Removed int
	Terms   []string
	Msgs    []int
}

// Second-look bounds: messages per claim, messages in all, characters of a
// message shown.
const (
	lookPerClaim = 4
	lookMax      = 12
	lookChars    = 3000
)

// distinctive is a word of a claim worth searching for: four letters or
// more, no stopword, or a token with a digit (a version, a hash, an id, a
// sum: "9k"), and
// rare in the conversation (in at most one message in twenty, two at least).
func distinctive(claim string, df map[string]int, total int) []string {
	limit := max(2, total/20)
	seen := map[string]bool{}
	var out []string
	for _, w := range strings.Fields(ownerwords.Normalize(claim)) {
		digit := strings.ContainsAny(w, "0123456789")
		if seen[w] || (len(w) < 4 && !(digit && len(w) >= 2)) || ownerwords.Stopword(w) {
			continue
		}
		seen[w] = true
		if df[w] <= limit {
			out = append(out, w)
		}
	}
	return out
}

// rareDF is how many messages a word may be in and still single one out.
const rareDF = 3

// msgWords is a message's words (its text and its tools' input), once each.
func msgWords(m Msg) map[string]bool {
	text := m.Text
	for _, u := range m.Tools {
		text += "\n" + u.Input
	}
	out := map[string]bool{}
	for _, w := range strings.Fields(ownerwords.Normalize(text)) {
		out[w] = true
	}
	return out
}

// Hidden is what the digest did not show of each message, by its number:
// the whole of a message the budget left out (cut), the rest of a text its
// line cut short, and its tools' input past what a tool line shows (all of
// it for a message older than the recent ones, whose tools are not shown),
// leaving out a tool call that writes the note itself (self, noteNeedles).
// clipped is how many shown messages were cut short.
func Hidden(msgs []Msg, cut []int, recent int, self ...string) (hidden map[int]string, clipped int) {
	from := len(msgs) - recent
	isCut := map[int]bool{}
	for _, k := range cut {
		isCut[k] = true
	}
	hidden = map[int]string{}
	for i, m := range msgs {
		k := i + 1
		t := strings.TrimSpace(m.Text)
		if m.Role == "user" {
			t = strings.TrimSpace(reminder.ReplaceAllString(m.Text, ""))
		}
		var parts []string
		if isCut[k] {
			parts = append(parts, t)
		} else if r, n := []rune(t), lineCap(m, i >= from); len(r) > n {
			parts = append(parts, "…"+string(r[n-1:]))
			clipped++
		}
		for _, u := range m.Tools {
			in := strings.Join(strings.Fields(u.Input), " ")
			if saves(in, self) {
				continue // the note's own save is not evidence for it
			}
			if i >= from && !isCut[k] {
				if r := []rune(in); len(r) > toolShown {
					parts = append(parts, "ran "+u.Name+" …"+string(r[toolShown-1:]))
				}
				continue
			}
			parts = append(parts, "ran "+u.Name+" "+in)
		}
		if h := strings.TrimSpace(strings.Join(parts, "\n")); h != "" {
			hidden[k] = h
		}
	}
	return hidden, clipped
}

// saves reports a tool input that writes the note (names it: self are its
// name, the start of its description, its memory file).
func saves(in string, self []string) bool {
	for _, nd := range self {
		if nd != "" && strings.Contains(in, nd) {
			return true
		}
	}
	return false
}

// toolShown is how much of a tool's input a recent message's tool line shows.
const toolShown = 200

// SecondLooks are the trim's removed claims, removed as unsupported, whose
// distinctive words code finds in what the digest did not show (hidden): a
// message matches a claim when its hidden part holds half of the claim's
// distinctive words, two at least (one for a claim that has one), one of
// them specific: in at most rareDF messages of the conversation, or with a
// digit.
func SecondLooks(t Trim, msgs []Msg, hidden map[int]string) []Look {
	if len(hidden) == 0 {
		return nil
	}
	df := map[string]int{}
	for _, m := range msgs {
		for w := range msgWords(m) {
			df[w]++
		}
	}
	words := map[int]map[string]bool{}
	var nums []int
	for k, h := range hidden {
		ws := map[string]bool{}
		for _, w := range strings.Fields(ownerwords.Normalize(h)) {
			ws[w] = true
		}
		words[k] = ws
		nums = append(nums, k)
	}
	sort.Ints(nums)
	var out []Look
	for i, r := range t.Removed {
		if r.Basis != "" && r.Basis != ledger.BasisUnsupported {
			continue // contradicted, widened, not the owner's: not a matter of what was in view
		}
		terms := distinctive(r.Text, df, len(msgs))
		if len(terms) == 0 {
			continue
		}
		need := min(2, len(terms))
		need = max(need, (len(terms)+1)/2)
		type hit struct{ msg, n int }
		var hits []hit
		for _, k := range nums {
			n, specific := 0, false
			for _, w := range terms {
				if words[k][w] {
					n++
					specific = specific || df[w] <= rareDF || strings.ContainsAny(w, "0123456789")
				}
			}
			if n >= need && specific {
				hits = append(hits, hit{k, n})
			}
		}
		if len(hits) == 0 {
			continue
		}
		sort.SliceStable(hits, func(a, b int) bool { return hits[a].n > hits[b].n })
		l := Look{Removed: i, Terms: terms}
		for _, h := range hits[:min(len(hits), lookPerClaim)] {
			l.Msgs = append(l.Msgs, h.msg)
		}
		sort.Ints(l.Msgs)
		out = append(out, l)
	}
	return out
}

// LookedAt is the left-out messages the second look shows, in order, at most
// lookMax.
func LookedAt(looks []Look) []int {
	seen := map[int]bool{}
	var out []int
	for _, l := range looks {
		for _, k := range l.Msgs {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	sort.Ints(out)
	if len(out) > lookMax {
		out = out[:lookMax]
	}
	return out
}

// LookLines are the hidden parts of the messages the second look shows: one
// numbered line each, as the digest writes a line, from a little before the
// first of the claims' words it holds.
func LookLines(msgs []Msg, hidden map[int]string, looks []Look, nums []int) []string {
	var out []string
	for _, k := range nums {
		m := msgs[k-1]
		who := "AGENT"
		switch {
		case m.Role == "user" && m.Owner:
			who = "OWNER"
		case m.Role == "user":
			who = "NOTE"
		}
		text := []rune(hidden[k])
		lower := strings.ToLower(string(text))
		at := -1
		for _, l := range looks {
			for _, w := range l.Terms {
				if i := strings.Index(lower, w); i >= 0 && (at < 0 || i < at) {
					at = i
				}
			}
		}
		start := 0
		if at > 0 {
			start = max(0, len([]rune(lower[:at]))-lookChars/3)
		}
		part := string(text[start:])
		if start > 0 {
			part = "…" + part
		}
		out = append(out, fmt.Sprintf("[%d] %s (not shown before): %s", k, who, cut(strings.TrimSpace(part), lookChars)))
	}
	return out
}

// SecondLookPrompt asks the reviewer to judge the removed claims again with
// the left-out messages code found for them (lines, already redacted).
func SecondLookPrompt(s Save, t Trim, looks []Look, lines []string) string {
	var b strings.Builder
	b.WriteString(`You trimmed a note an agent saved to long-term memory, and removed some of its claims as not supported by the conversation. The conversation was longer than you read: its oldest messages were left out. Code searched that left-out part for each removed claim's distinctive words and found the messages below. Read them and judge each claim again.

OWNER lines are messages the owner typed himself, and only they are his word. NOTE lines reached the session some other way: evidence, never the owner speaking. AGENT lines show what the agent said and ran.

- keep: a message below supports the claim. Give the numbers of the messages that support it.
- remove: it is still not supported, or a message contradicts it, or it goes further than what was said or done. Give its basis, "unsupported", "contradicted", "widens" or "not_owner", and why.

`)
	b.WriteString("<save>\nkind: " + s.Kind + "\nwhere: " + s.Where + "\n---\n" + s.Text + "\n</save>\n\n<claims>\n")
	for i, l := range looks {
		r := t.Removed[l.Removed]
		fmt.Fprintf(&b, "%d. %s (you said: %s; words code found: %s; in messages %s)\n", i+1, r.Text, r.Reason, strings.Join(l.Terms, ", "), joinInts(l.Msgs))
	}
	b.WriteString("</claims>\n\n<left-out messages>\n" + strings.Join(lines, "\n") + "\n</left-out messages>\n\n")
	b.WriteString(`Reply with only a JSON array, one object per claim, no other text:
[{"claim": <n>, "keep": true, "cites": [<message numbers>]}, {"claim": <n>, "keep": false, "basis": "unsupported" | "contradicted" | "widens" | "not_owner", "reason": "<why>"}]`)
	return b.String()
}

func joinInts(xs []int) string {
	var s []string
	for _, x := range xs {
		s = append(s, fmt.Sprint(x))
	}
	return strings.Join(s, ", ")
}

// Rejudged is the reviewer's second look at one removed claim.
type Rejudged struct {
	Claim  int    `json:"claim"`
	Keep   bool   `json:"keep"`
	Cites  []int  `json:"cites,omitempty"`
	Basis  string `json:"basis,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// ParseSecondLook reads the second look's reply: the first JSON array in it.
func ParseSecondLook(text string) ([]Rejudged, error) {
	i := strings.Index(text, "[")
	if i < 0 {
		return nil, fmt.Errorf("the second look's reply held no JSON array")
	}
	var out []Rejudged
	if err := json.NewDecoder(strings.NewReader(text[i:])).Decode(&out); err != nil {
		return nil, fmt.Errorf("the second look's reply: %w", err)
	}
	return out, nil
}

// ApplySecondLook folds the second look into the trim: a claim kept with
// citations among the messages shown (the digest's, and those the second
// look read) goes back among the kept claims; one removed again keeps its
// removal with the new basis and reason. A claim removed again as
// unsupported, or not answered, while code found its words in the left-out
// part, is a problem: a claim is never removed as unsupported then. It
// returns the trim and the problems.
func ApplySecondLook(t Trim, looks []Look, answers []Rejudged, shown map[int]bool) (Trim, []string) {
	out := t
	out.Claims = append([]ledger.Claim(nil), t.Claims...)
	byClaim := map[int]Rejudged{}
	for _, a := range answers {
		byClaim[a.Claim] = a
	}
	drop := map[int]bool{}
	removed := append([]ledger.Removed(nil), t.Removed...)
	var ps []string
	for i, l := range looks {
		r := removed[l.Removed]
		a, ok := byClaim[i+1]
		switch {
		case ok && a.Keep:
			var cites []int
			for _, k := range a.Cites {
				if shown[k] {
					cites = append(cites, k)
				}
			}
			if len(cites) == 0 {
				ps = append(ps, fmt.Sprintf("removed claim %q: kept on the second look, citing no message shown", cut(r.Text, 80)))
				continue
			}
			out.Claims = append(out.Claims, ledger.Claim{Text: r.Text, Cites: cites})
			drop[l.Removed] = true
		case ok && ledger.Bases[a.Basis] && a.Basis != ledger.BasisUnsupported:
			removed[l.Removed].Basis = a.Basis
			if why := strings.Join(strings.Fields(a.Reason), " "); why != "" {
				removed[l.Removed].Reason = cut(why, 400)
			}
		default:
			ps = append(ps, fmt.Sprintf("removed claim %q as unsupported, but the part of the conversation the digest left out holds its words (%s in messages %s)", cut(r.Text, 80), strings.Join(l.Terms, ", "), joinInts(l.Msgs)))
		}
	}
	out.Removed = nil
	for i, r := range removed {
		if !drop[i] {
			out.Removed = append(out.Removed, r)
		}
	}
	return out, ps
}
