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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const dispatchAgentPermission = "agent.create"

// dispatchAgentAction implements FireAction and FirePayloadValidator for
// deferred "dispatch_agent" fires executed through createAgentHeadless.
type dispatchAgentAction struct {
	srv *Server
}

type dispatchAgentAuthorized struct {
	payload        DispatchAgentEventPayload
	auth           FireAuthority
	identity       Identity
	creatorName    string
	edgeCeiling    store.EffectCeiling
	edgeProvenance store.AuthorityProvenance
}

func (a *dispatchAgentAction) Type() string {
	return "dispatch_agent"
}

func (a *dispatchAgentAction) Permission() string {
	return dispatchAgentPermission
}

func (a *dispatchAgentAction) Detaches(_ hubfire.FireRequest) bool {
	return false
}

// Validate refuses in Stage 1: triggers refuse dispatch_agent at authoring
// until Stage 2/3, and schedule authoring handlers continue authorizing
// through authorizeScheduledDispatchAgentAuthoring.
func (a *dispatchAgentAction) Validate(_ context.Context, _ Identity, _ string, _ string) (string, error) {
	return "", errors.New("dispatch_agent action authoring validation is not wired in Stage 1")
}

func unmarshalDispatchAgentFirePayload(raw string) (DispatchAgentEventPayload, error) {
	var payload DispatchAgentEventPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return DispatchAgentEventPayload{}, fmt.Errorf("invalid dispatch_agent payload: %w", err)
	}
	if payload.AgentName == "" {
		return DispatchAgentEventPayload{}, fmt.Errorf("dispatch_agent payload: agentName is required")
	}
	if !templateRefAllowed(payload.Template) {
		return DispatchAgentEventPayload{}, fmt.Errorf("dispatch_agent payload: %s", templateRefRefusal)
	}
	return payload, nil
}

func (a *dispatchAgentAction) ValidatePayload(req hubfire.FireRequest) error {
	_, err := unmarshalDispatchAgentFirePayload(req.Payload)
	return err
}

func dispatchAgentRefusalReason(req hubfire.FireRequest, err error) *FireRefusal {
	reason := "dispatch_agent refused"
	if req.Authority.DefinitionKind == hubfire.DefinitionKindSchedule ||
		req.Authority.DefinitionKind == hubfire.DefinitionKindScheduledEvent {
		reason = err.Error()
	}
	return &FireRefusal{
		Reason: reason,
		Cause:  err,
	}
}

// authorizeDispatchAgentFire verifies live agent.create authorization,
// ScopeAgentCreate (for agent principals), and CanDelegate under the resolved
// revision identity.
func (s *Server) authorizeDispatchAgentFire(
	ctx context.Context,
	projectID string,
	fireID string,
	definitionKind string,
	auth FireAuthority,
	identity Identity,
	noAuth bool,
) error {
	if auth.PrincipalKind == store.DelegationPrincipalAgent {
		agentIdent, ok := identity.(AgentIdentity)
		if !ok || !agentIdent.HasScope(ScopeAgentCreate) {
			return fmt.Errorf("scheduled dispatch principal agent %q missing required scope: %s", auth.PrincipalID, ScopeAgentCreate)
		}
	}
	var project *store.Project
	if p, err := s.store.GetProject(ctx, projectID); err == nil {
		project = p
	}
	delegatedRole, _, _, err := s.resolveHeadlessPreCapRole(ctx, project, auth, "", noAuth)
	if err != nil {
		return err
	}

	if decision := s.agentCreateDecision(ctx, identity, projectID); !decision.Allowed {
		return fmt.Errorf("scheduled dispatch principal %s %q is not authorized to create agents in project %q: %s",
			auth.PrincipalKind, auth.PrincipalID, projectID, decision.Reason)
	}

	targetType := "scheduled_dispatch"
	if definitionKind == hubfire.DefinitionKindTrigger {
		targetType = "trigger_dispatch"
	}
	delegateDecision := s.authzService.CanDelegate(ctx, identity, GrantDescriptor{
		Type:      GrantTypeAgentDelegation,
		AgentRole: string(delegatedRole),
		ProjectID: projectID,
		ScopeType: store.RoleScopeProject,
		ScopeID:   projectID,
	})
	if !delegateDecision.Allowed {
		s.emitMutationAudit(ctx, &store.MutationAuditRecord{
			MutationType:       mutationTypeAgentDelegation,
			ActorPrincipalKind: auth.PrincipalKind,
			ActorPrincipalID:   auth.PrincipalID,
			TargetType:         targetType,
			TargetID:           fireID,
			CanDelegateResult:  "deny",
			CanDelegateReason:  delegateDecision.Reason,
		})
		return fmt.Errorf("scheduled dispatch principal %s %q failed CanDelegate at fire time: %s",
			auth.PrincipalKind, auth.PrincipalID, delegateDecision.Reason)
	}
	return nil
}

