package ownerwords

import "testing"

func TestOneTime(t *testing.T) {
	for _, c := range []struct {
		quote string
		want  Shape
	}{
		// A go-ahead, with or without the end of the task.
		{"go on, publish it once done", OneTimeClear},
		{"both can go", OneTimeClear},
		{"merge it, then ship", OneTimeClear},
		{"yes do it", OneTimeClear},
		// A list of chores with a reminder for the moment.
		{"send the invoice, already called the bank. remind me at 5 to renew the domain. no need to reply to the newsletter", OneTimeClear},
		// A list of chores alone reads like standing imperatives: an
		// instruction of the task at most.
		{"archive the older clone, delete the backup copy", OneTimeTask},
		{"write new tools in go, run gofmt on them", OneTimeTask},
		{"check the load, check free memory first", OneTimeTask},
		// Words tied to the moment.
		{"have the draft ready by today at 6", OneTimeClear},
		// A condition stands, even with a day in it.
		{"remind me every morning to check the queue", NotOneTime},
		{"remind me each morning to check the queue", NotOneTime},
		{"remind me daily to stretch", NotOneTime},
		// An instruction inside the task: one-time, but it may hold later.
		{"first we fix the parser then we try the new lexer", OneTimeTask},
		{"you can close the stale ones", OneTimeTask},
		// Standing rules.
		{"never push to main without a review", NotOneTime},
		{"always run the tests before any merge", NotOneTime},
		{"continue only when the tests pass", NotOneTime},
		{"release on each merge if the changelog is ready", NotOneTime},
		{"we ship only the small plan for now, the goal is the team plan", NotOneTime},
		// "today" alone means nowadays; a remark about the past is no task.
		{"that color today reads as an error, so avoid it in buttons", NotOneTime},
		{"the cooling was off at this time", NotOneTime},
	} {
		if got := OneTime(c.quote); got != c.want {
			t.Errorf("OneTime(%q) = %d, want %d", c.quote, got, c.want)
		}
	}
}

func TestMintsRuling(t *testing.T) {
	if MintsRuling("go on, publish it once done", "projects/storefront") {
		t.Error("a go-ahead minted in a project")
	}
	if MintsRuling("you can close the stale ones", "user") {
		t.Error("an instruction of the task minted for every session")
	}
	if !MintsRuling("you can close the stale ones", "projects/storefront") {
		t.Error("an instruction of the task in its project is still a ruling there")
	}
	if !MintsRuling("never push to main without a review", "user") {
		t.Error("a standing rule refused")
	}
}
