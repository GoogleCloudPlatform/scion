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

package hub

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// isShellAgent reports whether agent uses the reserved built-in "shell"
// harness config (config.ShellHarnessConfigName).
//
// AppliedConfig.HarnessConfig is the authoritative persisted field on
// store.Agent (written on create/update and mirrored into the indexed
// harness_config column). When AppliedConfig is nil or its HarnessConfig is
// empty, isShellAgent falls back to the enriched Agent.HarnessConfig field.
func isShellAgent(agent *store.Agent) bool {
	if agent == nil {
		return false
	}
	if agent.AppliedConfig != nil && agent.AppliedConfig.HarnessConfig != "" {
		return agent.AppliedConfig.HarnessConfig == config.ShellHarnessConfigName
	}
	return agent.HarnessConfig == config.ShellHarnessConfigName
}

// refuseReservedHarnessConfigSlug writes 400 reserved_name and returns true
// when slug is the reserved built-in "shell" harness config name. This guard
// is ungated and applies at every scope (global, project, user) so nothing
// can shadow the built-in shell harness config (ptone/scion#8812 §3.2).
func refuseReservedHarnessConfigSlug(w http.ResponseWriter, slug string) bool {
	if slug != config.ShellHarnessConfigName {
		return false
	}
	writeError(w, http.StatusBadRequest, ErrCodeReservedName,
		fmt.Sprintf("harness config slug %q is reserved by the built-in shell harness", slug),
		map[string]interface{}{"field": "slug"})
	return true
}

// refuseReservedHarnessType writes 400 reserved_name and returns true when
// harnessType is the reserved "shell" harness type. Only the built-in "shell"
// harness config may use the shell harness so non-reserved configs cannot
// bypass Hub-side shell-agent protections (ptone/scion#8812 §3.2).
func refuseReservedHarnessType(w http.ResponseWriter, harnessType string) bool {
	if !strings.EqualFold(strings.TrimSpace(harnessType), config.ShellHarnessConfigName) {
		return false
	}
	writeError(w, http.StatusBadRequest, ErrCodeReservedName,
		fmt.Sprintf("harness %q is reserved for the built-in %q harness config", config.ShellHarnessConfigName, config.ShellHarnessConfigName),
		map[string]interface{}{"field": "harness"})
	return true
}

// shellCreateDenial describes why creating a shell execution container was
// refused by checkShellAgentCreateAdmission.
type shellCreateDenial struct {
	status  int
	code    string
	message string
}

func (d *shellCreateDenial) Error() string {
	return d.code + ": " + d.message
}

func (d *shellCreateDenial) write(w http.ResponseWriter) {
	writeError(w, d.status, d.code, d.message, nil)
}

// checkShellAgentCreateAdmission enforces shell execution container creation
// admission (ptone/scion#8812 §3.2, §3.4 S6, §7) for both HTTP
// (createAgentInProject) and headless (createAgentHeadless) agent creation:
//   - S6 (ungated): only human principals may create shell agents (403).
//   - Experiment gate: hub.exec_containers must be enabled (400).
//   - Initial task: rejected (400 task_not_supported).
func (s *Server) checkShellAgentCreateAdmission(agent *store.Agent, isAgentPrincipal bool) *shellCreateDenial {
	if !isShellAgent(agent) {
		return nil
	}
	if isAgentPrincipal {
		return &shellCreateDenial{
			status:  http.StatusForbidden,
			code:    ErrCodeForbidden,
			message: "shell agent: human principals only",
		}
	}
	if !s.experimentEnabled(experiments.ExecContainers) {
		return &shellCreateDenial{
			status:  http.StatusBadRequest,
			code:    ErrCodeExperimentDisabled,
			message: "experiment_disabled: " + experiments.ExecContainers + " is off; execution containers are not enabled on this hub",
		}
	}
	if agent.AppliedConfig != nil && strings.TrimSpace(agent.AppliedConfig.Task) != "" {
		return &shellCreateDenial{
			status:  http.StatusBadRequest,
			code:    ErrCodeTaskNotSupported,
			message: "shell execution containers do not support an initial task",
		}
	}
	return nil
}

// shellAgentNotMessageableError returns the typed DM refusal for a shell
// execution container (ptone/scion#8812 §3.4 S5).
func shellAgentNotMessageableError(agent *store.Agent) *AgentDMError {
	return &AgentDMError{
		Code:       ErrCodeAgentNotMessageable,
		Message:    fmt.Sprintf("agent %q is an execution container and cannot receive messages", agentTargetLabel(agent)),
		HTTPStatus: http.StatusUnprocessableEntity,
	}
}
