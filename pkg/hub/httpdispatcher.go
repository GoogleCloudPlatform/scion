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
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
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

func (c *HTTPRuntimeBrokerClient) StartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID, task, projectPath, projectSlug, harnessConfig string, resolvedEnv map[string]string, resolvedSecrets []ResolvedSecret, inlineConfig *api.ScionConfig, sharedDirs []api.SharedDir, sharedWorkspace bool) (*RemoteAgentResponse, error) {
	return c.transport.StartAgent(ctx, brokerID, brokerEndpoint, agentID, projectID, task, projectPath, projectSlug, harnessConfig, resolvedEnv, resolvedSecrets, inlineConfig, sharedDirs, sharedWorkspace)
}

func (c *HTTPRuntimeBrokerClient) StopAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string) error {
	return c.transport.StopAgent(ctx, brokerID, brokerEndpoint, agentID, projectID)
}

func (c *HTTPRuntimeBrokerClient) RestartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string, resolvedEnv map[string]string) error {
	return c.transport.RestartAgent(ctx, brokerID, brokerEndpoint, agentID, projectID, resolvedEnv)
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

func (c *HTTPRuntimeBrokerClient) FinalizeEnv(ctx context.Context, brokerID, brokerEndpoint, agentID string, env map[string]string) (*RemoteAgentResponse, error) {
	return c.transport.FinalizeEnv(ctx, brokerID, brokerEndpoint, agentID, env)
}

func (c *HTTPRuntimeBrokerClient) GetAgentLogs(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string, tail int) (string, error) {
	return c.transport.GetAgentLogs(ctx, brokerID, brokerEndpoint, agentID, projectID, tail)
}

func (c *HTTPRuntimeBrokerClient) ExecAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string, command []string, timeout int) (string, int, error) {
	return c.transport.ExecAgent(ctx, brokerID, brokerEndpoint, agentID, projectID, command, timeout)
}

func (c *HTTPRuntimeBrokerClient) CleanupProject(ctx context.Context, brokerID, brokerEndpoint, projectSlug string) error {
	return c.transport.CleanupProject(ctx, brokerID, brokerEndpoint, projectSlug)
}

// GetClient returns the underlying RuntimeBrokerClient.
func (d *HTTPAgentDispatcher) GetClient() RuntimeBrokerClient {
	return d.client
}

