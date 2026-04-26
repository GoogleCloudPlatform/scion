/*
Copyright 2026 The Scion Authors.
*/

package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

var (
	provisionManifestPath string
)

// harnessCmd is the root for harness-related sciontool subcommands.
var harnessCmd = &cobra.Command{
	Use:   "harness",
	Short: "Container-side harness operations",
	Long: `harness contains container-side commands invoked by Scion lifecycle hooks.

These commands are not intended for direct use on a developer host; they run
inside the agent container as part of sciontool init's lifecycle processing.`,
}

// harnessProvisionCmd runs the staged provisioner script declared in a
// container-script harness's config.yaml. It is invoked from a trusted
// pre-start hook wrapper that scion stages into the agent home.
var harnessProvisionCmd = &cobra.Command{
	Use:   "provision",
	Short: "Run the harness provisioner script inside the agent container",
	Long: `provision validates the staged harness manifest, executes the declared
provisioner.command from config.yaml under a timeout, and validates that any
generated outputs are well-formed JSON. It refuses to run if the manifest
references paths outside $HOME/.scion/harness or the agent home.

This command is invoked by sciontool's pre-start lifecycle hook from the
trusted wrapper at $HOME/.scion/hooks/pre-start.d/20-harness-provision and is
not meant to be run directly.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runHarnessProvision(cmd.Context(), provisionManifestPath)
	},
}

func init() {
	rootCmd.AddCommand(harnessCmd)
	harnessCmd.AddCommand(harnessProvisionCmd)

	harnessProvisionCmd.Flags().StringVar(&provisionManifestPath, "manifest", "",
		"Path to the staged manifest.json (required)")
	_ = harnessProvisionCmd.MarkFlagRequired("manifest")
}

// containerProvisionManifest mirrors pkg/harness.ProvisionManifest but is
// duplicated here to avoid pulling pkg/harness (and pkg/config) into the
// container-side binary. The script reads this from JSON; only the fields
// sciontool needs to validate and dispatch are declared.
type containerProvisionManifest struct {
	SchemaVersion    int                 `json:"schema_version"`
	Command          string              `json:"command"`
	AgentName        string              `json:"agent_name"`
	AgentHome        string              `json:"agent_home"`
	AgentWorkspace   string              `json:"agent_workspace"`
	HarnessBundleDir string              `json:"harness_bundle_dir"`
	HarnessConfig    containerHarnessCfg `json:"harness_config"`
	Inputs           map[string]string   `json:"inputs"`
	Outputs          containerOutputs    `json:"outputs"`
	Platform         map[string]string   `json:"platform"`
}

type containerHarnessCfg struct {
	Harness     string                `json:"harness"`
	Provisioner *containerProvisioner `json:"provisioner,omitempty"`
}

type containerProvisioner struct {
	Type             string   `json:"type,omitempty"`
	InterfaceVersion int      `json:"interface_version,omitempty"`
	Command          []string `json:"command,omitempty"`
	Timeout          string   `json:"timeout,omitempty"`
}

type containerOutputs struct {
	Env          string `json:"env,omitempty"`
	ResolvedAuth string `json:"resolved_auth,omitempty"`
	Status       string `json:"status,omitempty"`
}

// runHarnessProvision implements the sciontool harness provision flow.
func runHarnessProvision(ctx context.Context, manifestPath string) error {
	if manifestPath == "" {
		return fmt.Errorf("--manifest is required")
	}
	abs, err := filepath.Abs(manifestPath)
	if err != nil {
		return fmt.Errorf("resolve manifest path: %w", err)
	}
	manifest, err := loadProvisionManifest(abs)
	if err != nil {
		return err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve $HOME: %w", err)
	}
	bundleRoot := filepath.Join(home, ".scion", "harness")

	if err := validateManifestPaths(manifest, bundleRoot, home); err != nil {
		return err
	}

	prov := manifest.HarnessConfig.Provisioner
	if prov == nil || prov.Type != "container-script" {
		return fmt.Errorf("manifest does not declare provisioner.type: container-script")
	}
	if len(prov.Command) == 0 {
		return fmt.Errorf("manifest provisioner.command is empty")
	}

	timeout := 30 * time.Second
	if prov.Timeout != "" {
		t, err := time.ParseDuration(prov.Timeout)
		if err != nil {
			return fmt.Errorf("parse provisioner.timeout %q: %w", prov.Timeout, err)
		}
		timeout = t
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, prov.Command[0], prov.Command[1:]...)
	cmd.Env = minimalEnv(home, manifest)
	cmd.Stdin = nil
	cmd.Stdout = os.Stderr
	stderrBuf := &capturedStderr{}
	cmd.Stderr = io.MultiWriter(os.Stderr, stderrBuf)

	if err := cmd.Run(); err != nil {
		stderr := scrubSecrets(stderrBuf.String(), manifest)
		switch {
		case runCtx.Err() == context.DeadlineExceeded:
			return fmt.Errorf("harness provisioner timed out after %s: %s", timeout, truncate(stderr, 4096))
		case isExitCode(err, 2):
			return fmt.Errorf("harness provisioner exited 2 (unsupported command): %s", truncate(stderr, 4096))
		default:
			return fmt.Errorf("harness provisioner failed: %w: %s", err, truncate(stderr, 4096))
		}
	}

	// Validate the well-known outputs.
	if manifest.Outputs.Env != "" && fileExists(manifest.Outputs.Env) {
		if err := validateJSONFile(manifest.Outputs.Env, 1<<20); err != nil {
			return fmt.Errorf("invalid env output: %w", err)
		}
	}
	if manifest.Outputs.ResolvedAuth != "" && fileExists(manifest.Outputs.ResolvedAuth) {
		if err := validateJSONFile(manifest.Outputs.ResolvedAuth, 1<<20); err != nil {
			return fmt.Errorf("invalid resolved-auth output: %w", err)
		}
	}

	log.Info("harness provisioner completed for %s", manifest.AgentName)
	return nil
}

func loadProvisionManifest(path string) (*containerProvisionManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	if len(data) > 1<<20 {
		return nil, fmt.Errorf("manifest is larger than 1MiB; refusing to load")
	}
	var manifest containerProvisionManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if manifest.SchemaVersion == 0 {
		return nil, fmt.Errorf("manifest is missing schema_version")
	}
	if manifest.SchemaVersion > 1 {
		return nil, fmt.Errorf("manifest schema_version %d is newer than supported (1)", manifest.SchemaVersion)
	}
	return &manifest, nil
}

// validateManifestPaths refuses to run if any path in the manifest escapes the
// allowed roots (the harness bundle dir or the agent home). Path traversal in
// a manifest could let a malicious harness-config write outside its sandbox.
func validateManifestPaths(m *containerProvisionManifest, bundleRoot, home string) error {
	allowed := []string{bundleRoot, home}
	check := func(label, path string) error {
		if path == "" {
			return nil
		}
		clean := filepath.Clean(path)
		for _, root := range allowed {
			if strings.HasPrefix(clean, root) {
				return nil
			}
		}
		return fmt.Errorf("%s path %q escapes allowed roots %v", label, clean, allowed)
	}

	if err := check("harness_bundle_dir", m.HarnessBundleDir); err != nil {
		return err
	}
	for k, v := range m.Inputs {
		if err := check("inputs."+k, v); err != nil {
			return err
		}
	}
	if err := check("outputs.env", m.Outputs.Env); err != nil {
		return err
	}
	if err := check("outputs.resolved_auth", m.Outputs.ResolvedAuth); err != nil {
		return err
	}
	if err := check("outputs.status", m.Outputs.Status); err != nil {
		return err
	}

	// Provisioner command path must be absolute or resolve via PATH; we leave
	// PATH lookups to exec.Command. We still reject obvious traversal.
	prov := m.HarnessConfig.Provisioner
	if prov != nil && len(prov.Command) > 0 {
		if strings.Contains(prov.Command[0], "..") {
			return fmt.Errorf("provisioner.command[0] %q contains traversal", prov.Command[0])
		}
	}
	return nil
}

// minimalEnv builds a controlled environment for the script. We keep $HOME,
// $PATH, $LANG, $TZ, and any SCION_* vars from the parent so the script can
// reach python3 and locate scion runtime hints, but we deliberately drop
// unrelated host-level env to mirror the design's containment guidance.
func minimalEnv(home string, m *containerProvisionManifest) []string {
	env := []string{
		"HOME=" + home,
		"PATH=" + envOr("PATH", "/usr/local/bin:/usr/bin:/bin"),
		"LANG=" + envOr("LANG", "C.UTF-8"),
		"TZ=" + envOr("TZ", "UTC"),
		"SCION_AGENT_NAME=" + m.AgentName,
		"SCION_AGENT_HOME=" + m.AgentHome,
		"SCION_AGENT_WORKSPACE=" + m.AgentWorkspace,
		"SCION_HARNESS_BUNDLE=" + m.HarnessBundleDir,
		"SCION_HARNESS=" + m.HarnessConfig.Harness,
	}
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "SCION_") {
			// Pass through SCION_* hints not already set above.
			key := strings.SplitN(e, "=", 2)[0]
			if !envHasKey(env, key) {
				env = append(env, e)
			}
		}
	}
	return env
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envHasKey(env []string, key string) bool {
	prefix := key + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}

func validateJSONFile(path string, maxBytes int) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Size() > int64(maxBytes) {
		return fmt.Errorf("%s exceeds %d bytes", path, maxBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var anyJSON interface{}
	if err := json.Unmarshal(data, &anyJSON); err != nil {
		return fmt.Errorf("%s is not valid JSON: %w", path, err)
	}
	return nil
}

// capturedStderr captures stderr output for inclusion in error messages,
// up to a fixed cap to avoid unbounded memory.
type capturedStderr struct {
	buf []byte
}

func (c *capturedStderr) Write(p []byte) (int, error) {
	const cap = 64 * 1024
	remaining := cap - len(c.buf)
	if remaining <= 0 {
		return len(p), nil
	}
	if len(p) > remaining {
		c.buf = append(c.buf, p[:remaining]...)
	} else {
		c.buf = append(c.buf, p...)
	}
	return len(p), nil
}

func (c *capturedStderr) String() string { return string(c.buf) }

// scrubSecrets replaces obvious secret values in stderr output. It looks for
// values mentioned in inputs/outputs/auth_candidates files and the contents of
// staged secret files (written by ContainerScriptHarness.ApplyAuthSettings),
// then removes their occurrences. This is best-effort — scripts should not
// echo credentials.
func scrubSecrets(s string, m *containerProvisionManifest) string {
	out := s
	for _, val := range loadAuthCandidatesValues(m) {
		if val != "" && len(val) >= 8 {
			out = strings.ReplaceAll(out, val, "[REDACTED]")
		}
	}
	// auth-candidates.json holds *names* (and now file paths) but not the raw
	// secret values; the actual values live as 0600 files under
	// .scion/harness/secrets/. Read those too so a script that accidentally
	// echoes its API key still gets redacted.
	for _, val := range loadStagedSecretValues(m) {
		if val != "" && len(val) >= 8 {
			out = strings.ReplaceAll(out, val, "[REDACTED]")
		}
	}
	return out
}

// loadStagedSecretValues reads the per-secret files written by the host-side
// ApplyAuthSettings into agent_home/.scion/harness/secrets/. The directory is
// optional; missing dir means "no env-secret values were staged" and is fine.
func loadStagedSecretValues(m *containerProvisionManifest) []string {
	if m.HarnessBundleDir == "" {
		return nil
	}
	dir := filepath.Join(expandHomePrefix(m.HarnessBundleDir), "secrets")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		val := strings.TrimRight(string(data), "\r\n")
		if val != "" {
			out = append(out, val)
		}
	}
	return out
}

// expandHomePrefix resolves a leading "$HOME/" prefix to the current user's
// home dir. Manifest paths are written with that prefix so they stay portable.
func expandHomePrefix(p string) string {
	if !strings.HasPrefix(p, "$HOME/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, strings.TrimPrefix(p, "$HOME/"))
}

// loadAuthCandidatesValues reads the auth-candidates.json input file (if
// present) and returns any non-empty string fields whose name suggests a
// secret. We use this only for stderr scrubbing; the manifest itself doesn't
// carry credentials.
func loadAuthCandidatesValues(m *containerProvisionManifest) []string {
	path := m.Inputs["auth_candidates"]
	if path == "" || !fileExists(path) {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil
	}
	var out []string
	collect(raw, &out)
	return out
}

func collect(node interface{}, out *[]string) {
	switch v := node.(type) {
	case map[string]interface{}:
		for _, child := range v {
			collect(child, out)
		}
	case []interface{}:
		for _, child := range v {
			collect(child, out)
		}
	case string:
		if v != "" && len(v) >= 8 {
			*out = append(*out, v)
		}
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func isExitCode(err error, code int) bool {
	if err == nil {
		return false
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode() == code
	}
	return false
}
