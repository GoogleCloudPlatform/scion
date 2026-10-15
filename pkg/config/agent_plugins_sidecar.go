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
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sync"

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

// ParseAgentPluginsSidecar strictly parses the bytes of an
// AgentPluginsSidecarFile: the support level must be yes, partial or no.
// Unknown keys are ignored. The loaders do not fail on its error (they
// degrade, see applyAgentPluginsSidecar); tests of the bundled files call it
// directly so a bad bundled sidecar fails CI.
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

// AgentPluginsCapability returns the entry's effective agent_plugins
// capability: the bundle sidecar when present, otherwise the deprecated
// capabilities.agent_plugins value of config.yaml, otherwise the zero value
// (the harness-config predates Agent Plugins support).
func (e *HarnessConfigEntry) AgentPluginsCapability() api.CapabilityField {
	if e.AgentPluginsSidecar != nil {
		return *e.AgentPluginsSidecar
	}
	if e.Capabilities != nil {
		return e.Capabilities.AgentPlugins
	}
	return api.CapabilityField{}
}

// invalidSidecarWarned dedupes the invalid-sidecar warning per location and
// error, so a harness-config loaded on every start warns once per process.
var invalidSidecarWarned sync.Map

// applyAgentPluginsSidecar records the sidecar bytes on entry. A sidecar that
// does not parse, or has a support level or shape this binary does not know
// (for example one written for a later release), never fails the load: it
// degrades to support "no" with the parse error as the reason, and a warning
// is logged. The feature it gates is optional; the harness stays usable.
func applyAgentPluginsSidecar(entry *HarnessConfigEntry, data []byte, where string) {
	c, err := ParseAgentPluginsSidecar(data)
	if err != nil {
		c = invalidAgentPluginsSidecar(err, where)
	}
	entry.AgentPluginsSidecar = &c
}

func invalidAgentPluginsSidecar(err error, where string) api.CapabilityField {
	if _, seen := invalidSidecarWarned.LoadOrStore(where+"\x00"+err.Error(), true); !seen {
		slog.Warn("ignoring invalid harness-config agent-plugins.yaml; Agent Plugins treated as unsupported",
			"path", where, "error", err)
	}
	return api.CapabilityField{Support: api.SupportNo, Reason: err.Error()}
}

// ApplyAgentPluginsSidecarDir records dir's AgentPluginsSidecarFile on entry.
// A missing sidecar leaves entry unchanged; an unreadable or invalid one
// degrades to support "no" (see applyAgentPluginsSidecar).
func ApplyAgentPluginsSidecarDir(entry *HarnessConfigEntry, dir string) {
	p := filepath.Join(dir, AgentPluginsSidecarFile)
	data, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		c := invalidAgentPluginsSidecar(fmt.Errorf("invalid %s: %w", AgentPluginsSidecarFile, err), p)
		entry.AgentPluginsSidecar = &c
		return
	}
	applyAgentPluginsSidecar(entry, data, p)
}

// ApplyAgentPluginsSidecarFS is ApplyAgentPluginsSidecarDir for a bundle
// directory dir inside fsys (for example the embedded harnesses).
func ApplyAgentPluginsSidecarFS(entry *HarnessConfigEntry, fsys fs.FS, dir string) {
	p := path.Join(dir, AgentPluginsSidecarFile)
	data, err := fs.ReadFile(fsys, p)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		c := invalidAgentPluginsSidecar(fmt.Errorf("invalid %s: %w", AgentPluginsSidecarFile, err), p)
		entry.AgentPluginsSidecar = &c
		return
	}
	applyAgentPluginsSidecar(entry, data, p)
}
