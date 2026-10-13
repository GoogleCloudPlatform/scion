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

// Package agentplugin stages Agent Plugins 1.0.0 bundles declared in a
// template's agent_plugins for native install by a harness.
//
// Go resolves the template-relative source, validates plugin.json and
// mcp.json against the 1.0.0 specification, copies the bundle without ever
// following a symlink, and writes the staged bundle plus an index
// (inputs/plugins.json) into the agent's harness bundle. Each harness's
// provision.py then installs the staged bundle natively.
package agentplugin

const (
	// SpecVersion is the only Agent Plugins version Scion supports.
	SpecVersion = "1.0.0"
	// PluginSchemaV1 is the canonical plugin.json $schema identifier for 1.0.0.
	PluginSchemaV1 = "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json"
	// MCPSchemaV1 is the canonical mcp.json $schema identifier for 1.0.0.
	MCPSchemaV1 = "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json"

	// ManifestFile is the fixed manifest location in a plugin root.
	ManifestFile = "plugin.json"
	// MCPFile is the fixed MCP configuration location in a plugin root.
	MCPFile = "mcp.json"
	// SkillsDir is the fixed skills location in a plugin root.
	SkillsDir = "skills"

	// maxJSONFileSize caps plugin.json and mcp.json.
	maxJSONFileSize = 1 << 20
)
