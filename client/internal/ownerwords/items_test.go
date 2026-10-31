package ownerwords

import "testing"

// The item of a turn that holds a quote: the question's words above a terse
// answer, never another item's, in the shapes a list of answers takes; no
// item where the turn does not say where one ends.
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
		{"a bulleted question keeps its answer", "- which lantern proxy file stands?\n- keep the machine one", "keep the machine one",
			"- which lantern proxy file stands?\n- keep the machine one"},
		{"an answer that goes on in a second sentence keeps it", "which lantern proxy port file stands?\n- keep the old one. the lantern proxy port still works.\nwhich zebra nightly cron stands?\n- retire both of them", "retire both of them",
			"\nwhich zebra nightly cron stands?\n- retire both of them"},
		{"a question sentence after the answer before it, quoted with its answer", "which file stands; - keep it, check source.  retiring both lantern notes (only you may answer this kind). - yes", "retiring both lantern notes (only you may answer this kind). - yes",
			"  retiring both lantern notes (only you may answer this kind). - yes"},
		{"answers split by semicolons only: no item", "which lantern proxy file stands; - keep the old one; which zebra nightly cron stands; - retire both of them", "retire both of them", ""},
		{"a quote across two items: no item", oneLine, "every night. whether to keep both lantern", ""},
		{"no dash: the turn whole", "the lantern proxy moved, keep the machine one and drop the copy", "keep the machine one and drop", "the lantern proxy moved, keep the machine one and drop the copy"},
		{"a dash inside a word is none", "the key-env notes for tower are done, retire both of them", "retire both of them", "the key-env notes for tower are done, retire both of them"},
		{"case, apostrophes and punctuation as Find reads them", "Don't keep the USER copy; it's noise", "dont keep the user copy its noise", "Don't keep the USER copy; it's noise"},
		{"not in the turn", oneLine, "retire both of the lantern files", ""},
	} {
		if got := ItemAround(c.turn, c.quote); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}