// AgentTokenGenerator generates JWT tokens for agents.
type AgentTokenGenerator interface {
	GenerateAgentToken(agentID, projectID string, ancestry []string, additionalScopes ...AgentTokenScope) (string, error)
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
	store           store.Store
	client          RuntimeBrokerClient
	tokenGenerator  AgentTokenGenerator
	secretBackend   secret.SecretBackend
	authzService    *AuthzService        // Optional authz service for progeny secret verification
	githubAppMinter GitHubAppTokenMinter // Optional GitHub App token minter
	hubEndpoint     string               // Hub endpoint URL for agents to call back
	hubID           string               // Hub instance ID for hub-scoped queries
	devAuthToken    string               // Dev auth token to inject into agent env (dev-auth mode only)
	debug           bool
	log             *slog.Logger
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

// SetHubEndpoint sets the Hub endpoint URL that agents will use to call back.
func (d *HTTPAgentDispatcher) SetHubEndpoint(endpoint string) {
	d.hubEndpoint = endpoint
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

// SetGitHubAppMinter sets the GitHub App token minter for resolving
// GitHub App installation tokens during agent credential resolution.
func (d *HTTPAgentDispatcher) SetGitHubAppMinter(m GitHubAppTokenMinter) {
	d.githubAppMinter = m
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
		RequestID:   api.NewUUID(),
		ID:          agent.ID,
		Slug:        agent.Slug,
		Name:        agent.Name,
		ProjectID:     agent.ProjectID,
		UserID:      agent.OwnerID,
		HubEndpoint: d.hubEndpoint,
		ProjectPath: projectInfo.projectPath,
		ProjectSlug: projectInfo.projectSlug,
		SharedDirs:  projectInfo.sharedDirs,
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
		// Convert hub access scopes from AppliedConfig to AgentTokenScope
		var additionalScopes []AgentTokenScope
		if agent.AppliedConfig != nil {
			for _, s := range agent.AppliedConfig.HubAccessScopes {
				additionalScopes = append(additionalScopes, AgentTokenScope(s))
			}
			// Inject GCP token scope when the agent has an assigned service account
			if gcpID := agent.AppliedConfig.GCPIdentity; gcpID != nil && gcpID.MetadataMode == store.GCPMetadataModeAssign && gcpID.ServiceAccountID != "" {
				additionalScopes = append(additionalScopes, GCPTokenScopeForSA(gcpID.ServiceAccountID))
			}
		}
		token, err := d.tokenGenerator.GenerateAgentToken(agent.ID, agent.ProjectID, agent.Ancestry, additionalScopes...)
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
			workspace = ""
		}
		var remoteGCPIdentity *RemoteGCPIdentityConfig
		if gcpID := agent.AppliedConfig.GCPIdentity; gcpID != nil {
			remoteGCPIdentity = &RemoteGCPIdentityConfig{
				MetadataMode: gcpID.MetadataMode,
				SAEmail:      gcpID.ServiceAccountEmail,
				ProjectID:    gcpID.ProjectID,
			}
		}
		req.Config = &RemoteAgentConfig{
			Template:        agent.Template,
			Image:           agent.AppliedConfig.Image,
			HarnessConfig:   agent.AppliedConfig.HarnessConfig,
			HarnessAuth:     agent.AppliedConfig.HarnessAuth,
			Task:            agent.AppliedConfig.Task,
			Workspace:       workspace,
			Profile:         agent.AppliedConfig.Profile,
			Branch:          agent.AppliedConfig.Branch,
			TemplateID:      agent.AppliedConfig.TemplateID,
			TemplateHash:    agent.AppliedConfig.TemplateHash,
			GitClone:        gitClone,
			SharedWorkspace: projectInfo.sharedWorkspace,
			GCPIdentity:     remoteGCPIdentity,
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

	// Resolve type-aware secrets from all applicable scopes
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

	// Log a summary of env resolution sources
	if d.debug {
		configEnvCount := 0
		if agent.AppliedConfig != nil {
			configEnvCount = len(agent.AppliedConfig.Env)
		}
		d.log.Debug("buildCreateRequest: env resolution summary",
			"configEnvCount", configEnvCount,
			"storageEnvCount", len(envFromStorage),
			"resolvedSecretsCount", len(resolvedSecrets),
			"totalResolvedEnvCount", len(req.ResolvedEnv),
		)
	}

	// In dev-auth mode, inject the dev token so agents can use it as fallback auth
	if d.devAuthToken != "" {
		if req.ResolvedEnv == nil {
			req.ResolvedEnv = make(map[string]string)
		}
		req.ResolvedEnv["SCION_DEV_TOKEN"] = d.devAuthToken
	}

	return req, nil
}

// projectDispatchInfo contains resolved project information for dispatching agent requests.
type projectDispatchInfo struct {
	projectPath     string
	projectSlug     string
	sharedDirs      []api.SharedDir
	sharedWorkspace bool // true for git-workspace hybrid projects
}

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
	if err := requireRuntimeBrokerAssigned(agent); err != nil {
		return err
	}

	endpoint, err := d.getBrokerEndpoint(ctx, agent.RuntimeBrokerID)
	if err != nil {
		return err
	}

	req, err := d.buildCreateRequest(ctx, agent, "DispatchAgentCreate")
	if err != nil {
		return err
	}

	resp, err := d.client.CreateAgent(ctx, agent.RuntimeBrokerID, endpoint, req)
	if err != nil {
		return err
	}

	d.applyBrokerResponse(agent, resp)
	return nil
}

// DispatchAgentProvision provisions an agent on the runtime broker without starting it.
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

	// Merge resolved storage env vars back into AppliedConfig so they are
	// visible in the advanced config form. Exclude internal SCION_* vars
	// and dev tokens which are injected at start time.
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

	resp, err := d.client.CreateAgent(ctx, agent.RuntimeBrokerID, endpoint, req)
	if err != nil {
		return err
	}

	d.applyBrokerResponse(agent, resp)
	return nil
}

