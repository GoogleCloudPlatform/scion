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

// Package hub provides the Scion Hub API server.
package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/observability/dispatchmetrics"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"golang.org/x/sync/errgroup"
)

// HTTPRuntimeBrokerClient is an HTTP-based implementation of RuntimeBrokerClient.
// It communicates with remote runtime brokers via their REST API.
type HTTPRuntimeBrokerClient struct {
	transport *brokerHTTPTransport
}

// NewHTTPRuntimeBrokerClient creates a new HTTP runtime broker client.
func NewHTTPRuntimeBrokerClient() *HTTPRuntimeBrokerClient {
	return &HTTPRuntimeBrokerClient{transport: newBrokerHTTPTransport(false, nil)}
}

// NewHTTPRuntimeBrokerClientWithDebug creates a new HTTP runtime broker client with debug logging.
func NewHTTPRuntimeBrokerClientWithDebug(debug bool) *HTTPRuntimeBrokerClient {
	return &HTTPRuntimeBrokerClient{transport: newBrokerHTTPTransport(debug, nil)}
}

func (c *HTTPRuntimeBrokerClient) CreateAgent(ctx context.Context, brokerID, brokerEndpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, error) {
	return c.transport.CreateAgent(ctx, brokerID, brokerEndpoint, req)
}

func (c *HTTPRuntimeBrokerClient) StartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID, task, projectPath, projectSlug, harnessConfig string, resolvedEnv map[string]string, resolvedSecrets []ResolvedSecret, inlineConfig *api.ScionConfig, sharedDirs []api.SharedDir, sharedWorkspace, resume bool) (*RemoteAgentResponse, error) {
	return c.transport.StartAgent(ctx, brokerID, brokerEndpoint, agentID, projectID, task, projectPath, projectSlug, harnessConfig, resolvedEnv, resolvedSecrets, inlineConfig, sharedDirs, sharedWorkspace, resume)
}

func (c *HTTPRuntimeBrokerClient) StopAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string) error {
	return c.transport.StopAgent(ctx, brokerID, brokerEndpoint, agentID, projectID)
}

func (c *HTTPRuntimeBrokerClient) RestartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string, resolvedEnv map[string]string) error {
	return c.transport.RestartAgent(ctx, brokerID, brokerEndpoint, agentID, projectID, resolvedEnv)
}

func (c *HTTPRuntimeBrokerClient) ResetAuthAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID, token string) error {
	return c.transport.ResetAuthAgent(ctx, brokerID, brokerEndpoint, agentID, projectID, token)
}

func (c *HTTPRuntimeBrokerClient) DeleteAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string, deleteFiles, removeBranch, softDelete bool, deletedAt time.Time) error {
	return c.transport.DeleteAgent(ctx, brokerID, brokerEndpoint, agentID, projectID, deleteFiles, removeBranch, softDelete, deletedAt)
}

func (c *HTTPRuntimeBrokerClient) MessageAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID, message string, interrupt bool, structuredMsg *messages.StructuredMessage) error {
	return c.transport.MessageAgent(ctx, brokerID, brokerEndpoint, agentID, projectID, message, interrupt, structuredMsg)
}

// HasPromptResponse is the response from the has-prompt action.
type HasPromptResponse struct {
	HasPrompt bool `json:"hasPrompt"`
}

func (c *HTTPRuntimeBrokerClient) CheckAgentPrompt(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string) (bool, error) {
	return c.transport.CheckAgentPrompt(ctx, brokerID, brokerEndpoint, agentID, projectID)
}

// CreateAgentWithGather creates an agent and handles 202 env-gather responses.
func (c *HTTPRuntimeBrokerClient) CreateAgentWithGather(ctx context.Context, brokerID, brokerEndpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
	return c.transport.CreateAgentWithGather(ctx, brokerID, brokerEndpoint, req)
}

func (c *HTTPRuntimeBrokerClient) GetAgentLogs(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string, tail int) (string, error) {
	return c.transport.GetAgentLogs(ctx, brokerID, brokerEndpoint, agentID, projectID, tail)
}

func (c *HTTPRuntimeBrokerClient) ExecAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string, command []string, timeout int) (string, int, error) {
	return c.transport.ExecAgent(ctx, brokerID, brokerEndpoint, agentID, projectID, command, timeout)
}

func (c *HTTPRuntimeBrokerClient) CleanupProject(ctx context.Context, brokerID, brokerEndpoint, projectSlug, projectID string) error {
	return c.transport.CleanupProject(ctx, brokerID, brokerEndpoint, projectSlug, projectID)
}

// GetClient returns the underlying RuntimeBrokerClient.
func (d *HTTPAgentDispatcher) GetClient() RuntimeBrokerClient {
	return d.client
}

// AgentTokenGenerator generates JWT tokens for agents.
type AgentTokenGenerator interface {
	GenerateAgentToken(agentID, projectID string, ancestry []string, role AgentRole, additionalScopes []AgentTokenScope) (string, error)
}

// GitHubAppTokenMinter mints GitHub App installation tokens for projects.
type GitHubAppTokenMinter interface {
	// MintGitHubAppTokenForProject mints a GitHub App installation token for the given project.
	// Returns the token, expiry (ISO 8601 string), and any error.
	// If the project has no installation or the app is not configured, returns ("", "", nil).
	MintGitHubAppTokenForProject(ctx context.Context, project *store.Project) (token string, expiry string, err error)
}

// HTTPAgentDispatcher dispatches agent operations to remote runtime brokers via HTTP.
// It looks up the runtime broker endpoint from the store and uses HTTPRuntimeBrokerClient
// to make the actual API calls.
type HTTPAgentDispatcher struct {
	store             store.Store
	client            RuntimeBrokerClient
	tokenGenerator    AgentTokenGenerator
	secretBackend     secret.SecretBackend
	authzService      *AuthzService        // Optional authz service for progeny secret verification
	githubAppMinter   GitHubAppTokenMinter // Optional GitHub App token minter
	hubEndpoint       string               // Hub endpoint URL for agents to call back
	hubName           string               // Hub display name for agent log labeling
	hubID             string               // Hub instance ID for hub-scoped queries
	devAuthToken      string               // Dev auth token to inject into agent env (dev-auth mode only)
	transportMinter   TransportTokenMinter // Optional transport token minter for OIDC dispatch
	transportAudience string               // OIDC audience for transport tokens
	transportMode     string               // Transport auth mode (iap, cloudrun_invoker)
	debug             bool
	log               *slog.Logger

	// Cross-node dispatch deps (B4-2). When events + commandBus are non-nil
	// and client.StartAgent/StopAgent/RestartAgent returns ErrLifecycleDeferred,
	// the dispatcher writes durable intent + signals the owning node + waits
	// for the terminal phase transition. Nil = cross-node dispatch disabled
	// (single-node / SQLite mode: all brokers are local).
	events          EventPublisher
	commandBus      CommandBus
	dispatchMetrics dispatchmetrics.Recorder

	// imageRegistry is the configured image registry prefix for rewriting
	// bare image names before dispatching to brokers.
	imageRegistry string

	// Resource hash repair callbacks sync a resource's DB manifest from GCS
	// when a hash mismatch is detected during dispatch. Nil = no repair.
	harnessConfigRepairer func(ctx context.Context, name string) error
	templateRepairer      func(ctx context.Context, ref string) error

	// hubAgentDefaultsProvider returns the hub's operational agent_defaults at
	// dispatch time. A callback rather than a snapshot because the settings
	// propagation goroutine rewrites them while the hub runs; the Server's
	// accessor reads under its lock. Nil = no hub defaults (local dispatcher,
	// tests) and the wire field is omitted.
	hubAgentDefaultsProvider func() opsettings.AgentDefaultsSettings
}

// NewHTTPAgentDispatcher creates a new HTTP-based agent dispatcher.
func NewHTTPAgentDispatcher(s store.Store, debug bool, log *slog.Logger) *HTTPAgentDispatcher {
	return &HTTPAgentDispatcher{
		store:  s,
		client: NewHTTPRuntimeBrokerClientWithDebug(debug),
		debug:  debug,
		log:    log,
	}
}

// NewHTTPAgentDispatcherWithClient creates a new HTTP-based agent dispatcher with a custom client.
func NewHTTPAgentDispatcherWithClient(s store.Store, client RuntimeBrokerClient, debug bool, log *slog.Logger) *HTTPAgentDispatcher {
	return &HTTPAgentDispatcher{
		store:  s,
		client: client,
		debug:  debug,
		log:    log,
	}
}

// SetTokenGenerator sets the token generator for agent authentication.
func (d *HTTPAgentDispatcher) SetTokenGenerator(gen AgentTokenGenerator) {
	d.tokenGenerator = gen
}

// agentRoleAndScopes extracts the effective role and config-based additional
// scopes (e.g. GCP token) from the agent record. Used by all dispatch call
// sites to keep role/scope computation in a single place.
func agentRoleAndScopes(agent *store.Agent) (AgentRole, []AgentTokenScope) {
	if agent == nil {
		return AgentRoleFull, nil
	}
	// Default to full for pre-role agents. This matches the design decision
	// that null/unset agent role resolves to full (see ScopesForRole and
	// roleOrdinal). Baseline would deny create/lifecycle/secret scopes that
	// standing agents (coordinators, leads) need.
	role := AgentRoleFull
	if agent.AppliedConfig != nil && agent.AppliedConfig.AgentRole != "" {
		role = AgentRole(agent.AppliedConfig.AgentRole)
	}

	var additionalScopes []AgentTokenScope
	if agent.AppliedConfig != nil {
		// GCP token scope is config-based, not role-based.
		if gcpID := agent.AppliedConfig.GCPIdentity; gcpID != nil && gcpID.MetadataMode == store.GCPMetadataModeAssign && gcpID.ServiceAccountID != "" {
			additionalScopes = append(additionalScopes, GCPTokenScopeForSA(gcpID.ServiceAccountID))
		}
	}
	return role, additionalScopes
}

// SetHubEndpoint sets the Hub endpoint URL that agents will use to call back.
func (d *HTTPAgentDispatcher) SetHubEndpoint(endpoint string) {
	d.hubEndpoint = endpoint
}

// SetHubName sets the hub display name for agent log labeling.
// When set, agents receive SCION_HUB_NAME so their Cloud Logging entries
// carry a "hub" label matching the hub-scoped log query filter.
func (d *HTTPAgentDispatcher) SetHubName(name string) {
	d.hubName = name
}

// SetSecretBackend sets the secret backend for resolving secrets.
func (d *HTTPAgentDispatcher) SetSecretBackend(b secret.SecretBackend) {
	d.secretBackend = b
}

// SetHubID sets the hub instance ID for hub-scoped queries.
func (d *HTTPAgentDispatcher) SetHubID(id string) {
	d.hubID = id
}

// SetDevAuthToken sets the dev auth token to inject into agent containers.
// When set, agents receive SCION_DEV_TOKEN as a fallback authentication method.
func (d *HTTPAgentDispatcher) SetDevAuthToken(token string) {
	d.devAuthToken = token
}

// SetAuthzService sets the authorization service for progeny secret verification.
func (d *HTTPAgentDispatcher) SetAuthzService(a *AuthzService) {
	d.authzService = a
}

// SetTransportMinter sets the transport token minter, audience, and mode for
// injecting transport-layer OIDC tokens into agent dispatch payloads.
func (d *HTTPAgentDispatcher) SetTransportMinter(minter TransportTokenMinter, audience, mode string) {
	d.transportMinter = minter
	d.transportAudience = audience
	d.transportMode = mode
}

// SetGitHubAppMinter sets the GitHub App token minter for resolving
// GitHub App installation tokens during agent credential resolution.
func (d *HTTPAgentDispatcher) SetGitHubAppMinter(m GitHubAppTokenMinter) {
	d.githubAppMinter = m
}

// SetCrossNodeDeps wires the event publisher and command bus needed for
// cross-node lifecycle dispatch (B4-2). When both are set and a lifecycle
// op returns ErrLifecycleDeferred, the dispatcher writes durable intent,
// signals the owning node, and waits for the terminal phase.
func (d *HTTPAgentDispatcher) SetCrossNodeDeps(events EventPublisher, bus CommandBus) {
	d.events = events
	d.commandBus = bus
}

// SetDispatchMetrics wires the dispatch metrics recorder (B5-2).
func (d *HTTPAgentDispatcher) SetDispatchMetrics(rec dispatchmetrics.Recorder) {
	d.dispatchMetrics = rec
}

// SetHarnessConfigRepairer registers a callback that syncs a harness-config's
// DB manifest from storage when a hash mismatch is detected during dispatch.
func (d *HTTPAgentDispatcher) SetHarnessConfigRepairer(fn func(ctx context.Context, name string) error) {
	d.harnessConfigRepairer = fn
}

// SetHubAgentDefaultsProvider registers the accessor for the hub's operational
// agent_defaults, read on every dispatch so a settings change takes effect
// without a restart. Mirrors SetHarnessConfigRepairer.
func (d *HTTPAgentDispatcher) SetHubAgentDefaultsProvider(fn func() opsettings.AgentDefaultsSettings) {
	d.hubAgentDefaultsProvider = fn
}

// SetImageRegistry sets the image registry prefix for rewriting bare image
// names before dispatching to brokers.
func (d *HTTPAgentDispatcher) SetImageRegistry(registry string) {
	d.imageRegistry = registry
}

// SetTemplateRepairer registers a callback that syncs a template's DB manifest
// from storage when a hash mismatch is detected during dispatch.
func (d *HTTPAgentDispatcher) SetTemplateRepairer(fn func(ctx context.Context, ref string) error) {
	d.templateRepairer = fn
}

// isHashMismatchError reports whether err is a broker hash-mismatch error
// from resource hydration (template or harness-config).
func isHashMismatchError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "hash mismatch for file")
}

