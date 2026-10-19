package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// claudeMCPName is the server name in Claude Code's config; the tools reach
// the model as mcp__tacit__tacit_search, mcp__tacit__tacit_fact and
// mcp__tacit__tacit_add.
const claudeMCPName = "tacit"

// claudeUserConfig is the file Claude Code reads user-scope MCP servers from
// (`claude mcp add -s user` writes there): ~/.claude.json, top-level
// "mcpServers".
func claudeUserConfig(home string) string { return filepath.Join(home, ".claude.json") }

// claudeConfigBackup is the copy of the config as it was before tacit's last
// change to it.
func claudeConfigBackup(path string) string { return path + ".tacit-backup" }

// jsonField is one member of a JSON object, its value as written.
type jsonField struct {
	key string
	val json.RawMessage
}

// readJSONObject reads a top-level JSON object in file order, every value
// byte for byte (numbers stay as written).
func readJSONObject(b []byte) ([]jsonField, error) {
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, errors.New("not a JSON object")
	}
	var out []jsonField
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := tok.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		out = append(out, jsonField{key, raw})
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("data after the JSON object")
	}
	return out, nil
}

// writeJSONObject writes the members in order, indented by two spaces as
// Claude Code writes the file.
func writeJSONObject(fs []jsonField) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString("{")
	for i, f := range fs {
		if i > 0 {
			b.WriteString(",")
		}
		var k bytes.Buffer
		enc := json.NewEncoder(&k)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(f.key); err != nil {
			return nil, err
		}
		b.WriteString("\n  " + strings.TrimSpace(k.String()) + ": ")
		if err := json.Indent(&b, f.val, "  ", "  "); err != nil {
			return nil, err
		}
	}
	if len(fs) > 0 {
		b.WriteString("\n")
	}
	b.WriteString("}\n")
	return b.Bytes(), nil
}

// ourMCPEntry reports an mcpServers entry that runs `tacit mcp`.
func ourMCPEntry(raw json.RawMessage) bool {
	var e struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
	}
	if json.Unmarshal(raw, &e) != nil {
		return false
	}
	base := strings.ToLower(filepath.Base(strings.ReplaceAll(e.Command, `\`, "/")))
	return strings.HasPrefix(base, "tacit") && slices.Contains(e.Args, "mcp")
}

// setClaudeMCP registers (entry non-nil) or removes (entry nil) tacit's
// server in the mcpServers of Claude Code's config at path, keeping every
// other member and server as written. A server named tacit that does not run
// `tacit mcp` is someone else's: it is left alone and the call fails. The
// file is replaced atomically (writeFileMode) with its mode kept, after the old bytes are
// copied to claudeConfigBackup(path); nothing is written when nothing
// changes. It reports whether the file changed.
func setClaudeMCP(path string, entry map[string]any) (bool, error) {
	old, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	top, err := readJSONObject(old)
	if err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	at := -1
	var servers []jsonField
	for i, f := range top {
		if f.key == "mcpServers" {
			at = i
			if servers, err = readJSONObject(f.val); err != nil {
				return false, fmt.Errorf("%s: mcpServers: %w", path, err)
			}
		}
	}
	var kept []jsonField
	var mine json.RawMessage
	for _, s := range servers {
		if s.key != claudeMCPName {
			kept = append(kept, s)
			continue
		}
		if !ourMCPEntry(s.val) {
			return false, fmt.Errorf("%s already has an MCP server named %q that does not run `tacit mcp`; nothing written (remove it, or install with --no-mcp)", path, claudeMCPName)
		}
		mine = s.val
	}
	if entry == nil && mine == nil {
		return false, nil
	}
	if entry != nil {
		raw, _ := json.Marshal(entry)
		if mine != nil && jsonEqual(mine, raw) {
			return false, nil
		}
		// Replace ours where it is; a new one goes last.
		placed := false
		for i, s := range servers {
			if s.key == claudeMCPName {
				servers[i].val, placed = raw, true
			}
		}
		if !placed {
			servers = append(servers, jsonField{claudeMCPName, raw})
		}
		kept = servers
	}
	sv, err := writeJSONObject(kept)
	if err != nil {
		return false, err
	}
	if at >= 0 {
		top[at].val = sv
	} else {
		top = append(top, jsonField{"mcpServers", sv})
	}
	out, err := writeJSONObject(top)
	if err != nil {
		return false, err
	}
	mode := os.FileMode(0o600) // Claude Code keeps its account state here
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	if old != nil {
		if err := writeFileMode(claudeConfigBackup(path), old, mode); err != nil {
			return false, fmt.Errorf("backup: %w", err)
		}
	}
	return true, writeFileMode(path, out, mode)
}

// jsonEqual compares two JSON values by meaning.
func jsonEqual(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	xa, _ := json.Marshal(x)
	ya, _ := json.Marshal(y)
	return bytes.Equal(xa, ya)
}

// claudeMCPEntry is the stdio server Claude Code starts for tacit.
func claudeMCPEntry(exe string) map[string]any {
	var args []string
	_ = json.Unmarshal([]byte(mcpArguments("claude")), &args)
	return map[string]any{"type": "stdio", "command": exe, "args": args, "env": map[string]any{}}
}
