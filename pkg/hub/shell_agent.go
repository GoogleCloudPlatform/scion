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
// AppliedConfig.HarnessConfig is the single authoritative persisted source:
// entadapter.harnessConfigOf mirrors it unconditionally into the indexed
// harness_config column on CreateAgent/UpdateAgent, and
// entadapter.isShellEntAgent reads that shadow column directly. When
// AppliedConfig is nil or empty (e.g. a partial or enriched store.Agent
// projection), isShellAgent falls back to Agent.HarnessConfig.
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

// isHumanShellPrincipal reports whether identity is an interactive/dev human
// principal permitted to create a shell execution container (ptone/scion#8812
// §3.4 S6). Allowlists "user" and "dev" only and fails closed on nil,
// delegated agent credentials, agent identities, or any other principal kind.
func isHumanShellPrincipal(identity Identity) bool {
	if identity == nil || isDelegatedAgentIdentity(identity) {
		return false
	}
	switch identity.Type() {
	case "user", "dev":
		return true
	default:
		return false
	}
}

// isHumanShellCreatePrincipal reports whether the caller identity and
// delegation principal kind represent a human principal permitted to create a
// shell execution container (ptone/scion#8812 §3.4 S6).
//
// Both HTTP (createAgentInProject) and headless (createAgentHeadless) create
// paths share this helper: identity must satisfy isHumanShellPrincipal
// ("user"/"dev" allowlist, rejecting nil and delegated agent credentials) AND
// principalKind must be store.DelegationPrincipalUser.
func isHumanShellCreatePrincipal(identity Identity, principalKind string) bool {
	return isHumanShellPrincipal(identity) && principalKind == store.DelegationPrincipalUser
}

// templateSuppliesShellHarnessConfig reports whether resolvedTemplate is what
// supplies the reserved "shell" harness config (i.e. neither the request nor
// the project default_harness_config annotation set a harness config, and the
// template declares default_harness_config: shell).
func templateSuppliesShellHarnessConfig(resolvedTemplate *store.Template, agent *store.Agent, project *store.Project) bool {
	if resolvedTemplate == nil || agent == nil || agent.AppliedConfig == nil {
		return false
	}
	if agent.AppliedConfig.HarnessConfig != "" {
		return false
	}
	if project != nil && project.Annotations != nil && project.Annotations[projectSettingDefaultHarnessConfig] != "" {
		return false
	}
	return templateHarnessConfigName(resolvedTemplate) == config.ShellHarnessConfigName
}

// shellAgentCreateOpts carries caller and template provenance into
// checkShellAgentCreateAdmission.
type shellAgentCreateOpts struct {
	IsHumanPrincipal        bool
	TemplateExplicit        bool
	TemplateSuppliedShellHC bool
}

// checkShellAgentCreateAdmission enforces shell execution container creation
// admission (ptone/scion#8812 §3.2, §3.4 S6, §7) for both HTTP
// (createAgentInProject) and headless (createAgentHeadless) agent creation:
//   - S6 (ungated): only human principals (user/dev allowlist + user principal kind) may create shell agents (403).
//   - Experiment gate: hub.exec_containers must be enabled (400).
//   - Initial task: rejected (400 task_not_supported).
//   - No templates (security audit H1): an explicitly requested template or a
//     template that itself declares default_harness_config: shell is refused
//     (400 validation_error, fail-closed); any defaulted template (project
//     default annotation, hub default, implicit "default") is dropped from the
//     agent record so no member-authored template home/ can reach the shell.
//   - No pre-start hook (security audit H3): clears any ProjectPreStartHookID
//     and ProjectPreStartHookScript on AppliedConfig as defence in depth.
func (s *Server) checkShellAgentCreateAdmission(agent *store.Agent, opts shellAgentCreateOpts) *shellCreateDenial {
	if !isShellAgent(agent) {
		return nil
	}
	if !opts.IsHumanPrincipal {
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
	if opts.TemplateExplicit {
		return &shellCreateDenial{
			status:  http.StatusBadRequest,
			code:    ErrCodeValidationError,
			message: "shell execution containers do not use templates",
		}
	}
	if opts.TemplateSuppliedShellHC {
		return &shellCreateDenial{
			status:  http.StatusBadRequest,
			code:    ErrCodeValidationError,
			message: "shell execution containers do not use templates; pass harness_config \"shell\" explicitly without a template",
		}
	}

	// Drop any defaulted template (project annotation, hub default, implicit "default").
	agent.Template = ""
	if agent.AppliedConfig != nil {
		agent.AppliedConfig.TemplateID = ""
		agent.AppliedConfig.TemplateHash = ""
		agent.AppliedConfig.ProjectPreStartHookID = ""
		agent.AppliedConfig.ProjectPreStartHookScript = ""
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
