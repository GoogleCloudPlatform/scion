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

package runtimebroker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
)

// harnessPolicyInput describes the harness-config a dispatch's launch will
// use, for enforceHarnessConfigPolicy. Req supplies the name inputs
// (Config.HarnessConfig / Template / Profile) and ProjectPath;
// HydratedTemplatePath and HydratedHCPath are the hub-hydrated paths launch
// will use (empty when none).
type harnessPolicyInput struct {
	Req                  CreateAgentRequest
	HydratedTemplatePath string
	HydratedHCPath       string
}

// enforceHarnessConfigPolicy is the broker's early, side-effect-free
// container-script policy check, shared by create, start and restart. Each
// caller runs it at its single hydration point, against the hydrated
// harness-config when there is one, before any provisioning, start or stop
// side effect. It resolves the harness-config (lookupHarnessConfigDirForPolicy)
// and evaluates it (evaluateHarnessConfigPolicy).
//
// It is an early-out only. The authoritative evaluation is the policy hook
// (harnessConfigPolicyHook), which pkg/agent runs where launch resolves the
// harness-config, with launch's own name resolution and the effective entry.
// A refusal from either refuses the dispatch: this check passing never
// overrides the hook, so where the two disagree the hook's refusal stands
// (fail closed).
//
// When the policy can refuse (allow_container_script_harnesses=false) and a
// hydrated copy is supplied but cannot be loaded, it fails closed with a 500.
// With allow=true every entry passes, so that case is left to launch.
//
// A non-OK decision carries the response (HTTPStatus, Code, Message) and, in
// Detail, the text for logs and dispatch-attempt records; callers write it
// with writeHarnessPolicyRefusal so every path answers identically.
func (s *Server) enforceHarnessConfigPolicy(in harnessPolicyInput) harnessPolicyDecision {
	name, entries, hcDir, ok, err := s.lookupHarnessConfigDirForPolicy(in.Req, in.HydratedTemplatePath, in.HydratedHCPath)
	if err != nil {
		if s.config.AllowContainerScriptHarnesses {
			return harnessPolicyDecision{OK: true}
		}
		return harnessPolicyDecision{
			OK:         false,
			Code:       ErrCodeRuntimeError,
			HTTPStatus: 500,
			// The detail names a broker path; it goes to the log and the
			// dispatch attempt, not the response, as for launch errors.
			Message: "Failed to evaluate harness-config policy",
			Detail:  "Failed to evaluate harness-config policy: " + err.Error(),
		}
	}
	if !ok {
		return harnessPolicyDecision{OK: true}
	}
	// When the policy can refuse, an unusable provisioner is reported
	// first: it is the more specific error, and enabling container-script
	// harnesses would only surface it at launch (ptone/scion#611). With
	// container-script harnesses allowed, launch reports it. A settings-only
	// entry (no directory) is not checked: launch never takes a provisioner
	// from settings.
	if !s.config.AllowContainerScriptHarnesses && hcDir != nil {
		for _, entry := range entries {
			if ue := harness.CheckProvisionerUsable(name, hcDir, entry); ue != nil {
				return harnessPolicyDecision{
					OK:         false,
					Code:       ErrCodeHarnessConfigUnusable,
					HTTPStatus: http.StatusUnprocessableEntity,
					Message:    ue.PublicMessage(),
					Detail:     ue.Error(),
				}
			}
		}
	}
	// Any refused entry refuses the dispatch.
	for _, entry := range entries {
		if d := s.evaluateHarnessConfigPolicy(name, entry); !d.OK {
			return d
		}
	}
	return harnessPolicyDecision{OK: true}
}

// writeHarnessPolicyRefusal writes a non-OK harness-config policy decision
// and logs it. op names the dispatch path for the log. details is nil for a
// refusal raised before any side effect (the early check, create admission,
// Provision on create); a refusal raised from inside Manager.Start passes
// startFailureDetails, so the hub sees the start-attempted marker and the
// run the runtime holds now, as for any other failure from inside Start.
func (s *Server) writeHarnessPolicyRefusal(w http.ResponseWriter, d harnessPolicyDecision, op, agentID string, details map[string]interface{}) {
	s.agentLifecycleLog.Warn("Harness-config policy refused dispatch",
		"op", op, "agent_id", agentID, "status", d.HTTPStatus, "detail", d.detail())
	writeError(w, d.HTTPStatus, d.Code, d.Message, details)
}

