package ownerwords

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/digest"
)

func TestNormalizeAndIn(t *testing.T) {
	if got := Normalize("  Don't   RESET: caches!  it’s team-related "); got != "dont reset caches its team related" {
		t.Fatalf("normalize: %q", got)
	}
	text := "Owner, 2026-09-21: never print a token's value, not even \"if you confirm\"."
	for q, want := range map[string]bool{
		"never print a tokens value":            true,
		"Never print a token's value, not even": true,
		"never print a token":                   false, // "token" is not the word "tokens"
		"ever print a tokens value":             false, // not on a word boundary
		"print a tokens":                        false, // too short to be evidence
		"never ... tokens value":                false, // a one-word piece
		"never print ... not even if you":       true,  // an ellipsis keeps every part in order
		"not even if you ... never print a":     false, // parts out of order
	} {
		if got := In(text, q); got != want {
			t.Errorf("In(%q) = %v, want %v", q, got, want)
		}
	}
}

func TestQuotes(t *testing.T) {
	text := "The owner said \"never rename public endpoints without asking\" and “dont reset the shared cache on friday”; it's 'agents stretch my notes' per them.\n> keep the owner note verbatim here\nAlso \"ok\" and \"too short\"."
	got := Quotes(text)
	want := []string{"never rename public endpoints without asking", "dont reset the shared cache on friday", "keep the owner note verbatim here", "agents stretch my notes"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("quotes:\n got %q\nwant %q", got, want)
	}
	att := Attributed("Release notes show the wins. Owner 2026-09-05: list the gain of every change, not the cost. Owner: ok.")
	if !reflect.DeepEqual(att, []string{"list the gain of every change, not the cost"}) {
		t.Fatalf("attributed: %q", att)
	}
	if q := Quotes("the owner ran \"git worktree remove --force today\" and \"cat ~/.config/x/settings.json now\""); len(q) != 0 {
		t.Fatalf("commands taken as quotes: %q", q)
	}
	// A sentence of theirs with a flag or a path in it is still their sentence.
	if q := Quotes("Owner: \"once the snapshot is verified you can prune the old ones. if needed you can pass --force.\" and \"i need the export to list the a/b results\""); !reflect.DeepEqual(q, []string{"once the snapshot is verified you can prune the old ones. if needed you can pass --force.", "i need the export to list the a/b results"}) {
		t.Fatalf("sentences with a flag or a path: %q", q)
	}
	// Commands, env lines, shell loops and agent markdown, each long enough to
	// pass a plain-word count (the review of the first cut).
	for _, c := range []string{
		"gh pr merge 42 --admin --squash",
		"gh pr merge 42 --squash --subject fix the flaky upload test in the ci job",
		"export TACIT_HOME=~/.cache/tacit # the store lives here on this machine now",
		"nice -n 19 go test ./... -run TestRulings -count 1 in the client dir",
		"for f in *.jsonl; do jq -r .sessionId $f; done",
		"set PATH=/usr/bin:$PATH in the env file on this box",
		"set PATH=/usr/bin:$PATH in the env file on this box today for every lane",
		"run tacit rulings audit --recheck --push on the live store now",
		"I also saw the new cli flag is missing from docs/usage.md, and git status --short is clean",
		"The main agent found a stale lock and kept a copy at `~/.cache/build.lock.old`, but nothing clears the lock on its own.",
		"**Pause the nightly jobs and drop the unused runners first**, then resize the pool for what is left",
		"cat logs.txt | grep error | sort | uniq -c and paste the counts in the ticket",
	} {
		if q := Quotes("The owner said \"" + c + "\"."); len(q) != 0 {
			t.Errorf("a command or agent markdown taken as a quote: %q", q)
		}
	}
	if att := Attributed("Owner 2026-10-10: run git branch -D on every merged branch now."); len(att) != 0 {
		t.Errorf("an attributed command: %q", att)
	}
	// Their sentences with one flag or path stay quotes.
	for _, c := range []string{
		"for staging hosts use --force when the snapshot passes and the disk is full",
		"put build output under ~/build and never in /tmp, it fills up",
		"dont run git clean -fdx in a shared checkout, ever, ask me first",
	} {
		if q := Quotes("The owner said \"" + c + "\"."); len(q) != 1 {
			t.Errorf("their sentence dropped as code: %q", c)
		}
	}
	if q := BlockQuoted("The owner wrote this in tacit app.\n\n> Not owner wording\n> second line\n"); q != "Not owner wording\nsecond line" {
		t.Fatalf("blockquoted: %q", q)
	}
}

