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

package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"gopkg.in/yaml.v3"
)

// AgentPluginsSidecarFile is the harness-config bundle file that declares
// native Agent Plugins support: a mapping with `support` (yes, partial or no)
// and an optional `reason`.
//
// It lives next to config.yaml instead of in config.yaml's capabilities block
// because binaries built before GoogleCloudPlatform/scion#3698 validate
// config.yaml against a schema that rejects unknown capability keys
// (ptone/scion#5196). Those binaries never read or validate other bundle
// files, and the file travels with the bundle's provision.py, so a stale
// harness-config without plugin code has no sidecar and is still detected
// as predating support.
const AgentPluginsSidecarFile = "agent-plugins.yaml"

// agentPluginsSidecar is the on-disk shape of AgentPluginsSidecarFile.
// Unknown keys are ignored so a later addition does not break this binary.
type agentPluginsSidecar struct {
	Support api.SupportLevel `yaml:"support"`
	Reason  string           `yaml:"reason"`
}

// ParseAgentPluginsSidecar parses the bytes of an AgentPluginsSidecarFile.
// The support level must be yes, partial or no.
func ParseAgentPluginsSidecar(data []byte) (api.CapabilityField, error) {
	var s agentPluginsSidecar
	if err := yaml.Unmarshal(data, &s); err != nil {
		return api.CapabilityField{}, fmt.Errorf("invalid %s: %w", AgentPluginsSidecarFile, err)
	}
	switch s.Support {
	case api.SupportYes, api.SupportPartial, api.SupportNo:
	default:
		return api.CapabilityField{}, fmt.Errorf("invalid %s: support must be yes, partial or no, got %q", AgentPluginsSidecarFile, s.Support)
	}
	return api.CapabilityField{Support: s.Support, Reason: s.Reason}, nil
}

// applyAgentPluginsSidecar sets entry's agent_plugins capability from the
// sidecar bytes. The sidecar wins over a capabilities.agent_plugins value in
// config.yaml (the deprecated Phase 1 location, still read as the fallback).
func applyAgentPluginsSidecar(entry *HarnessConfigEntry, data []byte) error {
	c, err := ParseAgentPluginsSidecar(data)
	if err != nil {
		return err
	}
	if entry.Capabilities == nil {
		entry.Capabilities = &api.HarnessAdvancedCapabilities{}
	}
	entry.Capabilities.AgentPlugins = c
	return nil
}

// ApplyAgentPluginsSidecarDir applies dir's AgentPluginsSidecarFile to entry.
// A missing sidecar leaves entry unchanged.
func ApplyAgentPluginsSidecarDir(entry *HarnessConfigEntry, dir string) error {
	data, err := os.ReadFile(filepath.Join(dir, AgentPluginsSidecarFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to read %s: %w", AgentPluginsSidecarFile, err)
	}
	return applyAgentPluginsSidecar(entry, data)
}

// ApplyAgentPluginsSidecarFS is ApplyAgentPluginsSidecarDir for a bundle
// directory dir inside fsys (for example the embedded harnesses).
func ApplyAgentPluginsSidecarFS(entry *HarnessConfigEntry, fsys fs.FS, dir string) error {
	data, err := fs.ReadFile(fsys, path.Join(dir, AgentPluginsSidecarFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to read %s: %w", AgentPluginsSidecarFile, err)
	}
	return applyAgentPluginsSidecar(entry, data)
}
