package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"

	"github.com/agent-sh/tacit/client/internal/digest"
	"github.com/agent-sh/tacit/client/internal/fact"
	"github.com/agent-sh/tacit/client/internal/index"
	"github.com/agent-sh/tacit/client/internal/secretscan"
	"github.com/agent-sh/tacit/client/internal/tree"
)

// Every write into the store is masked before it is staged (SPEC.md, "Every
// write is masked"): the prose a commit adds to the files it names goes
// through scrump's text profile, whoever wrote it and whatever text it copied.
// In a Markdown file (a fact, a note, a record, an overview, a review file)
// that is the description when the write changed it and the runs of lines
// HEAD does not hold, each with the line before it as context; in a JSONL
// ledger, the string values of the lines it adds that are not names, paths,
// sessions, days or hashes. The 2026-10-26 rebuild's curator split an
// imported memory whose body held a loopback llama-server key into a curated
// fact: the note had come in through a path that did not mask it, and nothing
// between the note and the push masked what the curator copied. A write whose
// redaction cannot run is refused before anything is staged (fail closed).
//
// What the mask leaves: what HEAD already holds (a write masks what it adds;
// it does not clean the store), the frontmatter but a description (names,
// scopes, sessions and hashes: on the author's store the text profile took
// 113 session ids, 14 origins and 5 names for secrets), a [[link]]'s target
// when it is a fact's name, the owner's recorded words (owner_quote,
// owner_words, a ledger's quote and words: theirs, as their text in tacit app
// is), and the files tacit generates from the facts (written again from the
// masked text, Reindex).

// Redact masks secret-shaped text in several texts at once, each as it would
// be alone, returning each at its length (digest.RedactAllContext: scrump's
// text profile, records mode). Tests replace it.
var Redact = digest.RedactAllContext

// Reindex writes again the indexes tacit generates from the facts (MEMORY.md
// and the folder indexes), from the facts and records as the commit of rels
// will hold them, and returns the files it wrote, which join the commit. A
// write that masked a fact or a record calls it with always set, so the
// indexes hold the text the commit holds (with none set such a write is
// refused); a write that names an index calls it without, and it writes
// nothing when the clone holds no fact or record the commit does not carry.
// cmd/tacit sets it.
var Reindex func(ctx context.Context, dir string, rels []string, always bool) ([]string, error)

// ErrRedaction is a write refused because what it adds could not be
// masked: nothing was staged or committed.
var ErrRedaction = errors.New("redaction could not run, so nothing was written")

// canary rides with every batch the mask sends (digest.RedactionReady's
// canary, built from pieces so no secret-shaped literal sits in the source):
// a redactor that runs but masks nothing (a scrump older than 0.2.0, whose
// records probe fails and whose joined fallback masks nothing) answers it
// unmasked, and the write is refused. It costs no second scrump start.
var canary = "pass" + "word=" + "Zq7Vx9" + "Lm2Kp4"

// canaryMasked reports that the redactor masked the canary.
func canaryMasked(red string) bool {
	return len(red) == len(canary) && strings.Contains(red, "****") && !strings.Contains(red, "Zq7Vx9")
}

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

// maskedKind is how the mask reads a file: "md" a Markdown file tacit does
// not generate, "jsonl" a ledger, "" a file it leaves (the indexes, the merge
// rules, a JSON marker, which holds no prose).
func maskedKind(rel string) string {
	switch {
	case rel == index.File || path.Base(rel) == tree.FileName || rel == ".gitattributes":
		return ""
	case strings.HasSuffix(rel, ".md"):
		return "md"
	case strings.HasSuffix(rel, ".jsonl"):
		return "jsonl"
	}
	return ""
}

// maskPlan is one file's texts for the redactor and how to put the masked
// texts back.
type maskPlan struct {
	texts []string
	apply func(red []string) (string, error)
}

