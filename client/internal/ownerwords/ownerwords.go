// Package ownerwords decides whether words in a fact are the owner's own. A
// fact is an owner ruling only when it carries theirs verbatim words, and this
// package is the one place that says what "verbatim" means: a quoted phrase
// in the fact that is found, after normalizing case, whitespace and
// punctuation, in one of their own turns in their transcripts, or in a note the owner
// wrote themself. Everything an agent wrote (a subagent prompt, a headless
// run, a compaction summary, a harness notice) is not their turn.
package ownerwords

import (
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"unicode"
)

// Normalize lowers case, drops apostrophes (the owner writes "dont", an agent quoting
// them writes "don't"), turns every other character that is not a letter or a
// digit into a space, and collapses spaces. Two texts that differ only in
// case, whitespace or punctuation normalize to the same string.
func Normalize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := true
	for _, r := range s {
		switch {
		case r == '\'' || r == '’' || r == '‘' || r == '`':
			continue
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(unicode.ToLower(r))
			space = false
		default:
			if !space {
				b.WriteByte(' ')
				space = true
			}
		}
	}
	return strings.TrimRight(b.String(), " ")
}

// Minimum size of a quote that counts as evidence. A two-word phrase ("do
// not", "dead code") is found in some turn of theirs whatever the fact says; a
// phrase of four words, or three long ones, is their sentence.
const (
	minWords = 3
	minChars = 18
)

// segments splits a quote at ellipses ("...", "…", "[...]"): an agent that
// shortened their sentence keeps every part verbatim and in order.
var ellipsis = regexp.MustCompile(`\[?(\.\.\.|…)\]?`)

// Segments are the normalized parts of a quote, in order; empty when the quote
// is too short to be evidence.
func Segments(quote string) []string {
	var segs []string
	words, chars := 0, 0
	for _, p := range ellipsis.Split(quote, -1) {
		n := Normalize(p)
		if n == "" {
			continue
		}
		segs = append(segs, n)
		words += len(strings.Fields(n))
		chars += len(n)
	}
	if words < minWords || chars < minChars {
		return nil
	}
	for _, s := range segs {
		if len(segs) > 1 && len(strings.Fields(s)) < 2 {
			return nil // a one-word piece between ellipses proves nothing
		}
	}
	return segs
}

// Usable reports whether a quote is long enough to be evidence that a phrase
// is theirs.
func Usable(quote string) bool { return Segments(quote) != nil }

// ProofWords is how many words of theirs a quote needs to prove a ruling by
// itself. A shorter phrase ("what would you build", a session's name) is theirs
// but may not be the rule the fact states: the fact carries it as their words
// (source.owner_words), evidence for the verifier, not a ruling.
const ProofWords = 6

// ProofStems is how many content stems (ContentStems) a quote of ProofWords
// or more must share with what the fact says it rules (its description and
// its name) to prove that rule. Six words of theirs with none of the rule's
// words in them are their words in the fact, not that rule.
//
// The gate was chosen on the author's store (2026-10-11): of the 31 facts the
// plain six-word rule kept and a lexical gate questions, 29 are their rulings
// an agent paraphrased and 2 are agent lessons (a dashboard checklist under a
// request of theirs, a removal checklist under a principle of theirs). One shared
// stem sends 7 of the 29 to observed with their words recorded: their quotes
// share no word with the rule as the agent wrote it, so no overlap gate keeps
// them while keeping the removal checklist off. A
// share of the description's stems adds nothing here: any share over zero
// is one stem.
const ProofStems = 1

// ProvesRuling reports a quote that proves the fact (its description and its
// name) is their ruling: it is not a request (Request), and it holds three
// fifths of the description's content stems, or it has six words or more and
// shares ProofStems content stems with the description and the name.
func ProvesRuling(quote, description, name string) bool {
	segs := Segments(quote)
	if segs == nil || Request(quote) {
		return false
	}
	n := 0
	for _, s := range segs {
		n += len(strings.Fields(s))
	}
	if shared, total := SharedStems(quote, description), len(ContentStems(description)); total > 0 && shared*5 >= total*3 {
		return true
	}
	return n >= ProofWords && SharedStems(quote, description+" "+name) >= ProofStems
}

