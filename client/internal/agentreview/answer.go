package agentreview

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/ownerwords"
)

// Answer kinds: the owner's yes, no, or their correction in their words.
const (
	AnswerYes     = "yes"
	AnswerNo      = "no"
	AnswerCorrect = "correct"
)

// Answer is the owner's answer to a question, read from their own turn.
type Answer struct {
	Kind     string `json:"kind"`
	Words    string `json:"words,omitempty"` // a correction, or why not
	From     string `json:"from"`            // <harness>:<session>, or rulebook:<session>
	TurnHash string `json:"turn_hash,omitempty"`
	At       string `json:"at"`
}

var (
	answerTag  = regexp.MustCompile(`(?i)^\s*\[?(tacit\s+)?q-[0-9a-f]{6}\]?\s*[:,.-]?\s*`)
	answerLead = regexp.MustCompile(`^[\s"'“”‘’*>_~-]+`)
	correctRx  = regexp.MustCompile(`(?is)^(correct it|correction|fix it)\b\s*[:,;.-]?\s*(.*)$`)
	noCorrect  = regexp.MustCompile(`(?is)^(no|nope|wrong|not quite)\s*[,.;:!-]*\s+((it'?s|it is|its|actually|rather|instead|it should be|should be)\b.+)$`)
	yesRx      = regexp.MustCompile(`(?is)^(yes|yeah|yep|yup|yea|y|right|correct|true|confirmed|exactly|that'?s right)\b\s*[,.;:!-]*\s*(.*)$`)
	yesBut     = regexp.MustCompile(`(?is)^(but|except|only|although|though)\b`)
	noRx       = regexp.MustCompile(`(?is)^(no|nope|wrong|false|incorrect|not right|not true|thats wrong|that'?s wrong)\b\s*[,.;:!-]*\s*(.*)$`)
)

// ParseAnswer reads the owner's turn as an answer to a question: "yes"
// (right, correct, ...), "no" with an optional reason, or a correction in
// their words ("correct it: ...", "no, it's ...", "yes but ..."). Only the
// turn's first paragraph is read; ok is false for a turn that is no answer
// (they went on with something else).
func ParseAnswer(turn string) (kind, words string, ok bool) {
	t := strings.TrimSpace(turn)
	if i := strings.Index(t, "\n\n"); i >= 0 {
		t = strings.TrimSpace(t[:i])
	}
	t = answerTag.ReplaceAllString(t, "")
	t = answerLead.ReplaceAllString(t, "")
	if t == "" {
		return "", "", false
	}
	if m := correctRx.FindStringSubmatch(t); m != nil {
		if w := clean(m[2]); w != "" {
			return AnswerCorrect, w, true
		}
		return "", "", false
	}
	if m := noCorrect.FindStringSubmatch(t); m != nil {
		return AnswerCorrect, clean(m[2]), true
	}
	if m := yesRx.FindStringSubmatch(t); m != nil {
		rest := clean(m[2])
		if yesBut.MatchString(rest) {
			return AnswerCorrect, clean(t), true
		}
		return AnswerYes, "", true
	}
	if m := noRx.FindStringSubmatch(t); m != nil {
		return AnswerNo, clean(m[2]), true
	}
	return "", "", false
}

func clean(s string) string {
	return strings.TrimSpace(strings.Join(strings.Fields(s), " "))
}

// FindAnswer looks in msgs, from start on, for the owner's answer to the
// question tagged tag: the first agent message that holds the tag (the agent
// asked it), then the first message the owner typed after it (OwnerTurn).
// What another agent, a hook or a note says in between is never theirs: an
// agent relaying "the owner says yes" is no answer. asked reports that the
// agent asked; turn is the owner's turn read, "" when none came yet; ok that
// it is an answer. A turn that is no answer means the owner passed over the
// question.
func FindAnswer(msgs []Msg, start int, tag string) (asked bool, turn, kind, words string, ok bool) {
	if start < 0 {
		start = 0
	}
	for i := start; i < len(msgs); i++ {
		m := msgs[i]
		if !asked {
			if m.Role == "assistant" && strings.Contains(m.Text, tag) {
				asked = true
			}
			continue
		}
		if m.Role != "user" || !m.Owner {
			continue
		}
		kind, words, ok = ParseAnswer(m.Text)
		return asked, m.Text, kind, words, ok
	}
	return asked, "", "", "", false
}

// ReadAnswers reads the owner's answers to the questions put in sessions,
// each from its session after the question was asked (Ask.Offset): the
// agent's message that asked it (its tag), then the owner's own next turn
// (FindAnswer). A turn of theirs that answers something else, or one that
// came with no question asked before it, passes the ask over. only, when
// set, limits the reading to some asks (the prompt hook reads its own
// session's). It reports a change.
func ReadAnswers(ctx context.Context, q *Queue, src Sources, only func(Ask) bool, now time.Time) bool {
	changed := false
	for _, w := range q.Waiting() {
		it, a := &q.Items[w[0]], &q.Items[w[0]].Asks[w[1]]
		if it.State != StateOpen || only != nil && !only(*a) {
			continue
		}
		var conv Conversation
		var err error
		if a.Harness == "hermes" {
			if src.HermesDB == "" {
				continue
			}
			conv, err = ReadHermesAfter(ctx, src.HermesDB, a.Session, a.Offset)
		} else {
			path, f, ok := a.Transcript, digest.Format(0), false
			switch a.Harness {
			case "claude":
				f, ok = digest.Claude, path != ""
			case "codex":
				f, ok = digest.Codex, path != ""
			case "pi":
				f, ok = digest.Pi, path != ""
			}
			if !ok {
				if path, f, ok = src.Locate(a.Harness, a.Session); !ok {
					continue
				}
			}
			conv, err = ReadTranscriptFrom(path, f, a.Offset)
		}
		if err != nil {
			continue
		}
		asked, turn, kind, words, ok := FindAnswer(conv.Msgs, 0, it.Tag())
		if !asked {
			// The agent did not ask it before the owner's next turn: passed.
			for _, m := range conv.Msgs {
				if m.Role == "user" && m.Owner {
					a.Passed, changed = true, true
					break
				}
			}
			continue
		}
		if turn == "" {
			continue // their turn is still to come
		}
		if !ok {
			a.Passed, changed = true, true
			continue
		}
		it.Answer = &Answer{Kind: kind, Words: words, From: a.Harness + ":" + a.Session, TurnHash: ledger.TurnHash(ownerwords.Normalize(turn)), At: now.UTC().Format(time.RFC3339)}
		it.State = StateAnswered
		changed = true
	}
	return changed
}

// TakeRulebookAnswers takes the owner's answers given in the rulebook pane
// (RulebookAnswers) for the open questions. It reports a change.
func TakeRulebookAnswers(q *Queue, answers map[string]Answer) bool {
	changed := false
	for i := range q.Items {
		it := &q.Items[i]
		if a, ok := answers[it.ID]; ok && it.State == StateOpen {
			a := a
			it.Answer, it.State = &a, StateAnswered
			changed = true
		}
	}
	return changed
}
