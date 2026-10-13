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

package agentplugin

import "fmt"

// Staged layout, relative to the agent home.
const (
	// InputsRel is the staged harness inputs directory, relative to the
	// agent home.
	InputsRel = ".scion/harness/inputs"
	// PluginsDirName is the directory under InputsRel that holds one staged
	// bundle per plugin, named by the validated manifest name.
	PluginsDirName = "plugins"
	// IndexFileName is the index written under InputsRel.
	IndexFileName = "plugins.json"
	// IndexSchemaVersion is the plugins.json schema_version.
	IndexSchemaVersion = 1
)

// Severity classifies a Diagnostic.
type Severity string

const (
	SeverityWarning Severity = "warning"
	SeverityError   Severity = "error"
)

// Diagnostic is a non-fatal finding about a plugin or a declaration.
type Diagnostic struct {
	Plugin   string   `json:"plugin,omitempty"`
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
}

// String renders the diagnostic as a start warning, prefixed "agent_plugins:".
func (d Diagnostic) String() string {
	if d.Plugin != "" {
		return fmt.Sprintf("agent_plugins: plugin %q: %s", d.Plugin, d.Message)
	}
	return "agent_plugins: " + d.Message
}

func warning(format string, args ...any) Diagnostic {
	return Diagnostic{Severity: SeverityWarning, Message: fmt.Sprintf(format, args...)}
}

// Index is the staged inputs/plugins.json document read by the harness's
// provision.py.
type Index struct {
	SchemaVersion int          `json:"schema_version"`
	SpecVersion   string       `json:"spec_version"`
	Plugins       []IndexEntry `json:"plugins"`
}

// IndexEntry describes one staged plugin.
type IndexEntry struct {
	Name        string `json:"name"`
	Version     string `json:"version,omitempty"`
	Description string `json:"description,omitempty"`
	// Dir is the staged bundle directory, relative to the inputs directory.
	Dir    string      `json:"dir"`
	Digest string      `json:"digest"`
	Source IndexSource `json:"source"`
	// Skills are the skill directory names discovered under skills/.
	Skills []string `json:"skills"`
	// SkillsDirConflict reports that a skill with the same name is already
	// installed in the harness skills directory by this provisioning.
	SkillsDirConflict bool `json:"skills_dir_conflict"`
	// MCP is nil when mcp.json is absent or disabled as a whole.
	MCP         *MCPConfig   `json:"mcp"`
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
}

// IndexSource records where a staged plugin came from.
type IndexSource struct {
	Template string `json:"template"`
	Path     string `json:"path"`
}
