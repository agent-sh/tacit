package fact

import (
	"errors"
	"strings"

	"go.yaml.in/yaml/v3"
)

// PatchSource rewrites only the source block of a fact file to match f (the
// kind and the owner evidence change when a ruling is audited), leaving every
// other byte as it was.
func PatchSource(data []byte, f *Fact) ([]byte, error) {
	fm, body, ok := Split(data)
	if !ok {
		return nil, errors.New("no frontmatter block")
	}
	nl := strings.IndexByte(string(data), '\n')
	opener := string(data[:nl+1])
	closer := string(data[len(opener)+len(fm) : len(data)-len(body)])
	eol := "\n"
	if strings.HasSuffix(opener, "\r\n") {
		eol = "\r\n"
	}
	var buf strings.Builder
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(struct {
		Source Source `yaml:"source"`
	}{f.Meta.Source}); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	block := buf.String()
	if eol != "\n" {
		block = strings.ReplaceAll(block, "\n", eol)
	}
	lines := strings.SplitAfter(string(fm), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	start, end := -1, -1
	for i, l := range lines {
		if strings.HasPrefix(l, "source:") {
			start, end = i, i+1
			for end < len(lines) && strings.HasPrefix(lines[end], " ") {
				end++
			}
			break
		}
	}
	if start < 0 {
		return nil, errors.New("no source block")
	}
	lines = append(lines[:start], append([]string{block}, lines[end:]...)...)
	out := opener + strings.Join(lines, "") + closer + string(body)
	got, err := Parse(f.Path, []byte(out))
	if err != nil {
		return nil, err
	}
	if got.Meta.Source != f.Meta.Source {
		return nil, errors.New("patched source does not read back")
	}
	return []byte(out), nil
}
