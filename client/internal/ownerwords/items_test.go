package ownerwords

import "testing"

// The item of a turn that holds a quote: the question's words above a terse
// answer, never another item's, in the shapes a list of answers takes; no
// item where the turn does not say which question the quote answers.
func TestItemAround(t *testing.T) {
	const oneLine = "whether the zebra cron runs nightly; - it does, every night.  whether to keep both lantern files (only you may answer); - keep the machine one, the user copy is noise. which quokka queue stands; - the green one, check the unit"
	const lines = "whether the zebra cron runs nightly\n- it does, every night at two\nwhich lantern file stands\n- keep the machine one\n"
	for _, c := range []struct{ name, turn, quote, want string }{
		{"the answer after a dash, under its question", oneLine, "keep the machine one, the user copy is noise",
			"  whether to keep both lantern files (only you may answer); - keep the machine one, the user copy is noise."},
		{"the question and its answer quoted whole", oneLine, "whether to keep both lantern files (only you may answer); - keep the machine one",
			"  whether to keep both lantern files (only you may answer); - keep the machine one, the user copy is noise."},
		{"the first item", oneLine, "it does, every night", "whether the zebra cron runs nightly; - it does, every night."},
		{"the last item", oneLine, "the green one, check the unit", " which quokka queue stands; - the green one, check the unit"},
		{"one item to a line", lines, "keep the machine one", "\nwhich lantern file stands\n- keep the machine one"},
		{"the line under the first question", lines, "it does, every night at two", "whether the zebra cron runs nightly\n- it does, every night at two"},
		{"a question in its own sentence, the answer after it", "retiring both lantern files (only you may answer this kind). - yes, retire them both", "yes, retire them both",
			"retiring both lantern files (only you may answer this kind). - yes, retire them both"},
		{"a dash between two words joins them", "retiring both lantern notes (only you may answer this kind). - yes - retire both of them", "yes - retire both of them",
			"retiring both lantern notes (only you may answer this kind). - yes - retire both of them"},
		{"a bulleted question keeps its answer", "- which lantern proxy file stands?\n- keep the machine one", "keep the machine one",
			"- which lantern proxy file stands?\n- keep the machine one"},
		{"an answer that goes on in a second sentence keeps it", "which lantern proxy port file stands?\n- keep the old one. the lantern proxy port still works.\nwhich zebra nightly cron stands?\n- retire both of them", "retire both of them",
			"\nwhich zebra nightly cron stands?\n- retire both of them"},
		{"a question after an answer on its line asks", "which zebra cron schedule holds? - keep the nightly one. which lantern proxy port holds? - keep the 7790 one, the unit runs it", "keep the 7790 one, the unit runs it",
			" which lantern proxy port holds? - keep the 7790 one, the unit runs it"},
		{"a question sentence after the answer before it, quoted with its answer", "which file stands; - keep it, check source.  retiring both lantern notes (only you may answer this kind). - yes", "retiring both lantern notes (only you may answer this kind). - yes",
			"  retiring both lantern notes (only you may answer this kind). - yes"},
		{"no dash: the sentence with the question right above it", "which zebra cron schedule holds? keep the nightly one, it runs at two\nwhich lantern proxy port holds? keep the 7790 one, the unit runs it", "keep the 7790 one, the unit runs it",
			"\nwhich lantern proxy port holds? keep the 7790 one, the unit runs it"},
		{"no dash: another sentence is not the quote's", "my quokka scheduler rule stands, the blue queue stays. lets ship the release notes for the parser today", "lets ship the release notes for the parser today",
			" lets ship the release notes for the parser today"},
		{"no dash, one sentence", "the lantern proxy moved, keep the machine one and drop the copy", "keep the machine one and drop", "the lantern proxy moved, keep the machine one and drop the copy"},
		{"a question's words alone: no item", "whether to retire both lantern files\n- no, keep the machine one", "whether to retire both lantern files", ""},
		{"a question's words before its own dash: no item", "whether to retire both lantern files; - no, keep the machine one", "whether to retire both lantern files", ""},
		{"from an answer into the next question: no item", lines, "it does, every night at two which lantern file stands", ""},
		{"answers split by semicolons only: no item", "which lantern proxy file stands; - keep the old one; which zebra nightly cron stands; - retire both of them", "retire both of them", ""},
		{"a quote across two items: no item", oneLine, "every night. whether to keep both lantern", ""},
		{"a dash inside a word is none", "the api-key notes for lantern are done, retire both of them", "retire both of them", "the api-key notes for lantern are done, retire both of them"},
		{"case, apostrophes and punctuation as Find reads them", "Don't keep the USER copy; it's noise", "dont keep the user copy its noise", "Don't keep the USER copy; it's noise"},
		{"not in the turn", oneLine, "retire both of the lantern files", ""},
		// The line above an answer line is its question, and only that line.
		{"an answer line answers the line above, not the line before that", "which zebra nightly cron stands?\n- retire it\nthe lantern proxy port file is noise too\nwhich quokka queue stands?\n- keep the green one please", "keep the green one please",
			"\nwhich quokka queue stands?\n- keep the green one please"},
		{"an answer that asks back answers nothing for the next one", "which zebra nightly cron stands?\n- retire both, right?\nwhich lantern proxy file stands?\n- keep the machine one", "keep the machine one",
			"\nwhich lantern proxy file stands?\n- keep the machine one"},
		{"the question after the line's own answer", "which zebra cron stands? - keep the nightly one. which lantern proxy holds? the old docs say 7790\n- keep the 7790 one please", "keep the 7790 one please",
			" which lantern proxy holds? the old docs say 7790\n- keep the 7790 one please"},
		{"a blank line between", "which lantern proxy port file stands?\n\n- keep the machine one", "keep the machine one", "which lantern proxy port file stands?\n\n- keep the machine one"},
		{"a no-break space before the bullet", "which lantern proxy file stands?\n\u00a0- keep the machine one", "keep the machine one", "which lantern proxy file stands?\n\u00a0- keep the machine one"},
		{"line ends with CRLF", "which lantern proxy file stands?\r\n- keep the machine one\r\nwhich zebra cron stands?\r\n- retire it now please\r\n", "retire it now please", "\nwhich zebra cron stands?\r\n- retire it now please\r"},
		{"a quote that starts in an answer's going on starts at the answer", "which lantern proxy port file stands?\n- yes. retire both of them", "retire both of them", "which lantern proxy port file stands?\n- yes. retire both of them"},
		{"from an answer's going on into the next item: no item", "which zebra nightly cron stands?\n- retire it. the lantern proxy file is noise.\nwhich quokka queue stands?\n- keep the green one please", "the lantern proxy file is noise. which quokka queue stands? - keep the green one please", ""},
		// With no answer dash, a question line above is the answer's question.
		{"no dash: the line above asks", "which lantern proxy port file stands? \nkeep the machine one", "keep the machine one", "which lantern proxy port file stands? \nkeep the machine one"},
		{"no dash: a pasted question of two sentences", "Two files hold the name lantern-proxy-port: user/lantern-proxy-port.md and machines/lantern-proxy-port.md. Which one stands?\nkeep the machine one", "keep the machine one",
			"Two files hold the name lantern-proxy-port: user/lantern-proxy-port.md and machines/lantern-proxy-port.md. Which one stands?\nkeep the machine one"},
		{"no dash: a line above that does not ask is its own", "keep the nightly one, it runs at two\nkeep the 7790 one, the unit runs it", "keep the 7790 one, the unit runs it", "\nkeep the 7790 one, the unit runs it"},
		{"no dash: only the line right above", "which lantern proxy port stands?\nwhy do we even have two?\nwhich zebra nightly cron stands?\nkeep the later one, it runs at three", "keep the later one, it runs at three",
			"\nwhich zebra nightly cron stands?\nkeep the later one, it runs at three"},
		{"no dash: from an answer into the next question: no item", "which zebra cron schedule holds? retire the zebra cron entirely. which lantern proxy port file stands? keep the machine one", "the zebra cron entirely. which lantern proxy port file", ""},
		// Known limits: these fail closed.
		{"limit: a dash after a closing bracket joining clauses", "which lantern file stands?\n- keep the machine one (host-a) - the user copy is noise\nwhich zebra cron stands?\n- retire it now please", "keep the machine one (host-a) - the user copy is noise", ""},
		{"limit: a plain answer in a turn with dashed ones", "keep the machine lantern proxy file, the user copy is noise.\n- also fix the zebra cron", "keep the machine lantern proxy file", ""},
	} {
		if got := ItemAround(c.turn, c.quote); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}
