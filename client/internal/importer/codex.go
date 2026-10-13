package importer

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"

	"github.com/agent-sh/tacit/client/internal/fact"
)

// Codex imports ~/.codex/memories/memory_summary.md: one fact per bullet in
// the "User preferences" and "General Tips" sections, and the "User Profile"
// paragraph as one user-scoped fact. rollout_summaries/ and raw_memories.md
// are skipped. source.original_type carries the section name.
func Codex(o Options, memoriesDir string) Result {
	var r Result
	if err := o.Validate(); err != nil {
		r.Errors = append(r.Errors, err)
		return r
	}
	src, err := filepath.Abs(filepath.Join(memoriesDir, "memory_summary.md"))
	if err != nil {
		r.Errors = append(r.Errors, err)
		return r
	}
	fh, err := os.Open(src)
	if err != nil {
		r.Errors = append(r.Errors, err)
		return r
	}
	defer fh.Close()

	created := today()
	if st, err := fh.Stat(); err == nil {
		created = st.ModTime().Format("2006-01-02")
	}
	section := ""
	var profile []string
	var bullet strings.Builder
	emit := func(name, text, section string) {
		dst, err := o.inbox("codex", name+".md")
		if err != nil {
			r.Errors = append(r.Errors, err)
			return
		}
		writeFact(dst, codexFact(o, src, name, text, section, created), &r)
	}
	flush := func() {
		text := strings.TrimSpace(bullet.String())
		bullet.Reset()
		if text == "" {
			return
		}
		emit(codexName(text), text, section)
	}
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "## "):
			flush()
			section = strings.TrimSpace(strings.TrimPrefix(line, "## "))
		case strings.HasPrefix(line, "- ") && (section == "User preferences" || section == "General Tips"):
			flush()
			bullet.WriteString(strings.TrimPrefix(line, "- "))
		case strings.HasPrefix(line, "  ") && bullet.Len() > 0:
			bullet.WriteString(" " + strings.TrimSpace(line))
		case section == "User Profile" && strings.TrimSpace(line) != "":
			profile = append(profile, strings.TrimSpace(line))
		default:
			flush()
		}
	}
	flush()
	if err := sc.Err(); err != nil {
		r.Errors = append(r.Errors, err)
	}
	if len(profile) > 0 {
		emit("codex-user-profile", strings.Join(profile, " "), "User Profile")
	}
	return r
}

func codexName(text string) string {
	words := strings.Fields(stripTag(text))
	if len(words) > 8 {
		words = words[:8]
	}
	return Slug(strings.Join(words, " "), 60) + "-" + shortHash(text)
}

// stripTag drops the trailing "[claude]"-style provenance tag Codex adds.
func stripTag(s string) string {
	if i := strings.LastIndex(s, " ["); i > 0 && strings.HasSuffix(s, "]") {
		return s[:i]
	}
	return s
}

// codexFact builds an inbox fact. The stored type is unclassified until the
// curator assigns one; source.original_type keeps the Codex section. "User
// preferences" and the profile are Codex's summary of what the owner said:
// kind user only when a quoted phrase of it is their words (Options.OwnerWords);
// "General Tips" are observations.
func codexFact(o Options, src, name, text, section, created string) *fact.Fact {
	f := &fact.Fact{
		Meta: fact.Meta{
			Name:        name,
			Description: clip(stripTag(text), 200),
			Type:        "unclassified",
			Scope:       "user",
			Source: fact.Source{
				Harness:      "import",
				Host:         o.Host,
				Kind:         "observed",
				ImportedFrom: o.tilde(src),
				OriginalType: Slug(section, 40),
			},
			Created: created,
		},
		Body: stripTag(text) + "\n",
	}
	if section == "User preferences" || section == "User Profile" {
		o.ownerKind(&f.Meta.Source, f.Meta.Name, f.Meta.Description, f.Body)
	}
	return f
}