// repairHashMismatch attempts to sync the affected resource's DB manifest from
// storage. It inspects the error prefix to determine whether a template or
// harness-config needs repair. Returns nil on success so the caller can retry.
func (d *HTTPAgentDispatcher) repairHashMismatch(ctx context.Context, agent *store.Agent, dispatchErr error) error {
	if agent.AppliedConfig == nil {
		return fmt.Errorf("no applied config")
	}

	msg := dispatchErr.Error()

	// Route to the correct repairer based on the hydration error prefix.
	if strings.Contains(msg, "Failed to hydrate template:") {
		return d.repairTemplate(ctx, agent)
	}
	if strings.Contains(msg, "Failed to hydrate harness-config:") {
		return d.repairHarnessConfig(ctx, agent)
	}

	// Prefix not recognized — try both (harness-config first, then template).
	if err := d.repairHarnessConfig(ctx, agent); err == nil {
		return nil
	}
	return d.repairTemplate(ctx, agent)
}

func (d *HTTPAgentDispatcher) repairHarnessConfig(ctx context.Context, agent *store.Agent) error {
	if d.harnessConfigRepairer == nil || agent.AppliedConfig == nil || agent.AppliedConfig.HarnessConfig == "" {
		return fmt.Errorf("no repairer or harness config")
	}
	name := agent.AppliedConfig.HarnessConfig
	d.log.Warn("hash mismatch detected, attempting harness-config DB→storage repair",
		"agent", agent.Slug, "harnessConfig", name)
	if err := d.harnessConfigRepairer(ctx, name); err != nil {
		d.log.Warn("harness-config repair failed", "harnessConfig", name, "error", err)
		return err
	}
	d.log.Info("harness-config repair succeeded, retrying dispatch",
		"agent", agent.Slug, "harnessConfig", name)
	return nil
}

func (d *HTTPAgentDispatcher) repairTemplate(ctx context.Context, agent *store.Agent) error {
	if d.templateRepairer == nil {
		return fmt.Errorf("no template repairer")
	}
	var ref string
	if agent.AppliedConfig != nil {
		ref = agent.AppliedConfig.TemplateID
	}
	if ref == "" {
		ref = agent.Template
	}
	if ref == "" {
		return fmt.Errorf("no template reference")
	}
	d.log.Warn("hash mismatch detected, attempting template DB→storage repair",
		"agent", agent.Slug, "template", ref)
	if err := d.templateRepairer(ctx, ref); err != nil {
		d.log.Warn("template repair failed", "template", ref, "error", err)
		return err
	}
	d.log.Info("template repair succeeded, retrying dispatch",
		"agent", agent.Slug, "template", ref)
	return nil
}

// getBrokerEndpoint retrieves the endpoint URL for a runtime broker.
// Returns an empty string without error when no endpoint is configured,
// which is normal for brokers that connect via WebSocket control channel.
// The HybridBrokerClient will route through the control channel when
// available; only the HTTP fallback path requires a non-empty endpoint.
func (d *HTTPAgentDispatcher) getBrokerEndpoint(ctx context.Context, brokerID string) (string, error) {
	broker, err := d.store.GetRuntimeBroker(ctx, brokerID)
	if err != nil {
		return "", fmt.Errorf("failed to get runtime broker: %w", err)
	}

	return broker.Endpoint, nil
}

// buildCreateRequest builds a RemoteCreateAgentRequest from the agent's store record.
// This is shared between DispatchAgentCreate and DispatchAgentProvision.
func (d *HTTPAgentDispatcher) buildCreateRequest(ctx context.Context, agent *store.Agent, callerName string) (*RemoteCreateAgentRequest, error) {
	projectInfo := d.resolveDispatchProjectInfo(ctx, agent)

	// Build the remote create request
	req := &RemoteCreateAgentRequest{
		RequestID:     api.NewUUID(),
		ID:            agent.ID,
		Slug:          agent.Slug,
		Name:          agent.Name,
		ProjectID:     agent.ProjectID,
		UserID:        agent.OwnerID,
		HubEndpoint:   d.hubEndpoint,
		ProjectPath:   projectInfo.projectPath,
		ProjectSlug:   projectInfo.projectSlug,
		SharedDirs:    projectInfo.sharedDirs,
		WorkspaceMode: projectInfo.workspaceMode,
	}

	// Propagate attach mode from applied config
	if agent.AppliedConfig != nil {
		req.Attach = agent.AppliedConfig.Attach
	}

	// Propagate creator name for SCION_CREATOR env var
	if agent.AppliedConfig != nil && agent.AppliedConfig.CreatorName != "" {
		req.CreatorName = agent.AppliedConfig.CreatorName
	}

	// Pass workspace storage path for GCS bootstrap (non-git workspaces)
	if agent.AppliedConfig != nil && agent.AppliedConfig.WorkspaceStoragePath != "" {
		req.WorkspaceStoragePath = agent.AppliedConfig.WorkspaceStoragePath
	}

	if d.debug {
		d.log.Debug(callerName,
			"agent_id", agent.ID,
			"agentName", agent.Name,
			"hubEndpoint", d.hubEndpoint,
			"hasTokenGenerator", d.tokenGenerator != nil,
		)
	}

	// Generate agent token if token generator is available
	if d.tokenGenerator != nil {
		agentRole, additionalScopes := agentRoleAndScopes(agent)
		token, err := d.tokenGenerator.GenerateAgentToken(agent.ID, agent.ProjectID, agent.Ancestry, agentRole, additionalScopes)
		if err != nil {
			if d.debug {
				d.log.Warn("Failed to generate agent token", "error", err)
			}
			// Continue without token - agent will operate in unauthenticated mode
		} else {
			req.AgentToken = token
			if d.debug {
				d.log.Debug("Generated agent token", "length", len(token))
			}
		}
	} else if d.debug {
		d.log.Debug("No token generator configured - agent will not have Hub credentials")
	}

	// Add configuration if available
	if agent.AppliedConfig != nil {
		workspace := agent.AppliedConfig.Workspace
		gitClone := agent.AppliedConfig.GitClone
		// When the broker has a local provider path for this project, clear
		// the hub-native workspace path — the broker will derive its own
		// workspace location from the project path. However, keep GitClone
		// config: all hub-linked projects with a git remote use clone-based
		// provisioning (HTTPS + GitHub token) rather than worktree-based,
		// ensuring a consistent workspace strategy regardless of whether
		// the broker happens to have the repo locally.
		if projectInfo.projectPath != "" {
			if workspace == "" || filepath.IsAbs(workspace) {
				workspace = ""
			}
			// else: relative workspace -- keep it; broker joins with its own project root
		}
		var remoteGCPIdentity *RemoteGCPIdentityConfig
		if gcpID := agent.AppliedConfig.GCPIdentity; gcpID != nil {
			remoteGCPIdentity = &RemoteGCPIdentityConfig{
				MetadataMode: gcpID.MetadataMode,
				SAEmail:      gcpID.ServiceAccountEmail,
				ProjectID:    gcpID.ProjectID,
			}
		}
		image := agent.AppliedConfig.Image
		if image != "" && d.imageRegistry != "" {
			image = config.RewriteImageRegistry(image, d.imageRegistry)
		}
		req.Config = &RemoteAgentConfig{
			Template:                  agent.Template,
			Image:                     image,
			HarnessConfig:             agent.AppliedConfig.HarnessConfig,
			HarnessAuth:               agent.AppliedConfig.HarnessAuth,
			Task:                      agent.AppliedConfig.Task,
			Workspace:                 workspace,
			Profile:                   agent.AppliedConfig.Profile,
			Branch:                    agent.AppliedConfig.Branch,
			TemplateID:                agent.AppliedConfig.TemplateID,
			TemplateHash:              agent.AppliedConfig.TemplateHash,
			HarnessConfigID:           agent.AppliedConfig.HarnessConfigID,
			HarnessConfigHash:         agent.AppliedConfig.HarnessConfigHash,
			GitClone:                  gitClone,
			SharedWorkspace:           projectInfo.sharedWorkspace,
			GCPIdentity:               remoteGCPIdentity,
			ProjectPreStartHookScript: agent.AppliedConfig.ProjectPreStartHookScript,
		}

		// Hub operational agent_defaults (limits/resources only) travel in
		// their own low-rank slot, NOT in InlineConfig: InlineConfig lands in
		// the override position at provision.go's merge and would let a
		// hub-wide floor outrank a template's explicit max_turns. The broker
		// applies these below the template and above its own settings.yaml
		// defaults. Nil in file mode — see remoteHubAgentDefaults.
		if d.hubAgentDefaultsProvider != nil {
			req.Config.HubAgentDefaults = remoteHubAgentDefaults(d.hubAgentDefaultsProvider())
		}

		req.ResolvedEnv = agent.AppliedConfig.Env

		// Thread through the full inline ScionConfig for broker-side provisioning
		req.InlineConfig = agent.AppliedConfig.InlineConfig

		if d.debug {
			d.log.Debug("buildCreateRequest: config sent to broker",
				"template", agent.Template,
				"image", agent.AppliedConfig.Image,
				"harnessConfig", agent.AppliedConfig.HarnessConfig,
				"profile", agent.AppliedConfig.Profile,
				"templateID", agent.AppliedConfig.TemplateID,
				"projectPath", req.ProjectPath,
				"hasInlineConfig", agent.AppliedConfig.InlineConfig != nil,
			)
		}
	}

	// Clone req.ResolvedEnv to avoid mutating the shared agent.AppliedConfig.Env
	// map, which is a direct reference and may be read concurrently.
	if req.ResolvedEnv != nil {
		req.ResolvedEnv = maps.Clone(req.ResolvedEnv)
	}
	if req.ResolvedEnv == nil {
		req.ResolvedEnv = make(map[string]string)
	}
	injectModelEnv(req.ResolvedEnv, agent.AppliedConfig)
	injectThinkingLevelEnv(req.ResolvedEnv, agent.AppliedConfig)

	// Inject hub name so agents can label their Cloud Logging entries with the
	// hub identity, matching the hub-scoped log query filter (labels.hub).
	if d.hubName != "" {
		req.ResolvedEnv["SCION_HUB_NAME"] = d.hubName
	}

	// Resolve env vars from Hub storage (user/project/broker scopes) and merge.
	// Storage env vars fill in keys not already set (with a non-empty value)
	// by explicit config env vars. Empty-value config entries are passthrough
	// markers and should be overridden by storage values.
	envFromStorage, err := d.resolveEnvFromStorage(ctx, agent)
	if err != nil {
		if d.debug {
			d.log.Warn("buildCreateRequest: failed to resolve env from storage", "agent_id", agent.ID, "error", err)
		}
	} else if len(envFromStorage) > 0 {
		if req.ResolvedEnv == nil {
			req.ResolvedEnv = make(map[string]string)
		}
		for k, v := range envFromStorage {
			if existing, exists := req.ResolvedEnv[k]; !exists || existing == "" {
				req.ResolvedEnv[k] = v
			}
		}
	}

	// Include template secrets declarations for broker env-gather
	if agent.AppliedConfig != nil && agent.AppliedConfig.TemplateID != "" {
		tmpl, err := d.store.GetTemplate(ctx, agent.AppliedConfig.TemplateID)
		if err == nil && tmpl != nil && tmpl.Config != nil && len(tmpl.Config.Secrets) > 0 {
			req.RequiredSecrets = make([]api.RequiredSecret, len(tmpl.Config.Secrets))
			for i, s := range tmpl.Config.Secrets {
				req.RequiredSecrets[i] = api.RequiredSecret{
					Key:         s.Key,
					Description: s.Description,
					Type:        s.Type,
					Target:      s.Target,
				}
			}
		}
	}

	// Propagate no-auth intent from the agent's applied config.
	// NoAuth suppresses LLM-auth secrets (API keys, credential files) but must
	// NOT suppress git-related credentials (GITHUB_TOKEN) which are needed for
	// repository clone/pull operations regardless of LLM auth status.
	noAuth := agent.AppliedConfig != nil && agent.AppliedConfig.NoAuth
	if noAuth {
		req.NoAuth = true
		req.ResolvedSecrets = nil
		if d.debug {
			d.log.Debug("NoAuth enabled: skipping secret resolution", "agent_id", agent.ID)
		}

		// Exempt git credentials from NoAuth suppression. GITHUB_TOKEN is
		// stored in the project secrets table but is unrelated to LLM auth —
		// it enables repository clone/pull in clone-per-agent workspaces.
		// Without this, NoAuth blanket-suppresses all secrets including
		// GITHUB_TOKEN, causing git clone failures (#1165).
		if agent.ProjectID != "" && d.secretBackend != nil {
			ghSecret, err := d.secretBackend.Get(ctx, "GITHUB_TOKEN", secret.ScopeProject, agent.ProjectID)
			if err != nil {
				if d.debug {
					d.log.Debug("NoAuth: failed to resolve GITHUB_TOKEN from project secrets",
						"agent_id", agent.ID, "project_id", agent.ProjectID, "error", err)
				}
			} else if ghSecret != nil && ghSecret.Value != "" {
				req.ResolvedEnv["GITHUB_TOKEN"] = ghSecret.Value
				if d.debug {
					d.log.Debug("NoAuth: resolved GITHUB_TOKEN from project secrets for git operations",
						"agent_id", agent.ID, "project_id", agent.ProjectID)
				}
			}

			// Fall back to the creating user's profile-level GITHUB_TOKEN,
			// mirroring the cascade in resolveCloneToken. Users who store
			// GITHUB_TOKEN at user/profile scope only (no project-scoped token)
			// would otherwise still hit the NoAuth suppression bug (#1165).
			if (ghSecret == nil || ghSecret.Value == "") && agent.OwnerID != "" {
				ghSecret, err = d.secretBackend.Get(ctx, "GITHUB_TOKEN", secret.ScopeUser, agent.OwnerID)
				if err != nil {
					if d.debug {
						d.log.Debug("NoAuth: failed to resolve GITHUB_TOKEN from user secrets",
							"agent_id", agent.ID, "owner_id", agent.OwnerID, "error", err)
					}
				} else if ghSecret != nil && ghSecret.Value != "" {
					req.ResolvedEnv["GITHUB_TOKEN"] = ghSecret.Value
					if d.debug {
						d.log.Debug("NoAuth: resolved GITHUB_TOKEN from user secrets for git operations",
							"agent_id", agent.ID, "owner_id", agent.OwnerID)
					}
				}
			}
		}
	}

	// Resolve type-aware secrets from all applicable scopes
	if !noAuth {
		resolvedSecrets, err := d.resolveSecrets(ctx, agent)
		if err != nil {
			if d.debug {
				d.log.Warn("Failed to resolve secrets", "agent_id", agent.ID, "error", err)
			}
			// Continue without secrets rather than failing agent creation
		} else if len(resolvedSecrets) > 0 {
			req.ResolvedSecrets = resolvedSecrets
			if d.debug {
				d.log.Debug("Resolved secrets for agent", "count", len(resolvedSecrets))
			}

			// Inject environment-type secrets into ResolvedEnv so the broker
			// receives them as plain env vars for auth resolution. This mirrors
			// DispatchAgentStart which merges env-type secrets into resolvedEnv
			// before dispatching. Without this, the broker's auth pipeline
			// relies solely on buildAuthEnvOverlay in run.go, which may not
			// see secrets if they are only in ResolvedSecrets.
			if req.ResolvedEnv == nil {
				req.ResolvedEnv = make(map[string]string)
			}
			for _, s := range resolvedSecrets {
				if (s.Type == "environment" || s.Type == "") && s.Target != "" {
					if existing, exists := req.ResolvedEnv[s.Target]; !exists || existing == "" {
						req.ResolvedEnv[s.Target] = s.Value
					}
				}
			}
		}
	}

	// GitHub App token minting: if the project has a GitHub App installation,
	// always mint an installation token. GitHub App tokens take priority over
	// GITHUB_TOKEN from secrets/env because they provide managed, scoped access
	// with automatic refresh. If minting fails, fall back to any existing
	// GITHUB_TOKEN from secrets/env.
	if d.githubAppMinter != nil && agent.ProjectID != "" {
		project, projectErr := d.store.GetProject(ctx, agent.ProjectID)
		if projectErr == nil {
			// Determine which project to use for GitHub App token minting.
			// Prefer the agent's own project; fall back to a source project
			// referenced by label (e.g. for template-sync agents loading
			// from an external repo whose git project has the app installed).
			mintProject := project
			if project.GitHubInstallationID == nil {
				if sourceProjectID := agent.Labels["scion.dev/github-token-source-project"]; sourceProjectID != "" {
					if sg, sgErr := d.store.GetProject(ctx, sourceProjectID); sgErr == nil && sg.GitHubInstallationID != nil {
						mintProject = sg
						if d.debug {
							d.log.Debug("buildCreateRequest: using source project for GitHub App token",
								"sourceProjectID", sourceProjectID,
								"installationID", *sg.GitHubInstallationID)
						}
					}
				}
			}
			if mintProject.GitHubInstallationID != nil {
				if req.ResolvedEnv != nil && req.ResolvedEnv["GITHUB_TOKEN"] != "" {
					// User already has a GITHUB_TOKEN from secrets/env.
					// Respect it: skip overwriting with the GitHub App token.
					d.log.Warn("buildCreateRequest: user has GITHUB_TOKEN from secrets; skipping GitHub App token injection — user token takes precedence for gh CLI, GitHub App will still be used for git credential helper",
						"project_id", agent.ProjectID)
					req.ResolvedEnv["SCION_USER_GITHUB_TOKEN"] = "true"
					// Still enable the GitHub App machinery so the credential
					// helper can mint tokens for git push/pull operations.
					req.ResolvedEnv["SCION_GITHUB_APP_ENABLED"] = "true"
				} else {
					token, expiry, mintErr := d.githubAppMinter.MintGitHubAppTokenForProject(ctx, mintProject)
					if mintErr != nil {
						if d.debug {
							d.log.Warn("buildCreateRequest: GitHub App token minting failed, falling back to PAT",
								"error", mintErr, "project_id", agent.ProjectID)
						}
						// Fall through — PAT from secrets/env may still be available
					} else if token != "" {
						if req.ResolvedEnv == nil {
							req.ResolvedEnv = make(map[string]string)
						}
						req.ResolvedEnv["GITHUB_TOKEN"] = token
						req.ResolvedEnv["SCION_GITHUB_APP_ENABLED"] = "true"
						req.ResolvedEnv["SCION_GITHUB_TOKEN_EXPIRY"] = expiry
						req.ResolvedEnv["SCION_GITHUB_TOKEN_PATH"] = "/tmp/.github-token"
						if d.debug {
							d.log.Debug("buildCreateRequest: injected GitHub App token",
								"project_id", agent.ProjectID,
								"installationID", *mintProject.GitHubInstallationID,
								"expiry", expiry)
						}
					}
				}
			}
		}
	}

	// Collect project-scope secrets for provision-time credential resolution.
	// These are NOT merged into ResolvedEnv and will not appear in the container env.
	// NOT gated on noAuth: provisionCredentials serve skill resolution (gh://
	// convention tokens like GH_{OWNER}), not harness auth. Suppressing them under
	// noAuth starves the GitHubSkillResolver of credentials for private repos.
	if agent.ProjectID != "" && d.secretBackend != nil {
		projectSecrets, listErr := d.secretBackend.List(ctx, secret.Filter{
			Scope:   secret.ScopeProject,
			ScopeID: agent.ProjectID,
		})
		if listErr != nil {
			if d.debug {
				d.log.Warn("buildCreateRequest: failed to list project secrets for ProvisionCredentials",
					"agent_id", agent.ID, "error", listErr)
			}
		} else if len(projectSecrets) > 0 {
			type namedValue struct{ name, value string }
			fetched := make([]namedValue, len(projectSecrets))

			g, gctx := errgroup.WithContext(ctx)
			for i, sm := range projectSecrets {
				if sm.SecretType == store.SecretTypeInternal {
					continue
				}
				i, sm := i, sm // capture loop vars
				g.Go(func() error {
					sv, getErr := d.secretBackend.Get(gctx, sm.Name, secret.ScopeProject, agent.ProjectID)
					if getErr != nil {
						if d.debug {
							d.log.Warn("buildCreateRequest: failed to get project secret for ProvisionCredentials",
								"agent_id", agent.ID, "secret", sm.Name, "error", getErr)
						}
						return nil // don't fail the group for individual secrets
					}
					if sv != nil && sv.Value != "" {
						fetched[i] = namedValue{sm.Name, sv.Value}
					}
					return nil
				})
			}
			_ = g.Wait()

			creds := make(map[string]string)
			for _, nv := range fetched {
				if nv.name != "" {
					creds[nv.name] = nv.value
				}
			}
			if len(creds) > 0 {
				req.ProvisionCredentials = creds
			}
		}
	}

	// Log a summary of env resolution sources
	if d.debug {
		configEnvCount := 0
		if agent.AppliedConfig != nil {
			configEnvCount = len(agent.AppliedConfig.Env)
		}
		d.log.Debug("buildCreateRequest: env resolution summary",
			"configEnvCount", configEnvCount,
			"storageEnvCount", len(envFromStorage),
			"resolvedSecretsCount", len(req.ResolvedSecrets),
			"totalResolvedEnvCount", len(req.ResolvedEnv),
			"provisionCredentialsCount", len(req.ProvisionCredentials),
		)
	}

	// In dev-auth mode, inject the dev token so agents can use it as fallback auth
	if d.devAuthToken != "" {
		if req.ResolvedEnv == nil {
			req.ResolvedEnv = make(map[string]string)
		}
		req.ResolvedEnv["SCION_DEV_TOKEN"] = d.devAuthToken
	}

	// Transport token minting for platform-layer auth (IAP / Cloud Run invoker)
	if d.transportMinter != nil && d.transportAudience != "" {
		tToken, tExpiry, tErr := d.transportMinter.MintIDToken(ctx, d.transportAudience)
		if tErr != nil {
			if d.debug {
				d.log.Warn("buildCreateRequest: failed to mint transport token", "error", tErr)
			}
		} else if tToken != "" {
			if req.ResolvedEnv == nil {
				req.ResolvedEnv = make(map[string]string)
			}
			req.ResolvedEnv["SCION_TRANSPORT_TOKEN"] = tToken
			req.ResolvedEnv["SCION_TRANSPORT_AUDIENCE"] = d.transportAudience
			req.ResolvedEnv["SCION_TRANSPORT_TOKEN_EXPIRY"] = tExpiry.UTC().Format(time.RFC3339)
			if d.transportMode != "" {
				req.ResolvedEnv["SCION_TRANSPORT_MODE"] = d.transportMode
			}
		}
	}

	return req, nil
}

