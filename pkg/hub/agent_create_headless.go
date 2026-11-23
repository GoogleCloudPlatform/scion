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
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/labels"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Fixed allow-list of event data environment variables accepted by
// HeadlessCreate.EventEnv (Stage 1 design §5.7 constraint 3 & 4; Stage 2 §5.4).
const (
	EventEnvKeyTriggerEventID   = "SCION_TRIGGER_EVENT_ID"
	EventEnvKeyTriggerID        = "SCION_TRIGGER_ID"
	EventEnvKeyTriggerEventFile = api.EnvTriggerEventFile

	// HeadlessEventFilePath is the relative home-directory path where
	// HeadlessCreate.EventFile is written via WithCreateHomeFiles (Stage 1
	// design §5.7, Stage 2 §5.4).
	HeadlessEventFilePath = api.TriggerEventHomeFilePath
	// HeadlessEventFileContainerPath is the default absolute path inside the
	// agent container advertised via SCION_TRIGGER_EVENT_FILE (Stage 2 §5.4).
	// The Runtime Broker overrides this with the container user's resolved
	// home directory (e.g. /root or /home/<user>) at start time.
	HeadlessEventFileContainerPath = "/home/scion/" + HeadlessEventFilePath
	headlessEventFileMode          = 0o600

	// maxHeadlessEventFileBytes bounds HeadlessCreate.EventFile to the 64 KiB
	// HomeFiles channel cap (Stage 2 §5.4; external_payload inside the file is
	// separately bounded to 16 KiB by messaging.NewExternalPayload).
	maxHeadlessEventFileBytes = api.MaxHomeFilesTotalBytes
)

var (
	errDisallowedHeadlessEventEnvKey       = errors.New("disallowed headless event env key")
	errInvalidHeadlessEventEnvValue        = errors.New("headless event env value contains NUL byte")
	errHeadlessEventFileTooLarge           = errors.New("headless create event file exceeds limit")
	errHeadlessGitHubTokenUnsupported      = errors.New("headless create githubToken is not supported before Stage 2 P1c")
	errHeadlessProvenancePrincipalMismatch = errors.New("headless create: edge provenance principal does not match authority principal")
	errHeadlessInvalidEdgeProvenance       = errors.New("headless create: invalid scheduler edge provenance")
	errHeadlessNoUsableRole                = errors.New("no usable role for the recorded authority; set noAuth: true or re-authorize the schedule")
	errHeadlessMissingResolvedRole         = errors.New("headless create: missing pre-authorized resolved role")
	errHeadlessNoDispatcherForHomeFiles    = errors.New("no dispatcher available for homeFiles create")
	errHeadlessAgentExists                 = errors.New("agent already exists in project")
)

type headlessAgentExistsError struct {
	slug string
}

func (e *headlessAgentExistsError) Error() string {
	return fmt.Sprintf("agent %q already exists in project", e.slug)
}