// harnessPolicyRefusal is the error the broker's harness-config policy hook
// returns to pkg/agent; pkg/agent wraps it with agent.ErrHarnessConfigPolicy.
// It carries the decision so every dispatch path writes the same refusal.
type harnessPolicyRefusal struct {
	d harnessPolicyDecision
}

func (e *harnessPolicyRefusal) Error() string { return e.d.detail() }

// harnessConfigPolicyHook returns the broker's container-script policy as a
// config.HarnessConfigPolicyFunc. Create, start and restart attach it to the
// launch context, so the policy is evaluated where launch resolves the
// harness-config (pkg/agent: template and harness-config resolution and each
// harness construction), against the effective entry exactly as it will run.
// enforceHarnessConfigPolicy remains the early, side-effect-free refusal
// ahead of that.
func (s *Server) harnessConfigPolicyHook() config.HarnessConfigPolicyFunc {
	return func(name string, entry config.HarnessConfigEntry) error {
		if d := s.evaluateHarnessConfigPolicy(name, entry); !d.OK {
			return &harnessPolicyRefusal{d: d}
		}
		return nil
	}
}

// withHarnessConfigPolicy attaches the broker's harness-config policy hook to
// ctx when the policy can refuse (allow_container_script_harnesses=false).
// With allow=true every harness-config is accepted, so nothing is attached and
// launch behaves exactly as with no policy.
func (s *Server) withHarnessConfigPolicy(ctx context.Context) context.Context {
	if s.config.AllowContainerScriptHarnesses {
		return ctx
	}
	return config.ContextWithHarnessConfigPolicy(ctx, s.harnessConfigPolicyHook())
}

// harnessPolicyRefusalFrom reports whether err is (or wraps) a harness-config
// policy refusal, returning the decision to answer it with:
//   - a refusal from harnessConfigPolicyHook carries its own decision (the
//     actionable allow_container_script_harnesses message);
//   - any other agent.ErrHarnessConfigPolicy error (the policy is attached
//     but the harness-config could not be evaluated) is answered with a 403
//     and a neutral message naming the harness-config and the setting; the
//     underlying error goes to Detail only.
func harnessPolicyRefusalFrom(err error) (harnessPolicyDecision, bool) {
	var r *harnessPolicyRefusal
	if errors.As(err, &r) {
		return r.d, true
	}
	if errors.Is(err, agent.ErrHarnessConfigPolicy) {
		msg := "Harness configuration not permitted by policy (allow_container_script_harnesses)"
		var ne *agent.HarnessConfigNotEvaluatedError
		if errors.As(err, &ne) {
			msg = fmt.Sprintf("harness-config %q not permitted by policy (allow_container_script_harnesses)", ne.Name)
		}
		return harnessPolicyDecision{
			OK:         false,
			Code:       ErrCodeForbidden,
			HTTPStatus: http.StatusForbidden,
			Message:    msg,
			Detail:     err.Error(),
		}, true
	}
	return harnessPolicyDecision{}, false
}

// harnessPolicyInputForStart builds the policy input for start and restart
// from the built start context: the hydrated harness-config launch will use
// (opts.HarnessConfigPath, set by buildStartContext's hydration) and a
// harness-config name: the dispatch's opts.HarnessConfig, else the agent's
// saved agent-info harness-config, else the settings default (resolved by
// lookupHarnessConfigDirForPolicy). That is the early check's view; names Start
// derives from the stored or template config are evaluated by the policy hook
// where Start resolves them. The template chain is taken from opts.Template,
// as harness.Resolve's caller does.
func harnessPolicyInputForStart(opts api.StartOptions, agentName string) harnessPolicyInput {
	name := opts.HarnessConfig
	if name == "" && opts.ProjectPath != "" {
		name = agent.GetSavedHarnessConfig(agentName, harnessConfigProjectDir(opts.ProjectPath))
	}
	// The project path is passed as given; lookupHarnessConfigDirForPolicy
	// resolves it to the project dir launch uses.
	req := CreateAgentRequest{
		ProjectPath: opts.ProjectPath,
		Config: &CreateAgentConfig{
			HarnessConfig: name,
			Profile:       opts.Profile,
		},
	}
	var hydratedTemplatePath string
	if filepath.IsAbs(opts.Template) {
		hydratedTemplatePath = opts.Template
	} else {
		req.Config.Template = opts.Template
	}
	return harnessPolicyInput{
		Req:                  req,
		HydratedTemplatePath: hydratedTemplatePath,
		HydratedHCPath:       opts.HarnessConfigPath,
	}
}

