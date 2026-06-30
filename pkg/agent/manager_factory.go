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

package agent

import (
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/managedagent/google"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// NewManagerForProfile returns a Manager appropriate for the given profile.
// The "managed-agents" profile returns a ManagedAgentManager backed by the
// configured cloud provider; all other profiles return the standard
// container-based AgentManager.
func NewManagerForProfile(rt runtime.Runtime, profile string, settings *config.VersionedSettings, stateDir string) (Manager, error) {
	if profile != "managed-agents" {
		return NewManager(rt), nil
	}

	if settings == nil || settings.ManagedAgents == nil || settings.ManagedAgents.Google == nil {
		return nil, fmt.Errorf("managed-agents profile requires managed_agents.google configuration in settings")
	}

	cfg := settings.ManagedAgents.Google
	backend, err := google.NewBackend(google.BackendConfig{
		APIKey:    cfg.APIKey,
		BaseAgent: cfg.BaseAgent,
		Model:     cfg.Model,
	})
	if err != nil {
		return nil, fmt.Errorf("creating managed agent backend: %w", err)
	}

	return NewManagedAgentManager(backend, stateDir), nil
}
