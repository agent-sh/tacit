package fact

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/agent-sh/tacit/client/internal/ledger"
)

// StoreState is what a store says about its facts beyond their files: whether
// the rulings audit has run on it (its marker), and the records the owner's
// evidence must match (the quote ledger, their standing decisions). Every read
// path applies it, so a fact read on its own (Parse) is judged the same as one
// from Walk (WithStore).
type StoreState struct {
	Audited bool
	// Format is the store's format marker (ledger.FormatPath); zero without
	// one. FormatErr is a marker that exists and cannot be read: no writer
	// writes the new format then (fail closed), and lint reports it.
	Format    ledger.Format
	FormatErr error
	quotes    map[string]ledger.Quote
	endorsed  map[string]ledger.Endorsement
	notMine   map[string]string
	// Reviews is the reviewer's ledger (ledger.ReviewsPath).
	Reviews *ledger.Reviews
}

// ErrMarker is a rulings audit marker that exists but cannot be read. The
// store does not fall back to the old definition of a ruling (that would make
// every user_standard an agent filed a rule no agent may touch, and drop the
// rulings that rest on their words): it is an error.
var ErrMarker = errors.New("the rulings audit marker cannot be read")

// LoadState reads the store state at root.
func LoadState(root string) (StoreState, error) {
	st := StoreState{}
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(ledger.MarkerPath)))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return st, fmt.Errorf("%w: %v", ErrMarker, err)
	default:
		if _, perr := ledger.ParseMarker(b); perr != nil {
			return st, fmt.Errorf("%w (%s): %v", ErrMarker, ledger.MarkerPath, perr)
		}
		st.Audited = true
	}
	if f, ok, ferr := ledger.ReadFormat(root); ferr != nil {
		st.FormatErr = ferr
	} else if ok {
		st.Format = f
	}
	ql, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(ledger.QuotesPath)))
	dl, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(ledger.DecisionsPath)))
	st.quotes, st.endorsed, st.notMine = ledger.Quotes(ql), ledger.Endorsements(dl), ledger.NotMine(dl)
	rl, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(ledger.ReviewsPath)))
	st.Reviews = ledger.ParseReviews(rl)
	return st, nil
}

// Apply sets on f what the store says about it: audited, and whether its
// evidence is backed (a ledger entry for its quote or its words, a standing
// decision of theirs for its endorsement). Their standing Not mine on a fact
// answers its words: they no longer hold it.
func (st StoreState) Apply(f *Fact) {
	f.Audited = st.Audited
	s := f.Meta.Source
	_, f.QuoteBacked = st.quotes[ledger.Quote{Name: f.Meta.Name, Quote: s.OwnerQuote, From: s.OwnerQuoteFrom}.Key()]
	f.EndorsedBacked = s.Endorsed != "" && st.endorsed[f.Meta.Name].ID == s.Endorsed
	_, f.NotMine = st.notMine[f.Meta.Name]
	_, words := st.quotes[ledger.Quote{Name: f.Meta.Name, Quote: s.OwnerWords, From: s.OwnerWordsFrom, Words: true}.Key()]
	f.WordsRecorded = words && s.OwnerWords != "" && s.OwnerWordsFrom != "" && !f.InInbox()
	f.WordsBacked = f.WordsRecorded && !f.NotMine
	st.ApplyReview(f)
}

// ApplyReview sets f.Review from the reviewer's ledger: the verdict on the
// note at f's path, else, for a curated fact, on the note it was made from
// (its origin). A verdict field in f's frontmatter is a claim and changes
// nothing here.
func (st StoreState) ApplyReview(f *Fact) {
	f.Review, f.ReviewReason, f.ReviewDay = "", "", ""
	r, ok := st.Reviews.ForNote(f.Path)
	if !ok && !f.InInbox() && f.Meta.Origin != "" && f.Meta.Source.Harness == "curator" {
		r, ok = st.Reviews.ForOrigin(f.Meta.Origin)
	}
	if ok {
		f.Review, f.ReviewReason = r.Verdict, r.Reason
		if len(r.At) >= 10 {
			f.ReviewDay = r.At[:10]
		}
	}
}

// WithStore applies the state of the store at root to a fact read on its own
// (Parse). A state that cannot be read (a broken marker) is an error: the
// caller must refuse to act, since it cannot tell a ruling from any other
// fact (fail closed).
func WithStore(root string, f *Fact) error {
	st, err := LoadState(root)
	if err != nil {
		return err
	}
	st.Apply(f)
	return nil
}

// FormatWrites reports whether writers of this binary may write the folder
// records format into the store at root (ledger.Format.Writes), and why not.
func FormatWrites(root string) (bool, string) {
	f, ok, err := ledger.ReadFormat(root)
	switch {
	case err != nil:
		return false, fmt.Sprintf("the store's format marker (%s) cannot be read: %v", ledger.FormatPath, err)
	case !ok:
		return false, fmt.Sprintf("the store has no format marker (%s): folder records, the areas/ and topics/ folders and last_checked are written only once every machine runs a binary that reads them and the owner has run `tacit format --set`", ledger.FormatPath)
	case !f.Writes():
		return false, fmt.Sprintf("the store's format is version %d, newer than this binary's %d: update tacit", f.Version, ledger.FormatVersion)
	}
	return true, ""
}