// projectDispatchInfo contains resolved project information for dispatching agent requests.
type projectDispatchInfo struct {
	projectPath     string
	projectSlug     string
	sharedDirs      []api.SharedDir
	sharedWorkspace bool   // true for git-workspace hybrid projects
	workspaceMode   string // resolved workspace mode label (e.g. "shared", "worktree-per-agent")
}

//nolint:unused // Kept for dispatcher compatibility while dispatch paths are split.
func (d *HTTPAgentDispatcher) resolveDispatchProjectPath(ctx context.Context, agent *store.Agent) (string, string) {
	info := d.resolveDispatchProjectInfo(ctx, agent)
	return info.projectPath, info.projectSlug
}

func (d *HTTPAgentDispatcher) resolveDispatchProjectInfo(ctx context.Context, agent *store.Agent) projectDispatchInfo {
	// Look up the local path for this project on the target runtime broker.
	// A provider LocalPath (linked project) takes precedence over hub-native
	// slug resolution, even for projects without a git remote. Only when there
	// is no provider path and no git remote do we fall back to projectSlug so
	// the broker resolves the conventional ~/.scion/projects/<slug> path.
	if agent.ProjectID == "" {
		return projectDispatchInfo{}
	}

	var info projectDispatchInfo

	project, err := d.store.GetProject(ctx, agent.ProjectID)
	if err != nil {
		return projectDispatchInfo{}
	}

	info.sharedDirs = project.SharedDirs
	info.sharedWorkspace = project.IsSharedWorkspace()
	info.workspaceMode = project.Labels[store.LabelWorkspaceMode]

	// First check if the broker has a registered local path for this project.
	if agent.RuntimeBrokerID != "" {
		provider, provErr := d.store.GetProjectProvider(ctx, agent.ProjectID, agent.RuntimeBrokerID)
		if provErr != nil {
			if d.debug {
				d.log.Warn("Failed to get project provider for path lookup", "error", provErr)
			}
		} else if provider.LocalPath != "" {
			info.projectPath = provider.LocalPath
			if d.debug {
				d.log.Debug("Found project path for broker", "brokerID", agent.RuntimeBrokerID, "path", info.projectPath)
			}
		}
	}
	// If no provider path was found, let the broker resolve the path via
	// slug. This applies to both hub-native projects (no git remote) and
	// git-anchored projects — the broker needs a project identity to create
	// agent directories under ~/.scion/projects/<slug>/ rather than falling
	// back to the global project.
	if info.projectPath == "" {
		info.projectSlug = project.Slug
	}
	return info
}

// applyBrokerResponse updates agent fields from the broker's response.
func (d *HTTPAgentDispatcher) applyBrokerResponse(agent *store.Agent, resp *RemoteAgentResponse) {
	if resp.Agent != nil {
		if d.debug {
			d.log.Debug("applyBrokerResponse: applying broker phase",
				"agentName", agent.Name,
				"previousPhase", agent.Phase,
				"brokerPhase", resp.Agent.Phase,
				"containerStatus", resp.Agent.ContainerStatus,
				"brokerAgentID", resp.Agent.ID,
			)
		}
		if resp.Agent.Phase != "" {
			agent.Phase = resp.Agent.Phase
		}
		if resp.Agent.Activity != "" {
			agent.Activity = resp.Agent.Activity
		}
		agent.ContainerStatus = resp.Agent.ContainerStatus
		if resp.Agent.ID != "" {
			agent.RuntimeState = "container:" + resp.Agent.ID
		}
		// Capture template, harness, and runtime from the broker response
		if resp.Agent.Template != "" {
			agent.Template = resp.Agent.Template
		}
		if agent.AppliedConfig != nil {
			if resp.Agent.HarnessConfig != "" {
				agent.AppliedConfig.HarnessConfig = resp.Agent.HarnessConfig
			}
			if resp.Agent.HarnessAuth != "" {
				agent.AppliedConfig.HarnessAuth = resp.Agent.HarnessAuth
			}
			if resp.Agent.Image != "" {
				agent.AppliedConfig.Image = resp.Agent.Image
			}
			if resp.Agent.Profile != "" {
				agent.AppliedConfig.Profile = resp.Agent.Profile
			}
		}
		if resp.Agent.Runtime != "" {
			agent.Runtime = resp.Agent.Runtime
		}
	} else if d.debug {
		d.log.Debug("applyBrokerResponse: broker response has nil Agent",
			"agentName", agent.Name,
		)
	}
}

// DispatchAgentCreate creates and starts an agent on the runtime broker.
func (d *HTTPAgentDispatcher) DispatchAgentCreate(ctx context.Context, agent *store.Agent) error {
	ctx, span := tracer.Start(ctx, "hub.dispatch.create")
	defer span.End()
	span.SetAttributes(
		attribute.String("scion.agent.id", agent.ID),
		attribute.String("scion.broker.id", agent.RuntimeBrokerID),
	)

	if err := requireRuntimeBrokerAssigned(agent); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	endpoint, err := d.getBrokerEndpoint(ctx, agent.RuntimeBrokerID)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	req, err := d.buildCreateRequest(ctx, agent, "DispatchAgentCreate")
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	resp, err := d.client.CreateAgent(ctx, agent.RuntimeBrokerID, endpoint, req)
	if isHashMismatchError(err) {
		if repairErr := d.repairHashMismatch(ctx, agent, err); repairErr == nil {
			resp, err = d.client.CreateAgent(ctx, agent.RuntimeBrokerID, endpoint, req)
		}
	}
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	d.applyBrokerResponse(agent, resp)
	return nil
}