func (e *headlessAgentExistsError) Is(target error) bool {
	return target == errHeadlessAgentExists
}

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
// dispatch_agent in Stage 2/3). Ownership and ancestry are derived strictly
// from Authority (never overridden by callers).
type HeadlessCreate struct {
	ProjectID      string
	Authority      FireAuthority
	Identity       Identity
	CreatorName    string
	EdgeCeiling    store.EffectCeiling
	EdgeProvenance store.AuthorityProvenance

	// Credential and lineage posture (Stage 1 design rev 8.27 & 8.30,
	// headless-identity-note.md §2 & §2.3). When NoAuth is true, the create
	// explicitly requests AgentRoleNone (roleExplicit=true) and launches without
	// credentials under any frozen ceiling. Otherwise, when AgentRole is empty,
	// createAgentHeadless resolves the role through the project/hub default
	// ladder capped by projectSettingMaxAgentRole, the edge ceiling, and (for
	// agent principals) the parent agent's role; if no role fits the ceiling,
	// the create fails with errHeadlessNoUsableRole.
	AgentRole AgentRole
	NoAuth    bool

	// resolvedRole, when populated by dispatchAgentAction.Authorize, supplies
	// the exact pre-cap role tuple approved by CanDelegate so Fire never
	// re-resolves project or parent role defaults.
	resolvedRole *headlessResolvedRole

	// Trusted authoring-time values (Stage 1 design §5.8).
	Name     string
	Template string
	Task     string
	Branch   string
	Labels   map[string]string // validated by pkg/labels (<=16, <=63 chars)

	// GitHubToken reserves the Stage 2 P1c dispatch token precedence preference
	// ("" | "installation" | "user_preferred"; "" keeps today's dispatch
	// precedence; non-empty values are refused until Stage 2 P1c lands).
	GitHubToken string

	// OnCommitTx, when non-nil, runs inside commitAgentCreate's transaction
	// with the newly created agent's ID before commit (e.g. binding a trigger
	// subject claim atomically with the agent create audit, Stage 2 §5.3).
	OnCommitTx func(ctx context.Context, tx store.Store, agentID string) error

	// OnCommitted, when non-nil, runs immediately after commitAgentCreate's
	// transaction commits successfully (and never on rollback).
	OnCommitted func(agentID string)

	// Data-only channels (never material, never participating in secret/env
	// resolution).
	EventEnv  map[string]string // allow-listed keys only: SCION_TRIGGER_EVENT_ID, SCION_TRIGGER_ID
	EventFile []byte            // ~/.scion/trigger-event.json, <= 16 KiB; nil in Stage 1

	// Secrets supplies optional create-time agent-scoped secrets (behind
	// hub.agent_secrets), stored after commit and before dispatch.
	// Future producers must not persist secret values in schedule or trigger payloads.
	Secrets []AgentSecretInput

	Detached      bool
	NotifyBlocked func(ctx context.Context, existing *store.Agent, placementRefused bool)
}

// headlessResolvedRole carries the pre-cap role tuple resolved during
// dispatchAgentAction.Authorize so createAgentHeadless writes the exact role
// approved by CanDelegate without a second store lookup of role defaults.
type headlessResolvedRole struct {
	preCapRole        AgentRole
	roleExplicit      bool
	parentMessageMode string
}

