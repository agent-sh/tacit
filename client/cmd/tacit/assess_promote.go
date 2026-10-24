package main

// The rulings audit's promotion before the assessment (SPEC.md, "Folder
// records", Owner words beat dates): the assessment reads a note that claims
// the owner's authority without their words as an agent's note
// (noteClaims), so first the claims whose words are the owner's are made
// theirs. A curated fact that claims their authority (search.FactClaims,
// delivery's check) whose quoted or attributed phrase is found in the owner's
// history (their turns, their app notes, a quote the ledger holds) becomes
// their ruling, or holds their words when the phrase is too short to prove
// the rule, with its quote ledger entry: what `tacit rulings audit` decides
// for it (rulingAuditor.decide). Code only, no model. Nothing is demoted, the
// audit marker is not touched, and a store the audit has not run on is left
// alone (its marker switches the store to the definition the promotion
// writes).

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/ownerwords"
	"github.com/agent-sh/tacit/client/internal/search"
	"github.com/agent-sh/tacit/client/internal/store"
)

// promotes reports a verdict the promotion writes: the fact becomes the
// owner's ruling or holds their words, and its source block changes.
func promotes(v rulingVerdict, f *fact.Fact) bool {
	return (v.Ruling() || v.Outcome == ruledShort) && v.changed(f)
}

// claimPromotions are the promotions the store at dir holds now: every
// curated store fact that claims the owner's authority without their words
// and whose words the auditor finds.
func claimPromotions(ctx context.Context, dir string, corpus *ownerwords.Corpus) (*rulingsReport, error) {
	facts, a, err := auditorFor(ctx, dir, corpus)
	if err != nil {
		return nil, err
	}
	rep := &rulingsReport{}
	if corpus != nil {
		rep.Corpus = corpus.Stats
	}
	for _, f := range facts {
		if f.InInbox() || strings.HasPrefix(f.Meta.Scope, "derived/") || !search.FactClaims(f, false) {
			continue
		}
		v := a.decide(f)
		if !promotes(v, f) {
			continue
		}
		rep.Verdicts = append(rep.Verdicts, v)
		rep.Changed = append(rep.Changed, f.Path)
	}
	return rep, nil
}

// promoteClaims runs the promotion on the store before an assessment: under
// the store lock as curator (curatorWrite) unless dry, which only counts.
// The local layer's runs leave the store's clone alone (the machine that
// curates the store promotes its claims). It returns the promoted verdicts.
func promoteClaims(ctx context.Context, s *store.Store, corpus *ownerwords.Corpus, local, dry bool) ([]rulingVerdict, error) {
	if local {
		return nil, nil
	}
	if _, ok := ledger.ReadMarker(s.Dir); !ok {
		return nil, nil
	}
	if dry {
		rep, err := claimPromotions(ctx, s.Dir, corpus)
		if err != nil {
			return nil, err
		}
		return rep.Verdicts, nil
	}
	var rep *rulingsReport
	_, err := curatorWrite(ctx, s, func() (map[string][]byte, string, error) {
		var err error
		rep, err = claimPromotions(ctx, s.Dir, corpus)
		if err != nil || len(rep.Verdicts) == 0 {
			return nil, "", err
		}
		head, _ := s.Head(ctx)
		files, err := rep.writes(s.Dir, head, time.Now())
		if err != nil {
			return nil, "", err
		}
		delete(files, ledger.MarkerPath) // the promotion never rewrites the audit's marker
		return files, promotionMessage(rep), nil
	})
	if err != nil {
		return nil, err
	}
	return rep.Verdicts, nil
}

func promotionMessage(rep *rulingsReport) string {
	c := rep.counts()
	return fmt.Sprintf("curator: rulings promotion before assess: %d note(s) claiming the owner's authority hold their words (%d ruling(s): %d quoted, %d kept by them; %d with their words recorded)\n\n"+
		"A note that claims the owner's authority is an agent's note to the assessment until its words are\n"+
		"found to be theirs (SPEC.md, Owner words beat dates). No model was called.",
		len(rep.Verdicts), c.kept(), c.byOutcome[ruledQuote], c.byOutcome[ruledEndorsed], c.byOutcome[ruledShort])
}

// reportPromotions prints what the promotion did.
func reportPromotions(vs []rulingVerdict, dry bool) {
	if len(vs) == 0 {
		return
	}
	verb := "promoted"
	if dry {
		verb = "would promote"
	}
	for _, v := range vs {
		fmt.Fprintf(os.Stderr, "assess: rulings promotion %s %s (%s): %s %s\n", verb, v.Name, v.Outcome, v.From, oneLine(v.Quote))
	}
}