// Request reports a quote that asks an agent for something ("i need the board
// to show disk i/o", "can you add a filter", "please show me the list") rather
// than stating a rule: it opens with "can/could/would you", "please", "show
// me" or "give me", or with "i need/want" and a thing to be done ("i need
// the X to ..."), and holds no rule word (never, always, dont, only, must,
// should, every) and no condition (not, no, before, after, until, unless,
// any, instead, without, when, if: "i need the tests to pass before any
// merge" is a standing condition). Their rules are statements; a request of theirs under an
// agent's lesson is their words in the fact, not that lesson's rule. The check
// is kept narrow on purpose: on the author's store (2026-10-11) it moves only
// the dashboard checklist above, and none of their rulings.
func Request(quote string) bool {
	ws := strings.Fields(Normalize(quote))
	for len(ws) > 0 && (ws[0] == "so" || ws[0] == "and" || ws[0] == "ok") {
		ws = ws[1:]
	}
	if len(ws) < 3 {
		return false
	}
	for _, w := range ws {
		if ruleWords[w] {
			return false
		}
	}
	switch {
	case (ws[0] == "can" || ws[0] == "could" || ws[0] == "would") && ws[1] == "you",
		ws[0] == "please",
		(ws[0] == "show" || ws[0] == "give") && ws[1] == "me":
		return true
	case ws[0] == "i" && (ws[1] == "need" || ws[1] == "want") && requestObject[ws[2]]:
		for _, w := range ws[3:min(len(ws), 7)] {
			if w == "to" || w == "that" {
				return true
			}
		}
	}
	return false
}

// InTask reports a quote of theirs that is an instruction inside the task at
// hand ("lets create the bucket as a draft", "set the cli to run on the
// staging region", "1. drop it 3. fix it") rather than a rule for later
// sessions: it opens with a task verb or a reply (yes, ok, lets, first,
// now, check, fix, you can, ...; a leading so, and or plus is skipped), is a
// numbered answer, holds a question mark or says "im", and holds no word that
// generalizes (if, when, before, after, any, every, always, never, dont,
// instead, prefer, ...: "now run the tests before any push" is a standing
// condition). It is still their
// ruling; delivery ranks it as an ordinary fact (SPEC.md, Delivery).
//
// The lists were derived on the dev half of a judge's labels of 283 kept
// rulings on the author's store (2026-10-19): on the test half it flags 25
// of 89 one-off instructions and 2 of 51 standing rules.
func InTask(quote string) bool {
	ws := strings.Fields(Normalize(quote))
	for len(ws) > 0 && inTaskLead[ws[0]] {
		ws = ws[1:]
	}
	if len(ws) == 0 {
		return false
	}
	digits := 0
	for i, w := range ws {
		// "do not" is "dont" spelled out.
		if generalWords[w] || (w == "not" && i > 0 && ws[i-1] == "do") {
			return false
		}
		if len(w) == 1 && w[0] >= '0' && w[0] <= '9' {
			digits++
		}
	}
	switch {
	case numberedStart.MatchString(quote), len(numberedItem.FindAllString(quote, -1)) >= 2, digits >= 2, strings.Contains(quote, "?"), slices.Contains(ws, "im"):
		return true
	case taskOpeners[ws[0]]:
		return true
	case len(ws) > 1 && taskOpeners2[ws[0]+" "+ws[1]]:
		return true
	}
	return false
}

var (
	// numberedItem is an answer by number: "1. keep it", "2) drop", "3- fix".
	// One counts at the start of the quote; elsewhere it takes two, since a
	// sentence may end in a number ("keep the cap at 50. it is the limit").
	numberedItem  = regexp.MustCompile(`(^|\s)\d+[.)-]\s`)
	numberedStart = regexp.MustCompile(`^\s*\d+[.)-]\s`)
	// generalWords make a quote hold beyond the task: a condition, a
	// quantifier, a rule word or a preference.
	generalWords = wordSet("if when whenever before after unless until any every some something anything problem way general always never dont ever instead rather better prefer")
	// taskOpeners open an instruction or a reply inside the task.
	taskOpeners  = wordSet("yes yea yeah ok okay lets first now do check open update remove try use go continue run fix push merge add make create set change clean empty review scan solve")
	taskOpeners2 = map[string]bool{"you can": true, "i approve": true}
	inTaskLead   = wordSet("so and plus")
)

func wordSet(s string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(s) {
		m[w] = true
	}
	return m
}