// lookupHarnessConfigDirForPolicy resolves the harness-config entries this
// dispatch's launch may use, for evaluateHarnessConfigPolicy, and the
// directory they came from (nil for a settings entry). Directories are
// resolved through config.ResolveHarnessConfigDir, the ordering launch and
// extractRequiredEnvKeys use: the hub-hydrated copy (hydratedHCPath) when
// supplied, else template-bundled, project, then global directories, with the
// template chain taken from hydratedTemplatePath when supplied (else the
// request's template slug). A settings harness_configs entry is the last
// fallback. ok is false when no harness-config was specified or could be
// found, which short-circuits the policy check (no policy applies).
//
// Without a hydrated copy, the project tier is searched in the single resolved
// project dir (harnessConfigProjectDir) that provisioning and launch use. The
// result is returned as a slice so the caller evaluates every entry.
//
// err is non-nil only when hydratedHCPath is set but cannot be loaded. That
// is not a "not found" case: launch would use exactly that directory, so
// the caller must not fall back to evaluating some other entry (fail
// closed) when the policy can refuse.
func (s *Server) lookupHarnessConfigDirForPolicy(req CreateAgentRequest, hydratedTemplatePath, hydratedHCPath string) (string, []config.HarnessConfigEntry, *config.HarnessConfigDir, bool, error) {
	projectDir := harnessConfigProjectDir(req.ProjectPath)

	var settings *config.VersionedSettings
	if projectDir != "" {
		if vs, _, err := config.LoadEffectiveSettings(projectDir); err == nil {
			settings = s.settingsView(vs)
		}
	}

	name := policyHarnessConfigName(req, settings)

	// A hydrated copy is what launch loads whatever name it resolves, so it
	// is evaluated even when no name resolves here.
	if hydratedHCPath != "" {
		hcDir, err := config.ResolveHarnessConfigDir(hydratedHCPath, name, "")
		if err != nil {
			return name, nil, nil, false, fmt.Errorf("hydrated harness-config %q could not be loaded: %w", name, err)
		}
		if name == "" {
			name = hcDir.Name
		}
		return name, []config.HarnessConfigEntry{hcDir.Config}, hcDir, true, nil
	}
	if name == "" {
		return "", nil, nil, false, nil
	}

	// The template chain is resolved against the project path as given, as
	// launch resolves it; the project tier uses the resolved project dir.
	templateForChain := hydratedTemplatePath
	if templateForChain == "" && req.Config != nil {
		templateForChain = req.Config.Template
	}
	if projectDir != "" {
		hcDir, err := config.ResolveHarnessConfigDir("", name, projectDir, templateChainPaths(templateForChain, req.ProjectPath)...)
		if err == nil && hcDir != nil {
			return name, []config.HarnessConfigEntry{hcDir.Config}, hcDir, true, nil
		}
	}
	if settings != nil {
		if hcfg, ok := settings.HarnessConfigs[name]; ok {
			return name, []config.HarnessConfigEntry{hcfg}, nil, true, nil
		}
	}
	return name, nil, nil, false, nil
}

// policyHarnessConfigName is the harness-config name the early policy check
// evaluates: the dispatch's explicit harness-config, else the profile or
// settings default (config.ResolveHarnessConfigName's CLI, profile and
// settings tiers). Names launch derives from template, inline or stored
// config are evaluated where launch resolves them (CheckHarnessConfigPolicy
// in pkg/agent).
func policyHarnessConfigName(req CreateAgentRequest, settings *config.VersionedSettings) string {
	inputs := config.HarnessConfigInputs{Settings: settings}
	if req.Config != nil {
		inputs.CLIFlag = req.Config.HarnessConfig
		inputs.ProfileName = req.Config.Profile
	}
	res, err := config.ResolveHarnessConfigName(inputs)
	if err != nil {
		return ""
	}
	return res.Name
}

