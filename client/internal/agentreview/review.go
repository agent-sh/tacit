package agentreview

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/ledger"
)

// Runner asks the deciding model one prompt: its text, cost and the model
// that answered.
type Runner func(ctx context.Context, prompt string) (text string, cost float64, model string, err error)

// Reviewer judges agents' notes against their conversations.
type Reviewer struct {
	Sources Sources
	Run     Runner
	// Redact masks secrets in the conversation before any model reads it
	// (scrump, fail closed: an error means no review).
	Redact func(texts []string) ([]string, error)
	Now    func() time.Time
}

// SessionOf is the harness whose session a note came from and its id: the
// harness that saved it, or, for a memory imported by `tacit import`, the
// harness whose memory directory it was read from. ok is false for a note
// that names no session.
func SessionOf(n Note) (harness, id string, ok bool) {
	harness = n.Harness
	if harness == "import" || harness == "" {
		harness = harnessOfPath(n.ImportedFrom)
	}
	id = n.Session
	if i := strings.Index(id, ":"); i > 0 && strings.Contains("claude codex pi hermes", id[:i]) {
		harness, id = id[:i], id[i+1:]
	}
	if harness == "" || id == "" || !ValidSession(id) {
		return harness, id, false
	}
	return harness, id, true
}

func harnessOfPath(p string) string {
	p = filepath.ToSlash(p)
	switch {
	case strings.Contains(p, "/.claude/"):
		return "claude"
	case strings.Contains(p, "/.codex/"):
		return "codex"
	case strings.Contains(p, "/.hermes/"):
		return "hermes"
	case strings.Contains(p, "/.pi/"):
		return "pi"
	}
	return ""
}

// savePoint is the message the note was saved at: the newest agent message
// whose tool call names the note (its name, the start of its description,
// the memory file), and the end of what the reviewer reads is a little
// after it; len(msgs) when none is found.
func savePoint(msgs []Msg, n Note) int {
	needles := noteNeedles(n)
	for i := len(msgs) - 1; i >= 0; i-- {
		for _, t := range msgs[i].Tools {
			in := strings.Join(strings.Fields(t.Input), " ")
			for _, nd := range needles {
				if nd != "" && strings.Contains(in, nd) {
					return min(len(msgs), i+afterSave)
				}
			}
		}
	}
	return len(msgs)
}

// noteNeedles are what a tool call that saves the note holds: its name, the
// start of its description, its memory file.
func noteNeedles(n Note) []string {
	var needles []string
	if n.Name != "" {
		needles = append(needles, n.Name)
	}
	if d := strings.Join(strings.Fields(n.Desc), " "); len(d) >= 24 {
		needles = append(needles, d[:24])
	}
	if n.ImportedFrom != "" {
		needles = append(needles, filepath.Base(n.ImportedFrom))
	}
	return needles
}

// afterSave is how many messages after the save the reviewer still reads:
// the owner's reaction to it is evidence too.
const afterSave = 20

// Result is one review: its ledger line, the conversation it was judged
// against (Cwd for the question), and whether a model was asked. For an
// uncertain note the reviewer cut down, Trim is its trim when code's check
// passed it (CheckTrim), TrimProblems why not, and Msgs the messages the
// trim's numbers name.
type Result struct {
	Line         ledger.Review
	Conv         Conversation
	Called       bool
	Trim         *Trim
	TrimProblems []string
	Msgs         []Msg
}

