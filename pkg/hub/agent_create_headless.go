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
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/labels"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Fixed allow-list of event data environment variables accepted by
// HeadlessCreate.EventEnv (Stage 1 design §5.7 constraint 3 & 4).
const (
	EventEnvKeyTriggerEventID = "SCION_TRIGGER_EVENT_ID"
	EventEnvKeyTriggerID      = "SCION_TRIGGER_ID"

	// maxHeadlessEventFileBytes bounds HeadlessCreate.EventFile to the 16 KiB
	// trigger envelope cap (Stage 1 design §5.7).
	maxHeadlessEventFileBytes = 16 * 1024
)

var (
	errDisallowedHeadlessEventEnvKey = errors.New("disallowed headless event env key")
	errInvalidHeadlessEventEnvValue  = errors.New("headless event env value contains NUL byte")
	errHeadlessEventFileTooLarge     = errors.New("headless create event file exceeds 16 KiB limit")
	errHeadlessEventFileUnsupported  = errors.New("headless create event file is not wired before homeFiles channel restack")
)

// HeadlessMissingEnvError is the content-free error returned when a headless
// agent create still has unsatisfied required environment variables after
// DispatchAgentCreateWithGather's as-needed second pass. Only the count is
// carried here so FireOutcome.Error never exposes variable names or values;
// key names (never values) are recorded in the compensation audit and slog.
type HeadlessMissingEnvError struct {
	Count int
}

func (e *HeadlessMissingEnvError) Error() string {
	return fmt.Sprintf("missing required environment variables (%d)", e.Count)
}

// HeadlessCreate describes a non-interactive agent creation executed under an
// already-resolved FireAuthority (scheduled dispatch_agent today; trigger
// dispatch_agent in Stage 2/3).
type HeadlessCreate struct {
	ProjectID      string
	Authority      FireAuthority
	Identity       Identity
	CreatorName    string
	EdgeCeiling    store.EffectCeiling
	EdgeProvenance store.AuthorityProvenance

	// Credential and lineage posture (Stage 1 design rev 8.27,
	// headless-identity-note.md §2 & §2.3). When AgentRole is empty,
	// createAgentHeadless resolves the role through the project/hub default
	// ladder capped by scion.io/max-agent-role, the edge ceiling, and (for agent
	// principals) the parent agent's role. NoAuth is true when explicitly
	// requested (e.g. payload.noAuth) or when the resolved role is AgentRoleNone.
	// When OwnerID and Ancestry are empty, they are derived from Authority
	// exactly as an interactive create by that principal records them.
	AgentRole AgentRole
	NoAuth    bool
	OwnerID   string
	Ancestry  []string

	// Trusted authoring-time values (Stage 1 design §5.8).
	Name     string
	Template string
	Task     string
	Branch   string
	Labels   map[string]string // validated by pkg/labels (<=16, <=63 chars)

	// GitHubToken reserves the Stage 2 P1c dispatch token precedence preference
	// ("" | "installation" | "user_preferred"; "" keeps today's dispatch
	// precedence; Stage 1 design rev 8.28).
	GitHubToken string

	// Data-only channels (never material, never participating in secret/env
	// resolution).
	EventEnv  map[string]string // allow-listed keys only: SCION_TRIGGER_EVENT_ID, SCION_TRIGGER_ID
	EventFile []byte            // ~/.scion/trigger-event.json, <= 16 KiB; nil in Stage 1

	Detached      bool
	NotifyBlocked func(ctx context.Context, existing *store.Agent, placementRefused bool)
}

func validateHeadlessEventEnv(env map[string]string) error {
	for k, v := range env {
		switch k {
		case EventEnvKeyTriggerEventID, EventEnvKeyTriggerID:
		default:
			return fmt.Errorf("%w: %q", errDisallowedHeadlessEventEnvKey, k)
		}
		if strings.IndexByte(v, 0) >= 0 {
			return fmt.Errorf("%w: key %q", errInvalidHeadlessEventEnvValue, k)
		}
	}
	return nil
}

