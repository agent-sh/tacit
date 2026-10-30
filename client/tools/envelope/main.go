// Command envelope measures how often the curator's rulings pass answers
// with something that is not a JSON array (a sentence before it, a code
// fence after one), the answer ParseChecked refuses whole. It sends the
// rulings prompt for each inbox note given, alone as a batch the way a quick
// run bisects to one note, through the judge route the curator uses, and
// counts the answers by what the parser makes of them, and the arrays that
// hold an entry (a ruling the pass recovered). With -retry, an
// answer refused for its envelope is sent back once the way the curator
// sends it back (curate.SplitChecked), and the second answer is counted too.
//
//	go run ./tools/envelope -store ~/.local/share/tacit/store -calls 5 \
//	    [-template old.txt] [-retry] inbox/<writer>/<note>.md ...
//
// -template replaces the built-in prompt with a file holding the same two
// %s verbs (the extracted rulings, the notes), for the prompt a change
// replaces. The tool is newer than the prompt it was first measured
// against: copy this file onto the base commit and run -print-template
// there to write the old one. The route is TACIT_JUDGE_RUNNER,
// TACIT_JUDGE_MODEL and TACIT_JUDGE_EFFORT, or claude with the flags given.
// For each note it also counts the calls whose answer holds an entry whose
// owner_quote is in the note word for word (a ruling the pass would keep),
// so two prompts can be compared note by note. Only counts and the first
// 160 characters of each refused answer are printed; nothing is written to
// the store, and every call's session is on the dreamer's exclude list.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/curate"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/modelrun"
)

func main() {
	store := flag.String("store", "", "the store clone the notes are in")
	calls := flag.Int("calls", 3, "calls per note")
	template := flag.String("template", "", "a rulings prompt template to use instead of the built-in one")
	printTemplate := flag.Bool("print-template", false, "print the built-in rulings prompt template and exit")
	retry := flag.Bool("retry", false, "send an answer refused for its envelope back once, as the curator does")
	runner := flag.String("runner", "claude", "runner, when TACIT_JUDGE_RUNNER is unset")
	model := flag.String("model", "claude-opus-5-5", "model, when TACIT_JUDGE_MODEL is unset")
	effort := flag.String("effort", "medium", "effort, when TACIT_JUDGE_EFFORT is unset")
	flag.Parse()
	if *printTemplate {
		os.Stdout.WriteString(curate.RulingsPrompt)
		return
	}
	tmpl := curate.RulingsPrompt
	if *template != "" {
		b, err := os.ReadFile(*template)
		check(err)
		tmpl = string(b)
	}
	route := modelrun.Route{Runner: env("TACIT_JUDGE_RUNNER", *runner), Model: env("TACIT_JUDGE_MODEL", *model), Effort: env("TACIT_JUDGE_EFFORT", *effort)}
	ctx := modelrun.WithRoute(context.Background(), route)
	counts := map[string]int{}
	var cost float64
	for _, rel := range flag.Args() {
		b, err := os.ReadFile(filepath.Join(*store, rel))
		check(err)
		n, err := fact.Parse(rel, b)
		check(err)
		prompt := fmt.Sprintf(tmpl, "(none)", curate.Render([]*fact.Fact{n}))
		quoted := 0
		for i := 0; i < *calls; i++ {
			text, c, err := call(ctx, route.Model, prompt)
			cost += c
			if err != nil {
				counts["call failed"]++
				fmt.Printf("%s call %d: failed: %v\n", n.Meta.Name, i+1, err)
				continue
			}
			kind := envelope(text)
			counts[kind]++
			if kind == "array" {
				es, _ := curate.ParseChecked(text)
				if len(es) > 0 {
					counts["array with entries"]++
				}
				if holdsQuote(es, n) {
					counts["array with a quote from the note"]++
					quoted++
				}
				continue
			}
			fmt.Printf("%s call %d: %s: %q\n", n.Meta.Name, i+1, kind, clip(text))
			if !*retry || kind == "entry problems" {
				continue
			}
			_, perr := curate.ParseChecked(text)
			text2, c2, err := call(ctx, route.Model, curate.RetryPrompt(prompt, text, perr))
			cost += c2
			if err != nil {
				counts["retry: call failed"]++
				continue
			}
			k2 := envelope(text2)
			counts["retry: "+k2]++
			if es, _ := curate.ParseChecked(text2); k2 == "array" && len(es) > 0 {
				counts["retry: array with entries"]++
			}
			if k2 != "array" {
				fmt.Printf("%s call %d retry: %s: %q\n", n.Meta.Name, i+1, k2, clip(text2))
			}
		}
		fmt.Printf("note %s: %d of %d call(s) with a quote from the note\n", n.Meta.Name, quoted, *calls)
	}
	fmt.Printf("route %s, %d note(s) x %d call(s): %v, $%.3f\n", route.Label(), len(flag.Args()), *calls, counts, cost)
}

func call(ctx context.Context, model, prompt string) (string, float64, error) {
	c, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	// The call is a session of its runner: the dreamer must never read it.
	sid := modelrun.NewSessionID()
	_ = modelrun.Exclude(sid)
	res, err := modelrun.Run(c, "judge", model, prompt, sid)
	if res.SessionID != "" && res.SessionID != sid {
		_ = modelrun.Exclude(res.SessionID)
	}
	return res.Text, res.CostUSD, err
}

// holdsQuote reports an entry whose owner_quote is in the note word for word
// (spaces collapsed), as the curator's evidence check needs.
func holdsQuote(es []curate.Entry, n *fact.Fact) bool {
	norm := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	text := norm(n.Meta.Description + " " + n.Body)
	for _, e := range es {
		if q := norm(e.OwnerQuote); len(strings.Fields(q)) >= 6 && strings.Contains(text, q) {
			return true
		}
	}
	return false
}

// envelope names what ParseChecked makes of an answer: "array" (parsed,
// whatever its entries), "not an array", "malformed array", or "entry
// problems" (an array whose entries the parser refuses).
func envelope(text string) string {
	_, err := curate.ParseChecked(text)
	switch {
	case err == nil:
		return "array"
	case len(curate.Problems(err)) > 0:
		return "entry problems"
	case errors.Is(err, modelrun.ErrInvalidResult) && strings.Contains(err.Error(), "must return a JSON array"):
		return "not an array"
	}
	return "malformed array"
}

func clip(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 160 {
		return s[:160] + "..."
	}
	return s
}

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "envelope:", err)
		os.Exit(1)
	}
}
