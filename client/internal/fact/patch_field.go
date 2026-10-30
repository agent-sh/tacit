package fact

import (
	"errors"
	"reflect"
	"strings"

	"go.yaml.in/yaml/v3"
)

// PatchField rewrites only one top-level key's block of a file with
// frontmatter to value, leaving every other byte as it was: the store's mask
// uses it on commands it masked (a fact's applies_to, a record's commands;
// internal/store). The frontmatter must read back the same but for key, and
// key as value.
func PatchField(data []byte, key string, value any) ([]byte, error) {
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
	if err := enc.Encode(map[string]any{key: value}); err != nil {
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
		if strings.HasPrefix(l, key+":") {
			start, end = i, i+1
			for end < len(lines) && (strings.HasPrefix(lines[end], " ") || strings.HasPrefix(lines[end], "- ") || strings.TrimSpace(lines[end]) == "") {
				end++
			}
			break
		}
	}
	if start < 0 {
		return nil, errors.New("no " + key + " block")
	}
	patched := strings.Join(append(append(append([]string{}, lines[:start]...), block), lines[end:]...), "")
	var before, after, want map[string]any
	if err := yaml.Unmarshal(fm, &before); err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal([]byte(patched), &after); err != nil {
		return nil, err
	}
	if err := yaml.Unmarshal([]byte(buf.String()), &want); err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(after[key], want[key]) {
		return nil, errors.New("patched " + key + " does not read back")
	}
	delete(before, key)
	delete(after, key)
	if !reflect.DeepEqual(before, after) {
		return nil, errors.New("patching " + key + " changed another field")
	}
	return []byte(opener + patched + closer + string(body)), nil
}
