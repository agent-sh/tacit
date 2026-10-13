package main

// "This is mine" and "Not mine": the owner says whether a fact the agents
// wrote is their rule. The app records their decision (review/decisions.jsonl,
// action "mine" or "not-mine"), and for "mine" makes the fact a ruling with
// that decision as its evidence (source.kind user, source.endorsed), in one
// owner commit. Nothing asks them: the facts agents believe the owner prefers wait in
// their own place (What agents believe you prefer) until the owner browses them.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/ledger"
	"github.com/agent-sh/tacit/client/internal/ownerwords"
	"github.com/agent-sh/tacit/client/internal/store"
)

// actionMine is the decision the app records when the owner says a fact is
// theirs ("This is mine"); actionNotMine when the owner says it is an agent's.
const (
	actionMine    = ledger.ActionMine
	actionNotMine = ledger.ActionNotMine
)

// mineID is the decision id of "This is mine" on a fact: one per fact, so a
// second press finds the first.
func mineID(name string) string { return decisionID(actionMine, name) }

// notMineID is the decision id of "Not mine" on a fact.
func notMineID(name string) string { return decisionID(actionNotMine, name) }

func decisionID(action, name string) string {
	h := sha256.Sum256([]byte(action + "\x00" + name))
	return hex.EncodeToString(h[:6])
}

// believed reports a fact the agents filed as the owner's preference that is
// not their ruling and that the owner has not said is an agent's: a curated
// user_standard without their words, or a fact that holds a few words of theirs
// (fact.OwnerWordsOf). The app lists them apart ("What agents believe you
// prefer") for them to browse, never in Needs you; other facts the audit set
// to observed are in All facts.
func believed(f *fact.Fact) bool {
	return !f.InInbox() && !isRuling(f) && !f.NotMine && !strings.HasPrefix(f.Meta.Scope, "derived/") &&
		(f.Meta.Type == "user_standard" || fact.OwnerWordsOf(f) != "")
}

// undoNotMine takes back their last Not mine on a fact: it reopens that
// decision, so the fact is one agents believe the owner prefers again, and words of
// their it holds count again.
func (rs *reviewServer) undoNotMine(ctx context.Context, ref string) (*saveResult, error) {
	return rs.ownerWrite(ctx, func() (*ownerChange, error) {
		f, _, err := rs.ownerFact(ctx, ref)
		if err != nil {
			return nil, err
		}
		dl, _ := os.ReadFile(filepath.Join(rs.s.Dir, filepath.FromSlash(ledger.DecisionsPath)))
		if _, ok := ledger.NotMine(dl)[f.Meta.Name]; !ok {
			return &ownerChange{name: f.Meta.Name, note: "You have not said this fact is not yours."}, nil
		}
		subj := ownerCommit + "not mine: " + f.Meta.Name
		sha, err := store.Git(ctx, rs.s.Dir, "log", "-1", "--format=%H", "--fixed-strings", "--grep="+subj, "--", ledger.DecisionsPath)
		if err != nil || strings.TrimSpace(sha) == "" {
			return nil, errors.New("the store has no Not mine of yours on this fact to undo")
		}
		files := map[string][]byte{}
		rs.appendDecision(files, decision{ID: notMineID(f.Meta.Name), Action: "reopen", A: f.Meta.Name, At: time.Now().Format(time.RFC3339)})
		return &ownerChange{name: f.Meta.Name, write: files,
			msg: fmt.Sprintf("%sundo not mine: %s\n\nUndone by the owner in tacit app.", ownerCommit, f.Meta.Name)}, nil
	})
}

// endorse makes a fact their ruling on their word.
func (rs *reviewServer) endorse(ctx context.Context, ref string) (*saveResult, error) {
	return rs.ownerWrite(ctx, func() (*ownerChange, error) {
		f, data, err := rs.ownerFact(ctx, ref)
		if err != nil {
			return nil, err
		}
		ch := &ownerChange{name: f.Meta.Name}
		if isRuling(f) {
			ch.note = "This is already your rule."
			return ch, nil
		}
		id := mineID(f.Meta.Name)
		c := *f
		c.Meta.Source.Kind, c.Meta.Source.Endorsed = "user", id
		nb, err := fact.PatchSource(data, &c)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.Path, err)
		}
		files := map[string][]byte{f.Path: nb}
		rs.appendDecision(files, decision{ID: id, Action: actionMine, A: f.Meta.Name, At: time.Now().Format(time.RFC3339)})
		ch.write = files
		ch.msg = fmt.Sprintf("%sthis is mine: %s\n\nThe owner says this fact is their rule, in tacit app.", ownerCommit, f.Meta.Name)
		return ch, nil
	})
}

// disown records that a fact is an agent's, not their rule: it leaves What
// agents believe you prefer, words of theirs it holds no longer guard it
// (fact.StoreState.Apply), and the file stays as it is.
func (rs *reviewServer) disown(ctx context.Context, ref string) (*saveResult, error) {
	return rs.ownerWrite(ctx, func() (*ownerChange, error) {
		f, _, err := rs.ownerFact(ctx, ref)
		if err != nil {
			return nil, err
		}
		ch := &ownerChange{name: f.Meta.Name}
		if isRuling(f) {
			return nil, fmt.Errorf("%s is your rule; retire it, or edit it", f.Meta.Name)
		}
		if f.NotMine {
			ch.note = "You already said this is not yours."
			return ch, nil
		}
		files := map[string][]byte{}
		rs.appendDecision(files, decision{ID: notMineID(f.Meta.Name), Action: actionNotMine, A: f.Meta.Name, At: time.Now().Format(time.RFC3339)})
		ch.write = files
		ch.msg = fmt.Sprintf("%snot mine: %s\n\nThe owner says this fact is an agent's statement, not their rule, in tacit app.", ownerCommit, f.Meta.Name)
		return ch, nil
	})
}

// keepOwnWords is the edit rule for a ruling resting on a quote: when the
// owner's edit takes the quoted phrase out of the text, the words are their
// edit now, so the fact keeps its standing on a "mine" decision instead of a
// quote it no longer holds. files gets the decision; nf is changed in place.
//
// Words of theirs on a fact that is not their rule (source.owner_words) go with
// the phrase: an edit of theirs that takes it out drops them, since the text no
// longer holds them.
func (rs *reviewServer) keepOwnWords(files map[string][]byte, old, nf *fact.Fact) {
	if w := nf.Meta.Source.OwnerWords; w != "" && !quoteIn(nf, w) {
		nf.Meta.Source.OwnerWords, nf.Meta.Source.OwnerWordsFrom = "", ""
	}
	if !isRuling(old) || old.Meta.Source.OwnerQuote == "" || fact.OwnerWrote(old) {
		return
	}
	if quoteIn(nf, old.Meta.Source.OwnerQuote) {
		return
	}
	id := mineID(nf.Meta.Name)
	nf.Meta.Source.OwnerQuote, nf.Meta.Source.OwnerQuoteFrom = "", ""
	nf.Meta.Source.Endorsed = id
	rs.appendDecision(files, decision{ID: id, Action: actionMine, A: nf.Meta.Name, At: time.Now().Format(time.RFC3339)})
}

func quoteIn(f *fact.Fact, q string) bool {
	return ownerwords.In(f.Meta.Description+"\n"+f.Body, q)
}
