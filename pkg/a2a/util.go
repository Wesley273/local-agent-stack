package a2a

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"
)

// cryptoRead fills b with cryptographically random bytes.
func cryptoRead(b []byte) (int, error) { return rand.Read(b) }

// ParseJSON is a tolerant helper that decodes into a map.
func ParseJSON(b []byte) (map[string]any, error) {
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// CloneMap shallow-copies a string-keyed map.
func CloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// MustJSON is JSONLine that never fails loudly (used in logging paths).
func MustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%q", fmt.Sprint(v))
	}
	return string(b)
}

// NormalizePath strips a leading "/a2a/{agentId}" and optional "/v1".
// Returns (agentId, restPath, ok). Used by the sidecar's single-prefix handler.
func NormalizePath(path string) (string, string, bool) {
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	p := strings.TrimPrefix(path, "/")
	if !strings.HasPrefix(p, "a2a/") {
		return "", "", false
	}
	rest := strings.TrimPrefix(p, "a2a/")
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		// "/a2a/{agentId}" — JSON-RPC endpoint with agentId in path
		return rest, "", true
	}
	agentID := rest[:slash]
	rest = rest[slash:] // begins with "/"
	rest = strings.TrimPrefix(rest, "/v1")
	return agentID, rest, true
}
