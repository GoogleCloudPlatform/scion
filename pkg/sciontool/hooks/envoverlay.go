/*
Copyright 2026 The Scion Authors.
*/

package hooks

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// maxEnvOverlayBytes caps the env overlay file size to prevent abuse from
// a misbehaving harness script.
const maxEnvOverlayBytes = 1 << 20 // 1 MiB

// maxEnvSecretFileBytes caps the size of any from_file referent. Secret
// files are env values, not arbitrary blobs; legitimate API keys and tokens
// are well under 64 KiB.
const maxEnvSecretFileBytes = 64 * 1024

// LoadEnvOverlay reads the env overlay JSON written by a container-script
// harness's pre-start provisioner and returns the resolved key/value pairs.
//
// The overlay schema accepts two forms per key:
//
//	"KEY": "value"                              -> direct string
//	"KEY": { "from_file": "/path/to/secret" }   -> file content (trimmed)
//
// from_file paths must live inside one of the allowedRoots so a misbehaving
// script cannot exfiltrate arbitrary files into the child environment.
//
// Returns (nil, nil) if path does not exist (the file is optional). Returns
// an error on malformed JSON, oversized payloads, escaping paths, or missing
// from_file referents — the caller is expected to fail startup when those
// errors arise for a required overlay.
func LoadEnvOverlay(path string, allowedRoots []string) (map[string]string, error) {
	if path == "" {
		return nil, nil
	}
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("stat env overlay %s: %w", path, err)
	}
	if info.Size() > maxEnvOverlayBytes {
		return nil, fmt.Errorf("env overlay %s exceeds %d bytes", path, maxEnvOverlayBytes)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read env overlay %s: %w", path, err)
	}

	// Decode as map[string]json.RawMessage so we can switch between string
	// and object form per entry without a custom UnmarshalJSON for every key.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse env overlay %s: %w", path, err)
	}

	out := make(map[string]string, len(raw))
	for key, rawVal := range raw {
		if key == "" {
			return nil, fmt.Errorf("env overlay %s: empty key", path)
		}
		if !validEnvKey(key) {
			return nil, fmt.Errorf("env overlay %s: invalid key %q", path, key)
		}
		val, err := resolveEnvValue(rawVal, allowedRoots)
		if err != nil {
			return nil, fmt.Errorf("env overlay %s key %s: %w", path, key, err)
		}
		out[key] = val
	}
	return out, nil
}

// resolveEnvValue interprets one entry of the env overlay JSON.
func resolveEnvValue(raw json.RawMessage, allowedRoots []string) (string, error) {
	// Try string form first.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}

	// Object form: {"from_file": "<path>"} (the only object form supported).
	var obj map[string]string
	if err := json.Unmarshal(raw, &obj); err != nil {
		return "", fmt.Errorf("value is not a string or object: %w", err)
	}
	from, ok := obj["from_file"]
	if !ok || from == "" {
		return "", fmt.Errorf("object form must contain a non-empty from_file")
	}

	cleaned, err := filepath.Abs(from)
	if err != nil {
		return "", fmt.Errorf("resolve from_file %q: %w", from, err)
	}
	if !pathInAnyRoot(cleaned, allowedRoots) {
		return "", fmt.Errorf("from_file %q escapes allowed roots %v", cleaned, allowedRoots)
	}

	info, err := os.Stat(cleaned)
	if err != nil {
		return "", fmt.Errorf("from_file %q not found: %w", cleaned, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("from_file %q is a directory", cleaned)
	}
	if info.Size() > maxEnvSecretFileBytes {
		return "", fmt.Errorf("from_file %q exceeds %d bytes", cleaned, maxEnvSecretFileBytes)
	}

	content, err := os.ReadFile(cleaned)
	if err != nil {
		return "", fmt.Errorf("read from_file %q: %w", cleaned, err)
	}
	// Trim trailing whitespace; tokens written via shell heredoc/echo often
	// pick up a trailing newline that breaks Bearer-token comparisons.
	return strings.TrimRight(string(content), "\r\n \t"), nil
}

// MergeEnvOverlay merges overlay values into env and returns the result.
// existing entries in env (the runtime environment) win on conflict so that
// CLI-provided env vars cannot be silently masked by a harness script.
//
// Precedence:
//
//	CLI/runtime env  >  generated harness overlay
//
// env is in the same KEY=VALUE form used by exec.Cmd.Env / os.Environ.
func MergeEnvOverlay(env []string, overlay map[string]string) []string {
	if len(overlay) == 0 {
		return env
	}

	existing := make(map[string]struct{}, len(env))
	for _, e := range env {
		if i := strings.IndexByte(e, '='); i > 0 {
			existing[e[:i]] = struct{}{}
		}
	}

	// Append overlay entries that don't conflict, in deterministic order.
	keys := make([]string, 0, len(overlay))
	for k := range overlay {
		keys = append(keys, k)
	}
	// Simple insertion sort: overlays are small.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}

	for _, k := range keys {
		if _, taken := existing[k]; taken {
			continue
		}
		env = append(env, k+"="+overlay[k])
	}
	return env
}

// validEnvKey enforces a conservative env-key syntax: alpha or underscore
// followed by alnum/underscore. This prevents a malicious overlay from
// emitting weird keys like "FOO=bar" or names with shell metacharacters.
func validEnvKey(k string) bool {
	for i, r := range k {
		if i == 0 {
			if !(r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')) {
				return false
			}
			continue
		}
		if !(r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return len(k) > 0
}

func pathInAnyRoot(path string, roots []string) bool {
	if len(roots) == 0 {
		return true
	}
	for _, root := range roots {
		if root == "" {
			continue
		}
		abs, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		// Use Rel to avoid prefix-mismatch (e.g. /foo vs /foobar).
		rel, err := filepath.Rel(abs, path)
		if err != nil {
			continue
		}
		if rel == "." || (!strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)) {
			return true
		}
	}
	return false
}
