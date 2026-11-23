package ownerwords

import (
	"regexp"
	"strings"
)

// Shape is how one-time words of theirs read: no one-time shape, an
// instruction inside the task at hand (InTask: it may still hold later), or
// words that hold for the moment only (OneTimeClear).
type Shape int

const (
	NotOneTime Shape = iota
	// OneTimeTask is InTask's shape, or a list of chores ("archive the older
	// clone, delete the backup copy"), and nothing stronger: an instruction
	// of the task at hand, which may be meant to hold later too.
	OneTimeTask
	// OneTimeClear is an approval to go on ("go on, publish it once done",
	// "both can go") or words tied to the moment ("remind me", "tonight",
	// "by today").
	OneTimeClear
)

// OneTime says whether the owner's words are a one-time instruction rather
// than a standing rule, and how sure code is (SPEC.md, Rulings, "One-time
// words"). One-time words are never minted as a ruling, a preference or a
// decision: they stay what the owner said in that session.
//
// Words that generalize (if, every, always, never, dont, before, after, ...:
// generalWords) are never one-time, with one exception: "when done" and its
// forms ("once done", "when finished", "when youre done") end a task rather
// than state a condition.
func OneTime(quote string) Shape {
	norm := doneRe.ReplaceAllString(" "+Normalize(quote)+" ", " ")
	ws := strings.Fields(norm)
	for len(ws) > 0 && inTaskLead[ws[0]] {
		ws = ws[1:]
	}
	if len(ws) == 0 {
		return NotOneTime
	}
	for i, w := range ws {
		if generalWords[w] || frequencyWords[w] || (w == "not" && i > 0 && ws[i-1] == "do") {
			return NotOneTime
		}
	}
	joined := " " + strings.Join(ws, " ") + " "
	for _, p := range momentPhrases {
		if strings.Contains(joined, " "+p+" ") {
			return OneTimeClear
		}
	}
	if approval(ws) {
		return OneTimeClear
	}
	// A list of chores reads like a list of standing imperatives ("write new
	// tools in go, run gofmt on them"), so it is no more than an instruction
	// of the task: asked about in the user scope, kept in a project.
	if choreList(quote) || InTask(quote) {
		return OneTimeTask
	}
	return NotOneTime
}

// doneRe is the end of the task ("when done", "once youre done"), read out
// before the generalizing words are looked for.
var doneRe = regexp.MustCompile(` (when|once) (you are |youre |its |it is |we are |were |all is |all are )?(done|finished|ready|complete) `)

// momentPhrases tie words to the moment they were said.
// "today" alone is not one: "that color today reads as an error" means
// nowadays; "this time" is often a past one ("the cooling was off at this
// time"). "until today" never gets here ("until" generalizes).
var momentPhrases = []string{"remind me", "today at", "by today", "untill today", "tonight", "tomorrow", "at noon", "this morning", "this evening", "this afternoon", "for this session", "for this task"}

// frequencyWords make words that would be tied to the moment a standing
// rule: "remind me each morning", "run it daily".
var frequencyWords = wordSet("each daily weekly nightly monthly hourly usually regularly")

// approval is a short go-ahead: every word an approval, a filler or a
// pointer to the thing at hand ("continue", "go ahead", "yes do it", "all
// can be removed", "ship it", "release it").
func approval(ws []string) bool {
	if len(ws) > 6 {
		return false
	}
	verb := false
	for _, w := range ws {
		switch {
		case approvalWords[w]:
			verb = true
		case deictic[w], fillers[w]:
		default:
			return false
		}
	}
	return verb
}

var (
	approvalWords = wordSet("do continue proceed go ahead approved approve yes yeah yep ok okay sure ship release merge push publish land finish keep going removed remove deleted delete dropped drop done fine good")
	deictic       = wordSet("it this that these those them all both everything rest")
	fillers       = wordSet("can be you we and then the to just so please now on with up")
)

// choreList is two or more sentences or clauses each a chore: it opens
// with an imperative of a chore ("drop", "remind", "pay", "run", "answer")
// or says one is not needed or already done ("no need to answer", "already
// answered").
func choreList(quote string) bool {
	n := 0
	for _, c := range clauseSplit.Split(strings.ToLower(quote), -1) {
		ws := strings.Fields(Normalize(c))
		for len(ws) > 0 && inTaskLead[ws[0]] {
			ws = ws[1:]
		}
		if len(ws) == 0 {
			continue
		}
		switch {
		case choreVerbs[ws[0]], ws[0] == "already", len(ws) > 2 && ws[0] == "no" && ws[1] == "need":
			n++
		default:
			return false
		}
	}
	return n >= 2
}

var (
	clauseSplit = regexp.MustCompile(`[.;!\n]+|,\s+`)
	choreVerbs  = wordSet("drop remind pay answer reply run take send call book order buy check remove delete add update fix push merge stop start move close open cancel renew schedule write email text post ping ask tell install upgrade restart rerun retry clean archive release publish finish continue leave kill skip ignore")
)

// MintsRuling reports whether the owner's words may be minted as a ruling,
// a preference or a decision in scope. Words that hold for the moment only
// (OneTimeClear) never are; an instruction inside the task at hand
// (OneTimeTask) is not in the user or system scope, which every session
// reads, where it would turn one session's instruction into a standing
// rule for all of them. In a project, machine or other folder it stays as
// before (InTask: still a ruling, delivered as an ordinary fact).
func MintsRuling(quote, scope string) bool {
	switch OneTime(quote) {
	case OneTimeClear:
		return false
	case OneTimeTask:
		return scope != "user" && scope != "system"
	}
	return true
}