// maskAdded masks what a write adds to the files it names and writes back
// each file whose text the mask changed, returning them.
func (s *Store) maskAdded(ctx context.Context, rels []string) ([]string, error) {
	type file struct {
		rel, kind, now string
		perm           os.FileMode
		plan           maskPlan
		first          int
	}
	var files []*file
	seen := map[string]bool{}
	for _, rel := range rels {
		kind := maskedKind(rel)
		if seen[rel] || kind == "" {
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
			// The redactor reads text; a file it cannot read is not written.
			return nil, fmt.Errorf("%w: %s holds a NUL byte", ErrRedaction, rel)
		}
		files = append(files, &file{rel: rel, kind: kind, now: string(b), perm: st.Mode().Perm()})
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
		if f.kind == "jsonl" {
			f.plan = planJSONL(old[f.rel], f.now)
		} else {
			f.plan = planMarkdown(old[f.rel], f.now)
		}
		f.first = len(texts)
		texts = append(texts, f.plan.texts...)
	}
	if len(texts) == 0 {
		return nil, nil
	}
	red, err := Redact(ctx, append(texts[:len(texts):len(texts)], canary))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRedaction, err)
	}
	if len(red) != len(texts)+1 {
		return nil, fmt.Errorf("%w: the redactor answered %d texts for %d", ErrRedaction, len(red), len(texts)+1)
	}
	if !canaryMasked(red[len(texts)]) {
		return nil, fmt.Errorf("%w: the redactor runs but did not mask its canary; install scrump 0.2.0 or later (docs/portable.md, Redaction)", ErrRedaction)
	}
	red = red[:len(texts)]
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

// unchanged reports that none of rels differs from HEAD: what the write
// wanted is in the store already (the mask took back all it changed).
func (s *Store) unchanged(ctx context.Context, rels []string) bool {
	out, err := git(ctx, s.Dir, append([]string{"status", "--porcelain", "-z", "--untracked-files=all", "--"}, rels...)...)
	return err == nil && strings.TrimSpace(out) == ""
}

// frontmatter is what the mask reads of a file's frontmatter: its
// description, its commands (a fact's applies_to.commands, a record's
// commands: an argv can carry a key, and the curator copies a split's
// commands as the model gave them) and the owner's recorded words.
type frontmatter struct {
	Description string `yaml:"description"`
	AppliesTo   struct {
		Commands []string `yaml:"commands"`
	} `yaml:"applies_to"`
	Commands []string `yaml:"commands"`
	Source   struct {
		OwnerQuote string `yaml:"owner_quote"`
		OwnerWords string `yaml:"owner_words"`
	} `yaml:"source"`
}

// planMarkdown plans the mask of one Markdown file: with frontmatter that
// reads, its description when the write changed it, the commands the write
// adds (each alone) and the runs of body lines HEAD does not hold; otherwise
// the runs of added lines of the whole text. A
// link's target that is a fact's name and the owner's recorded words are left
// as they are.
func planMarkdown(old, now string) maskPlan {
	fm, body, ok := fact.Split([]byte(now))
	var meta frontmatter
	if !ok || yaml.Unmarshal(fm, &meta) != nil {
		// No frontmatter, or one that does not read: the whole text is prose.
		return planProse(old, now, protected(now, nil))
	}
	oldBody, oldDesc := old, ""
	var om frontmatter
	if ofm, ob, ok := fact.Split([]byte(old)); ok {
		oldBody = string(ob)
		if yaml.Unmarshal(ofm, &om) == nil {
			oldDesc = om.Description
		}
	}
	words := phrases(meta.Source.OwnerQuote, meta.Source.OwnerWords)
	head := now[:len(now)-len(body)]
	bp := planProse(oldBody, string(body), protected(string(body), words))
	desc := meta.Description != "" && meta.Description != oldDesc
	p := maskPlan{texts: append([]string(nil), bp.texts...)}
	if desc {
		p.texts = append(p.texts, meta.Description)
	}
	// The commands the write adds, each alone: a fact's under applies_to, a
	// record's at the top.
	lists := []*cmdList{
		{key: "applies_to", now: meta.AppliesTo.Commands, old: om.AppliesTo.Commands},
		{key: "commands", now: meta.Commands, old: om.Commands},
	}
	for _, l := range lists {
		l.first = len(p.texts)
		l.added = addedEntries(l.old, l.now)
		for _, i := range l.added {
			p.texts = append(p.texts, l.now[i])
		}
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
		for _, l := range lists {
			masked, changed := append([]string(nil), l.now...), false
			for j, i := range l.added {
				if red[l.first+j] != masked[i] {
					masked[i], changed = red[l.first+j], true
				}
			}
			if !changed {
				continue
			}
			var value any = masked
			if l.key == "applies_to" {
				// The whole block is written again, its other lists as they were.
				fmb, _, _ := fact.Split([]byte(out))
				var all struct {
					AppliesTo fact.AppliesTo `yaml:"applies_to"`
				}
				if err := yaml.Unmarshal(fmb, &all); err != nil {
					return "", err
				}
				all.AppliesTo.Commands = masked
				value = all.AppliesTo
			}
			patched, err := fact.PatchField([]byte(out), l.key, value)
			if err != nil {
				return "", err
			}
			out = string(patched)
		}
		return out, nil
	}
	return p
}

