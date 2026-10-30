package fact

import (
	"errors"
	"reflect"
	"strings"

	"go.yaml.in/yaml/v3"
)

// PatchDescription rewrites only the description of a file with frontmatter
// (a fact, an inbox note, a record), leaving every other byte as it was: the
// store's mask uses it on a description it masked (internal/store). The
// frontmatter must read back the same but for the description.
func PatchDescription(data []byte, desc string) ([]byte, error) {
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
		Description string `yaml:"description"`
	}{desc}); err != nil {
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
		if strings.HasPrefix(l, "description:") {
			start, end = i, i+1
			for end < len(lines) && (strings.HasPrefix(lines[end], " ") || strings.TrimSpace(lines[end]) == "") {
				end++
			}
			break
		}
	}
	if start < 0 {
		return nil, errors.New("no description line")
	}
	patched := strings.Join(append(append(append([]string{}, lines[:start]...), block), lines[end:]...), "")
	var before, after map[string]any
	if err := yaml.Unmarshal(fm, &before); err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal([]byte(patched), &after); err != nil {
		return nil, err
	}
	if got, _ := after["description"].(string); got != desc {
		return nil, errors.New("patched description does not read back")
	}
	delete(before, "description")
	delete(after, "description")
	if !reflect.DeepEqual(before, after) {
		return nil, errors.New("patching the description changed another field")
	}
	return []byte(opener + patched + closer + string(body)), nil
}
