/*
Copyright 2026 The Scion Authors.
*/

package hooks

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Env vars that move the harness bundle's outputs/ and secrets/ directories
// out of the agent home. When unset or empty, the directories are the
// bundle's own outputs/ and secrets/, exactly as before.
const (
	HarnessOutputsDirEnv = "SCION_HARNESS_OUTPUTS_DIR"
	HarnessSecretsDirEnv = "SCION_HARNESS_SECRETS_DIR"
)

// HarnessDirs holds the effective outputs and secrets directories of a
// harness bundle.
type HarnessDirs struct {
	Bundle  string
	Outputs string
	Secrets string
	// outputsSet and secretsSet record whether the env vars set them.
	outputsSet bool
	secretsSet bool
}

// ResolveHarnessDirs returns the outputs and secrets directories for the
// bundle at bundleDir, from HarnessOutputsDirEnv and HarnessSecretsDirEnv
// when set, else bundleDir/outputs and bundleDir/secrets. A value that is
// not an absolute path is an error.
func ResolveHarnessDirs(bundleDir string) (HarnessDirs, error) {
	d := HarnessDirs{
		Bundle:  bundleDir,
		Outputs: filepath.Join(bundleDir, "outputs"),
		Secrets: filepath.Join(bundleDir, "secrets"),
	}
	if v := os.Getenv(HarnessOutputsDirEnv); v != "" {
		if !filepath.IsAbs(v) {
			return HarnessDirs{}, fmt.Errorf("%s must be an absolute path, got %q", HarnessOutputsDirEnv, v)
		}
		d.Outputs, d.outputsSet = filepath.Clean(v), true
	}
	if v := os.Getenv(HarnessSecretsDirEnv); v != "" {
		if !filepath.IsAbs(v) {
			return HarnessDirs{}, fmt.Errorf("%s must be an absolute path, got %q", HarnessSecretsDirEnv, v)
		}
		d.Secrets, d.secretsSet = filepath.Clean(v), true
	}
	return d, nil
}

// OutputPath maps a path under the bundle's outputs/ directory to the same
// name under the effective outputs directory. Other paths, and every path
// when the env var is unset, are returned unchanged.
func (d HarnessDirs) OutputPath(p string) string {
	if !d.outputsSet {
		return p
	}
	return remapUnder(p, filepath.Join(d.Bundle, "outputs"), d.Outputs)
}

// SecretPath does the same as OutputPath for the secrets/ directory.
func (d HarnessDirs) SecretPath(p string) string {
	if !d.secretsSet {
		return p
	}
	return remapUnder(p, filepath.Join(d.Bundle, "secrets"), d.Secrets)
}

// ExtraRoots returns the directories set by the env vars, which lie outside
// the bundle and the agent home and so must be added to the allowed roots
// for paths that point into them. It is empty when neither var is set.
func (d HarnessDirs) ExtraRoots() []string {
	var out []string
	if d.outputsSet {
		out = append(out, d.Outputs)
	}
	if d.secretsSet {
		out = append(out, d.Secrets)
	}
	return out
}

func remapUnder(p, from, to string) string {
	if p == "" {
		return p
	}
	clean := filepath.Clean(p)
	if clean == from {
		return to
	}
	if rel, ok := strings.CutPrefix(clean, from+string(filepath.Separator)); ok {
		return filepath.Join(to, rel)
	}
	return p
}

// ResolveEnvOverlay returns the absolute path of the env overlay output and
// the roots its from_file references may point into. With the env vars
// unset these are the manifest path and {BundleDir, agentHome}.
func (r HarnessManifestRequirement) ResolveEnvOverlay(agentHome string) (string, []string, error) {
	dirs, err := ResolveHarnessDirs(r.BundleDir)
	if err != nil {
		return "", nil, err
	}
	path := dirs.OutputPath(ResolveContainerPath(r.EnvOverlayPath, agentHome))
	roots := append([]string{r.BundleDir, agentHome}, dirs.ExtraRoots()...)
	return path, roots, nil
}