func validateHeadlessEventEnv(env map[string]string) error {
	for k, v := range env {
		switch k {
		case EventEnvKeyTriggerEventID, EventEnvKeyTriggerID, EventEnvKeyTriggerEventFile:
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

// fireParentAgentRoleCeiling resolves the parent agent's role ceiling and
// message mode for an agent-principal fire, reusing auth.agent when already
// loaded and falling back to callerAgentRoleCeiling (baseline on unreadable or
// invalid stored role) so authorizeDispatchAgentFire and resolveHeadlessPreCapRole
// evaluate the identical parent role.
func (s *Server) fireParentAgentRoleCeiling(ctx context.Context, auth FireAuthority) (AgentRole, string) {
	if auth.agent == nil {
		return s.callerAgentRoleCeiling(ctx, auth.PrincipalID)
	}
	role, _ := agentRoleAndScopes(auth.agent)
	if !ValidAgentRole(role) {
		slog.Warn("Parent agent has invalid stored role, defaulting to baseline",
			"parent_agent_id", auth.PrincipalID, "stored_role", role)
		role = AgentRoleBaseline
	}
	return role, auth.agent.MessageMode
}

func (s *Server) resolveHeadlessPreCapRole(
	ctx context.Context,
	project *store.Project,
	auth FireAuthority,
	requestedRole AgentRole,
	noAuth bool,
) (preCapRole AgentRole, roleExplicit bool, parentMessageMode string, err error) {
	projectMax := projectMaxAgentRole(project)
	defaultAgentRole := s.projectDefaultAgentRole(project)
	if requestedRole != "" {
		if !ValidAgentRole(requestedRole) {
			return "", false, "", fmt.Errorf("invalid agent role %q", requestedRole)
		}
		roleExplicit = true
	}
	if noAuth {
		if requestedRole != "" && requestedRole != AgentRoleNone {
			return "", false, "", fmt.Errorf("cannot request agent role %q together with noAuth", requestedRole)
		}
		requestedRole = AgentRoleNone
		roleExplicit = true
	}

	if auth.PrincipalKind == store.DelegationPrincipalAgent {
		parentRole, msgMode := s.fireParentAgentRoleCeiling(ctx, auth)
		parentMessageMode = msgMode
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
	if hc.resolvedRole == nil {
		return nil, errHeadlessMissingResolvedRole
	}
	validatedSecrets, secErr := s.validateCreateAgentSecrets(hc.Secrets)
	if secErr != nil {
		return nil, secErr
	}
	if hc.Name == "" {
		return nil, fmt.Errorf("dispatch_agent payload: agentName is required")
	}
	if hc.GitHubToken != "" {
		return nil, errHeadlessGitHubTokenUnsupported
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
	if edgeProvenance.SourcePrincipalKind != hc.Authority.PrincipalKind ||
		edgeProvenance.SourcePrincipalID == "" ||
		edgeProvenance.SourcePrincipalID != hc.Authority.PrincipalID {
		return nil, fmt.Errorf("%w: provenance (%s:%s) != authority (%s:%s)",
			errHeadlessProvenancePrincipalMismatch,
			edgeProvenance.SourcePrincipalKind, edgeProvenance.SourcePrincipalID,
			hc.Authority.PrincipalKind, hc.Authority.PrincipalID)
	}
	if edgeProvenance.SourceCredentialKind != store.SourceCredentialScheduler ||
		edgeProvenance.SourceEventID == "" ||
		edgeProvenance.SourceAuthorizationRevision < 1 ||
		(hc.Authority.Recorded.DefinitionKind == hubfire.DefinitionKindSchedule && edgeProvenance.SourceScheduleID == "") {
		return nil, fmt.Errorf("%w: credential=%q event=%q schedule=%q revision=%d",
			errHeadlessInvalidEdgeProvenance,
			edgeProvenance.SourceCredentialKind,
			edgeProvenance.SourceEventID,
			edgeProvenance.SourceScheduleID,
			edgeProvenance.SourceAuthorizationRevision)
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
	preCapRole := hc.resolvedRole.preCapRole
	roleExplicit := hc.resolvedRole.roleExplicit
	parentMessageMode := hc.resolvedRole.parentMessageMode
	fireID := hc.Authority.Recorded.FireID
	childRole, cause, fits := childRoleWithinCeiling(
		uatHopEffectCeiling(edgeProvenance.SourcePrincipalKind, edgeProvenance.SourcePrincipalID, edgeProvenance, edgeCeiling),
		preCapRole,
		roleExplicit,
	)
	if !fits {
		slog.Warn("Scheduler: headless agent create denied by recorded authority ceiling",
			"eventID", fireID,
			"agentName", slug,
			"projectID", hc.ProjectID,
			"requestedRole", preCapRole,
			"roleExplicit", roleExplicit,
			"cause", cause)
		if !roleExplicit {
			return nil, errHeadlessNoUsableRole
		}
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
	if len(hc.EventFile) > 0 {
		if err := checkBrokerHomeFilesCapability(scheduledBroker); err != nil {
			return nil, fmt.Errorf("scheduled dispatch of agent %q: %w", slug, err)
		}
	}
	placement, err := s.flatCreatePlacement(ctx, scheduledBroker, "", "")
	if err != nil {
		return nil, err
	}

	existingAgent, err := s.store.GetAgentBySlug(ctx, hc.ProjectID, slug)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("failed to check existing agent %q in project %q: %w", slug, hc.ProjectID, err)
	}
	if err == nil && existingAgent != nil {
		if hc.Authority.Recorded.DefinitionKind != hubfire.DefinitionKindTrigger {
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
		}
		return nil, &headlessAgentExistsError{slug: slug}
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
	pinResolvedCreateProfile(agent, project, scheduledBroker, false)

	templateExplicit := templateName != ""

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
		agent.MessageMode = s.defaultAgentMessageMode()
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

	templateSuppliedShellHC := templateSuppliesShellHarnessConfig(tmpl, agent, project)

	if err := s.deriveAgentConfig(ctx, agent, project, tmpl); err != nil {
		return nil, fmt.Errorf("scheduled dispatch of agent %q: %w", slug, err)
	}

	// Shell execution container admission (ptone/scion#8812 §3.2, §3.4 S6, §7):
	// enforce human-only creation (user/dev allowlist + user principal kind),
	// hub.exec_containers flag, no initial task, and no templates on the
	// headless dispatch_agent path as well as HTTP create.
	if denial := s.checkShellAgentCreateAdmission(agent, shellAgentCreateOpts{
		IsHumanPrincipal:        isHumanShellCreatePrincipal(hc.Identity, hc.Authority.PrincipalKind),
		TemplateExplicit:        templateExplicit,
		TemplateSuppliedShellHC: templateSuppliedShellHC,
	}); denial != nil {
		return nil, fmt.Errorf("scheduled dispatch of agent %q: %w", slug, denial)
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

	// Mark create-time HomeFiles as pending before committing the agent row so
	// no concurrent manual start can launch the agent without its event file
	// before our create dispatch delivers it (P1c-1 O-C / sec L2).
	if len(hc.EventFile) > 0 {
		if agent.AppliedConfig.CreateInputs == nil {
			agent.AppliedConfig.CreateInputs = &store.AgentCreateInputs{}
		}
		agent.AppliedConfig.CreateInputs.HomeFilesPending = true
	}

	agent.LaunchAsyncOptIn = true

	scheduledDispatchAudit := &store.MutationAuditRecord{
		MutationType:       mutationTypeAgentDelegation,
		ActorPrincipalKind: creatorIdentity.Type(),
		ActorPrincipalID:   creatorIdentity.ID(),
		CanDelegateResult:  "allow",
		AfterSummary:       createAuditAfterSummaryWithSecrets(validatedSecrets),
	}
	initiator := s.scheduledInitiator(hc.Authority.Recorded.Attribution)
	if initiatorMatchesExecutor(initiator, creatorIdentity) {
		if hubKind := hubCredentialKindForInitiator(initiator.CredentialKind); hubKind != "" {
			scheduledDispatchAudit.ActorCredentialType = hubKind
			scheduledDispatchAudit.ActorCredentialID = initiator.CredentialID
		}
	}

	var onCommitTx func(ctx context.Context, tx store.Store) error
	if hc.OnCommitTx != nil {
		onCommitTx = func(ctx context.Context, tx store.Store) error {
			return hc.OnCommitTx(ctx, tx, agent.ID)
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
		Audit:      scheduledDispatchAudit,
		SAOrigin:   saOrigin,
		OnCommitTx: onCommitTx,
	}); err != nil {
		return nil, fmt.Errorf("failed to create agent %q: %w", slug, err)
	}
	if hc.OnCommitted != nil {
		hc.OnCommitted(agent.ID)
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

	if len(validatedSecrets) > 0 {
		if err := s.storeCreateAgentSecrets(ctx, agent, validatedSecrets, headlessSecretCreatorAttribution(hc.Authority), true); err != nil {
			if errors.Is(err, errAgentDeletedDuringSecretWrite) {
				return nil, fmt.Errorf("scheduled dispatch of agent %q: %w", slug, errScheduledChildDeletedDuringCreate)
			}
			slog.ErrorContext(ctx, "headless agent create: failed to store create-time agent secrets; rolling back agent",
				"agent_id", agent.ID, "error", err)
			if rbErr := rollback(createRollback{Stage: createStageSecrets, Cause: err}); rbErr != nil {
				return nil, fmt.Errorf("%s: %w", createAgentSecretsStoreFailedMessage, rbErr)
			}
			return nil, fmt.Errorf("%s: %w", createAgentSecretsStoreFailedMessage, err)
		}
	}

	dispatchExecutor, _ := ExecutorContextFromContext(ctx)
	dispatcher := s.GetDispatcher()
	if dispatcher == nil {
		if len(hc.EventFile) > 0 {
			slog.Error("Scheduler: no dispatcher available for homeFiles create; rolling back",
				"eventID", fireID,
				"agent_id", agent.ID,
				"agentName", agent.Name,
				"executor_kind", dispatchExecutor.Kind,
				"executor_id", dispatchExecutor.ID)
			return nil, rollback(createRollback{
				Stage: createStageDispatch,
				Cause: errHeadlessNoDispatcherForHomeFiles,
			})
		}
		slog.Warn("Scheduler: no dispatcher available, agent created but not started",
			"eventID", fireID,
			"agent_id", agent.ID,
			"agentName", agent.Name,
			"executor_kind", dispatchExecutor.Kind,
			"executor_id", dispatchExecutor.ID)
		return agent, nil
	}

	created, err := s.createUnderClaim(ctx, agent, func(ctx context.Context) (*CreateDispatchResult, error) {
		if len(hc.EventFile) > 0 {
			ctx = WithCreateHomeFiles(ctx, []api.HomeFile{{
				Path:    HeadlessEventFilePath,
				Mode:    headlessEventFileMode,
				Content: hc.EventFile,
			}})
		}
		res, derr := dispatcher.DispatchAgentCreateWithGather(ctx, agent)
		if derr == nil && len(hc.EventFile) > 0 && res.AcceptedLaunch() == nil &&
			(res == nil || res.EnvRequirements() == nil || len(res.EnvRequirements().Needs) == 0) {
			s.clearAgentHomeFilesPending(ctx, agent)
		}
		return res, derr
	})
	var missingNeeds []string
	var homeFilesMissing *homeFilesEnvGatherDeferredError
	if errors.As(err, &homeFilesMissing) && len(homeFilesMissing.needs) > 0 {
		missingNeeds = homeFilesMissing.needs
		err = nil
	}
	var held *store.ClaimHeldError
	if errors.As(err, &held) {
		if len(hc.EventFile) > 0 {
			slog.Warn("Scheduler: start claim held during homeFiles create; rolling back pending agent",
				"eventID", fireID, "agent_id", agent.ID, "holder", string(held.Kind))
			return nil, rollback(createRollback{Stage: createStageRunIntent, Cause: err})
		}
		slog.Info("Scheduler: agent already starting; event skipped",
			"eventID", fireID, "agent_id", agent.ID, "holder", string(held.Kind))
		return agent, nil
	}
	if errors.Is(err, ErrLaunchInvalidPhase) {
		if len(hc.EventFile) > 0 {
			return nil, rollback(createRollback{Stage: createStageDispatch, Cause: err})
		}
		return nil, fmt.Errorf("failed to dispatch agent %q: %w", slug, err)
	}
	if errors.Is(err, errStartClaimWrite) {
		return nil, rollback(createRollback{Stage: createStageRunIntent, Cause: err})
	}
	if err != nil && !errors.Is(err, store.ErrDeleteInProgress) &&
		(errors.Is(err, errStartingWrite) || errors.Is(err, store.ErrClaimPredicate) || errors.Is(err, errStartClaimLost)) {
		if len(hc.EventFile) > 0 {
			return nil, rollback(createRollback{Stage: createStageRunIntent, Cause: err})
		}
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

	if len(missingNeeds) == 0 && created != nil {
		if envReqs := created.EnvRequirements(); envReqs != nil {
			missingNeeds = envReqs.Needs
		}
	}
	if len(missingNeeds) > 0 {
		sortedNeeds := slices.Clone(missingNeeds)
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
	} else if !s.preserveTerminalPhase(ctx, agent) {
		// Persist the broker's synchronous create echo (including the
		// resolved Profile recorded by store.ApplyBrokerAgentConfig) so
		// subsequent starts and reincarnations read it from the store
		// (ptone/scion#5149).
		if err := s.updateAgentAfterDispatch(ctx, agent); err != nil {
			slog.Warn("Scheduler: failed to persist agent after dispatch",
				"eventID", fireID, "agent_id", agent.ID, "error", err)
		}
	}

	slog.Info("Scheduler: agent dispatched successfully",
		"eventID", fireID, "agent_id", agent.ID, "agentName", agent.Name,
		"project_id", hc.ProjectID,
		"executor_kind", dispatchExecutor.Kind,
		"executor_id", dispatchExecutor.ID)
	return agent, nil
}
