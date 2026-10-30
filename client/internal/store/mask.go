package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/index"
	"github.com/agent-sh/tacit/client/internal/tree"
)

// Every write into the store is masked before it is staged (SPEC.md, "Every
// write is masked"): the prose a commit adds to the Markdown files it names
// (a fact's or a note's description and body, a record's and an overview's
// text, a review file's lines) goes through scrump's text profile, each run
// of added lines as it would be alone, whoever wrote it and whatever text it
// copied. The 2026-10-26 rebuild's curator split an imported memory whose
// body held a loopback llama-server key into a curated fact: the note had
// come in through a path that did not mask it, and nothing between the note
// and the push masked what the curator copied. A write whose redaction
// cannot run is refused before anything is staged (fail closed).
//
// What the mask leaves: what HEAD already holds (a write masks what it adds;
// it does not clean the store), the frontmatter but a description (names,
// scopes, sessions and hashes: on the author's store the text profile took
// 113 session ids, 14 origins and 5 names for secrets), a [[link]] (a fact's
// name), the owner's recorded words (owner_quote, owner_words: theirs, as
// their text in tacit app is), the files tacit generates from the facts, and
// JSON files, whose free text their writers mask.

// Redact masks secret-shaped text in several texts at once, each as it would
// be alone, returning each at its length (digest.RedactAllContext: scrump's
// text profile, records mode). Tests replace it.
var Redact = digest.RedactAllContext

// Reindex writes again the indexes tacit generates from the facts' text
// (MEMORY.md and the folder indexes) and returns the files it wrote, which
// join the commit. A write that masked a fact or a record calls it, so the
// indexes hold the text the commit holds. cmd/tacit sets it.
var Reindex func(ctx context.Context, dir string) ([]string, error)

// ErrRedaction is a write refused because what it adds could not be
// masked: nothing was staged or committed.
var ErrRedaction = errors.New("redaction could not run, so nothing was written")

type ownerTextKey struct{}

// WithOwnerText marks a write of the owner's own text from tacit app: it is
// theirs and is not masked (SPEC.md, "App"). The server's secret scan still
// refuses a push that holds a secret.
func WithOwnerText(ctx context.Context) context.Context {
	return context.WithValue(ctx, ownerTextKey{}, true)
}

func ownerText(ctx context.Context) bool {
	v, _ := ctx.Value(ownerTextKey{}).(bool)
	return v
}

type maskedKey struct{}

// WithMasked marks a write whose writer masked the prose it adds itself, in
// the same call, as the mask would (tacit add masks a note's description and
// body before it writes the note): the store does not mask it twice. scrump
// takes about half a second to start, which an agent's add would pay twice.
func WithMasked(ctx context.Context) context.Context {
	return context.WithValue(ctx, maskedKey{}, true)
}

func preMasked(ctx context.Context) bool {
	v, _ := ctx.Value(maskedKey{}).(bool)
	return v
}

// maskedKind is a file the mask reads: a Markdown file tacit does not
// generate.
func maskedKind(rel string) bool {
	if !strings.HasSuffix(rel, ".md") || rel == index.File || path.Base(rel) == tree.FileName {
		return false
	}
	return true
}

// maskPlan is one file's texts for the redactor and how to put the masked
// texts back.
type maskPlan struct {
	texts []string
	apply func(red []string) (string, error)
}

// maskAdded masks what a write adds to the Markdown files it names and
// writes back each file whose text the mask changed, returning them.
func (s *Store) maskAdded(ctx context.Context, rels []string) ([]string, error) {
	type file struct {
		rel, now string
		perm     os.FileMode
		plan     maskPlan
		first    int
	}
	var files []*file
	seen := map[string]bool{}
	for _, rel := range rels {
		if seen[rel] || !maskedKind(rel) {
			continue
		}
		seen[rel] = true
		if !filepath.IsLocal(filepath.FromSlash(rel)) || s.linkedParent(rel) {
			continue // git stages no file outside the clone or beyond a link
		}
		p := filepath.Join(s.Dir, filepath.FromSlash(rel))
		st, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) {
			continue // a retirement or a move removes a file
		}
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrRedaction, err)
		}
		if !st.Mode().IsRegular() {
			continue // a link is staged as a link; its target is not read
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrRedaction, err)
		}
		if bytes.IndexByte(b, 0) >= 0 {
			continue // not text
		}
		files = append(files, &file{rel: rel, now: string(b), perm: st.Mode().Perm()})
	}
	if len(files) == 0 {
		return nil, nil
	}
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = f.rel
	}
	old, err := s.headBlobs(ctx, names)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRedaction, err)
	}
	var texts []string
	for _, f := range files {
		f.plan = planMarkdown(old[f.rel], f.now)
		f.first = len(texts)
		texts = append(texts, f.plan.texts...)
	}
	if len(texts) == 0 {
		return nil, nil
	}
	red, err := Redact(ctx, texts)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRedaction, err)
	}
	if len(red) != len(texts) {
		return nil, fmt.Errorf("%w: the redactor answered %d texts for %d", ErrRedaction, len(red), len(texts))
	}
	for i := range texts {
		if len(red[i]) != len(texts[i]) {
			return nil, fmt.Errorf("%w: the redactor changed the length of a text", ErrRedaction)
		}
	}
	var masked []string
	for _, f := range files {
		if len(f.plan.texts) == 0 {
			continue
		}
		data, err := f.plan.apply(red[f.first : f.first+len(f.plan.texts)])
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %v", ErrRedaction, f.rel, err)
		}
		if data == f.now {
			continue
		}
		if err := writeAtomic(filepath.Join(s.Dir, filepath.FromSlash(f.rel)), []byte(data), f.perm); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrRedaction, err)
		}
		masked = append(masked, f.rel)
	}
	return masked, nil
}