// DispatchAgentCreateWithGather creates an agent with env-gather support.
// If the broker returns 202 with env requirements, it returns the requirements
// as the first value instead of an error.
func (d *HTTPAgentDispatcher) DispatchAgentCreateWithGather(ctx context.Context, agent *store.Agent) (*RemoteEnvRequirementsResponse, error) {
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

	resp, envReqs, err := d.client.CreateAgentWithGather(ctx, agent.RuntimeBrokerID, endpoint, req)
	if err != nil {
		return nil, err
	}

	if envReqs != nil {
		return envReqs, nil
	}

	if resp != nil {
		d.applyBrokerResponse(agent, resp)
	}
	return nil, nil
}

// DispatchFinalizeEnv sends gathered env vars to the broker to complete agent creation.
func (d *HTTPAgentDispatcher) DispatchFinalizeEnv(ctx context.Context, agent *store.Agent, env map[string]string) error {
	if err := requireRuntimeBrokerAssigned(agent); err != nil {
		return err
	}

	endpoint, err := d.getBrokerEndpoint(ctx, agent.RuntimeBrokerID)
	if err != nil {
		return err
	}

	resp, err := d.client.FinalizeEnv(ctx, agent.RuntimeBrokerID, endpoint, agent.ID, env)
	if err != nil {
		return err
	}

	if resp != nil {
		d.applyBrokerResponse(agent, resp)
	}
	return nil
}

// resolveEnvFromStorage queries Hub env var storage for all applicable scopes
// and returns a merged map with precedence: user > project > global.
func (d *HTTPAgentDispatcher) resolveEnvFromStorage(ctx context.Context, agent *store.Agent) (map[string]string, error) {
	result := make(map[string]string)

	// Query hub-scoped env vars (lowest precedence)
	vars, err := d.store.ListEnvVars(ctx, store.EnvVarFilter{Scope: store.ScopeHub, ScopeID: d.hubID})
	if err != nil {
		if d.debug {
			d.log.Warn("Failed to list hub env vars", "error", err)
		}
	} else {
		if d.debug {
			keys := make([]string, 0, len(vars))
			for _, v := range vars {
				keys = append(keys, v.Key)
			}
			d.log.Debug("resolveEnvFromStorage: hub scope", "count", len(vars), "keys", keys)
		}
		for _, v := range vars {
			result[v.Key] = v.Value
		}
	}

	// Query project-scoped env vars
	if agent.ProjectID != "" {
		vars, err := d.store.ListEnvVars(ctx, store.EnvVarFilter{Scope: "project", ScopeID: agent.ProjectID})
		if err != nil {
			if d.debug {
				d.log.Warn("Failed to list project env vars", "error", err)
			}
		} else {
			if d.debug {
				keys := make([]string, 0, len(vars))
				for _, v := range vars {
					keys = append(keys, v.Key)
				}
				d.log.Debug("resolveEnvFromStorage: project scope", "project_id", agent.ProjectID, "count", len(vars), "keys", keys)
			}
			for _, v := range vars {
				result[v.Key] = v.Value
			}
		}
	} else if d.debug {
		d.log.Debug("resolveEnvFromStorage: skipping project scope (empty projectID)")
	}

	// Query user-scoped env vars (higher precedence)
	if agent.OwnerID != "" {
		vars, err := d.store.ListEnvVars(ctx, store.EnvVarFilter{Scope: "user", ScopeID: agent.OwnerID})
		if err != nil {
			if d.debug {
				d.log.Warn("Failed to list user env vars", "error", err)
			}
		} else {
			if d.debug {
				keys := make([]string, 0, len(vars))
				for _, v := range vars {
					keys = append(keys, v.Key)
				}
				d.log.Debug("resolveEnvFromStorage: user scope", "ownerID", agent.OwnerID, "count", len(vars), "keys", keys)
			}
			for _, v := range vars {
				result[v.Key] = v.Value
			}
		}
	} else if d.debug {
		d.log.Debug("resolveEnvFromStorage: skipping user scope (empty ownerID)")
	}

	// Query runtime_broker-scoped env vars (if applicable)
	if agent.RuntimeBrokerID != "" {
		vars, err := d.store.ListEnvVars(ctx, store.EnvVarFilter{Scope: "runtime_broker", ScopeID: agent.RuntimeBrokerID})
		if err != nil {
			if d.debug {
				d.log.Warn("Failed to list broker env vars", "error", err)
			}
		} else {
			if d.debug {
				keys := make([]string, 0, len(vars))
				for _, v := range vars {
					keys = append(keys, v.Key)
				}
				d.log.Debug("resolveEnvFromStorage: broker scope", "brokerID", agent.RuntimeBrokerID, "count", len(vars), "keys", keys)
			}
			for _, v := range vars {
				result[v.Key] = v.Value
			}
		}
	} else if d.debug {
		d.log.Debug("resolveEnvFromStorage: skipping broker scope (empty brokerID)")
	}

	return result, nil
}