func (a *dispatchAgentAction) Authorize(
	ctx context.Context,
	req hubfire.FireRequest,
	auth FireAuthority,
	identity Identity,
) (FireAuthorized, *FireRefusal, error) {
	if a == nil || a.srv == nil {
		return nil, nil, errors.New("dispatch_agent action: server not configured")
	}
	s := a.srv
	payload, err := unmarshalDispatchAgentFirePayload(req.Payload)
	if err != nil {
		return nil, nil, err
	}

	fireID := req.Authority.FireID
	if err := s.authorizeDispatchAgentFire(ctx, req.ProjectID, fireID, req.Authority.DefinitionKind, auth, identity, payload.NoAuth); err != nil {
		slog.Warn("Scheduler: dispatch_agent refused at fire time",
			"eventID", fireID,
			"definitionKind", req.Authority.DefinitionKind,
			"definitionID", req.Authority.DefinitionID,
			"projectID", req.ProjectID,
			"principal_kind", auth.PrincipalKind,
			"principal_id", auth.PrincipalID,
			"authorization_revision", auth.Revision,
			"error", err)
		return nil, dispatchAgentRefusalReason(req, err), nil
	}

	edgeCeiling, edgeProvenance, err := s.fireEffectCeiling(ctx, auth)
	if err != nil {
		if errors.Is(err, errScheduledAuthorityDenied) {
			slog.Warn("Scheduler: dispatch_agent ceiling denied at fire time",
				"eventID", fireID,
				"definitionKind", req.Authority.DefinitionKind,
				"definitionID", req.Authority.DefinitionID,
				"projectID", req.ProjectID,
				"error", err)
			return nil, dispatchAgentRefusalReason(req, err), nil
		}
		return nil, nil, err
	}

	if err := s.fireStanding(ctx, auth, nil); err != nil {
		return nil, dispatchAgentRefusalReason(req, err), nil
	}

	return &dispatchAgentAuthorized{
		payload:        payload,
		auth:           auth,
		identity:       identity,
		creatorName:    fireCreatorName(auth),
		edgeCeiling:    edgeCeiling,
		edgeProvenance: edgeProvenance,
	}, nil, nil
}

func (a *dispatchAgentAction) Fire(ctx context.Context, req hubfire.FireRequest, authorized FireAuthorized) (hubfire.FireOutcome, error) {
	if a == nil || a.srv == nil {
		return hubfire.FireOutcome{}, errors.New("dispatch_agent action: server not configured")
	}
	da, ok := authorized.(*dispatchAgentAuthorized)
	if !ok || da == nil {
		return hubfire.FireOutcome{}, errors.New("dispatch_agent action: missing authorized state")
	}

	staleness := time.Since(req.FireAt)
	if !req.FireAt.IsZero() && staleness > 1*time.Minute {
		slog.Warn("Scheduler: firing stale dispatch_agent event",
			"eventID", req.Authority.FireID,
			"agentName", da.payload.AgentName,
			"scheduledFor", req.FireAt.UTC().Format(time.RFC3339),
			"staleness", staleness.Truncate(time.Second).String())
	}

	agent, err := a.srv.createAgentHeadless(ctx, HeadlessCreate{
		ProjectID:      req.ProjectID,
		Authority:      da.auth,
		Identity:       da.identity,
		CreatorName:    da.creatorName,
		EdgeCeiling:    da.edgeCeiling,
		EdgeProvenance: da.edgeProvenance,
		NoAuth:         da.payload.NoAuth,
		Name:           da.payload.AgentName,
		Template:       da.payload.Template,
		Task:           da.payload.Task,
		Branch:         da.payload.Branch,
		Detached:       true,
		NotifyBlocked: func(ctx context.Context, existing *store.Agent, placementRefused bool) {
			a.srv.notifyFireBlocked(ctx, req, existing, placementRefused)
		},
	})
	if err != nil {
		return hubfire.FireOutcome{}, err
	}
	var resultRef string
	if agent != nil {
		resultRef = agent.ID
	}
	return hubfire.FireOutcome{
		Status:    hubfire.FireStatusFired,
		ResultRef: resultRef,
	}, nil
}
