package importer

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/agent-sh/tacit/client/internal/fact"
)

// claudeMeta is the subset of Claude Code auto-memory frontmatter the import
// uses. Claude's own `name` field is not used: links resolve by file stem.
type claudeMeta struct {
	Description string `yaml:"description"`
	Metadata    struct {
		Type            string `yaml:"type"`
		OriginSessionID string `yaml:"originSessionId"`
		Modified        string `yaml:"modified"`
	} `yaml:"metadata"`
}

// Claude imports one or more Claude Code auto-memory directories
// (<config>/projects/<project>/memory). The project directory name gives the
// scope guess and the inbox subdirectory.
func Claude(o Options, dirs ...string) Result {
	var r Result
	if err := o.Validate(); err != nil {
		r.Errors = append(r.Errors, err)
		return r
	}
	for _, dir := range dirs {
		abs, err := filepath.Abs(dir)
		if err != nil {
			r.Errors = append(r.Errors, err)
			continue
		}
		project := filepath.Base(filepath.Dir(abs)) // .../projects/<project>/memory
		scope := o.scopeFromClaudeProject(project)
		entries, err := os.ReadDir(abs)
		if err != nil {
			r.Errors = append(r.Errors, err)
			continue
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			if e.IsDir() || e.Type()&os.ModeSymlink != 0 || !strings.HasSuffix(e.Name(), ".md") || e.Name() == "MEMORY.md" {
				r.Skipped++
				continue
			}
			names = append(names, e.Name())
		}
		sort.Strings(names)
		// Two stems can slug to the same name (foo_bar.md and foo-bar.md).
		// The first keeps the plain slug, later ones get a hash suffix, so
		// nothing is silently overwritten and re-imports stay stable.
		taken := map[string]bool{}
		for _, name := range names {
			src := filepath.Join(abs, name)
			data, err := os.ReadFile(src)
			if err != nil {
				r.Errors = append(r.Errors, err)
				continue
			}
			stem := strings.TrimSuffix(name, ".md")
			slug := Slug(stem, 80)
			if taken[slug] {
				slug = Slug(stem, 70) + "-" + shortHash(stem)
			}
			taken[slug] = true
			f, err := claudeFact(o, src, data, scope, slug)
			if err != nil {
				r.Errors = append(r.Errors, fmt.Errorf("%s: %w", src, err))
				continue
			}
			dst, err := o.inbox(project, slug+".md")
			if err != nil {
				r.Errors = append(r.Errors, err)
				continue
			}
			writeFact(dst, f, &r)
		}
	}
	return r
}

func claudeFact(o Options, src string, data []byte, scope, name string) (*fact.Fact, error) {
	var cm claudeMeta
	fm, body, ok := fact.Split(data)
	if ok {
		// Claude's own frontmatter is not strict and not always valid YAML
		// (unquoted descriptions containing ": "). Decode what we know; on a
		// YAML error fall back to line-based extraction of the fields we use.
		if err := fact.DecodeLoose(fm, &cm); err != nil {
			cm = claudeMetaLoose(fm)
		}
	} else {
		body = data
	}
	body = []byte(rewriteLinks(string(body)))
	desc := strings.TrimSpace(cm.Description)
	if desc == "" {
		desc = firstLine(string(body))
	}
	desc = clip(desc, 200)
	created := ""
	if len(cm.Metadata.Modified) >= 10 && fact.IsDate(cm.Metadata.Modified[:10]) {
		created = cm.Metadata.Modified[:10]
	} else if st, err := os.Stat(src); err == nil {
		created = st.ModTime().Format("2006-01-02")
	} else {
		created = today()
	}
	if strings.TrimSpace(string(body)) == "" {
		body = []byte(desc + "\n")
	}
	f := &fact.Fact{
		Meta: fact.Meta{
			Name:        name,
			Description: desc,
			Type:        "unclassified",
			Scope:       scope,
			Source: fact.Source{
				Harness:      "import",
				Host:         o.Host,
				Session:      cm.Metadata.OriginSessionID,
				Kind:         "observed",
				ImportedFrom: o.tilde(src),
				OriginalType: cm.Metadata.Type,
			},
			Created: created,
		},
		Body: string(body),
	}
	if cm.Metadata.Type == "feedback" {
		// A feedback memory is the agent's note of what the owner said; it is their
		// ruling only with their words in it.
		o.ownerKind(&f.Meta.Source, f.Meta.Name, desc, desc+"\n"+string(body))
	}
	return f, nil
}

// claudeMetaLoose extracts description and metadata.* from frontmatter that
// does not parse as YAML. A value is the text after the first ":"; matching
// surrounding quotes are removed; a block scalar (`|` or `>`) takes the
// following indented lines.
func claudeMetaLoose(fm []byte) claudeMeta {
	var cm claudeMeta
	lines := strings.Split(string(fm), "\n")
	for i := 0; i < len(lines); i++ {
		key, val, ok := strings.Cut(lines[i], ":")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		if v := strings.TrimRight(val, "-+"); v == "|" || v == ">" {
			var block []string
			for i+1 < len(lines) && (strings.HasPrefix(lines[i+1], " ") || lines[i+1] == "") {
				i++
				block = append(block, strings.TrimSpace(lines[i]))
			}
			val = strings.TrimSpace(strings.Join(block, " "))
		} else if n := len(val); n >= 2 && (val[0] == '"' && val[n-1] == '"' || val[0] == '\'' && val[n-1] == '\'') {
			val = val[1 : n-1]
		}
		switch strings.TrimSpace(key) {
		case "description":
			cm.Description = val
		case "type":
			cm.Metadata.Type = val
		case "originSessionId":
			cm.Metadata.OriginSessionID = val
		case "modified":
			cm.Metadata.Modified = val
		}
	}
	return cm
}

var (
	linkRe   = regexp.MustCompile(`\[\[([^\]\n|#]+)((?:[|#][^\]\n]*)?)\]\]`)
	linkName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)
)

// rewriteLinks slugs [[link]] targets the same way import slugs file stems,
// so a link to `feedback_no_tool_sprawl` resolves once that file is imported
// as `feedback-no-tool-sprawl`. Only link-shaped targets are touched: bash
// `[[ $n == 0 ]]`, `df[["a","b"]]` and `[[ ]]` are left alone.
func rewriteLinks(body string) string {
	return linkRe.ReplaceAllStringFunc(body, func(m string) string {
		sub := linkRe.FindStringSubmatch(m)
		target := strings.TrimSpace(sub[1])
		if target != sub[1] || !linkName.MatchString(target) {
			return m
		}
		return "[[" + Slug(target, 80) + sub[2] + "]]"
	})
}

func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(strings.TrimLeft(l, "# "))
		if l != "" {
			return l
		}
	}
	return "imported fact"
}

func clip(s string, n int) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n-1]) + "…"
}