// buildEnvSources creates a map of env key -> scope for reporting to the CLI.
func (d *HTTPAgentDispatcher) buildEnvSources(ctx context.Context, agent *store.Agent, resolvedEnv map[string]string) map[string]string {
	sources := make(map[string]string)

	// Check hub scope (lowest precedence — later scopes override)
	vars, err := d.store.ListEnvVars(ctx, store.EnvVarFilter{Scope: store.ScopeHub, ScopeID: d.hubID})
	if err == nil {
		for _, v := range vars {
			if _, inResolved := resolvedEnv[v.Key]; inResolved {
				sources[v.Key] = "hub"
			}
		}
	}

	// Check project scope
	if agent.ProjectID != "" {
		vars, err := d.store.ListEnvVars(ctx, store.EnvVarFilter{Scope: "project", ScopeID: agent.ProjectID})
		if err == nil {
			for _, v := range vars {
				if _, inResolved := resolvedEnv[v.Key]; inResolved {
					sources[v.Key] = "project"
				}
			}
		}
	}

	// Check user scope (overrides project)
	if agent.OwnerID != "" {
		vars, err := d.store.ListEnvVars(ctx, store.EnvVarFilter{Scope: "user", ScopeID: agent.OwnerID})
		if err == nil {
			for _, v := range vars {
				if _, inResolved := resolvedEnv[v.Key]; inResolved {
					sources[v.Key] = "user"
				}
			}
		}
	}

	// Check config scope
	if agent.AppliedConfig != nil {
		for k := range agent.AppliedConfig.Env {
			if _, inResolved := resolvedEnv[k]; inResolved {
				sources[k] = "config"
			}
		}
	}

	return sources
}

