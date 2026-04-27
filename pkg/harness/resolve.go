// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package harness

import (
	"context"
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	scionplugin "github.com/GoogleCloudPlatform/scion/pkg/plugin"
)

// ResolveOptions selects how a harness is constructed.
//
// Production callers that already know the resolved harness-config name should
// pass it as Name. The resolver looks up the harness-config directory through
// the same precedence used by config.FindHarnessConfigDir (template, grove,
// global) and merges any settings overrides via VersionedSettings.
//
// Legacy callers without harness-config context can use the harness.New shim,
// which preserves built-in behavior.
type ResolveOptions struct {
	Name          string                    // harness-config name (e.g. "claude")
	GrovePath     string                    // optional grove path for resolution
	TemplatePaths []string                  // optional template dirs (highest priority)
	ProfileName   string                    // active profile (for settings overlay)
	Settings      *config.VersionedSettings // optional settings overlay
}

// ResolvedHarness is the result of harness.Resolve. The selected
// implementation is captured in Implementation so callers and tests can
// distinguish the four supported paths.
type ResolvedHarness struct {
	Harness        api.Harness
	ConfigName     string
	ConfigDir      *config.HarnessConfigDir
	Config         config.HarnessConfigEntry
	Implementation string // builtin | container-script | plugin | generic
}

// Resolve constructs an api.Harness using the priority order from the design:
//
//  1. Explicit container-script harness (provisioner.type: container-script)
//  2. Built-in Go harness for known harness types
//  3. Go-plugin harness if registered
//  4. Declarative generic harness (config.yaml only)
//
// For (1) the resolved harness-config dir must exist and contain provision.py
// (the activation step in Phase 1 enforces this). For (2)-(4) the harness-
// config dir is optional.
func Resolve(_ context.Context, opts ResolveOptions) (*ResolvedHarness, error) {
	if opts.Name == "" {
		return nil, fmt.Errorf("harness.Resolve requires a name")
	}

	hcDir, hcErr := config.FindHarnessConfigDir(opts.Name, opts.GrovePath, opts.TemplatePaths...)

	entry := config.HarnessConfigEntry{Harness: opts.Name}
	if hcDir != nil {
		entry = hcDir.Config
	}

	// Settings overlay: profile-level overrides on top of the dir entry.
	if opts.Settings != nil {
		settingsEntry, _ := opts.Settings.ResolveHarnessConfig(opts.ProfileName, opts.Name)
		entry = mergeHarnessConfigEntries(entry, settingsEntry)
	}

	if entry.Harness == "" {
		entry.Harness = opts.Name
	}

	// 1. Explicit container-script
	if entry.Provisioner != nil && entry.Provisioner.Type == "container-script" {
		if hcDir == nil || hcDir.Path == "" {
			return nil, fmt.Errorf("container-script harness %q requires an on-disk harness-config directory: %w", opts.Name, hcErr)
		}
		h, err := NewContainerScriptHarness(hcDir.Path, entry)
		if err != nil {
			return nil, err
		}
		return &ResolvedHarness{
			Harness:        h,
			ConfigName:     opts.Name,
			ConfigDir:      hcDir,
			Config:         entry,
			Implementation: "container-script",
		}, nil
	}

	// 2. Built-in
	if builtin := newBuiltin(entry.Harness); builtin != nil {
		return &ResolvedHarness{
			Harness:        builtin,
			ConfigName:     opts.Name,
			ConfigDir:      hcDir,
			Config:         entry,
			Implementation: "builtin",
		}, nil
	}

	// 3. Plugin
	if pluginManager != nil && pluginManager.HasPlugin(scionplugin.PluginTypeHarness, entry.Harness) {
		h, err := pluginManager.GetHarness(entry.Harness)
		if err == nil && h != nil {
			return &ResolvedHarness{
				Harness:        h,
				ConfigName:     opts.Name,
				ConfigDir:      hcDir,
				Config:         entry,
				Implementation: "plugin",
			}, nil
		}
	}

	// 4. Declarative generic. If config.yaml has declarative metadata
	// (command/env_template/capabilities), use the declarative wrapper so
	// callers get those fields. Otherwise fall back to the legacy Generic.
	if hasDeclarativeMetadata(entry) {
		return &ResolvedHarness{
			Harness:        NewDeclarativeGenericHarness(entry),
			ConfigName:     opts.Name,
			ConfigDir:      hcDir,
			Config:         entry,
			Implementation: "generic",
		}, nil
	}

	return &ResolvedHarness{
		Harness:        &Generic{},
		ConfigName:     opts.Name,
		ConfigDir:      hcDir,
		Config:         entry,
		Implementation: "generic",
	}, nil
}

// newBuiltin returns the compiled-in harness for the given harness type, or
// nil if none exists.
func newBuiltin(harnessName string) api.Harness {
	switch harnessName {
	case "claude":
		return &ClaudeCode{}
	case "gemini":
		return &GeminiCLI{}
	case "opencode":
		return &OpenCode{}
	case "codex":
		return &Codex{}
	}
	return nil
}

// mergeHarnessConfigEntries overlays settings overrides on top of the
// harness-config dir entry. Only the fields the settings layer is expected to
// override are merged here; all other declarative metadata flows from the dir.
func mergeHarnessConfigEntries(base, overlay config.HarnessConfigEntry) config.HarnessConfigEntry {
	if overlay.Harness != "" {
		base.Harness = overlay.Harness
	}
	if overlay.Image != "" {
		base.Image = overlay.Image
	}
	if overlay.User != "" {
		base.User = overlay.User
	}
	if overlay.Model != "" {
		base.Model = overlay.Model
	}
	if overlay.TaskFlag != "" {
		base.TaskFlag = overlay.TaskFlag
	}
	if len(overlay.Args) > 0 {
		base.Args = overlay.Args
	}
	if overlay.Env != nil {
		if base.Env == nil {
			base.Env = map[string]string{}
		}
		for k, v := range overlay.Env {
			base.Env[k] = v
		}
	}
	if len(overlay.Volumes) > 0 {
		base.Volumes = append(base.Volumes, overlay.Volumes...)
	}
	if overlay.AuthSelectedType != "" {
		base.AuthSelectedType = overlay.AuthSelectedType
	}
	if len(overlay.Secrets) > 0 {
		base.Secrets = overlay.Secrets
	}
	return base
}

func hasDeclarativeMetadata(entry config.HarnessConfigEntry) bool {
	if entry.Command != nil && len(entry.Command.Base) > 0 {
		return true
	}
	if entry.Capabilities != nil {
		return true
	}
	if len(entry.EnvTemplate) > 0 {
		return true
	}
	if entry.ConfigDir != "" || entry.SkillsDir != "" {
		return true
	}
	return false
}