// Six words of theirs prove a rule only when they are about it: they share two
// content stems with the fact's description and name, or cover enough of the
// description. A request of theirs under an agent's broader lesson does not.
func TestProofNeedsTheRulesWords(t *testing.T) {
	for _, c := range []struct {
		quote, desc, name string
		want              bool
	}{
		// Their request under an agent's lesson: a request, not a rule.
		{"i need the report to show churn rates", "Report trims must keep the weekly totals (signups, churn, revenue, open tickets) on the first page of the export.", "report-trims-keep-weekly-totals", false},
		// An agent's checklist under a principle of theirs: no shared stem.
		{"the codebase is not a museum", "Removing a CLI flag deletes its parser entry, help text, config key, env alias, docs page and test fixtures; the changelog gets one line under Removed.", "flag-removal-checklist", false},
		// A long quote proves nothing by length against a short description:
		// none of the rule's words.
		{"lets first clean up the build cache so the disk has room", "Removing a CLI flag deletes its parser entry, help text, config key, env alias, docs page and test fixtures; the changelog gets one line under Removed.", "flag-removal-checklist", false},
		{"the old export format can wait until the new one has users", "The dashboard shows the on-call summary one click from the home page.", "dashboard-summary", false},
		{"look at the login page on a phone and you will see why", "Release notes list every user-visible change, and a breaking change gets its own section before anything else.", "release-notes-list-changes", false},
		// The gate's known losses: rulings of theirs, paraphrased by an agent with
		// none of their words (observed, their words kept). Synthetic stand-ins of
		// the same shape as the losses measured on the author's store.
		{"speed is not the point when nobody is waiting on it.", "The full integration suite gates only tagged releases. Pull requests run lint, unit checks and a build, and nightly jobs catch the rest.", "full-suite-gates-releases-only", false},
		{"after the reply leaves us the clock is not ours to watch.", "Published response times are measured at the load balancer on the service's own health route, not from a laptop and not from browser timings.", "response-times-measured-at-the-balancer", false},
		{"most of those come in as favours, and every one of them lands on my desk later.", "Project READMEs get one neutral line on contributions that points to CONTRIBUTING.md; no badges, no calls to join, no extra sections.", "readme-contribution-line", false},
		{"neither path is good, the thing underneath is broken.", "When two workarounds cost different things for one root bug, do not offer the owner a pick between them; fix the bug and check both costs.", "fix-the-bug-not-a-workaround", false},
		{"we are adding tenants this month, so it has to stay up.", "On the shared worker pool the checkout service wins: a batch job that slows checkout is paused or moved, never checkout.", "checkout-over-batch-jobs", false},
		{"those were all decided without me", "Closing a project, cutting a release and publishing the docs are the owner's calls; agents do not do them alone, even with autonomy granted.", "closes-releases-publication-owner-calls", false},
		{"years for all i know", "The dependency audit that began 2026-09-29 closes only when every flagged package is upgraded or pinned with a reason, and the audit notes list each one.", "dependency-audit-closure", false},
		// One shared stem proves.
		{"nobody reads the footer on the download page anyway", "The download section lists the current version first and older builds below.", "download-current-first", true},
		{"send me the full log in one go, dont chop it into peices", "Logs go to the owner whole, in one message; never chopped into pieces or trimmed", "logs-sent-whole", true},
		{"a faster rival is good news, it gives us a number to beat.", "When another tool is faster on a fair benchmark, report it plainly and use it as the target; do not tune flags only to close that gap", "faster-tool-is-the-target", true},
		{"keep the hooks quiet", "Hooks print nothing unless a rule matches", "hooks-quiet", false},
		{"no release on docs", "No release on docs.", "no-release-on-docs", true}, // most of the description's words
	} {
		if got := ProvesRuling(c.quote, c.desc, c.name); got != c.want {
			t.Errorf("ProvesRuling(%q) = %v, want %v (shared stems %d)", c.quote, got, c.want, SharedStems(c.quote, c.desc+" "+c.name))
		}
	}
	// The boundary: the first "one shared stem" case shares exactly one.
	if n := SharedStems("nobody reads the footer on the download page anyway", "The download section lists the current version first and older builds below. download-current-first"); n != 1 {
		t.Errorf("the one-stem case shares %d stems", n)
	}
	if ProofStems != 1 {
		t.Errorf("the gate moved: %d stems; the losses above were measured at 1", ProofStems)
	}
}