func validateHeadlessEventFile(content []byte) error {
	if len(content) > maxHeadlessEventFileBytes {
		return fmt.Errorf("%w: %d bytes exceeds maximum of %d", errHeadlessEventFileTooLarge, len(content), maxHeadlessEventFileBytes)
	}
	if len(content) > 0 {
		return errHeadlessEventFileUnsupported
	}
	return nil
}

// projectDefaultAgentRole returns the project's default agent role annotation,
// falling back to the hub default and then AgentRoleFull, matching
// createAgentInProject (handlers_agents_core.go).
func (s *Server) projectDefaultAgentRole(project *store.Project) AgentRole {
	if project != nil && project.Annotations != nil {
		if defStr, ok := project.Annotations[projectSettingDefaultAgentRole]; ok && defStr != "" {
			if ValidAgentRole(AgentRole(defStr)) {
				return AgentRole(defStr)
			}
		}
	}
	if hubDefault := s.hubAgentDefaults().DefaultAgentRole; hubDefault != "" {
		if ValidAgentRole(AgentRole(hubDefault)) {
			return AgentRole(hubDefault)
		}
	}
	return AgentRoleFull
}

func (s *Server) resolveHeadlessOwnerAndAncestry(ctx context.Context, hc HeadlessCreate) (ownerID string, ancestry []string, err error) {
	if hc.OwnerID != "" || len(hc.Ancestry) > 0 {
		return hc.OwnerID, slices.Clone(hc.Ancestry), nil
	}
	switch hc.Authority.PrincipalKind {
	case store.DelegationPrincipalUser:
		if hc.Authority.PrincipalID == "" {
			return "", nil, errors.New("headless create: missing user principal ID")
		}
		return hc.Authority.PrincipalID, []string{hc.Authority.PrincipalID}, nil
	case store.DelegationPrincipalAgent:
		creatorAgent := hc.Authority.agent
		if creatorAgent == nil {
			creatorAgent, err = s.store.GetAgent(ctx, hc.Authority.PrincipalID)
			if err != nil {
				return "", nil, fmt.Errorf("%w: %s: %w", errAgentCreatorUnreadable, hc.Authority.PrincipalID, err)
			}
		}
		anc := make([]string, 0, len(creatorAgent.Ancestry)+1)
		anc = append(anc, creatorAgent.Ancestry...)
		anc = append(anc, creatorAgent.ID)
		return creatorAgent.ID, anc, nil
	default:
		return "", nil, fmt.Errorf("headless create: unsupported principal kind %q", hc.Authority.PrincipalKind)
	}
}

func (s *Server) resolveHeadlessPreCapRole(
	ctx context.Context,
	project *store.Project,
	auth FireAuthority,
	requestedRole AgentRole,
) (preCapRole AgentRole, roleExplicit bool, parentMessageMode string, err error) {
	projectMax := projectMaxAgentRole(project)
	defaultAgentRole := s.projectDefaultAgentRole(project)
	if requestedRole != "" {
		if !ValidAgentRole(requestedRole) {
			return "", false, "", fmt.Errorf("invalid agent role %q", requestedRole)
		}
		roleExplicit = true
	}

	if auth.PrincipalKind == store.DelegationPrincipalAgent {
		var parentRole AgentRole
		if auth.agent != nil {
			parentRole, _ = agentRoleAndScopes(auth.agent)
			parentMessageMode = auth.agent.MessageMode
			if !ValidAgentRole(parentRole) {
				parentRole = AgentRoleBaseline
			}
		} else {
			parentRole, parentMessageMode = s.callerAgentRoleCeiling(ctx, auth.PrincipalID)
		}
		if !roleExplicit {
			requestedRole = parentRole
		}
		if roleExplicit && CompareRoles(requestedRole, parentRole) > 0 {
			return "", false, "", fmt.Errorf("cannot grant sub-agent role %q: parent agent role is %q", requestedRole, parentRole)
		}
		return minRole(requestedRole, parentRole, projectMax), roleExplicit, parentMessageMode, nil
	}

	if !roleExplicit {
		requestedRole = defaultAgentRole
	}
	if roleExplicit && CompareRoles(requestedRole, projectMax) > 0 {
		return "", false, "", fmt.Errorf("cannot grant agent role %q: project maximum is %q", requestedRole, projectMax)
	}
	return minRole(requestedRole, projectMax), roleExplicit, "", nil
}

