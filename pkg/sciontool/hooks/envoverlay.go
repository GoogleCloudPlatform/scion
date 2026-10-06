/*
Copyright 2026 The Scion Authors.
*/

package hooks

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/dirfd"
)

// maxEnvOverlayBytes caps the env overlay file size to prevent abuse from
// a misbehaving harness script.
const maxEnvOverlayBytes = 1 << 20 // 1 MiB

// maxEnvSecretFileBytes caps the size of any from_file referent. Secret
// files are env values, not arbitrary blobs; legitimate API keys and tokens
// are well under 64 KiB.
const maxEnvSecretFileBytes = 64 * 1024

// NativeTelemetryPolicyKey is emitted by provisioners that configure native
// telemetry. It is a runtime contract, not an environment variable for the child.
const NativeTelemetryPolicyKey = "SCION_NATIVE_TELEMETRY_POLICY"

// ValidateNativeTelemetryEnv rejects runtime values that could change a
// provisioner's native telemetry policy. The generated overlay is the complete
// set of permitted reserved keys; this also rejects alternate endpoint aliases.
// Errors name keys only, never values.
func ValidateNativeTelemetryEnv(policy string, env []string, overlay, overrides map[string]string) error {
	if policy != "enabled" && policy != "disabled" {
		return fmt.Errorf("invalid native telemetry policy")
	}
	for _, entry := range env {
		i := strings.IndexByte(entry, '=')
		if i > 0 && entry[:i] == NativeTelemetryPolicyKey {
			return fmt.Errorf("native telemetry policy conflict: %s", NativeTelemetryPolicyKey)
		}
		if i <= 0 || (!IsReservedNativeTelemetryKey(entry[:i]) && (entry[:i] != "CODEX_HOME" || overlay["CODEX_HOME"] == "")) {
			continue
		}
		key := entry[:i]
		want, ok := overlay[key]
		if !ok || entry[i+1:] != want {
			return fmt.Errorf("native telemetry policy conflict: %s", key)
		}
	}
	keys := make([]string, 0, len(overrides))
	for key := range overrides {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := overrides[key]
		if key == NativeTelemetryPolicyKey {
			return fmt.Errorf("native telemetry policy conflict: %s", key)
		}
		if !IsReservedNativeTelemetryKey(key) && (key != "CODEX_HOME" || overlay["CODEX_HOME"] == "") {
			continue
		}
		want, ok := overlay[key]
		if !ok || value != want {
			return fmt.Errorf("native telemetry policy conflict: %s", key)
		}
	}
	return nil
}

// MergeEnvOverlayWithNativeTelemetryPolicy validates protected keys before
// applying the ordinary additive merge. Secret overrides are checked here
// even though the caller applies them after this merge.
func MergeEnvOverlayWithNativeTelemetryPolicy(policy string, env []string, overlay, overrides map[string]string) ([]string, error) {
	if err := ValidateNativeTelemetryEnv(policy, env, overlay, overrides); err != nil {
		return nil, err
	}
	return MergeEnvOverlay(env, overlay), nil
}

// IsReservedNativeTelemetryKey reports whether key is reserved by the
// native telemetry policy, so an inherited or secret value for it must match
// the generated overlay (see ValidateNativeTelemetryEnv).
//
// OTEL_ keys can change SDK behavior, including OTEL_SDK_DISABLED. Under an
// active policy, unknown inherited aliases are rejected rather than allowed
// to silently change the generated configuration. COPILOT_OTEL_* and
// GROK_TELEMETRY_*/GROK_EXTERNAL_OTEL are the copilot/grok-build equivalents
// of CLAUDE_CODE_ENABLE_TELEMETRY and GEMINI_TELEMETRY_*: they switch native
// telemetry on/off and pick the exporter, so they get the same protection
// (harnesses/copilot/provision.py, harnesses/grok-build/provision.py).
//
// It is exported so tests outside this package (pkg/sciontool/supervisor's
// native telemetry env tests) clear exactly the keys the policy inspects.
// CODEX_HOME is not included: ValidateNativeTelemetryEnv protects it only
// when the overlay sets it.
func IsReservedNativeTelemetryKey(key string) bool {
	if key == "CLAUDE_CODE_ENABLE_TELEMETRY" || key == "GROK_EXTERNAL_OTEL" {
		return true
	}
	if strings.HasPrefix(key, "GEMINI_TELEMETRY_") || strings.HasPrefix(key, "COPILOT_OTEL_") || strings.HasPrefix(key, "GROK_TELEMETRY_") {
		return true
	}
	if strings.HasPrefix(key, "OTEL_") {
		return true
	}
	return false
}

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
	// This overlay file is written by a pre-start provisioner running as
	// the workload user, into a directory the workload owns outright, and
	// is then read back here — potentially by root, if a future caller
	// moves overlay loading earlier. dirfd.ReadFileNoFollow refuses a
	// symlink at any component, requires a single-link regular file, and
	// bounds the read; a separate os.Stat-then-os.ReadFile would be a
	// TOCTOU (the file could change between the two calls), and
	// os.ReadFile follows symlinks unconditionally.
	data, err := dirfd.ReadFileNoFollow(path, maxEnvOverlayBytes)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		if errors.Is(err, dirfd.ErrTooLarge) {
			return nil, fmt.Errorf("env overlay %s exceeds %d bytes: %w", path, maxEnvOverlayBytes, err)
		}
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

	content, err := readFromFileNoFollow(cleaned, allowedRoots)
	if err != nil {
		return "", err
	}
	// Trim trailing whitespace; tokens written via shell heredoc/echo often
	// pick up a trailing newline that breaks Bearer-token comparisons.
	return strings.TrimRight(string(content), "\r\n \t"), nil
}