// harnessConfigProjectDir returns the single resolved project dir that
// harness-config resolution uses for provisioning, launch and the policy
// gate: config.GetResolvedProjectDir(projectPath), which maps a project root
// to <root>/.scion or its external split-storage directory, and an empty path
// to the project found by walking up from the working directory, else the
// global directory. If resolution fails the path is returned as given
// (launch fails on the same error).
func harnessConfigProjectDir(projectPath string) string {
	if dir, err := config.GetResolvedProjectDir(projectPath); err == nil && dir != "" {
		return dir
	}
	return projectPath
}

// templateChainPaths returns the on-disk paths of template's chain resolved
// against searchPath, in merge order, for use as the template-bundled tier of
// config.ResolveHarnessConfigDir. template may be a slug or an absolute
// (hydrated) path; an empty searchPath resolves as config.GetTemplateChainInProject
// does for it (global templates). Returns nil when template is empty or the
// chain does not resolve.
func templateChainPaths(template, searchPath string) []string {
	if template == "" {
		return nil
	}
	chain, err := config.GetTemplateChainInProject(template, searchPath)
	if err != nil {
		return nil
	}
	paths := make([]string, 0, len(chain))
	for _, tpl := range chain {
		paths = append(paths, tpl.Path)
	}
	return paths
}

// harnessPolicyDecision describes the outcome of a per-dispatch policy check.
// Code is the API error code (matches errors.go constants); HTTPStatus is the
// HTTP status the broker should return; Message is the user-facing error
// string. An ok=true decision means the dispatch may proceed.
type harnessPolicyDecision struct {
	OK         bool
	Code       string
	HTTPStatus int
	Message    string
	// Detail, when set, is a fuller description for logs and dispatch
	// attempts than the user-facing Message.
	Detail string
}

// detail returns Detail, or Message when no separate detail was set.
func (d harnessPolicyDecision) detail() string {
	if d.Detail != "" {
		return d.Detail
	}
	return d.Message
}

// evaluateHarnessConfigPolicy enforces broker-level dispatch policy on the
// resolved harness-config. Today it gates container-script provisioners
// behind ServerConfig.AllowContainerScriptHarnesses (defaults to true).
// Set AllowContainerScriptHarnesses=false to block container-script
// dispatches on this broker.
//
// Caller passes the resolved harness-config name (for logs/error text) and
// the parsed config entry. Returns a non-OK decision when the dispatch
// should be refused.
//
// The policy is intentionally minimal in v1 — scripted provisioning is gated.
// Future additions (e.g., trusted_harness_config_publishers) can extend this
// function without touching the dispatch path.
//
// GUARD: from the policy hook, entry is the effective entry launch runs
// (harness.EffectiveConfig, which applies the settings overlay including
// profile harness_overrides). From the early check
// (enforceHarnessConfigPolicy), entry is the directory (or settings) entry
// WITHOUT that merge. Today the policy reads only Provisioner, which overrides
// cannot set, so both agree. Any policy field that a profile harness_override
// can mutate (image, image pull policy, user, env, volumes, auth type,
// resources) MUST be evaluated only after the override merge (i.e. from the
// hook, or with the early check applying the merge too), or the early check
// will judge a different value than the one launch uses.
// TestEvaluateHarnessConfigPolicy_IgnoresOverrideMutableFields fails if this
// function starts depending on such a field.
func (s *Server) evaluateHarnessConfigPolicy(harnessConfigName string, entry config.HarnessConfigEntry) harnessPolicyDecision {
	if entry.Provisioner == nil {
		return harnessPolicyDecision{OK: true}
	}
	if s.config.AllowContainerScriptHarnesses {
		return harnessPolicyDecision{OK: true}
	}
	return harnessPolicyDecision{
		OK:         false,
		Code:       ErrCodeForbidden,
		HTTPStatus: 403,
		Message: fmt.Sprintf(
			"harness-config %q uses scripted provisioning but this broker has allow_container_script_harnesses=false. Set broker.allow_container_script_harnesses=true (SCION_SERVER_BROKER_ALLOWCONTAINERSCRIPTHARNESSES=true) to enable.",
			harnessConfigName,
		),
	}
}