var (
	// ruleWords make a sentence a standing rule, not a request.
	// Condition words ("before any merge", "on the workstation not on the box") make
	// it a standing condition on how work is done, not a task.
	ruleWords = map[string]bool{"never": true, "always": true, "dont": true, "only": true, "must": true, "should": true, "every": true, "cant": true, "shouldnt": true,
		"not": true, "no": true, "before": true, "after": true, "until": true, "unless": true, "any": true, "instead": true, "without": true, "when": true, "whenever": true, "if": true}
	// requestObject opens the thing the owner asks for: "i need the crm to ...".
	requestObject = map[string]bool{"the": true, "a": true, "an": true, "my": true, "our": true, "this": true, "that": true, "some": true, "it": true}
)

// SharedStems is how many content stems quote and text share.
func SharedStems(quote, text string) int {
	ts := map[string]bool{}
	for _, s := range ContentStems(text) {
		ts[s] = true
	}
	n := 0
	for _, s := range ContentStems(quote) {
		if ts[s] {
			n++
		}
	}
	return n
}

// stemLen is the prefix a content word is cut to: a typed "piecess" and an
// agent's "pieces", a typed "messurment" and "measured" do not meet, but
// "review" and "reviews", "functionalities" and "functionality" do.
const stemLen = 4

// ContentStems are the content words of text cut to stemLen letters, each
// once; stopwords and words under three letters are left out.
func ContentStems(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, w := range strings.Fields(Normalize(text)) {
		if len(w) < 3 || stopwords[w] {
			continue
		}
		if r := []rune(w); len(r) > stemLen {
			w = string(r[:stemLen])
		}
		if !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	return out
}

// stopwords carry no subject: function words, and the words every ruling
// is written with ("owner", "rule", "always").
var stopwords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`the and are for not but with this that these those then than its was were has have had
		been being from into onto over under about after before when what which who whom whose why how where there here
		you your yours our ours his him her hers she they them their theirs its out off all any each every some such
		can could will would shall should must may might does did doing dont doesnt didnt isnt arent wasnt cant wont
		also just only very too more most less much many own same other another one two yes nope via per
		owner owners ruling rulings rule rules said says say always never ever need needs needed want wants
		lets let get got use used using make makes made thing things way ok okay please now`) {
		stopwords[w] = true
	}
	for _, w := range OwnerNames() {
		stopwords[w] = true
	}
}

var ownerNameRe = regexp.MustCompile(`^[a-z0-9]+$`)

// OwnerNames are the words besides "owner" that agents use for the owner
// in a store's facts ("Dana 2026-09-05: ..."): $TACIT_OWNER_NAMES, comma or
// space separated, lower case, letters and digits only.
func OwnerNames() []string {
	var out []string
	for _, w := range strings.FieldsFunc(strings.ToLower(os.Getenv("TACIT_OWNER_NAMES")), func(r rune) bool { return r == ',' || r == ' ' }) {
		if ownerNameRe.MatchString(w) {
			out = append(out, w)
		}
	}
	return out
}

// In reports whether quote occurs in text, normalized, on word boundaries,
// every ellipsis-separated part in order.
func In(text, quote string) bool {
	segs := Segments(quote)
	if segs == nil {
		return false
	}
	return segsIn(" "+Normalize(text)+" ", segs)
}

func segsIn(padded string, segs []string) bool {
	at := 0
	for _, s := range segs {
		i := strings.Index(padded[at:], " "+s+" ")
		if i < 0 {
			return false
		}
		at += i + len(s) + 1
	}
	return true
}