// Review judges one note. A note with no session, or whose session is not on
// this machine, gets nosource with no model call: a low-weight agent's note,
// never trusted. An error (redaction, the model, a reply with no verdict)
// is no verdict: the note waits for the next run.
func (rv Reviewer) Review(ctx context.Context, n Note) (Result, error) {
	now := time.Now
	if rv.Now != nil {
		now = rv.Now
	}
	line := ledger.Review{Note: n.Path, Name: n.Name, Hash: ledger.NoteHash(n.Desc, n.Body), By: ledger.ByTacit, At: now().UTC().Format(time.RFC3339)}
	harness, id, ok := SessionOf(n)
	if !ok {
		line.Verdict, line.Reason = ledger.VerdictNoSource, "the note names no session to judge it against"
		return Result{Line: line}, nil
	}
	line.Session = harness + ":" + id
	conv, err := rv.Sources.Load(ctx, harness, id)
	if errors.Is(err, ErrNoSession) {
		line.Verdict, line.Reason = ledger.VerdictNoSource, fmt.Sprintf("its %s session %s is not on this machine", harness, shortID(id))
		return Result{Line: line}, nil
	}
	if err != nil {
		return Result{Line: line}, fmt.Errorf("reading its session: %w", err)
	}
	conv.Harness, conv.Session = harness, id
	msgs := conv.Msgs[:savePoint(conv.Msgs, n)]
	text, shown, omitted := DigestCut(msgs, DigestBudget, DigestRecent)
	hidden, clipped := Hidden(msgs, omitted, DigestRecent, noteNeedles(n)...)
	save := Save{Kind: "tacit note", Label: n.Type, Where: "tacit scope " + n.Scope + ", name " + n.Name, Text: n.Desc + "\n" + n.Body, Cwd: conv.Cwd, Omitted: Omission(omitted, clipped, len(msgs))}
	line.Omitted = len(omitted)
	if n.ImportedFrom != "" {
		save.Kind, save.Where = "memory file", "memory file "+n.ImportedFrom
	}
	if rv.Redact == nil {
		return Result{Line: line, Conv: conv}, errors.New("no redaction: the conversation is not sent")
	}
	red, err := rv.Redact([]string{text, save.Text})
	if err != nil {
		return Result{Line: line, Conv: conv}, fmt.Errorf("redaction unavailable, not reviewed: %w", err)
	}
	text, save.Text = red[0], red[1]
	out, cost, model, err := rv.Run(ctx, Prompt(save, text))
	res := Result{Line: line, Conv: conv, Called: true}
	res.Line.CostUSD, res.Line.Model = cost, model
	if err != nil {
		return res, err
	}
	v, err := Parse(out)
	if err != nil {
		return res, err
	}
	res.Line.Verdict, res.Line.Reason, res.Line.Doubt, res.Line.Kind = v.Verdict, v.Reason, v.Doubt, v.Kind
	if v.Trim != nil {
		t := *v.Trim
		var ps []string
		// A claim removed as unsupported whose words code finds in what the
		// digest left out gets a second look with those messages.
		if looks := SecondLooks(t, msgs, hidden); len(looks) > 0 {
			t, ps = rv.secondLook(ctx, &res, save, t, looks, msgs, hidden, shown)
		}
		// The trim is checked against the conversation as the model read it
		// (redacted): its citations name those messages.
		ps = append(ps, CheckTrim(n, msgs, shown, v.Kind, t)...)
		if t.OneOff != nil && t.OneOff.Relevance != nil {
			j := *t.OneOff.Relevance
			res.Line.Relevance = &j
		}
		if len(ps) > 0 {
			res.TrimProblems = ps
		} else {
			res.Trim, res.Msgs = &t, msgs
		}
	}
	return res, nil
}

// secondLook asks the reviewer about the removed claims code found in the
// left-out part, with those messages (redacted), and folds its answer into
// the trim; the messages it read count as shown. A failed call is a problem
// of the trim (the note keeps its uncertain verdict), not of the review.
func (rv Reviewer) secondLook(ctx context.Context, res *Result, save Save, t Trim, looks []Look, msgs []Msg, hidden map[int]string, shown map[int]bool) (Trim, []string) {
	nums := LookedAt(looks)
	// Only the messages shown may be cited; a look past lookMax is cut to them.
	in := map[int]bool{}
	for _, k := range nums {
		in[k] = true
	}
	var ps []string
	var kept []Look
	for _, l := range looks {
		var keep []int
		for _, k := range l.Msgs {
			if in[k] {
				keep = append(keep, k)
			}
		}
		if len(keep) == 0 {
			// Its messages are past the second look's bound: it is not
			// removed as unsupported either.
			ps = append(ps, fmt.Sprintf("removed claim %q as unsupported, but the part of the conversation the digest left out holds its words (messages %s)", cut(t.Removed[l.Removed].Text, 80), joinInts(l.Msgs)))
			continue
		}
		l.Msgs = keep
		kept = append(kept, l)
	}
	looks = kept
	if len(looks) == 0 {
		return t, ps
	}
	lines := LookLines(msgs, hidden, looks, nums)
	red, err := rv.Redact(lines)
	if err != nil || len(red) != len(lines) {
		return t, append(ps, "the second look's messages could not be redacted: not sent")
	}
	res.Line.Looked = nums
	out, cost, _, err := rv.Run(ctx, SecondLookPrompt(save, t, looks, red))
	res.Line.CostUSD += cost
	if err != nil {
		return t, append(ps, "the second look failed: "+err.Error())
	}
	answers, err := ParseSecondLook(out)
	if err != nil {
		return t, append(ps, err.Error())
	}
	for _, k := range nums {
		shown[k] = true // a claim kept on the second look cites them (CheckTrim)
	}
	t, more := ApplySecondLook(t, looks, answers, shown)
	return t, append(ps, more...)
}