// linkedParent reports a path one of whose directories in the clone is a
// symbolic link: the mask never reads or writes a file through one.
func (s *Store) linkedParent(rel string) bool {
	dir := s.Dir
	parts := strings.Split(filepath.FromSlash(rel), string(filepath.Separator))
	for _, part := range parts[:len(parts)-1] {
		dir = filepath.Join(dir, part)
		if st, err := os.Lstat(dir); err == nil && st.Mode()&os.ModeSymlink != 0 {
			return true
		}
	}
	return false
}

// frontmatter is what the mask reads of a file's frontmatter: its
// description and the owner's recorded words.
type frontmatter struct {
	Description string `yaml:"description"`
	Source      struct {
		OwnerQuote string `yaml:"owner_quote"`
		OwnerWords string `yaml:"owner_words"`
	} `yaml:"source"`
}

// planMarkdown plans the mask of one Markdown file: with frontmatter, its
// description when the write changed it and the runs of body lines HEAD
// does not hold; without, the runs of added lines of the whole text. A
// [[link]] and the owner's recorded words are left as they are.
func planMarkdown(old, now string) maskPlan {
	fm, body, ok := fact.Split([]byte(now))
	if !ok {
		return planProse(old, now, protected(now, nil))
	}
	var meta frontmatter
	if yaml.Unmarshal(fm, &meta) != nil {
		meta = frontmatter{} // a frontmatter that does not read: only its body is prose
	}
	oldBody, oldDesc := old, ""
	if ofm, ob, ok := fact.Split([]byte(old)); ok {
		oldBody = string(ob)
		var om frontmatter
		if yaml.Unmarshal(ofm, &om) == nil {
			oldDesc = om.Description
		}
	}
	words := phrases(meta.Source.OwnerQuote, meta.Source.OwnerWords)
	head := now[:len(now)-len(body)]
	bp := planProse(oldBody, string(body), protected(string(body), words))
	desc := meta.Description != "" && meta.Description != oldDesc
	p := maskPlan{texts: bp.texts}
	if desc {
		p.texts = append(append([]string(nil), bp.texts...), meta.Description)
	}
	p.apply = func(red []string) (string, error) {
		b, err := bp.apply(red[:len(bp.texts)])
		if err != nil {
			return "", err
		}
		out := head + b
		if desc {
			d := keep(meta.Description, red[len(bp.texts)], wordSpans(meta.Description, words))
			if d != meta.Description {
				patched, err := fact.PatchDescription([]byte(out), d)
				if err != nil {
					return "", err
				}
				out = string(patched)
			}
		}
		return out, nil
	}
	return p
}

// planProse plans the mask of the runs of lines of now that old does not
// hold; spans are byte ranges of now the mask leaves as they are.
func planProse(old, now string, spans [][2]int) maskPlan {
	lines := splitLines(now)
	runs := addedRuns(splitLines(old), lines)
	at := make([]int, len(lines)+1)
	for i, l := range lines {
		at[i+1] = at[i] + len(l)
	}
	p := maskPlan{}
	for _, r := range runs {
		p.texts = append(p.texts, now[at[r[0]]:at[r[1]]])
	}
	p.apply = func(red []string) (string, error) {
		var b strings.Builder
		prev := 0
		for i, r := range runs {
			start, end := at[r[0]], at[r[1]]
			b.WriteString(now[prev:start])
			var local [][2]int
			for _, sp := range spans {
				if sp[1] > start && sp[0] < end {
					local = append(local, [2]int{max(sp[0], start) - start, min(sp[1], end) - start})
				}
			}
			b.WriteString(keep(now[start:end], red[i], local))
			prev = end
		}
		b.WriteString(now[prev:])
		return b.String(), nil
	}
	return p
}