// codeQuote reports a quoted command, path, flag or agent markdown rather
// than a sentence of theirs. Their sentences carry a flag or a path now and then
// ("if needed you can use --admin", "org/* included"), so a phrase is
// judged as a whole. It is code when:
//   - it holds a backtick or "**" (the owner types in a terminal; markdown is an
//     agent's), or an environment assignment (KEY=value);
//   - two code-like words sit next to each other ("--recheck --push");
//   - it opens with a command ("gh pr merge ...", "run tacit ..."), or a
//     command is followed within three words by a code-like word ("git diff
//     --check"), and it holds a code-like word;
//   - a shell loop or operator runs through it ("; do", "|", "&&", "#"):
//     every word after the operator is code;
//   - or it has fewer than ProofWords plain words, or fewer than two plain
//     words for each code-like one.
func codeQuote(q string) bool {
	if markdownCode.MatchString(q) || envAssign.MatchString(q) {
		return true
	}
	toks := strings.Fields(q)
	isCode := make([]bool, len(toks))
	any := false
	for i, w := range toks {
		if Normalize(w) != "" && codeLike.MatchString(w) {
			isCode[i], any = true, true
		}
		if shellOps[w] || (strings.HasSuffix(w, ";") && i+1 < len(toks) && shellKeywords[toks[i+1]]) {
			any = true
		}
	}
	if !any {
		return false
	}
	// A command at the start, after a verb of theirs ("run", "use", "try").
	first := 0
	for first < len(toks) && leadVerbs[strings.ToLower(toks[first])] {
		first++
	}
	if first < len(toks) {
		w := strings.ToLower(strings.Trim(toks[first], ".,:;!?()"))
		if commandWords[w] || (w == "go" && first+1 < len(toks) && goSubcommands[toks[first+1]]) {
			return true
		}
	}
	// A sentence that opens as a rule of theirs ("dont use git push --force on
	// shared branches") is prose that names a command, not a command line.
	lead := strings.Fields(Normalize(strings.Join(toks[:min(2, len(toks))], " ")))
	ruleOpen := len(lead) > 0 && (lead[0] == "dont" || lead[0] == "never" || lead[0] == "always" || (len(lead) > 1 && lead[0] == "do" && lead[1] == "not"))
	code, plain, shell := 0, 0, false
	prevCode := false
	for i, w := range toks {
		if shellOps[w] || (strings.HasSuffix(w, ";") && i+1 < len(toks) && shellKeywords[toks[i+1]]) {
			shell = true
		}
		if Normalize(w) == "" {
			continue // punctuation alone ("--" as a dash, "/" between words)
		}
		if isCode[i] && prevCode {
			return true
		}
		prevCode = isCode[i]
		if !ruleOpen && commandWords[strings.ToLower(w)] && !ambiguousCommands[strings.ToLower(w)] {
			for k := i + 1; k < len(toks) && k <= i+3; k++ {
				if isCode[k] {
					return true
				}
			}
		}
		switch {
		case isCode[i] || shell:
			code++
		default:
			plain++
		}
	}
	return code > 0 && (plain < ProofWords || plain < 2*code)
}

var (
	markdownCode = regexp.MustCompile("`|\\*\\*")
	envAssign    = regexp.MustCompile(`(?:^|\s)[A-Za-z_][A-Za-z0-9_]*=\S`)
	// shellOps are shell operators standing as words; every word after one
	// is code.
	shellOps      = map[string]bool{"|": true, "||": true, "&&": true, "#": true, ">": true, ">>": true, "2>&1": true, "<": true}
	shellKeywords = map[string]bool{"do": true, "then": true, "done": true, "fi": true}
	leadVerbs     = map[string]bool{"run": true, "use": true, "try": true, "please": true, "then": true, "just": true, "and": true, "so": true, "ran": true}
	// commandWords open a command line. ambiguousCommands are also English
	// words: they count only at the start.
	commandWords      = map[string]bool{}
	ambiguousCommands = map[string]bool{}
	goSubcommands     = map[string]bool{"test": true, "build": true, "run": true, "vet": true, "mod": true, "install": true, "get": true, "generate": true, "work": true}
)

func init() {
	for _, w := range strings.Fields(`git gh cd export sudo nice cargo curl ssh scp rsync jq npm npx pnpm yarn bun node deno
		python python3 pip pip3 uv uvx docker podman kubectl helm rm cp mv ls cat grep rg sed awk find tacit claude codex
		systemctl journalctl bash sh zsh source chmod chown mkdir touch tar wget make cmake ninja nvcc ollama hf nvidia-smi
		tmux pkill pgrep nohup time watch kill env apt brew rustup rustc clippy gcloud aws`) {
		commandWords[w] = true
	}
	for _, w := range strings.Fields(`time watch kill cat source touch find make env tar hf node claude codex tacit aws`) {
		ambiguousCommands[w] = true
	}
}

var (
	// codeLike is a word of a command, path or flag: "--x", "-x", a path, an
	// assignment, a variable, a call, a "#tag", a shell operator.
	codeLike = regexp.MustCompile("--|/|=|\\$|`|\\.\\w+\\(|^-[A-Za-z]|^#[A-Za-z_]|&&|\\|")
	doubleQ  = regexp.MustCompile(`"([^"\n]+)"|“([^”\n]+)”|«([^»\n]+)»|„([^“”\n]+)[“”]`)
	// A single-quoted phrase only where a quote can start and end (an
	// apostrophe inside a word is not one).
	singleQ = regexp.MustCompile(`(?:^|[\s(:])'([^'\n]+)'(?:[\s).,;:!?]|$)`)
	blockQ  = regexp.MustCompile(`(?m)^\s*>\s?(.+)$`)
)