// DispatchAgentProvision provisions an agent on the runtime broker without starting it.
// It uses the same GatherEnv two-pass mechanism as DispatchAgentCreateWithGather so
// that as_needed env vars (e.g. GOOGLE_CLOUD_PROJECT, GOOGLE_CLOUD_REGION) are
// resolved before auth provisioning runs on the broker.
func (d *HTTPAgentDispatcher) DispatchAgentProvision(ctx context.Context, agent *store.Agent) error {
	if err := requireRuntimeBrokerAssigned(agent); err != nil {
		return err
	}

	endpoint, err := d.getBrokerEndpoint(ctx, agent.RuntimeBrokerID)
	if err != nil {
		return err
	}

	req, err := d.buildCreateRequest(ctx, agent, "DispatchAgentProvision")
	if err != nil {
		return err
	}
	req.ProvisionOnly = true
	req.GatherEnv = true

	// Track which scope provided each key
	req.EnvSources = d.buildEnvSources(ctx, agent, req.ResolvedEnv)

	// First pass: use CreateAgentWithGather so the broker can report which
	// env vars are still needed (returned as a 202 with env requirements).
	resp, envReqs, err := d.client.CreateAgentWithGather(ctx, agent.RuntimeBrokerID, endpoint, req)
	if isHashMismatchError(err) {
		if repairErr := d.repairHashMismatch(ctx, agent, err); repairErr == nil {
			resp, envReqs, err = d.client.CreateAgentWithGather(ctx, agent.RuntimeBrokerID, endpoint, req)
		}
	}
	if errors.Is(err, ErrLifecycleDeferred) {
		// deferredCreateWithGather dispatches via DispatchAgentCreateWithGather which
		// does not set ProvisionOnly — fall back to returning the error rather than
		// accidentally triggering a full create on the remote node.
		return fmt.Errorf("provision not supported for cross-node broker: %w", err)
	} else if err != nil {
		return err
	} else if resp != nil {
		d.applyBrokerResponse(agent, resp)
	}

	// Second pass: if the broker reported needed keys, check whether any can
	// be satisfied by as_needed env vars or secrets — mirroring the pattern in
	// DispatchAgentCreateWithGather. We inline this instead of calling
	// DispatchFinalizeEnv because the finalize path does not set ProvisionOnly.
	if envReqs != nil && len(envReqs.Needs) > 0 {
		asNeededEnv := d.resolveAsNeededForKeys(ctx, agent, envReqs.Needs, envReqs.Alternatives)
		if len(asNeededEnv) > 0 {
			if req.ResolvedEnv == nil {
				req.ResolvedEnv = make(map[string]string)
			}
			for k, v := range asNeededEnv {
				req.ResolvedEnv[k] = v
			}
			req.EnvSources = d.buildEnvSources(ctx, agent, req.ResolvedEnv)

			// Replay the provision request with the resolved env.
			resp2, envReqs2, err2 := d.client.CreateAgentWithGather(ctx, agent.RuntimeBrokerID, endpoint, req)
			if isHashMismatchError(err2) {
				if repairErr := d.repairHashMismatch(ctx, agent, err2); repairErr == nil {
					resp2, envReqs2, err2 = d.client.CreateAgentWithGather(ctx, agent.RuntimeBrokerID, endpoint, req)
				}
			}
			if err2 != nil {
				return err2
			}
			if envReqs2 != nil && len(envReqs2.Needs) > 0 {
				d.log.Warn("DispatchAgentProvision: env vars still missing after second pass",
					"agent", agent.Name, "needs", envReqs2.Needs)
			}
			if resp2 != nil {
				d.applyBrokerResponse(agent, resp2)
			}
		}
	}

	// Merge resolved storage env vars back into AppliedConfig so they are
	// visible in the advanced config form. Exclude internal SCION_* vars
	// and dev tokens which are injected at start time. This runs after both
	// passes so that as_needed vars resolved in the second pass are included.
	if agent.AppliedConfig != nil && len(req.ResolvedEnv) > 0 {
		if agent.AppliedConfig.Env == nil {
			agent.AppliedConfig.Env = make(map[string]string)
		}
		for k, v := range req.ResolvedEnv {
			if strings.HasPrefix(k, "SCION_") {
				continue
			}
			if _, exists := agent.AppliedConfig.Env[k]; !exists {
				agent.AppliedConfig.Env[k] = v
			}
		}
	}

	return nil
}

// DispatchAgentCreateWithGather creates an agent with env-gather support.
// If the broker returns 202 with env requirements, it returns the requirements
// as the first value instead of an error.
func (d *HTTPAgentDispatcher) DispatchAgentCreateWithGather(ctx context.Context, agent *store.Agent) (*RemoteEnvRequirementsResponse, error) {
	dispatchStart := time.Now()
	if err := requireRuntimeBrokerAssigned(agent); err != nil {
		return nil, err
	}

	endpoint, err := d.getBrokerEndpoint(ctx, agent.RuntimeBrokerID)
	if err != nil {
		return nil, err
	}

	req, err := d.buildCreateRequest(ctx, agent, "DispatchAgentCreateWithGather")
	if err != nil {
		return nil, err
	}
	req.GatherEnv = true

	// Track which scope provided each key
	req.EnvSources = d.buildEnvSources(ctx, agent, req.ResolvedEnv)

	d.log.Info("Dispatcher: request built, sending to broker",
		"agent_id", agent.ID, "agent", agent.Name,
		"broker", agent.RuntimeBrokerID, "buildElapsed", time.Since(dispatchStart).String())
	brokerCallStart := time.Now()
	resp, envReqs, err := d.client.CreateAgentWithGather(ctx, agent.RuntimeBrokerID, endpoint, req)
	d.log.Info("Dispatcher: broker responded",
		"agent_id", agent.ID, "agent", agent.Name,
		"brokerElapsed", time.Since(brokerCallStart).String(),
		"totalElapsed", time.Since(dispatchStart).String())
	if isHashMismatchError(err) {
		if repairErr := d.repairHashMismatch(ctx, agent, err); repairErr == nil {
			resp, envReqs, err = d.client.CreateAgentWithGather(ctx, agent.RuntimeBrokerID, endpoint, req)
		}
	}
	if errors.Is(err, ErrLifecycleDeferred) {
		envReqs, err = d.deferredCreateWithGather(ctx, agent)
		if err != nil {
			return nil, err
		}
		// Fall through to the second-pass as_needed resolution below.
	} else if err != nil {
		return nil, err
	} else if resp != nil {
		d.applyBrokerResponse(agent, resp)
	}

	// Second pass: if the broker reported needed keys, check whether any can
	// be satisfied by as_needed env vars or secrets. If so, finalize them
	// transparently without requiring CLI intervention.
	if envReqs != nil && len(envReqs.Needs) > 0 {
		asNeededEnv := d.resolveAsNeededForKeys(ctx, agent, envReqs.Needs, envReqs.Alternatives)
		if len(asNeededEnv) > 0 {
			err := d.DispatchFinalizeEnv(ctx, agent, asNeededEnv)
			if err == nil {
				return nil, nil // All needs satisfied by as_needed entries
			}
			var stillMissing *ErrEnvStillMissing
			if errors.As(err, &stillMissing) {
				return stillMissing.Requirements, nil // Partial; remaining needs returned
			}
			return nil, err
		}
	}

	return envReqs, nil
}

// deferredCreateWithGather handles a cross-node create-with-gather via durable dispatch.
func (d *HTTPAgentDispatcher) deferredCreateWithGather(ctx context.Context, agent *store.Agent) (*RemoteEnvRequirementsResponse, error) {
	result, err := d.deferredDataOpResult(ctx, agent, "create", &CreateWithGatherDispatchArgs{})
	if err != nil {
		return nil, err
	}
	if result.Result == "" {
		return nil, nil
	}
	var cr CreateWithGatherResult
	if err := json.Unmarshal([]byte(result.Result), &cr); err != nil {
		return nil, fmt.Errorf("unmarshal create result: %w", err)
	}
	return cr.EnvRequirements, nil
}

// ErrEnvStillMissing is returned when a replay-based finalize discovers that
// required env keys are still unsatisfied after merging CLI-gathered values.
type ErrEnvStillMissing struct {
	Requirements *RemoteEnvRequirementsResponse
}

func (e *ErrEnvStillMissing) Error() string {
	return fmt.Sprintf("env still missing after finalize: %v", e.Requirements.Needs)
}

// DispatchFinalizeEnv replays a full create request with CLI-gathered env merged
// at highest precedence, instead of calling the broker's stateful finalize-env
// action. This makes the finalize HA-safe: the replay can land on any broker
// replica because it carries the complete request state.
func (d *HTTPAgentDispatcher) DispatchFinalizeEnv(ctx context.Context, agent *store.Agent, env map[string]string) error {
	if err := requireRuntimeBrokerAssigned(agent); err != nil {
		return err
	}

	endpoint, err := d.getBrokerEndpoint(ctx, agent.RuntimeBrokerID)
	if err != nil {
		return err
	}

	req, err := d.buildCreateRequest(ctx, agent, "DispatchFinalizeEnv")
	if err != nil {
		return err
	}
	req.GatherEnv = true

	if req.ResolvedEnv == nil {
		req.ResolvedEnv = map[string]string{}
	}
	for k, v := range env {
		req.ResolvedEnv[k] = v
	}

	req.EnvSources = d.buildEnvSources(ctx, agent, req.ResolvedEnv)

	resp, envReqs, err := d.client.CreateAgentWithGather(ctx, agent.RuntimeBrokerID, endpoint, req)
	if isHashMismatchError(err) {
		if repairErr := d.repairHashMismatch(ctx, agent, err); repairErr == nil {
			resp, envReqs, err = d.client.CreateAgentWithGather(ctx, agent.RuntimeBrokerID, endpoint, req)
		}
	}
	if errors.Is(err, ErrLifecycleDeferred) {
		return d.deferredFinalizeEnv(ctx, agent, env)
	}
	if err != nil {
		return err
	}

	if envReqs != nil && len(envReqs.Needs) > 0 {
		// Second pass: try to satisfy remaining needs with as_needed entries,
		// mirroring the pattern in DispatchAgentCreateWithGather.
		asNeededEnv := d.resolveAsNeededForKeys(ctx, agent, envReqs.Needs, envReqs.Alternatives)
		if len(asNeededEnv) > 0 {
			for k, v := range asNeededEnv {
				req.ResolvedEnv[k] = v
			}
			resp2, envReqs2, err2 := d.client.CreateAgentWithGather(
				ctx, agent.RuntimeBrokerID, endpoint, req,
			)
			if err2 != nil {
				return err2
			}
			if envReqs2 != nil && len(envReqs2.Needs) > 0 {
				return &ErrEnvStillMissing{Requirements: envReqs2}
			}
			if resp2 != nil {
				d.applyBrokerResponse(agent, resp2)
			}
			return nil
		}
		return &ErrEnvStillMissing{Requirements: envReqs}
	}

	if resp != nil {
		d.applyBrokerResponse(agent, resp)
	}
	return nil
}

// deferredFinalizeEnv handles a cross-node finalize_env via durable dispatch.
func (d *HTTPAgentDispatcher) deferredFinalizeEnv(ctx context.Context, agent *store.Agent, env map[string]string) error {
	return d.deferredDataOp(ctx, agent, "finalize_env", &FinalizeEnvDispatchArgs{Env: env})
}

// envScopePrecedence is the single, authoritative statement of the order in
// which Hub env var STORAGE scopes are applied, LOWEST PRECEDENCE FIRST:
//
//	runtime_broker  <  hub  <  project  <  user
//
// The slice below is written LOWEST FIRST, so read the sequence rather than a
// word: runtime_broker, then hub, then project, then user. runtime_broker is
// therefore the WEAKEST of the four in precedence and the FIRST element in the
// slice; user is the strongest and the last element. Saying "runtime_broker is
// last" is true of precedence and false of the literal, which is why the
// sequence is spelled out instead.
//
// It was the strongest until this changed, and
// that was an accident of the order four near-identical blocks happened to
// appear in — not a decision. Broker-scoped env is the most infrastructural and
// least specific of the four, so it is the weakest default rather than an
// override nobody can escape. The scope may be removed entirely in a future
// release; bottom-ranking it is a step in that direction.
//
// THIS IS ONLY THE STORAGE-SCOPE LADDER. It is not the whole settings
// precedence chain — templates, harness overrides, profiles and project
// annotations all sit between these scopes and the final agent config, and they
// are resolved elsewhere. See the settings-precedence reference doc for the
// full stack; do not read the four names above as a complete ordering.
//
// Where explicit agent config sits, precisely, because the relation is NOT a
// plain inequality: buildCreateRequest seeds ResolvedEnv from
// AppliedConfig.Env, then storage fills only the keys config left ABSENT or set
// to the EMPTY STRING. So a non-empty config value outranks all four scopes,
// while an empty one is a passthrough marker that deliberately yields to
// storage.
//
// The three consumers in THIS file derive their order from this list — the
// resolver (resolveEnvFromStorage), the provenance reporter that tells the CLI
// where a value came from (buildEnvSources), and the startup shadow warning
// (WarnOutrankedBrokerEnvKeys). For those three, changing the order here is the
// only edit required, and it is a user-visible behaviour change for any
// deployment that defines the same key in two scopes.
//
// THAT IS NOT THE SAME AS "everywhere", AND THE DIFFERENCE IS LOAD-BEARING.
// Server.buildEnvGatherResponse in handlers_agents_core.go answers the same
// "where did this value come from" question for the env-gather path from its
// own hardcoded chain: it defaults the reported scope to hub, then checks user,
// project, config and secret, and never consults runtime_broker at all — so a
// broker-only key is reported there as "hub". It does not reference this list,
// and its user-before-project order agrees with this one by coincidence rather
// than by construction. Reordering here does not reach it. That reporter is
// tracked as a separate follow-up and is deliberately not changed by phase 10;
// what matters here is that you must not read this list as the only place a
// scope order is written down.
var envScopePrecedence = []string{
	store.ScopeRuntimeBroker,
	store.ScopeHub,
	store.ScopeProject,
	store.ScopeUser,
}

