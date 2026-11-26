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
	"context"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hostedguard"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// SetHostedMode makes the dispatcher check member-written agent config
// before every create, provision, start and restart dispatch
// (hostedConfigPrecheck). Set on a hosted hub (not workstation mode),
// whatever mode the receiving broker runs in.
func (d *HTTPAgentDispatcher) SetHostedMode(hosted bool) {
	d.hostedMode = hosted
}

// SetHostedHarnessConfigCheck sets the check hostedConfigPrecheck applies
// to the hub harness config an agent is dispatched with
// (Server.checkStoredHostedHarnessConfig). Nil checks nothing.
func (d *HTTPAgentDispatcher) SetHostedHarnessConfigCheck(check func(ctx context.Context, harnessConfigID, runtimeKind string) error) {
	d.hostedHarnessConfigCheck = check
}

// SetHostedWorkspaceResolver sets the resolver hostedConfigPrecheck uses to
// verify stored absolute workspaces on a hosted hub
// (Server.projectWorkspaceResolved). Nil fails closed for absolute workspaces.
func (d *HTTPAgentDispatcher) SetHostedWorkspaceResolver(resolver func(project *store.Project) (projectfs.Resolved, error)) {
	d.hostedWorkspaceResolver = resolver
}

// hostedConfigPrecheck refuses a dispatch on a hosted hub when the agent's
// stored inline config (written by the member who created or updated the
// agent), the project-scope hub harness config it is dispatched with, a
// harness config bundled in the project-scope hub template it uses, or its
// stored absolute workspace path is not permitted, so config stored before
// the check existed is refused at its next dispatch. The runtime kind is the
// one the target broker reports for the agent's profile (hostedRuntimeKind);
// when it is not known, rules that depend on it are left to the broker.
func (d *HTTPAgentDispatcher) hostedConfigPrecheck(ctx context.Context, agent *store.Agent) error {
	if !d.hostedMode || agent == nil || agent.AppliedConfig == nil {
		return nil
	}
	if err := d.hostedWorkspacePrecheck(ctx, agent); err != nil {
		return err
	}
	kind := hostedRuntimeKind(ctx, d.store, agent)
	if err := hostedConfigRefusal(agent.AppliedConfig.InlineConfig, kind); err != nil {
		return err
	}
	if d.hostedHarnessConfigCheck != nil && agent.AppliedConfig.HarnessConfigID != "" {
		if err := hostedCheckRefusal(d.hostedHarnessConfigCheck(ctx, agent.AppliedConfig.HarnessConfigID, kind)); err != nil {
			return err
		}
	}
	if d.hostedTemplateCheck != nil && agent.AppliedConfig.TemplateID != "" {
		return hostedCheckRefusal(d.hostedTemplateCheck(ctx, agent.AppliedConfig.TemplateID, kind))
	}
	return nil
}

// hostedWorkspacePrecheck verifies that a stored absolute workspace path on a
// hosted hub matches the project's resolved workspace from the layout
// authority. Pre-existing rows whose stored absolute workspace does not match
// are refused before dispatch with a typed workspace_not_permitted error.
func (d *HTTPAgentDispatcher) hostedWorkspacePrecheck(ctx context.Context, agent *store.Agent) error {
	raw := agent.AppliedConfig.Workspace
	if raw == "" || !isAbsWorkspace(raw) {
		return nil
	}
	projectInfo, err := d.resolveDispatchProjectInfo(ctx, agent)
	if err != nil {
		return err
	}
	ws := effectiveDispatchWorkspace(raw, projectInfo.projectPath)
	if !isAbsWorkspace(ws) {
		return nil
	}
	var project *store.Project
	if agent.ProjectID != "" {
		p, getErr := d.store.GetProject(ctx, agent.ProjectID)
		if getErr == nil {
			project = p
		}
	}
	var want projectfs.Resolved
	var resolveErr error
	if d.hostedWorkspaceResolver != nil {
		want, resolveErr = d.hostedWorkspaceResolver(project)
	} else {
		resolveErr = errors.New("project workspace resolver is not configured")
	}
	if resolveErr != nil || want.HostPath == "" || !sameWorkspacePath(ws, want.HostPath) {
		slog.Warn("Hosted dispatch refused: stored absolute workspace does not match resolved project workspace",
			"agent_id", agent.ID,
			"project_id", agent.ProjectID,
			"code", api.LaunchCodeWorkspaceNotPermitted)
		return &RuntimeTargetRefusal{
			Code:    api.LaunchCodeWorkspaceNotPermitted,
			Status:  http.StatusBadRequest,
			Message: "stored absolute workspace does not match the project workspace",
			Details: map[string]interface{}{"field": "workspace"},
		}
	}
	return nil
}

// isAbsWorkspace reports whether p is an absolute Unix or Windows path
// (including drive-letter and UNC paths even when checked on Unix).
func isAbsWorkspace(p string) bool {
	if p == "" {
		return false
	}
	if filepath.IsAbs(p) || strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`) {
		return true
	}
	if len(p) >= 2 && p[1] == ':' && ((p[0] >= 'a' && p[0] <= 'z') || (p[0] >= 'A' && p[0] <= 'Z')) {
		return true
	}
	return false
}

// sameWorkspacePath reports whether a and b refer to the same path, compared
// cleaned and, when both resolve, with symlinks evaluated.
func sameWorkspacePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

// hostedCheckRefusal maps a stored harness-config or template check's
// refusal to a 400 hosted_config_refused *RuntimeTargetRefusal; other errors
// are returned as they are.
func hostedCheckRefusal(err error) error {
	var he *hostedHarnessConfigError
	if errors.As(err, &he) {
		return &RuntimeTargetRefusal{Code: hostedguard.ErrorCode, Status: http.StatusBadRequest, Message: he.Error()}
	}
	return err
}

// hostedConfigRefusal checks member-written agent config with
// hostedguard for runtimeKind ("" when not known) and returns its refusal
// as a 400 hosted_config_refused *RuntimeTargetRefusal, which create, start
// and restart relay as is.
func hostedConfigRefusal(cfg *api.ScionConfig, runtimeKind string) error {
	if err := hostedguard.CheckMemberConfig(cfg, hostedguard.Origin{Kind: hostedguard.OriginAgentConfig}, runtimeKind); err != nil {
		return &RuntimeTargetRefusal{Code: hostedguard.ErrorCode, Status: http.StatusBadRequest, Message: err.Error()}
	}
	return nil
}

// hostedRuntimeKind is the hostedguard runtime kind of the profile agent
// runs on at its broker, from the profile type the broker reports
// (store.BrokerProfile.Type, resolved like the dispatch's other profile
// checks: resolveAgentRuntimeProfileType): hostedguard.RuntimeKubernetes for
// a Kubernetes type, the reported type otherwise, and "" when the broker,
// the profile or its type is not known, which leaves gcs to the broker.
func hostedRuntimeKind(ctx context.Context, st store.Store, agent *store.Agent) string {
	if agent == nil || agent.RuntimeBrokerID == "" {
		return ""
	}
	broker, err := st.GetRuntimeBroker(ctx, agent.RuntimeBrokerID)
	if err != nil || broker == nil {
		return ""
	}
	profile := ""
	if agent.AppliedConfig != nil {
		profile = agent.AppliedConfig.Profile
	}
	_, typ, ok := resolveAgentRuntimeProfileType(broker, profile)
	typ = strings.ToLower(strings.TrimSpace(typ))
	switch {
	case !ok || typ == "":
		return ""
	case isKubernetesRuntimeType(typ):
		return hostedguard.RuntimeKubernetes
	default:
		return typ
	}
}