// Quotes lists the quoted phrases of text that are long enough to be
// evidence: double, typographic and guillemet quotes, single quotes at word
// edges, and Markdown blockquote lines. Longest first, each once.
func Quotes(text string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(q string) {
		q = strings.TrimSpace(q)
		if q == "" || !Usable(q) || codeQuote(q) {
			return
		}
		k := strings.Join(Segments(q), "\x00")
		if seen[k] {
			return
		}
		seen[k] = true
		out = append(out, q)
	}
	for _, m := range doubleQ.FindAllStringSubmatch(text, -1) {
		for _, g := range m[1:] {
			add(g)
		}
	}
	for _, m := range singleQ.FindAllStringSubmatch(text, -1) {
		add(m[1])
	}
	for _, m := range blockQ.FindAllStringSubmatch(text, -1) {
		add(m[1])
	}
	sort.SliceStable(out, func(i, j int) bool { return len(Normalize(out[i])) > len(Normalize(out[j])) })
	return out
}

// attributed is a clause an agent wrote down as the owner's without quote
// marks: "Owner 2026-09-05: list the gain of every change." The clause
// runs to the end of its sentence.
// The names come from $TACIT_OWNER_NAMES, read on each call so a test or a
// long-running process sees the current value; the compiled form is kept per
// value.
func attributed() *regexp.Regexp {
	key := os.Getenv("TACIT_OWNER_NAMES")
	attributedRe.Lock()
	defer attributedRe.Unlock()
	if attributedRe.re == nil || attributedRe.key != key {
		attributedRe.key = key
		attributedRe.re = regexp.MustCompile(`(?i)\b(?:` + strings.Join(append([]string{"owner"}, OwnerNames()...), "|") + `)\b[^:.\n"]{0,40}:\s*([^.;!?\n"“”]{12,240})`)
	}
	return attributedRe.re
}

var attributedRe struct {
	sync.Mutex
	key string
	re  *regexp.Regexp
}

// Attributed lists the clauses text attributes to the owner without quote
// marks, each long enough to be evidence and not a command (codeQuote); the
// audit checks them like quotes.
func Attributed(text string) []string {
	var out []string
	for _, m := range attributed().FindAllStringSubmatch(text, -1) {
		if q := strings.TrimSpace(m[1]); Usable(q) && !codeQuote(q) {
			out = append(out, q)
		}
	}
	return out
}

// BlockQuoted is the text of the Markdown blockquote lines of body: in an
// owner note, their words; the lines around them are the app's template.
func BlockQuoted(body string) string {
	var lines []string
	for _, m := range blockQ.FindAllStringSubmatch(body, -1) {
		lines = append(lines, m[1])
	}
	return strings.Join(lines, "\n")
}

// disown matches the owner saying that facts are not theirs: "not owner
// wording", "non owner rulings", "not mine", "interpretation of agents",
// "agents' statement". The spelling is kept loose (a doubled or dropped
// letter).
var disown = regexp.MustCompile(`(?i)\bno[nt][\s-]+owner\s+(wordings?|words?|rul+ings?|rules?)\b` +
	`|\bnot\s+my\s+(rul+ing|rule|words|wording)s?\b` +
	`|\binterp\w*tation\s+of\s+(the\s+)?agents?\b`)

// Disowns reports whether the owner's words say the facts they are about are
// not theirs: agents' statements, not their wording or rulings.
// A phrase that does not carry its own "not" ("interpretation of agents")
// counts only when no negation of theirs comes right before it in the same
// clause: "these are my words, not an interpretation of agents" claims them.
func Disowns(words string) bool {
	for _, m := range disown.FindAllStringIndex(words, -1) {
		phrase := strings.ToLower(words[m[0]:m[1]])
		if strings.HasPrefix(phrase, "no") {
			return true // "not owner wording", "non owner rulings", "not my rule"
		}
		clause := words[:m[0]]
		if i := strings.LastIndexAny(clause, ",.;:!?\n"); i >= 0 {
			clause = clause[i+1:]
		}
		prev := strings.Fields(Normalize(clause))
		if len(prev) > 3 {
			prev = prev[len(prev)-3:]
		}
		negated := false
		for _, w := range prev {
			if negationWords[w] {
				negated = true
			}
		}
		if !negated {
			return true
		}
	}
	return false
}