// cmdList is one list of commands in a file's frontmatter, as the mask plans
// it: the entries the write adds (by index in now) and where their texts
// start in the plan.
type cmdList struct {
	key      string
	now, old []string
	added    []int
	first    int
}

// addedEntries is the indexes of the entries of now that old does not hold
// (as many times), empty ones aside.
func addedEntries(old, now []string) []int {
	held := map[string]int{}
	for _, e := range old {
		held[e]++
	}
	var out []int
	for i, e := range now {
		if held[e] > 0 {
			held[e]--
			continue
		}
		if e != "" {
			out = append(out, i)
		}
	}
	return out
}

// planProse plans the mask of the runs of lines of now that old does not
// hold, each sent with the line before it as context (a keyword there masks
// the token that starts the run) and only the run written back; spans are
// byte ranges of now the mask leaves as they are.
func planProse(old, now string, spans [][2]int) maskPlan {
	lines := splitLines(now)
	runs := addedRuns(splitLines(old), lines)
	at := make([]int, len(lines)+1)
	for i, l := range lines {
		at[i+1] = at[i] + len(l)
	}
	ctxLen := make([]int, len(runs))
	p := maskPlan{}
	for i, r := range runs {
		from := at[r[0]]
		if r[0] > 0 {
			from = at[r[0]-1]
		}
		ctxLen[i] = at[r[0]] - from
		p.texts = append(p.texts, now[from:at[r[1]]])
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
			b.WriteString(keep(now[start:end], red[i][ctxLen[i]:], local))
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

// protected is the byte ranges of text the mask leaves: the target of each
// [[link]] that is a fact's name (lower-case letters, digits and hyphens; a
// label, a section or a target of another shape is prose), and every place
// it holds one of the owner's recorded phrases.
func protected(text string, words []string) [][2]int {
	var out [][2]int
	for _, sp := range fact.LinkSpans(text) {
		at := sp.Start + 2 // after "[["
		if strings.HasPrefix(text[at:], sp.Target) && fact.IsSlug(strings.TrimSpace(sp.Target)) {
			out = append(out, [2]int{at, at + len(sp.Target)})
		}
	}
	return append(out, wordSpans(text, words)...)
}

// wordSpans is every place text holds one of words: byte for byte, or as
// lint's check of the owner's words reads them (ownerSpans).
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
		out = append(out, ownerSpans(text, w)...)
	}
	return out
}

// ownerSpans is every place text holds words as lint's check of the owner's
// words reads them (ownerwords.In, L15): case, apostrophes, punctuation and
// line breaks aside, on word boundaries. A span runs from the first letter or
// digit of a match to the end of its last one, so a hit inside it is left and
// the check still finds the words.
func ownerSpans(text, words string) [][2]int {
	seg, _ := normalized(words)
	if seg == "" {
		return nil
	}
	norm, at := normalized(text)
	padded := " " + norm + " "
	var out [][2]int
	for from := 0; ; {
		i := strings.Index(padded[from:], " "+seg+" ")
		if i < 0 {
			break
		}
		p := from + i // padded[p] is the space before the match, norm[p] its first byte
		out = append(out, [2]int{at[p][0], at[p+len(seg)-1][1]})
		from = p + 1 + len(seg)
	}
	return out
}