// createAgentHeadless creates and starts an agent non-interactively under
// hc.Authority and hc.Identity, mirroring an interactive create by the same
// principal (headless-identity-note.md §1–§2). Callers must resolve recorded
// authority, live create/delegation authorization, and standing before invoking
// this method.
//
// Dispatch runs through DispatchAgentCreateWithGather (buildCreateRequestFor ->
// resolveEnvFromStorageWithSource / resolveSecrets plus the as-needed second
// pass resolveAsNeededForKeys; NoAuth agents set req.NoAuth and skip secrets
// and broker gather). Any remaining EnvRequirements().Needs after the second
// pass fails the create: runtime files are deleted, minted credentials are
// revoked, the agent row and its delegation edge are compensated via
// cleanupFailedCreate (createStageMissingEnv), key names (never values) are
// written to the compensation audit and slog, and a content-free
// HeadlessMissingEnvError ("missing required environment variables (N)") is
// returned.
func (s *Server) createAgentHeadless(ctx context.Context, hc HeadlessCreate) (*store.Agent, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("headless create: server not configured")
	}
	if hc.Identity == nil {
		return nil, errors.New("headless create: missing creator identity")
	}
	if hc.Name == "" {
		return nil, fmt.Errorf("dispatch_agent payload: agentName is required")
	}
	templateName := hc.Template
	if !templateRefAllowed(templateName) {
		return nil, fmt.Errorf("dispatch_agent payload: %s", templateRefRefusal)
	}
	if hc.AgentRole != "" && !ValidAgentRole(hc.AgentRole) {
		return nil, fmt.Errorf("invalid agent role %q", hc.AgentRole)
	}
	if err := labels.Validate(hc.Labels); err != nil {
		return nil, fmt.Errorf("invalid labels: %w", err)
	}
	if err := validateHeadlessEventEnv(hc.EventEnv); err != nil {
		return nil, err
	}
	if err := validateHeadlessEventFile(hc.EventFile); err != nil {
		return nil, err
	}

	edgeCeiling := hc.EdgeCeiling
	edgeProvenance := hc.EdgeProvenance
	if edgeCeiling.Kind == "" {
		var err error
		edgeCeiling, edgeProvenance, err = s.fireEffectCeiling(ctx, hc.Authority)
		if err != nil {
			return nil, err
		}
	}

	slug, err := api.ValidateAgentName(hc.Name)
	if err != nil {
		return nil, fmt.Errorf("invalid agent name %q: %w", hc.Name, err)
	}

	project, err := s.store.GetProject(ctx, hc.ProjectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("project %q no longer exists", hc.ProjectID)
		}
		return nil, fmt.Errorf("failed to resolve project %q: %w", hc.ProjectID, err)
	}
	if project.IsTemplate() {
		return nil, errors.New("cannot create agents in a template project")
	}

	ownerID, ancestry, err := s.resolveHeadlessOwnerAndAncestry(ctx, hc)
	if err != nil {
		return nil, err
	}
	preCapRole, roleExplicit, parentMessageMode, err := s.resolveHeadlessPreCapRole(ctx, project, hc.Authority, hc.AgentRole)
	if err != nil {
		return nil, fmt.Errorf("scheduled dispatch of agent %q: %w", slug, err)
	}
	childRole, cause, fits := childRoleWithinCeiling(
		uatHopEffectCeiling(edgeProvenance.SourcePrincipalKind, edgeProvenance.SourcePrincipalID, edgeProvenance, edgeCeiling),
		preCapRole,
		roleExplicit,
	)
	if !fits {
		return nil, fmt.Errorf("scheduled dispatch of agent %q: %w: %s", slug, errScheduledAuthorityDenied, cause)
	}
	effectiveNoAuth := hc.NoAuth || childRole == AgentRoleNone

	creatorIdentity := hc.Identity
	runtimeBrokerID, err := s.resolveScheduledRuntimeBroker(
		contextWithIdentity(ctx, creatorIdentity), project)
	if err != nil {
		return nil, fmt.Errorf("scheduled dispatch of agent %q: %w", slug, err)
	}

	var scheduledBroker *store.RuntimeBroker
	if runtimeBrokerID != "" {
		if b, err := s.store.GetRuntimeBroker(ctx, runtimeBrokerID); err == nil {
			scheduledBroker = b
		} else if !errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("failed to resolve runtime broker %q: %w", runtimeBrokerID, err)
		}
	}
	placement, err := s.flatCreatePlacement(ctx, scheduledBroker, "", "")
	if err != nil {
		return nil, err
	}

	fireID := hc.Authority.Recorded.FireID
	existingAgent, err := s.store.GetAgentBySlug(ctx, hc.ProjectID, slug)
	if err == nil && existingAgent != nil {
		slog.Warn("Scheduler: agent already exists, skipping dispatch_agent",
			"eventID", fireID,
			"agentName", slug,
			"projectID", hc.ProjectID,
			"existingPhase", existingAgent.Phase)
		if existingAgent.Phase == string(state.PhaseError) {
			placementRefused := s.isScheduleBlockedPlacementRefusal(ctx, existingAgent)
			if hc.NotifyBlocked != nil {
				hc.NotifyBlocked(ctx, existingAgent, placementRefused)
			}
			return nil, scheduleBlockedError(existingAgent, placementRefused)
		}
		return nil, fmt.Errorf("agent %q already exists in project", slug)
	}

	var clonedLabels map[string]string
	if len(hc.Labels) > 0 {
		clonedLabels = maps.Clone(hc.Labels)
	}
	agent := &store.Agent{
		ID:              api.NewUUID(),
		Slug:            slug,
		Name:            slug,
		Template:        templateName,
		ProjectID:       hc.ProjectID,
		RuntimeBrokerID: runtimeBrokerID,
		Phase:           string(state.PhaseCreated),
		Detached:        hc.Detached,
		CreatedBy:       hc.Authority.PrincipalID,
		OwnerID:         ownerID,
		Ancestry:        ancestry,
		Labels:          clonedLabels,
	}
	applyPinnedPlacement(agent, placement)
	if agent.IsPinned() {
		if p := projectSettingsFromAnnotations(project).ActiveProfile; p != nil && *p != "" {
			slog.Warn("Scheduler: default Runtime Broker Profile not applied to a flat Runtime Broker agent",
				"eventID", fireID, "agent_id", agent.ID, "profile", *p, "runtime_broker_id", agent.PinnedRuntimeBrokerID)
		}
	}

	creatorName := hc.CreatorName
	if creatorName == "" {
		creatorName = fireCreatorName(hc.Authority)
	}
	agent.AppliedConfig = s.buildAppliedConfig(CreateAgentRequest{
		Task:   hc.Task,
		Branch: hc.Branch,
		NoAuth: effectiveNoAuth,
	}, creatorName, childRole)

	templateFromProjectDefault := false
	if templateName == "" && project != nil && project.Annotations != nil {
		if dt := project.Annotations[projectSettingDefaultTemplate]; dt != "" {
			templateName = dt
			agent.Template = dt
			templateFromProjectDefault = true
		}
	}

	templateFromHubDefault := false
	if templateName == "" {
		if d := s.hubAgentDefaults(); d.DefaultTemplate != "" {
			templateName = d.DefaultTemplate
			agent.Template = d.DefaultTemplate
			templateFromHubDefault = true
		}
	}

	templateFromImplicitDefault := false
	if templateName == "" {
		templateName = "default"
		agent.Template = "default"
		templateFromImplicitDefault = true
	}

	if !templateRefAllowed(templateName) {
		if templateFromProjectDefault {
			return nil, fmt.Errorf("dispatch_agent: %s", projectDefaultTemplateRefusal)
		}
		if !templateFromHubDefault {
			return nil, fmt.Errorf("dispatch_agent payload: %s", templateRefRefusal)
		}
		s.warnHubDefaultTemplateUnusable(ctx, templateName, hc.ProjectID, "not a template name")
		templateName = ""
		agent.Template = ""
	}

	var tmpl *store.Template
	if templateName != "" {
		var tmplErr error
		tmpl, tmplErr = s.resolveTemplate(ctx, templateName, hc.ProjectID)
		// SECURITY-GATE (ptone/scion#1916): same gate as the agent-create
		// HTTP path in handlers_agents_core.go — a resolved candidate is
		// not known to be one the revision principal may read.
		if tmplErr == nil && tmpl != nil && !s.authorizeResolvedTemplate(ctx, creatorIdentity, tmpl) {
			tmpl = nil
			tmplErr = store.ErrNotFound
		}
		if tmpl == nil && (tmplErr == nil || errors.Is(tmplErr, store.ErrNotFound)) {
			brokerHasLocal := false
			if runtimeBrokerID != "" {
				provider, provErr := s.store.GetProjectProvider(ctx, hc.ProjectID, runtimeBrokerID)
				if provErr == nil && provider.LocalPath != "" {
					brokerHasLocal = true
				}
			}
			switch {
			case brokerHasLocal:
				// Template will be resolved locally by the Runtime Broker.
			case templateFromHubDefault:
				s.warnHubDefaultTemplateUnusable(ctx, templateName, hc.ProjectID, "not found")
				agent.Template = ""
			case templateFromImplicitDefault:
				agent.Template = ""
			}
		}
		if tmplErr == nil && tmpl != nil {
			if tmpl.Slug != "" {
				agent.Template = tmpl.Slug
			}
		}
	}

	var templateMessageMode string
	if jc := s.templateJobConfig(ctx, tmpl); jc != nil {
		templateMessageMode = jc.MessageMode
	}
	if templateMessageMode != "" {
		if !store.IsValidMessageMode(templateMessageMode) {
			return nil, fmt.Errorf("invalid template message mode: %s", templateMessageMode)
		}
		agent.MessageMode = templateMessageMode
	} else if parentMessageMode != "" {
		agent.MessageMode = parentMessageMode
	} else {
		agent.MessageMode = store.MessageModeProject
	}
	if agent.MessageMode == store.MessageModeHub {
		if decision := s.AuthorizeMessageModeGrant(ctx, creatorIdentity, hc.ProjectID, agent.MessageMode); !decision.Allowed {
			return nil, fmt.Errorf("cannot grant hub message mode: %s", decision.Reason)
		}
	}

	persistence, persistenceSource, refusal := resolveAgentPersistence(persistenceInput{
		BrokerName:     persistenceBrokerName(scheduledBroker),
		BrokerSupports: brokerSupportsEphemeral(scheduledBroker),
		GateOn:         s.experimentEnabled(experiments.CloudRunEphemeralAgents),
	})
	if refusal != nil {
		return nil, errors.New(refusal.Message)
	}
	stampAgentPersistence(agent, persistence, persistenceSource)

	saOrigin, err := s.applyScheduledProjectDefaultGCPIdentity(
		contextWithIdentity(ctx, creatorIdentity), agent, project)
	if err != nil {
		return nil, fmt.Errorf("scheduled dispatch of agent %q: %w", slug, err)
	}

	if err := s.deriveAgentConfig(ctx, agent, project, tmpl); err != nil {
		return nil, fmt.Errorf("scheduled dispatch of agent %q: %w", slug, err)
	}

	// Inject allow-listed event data environment variables after deriveAgentConfig
	// so neither template nor project defaults overwrite hub-assigned trigger IDs.
	if len(hc.EventEnv) > 0 {
		if agent.AppliedConfig.Env == nil {
			agent.AppliedConfig.Env = make(map[string]string, len(hc.EventEnv))
		}
		for k, v := range hc.EventEnv {
			agent.AppliedConfig.Env[k] = v
		}
	}

	agent.LaunchAsyncOptIn = true

	scheduledDispatchAudit := &store.MutationAuditRecord{
		MutationType:       mutationTypeAgentDelegation,
		ActorPrincipalKind: creatorIdentity.Type(),
		ActorPrincipalID:   creatorIdentity.ID(),
		CanDelegateResult:  "allow",
	}
	initiator := s.scheduledInitiator(hc.Authority.Recorded.Attribution)
	if initiatorMatchesExecutor(initiator, creatorIdentity) {
		if hubKind := hubCredentialKindForInitiator(initiator.CredentialKind); hubKind != "" {
			scheduledDispatchAudit.ActorCredentialType = hubKind
			scheduledDispatchAudit.ActorCredentialID = initiator.CredentialID
		}
	}

	if err := s.commitAgentCreate(ctx, agentCreateWrite{
		Ceiling:    edgeCeiling,
		Provenance: edgeProvenance,
		Agent:      agent,
		Slug:       slug,
		Edge: &store.DelegationEdge{
			DelegatorType: hc.Authority.PrincipalKind,
			DelegatorID:   hc.Authority.PrincipalID,
			DelegateType:  store.DelegationPrincipalAgent,
			ScopeType:     store.RoleScopeProject,
			ScopeID:       hc.ProjectID,
			Role:          agent.AppliedConfig.AgentRole,
			Active:        true,
		},
		Audit:    scheduledDispatchAudit,
		SAOrigin: saOrigin,
	}); err != nil {
		return nil, fmt.Errorf("failed to create agent %q: %w", slug, err)
	}

	rollback := func(rb createRollback) error {
		rb.Agent = agent
		rb.RuntimeBrokerID = runtimeBrokerID
		rb.CreateAuditID = scheduledDispatchAudit.ID
		deleteWon := false
		rb.DeleteWon = &deleteWon
		corrID := s.cleanupFailedCreate(ctx, rb)
		if deleteWon && !errors.Is(rb.Cause, store.ErrDeleteInProgress) {
			return fmt.Errorf("scheduled dispatch of agent %q: %w", slug, errScheduledChildDeletedDuringCreate)
		}
		if corrID != "" {
			return fmt.Errorf("failed to dispatch agent %q: %w (rollback incomplete, correlation ID %s)", slug, rb.Cause, corrID)
		}
		return fmt.Errorf("failed to dispatch agent %q: %w", slug, rb.Cause)
	}

	dispatchExecutor, _ := ExecutorContextFromContext(ctx)
	dispatcher := s.GetDispatcher()
	if dispatcher == nil {
		slog.Warn("Scheduler: no dispatcher available, agent created but not started",
			"eventID", fireID,
			"agent_id", agent.ID,
			"agentName", agent.Name,
			"executor_kind", dispatchExecutor.Kind,
			"executor_id", dispatchExecutor.ID)
		return agent, nil
	}

	created, err := s.createUnderClaim(ctx, agent, func(ctx context.Context) (*CreateDispatchResult, error) {
		return dispatcher.DispatchAgentCreateWithGather(ctx, agent)
	})
	var held *store.ClaimHeldError
	if errors.As(err, &held) {
		slog.Info("Scheduler: agent already starting; event skipped",
			"eventID", fireID, "agent_id", agent.ID, "holder", string(held.Kind))
		return agent, nil
	}
	if errors.Is(err, ErrLaunchInvalidPhase) {
		return nil, fmt.Errorf("failed to dispatch agent %q: %w", slug, err)
	}
	if errors.Is(err, errStartClaimWrite) {
		return nil, rollback(createRollback{Stage: createStageRunIntent, Cause: err})
	}
	if err != nil && !errors.Is(err, store.ErrDeleteInProgress) &&
		(errors.Is(err, errStartingWrite) || errors.Is(err, store.ErrClaimPredicate) || errors.Is(err, errStartClaimLost)) {
		return nil, fmt.Errorf("failed to dispatch agent %q: %w", slug, err)
	}
	if err != nil {
		slog.Error("Scheduler: failed to dispatch agent creation",
			"eventID", fireID,
			"agent_id", agent.ID,
			"agentName", agent.Name,
			"error", err,
			"executor_kind", dispatchExecutor.Kind,
			"executor_id", dispatchExecutor.ID)
		return nil, rollback(createRollback{
			Stage:         createStageDispatch,
			Cause:         err,
			DeleteRuntime: dispatchDeleteFailedCreate(s.store, dispatcher, agent),
		})
	}

	envReqs := created.EnvRequirements()
	if envReqs != nil && len(envReqs.Needs) > 0 {
		sortedNeeds := slices.Clone(envReqs.Needs)
		slices.Sort(sortedNeeds)
		slog.Warn("Scheduler: headless agent create failed: missing required environment variables",
			"eventID", fireID,
			"definitionKind", hc.Authority.Recorded.DefinitionKind,
			"definitionID", hc.Authority.Recorded.DefinitionID,
			"agent_id", agent.ID,
			"agentName", agent.Name,
			"project_id", hc.ProjectID,
			"count", len(sortedNeeds),
			"missing_keys", sortedNeeds,
			"executor_kind", dispatchExecutor.Kind,
			"executor_id", dispatchExecutor.ID)
		deleteWon := false
		auditCause := fmt.Errorf("missing required environment variables: %s", strings.Join(sortedNeeds, ", "))
		corrID := s.cleanupFailedCreate(ctx, createRollback{
			Agent:             agent,
			RuntimeBrokerID:   runtimeBrokerID,
			CreateAuditID:     scheduledDispatchAudit.ID,
			Stage:             createStageMissingEnv,
			Cause:             auditCause,
			RevokeCredentials: true,
			DeleteRuntime:     dispatchDeleteFailedCreate(s.store, dispatcher, agent),
			DeleteWon:         &deleteWon,
		})
		if deleteWon {
			return nil, fmt.Errorf("scheduled dispatch of agent %q: %w", slug, errScheduledChildDeletedDuringCreate)
		}
		if corrID != "" {
			slog.Error("Scheduler: headless agent create missing-env rollback incomplete",
				"eventID", fireID,
				"agent_id", agent.ID,
				"correlation_id", corrID)
		}
		return nil, &HeadlessMissingEnvError{Count: len(sortedNeeds)}
	}

	if created.AcceptedLaunch() != nil {
		if _, err := s.persistAcceptedLaunch(ctx, agent); err != nil {
			slog.Warn("Scheduler: failed to persist agent after accepted launch",
				"eventID", fireID, "agent_id", agent.ID, "error", err)
		}
	} else if !s.scheduledChildLive(ctx, agent) {
		return nil, fmt.Errorf("scheduled dispatch of agent %q: %w", slug, errScheduledChildDeletedDuringCreate)
	}

	slog.Info("Scheduler: agent dispatched successfully",
		"eventID", fireID, "agent_id", agent.ID, "agentName", agent.Name,
		"project_id", hc.ProjectID,
		"executor_kind", dispatchExecutor.Kind,
		"executor_id", dispatchExecutor.ID)
	return agent, nil
}
