package dream

import (
	"fmt"
	"strings"
	"testing"

	"github.com/agent-sh/tacit/client/internal/fact"
)

func curatedHeld(scope, name, desc string) *fact.Fact {
	return &fact.Fact{Path: scope + "/" + name + ".md", Meta: fact.Meta{Name: name, Description: desc, Type: "machine_fact", Scope: scope, Created: "2026-10-20", Source: fact.Source{Kind: "observed", Harness: "curator"}}, Body: desc + "\n"}
}

func TestHeldListsWhatTheWindowIsAbout(t *testing.T) {
	waitfor := curatedHeld("machines/box", "pollwait", "pollwait blocks on a condition: `pollwait --until '<probe cmd>' --every 2m --timeout 1h`.")
	heartbeat := curatedHeld("projects/fleet", "gpu-box-heartbeat", "host-b heartbeat: `timeout 40 ssh host-b 'cd ~/jobs/box/orch; tail -3 orch.log'`.")
	other := curatedHeld("projects/elsewhere", "walrus", "The walrus cache lives in /srv/walrus/cache and is pruned by walrus-prune.")
	tied := curatedHeld("system", "tied", "The quokka scheduler listens on 7781.")
	unrelated := curatedHeld("machines/box", "borg", "Local borg backups live in ~/archive/borg.")
	rule := curatedHeld("user", "rule", "Never force-push to main.")
	rule.Meta.Source.Kind = "user"
	words := curatedHeld("machines/box", "words", "pollwait is what the owner wants for every wait.")
	words.Meta.Source.OwnerWords = "use pollwait"
	retired := curatedHeld("machines/box", "old-pollwait", "pollwait lives in ~/bin/pollwait.")
	retired.Meta.SupersededBy = "pollwait"
	overview := curatedHeld("projects/fleet", "_overview", "fleet: host-b heartbeat, orch.log, the jobs box.")
	inbox := curatedHeld("machines/box", "pending", "pollwait --until polls every 2m.")
	inbox.Path = "inbox/dreamer@box/pending.md"
	p := NewHeldPool([]*fact.Fact{waitfor, heartbeat, other, tied, unrelated, rule, words, retired, overview, inbox})
	lines := []string{
		"A: pollwait blocks on a condition, so I'll wait with pollwait --until '<probe cmd>' --every 2m --timeout 1h",
		"T: timeout 40 ssh host-b 'cd ~/jobs/box/orch; tail -3 orch.log'",
		"A: the walrus cache in /srv/walrus/cache was pruned by walrus-prune",
		"U: never force-push to main",
	}
	got := p.Held("fleet", "box", lines, HeldChars)
	want := []string{"- " + waitfor.Meta.Description, "- " + heartbeat.Meta.Description}
	if strings.Join(sortedCopy(got), "\n") != strings.Join(sortedCopy(want), "\n") {
		t.Fatalf("held:\n%s", strings.Join(got, "\n"))
	}
	// The cap keeps the best that fit.
	if got := p.Held("fleet", "box", lines, len(want[0])+2); len(got) != 1 {
		t.Fatalf("capped: %q", got)
	}
	if got := p.Held("fleet", "box", lines, 0); got != nil {
		t.Fatalf("off: %q", got)
	}
	// Another repository's fact is not held for this one.
	if got := p.Held("elsewhere", "other", lines[2:3], HeldChars); len(got) != 1 || !strings.Contains(got[0], "walrus") {
		t.Fatalf("its own repository's fact: %q", got)
	}
}

func sortedCopy(xs []string) []string {
	out := append([]string(nil), xs...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func TestPromptForWithoutHeldIsThePrompt(t *testing.T) {
	lines := []string{"U: hello", "A: done"}
	if got, want := PromptFor("r", "h", nil, lines), fmt.Sprintf(Prompt, "r", "h", strings.Join(lines, "\n")); got != want {
		t.Fatal("the prompt with nothing held changed")
	}
	got := PromptFor("r", "h", []string{"- a held fact"}, lines)
	if !strings.Contains(got, "ALREADY HELD") || !strings.Contains(got, "- a held fact\n") || !strings.HasSuffix(got, "DIGEST:\nU: hello\nA: done\n") || !strings.Contains(got, "Repo: r  Host: h\n") || strings.Index(got, "ALREADY HELD") < strings.Index(got, "Repo: r") {
		t.Fatalf("held prompt:\n%s", got)
	}
}