// normalized is s as ownerwords.Normalize writes it (lower case, no
// apostrophes, one space for each run of other characters that are no letter
// or digit), with the bytes of s each of its bytes comes from. The store does
// not import ownerwords, which reads the harnesses' sessions and their
// databases; a test holds the two equal.
func normalized(s string) (string, [][2]int) {
	var b []byte
	var at [][2]int
	space := true
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == '\'' || r == '’' || r == '‘' || r == '`':
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			for _, c := range utf8.AppendRune(nil, unicode.ToLower(r)) {
				b = append(b, c)
				at = append(at, [2]int{i, i + n})
			}
			space = false
		default:
			if !space {
				b = append(b, ' ')
				at = append(at, [2]int{i, i + n})
				space = true
			}
		}
		i += n
	}
	for len(b) > 0 && b[len(b)-1] == ' ' {
		b, at = b[:len(b)-1], at[:len(at)-1]
	}
	return string(b), at
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

// Keys of a ledger line whose string values are names, paths, sessions,
// verdicts, days or the owner's own words, never prose the mask reads.
var ledgerIDKeys = map[string]bool{
	"id": true, "a": true, "b": true, "at": true, "by": true, "model": true, "session": true, "sessions": true,
	"from": true, "words_from": true, "owner_quote_from": true, "owner_words_from": true, "turn_hash": true,
	"hash": true, "name": true, "path": true, "origin": true, "into": true, "of": true, "also": true,
	"as": true, "statement": true, "verdict": true, "action": true, "kind": true, "doubt": true,
	"closes": true, "proof": true, "repointed": true, "said": true, "day": true, "on": true,
	"created": true, "folder": true, "scope": true, "status": true, "target": true, "to": true,
	"sha": true, "commit": true, "host": true, "harness": true, "started": true, "outcome": true,
	"quote": true, "words": true, "relevance": true, "category": true,
}

// idLike reports a string value of a ledger line that is an id by its shape
// (idValue, or a session of a known harness, sessionValue) and holds no
// family's key (secretscan): `password:<token>` and `sk-or-v1-<hex>` have an
// id's shape and are prose the mask reads.
func idLike(v string) bool {
	if secretscan.MatchString(v) {
		return false
	}
	if m := sessionValue.FindStringSubmatch(v); m != nil {
		return sessionHarness[m[1]]
	}
	return idValue.MatchString(v)
}

// sessionValue is a session or a cite, <harness>:<id>; sessionHarness the
// harnesses and sources a ledger names one by (the author's ledgers hold
// claude, codex, hermes, pi, relay and git).
var (
	sessionValue   = regexp.MustCompile(`^([a-z]+):[\w.-]+$`)
	sessionHarness = map[string]bool{"claude": true, "codex": true, "pi": true, "hermes": true, "grok": true, "muse": true, "agy": true,
		"dreamer": true, "import": true, "curator": true, "human": true, "relay": true, "git": true, "commit": true, "note": true, "rulebook": true}
)

// idValue is a string value of a shape no prose has: a fact's name, a store
// path, a day or a time, a hash (a session is sessionValue's).
var idValue = regexp.MustCompile(`^(?:[a-z0-9][a-z0-9-]*|[\w.@-]+(?:/[\w.@-]+)+\.(?:md|jsonl|json)|\d{4}-\d{2}-\d{2}(?:[T ][\d:.+Z-]+)?|[0-9a-f]{7,64})$`)