// keep is red with the bytes of spans taken from orig (both of one length).
func keep(orig, red string, spans [][2]int) string {
	if len(spans) == 0 || orig == red {
		return red
	}
	b := []byte(red)
	for _, sp := range spans {
		copy(b[sp[0]:sp[1]], orig[sp[0]:sp[1]])
	}
	return string(b)
}

// protected is the byte ranges of text the mask leaves: its [[links]] and
// every place it holds one of the owner's recorded phrases.
func protected(text string, words []string) [][2]int {
	var out [][2]int
	for _, sp := range fact.LinkSpans(text) {
		out = append(out, [2]int{sp.Start, sp.End})
	}
	return append(out, wordSpans(text, words)...)
}

// wordSpans is every place text holds one of words.
func wordSpans(text string, words []string) [][2]int {
	var out [][2]int
	for _, w := range words {
		for from := 0; ; {
			i := strings.Index(text[from:], w)
			if i < 0 {
				break
			}
			out = append(out, [2]int{from + i, from + i + len(w)})
			from += i + len(w)
		}
	}
	return out
}

// phrases is the owner's recorded words as the text holds them: each quote
// split at its ellipses (a quote of parts holds them in order, apart).
func phrases(quotes ...string) []string {
	var out []string
	for _, q := range quotes {
		for _, part := range strings.FieldsFunc(strings.ReplaceAll(q, "...", "…"), func(r rune) bool { return r == '…' }) {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

// splitLines splits s into lines that keep their newline, so joining them
// gives s back.
func splitLines(s string) []string {
	var out []string
	for s != "" {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			out = append(out, s)
			break
		}
		out = append(out, s[:i+1])
		s = s[i+1:]
	}
	return out
}

// addedRuns is the runs of lines of now that old does not hold, as
// [start, end) line ranges. A line counts as old as many times as old holds
// it, so a line moved within the file is old and a copy of an old line is
// new. A last line that only lost or gained its newline is old.
func addedRuns(old, now []string) [][2]int {
	have := map[string]int{}
	for _, l := range old {
		have[strings.TrimSuffix(l, "\n")]++
	}
	var runs [][2]int
	start := -1
	for i, l := range now {
		key := strings.TrimSuffix(l, "\n")
		if have[key] > 0 {
			have[key]--
			if start >= 0 {
				runs = append(runs, [2]int{start, i})
				start = -1
			}
			continue
		}
		if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		runs = append(runs, [2]int{start, len(now)})
	}
	return runs
}

// headBlobs reads the named files as HEAD holds them, in one git call. A
// file HEAD does not hold, or a clone with no commit yet, reads empty.
func (s *Store) headBlobs(ctx context.Context, rels []string) (map[string]string, error) {
	out := map[string]string{}
	if _, err := git(ctx, s.Dir, "rev-parse", "--verify", "--quiet", "HEAD^{commit}"); err != nil {
		return out, nil
	}
	var in strings.Builder
	for _, rel := range rels {
		if strings.ContainsAny(rel, "\n\r") {
			return nil, fmt.Errorf("a path with a line break: %q", rel)
		}
		in.WriteString("HEAD:" + rel + "\n")
	}
	raw, err := gitEnvInput(ctx, s.Dir, nil, in.String(), "cat-file", "--batch")
	if err != nil {
		return nil, err
	}
	for _, rel := range rels {
		nl := strings.IndexByte(raw, '\n')
		if nl < 0 {
			return nil, errors.New("git cat-file: a short answer")
		}
		head := raw[:nl]
		raw = raw[nl+1:]
		if strings.HasSuffix(head, " missing") || strings.HasSuffix(head, " ambiguous") {
			continue
		}
		f := strings.Fields(head)
		if len(f) != 3 {
			return nil, fmt.Errorf("git cat-file: %q", head)
		}
		n, err := strconv.Atoi(f[2])
		if err != nil || n < 0 || n+1 > len(raw) {
			return nil, fmt.Errorf("git cat-file: %q", head)
		}
		if f[1] == "blob" {
			out[rel] = raw[:n]
		}
		raw = raw[n+1:]
	}
	return out, nil
}

// writeAtomic replaces a file by a rename, so a killed write leaves it whole.
func writeAtomic(p string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(p), ".mask-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, perm); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, p); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// touchesIndexes reports a masked file whose text the generated indexes
// show: a fact or a record (not an inbox note, not a review file).
func touchesIndexes(rels []string) bool {
	for _, rel := range rels {
		if !strings.HasPrefix(rel, "inbox/") && !strings.HasPrefix(rel, "review/") {
			return true
		}
	}
	return false
}

// withAll is rels with every path of more it does not hold yet, in order.
func withAll(rels, more []string) []string {
	have := map[string]bool{}
	for _, r := range rels {
		have[r] = true
	}
	out := append([]string(nil), rels...)
	for _, r := range more {
		if !have[r] {
			have[r] = true
			out = append(out, r)
		}
	}
	return out
}
