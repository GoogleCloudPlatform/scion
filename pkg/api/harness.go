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

package api

import (
	"context"
)

// Harness interface defines the methods a harness must implement
type Harness interface {
	Name() string
	DiscoverAuth(agentHome string) AuthConfig
	GetEnv(agentName string, agentHome string, unixUsername string, auth AuthConfig) map[string]string
	GetCommand(task string, resume bool, baseArgs []string) []string
	PropagateFiles(homeDir, unixUsername string, auth AuthConfig) error
	GetVolumes(unixUsername string, auth AuthConfig) []VolumeMount
	DefaultConfigDir() string
	HasSystemPrompt(agentHome string) bool

	// Provision performs harness-specific setup during agent creation.
	// This is called after templates are copied and scion-agent.json is written.
	Provision(ctx context.Context, agentName, agentHome, agentWorkspace string) error

	// SeedTemplateDir seeds the template directory for this harness.
	SeedTemplateDir(templateDir string, force bool) error

	// GetEmbedDir returns the name of the directory in pkg/config/embeds/
	// that contains template files for this harness (e.g., "claude", "gemini").
	GetEmbedDir() string

	// GetInterruptKey returns the key sequence used to interrupt the harness process (e.g., "C-c" or "Escape").
	GetInterruptKey() string
}