// envScopeID returns the ID the given scope is keyed by for this agent, or ""
// if the scope does not apply (e.g. an agent with no project). The hub scope is
// keyed by the hub's own instance ID, not by anything on the agent.
func (d *HTTPAgentDispatcher) envScopeID(scope string, agent *store.Agent) string {
	switch scope {
	case store.ScopeHub:
		return d.hubID
	case store.ScopeProject:
		return agent.ProjectID
	case store.ScopeUser:
		return agent.OwnerID
	case store.ScopeRuntimeBroker:
		return agent.RuntimeBrokerID
	default:
		return ""
	}
}

// envScopesInPrecedenceOrder returns the env var storage queries that apply to
// this agent, lowest precedence first, so a caller can simply run them in order
// and let later scopes overwrite earlier ones.
//
// Scopes whose scope ID is empty for this agent are omitted, with the exception
// of the hub scope: it is always queried, because an empty ScopeID means "no
// scope-ID filter" to the store and the hub scope has always been queried
// unconditionally.
func (d *HTTPAgentDispatcher) envScopesInPrecedenceOrder(agent *store.Agent) []store.EnvVarFilter {
	if agent == nil {
		return nil
	}
	filters := make([]store.EnvVarFilter, 0, len(envScopePrecedence))
	for _, scope := range envScopePrecedence {
		scopeID := d.envScopeID(scope, agent)
		if scopeID == "" && scope != store.ScopeHub {
			if d.debug {
				d.log.Debug("env scope does not apply to agent (empty scope ID)", "scope", scope, "agent_id", agent.ID)
			}
			continue
		}
		filters = append(filters, store.EnvVarFilter{Scope: scope, ScopeID: scopeID})
	}
	return filters
}

// envScopeSourceLabel maps a storage scope to the source name reported to the
// CLI. The labels are the user-facing names, which are not identical to the
// scope constants: store.ScopeRuntimeBroker is reported as "broker".
func envScopeSourceLabel(scope string) string {
	switch scope {
	case store.ScopeHub:
		return "hub"
	case store.ScopeProject:
		return "project"
	case store.ScopeUser:
		return "user"
	case store.ScopeRuntimeBroker:
		return "broker"
	default:
		return scope
	}
}

// envScopesOutranking returns the scopes in order that beat the given scope,
// i.e. those appearing after it. Returns nil if scope is not in order at all,
// and an empty slice if nothing outranks it.
//
// This is derived from the ordering list rather than hard-coded so that moving
// an entry in envScopePrecedence changes who outranks whom for all three
// consumers in this file at once — the same property that keeps the resolver
// and the `scion hub env list` provenance reporter from drifting apart. It says
// nothing about reporters that do not read the list; see envScopePrecedence for
// the one that does not.
func envScopesOutranking(order []string, scope string) []string {
	at := slices.Index(order, scope)
	if at < 0 {
		return nil
	}
	return slices.Clone(order[at+1:])
}

// envScopeCollision is one env var key that is defined at some scope and also
// at a scope that outranks it, so the lower scope's value is shadowed.
type envScopeCollision struct {
	// Key is the env var key defined in both places.
	Key string
	// ScopeIDs are the IDs, within the outranked scope, that define Key —
	// for the runtime_broker scope these are broker IDs. Sorted.
	ScopeIDs []string
	// OutrankedBy names the scopes that outrank the outranked scope and also
	// define Key, lowest precedence first.
	OutrankedBy []string
}

// envScopeCollisions reports the keys defined at scope `target` that are also
// defined at some scope outranking `target` under `order`, so that the value
// set at `target` never reaches an agent that the higher scope also applies to.
//
// `order` is a parameter rather than a read of envScopePrecedence so that this
// can be exercised against a ladder other than the one currently compiled in —
// which is the only way to test the warning while the ordering change it exists
// to announce has not landed yet.
//
// It deliberately OVER-reports: it matches on key alone and does not compare
// values or check that the two scope IDs share any agent. A broker-scoped key
// shadowed only by a user who never runs an agent on that broker is still
// listed. For a warning about a silent, unmigratable behaviour flip, a false
// positive costs a line of boot log and a false negative costs an operator
// their pinned value.
func envScopeCollisions(order []string, target string, vars []store.EnvVar) []envScopeCollision {
	higher := envScopesOutranking(order, target)
	if len(higher) == 0 {
		return nil
	}
	// key -> scope IDs at the target scope, and key -> outranking scopes.
	targetIDs := make(map[string]map[string]bool)
	shadowedBy := make(map[string]map[string]bool)
	for _, v := range vars {
		switch {
		case v.Scope == target:
			if targetIDs[v.Key] == nil {
				targetIDs[v.Key] = make(map[string]bool)
			}
			targetIDs[v.Key][v.ScopeID] = true
		case slices.Contains(higher, v.Scope):
			if shadowedBy[v.Key] == nil {
				shadowedBy[v.Key] = make(map[string]bool)
			}
			shadowedBy[v.Key][v.Scope] = true
		}
	}

	collisions := make([]envScopeCollision, 0, len(targetIDs))
	for key, ids := range targetIDs {
		scopes := shadowedBy[key]
		if len(scopes) == 0 {
			continue
		}
		// Report outranking scopes in precedence order, not alphabetically, so
		// the log reads in the same direction as the ladder.
		outrankedBy := make([]string, 0, len(scopes))
		for _, scope := range higher {
			if scopes[scope] {
				outrankedBy = append(outrankedBy, scope)
			}
		}
		scopeIDs := slices.Sorted(maps.Keys(ids))
		collisions = append(collisions, envScopeCollision{Key: key, ScopeIDs: scopeIDs, OutrankedBy: outrankedBy})
	}
	slices.SortFunc(collisions, func(a, b envScopeCollision) int {
		return strings.Compare(a.Key, b.Key)
	})
	return collisions
}

// WarnOutrankedBrokerEnvKeys logs, once at hub startup, every env var key that
// is set at runtime_broker scope and also at a scope that outranks
// runtime_broker — that is, every key whose broker-scoped value is silently not
// the one agents receive.
//
// It exists because moving runtime_broker down the ladder (design §3.4 variant
// 4-B) is a behaviour change with no migration available: the hub cannot tell a
// value a broker operator pinned deliberately from one set by accident, so it
// cannot fix them and must not try. Naming the affected keys at boot is the
// only warning that can be offered, and it is one query per scope.
//
// Whether it does anything at all is decided by envScopePrecedence and nothing
// else. UNDER THE LADDER THAT SHIPPED IN PHASE 10b, runtime_broker is the
// weakest scope, so hub, project and user all outrank it, envScopesOutranking
// returns those three, and THIS CHECK IS LIVE: it issues one query per
// outranking scope and warns on every shadowed key. Do not read the call site
// added at boot as a no-op.
//
// It goes inert only if runtime_broker is moved back to the top of that list,
// at which point envScopesOutranking returns empty and this returns before
// issuing a single query. The warning and the change it warns about are driven
// by the same one line, in both directions.
func (d *HTTPAgentDispatcher) WarnOutrankedBrokerEnvKeys(ctx context.Context) error {
	higher := envScopesOutranking(envScopePrecedence, store.ScopeRuntimeBroker)
	if len(higher) == 0 {
		return nil
	}

	// One query per scope. An empty ScopeID is "no scope-ID filter" to the
	// store, so each of these returns the scope's vars across every ID
	// (entadapter/secret_store.go: the ScopeID predicate is only applied when
	// the field is non-empty).
	var vars []store.EnvVar
	for _, scope := range append([]string{store.ScopeRuntimeBroker}, higher...) {
		got, err := d.store.ListEnvVars(ctx, store.EnvVarFilter{Scope: scope})
		if err != nil {
			return fmt.Errorf("listing %s-scoped env vars: %w", scope, err)
		}
		vars = append(vars, got...)
	}

	collisions := envScopeCollisions(envScopePrecedence, store.ScopeRuntimeBroker, vars)
	if len(collisions) == 0 {
		return nil
	}

	d.log.Warn("runtime_broker env vars are overridden by higher-precedence scopes; agents receive the higher scope's value",
		"key_count", len(collisions),
		"precedence_lowest_first", strings.Join(envScopePrecedence, " < "))
	for _, c := range collisions {
		d.log.Warn("broker-scoped env var is shadowed",
			"key", c.Key,
			"broker_ids", c.ScopeIDs,
			"outranked_by", c.OutrankedBy)
	}
	return nil
}

// resolveEnvFromStorage queries Hub env var storage for every scope that
// applies to the agent and returns a merged map. Scopes are applied lowest
// precedence first; the order itself is stated in exactly one place,
// envScopePrecedence above.
//
// The caller then overlays explicit agent config env on top of the result, so
// agent config outranks every storage scope (see buildCreateRequest).
func (d *HTTPAgentDispatcher) resolveEnvFromStorage(ctx context.Context, agent *store.Agent) (map[string]string, error) {
	result := make(map[string]string)
	if agent == nil {
		return result, nil
	}

	for _, filter := range d.envScopesInPrecedenceOrder(agent) {
		vars, err := d.store.ListEnvVars(ctx, filter)
		if err != nil {
			if d.debug {
				d.log.Warn("Failed to list env vars", "scope", filter.Scope, "scope_id", filter.ScopeID, "error", err)
			}
			continue
		}
		if d.debug {
			keys := make([]string, 0, len(vars))
			for _, v := range vars {
				keys = append(keys, v.Key)
			}
			d.log.Debug("resolveEnvFromStorage: scope", "scope", filter.Scope, "scope_id", filter.ScopeID, "count", len(vars), "keys", keys)
		}
		for _, v := range vars {
			if v.InjectionMode == store.InjectionModeAsNeeded {
				continue
			}
			result[v.Key] = v.Value
		}
	}

	// Progeny env var resolution: when the agent has ancestry, include
	// user-scoped env vars marked allowProgeny (with injectionMode=always)
	// whose creator is in the ancestry chain. These are added at user-scope
	// precedence — project/broker env vars with the same key will already
	// have overridden them.
	if agent != nil && len(agent.Ancestry) > 1 {
		progenyVars, err := d.store.ListProgenyEnvVars(ctx, agent.Ancestry)
		if err != nil {
			if d.debug {
				d.log.Warn("resolveEnvFromStorage: failed to list progeny env vars", "error", err)
			}
		} else {
			for _, v := range progenyVars {
				if _, exists := result[v.Key]; exists {
					continue // higher-precedence scope already set this key
				}
				result[v.Key] = v.Value
			}
		}
	}

	return result, nil
}

// resolveAsNeededForKeys resolves as_needed env vars and environment-type
// secrets whose key/target matches one of the requested keys. It returns a
// map suitable for passing to DispatchFinalizeEnv.
//
// This is the second pass of the two-pass env-gather resolution: the first
// pass (resolveEnvFromStorage + resolveSecrets) skips as_needed entries, then
// the broker reports which keys are still needed, and this function checks
// whether any of those keys can be satisfied by as_needed entries.
//
// Known limitation: file-type as_needed secrets are not handled here because
// DispatchFinalizeEnv only accepts a string key=value map. File-type secrets
// that need on-demand injection would require a different mechanism.
func (d *HTTPAgentDispatcher) resolveAsNeededForKeys(
	ctx context.Context,
	agent *store.Agent,
	keys []string,
	alternatives map[string][]string,
) map[string]string {
	result := make(map[string]string)
	keySet := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		keySet[k] = struct{}{}
	}

	// Expand keySet with alternatives and build a reverse map so that when
	// a stored var is keyed by an alternative name, we store its value under
	// the canonical key (which is what the broker expects).
	var altToCanonical map[string]string
	resultIsCanonical := make(map[string]bool)
	resultScopeIdx := make(map[string]int)
	if len(alternatives) > 0 {
		altToCanonical = make(map[string]string)
		for canonical, alts := range alternatives {
			for _, alt := range alts {
				keySet[alt] = struct{}{}
				altToCanonical[alt] = canonical
			}
		}
	}

	// 1. Check env_vars table (all scopes, in precedence order so last-wins).
	for _, filter := range d.envScopesInPrecedenceOrder(agent) {
		vars, err := d.store.ListEnvVars(ctx, filter)
		if err != nil {
			if d.debug {
				d.log.Warn("resolveAsNeededForKeys: failed to list env vars",
					"scope", filter.Scope, "scope_id", filter.ScopeID, "error", err)
			}
			continue
		}
		for _, v := range vars {
			if v.InjectionMode != store.InjectionModeAsNeeded {
				continue
			}
			if _, needed := keySet[v.Key]; needed {
				canonical, isAlt := altToCanonical[v.Key]
				resultKey := v.Key
				if isAlt {
					resultKey = canonical
				}
				currentScopeIdx := slices.Index(envScopePrecedence, filter.Scope)
				isCanonical := !isAlt
				storedScopeIdx, alreadySet := resultScopeIdx[resultKey]
				if !alreadySet || currentScopeIdx > storedScopeIdx {
					result[resultKey] = v.Value
					resultIsCanonical[resultKey] = isCanonical
					resultScopeIdx[resultKey] = currentScopeIdx
				} else if currentScopeIdx == storedScopeIdx {
					if isCanonical && !resultIsCanonical[resultKey] {
						result[resultKey] = v.Value
						resultIsCanonical[resultKey] = true
					}
				}
			}
		}
	}

	// 2. Check secrets (all scopes via backend.Resolve).
	// Only environment-type secrets can be mapped to env key=value pairs.
	if d.secretBackend != nil {
		var resolveOpts *secret.ResolveOpts
		if len(agent.Ancestry) > 1 && d.authzService != nil {
			agentID := agent.ID
			ancestry := agent.Ancestry
			resolveOpts = &secret.ResolveOpts{
				AgentAncestry: ancestry,
				AuthzCheck: func(s secret.SecretMeta) bool {
					decision := d.authzService.CheckAccess(ctx, &agentIdentityWrapper{
						AgentTokenClaims: &AgentTokenClaims{
							Claims:    jwt.Claims{Subject: agentID},
							ProjectID: agent.ProjectID,
							Ancestry:  ancestry,
						},
					}, Resource{
						Type: "secret",
						ID:   s.ID,
					}, ActionRead)
					return decision.Allowed
				},
			}
		}

		resolved, err := d.secretBackend.Resolve(
			ctx, agent.OwnerID, agent.ProjectID, agent.RuntimeBrokerID, resolveOpts)
		if err != nil {
			if d.debug {
				d.log.Warn("resolveAsNeededForKeys: failed to resolve secrets", "error", err)
			}
		} else {
			// Iterate in reverse: resolved is ordered lowest-precedence first
			// (runtime_broker < hub < project < user), so walking backwards
			// lets higher-precedence secrets win.
			for i := len(resolved) - 1; i >= 0; i-- {
				sv := resolved[i]
				if sv.InjectionMode != store.InjectionModeAsNeeded {
					continue
				}
				// Only environment-type secrets map to env vars.
				if sv.SecretType != store.SecretTypeEnvironment && sv.SecretType != "" {
					continue
				}
				target := sv.Target
				if target == "" {
					target = sv.Name
				}
				if _, needed := keySet[target]; needed {
					// Store under the canonical key if this was an alternative match
					resultKey := target
					if canonical, isAlt := altToCanonical[target]; isAlt {
						if _, already := result[canonical]; already {
							continue // canonical key already matched; don't overwrite
						}
						resultKey = canonical
					}
					if _, alreadySet := result[resultKey]; !alreadySet {
						result[resultKey] = sv.Value
					}
				}
			}
		}
	}

	if d.debug && len(result) > 0 {
		resolvedKeys := make([]string, 0, len(result))
		for k := range result {
			resolvedKeys = append(resolvedKeys, k)
		}
		d.log.Debug("resolveAsNeededForKeys: resolved as_needed entries",
			"count", len(result), "keys", resolvedKeys)
	}

	return result
}