// readFromFileNoFollow reads a from_file referent through the same
// fd-anchored, no-follow, bounded primitives used everywhere else in this
// package. Containment is enforced by the fd walk, not by the path's
// textual form, which a symlink defeats: a symlink whose own name sits
// inside an allowed root but whose target does not would pass a
// filepath.Abs + string-prefix check yet still resolve outside it.
//
// If allowedRoots is empty there is no containment policy to enforce —
// matching pathInAnyRoot's historical "no roots configured" behaviour, used
// only by tests — and cleaned is read directly (dirfd.ReadFileNoFollow,
// which refuses every symlink outright: with no root there is nothing to
// prove a target stays inside). Otherwise cleaned must resolve to inside
// one of allowedRoots, which dirfd.ReadUnderRootNoFollow verifies by
// walking an openat(O_NOFOLLOW) fd chain down from that root rather than by
// comparing path strings, so containment itself becomes symlink-safe.
//
// A symlink cleaned resolves through — at any component, including
// cleaned's own leaf — is followed, not refused, as long as
// ReadUnderRootNoFollow can prove its target stays under that same root
// through the fd-anchored walk itself. This is required, not optional: a
// Kubernetes projected-secret volume's key files are exactly this shape
// ("key" -> "..data/key", "..data" -> a timestamped sibling directory), so
// refusing every symlink unconditionally broke from_file for every secret
// mounted that way. A symlink whose name sits under allowedRoots but whose
// real target does not is still refused; see ReadUnderRootNoFollow's doc
// comment for exactly what "prove" means here and why a textual containment
// check is not enough. There is deliberately no separate stat anywhere in
// this path: the file is fstat'd and read exactly once, from the fd the
// walk finally verifies.
//
// Error messages preserve their pre-existing shapes ("not found", "escapes
// allowed roots", "exceeds N bytes") so callers and tests that key off
// those substrings keep working; only the mechanism producing them changed.
func readFromFileNoFollow(cleaned string, allowedRoots []string) ([]byte, error) {
	if len(allowedRoots) == 0 {
		data, err := dirfd.ReadFileNoFollow(cleaned, maxEnvSecretFileBytes)
		if err != nil {
			return nil, wrapFromFileErr(cleaned, err)
		}
		return data, nil
	}

	lastErr := dirfd.ErrPathEscapesRoot
	for _, root := range allowedRoots {
		if root == "" {
			continue
		}
		data, err := dirfd.ReadUnderRootNoFollow(root, cleaned, maxEnvSecretFileBytes)
		if err == nil {
			return data, nil
		}
		if errors.Is(err, dirfd.ErrPathEscapesRoot) {
			lastErr = err
			continue
		}
		return nil, wrapFromFileErr(cleaned, err)
	}
	return nil, fmt.Errorf("from_file %q escapes allowed roots %v: %w", cleaned, allowedRoots, lastErr)
}

// wrapFromFileErr translates a dirfd sentinel error into the from_file
// error message shape callers already depend on.
func wrapFromFileErr(cleaned string, err error) error {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("from_file %q not found: %w", cleaned, err)
	case errors.Is(err, dirfd.ErrTooLarge):
		return fmt.Errorf("from_file %q exceeds %d bytes: %w", cleaned, maxEnvSecretFileBytes, err)
	case errors.Is(err, dirfd.ErrNotSingleLinkRegular):
		return fmt.Errorf("from_file %q is not a plain file: %w", cleaned, err)
	default:
		return fmt.Errorf("read from_file %q: %w", cleaned, err)
	}
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
			if r != '_' && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') {
				return false
			}
			continue
		}
		if r != '_' && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return len(k) > 0
}