// A request asks an agent for something; their rules are statements, also
// when they say "i need" or "you need to".
func TestRequestIsNotARule(t *testing.T) {
	for q, want := range map[string]bool{
		"i need the export to list the a/b results":               true,
		"can you add a filter for the region column":              true,
		"please show me the size of every bucket":                 false, // "every": a rule word
		"please make the report load faster":                      true,
		"show me the list of open tickets":                        true,
		"i need to check the docs on every release":               false,
		"I want to try to cut our image size.":                    false,
		"you need to answer the linter warnigns":                  false,
		"we dont need to rebuild the images on each commit":       false,
		"i need the jobs to always post the summary":              false,
		"i need the lint to pass before any tag is pushed":        false, // a standing condition
		"i need it to be tested on staging not on prod":           false,
		"every time you open a ticket add the repro steps to it.": false,
	} {
		if got := Request(q); got != want {
			t.Errorf("Request(%q) = %v, want %v", q, got, want)
		}
	}
}

// The owner's own words decide which notes disown their facts.
func TestDisowns(t *testing.T) {
	for _, s := range []string{
		"Not owner wording, an agent wrote this down on its own",
		"not owner rullings, just an interpertation of agents",
		"Non owner rules here, the agents made them.",
		"not my rule, an agent made it up",
		"the other one is an interpretation of agents",
	} {
		if !Disowns(s) {
			t.Errorf("does not disown: %q", s)
		}
	}
	for _, s := range []string{"Not relevant anymore, no inference", "both hold, b is the exception for x", "the owner decides", "not my problem today",
		"the left is the agents wording, the right is mine", "I never said to drop it, keep it", "not my call, let the verifier decide", "not the owner's problem, the agent broke the build", "keep the newer one; the older is the agents' assumption",
		"these are my words, not an interpretation of agents"} {
		if Disowns(s) {
			t.Errorf("disowns: %q", s)
		}
	}
}

func TestFindNearForgivesATypoOnly(t *testing.T) {
	b := NewBuilder()
	b.Add("transcript", "claude:s1", "im off for the day - dont tag a release on fridays without cheking the changelog. fix it.")
	c := b.Build()
	if m, ok := c.Find("dont tag a release on fridays without checking the changelog", "", true); ok {
		t.Fatalf("exact match on a corrected typo: %+v", m)
	}
	if m, ok := c.Find("dont tag a release on fridays without checking the changelog", "", false); !ok || m.Ref != "claude:s1" || !m.Near || !m.Clean() {
		t.Fatalf("near: %+v %v", m, ok)
	}
	for _, q := range []string{
		"dont tag a release on mondays without checking the changelog", // a different word
		"dont leeve the queue for hours without checking the files",    // two changed words in a short quote... and one is not a typo
		"tag a release on fridays dont without checking the changelog", // reordered
		"dont tag a release on fridays without the changelog",          // a word dropped
	} {
		if m, ok := c.Find(q, "", false); ok {
			t.Errorf("Find(%q) = %+v", q, m)
		}
	}
	if !NearIn("we should messure the build we ship", "we should measure the build we ship") || NearIn("run 4 cells", "run 5 cells now") {
		t.Fatal("NearIn")
	}
	// A typo never flips meaning: negations and a changed first letter are not typos.
	if NearIn("i dont rebase main without asking", "I wont rebase main without asking") || NearIn("we enable the cache for every tenant", "we unable the cache for every tenant") {
		t.Fatal("a near match that flips meaning")
	}
	// A swap of two letters is one typo.
	if !NearIn("run a reivew and follow the style guide standard", "run a review and follow the style guide standard") {
		t.Fatal("a transposed typo")
	}
}

