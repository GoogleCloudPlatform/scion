/*
Copyright 2026 The Scion Authors.
*/

package hooks

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// HarnessManifestRequirement summarizes what sciontool init needs to know
// about a staged container-script harness manifest at startup. It is parsed
// from agent_home/.scion/harness/manifest.json with only the fields needed
// to decide whether pre-start provisioning is required and where to find
// the env overlay output.
type HarnessManifestRequirement struct {
	// Required is true when manifest.json exists with a container-script
	// provisioner. When true, pre-start hook failures must abort startup
	// because the child harness will be misconfigured otherwise.
	Required bool

	// EnvOverlayPath is the path declared in manifest.outputs.env, if any.
	// The path is the value the script will write to (a container path).
	EnvOverlayPath string

	// BundleDir is the staged bundle directory (typically
	// agent_home/.scion/harness). Used as an allowedRoot for env overlay
	// from_file resolution.
	BundleDir string
}

// LoadHarnessManifestRequirement inspects the staged manifest at
// agent_home/.scion/harness/manifest.json and returns the requirement
// details. If the manifest is missing, returns a zero-value (Required=false)
// and no error — that is the normal state for built-in harnesses.
func LoadHarnessManifestRequirement(agentHome string) (HarnessManifestRequirement, error) {
	if agentHome == "" {
		return HarnessManifestRequirement{}, nil
	}
	bundleDir := filepath.Join(agentHome, ".scion", "harness")
	manifestPath := filepath.Join(bundleDir, "manifest.json")

	data, err := os.ReadFile(manifestPath)
	if os.IsNotExist(err) {
		return HarnessManifestRequirement{}, nil
	}
	if err != nil {
		return HarnessManifestRequirement{}, fmt.Errorf("read harness manifest: %w", err)
	}

	// Parse the minimal shape we care about. Future fields are ignored.
	var manifest struct {
		HarnessConfig struct {
			Provisioner *struct {
				Type             string   `json:"type"`
				LifecycleEvents  []string `json:"lifecycle_events"`
				InterfaceVersion int      `json:"interface_version"`
			} `json:"provisioner"`
		} `json:"harness_config"`
		Outputs struct {
			Env string `json:"env"`
		} `json:"outputs"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return HarnessManifestRequirement{}, fmt.Errorf("parse harness manifest: %w", err)
	}

	prov := manifest.HarnessConfig.Provisioner
	if prov == nil || prov.Type != "container-script" {
		return HarnessManifestRequirement{}, nil
	}

	// pre-start participation is the default for container-script. If the
	// harness explicitly declares lifecycle_events, honor it; otherwise we
	// assume pre-start is required (matches the documented default).
	required := true
	if len(prov.LifecycleEvents) > 0 {
		required = false
		for _, ev := range prov.LifecycleEvents {
			if ev == EventPreStart {
				required = true
				break
			}
		}
	}

	return HarnessManifestRequirement{
		Required:       required,
		EnvOverlayPath: manifest.Outputs.Env,
		BundleDir:      bundleDir,
	}, nil
}

// ResolveContainerPath rewrites a path that may begin with the literal
// "$HOME/" prefix (as encoded in staged manifests) into an absolute path
// rooted at the runtime $HOME. Other paths are returned unchanged. This
// keeps the manifest container-portable while still letting sciontool init
// open the file at runtime.
func ResolveContainerPath(path, home string) string {
	if path == "" || home == "" {
		return path
	}
	if path == "$HOME" {
		return home
	}
	if len(path) >= 6 && path[:6] == "$HOME/" {
		return filepath.Join(home, path[6:])
	}
	return path
}