// DispatchAgentStart starts an agent on the runtime broker.
func (d *HTTPAgentDispatcher) DispatchAgentStart(ctx context.Context, agent *store.Agent, task string) error {
	if err := requireRuntimeBrokerAssigned(agent); err != nil {
		return err
	}

	endpoint, err := d.getBrokerEndpoint(ctx, agent.RuntimeBrokerID)
	if err != nil {
		return err
	}

	// If no explicit task provided, fall back to the agent's applied config task
	if task == "" && agent.AppliedConfig != nil {
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
		var additionalScopes []AgentTokenScope
		if agent.AppliedConfig != nil {
			for _, s := range agent.AppliedConfig.HubAccessScopes {
				additionalScopes = append(additionalScopes, AgentTokenScope(s))
			}
			// Inject GCP token scope when the agent has an assigned service account
			if gcpID := agent.AppliedConfig.GCPIdentity; gcpID != nil && gcpID.MetadataMode == store.GCPMetadataModeAssign && gcpID.ServiceAccountID != "" {
				additionalScopes = append(additionalScopes, GCPTokenScopeForSA(gcpID.ServiceAccountID))
			}
		}
		token, err := d.tokenGenerator.GenerateAgentToken(agent.ID, agent.ProjectID, agent.Ancestry, additionalScopes...)
		if err != nil {
			if d.debug {
				d.log.Warn("DispatchAgentStart: failed to generate agent token", "error", err)
			}
		} else if token != "" {
			resolvedEnv["SCION_AUTH_TOKEN"] = token
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

	resp, err := d.client.StartAgent(ctx, agent.RuntimeBrokerID, endpoint, agent.Slug, agent.ProjectID, task, projectPath, projectSlug, harnessConfig, resolvedEnv, resolvedSecrets, inlineConfig, projectInfo.sharedDirs, projectInfo.sharedWorkspace)
	if err != nil {
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

	return d.client.StopAgent(ctx, agent.RuntimeBrokerID, endpoint, agent.Slug, agent.ProjectID)
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

	// Build resolved env with fresh auth token and identity vars so the
	// restarted container retains Hub connectivity. Without this, the
	// broker's restartAgent handler has no token to inject.
	resolvedEnv := make(map[string]string)
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

	if d.tokenGenerator != nil {
		var additionalScopes []AgentTokenScope
		if agent.AppliedConfig != nil {
			for _, s := range agent.AppliedConfig.HubAccessScopes {
				additionalScopes = append(additionalScopes, AgentTokenScope(s))
			}
			if gcpID := agent.AppliedConfig.GCPIdentity; gcpID != nil && gcpID.MetadataMode == store.GCPMetadataModeAssign && gcpID.ServiceAccountID != "" {
				additionalScopes = append(additionalScopes, GCPTokenScopeForSA(gcpID.ServiceAccountID))
			}
		}
		token, err := d.tokenGenerator.GenerateAgentToken(agent.ID, agent.ProjectID, agent.Ancestry, additionalScopes...)
		if err != nil {
			if d.debug {
				d.log.Warn("DispatchAgentRestart: failed to generate agent token", "error", err)
			}
		} else if token != "" {
			resolvedEnv["SCION_AUTH_TOKEN"] = token
		}
	}

	return d.client.RestartAgent(ctx, agent.RuntimeBrokerID, endpoint, agent.Slug, agent.ProjectID, resolvedEnv)
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

	return d.client.DeleteAgent(ctx, agent.RuntimeBrokerID, endpoint, agent.Slug, agent.ProjectID, deleteFiles, removeBranch, softDelete, deletedAt)
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

	return d.client.CheckAgentPrompt(ctx, agent.RuntimeBrokerID, endpoint, agent.Slug, agent.ProjectID)
}

// resolveSecrets queries secrets from all applicable scopes and merges them
// into a flat list. Higher scopes override lower: user < project < runtime_broker.
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
						Claims:   jwt.Claims{Subject: agentID},
						ProjectID:  agent.ProjectID,
						Ancestry: ancestry,
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
	result := make([]ResolvedSecret, len(resolved))
	for i, sv := range resolved {
		result[i] = ResolvedSecret{
			Name:   sv.Name,
			Type:   sv.SecretType,
			Target: sv.Target,
			Value:  sv.Value,
			Source: sv.Scope,
			Ref:    sv.SecretRef,
		}
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