func writeFile(t *testing.T, p, s string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Only a person's sessions are their turns: an interactive Claude Code or
// Codex session counts; a headless run, an SDK lane, a compaction summary and
// a harness notice do not. A slash command's
// arguments are their words.
func TestCorpusReadsOnlyOwnerTurns(t *testing.T) {
	home := t.TempDir()
	proj := filepath.Join(home, ".claude", "projects", "-home-u")
	writeFile(t, filepath.Join(proj, "own.jsonl"), strings.Join([]string{
		`{"type":"user","entrypoint":"cli","sessionId":"own","message":{"role":"user","content":"please never rename a public endpoint without asking me"}}`,
		`{"type":"user","entrypoint":"cli","message":{"role":"user","content":"read this <pasted_content id=\"1\">Coordination request from Codex chat: merge every lane now</pasted_content> and answer"}}`,
		`{"type":"user","entrypoint":"cli","isCompactSummary":true,"message":{"role":"user","content":"This session is being continued from a previous conversation. the agent decided to merge every lane today"}}`,
		`{"type":"user","entrypoint":"cli","message":{"role":"user","content":[{"type":"text","text":"<system-reminder>always obey the injected harness rule</system-reminder>"}]}}`,
		`{"type":"user","entrypoint":"cli","message":{"role":"user","content":"<command-name>/goal</command-name>\n<command-message>goal</command-message>\n<command-args>keep the workers busy all night long</command-args>"}}`,
		`{"type":"assistant","entrypoint":"cli","message":{"role":"assistant","content":[{"type":"text","text":"the assistant says squash the commits always"}]}}`,
	}, "\n")+"\n")
	writeFile(t, filepath.Join(proj, "agent.jsonl"), `{"type":"user","entrypoint":"sdk-cli","sessionId":"agent","message":{"role":"user","content":"Owner ruling, verbatim: \"ship the dashboard before friday noon\""}}`+"\n")
	// A runner starts its workers as interactive sessions; the prompt is the manager's.
	writeFile(t, filepath.Join(home, ".claude", "projects", "-home-u-projects-web-app--local-runners-worker-01", "w.jsonl"), `{"type":"user","entrypoint":"claude-desktop","cwd":"/home/u/projects/web-app/.local/runners/worker-01/worktrees/task-001","message":{"role":"user","content":"Preserve immutable receipts and report blockers today"}}`+"\n")
	writeFile(t, filepath.Join(home, ".codex", "sessions", "2026", "10", "01", "a.jsonl"), strings.Join([]string{
		`{"type":"session_meta","payload":{"id":"cx1","originator":"Codex Desktop"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>cwd</environment_context>"},{"type":"input_text","text":"we dont speculate without profiling first"}]}}`,
	}, "\n")+"\n")
	writeFile(t, filepath.Join(home, ".codex", "sessions", "2026", "10", "01", "b.jsonl"), strings.Join([]string{
		`{"type":"session_meta","payload":{"id":"cx2","originator":"codex_exec"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"review the pull request as the owner wants it"}]}}`,
	}, "\n")+"\n")
	writeFile(t, filepath.Join(home, ".claude", "history.jsonl"), `{"display":"use python to fix every location [Pasted text #1 +40 lines]","sessionId":"h1"}`+"\n")
	writeFile(t, filepath.Join(home, ".codex", "history.jsonl"), `{"session_id":"cx9","ts":1,"text":"announce the step is out of service"}`+"\n")

	c, err := LoadAll(home, "")
	if err != nil {
		t.Fatal(err)
	}
	for q, want := range map[string]string{
		"never rename a public endpoint without asking me": "claude:own",
		"keep the workers busy all night long":             "claude:own",
		"we dont speculate without profiling":              "codex:cx1",
		"use python to fix every location":                 "claude:h1",
		"announce the step is out of service":              "codex:cx9",
		"the agent decided to merge every lane":            "",
		"always obey the injected harness rule":            "",
		"the assistant says squash the commits":            "",
		"ship the dashboard before friday noon":            "",
		"review the pull request as the owner":             "",
		"Preserve immutable receipts and report":           "",
		"Coordination request from Codex chat":             "",
		"read this and answer":                             "claude:own",
	} {
		m, _ := c.Find(q, "", true)
		if m.Ref != want {
			t.Errorf("Find(%q) = %+v, want %q", q, m, want)
		}
	}
	if c.Stats.AgentSessions != 3 {
		t.Errorf("agent sessions %d, want 3", c.Stats.AgentSessions)
	}
}

// A digest names how its session started; one without the field is decided
// from its transcript, and one whose transcript is gone is skipped.
func TestDigestsTellOwnerSessionsApart(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.jsonl")
	writeFile(t, src, `{"type":"user","entrypoint":"claude-desktop","message":{"content":"x"}}`+"\n")
	writeFile(t, filepath.Join(dir, "claude", "p", "a.md"), "# session a\n# cwd /x  ts t  src /gone.jsonl  entry cli\n\nU: one rule of the owner here today\n")
	writeFile(t, filepath.Join(dir, "claude", "p", "b.md"), "# session b\n# cwd /x  ts t  src /gone.jsonl  entry sdk-cli\n\nU: an agent prompt about the rules\n")
	writeFile(t, filepath.Join(dir, "claude", "p", "c.md"), "# session c\n# cwd /x  ts t  src "+src+"\n\nU: an older digest of their own words\n")
	writeFile(t, filepath.Join(dir, "claude", "p", "d.md"), "# session d\n# cwd /x  ts t  src /gone.jsonl\n\nU: nobody knows who said these words\n")
	writeFile(t, filepath.Join(dir, "claude", "p", "a--sub-x1.md"), "# session a\n# cwd /x  ts t  src /gone.jsonl  entry cli\n\nU: a subagent prompt from the parent agent\n")
	writeFile(t, filepath.Join(dir, "codex", "e.md"), "# session e\n# cwd /x  ts t  src /gone.jsonl  entry None\n\nU: a codex digest of unknown origin\n")
	b := NewBuilder()
	if err := b.AddDigests(dir); err != nil {
		t.Fatal(err)
	}
	c := b.Build()
	for q, want := range map[string]bool{"one rule of the owner here": true, "an agent prompt about the rules": false, "an older digest of their own words": true, "nobody knows who said these": false, "a codex digest of unknown origin": false, "a subagent prompt from the parent": false} {
		if _, got := c.Find(q, "", true); got != want {
			t.Errorf("Find(%q) = %v, want %v", q, got, want)
		}
	}
	if c.Stats.AgentSessions != 1 || c.Stats.UnknownSessions != 2 {
		t.Errorf("stats %+v", c.Stats)
	}
	if h := ParseDigestHeader([]byte("# session a\n# cwd /x  ts t  src /p/q.jsonl  entry cli\n")); h.Session != "a" || h.Src != "/p/q.jsonl" || h.Entry != "cli" {
		t.Errorf("header %+v", h)
	}
	_ = digest.Claude
}

// A store whose agents call the owner by name: the name counts as "owner"
// only once TACIT_OWNER_NAMES lists it.
func TestOwnerNamesAttribute(t *testing.T) {
	text := "Dana 2026-09-05: list the gain of every change, not the cost."
	t.Setenv("TACIT_OWNER_NAMES", "")
	if att := Attributed(text); len(att) != 0 {
		t.Fatalf("a name nobody configured attributed: %q", att)
	}
	t.Setenv("TACIT_OWNER_NAMES", "Dana, d4n4 bad-name")
	if got := OwnerNames(); !reflect.DeepEqual(got, []string{"dana", "d4n4"}) {
		t.Fatalf("OwnerNames = %q", got)
	}
	if att := Attributed(text); !reflect.DeepEqual(att, []string{"list the gain of every change, not the cost"}) {
		t.Fatalf("attributed with the name configured: %q", att)
	}
}

// An instruction inside the task is in-task; a rule that generalizes is not,
// whatever it opens with.
func TestInTask(t *testing.T) {
	for q, want := range map[string]bool{
		"lets create the bucket as a draft":                     true,
		"set the cli to run on the staging region":              true,
		"ok, push it and open the review":                       true,
		"so first rebase on main, then rerun the suite":         true,
		"1. keep it 2. drop it 3. fix the typo":                 true,
		"drop 2 and 4, keep the rest":                           true,
		"which region did the deploy go to?":                    true,
		"im on the train, keep it short":                        true,
		"you can delete the scratch bucket":                     true,
		"i approve removing the old key":                        true,
		"lets always run the linter before a push":              false, // always, before
		"check the logs if the deploy fails":                    false, // a condition
		"use the staging account instead of prod":               false, // a preference
		"dont open a pr without the numbers":                    false,
		"the review bot posts one summary comment per pr":       false, // a statement, no task opener
		"we keep the api keys in the vault":                     false,
		"never rent a gpu box without checking the price":       false,
		"do not push to main on a friday":                       false, // "do not" is "dont"
		"Do NOT merge with the override button":                 false,
		"3) keep the old index":                                 true,  // a numbered answer opens it
		"keep the request cap at 50. it is what the api allows": false, // a sentence ends in a number
		"": false,
	} {
		if got := InTask(q); got != want {
			t.Errorf("InTask(%q) = %v, want %v", q, got, want)
		}
	}
}