// buildEnvSources creates a map of env key -> scope for reporting to the CLI.
//
// It walks the same scopes in the same order as resolveEnvFromStorage, from the
// same envScopePrecedence list, so the source it reports is always the scope
// whose value actually won and the two functions cannot drift apart. Agent
// config is applied last because it outranks every storage scope.
func (d *HTTPAgentDispatcher) buildEnvSources(ctx context.Context, agent *store.Agent, resolvedEnv map[string]string) map[string]string {
	sources := make(map[string]string)
	if agent == nil {
		return sources
	}

	for _, filter := range d.envScopesInPrecedenceOrder(agent) {
		vars, err := d.store.ListEnvVars(ctx, filter)
		if err != nil {
			if d.debug {
				d.log.Warn("Failed to list env vars for source reporting", "scope", filter.Scope, "scope_id", filter.ScopeID, "error", err)
			}
			continue
		}
		label := envScopeSourceLabel(filter.Scope)
		for _, v := range vars {
			if v.InjectionMode == store.InjectionModeAsNeeded {
				continue
			}
			if _, inResolved := resolvedEnv[v.Key]; inResolved {
				sources[v.Key] = label
			}
		}
	}

	// Check config scope (outranks every storage scope)
	if agent.AppliedConfig != nil {
		for k := range agent.AppliedConfig.Env {
			if _, inResolved := resolvedEnv[k]; inResolved {
				sources[k] = "config"
			}
		}
	}

	return sources
}

// DispatchAgentStart starts an agent on the runtime broker. When resume is
// true, the harness is asked to continue its prior session (e.g. Claude
// --continue) instead of starting a fresh conversation. The hub is the source
// of truth for resume: callers compute it from the agent's stored phase
// (suspended → resume).
func (d *HTTPAgentDispatcher) DispatchAgentStart(ctx context.Context, agent *store.Agent, task string, resume bool) error {
	ctx, span := tracer.Start(ctx, "hub.dispatch.start")
	defer span.End()
	span.SetAttributes(
		attribute.String("scion.agent.id", agent.ID),
		attribute.String("scion.broker.id", agent.RuntimeBrokerID),
	)

	if err := requireRuntimeBrokerAssigned(agent); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	endpoint, err := d.getBrokerEndpoint(ctx, agent.RuntimeBrokerID)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	// If no explicit task provided, fall back to the agent's applied config
	// task. Skip this on a pure resume (no new message): the harness should
	// just continue its prior session rather than be re-handed the original
	// creation task. A wake-with-message still passes that message as task.
	if task == "" && !resume && agent.AppliedConfig != nil {
		task = agent.AppliedConfig.Task
	}

	projectInfo := d.resolveDispatchProjectInfo(ctx, agent)
	projectPath := projectInfo.projectPath
	projectSlug := projectInfo.projectSlug

	// Resolve env vars from Hub storage (user/project/broker scopes) so that
	// API keys and other secrets are available when restarting an agent.
	resolvedEnv := make(map[string]string)

	// Start with agent's applied config env (template/config-level vars)
	if agent.AppliedConfig != nil {
		for k, v := range agent.AppliedConfig.Env {
			resolvedEnv[k] = v
		}
	}

	injectModelEnv(resolvedEnv, agent.AppliedConfig)
	injectThinkingLevelEnv(resolvedEnv, agent.AppliedConfig)

	// Merge env vars from Hub storage; storage vars fill in keys not already
	// set (with a non-empty value) by explicit config env vars.
	// Empty-value config entries are passthrough markers — storage values
	// should override them so that hub-stored secrets (API keys, etc.) are
	// available to the agent.
	envFromStorage, err := d.resolveEnvFromStorage(ctx, agent)
	if err != nil {
		if d.debug {
			d.log.Warn("DispatchAgentStart: failed to resolve env from storage", "error", err)
		}
	} else if len(envFromStorage) > 0 {
		for k, v := range envFromStorage {
			if existing, exists := resolvedEnv[k]; !exists || existing == "" {
				resolvedEnv[k] = v
			}
		}
	}

	// Resolve type-aware secrets and inject environment-type secrets
	resolvedSecrets, err := d.resolveSecrets(ctx, agent)
	if err != nil {
		if d.debug {
			d.log.Warn("DispatchAgentStart: failed to resolve secrets", "error", err)
		}
	} else {
		for _, s := range resolvedSecrets {
			if (s.Type == "environment" || s.Type == "") && s.Target != "" {
				if existing, exists := resolvedEnv[s.Target]; !exists || existing == "" {
					resolvedEnv[s.Target] = s.Value
				}
			}
		}
	}

	// Include agent identity and hub connectivity so the container can
	// report status to the Hub. The createAgent path sets these via the
	// request body, but the startAgent path on the broker doesn't — so
	// we inject them here as resolved env vars.
	if agent.ID != "" {
		resolvedEnv["SCION_AGENT_ID"] = agent.ID
	}
	if agent.ProjectID != "" {
		resolvedEnv["SCION_GROVE_ID"] = agent.ProjectID
		resolvedEnv["SCION_PROJECT_ID"] = agent.ProjectID
	}
	if agent.Slug != "" {
		resolvedEnv["SCION_AGENT_SLUG"] = agent.Slug
	}
	// Include hub endpoint so the broker can inject it into the container.
	// The createAgent path sends this as req.HubEndpoint, but the startAgent
	// path relies on the broker's own config which may be empty for standalone
	// brokers. Including it here ensures the broker always has the endpoint.
	if d.hubEndpoint != "" {
		resolvedEnv["SCION_HUB_ENDPOINT"] = d.hubEndpoint
	}
	// Include hub name so agents can label their Cloud Logging entries with
	// the hub identity, matching the hub-scoped log query filter (labels.hub).
	if d.hubName != "" {
		resolvedEnv["SCION_HUB_NAME"] = d.hubName
	}

	// Inject canonical workspace sharing mode and git-ness so the broker can
	// surface them in the container env on the start path.  The createAgent
	// path carries these via WorkspaceMode in the request body; the startAgent
	// path relies on resolvedEnv injection (this block) following the existing
	// SCION_AGENT_ID / SCION_METADATA_MODE pattern.
	//
	// Resolve once so the switch below uses canonical constants — unrecognized
	// or future wire labels safely fall back to shared-plain behavior.
	resolvedMode := store.ResolveWorkspaceSharingMode(projectInfo.workspaceMode)
	if projectInfo.workspaceMode != "" {
		resolvedEnv["SCION_WORKSPACE_MODE"] = string(resolvedMode)
	}
	switch resolvedMode {
	case store.SharingModeClonePerAgent, store.SharingModeWorktreePerAgent:
		resolvedEnv["SCION_WORKSPACE_GIT"] = "true"
	case store.SharingModeSharedPlain:
		// For shared-plain, git-ness is detected from the applied GitClone config.
		// Note: broker-local linked projects where the workspace is already a
		// git repo on disk but has no HTTPS GitClone config cannot be detected
		// as git-backed here. The broker's on-disk util.IsGitRepoDir check in
		// buildStartContext covers this for the create path; on start/restart paths
		// SCION_WORKSPACE_GIT will be absent for such workspaces. This is an
		// acknowledged limitation noted in the design doc.
		if agent.AppliedConfig != nil && agent.AppliedConfig.GitClone != nil {
			resolvedEnv["SCION_WORKSPACE_GIT"] = "true"
		}
	}

	// Inject GCP identity env vars so the broker can configure the
	// metadata-server sidecar correctly on (re-)start.  During the
	// createAgent path this information travels inside CreateAgentConfig,
	// but the startAgent path doesn't carry that struct, so we surface
	// the values through resolvedEnv instead.
	if agent.AppliedConfig != nil {
		if gcpID := agent.AppliedConfig.GCPIdentity; gcpID != nil {
			resolvedEnv["SCION_METADATA_MODE"] = gcpID.MetadataMode
			if gcpID.MetadataMode == store.GCPMetadataModeAssign {
				resolvedEnv["SCION_METADATA_SA_EMAIL"] = gcpID.ServiceAccountEmail
				resolvedEnv["SCION_METADATA_PROJECT_ID"] = gcpID.ProjectID
			}
		}
	}

	// Generate a fresh agent token for Hub authentication
	if d.tokenGenerator != nil {
		agentRole, additionalScopes := agentRoleAndScopes(agent)
		token, err := d.tokenGenerator.GenerateAgentToken(agent.ID, agent.ProjectID, agent.Ancestry, agentRole, additionalScopes)
		if err != nil {
			if d.debug {
				d.log.Warn("DispatchAgentStart: failed to generate agent token", "error", err)
			}
		} else if token != "" {
			resolvedEnv["SCION_AUTH_TOKEN"] = token
		}
	}

	// Transport token minting for platform-layer auth (IAP / Cloud Run invoker)
	if d.transportMinter != nil && d.transportAudience != "" {
		tToken, tExpiry, tErr := d.transportMinter.MintIDToken(ctx, d.transportAudience)
		if tErr != nil {
			if d.debug {
				d.log.Warn("DispatchAgentStart: failed to mint transport token", "error", tErr)
			}
		} else if tToken != "" {
			resolvedEnv["SCION_TRANSPORT_TOKEN"] = tToken
			resolvedEnv["SCION_TRANSPORT_AUDIENCE"] = d.transportAudience
			resolvedEnv["SCION_TRANSPORT_TOKEN_EXPIRY"] = tExpiry.UTC().Format(time.RFC3339)
			if d.transportMode != "" {
				resolvedEnv["SCION_TRANSPORT_MODE"] = d.transportMode
			}
		}
	}

	// GitHub App token minting for agent start
	if d.githubAppMinter != nil && agent.ProjectID != "" {
		project, projectErr := d.store.GetProject(ctx, agent.ProjectID)
		if projectErr == nil {
			mintProject := project
			if project.GitHubInstallationID == nil {
				if sourceProjectID := agent.Labels["scion.dev/github-token-source-project"]; sourceProjectID != "" {
					if sg, sgErr := d.store.GetProject(ctx, sourceProjectID); sgErr == nil && sg.GitHubInstallationID != nil {
						mintProject = sg
					}
				}
			}
			if mintProject.GitHubInstallationID != nil {
				if resolvedEnv["GITHUB_TOKEN"] == "" {
					token, expiry, mintErr := d.githubAppMinter.MintGitHubAppTokenForProject(ctx, mintProject)
					if mintErr != nil {
						if d.debug {
							d.log.Warn("DispatchAgentStart: GitHub App token minting failed",
								"error", mintErr, "project_id", agent.ProjectID)
						}
					} else if token != "" {
						resolvedEnv["GITHUB_TOKEN"] = token
						resolvedEnv["SCION_GITHUB_APP_ENABLED"] = "true"
						resolvedEnv["SCION_GITHUB_TOKEN_EXPIRY"] = expiry
						resolvedEnv["SCION_GITHUB_TOKEN_PATH"] = "/tmp/.github-token"
					}
				} else {
					d.log.Warn("DispatchAgentStart: user GITHUB_TOKEN takes precedence over GitHub App token — user token will be used for gh CLI, GitHub App for git credential helper",
						"project_id", agent.ProjectID)
					resolvedEnv["SCION_USER_GITHUB_TOKEN"] = "true"
					resolvedEnv["SCION_GITHUB_APP_ENABLED"] = "true"
				}
			}
		}
	}

	if d.debug {
		configEnvCount := 0
		if agent.AppliedConfig != nil {
			configEnvCount = len(agent.AppliedConfig.Env)
		}
		d.log.Debug("DispatchAgentStart: env resolution summary",
			"configEnvCount", configEnvCount,
			"storageEnvCount", len(envFromStorage),
			"totalResolvedEnv", len(resolvedEnv),
		)
	}

	// Use agent name as identifier (runtime broker uses name or ID)
	// Pass the agent's harness config so the broker starts with the correct harness.
	harnessConfig := ""
	if agent.AppliedConfig != nil {
		harnessConfig = agent.AppliedConfig.HarnessConfig
	}

	// Thread through updated InlineConfig so the broker can apply config
	// changes (e.g. max_turns) made after initial provisioning.
	var inlineConfig *api.ScionConfig
	if agent.AppliedConfig != nil {
		inlineConfig = agent.AppliedConfig.InlineConfig
	}

	resp, err := d.client.StartAgent(ctx, agent.RuntimeBrokerID, endpoint, agent.Slug, agent.ProjectID, task, projectPath, projectSlug, harnessConfig, resolvedEnv, resolvedSecrets, inlineConfig, projectInfo.sharedDirs, projectInfo.sharedWorkspace, resume)
	if isHashMismatchError(err) {
		if repairErr := d.repairHashMismatch(ctx, agent, err); repairErr == nil {
			resp, err = d.client.StartAgent(ctx, agent.RuntimeBrokerID, endpoint, agent.Slug, agent.ProjectID, task, projectPath, projectSlug, harnessConfig, resolvedEnv, resolvedSecrets, inlineConfig, projectInfo.sharedDirs, projectInfo.sharedWorkspace, resume)
		}
	}
	if errors.Is(err, ErrLifecycleDeferred) {
		return d.deferredStart(ctx, agent, &StartDispatchArgs{
			Task:   task,
			Resume: resume,
		})
	}
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	if resp != nil {
		d.applyBrokerResponse(agent, resp)
	}
	return nil
}