// planJSONL plans the mask of the lines of a ledger HEAD does not hold: in
// each, the string values that are prose, by their key and their shape, each
// masked alone and written back as JSON. A line that is not JSON is masked
// whole, as prose.
func planJSONL(old, now string) maskPlan {
	lines := splitLines(now)
	runs := addedRuns(splitLines(old), lines)
	type value struct {
		line, start, end int // the literal's bytes in the line
		text             string
	}
	var vals []value
	var whole []int // lines masked whole
	for _, r := range runs {
		for i := r[0]; i < r[1]; i++ {
			l := strings.TrimRight(lines[i], "\r\n")
			if strings.TrimSpace(l) == "" {
				continue
			}
			vs, ok := jsonStrings(l)
			if !ok {
				whole = append(whole, i)
				continue
			}
			for _, v := range vs {
				if !ledgerIDKeys[v.key] && !idLike(v.text) {
					vals = append(vals, value{i, v.start, v.end, v.text})
				}
			}
		}
	}
	p := maskPlan{}
	for _, v := range vals {
		p.texts = append(p.texts, v.text)
	}
	for _, i := range whole {
		p.texts = append(p.texts, lines[i])
	}
	p.apply = func(red []string) (string, error) {
		out := append([]string(nil), lines...)
		// Values right to left within a line, so earlier offsets stay true.
		for k := len(vals) - 1; k >= 0; k-- {
			v := vals[k]
			if red[k] == v.text {
				continue
			}
			lit, err := json.Marshal(red[k])
			if err != nil {
				return "", err
			}
			out[v.line] = out[v.line][:v.start] + string(lit) + out[v.line][v.end:]
		}
		for j, i := range whole {
			out[i] = red[len(vals)+j]
		}
		return strings.Join(out, ""), nil
	}
	return p
}

// jsonString is one string value of a JSON line: its key (the key of the
// object it is a value of, or of the array that holds it), its text and its
// literal's bytes.
type jsonString struct {
	key, text  string
	start, end int
}

// jsonStrings is every string value of the JSON text l with its key; ok is
// false when l is not one JSON value.
func jsonStrings(l string) ([]jsonString, bool) {
	dec := json.NewDecoder(strings.NewReader(l))
	dec.UseNumber()
	type frame struct {
		object, wantKey bool
		key             string
	}
	var stack []frame
	var out []jsonString
	for {
		before := int(dec.InputOffset())
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, false
		}
		after := int(dec.InputOffset())
		top := len(stack) - 1
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{', '[':
				key := ""
				if top >= 0 {
					key = stack[top].key
					if stack[top].object {
						stack[top].wantKey = true
					}
				}
				stack = append(stack, frame{object: t == '{', wantKey: t == '{', key: key})
			default:
				stack = stack[:top]
			}
		case string:
			if top >= 0 && stack[top].object && stack[top].wantKey {
				stack[top].key, stack[top].wantKey = t, false
				continue
			}
			q := strings.IndexByte(l[before:after], '"')
			if q < 0 {
				return nil, false
			}
			key := ""
			if top >= 0 {
				key = stack[top].key
				if stack[top].object {
					stack[top].wantKey = true
				}
			}
			out = append(out, jsonString{key: key, text: t, start: before + q, end: after})
		default:
			if top >= 0 && stack[top].object {
				stack[top].wantKey = true
			}
		}
	}
	if dec.More() {
		return nil, false
	}
	return out, true
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
// new. A line that only lost or gained its newline, or a carriage return
// before it, is old.
func addedRuns(old, now []string) [][2]int {
	have := map[string]int{}
	for _, l := range old {
		have[strings.TrimRight(l, "\r\n")]++
	}
	var runs [][2]int
	start := -1
	for i, l := range now {
		key := strings.TrimRight(l, "\r\n")
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
// show: a fact or a record (not an inbox note, not a review file, not a
// ledger).
func touchesIndexes(rels []string) bool {
	for _, rel := range rels {
		if strings.HasSuffix(rel, ".md") && !strings.HasPrefix(rel, "inbox/") && !strings.HasPrefix(rel, "review/") {
			return true
		}
	}
	return false
}

// namesIndex reports a commit that names an index tacit generates: MEMORY.md
// or a folder's _index.md.
func namesIndex(rels []string) bool {
	for _, rel := range rels {
		if rel == index.File || path.Base(rel) == tree.FileName {
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
