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
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// patchBodyHasOwnerField reports whether a raw JSON PATCH body contains an
// ownerId or owner_id top-level key (case-insensitive, matching encoding/json).
// Used by applyAgentUpdate to explicitly refuse ownership transfer attempts on
// execution containers (ptone/scion#8812 §3.4 S8).
func patchBodyHasOwnerField(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return false
	}
	for k := range raw {
		if strings.EqualFold(k, "ownerId") || strings.EqualFold(k, "owner_id") {
			return true
		}
	}
	return false
}

// isShellAgent reports whether agent uses the reserved built-in "shell"
// harness config (config.ShellHarnessConfigName).
//
// Fails closed: returns true when EITHER the indexed harness_config column
// (Agent.HarnessConfig, populated by entAgentToStore) OR
// AppliedConfig.HarnessConfig says "shell". If applied_config fails to parse
// (leaving AppliedConfig nil) or the two disagree, protections stay enforced
// rather than dropping open.
func isShellAgent(agent *store.Agent) bool {
	if agent == nil {
		return false
	}
	if agent.HarnessConfig == config.ShellHarnessConfigName {
		return true
	}
	return agent.AppliedConfig != nil && agent.AppliedConfig.HarnessConfig == config.ShellHarnessConfigName
}

// willBeShellAgent reports whether agent is guaranteed to resolve to the
// reserved built-in "shell" harness config from tiers that strictly outrank
// templates (the agent's own fields or the project's default_harness_config
// annotation), before deriveAgentConfig runs. The lower hub-default tier is
// intentionally excluded here because a template's default_harness_config
// outranks the hub default; hub-default shell agents are enforced by the
// post-derivation GCP identity override after deriveAgentConfig.
func (s *Server) willBeShellAgent(agent *store.Agent, project *store.Project) bool {
	if isShellAgent(agent) {
		return true
	}
	if agent != nil && agent.AppliedConfig != nil && agent.AppliedConfig.HarnessConfig != "" {
		return false
	}
	return project != nil && project.Annotations != nil &&
		project.Annotations[projectSettingDefaultHarnessConfig] == config.ShellHarnessConfigName
}

// forceShellGCPBlock ensures agent.AppliedConfig is non-nil and sets its
// GCPIdentity to explicit MetadataMode "block" (ptone/scion#8812 §14).
func forceShellGCPBlock(agent *store.Agent) {
	if agent == nil {
		return
	}
	if agent.AppliedConfig == nil {
		agent.AppliedConfig = &store.AgentAppliedConfig{}
	}
	agent.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{
		MetadataMode: store.GCPMetadataModeBlock,
	}
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
			message: "execution container: human principals only",
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
			message: "execution containers do not support an initial task",
		}
	}
	if opts.TemplateExplicit {
		return &shellCreateDenial{
			status:  http.StatusBadRequest,
			code:    ErrCodeValidationError,
			message: "execution containers do not use templates",
		}
	}
	if opts.TemplateSuppliedShellHC {
		return &shellCreateDenial{
			status:  http.StatusBadRequest,
			code:    ErrCodeValidationError,
			message: "execution containers do not use templates; pass harness_config \"shell\" explicitly without a template",
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

const shellHarnessSwitchRefusalMessage = "cannot switch harness to or from execution container (shell)"

// shellPatchHarnessSwitchError returns a refusal message if a PATCH config
// payload attempts to switch an agent's harness to or from the reserved
// built-in "shell" execution container (ptone/scion#8812 §3.4 S7).
//
// An unchanged echo of "shell" on an existing shell agent is allowed (and
// later stripped by dropFixedConfigKeys). Any attempt to set harness_config,
// harness, or default_harness_config to "shell" on a non-shell agent, or to
// anything other than "shell" (including clearing it) on a shell agent, is
// refused outright. Checks the decoded struct directly and uses
// strings.EqualFold on rawFields so Unicode simple case-folding (e.g. U+017F)
// matches encoding/json and cannot bypass the check.
func shellPatchHarnessSwitchError(agent *store.Agent, cfg *api.ScionConfig, rawFields map[string]json.RawMessage) string {
	if cfg == nil {
		return ""
	}
	isShell := isShellAgent(agent)
	if !isShell {
		if strings.EqualFold(strings.TrimSpace(cfg.HarnessConfig), config.ShellHarnessConfigName) ||
			strings.EqualFold(strings.TrimSpace(cfg.Harness), config.ShellHarnessConfigName) ||
			strings.EqualFold(strings.TrimSpace(cfg.DefaultHarnessConfig), config.ShellHarnessConfigName) {
			return shellHarnessSwitchRefusalMessage
		}
		return ""
	}
	if (cfg.HarnessConfig != "" && cfg.HarnessConfig != config.ShellHarnessConfigName) ||
		(cfg.Harness != "" && cfg.Harness != config.ShellHarnessConfigName) ||
		(cfg.DefaultHarnessConfig != "" && cfg.DefaultHarnessConfig != config.ShellHarnessConfigName) {
		return shellHarnessSwitchRefusalMessage
	}
	for k := range rawFields {
		switch {
		case strings.EqualFold(k, "harness_config") && cfg.HarnessConfig != config.ShellHarnessConfigName:
			return shellHarnessSwitchRefusalMessage
		case strings.EqualFold(k, "harness") && cfg.Harness != config.ShellHarnessConfigName:
			return shellHarnessSwitchRefusalMessage
		case strings.EqualFold(k, "default_harness_config") && cfg.DefaultHarnessConfig != config.ShellHarnessConfigName:
			return shellHarnessSwitchRefusalMessage
		}
	}
	return ""
}

// shellReincarnateHarnessSwitchError returns a refusal message if a
// reincarnate request attempts a harness switch on a shell execution container
// or to the reserved "shell" harness config (ptone/scion#8812 §3.4 S7).
func shellReincarnateHarnessSwitchError(agent *store.Agent, reqHarnessConfig string) string {
	if reqHarnessConfig == "" {
		return ""
	}
	if isShellAgent(agent) || strings.EqualFold(strings.TrimSpace(reqHarnessConfig), config.ShellHarnessConfigName) {
		return shellHarnessSwitchRefusalMessage
	}
	return ""
}

// shellPatchMetadataKeys is the lower-cased allowlist of top-level PATCH
// /agents/{id} keys that are pure non-launch metadata (or optimistic-lock
// control) and keep agent.update on a shell execution container
// (ptone/scion#8812 §3.4 S7). Any other top-level key — including any future
// field added to the PATCH body — fails closed to owner-human ActionAttach.
var shellPatchMetadataKeys = map[string]bool{
	"name":         true,
	"labels":       true,
	"annotations":  true,
	"tasksummary":  true,
	"stateversion": true,
}

// shellPatchHasNonMetadataKeys reports whether raw PATCH body contains any
// top-level JSON key outside shellPatchMetadataKeys. Fails closed (returns
// true) on malformed JSON so unknown or unparseable payloads require attach.
func shellPatchHasNonMetadataKeys(body []byte) bool {
	if len(strings.TrimSpace(string(body))) == 0 {
		return false
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return true
	}
	for k := range top {
		if !shellPatchMetadataKeys[strings.ToLower(k)] {
			return true
		}
	}
	return false
}