// DispatchAgentStop stops an agent on the runtime broker.
func (d *HTTPAgentDispatcher) DispatchAgentStop(ctx context.Context, agent *store.Agent) error {
	if err := requireRuntimeBrokerAssigned(agent); err != nil {
		return err
	}

	endpoint, err := d.getBrokerEndpoint(ctx, agent.RuntimeBrokerID)
	if err != nil {
		return err
	}

	err = d.client.StopAgent(ctx, agent.RuntimeBrokerID, endpoint, agent.Slug, agent.ProjectID)
	if errors.Is(err, ErrLifecycleDeferred) {
		return d.deferredStop(ctx, agent)
	}
	return err
}

// DispatchAgentRestart restarts an agent on the runtime broker.
// It generates a fresh auth token so the restarted container has valid
// Hub credentials, preventing auth loss across container restarts.
func (d *HTTPAgentDispatcher) DispatchAgentRestart(ctx context.Context, agent *store.Agent) error {
	if err := requireRuntimeBrokerAssigned(agent); err != nil {
		return err
	}

	endpoint, err := d.getBrokerEndpoint(ctx, agent.RuntimeBrokerID)
	if err != nil {
		return err
	}

	// Build resolved env with all env vars, secrets, and a fresh auth token
	// so the restarted container has full credentials and Hub connectivity.
	// This mirrors the resolution in DispatchAgentStart — without it, env vars
	// like GOOGLE_CLOUD_PROJECT are missing and auth provisioning fails.
	resolvedEnv := make(map[string]string)

	// Start with agent's applied config env (template/config-level vars) —
	// same as DispatchAgentStart.
	if agent.AppliedConfig != nil {
		for k, v := range agent.AppliedConfig.Env {
			resolvedEnv[k] = v
		}
	}

	injectModelEnv(resolvedEnv, agent.AppliedConfig)
	injectThinkingLevelEnv(resolvedEnv, agent.AppliedConfig)

	// Merge env vars from Hub storage; storage vars fill in keys not already
	// set (with a non-empty value) — same precedence as DispatchAgentStart.
	envFromStorage, err := d.resolveEnvFromStorage(ctx, agent)
	if err != nil {
		if d.debug {
			d.log.Warn("DispatchAgentRestart: failed to resolve env from storage", "error", err)
		}
	} else if len(envFromStorage) > 0 {
		for k, v := range envFromStorage {
			if existing, exists := resolvedEnv[k]; !exists || existing == "" {
				resolvedEnv[k] = v
			}
		}
	}

	// Resolve type-aware secrets and inject environment-type secrets —
	// same as DispatchAgentStart.
	resolvedSecrets, secretErr := d.resolveSecrets(ctx, agent)
	if secretErr != nil {
		if d.debug {
			d.log.Warn("DispatchAgentRestart: failed to resolve secrets", "error", secretErr)
		}
	} else {
		for _, s := range resolvedSecrets {
			if (s.Type == "environment" || s.Type == "") && s.Target != "" {
				if existing, exists := resolvedEnv[s.Target]; !exists || existing == "" {
					resolvedEnv[s.Target] = s.Value
				}
			}
		}
	}

	// Identity vars at highest precedence (set after storage/secrets merge).
	if agent.ID != "" {
		resolvedEnv["SCION_AGENT_ID"] = agent.ID
	}
	if agent.ProjectID != "" {
		resolvedEnv["SCION_GROVE_ID"] = agent.ProjectID
		resolvedEnv["SCION_PROJECT_ID"] = agent.ProjectID
	}
	if agent.Slug != "" {
		resolvedEnv["SCION_AGENT_SLUG"] = agent.Slug
	}
	if d.hubEndpoint != "" {
		resolvedEnv["SCION_HUB_ENDPOINT"] = d.hubEndpoint
	}
	// Include hub name so agents can label their Cloud Logging entries with
	// the hub identity, matching the hub-scoped log query filter (labels.hub).
	if d.hubName != "" {
		resolvedEnv["SCION_HUB_NAME"] = d.hubName
	}

	// Inject canonical workspace sharing mode and git-ness so the broker can
	// surface them in the container env on the restart path.  Follows the same
	// pattern as DispatchAgentStart (resolve once, switch on canonical constants).
	projectInfo := d.resolveDispatchProjectInfo(ctx, agent)
	resolvedMode := store.ResolveWorkspaceSharingMode(projectInfo.workspaceMode)
	if projectInfo.workspaceMode != "" {
		resolvedEnv["SCION_WORKSPACE_MODE"] = string(resolvedMode)
	}
	switch resolvedMode {
	case store.SharingModeClonePerAgent, store.SharingModeWorktreePerAgent:
		resolvedEnv["SCION_WORKSPACE_GIT"] = "true"
	case store.SharingModeSharedPlain:
		// See DispatchAgentStart for the acknowledged limitation: broker-local
		// linked projects without a GitClone config cannot be detected as
		// git-backed here.
		if agent.AppliedConfig != nil && agent.AppliedConfig.GitClone != nil {
			resolvedEnv["SCION_WORKSPACE_GIT"] = "true"
		}
	}

	// Inject GCP identity env vars so the broker can configure the
	// metadata-server sidecar correctly on restart — same as DispatchAgentStart.
	if agent.AppliedConfig != nil {
		if gcpID := agent.AppliedConfig.GCPIdentity; gcpID != nil {
			resolvedEnv["SCION_METADATA_MODE"] = gcpID.MetadataMode
			if gcpID.MetadataMode == store.GCPMetadataModeAssign {
				resolvedEnv["SCION_METADATA_SA_EMAIL"] = gcpID.ServiceAccountEmail
				resolvedEnv["SCION_METADATA_PROJECT_ID"] = gcpID.ProjectID
			}
		}
	}

	if d.tokenGenerator != nil {
		agentRole, additionalScopes := agentRoleAndScopes(agent)
		token, err := d.tokenGenerator.GenerateAgentToken(agent.ID, agent.ProjectID, agent.Ancestry, agentRole, additionalScopes)
		if err != nil {
			if d.debug {
				d.log.Warn("DispatchAgentRestart: failed to generate agent token", "error", err)
			}
		} else if token != "" {
			resolvedEnv["SCION_AUTH_TOKEN"] = token
		}
	}

	// Transport token minting for platform-layer auth (IAP / Cloud Run invoker)
	if d.transportMinter != nil && d.transportAudience != "" {
		tToken, tExpiry, tErr := d.transportMinter.MintIDToken(ctx, d.transportAudience)
		if tErr != nil {
			if d.debug {
				d.log.Warn("DispatchAgentRestart: failed to mint transport token", "error", tErr)
			}
		} else if tToken != "" {
			resolvedEnv["SCION_TRANSPORT_TOKEN"] = tToken
			resolvedEnv["SCION_TRANSPORT_AUDIENCE"] = d.transportAudience
			resolvedEnv["SCION_TRANSPORT_TOKEN_EXPIRY"] = tExpiry.UTC().Format(time.RFC3339)
			if d.transportMode != "" {
				resolvedEnv["SCION_TRANSPORT_MODE"] = d.transportMode
			}
		}
	}

	// GitHub App token minting for agent restart — same as DispatchAgentStart.
	if d.githubAppMinter != nil && agent.ProjectID != "" {
		project, projectErr := d.store.GetProject(ctx, agent.ProjectID)
		if projectErr == nil {
			mintProject := project
			if project.GitHubInstallationID == nil {
				if sourceProjectID := agent.Labels["scion.dev/github-token-source-project"]; sourceProjectID != "" {
					if sg, sgErr := d.store.GetProject(ctx, sourceProjectID); sgErr == nil && sg.GitHubInstallationID != nil {
						mintProject = sg
					}
				}
			}
			if mintProject.GitHubInstallationID != nil {
				if resolvedEnv["GITHUB_TOKEN"] == "" {
					token, expiry, mintErr := d.githubAppMinter.MintGitHubAppTokenForProject(ctx, mintProject)
					if mintErr != nil {
						if d.debug {
							d.log.Warn("DispatchAgentRestart: GitHub App token minting failed",
								"error", mintErr, "project_id", agent.ProjectID)
						}
					} else if token != "" {
						resolvedEnv["GITHUB_TOKEN"] = token
						resolvedEnv["SCION_GITHUB_APP_ENABLED"] = "true"
						resolvedEnv["SCION_GITHUB_TOKEN_EXPIRY"] = expiry
						resolvedEnv["SCION_GITHUB_TOKEN_PATH"] = "/tmp/.github-token"
					}
				} else {
					d.log.Warn("DispatchAgentRestart: user GITHUB_TOKEN takes precedence over GitHub App token — user token will be used for gh CLI, GitHub App for git credential helper",
						"project_id", agent.ProjectID)
					resolvedEnv["SCION_USER_GITHUB_TOKEN"] = "true"
					resolvedEnv["SCION_GITHUB_APP_ENABLED"] = "true"
				}
			}
		}
	}

	err = d.client.RestartAgent(ctx, agent.RuntimeBrokerID, endpoint, agent.Slug, agent.ProjectID, resolvedEnv)
	if errors.Is(err, ErrLifecycleDeferred) {
		return d.deferredRestart(ctx, agent)
	}
	return err
}

// DispatchAgentResetAuth injects a fresh auth token into a running agent without
// restarting it. It generates a new token and sends it to the broker's reset-auth
// endpoint, which writes it into the container and signals the agent process.
func (d *HTTPAgentDispatcher) DispatchAgentResetAuth(ctx context.Context, agent *store.Agent) error {
	if err := requireRuntimeBrokerAssigned(agent); err != nil {
		return err
	}

	endpoint, err := d.getBrokerEndpoint(ctx, agent.RuntimeBrokerID)
	if err != nil {
		return err
	}

	var token string
	if d.tokenGenerator != nil {
		agentRole, additionalScopes := agentRoleAndScopes(agent)
		token, err = d.tokenGenerator.GenerateAgentToken(agent.ID, agent.ProjectID, agent.Ancestry, agentRole, additionalScopes)
		if err != nil {
			return fmt.Errorf("DispatchAgentResetAuth: failed to generate agent token: %w", err)
		}
	}
	if token == "" {
		return fmt.Errorf("DispatchAgentResetAuth: no token generated for agent %s", agent.ID)
	}

	return d.client.ResetAuthAgent(ctx, agent.RuntimeBrokerID, endpoint, agent.Slug, agent.ProjectID, token)
}

// DispatchAgentDelete deletes an agent from the runtime broker.
func (d *HTTPAgentDispatcher) DispatchAgentDelete(ctx context.Context, agent *store.Agent, deleteFiles, removeBranch, softDelete bool, deletedAt time.Time) error {
	if err := requireRuntimeBrokerAssigned(agent); err != nil {
		return err
	}

	endpoint, err := d.getBrokerEndpoint(ctx, agent.RuntimeBrokerID)
	if err != nil {
		return err
	}

	err = d.client.DeleteAgent(ctx, agent.RuntimeBrokerID, endpoint, agent.Slug, agent.ProjectID, deleteFiles, removeBranch, softDelete, deletedAt)
	if errors.Is(err, ErrLifecycleDeferred) {
		return d.deferredDelete(ctx, agent, deleteFiles, removeBranch, softDelete, deletedAt)
	}
	return err
}

// DispatchAgentMessage sends a message to an agent on the runtime broker.
func (d *HTTPAgentDispatcher) DispatchAgentMessage(ctx context.Context, agent *store.Agent, message string, interrupt bool, structuredMsg *messages.StructuredMessage) error {
	if err := requireRuntimeBrokerAssigned(agent); err != nil {
		return err
	}

	endpoint, err := d.getBrokerEndpoint(ctx, agent.RuntimeBrokerID)
	if err != nil {
		return err
	}

	return d.client.MessageAgent(ctx, agent.RuntimeBrokerID, endpoint, agent.Slug, agent.ProjectID, message, interrupt, structuredMsg)
}

// DispatchAgentLogs retrieves agent.log content from the runtime broker.
func (d *HTTPAgentDispatcher) DispatchAgentLogs(ctx context.Context, agent *store.Agent, tail int) (string, error) {
	if err := requireRuntimeBrokerAssigned(agent); err != nil {
		return "", err
	}

	endpoint, err := d.getBrokerEndpoint(ctx, agent.RuntimeBrokerID)
	if err != nil {
		return "", err
	}

	return d.client.GetAgentLogs(ctx, agent.RuntimeBrokerID, endpoint, agent.Slug, agent.ProjectID, tail)
}

// DispatchAgentExec executes a command in an agent on the runtime broker.
func (d *HTTPAgentDispatcher) DispatchAgentExec(ctx context.Context, agent *store.Agent, command []string, timeout int) (string, int, error) {
	if err := requireRuntimeBrokerAssigned(agent); err != nil {
		return "", 0, err
	}

	endpoint, err := d.getBrokerEndpoint(ctx, agent.RuntimeBrokerID)
	if err != nil {
		return "", 0, err
	}

	return d.client.ExecAgent(ctx, agent.RuntimeBrokerID, endpoint, agent.Slug, agent.ProjectID, command, timeout)
}

// DispatchCheckAgentPrompt checks if an agent has a non-empty prompt.md file.
func (d *HTTPAgentDispatcher) DispatchCheckAgentPrompt(ctx context.Context, agent *store.Agent) (bool, error) {
	if err := requireRuntimeBrokerAssigned(agent); err != nil {
		return false, err
	}

	endpoint, err := d.getBrokerEndpoint(ctx, agent.RuntimeBrokerID)
	if err != nil {
		return false, err
	}

	hasPrompt, err := d.client.CheckAgentPrompt(ctx, agent.RuntimeBrokerID, endpoint, agent.Slug, agent.ProjectID)
	if errors.Is(err, ErrLifecycleDeferred) {
		return d.deferredCheckPrompt(ctx, agent)
	}
	return hasPrompt, err
}

// deferredCheckPrompt handles a cross-node check_prompt via durable dispatch.
func (d *HTTPAgentDispatcher) deferredCheckPrompt(ctx context.Context, agent *store.Agent) (bool, error) {
	result, err := d.deferredDataOpResult(ctx, agent, "check_prompt", &CheckPromptDispatchArgs{})
	if err != nil {
		return false, err
	}
	var cr CheckPromptResult
	if result.Result != "" {
		if err := json.Unmarshal([]byte(result.Result), &cr); err != nil {
			return false, fmt.Errorf("unmarshal check_prompt result: %w", err)
		}
	}
	return cr.HasPrompt, nil
}

// =============================================================================
// injectModelEnv sets SCION_MODEL in env from the agent's applied config model,
// if a model is configured and the key is not already present in env.
// env must be non-nil.
func injectModelEnv(env map[string]string, cfg *store.AgentAppliedConfig) {
	if cfg == nil || cfg.Model == "" {
		return
	}
	if _, ok := env["SCION_MODEL"]; !ok {
		env["SCION_MODEL"] = cfg.Model
	}
}

// injectThinkingLevelEnv sets SCION_THINKING_LEVEL in env from the agent's
// applied config, if a thinking level is configured and the key is not already
// present in env. Mirrors the SCION_MODEL injector above and must be called
// from exactly the same dispatch sites — a site that injects one and not the
// other reproduces the annotation-drop bug on that path only, silently.
// env must be non-nil.
//
// The env var is the terminal hop for thinking level: pkg/agent/run.go reads
// SCION_THINKING_LEVEL from opts.Env under an "if not already set" guard, so a
// hub-supplied value wins and this fix works against already-deployed brokers
// without a wire-field change.
func injectThinkingLevelEnv(env map[string]string, cfg *store.AgentAppliedConfig) {
	if cfg == nil || cfg.ThinkingLevel == nil {
		return
	}
	if _, ok := env["SCION_THINKING_LEVEL"]; !ok {
		env["SCION_THINKING_LEVEL"] = strconv.Itoa(*cfg.ThinkingLevel)
	}
}

// Cross-node lifecycle dispatch (B4-2)
// =============================================================================

// isStartTerminal returns true for terminal phases of a start/restart op.
func isStartTerminal(phase string) bool { return phase == "running" || phase == "error" }

// isStopTerminal returns true for terminal phases of a stop op.
func isStopTerminal(phase string) bool { return phase == "stopped" || phase == "error" }

// deferredStart handles a cross-node agent start: subscribe → write intent →
// signal → wait for the terminal phase. Called when client.StartAgent returns
// ErrLifecycleDeferred (broker not locally connected).
func (d *HTTPAgentDispatcher) deferredStart(ctx context.Context, agent *store.Agent, args *StartDispatchArgs) error {
	return d.deferredLifecycle(ctx, agent, "start", args, isStartTerminal)
}

// deferredStop handles a cross-node agent stop.
func (d *HTTPAgentDispatcher) deferredStop(ctx context.Context, agent *store.Agent) error {
	return d.deferredLifecycle(ctx, agent, "stop", &StopDispatchArgs{}, isStopTerminal)
}

// deferredRestart handles a cross-node agent restart.
func (d *HTTPAgentDispatcher) deferredRestart(ctx context.Context, agent *store.Agent) error {
	return d.deferredLifecycle(ctx, agent, "restart", &RestartDispatchArgs{}, isStartTerminal)
}

// deferredDelete handles a cross-node agent delete: subscribe → write intent →
// signal → wait for the dispatch row to reach terminal state. Delete is
// idempotent: 404 from the owner is treated as success.
func (d *HTTPAgentDispatcher) deferredDelete(ctx context.Context, agent *store.Agent, deleteFiles, removeBranch, softDelete bool, deletedAt time.Time) error {
	args := &DeleteDispatchArgs{
		DeleteFiles:  deleteFiles,
		RemoveBranch: removeBranch,
		SoftDelete:   softDelete,
		DeletedAt:    deletedAt,
	}
	return d.deferredDataOp(ctx, agent, "delete", args)
}

// deferredDataOp is the common flow for cross-node ops that return a result
// via the dispatch row (delete, finalize_env, check_prompt, create):
//  1. Subscribe to broker.dispatch.<id>.done BEFORE writing intent
//  2. InsertBrokerDispatch with serialized args
//  3. Best-effort SignalBrokerCmd
//  4. waitForDispatchDone (reads result from the DB row — authoritative)
func (d *HTTPAgentDispatcher) deferredDataOp(
	ctx context.Context,
	agent *store.Agent,
	op string,
	args interface{},
) error {
	_, err := d.deferredDataOpResult(ctx, agent, op, args)
	return err
}

// deferredDataOpResult is like deferredDataOp but returns the completed
// dispatch row so callers can read the result JSON.
func (d *HTTPAgentDispatcher) deferredDataOpResult(
	ctx context.Context,
	agent *store.Agent,
	op string,
	args interface{},
) (*store.BrokerDispatch, error) {
	if d.events == nil || d.commandBus == nil {
		return nil, fmt.Errorf("cross-node dispatch not available: events or command bus not configured")
	}

	dispatchID := uuid.NewString()

	// 1. Subscribe BEFORE writing intent so we don't miss events.
	eventCh, unsub := d.events.Subscribe("broker.dispatch." + dispatchID + ".done")

	// 2. Serialize args and insert the durable intent row.
	argsJSON, err := MarshalDispatchArgs(args)
	if err != nil {
		unsub()
		return nil, fmt.Errorf("marshal dispatch args: %w", err)
	}

	dispatch := &store.BrokerDispatch{
		ID:        dispatchID,
		BrokerID:  agent.RuntimeBrokerID,
		AgentID:   agent.ID,
		AgentSlug: agent.Slug,
		ProjectID: agent.ProjectID,
		Op:        op,
		Args:      argsJSON,
	}
	if err := d.store.InsertBrokerDispatch(ctx, dispatch); err != nil {
		unsub()
		return nil, fmt.Errorf("insert dispatch intent: %w", err)
	}
	if rec := d.dispatchMetrics; rec != nil {
		rec.IncPublished(ctx, 1, attribute.String("op", op))
	}

	// 3. Best-effort signal.
	if err := d.commandBus.SignalBrokerCmd(ctx, agent.RuntimeBrokerID); err != nil {
		d.log.Warn("deferredDataOp: signal failed (durable intent is backstop)",
			"op", op, "brokerID", agent.RuntimeBrokerID, "error", err)
	}

	// 4. Wait for completion — reads result from the DB row (authoritative).
	result, err := waitForDispatchDone(ctx, eventCh, unsub, d.store, dispatchID)
	if err != nil {
		return nil, err
	}
	if result.State == store.DispatchStateFailed {
		return nil, fmt.Errorf("dispatch %s failed: %s", op, result.Error)
	}
	return result, nil
}

// deferredLifecycle is the common flow for cross-node start/stop/restart:
//  1. Subscribe to agent.<id>.status BEFORE writing intent (no missed events)
//  2. InsertBrokerDispatch with serialized resolved args
//  3. Best-effort SignalBrokerCmd (the row is durable; reconnect-drain backstop)
//  4. waitForAgentTransition with the op's terminal set
//  5. Return nil on success-terminal, ErrDispatchFailed on timeout, wrapped
//     error on error-terminal
func (d *HTTPAgentDispatcher) deferredLifecycle(
	ctx context.Context,
	agent *store.Agent,
	op string,
	args interface{},
	terminal func(string) bool,
) error {
	if d.events == nil || d.commandBus == nil {
		return fmt.Errorf("cross-node dispatch not available: events or command bus not configured")
	}

	// 1. Subscribe BEFORE writing intent so we don't miss events.
	eventCh, unsub := d.events.Subscribe("agent." + agent.ID + ".status")

	// 2. Serialize args and insert the durable intent row.
	argsJSON, err := MarshalDispatchArgs(args)
	if err != nil {
		unsub()
		return fmt.Errorf("marshal dispatch args: %w", err)
	}

	dispatch := &store.BrokerDispatch{
		ID:        uuid.NewString(),
		BrokerID:  agent.RuntimeBrokerID,
		AgentID:   agent.ID,
		AgentSlug: agent.Slug,
		ProjectID: agent.ProjectID,
		Op:        op,
		Args:      argsJSON,
	}
	if err := d.store.InsertBrokerDispatch(ctx, dispatch); err != nil {
		unsub()
		return fmt.Errorf("insert dispatch intent: %w", err)
	}
	if rec := d.dispatchMetrics; rec != nil {
		rec.IncPublished(ctx, 1, attribute.String("op", op))
	}

	// 3. Best-effort signal — the row is the durable intent; reconnect-drain
	//    is the backstop if the signal is missed or no node owns the broker.
	if err := d.commandBus.SignalBrokerCmd(ctx, agent.RuntimeBrokerID); err != nil {
		d.log.Warn("deferredLifecycle: signal failed (durable intent is backstop)",
			"op", op, "brokerID", agent.RuntimeBrokerID, "error", err)
	}

	// 4. Wait for terminal phase.
	phase, err := waitForAgentTransition(ctx, eventCh, unsub, terminal)
	if err != nil {
		return err
	}

	// 5. Map terminal phase.
	if phase == "error" {
		return fmt.Errorf("agent entered error phase during %s", op)
	}
	return nil
}

// resolveSecrets queries secrets from all applicable scopes and merges them
// into a flat list. Higher scopes override lower:
//
//	runtime_broker  <  hub  <  project  <  user
//
// This matches envScopePrecedence (see above). The divergence previously
// tracked in issue #624 was corrected in PR #1227.
func (d *HTTPAgentDispatcher) resolveSecrets(ctx context.Context, agent *store.Agent) ([]ResolvedSecret, error) {
	if d.secretBackend == nil {
		if d.debug {
			d.log.Debug("resolveSecrets: secretBackend is nil, skipping secret resolution")
		}
		return nil, nil
	}
	if d.debug {
		d.log.Debug("resolveSecrets: querying secret backend",
			"ownerID", agent.OwnerID,
			"project_id", agent.ProjectID,
			"brokerID", agent.RuntimeBrokerID,
		)
	}
	// Build resolve options: include agent ancestry for progeny secret resolution
	// when the creating principal is an agent (ancestry has more than one entry,
	// meaning the agent was created by another agent, not directly by the user).
	var resolveOpts *secret.ResolveOpts
	if len(agent.Ancestry) > 1 && d.authzService != nil {
		agentID := agent.ID
		ancestry := agent.Ancestry
		resolveOpts = &secret.ResolveOpts{
			AgentAncestry: ancestry,
			AuthzCheck: func(s secret.SecretMeta) bool {
				decision := d.authzService.CheckAccess(ctx, &agentIdentityWrapper{
					AgentTokenClaims: &AgentTokenClaims{
						Claims:    jwt.Claims{Subject: agentID},
						ProjectID: agent.ProjectID,
						Ancestry:  ancestry,
					},
				}, Resource{
					Type: "secret",
					ID:   s.ID,
				}, ActionRead)
				return decision.Allowed
			},
		}
	}

	resolved, err := d.secretBackend.Resolve(ctx, agent.OwnerID, agent.ProjectID, agent.RuntimeBrokerID, resolveOpts)
	if err != nil {
		return nil, err
	}
	result := make([]ResolvedSecret, 0, len(resolved))
	for _, sv := range resolved {
		// Only skip as_needed environment-type secrets (handled by the
		// two-pass env-gather flow). File-type and variable-type secrets
		// should always be placed regardless of injection mode — the
		// as_needed concept does not apply to them.
		if sv.InjectionMode == store.InjectionModeAsNeeded && (sv.SecretType == store.SecretTypeEnvironment || sv.SecretType == "") {
			continue
		}
		result = append(result, ResolvedSecret{
			Name:   sv.Name,
			Type:   sv.SecretType,
			Target: sv.Target,
			Value:  sv.Value,
			Source: sv.Scope,
			Ref:    sv.SecretRef,
		})
	}
	if d.debug {
		names := make([]string, len(result))
		for i, r := range result {
			names[i] = r.Name
		}
		d.log.Debug("resolveSecrets: resolved secrets", "count", len(result), "names", names)
	}
	return result, nil
}
