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
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentkeys"
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

// permissionProjectSecretRead is the registry permission ID that governs an
// agent reading secrets during resolution (pkg/hub/permissions/registry.go).
// It is declared on ResourceProject, so it cannot be derived from a
// resource type of "secret".
const permissionProjectSecretRead = "project.secret_read"

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

func (c *HTTPRuntimeBrokerClient) StartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID, task, projectPath, projectSlug, harnessConfig, harnessConfigID, harnessConfigHash string, resolvedEnv map[string]string, resolvedSecrets []ResolvedSecret, inlineConfig *api.ScionConfig, sharedDirs []api.SharedDir, sharedWorkspace, resume bool, extras StartExtras) (*RemoteAgentResponse, error) {
	return c.transport.StartAgent(ctx, brokerID, brokerEndpoint, agentID, projectID, task, projectPath, projectSlug, harnessConfig, harnessConfigID, harnessConfigHash, resolvedEnv, resolvedSecrets, inlineConfig, sharedDirs, sharedWorkspace, resume, extras)
}

func (c *HTTPRuntimeBrokerClient) StopAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string) error {
	return c.transport.StopAgent(ctx, brokerID, brokerEndpoint, agentID, projectID)
}

func (c *HTTPRuntimeBrokerClient) RestartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string, resolvedEnv map[string]string, extras StartExtras) error {
	return c.transport.RestartAgent(ctx, brokerID, brokerEndpoint, agentID, projectID, resolvedEnv, extras)
}

func (c *HTTPRuntimeBrokerClient) ResetAuthAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID, token, transportToken string) error {
	return c.transport.ResetAuthAgent(ctx, brokerID, brokerEndpoint, agentID, projectID, token, transportToken)
}

func (c *HTTPRuntimeBrokerClient) DeleteAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string, deleteFiles, removeBranch, softDelete bool, deletedAt time.Time) error {
	return c.transport.DeleteAgent(ctx, brokerID, brokerEndpoint, agentID, projectID, deleteFiles, removeBranch, softDelete, deletedAt)
}

func (c *HTTPRuntimeBrokerClient) MessageAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID, message string, interrupt bool, structuredMsg *messages.StructuredMessage) error {
	return c.transport.MessageAgent(ctx, brokerID, brokerEndpoint, agentID, projectID, message, interrupt, structuredMsg)
}

// ExecuteKeys dispatches a typed keys request to a runtime broker's dedicated
// keys route directly over HTTP (no HMAC signing). It implements
// agentkeys.BrokerClient.
func (c *HTTPRuntimeBrokerClient) ExecuteKeys(ctx context.Context, brokerID, brokerEndpoint, agentSlug string, req agentkeys.BrokerRequest) (agentkeys.BrokerResult, error) {
	return c.transport.ExecuteKeys(ctx, brokerID, brokerEndpoint, agentSlug, req)
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
	// GenerateAgentTokenForAgent issues a token for the stored agent record,
	// bounded by its delegation chain. Every dispatcher mint site uses it.
	GenerateAgentTokenForAgent(ctx context.Context, agent *store.Agent) (string, error)
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
	agentEndpoint     string               // Optional override of hubEndpoint for SCION_HUB_ENDPOINT only
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
	skillPreResolver      func(ctx context.Context, agent *store.Agent) *ResolveSkillsResponse

	// creatorSkillPreResolver is skillPreResolver's start/restart counterpart:
	// it resolves as the agent's recorded creator regardless of who is
	// dispatching the start/restart, so a re-provision reached through either
	// verb resolves the same set for a given agent no matter which permitted
	// principal triggers it (ptone/scion#1994). Nil = no creator-based
	// resolution (start/restart carry no PreResolvedSkills, same as before).
	creatorSkillPreResolver func(ctx context.Context, agent *store.Agent) *ResolveSkillsResponse

	// hubAgentDefaultsProvider returns the hub's operational agent_defaults at
	// dispatch time. A callback rather than a snapshot because the settings
	// propagation goroutine rewrites them while the hub runs; the Server's
	// accessor reads under its lock. Nil = no hub defaults (local dispatcher,
	// tests) and the wire field is omitted.
	hubAgentDefaultsProvider func() opsettings.AgentDefaultsSettings

	// asyncLaunchSettings returns the async agent launch settings. Nil =
	// every create is synchronous (tests, local dispatcher).
	asyncLaunchSettings func() AsyncLaunchSettings

	// autoExposePortsDefaultProvider returns the hub's auto-expose-ports
	// default (nil = unset) at dispatch time, for the broker's lowest env
	// tier. A callback for the same reason as hubAgentDefaultsProvider. Nil
	// provider = no default sent.
	autoExposePortsDefaultProvider func() *bool
}

// NewHTTPAgentDispatcher creates a new HTTP-based agent dispatcher.
func NewHTTPAgentDispatcher(s store.Store, debug bool, log *slog.Logger) *HTTPAgentDispatcher {
	if log == nil {
		log = slog.Default()
	}
	return &HTTPAgentDispatcher{
		store:  s,
		client: NewHTTPRuntimeBrokerClientWithDebug(debug),
		debug:  debug,
		log:    log,
	}
}

// NewHTTPAgentDispatcherWithClient creates a new HTTP-based agent dispatcher with a custom client.
func NewHTTPAgentDispatcherWithClient(s store.Store, client RuntimeBrokerClient, debug bool, log *slog.Logger) *HTTPAgentDispatcher {
	if log == nil {
		log = slog.Default()
	}
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
		return AgentRoleNone, nil
	}
	// Missing role data is treated as least-privileged. Existing pre-role
	// agents are backfilled to an explicit full role during store migration.
	role := AgentRoleNone
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

// SetAgentEndpoint sets an optional override of the Hub endpoint used only
// for the SCION_HUB_ENDPOINT value injected into dispatched agents. It takes
// precedence over the value set by SetHubEndpoint at every agent-injection
// site (create, start, restart), but has no effect on anything else the
// dispatcher or the wider Hub does with the regular hub endpoint. An empty
// value (the default) leaves injection using the value set by SetHubEndpoint.
func (d *HTTPAgentDispatcher) SetAgentEndpoint(endpoint string) {
	d.agentEndpoint = endpoint
}

// effectiveAgentHubEndpoint returns the Hub endpoint value to stamp into an
// agent's SCION_HUB_ENDPOINT: the agent-endpoint override when configured,
// otherwise the regular hub endpoint.
func (d *HTTPAgentDispatcher) effectiveAgentHubEndpoint() string {
	if d.agentEndpoint != "" {
		return d.agentEndpoint
	}
	return d.hubEndpoint
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

// SetAutoExposePortsDefaultProvider registers the accessor for the hub's
// auto-expose-ports default, read on every create, start and restart dispatch
// so a settings change reaches an agent at its next start.
func (d *HTTPAgentDispatcher) SetAutoExposePortsDefaultProvider(fn func() *bool) {
	d.autoExposePortsDefaultProvider = fn
}

// autoExposePortsDefault returns the hub auto-expose default, or nil when no
// provider is registered or the hub has none.
func (d *HTTPAgentDispatcher) autoExposePortsDefault() *bool {
	if d.autoExposePortsDefaultProvider == nil {
		return nil
	}
	return d.autoExposePortsDefaultProvider()
}

// SetImageRegistry sets the image registry prefix for rewriting bare image
// names before dispatching to brokers.
func (d *HTTPAgentDispatcher) SetImageRegistry(registry string) {
	d.imageRegistry = registry
}

// ImageRegistry returns the registry prefix this dispatcher rewrites bare
// image names to at send time ("" = no rewrite).
func (d *HTTPAgentDispatcher) ImageRegistry() string {
	return d.imageRegistry
}

// SetTemplateRepairer registers a callback that syncs a template's DB manifest
// from storage when a hash mismatch is detected during dispatch.
func (d *HTTPAgentDispatcher) SetTemplateRepairer(fn func(ctx context.Context, ref string) error) {
	d.templateRepairer = fn
}

// SetSkillPreResolver registers the callback that resolves an agent's
// Hub-registry skill references as its creator at dispatch (#1784). The result
// is attached to every create request as PreResolvedSkills.
func (d *HTTPAgentDispatcher) SetSkillPreResolver(fn func(ctx context.Context, agent *store.Agent) *ResolveSkillsResponse) {
	d.skillPreResolver = fn
}

// SetCreatorSkillPreResolver registers the callback that resolves an agent's
// Hub-registry skill references as its recorded creator, independent of the
// principal dispatching the current call (ptone/scion#1994). Start and
// restart use this so their PreResolvedSkills matches what the agent's
// creator would get, regardless of who starts/restarts the agent.
func (d *HTTPAgentDispatcher) SetCreatorSkillPreResolver(fn func(ctx context.Context, agent *store.Agent) *ResolveSkillsResponse) {
	d.creatorSkillPreResolver = fn
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

// getProvisioningBrokerEndpoint is getBrokerEndpoint for dispatches that
// (re-)provision or start an agent (create, provision, finalize-env, start,
// restart). It additionally fails closed when the agent's project is
// empty-per-agent and the broker does not advertise the
// emptyPerAgentWorkspace capability (design #2703 D3), so an old broker never
// receives the mode. A project lookup error also fails closed; only a
// missing project skips the check. Stop, delete and other non-provisioning
// dispatches keep using getBrokerEndpoint.
func (d *HTTPAgentDispatcher) getProvisioningBrokerEndpoint(ctx context.Context, agent *store.Agent) (string, error) {
	broker, err := d.store.GetRuntimeBroker(ctx, agent.RuntimeBrokerID)
	if err != nil {
		return "", fmt.Errorf("failed to get runtime broker: %w", err)
	}
	if agent.ProjectID != "" {
		project, err := d.store.GetProject(ctx, agent.ProjectID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			// A deleted project cannot be empty-per-agent; nothing to gate.
		case err != nil:
			// Fail closed: a transient lookup error must not skip the gate
			// (design #2703 D3).
			return "", fmt.Errorf("failed to load project for capability check: %w", err)
		default:
			if err := checkEmptyPerAgentBrokerCapability(project, broker); err != nil {
				return "", err
			}
		}
	}
	return broker.Endpoint, nil
}

// resolveProvisionCredentials collects project-scope secrets for use by the
// broker's provision-time credential resolution (skill resolution, URI
// variable substitution, credential helpers). These are never forwarded to
// the agent container environment. Shared by the create and start/restart
// dispatch paths so that a required gh:// skill can resolve with the same
// project credentials regardless of which path (re-)provisions the agent.
// callerName prefixes every log message this function writes, at any level.
func (d *HTTPAgentDispatcher) resolveProvisionCredentials(ctx context.Context, agent *store.Agent, callerName string) map[string]string {
	if agent.ProjectID == "" || d.secretBackend == nil {
		return nil
	}
	projectSecrets, listErr := d.secretBackend.List(ctx, secret.Filter{
		Scope:   secret.ScopeProject,
		ScopeID: agent.ProjectID,
	})
	if listErr != nil {
		// Logged regardless of debug: without these credentials a private
		// gh:// skill silently resolves with the default credential (or none).
		d.log.Warn(callerName+": failed to list project secrets for ProvisionCredentials",
			"agent_id", agent.ID, "project_id", agent.ProjectID, "error", listErr)
		return nil
	}
	if len(projectSecrets) == 0 {
		d.log.Info(callerName+": ProvisionCredentials resolved",
			"agent_id", agent.ID, "project_id", agent.ProjectID, "count", 0)
		return nil
	}

	type namedValue struct{ name, value string }
	fetched := make([]namedValue, len(projectSecrets))

	// interrupted counts secrets not fetched because ctx was cancelled or
	// timed out. Those are reported once below rather than once per secret.
	var interrupted atomic.Int64
	g, gctx := errgroup.WithContext(ctx)
	for i, sm := range projectSecrets {
		if sm.SecretType == store.SecretTypeInternal {
			continue
		}
		i, sm := i, sm // capture loop vars
		g.Go(func() error {
			if gctx.Err() != nil {
				interrupted.Add(1)
				return nil
			}
			sv, getErr := d.secretBackend.Get(gctx, sm.Name, secret.ScopeProject, agent.ProjectID)
			if getErr != nil {
				if ctx.Err() != nil || errors.Is(getErr, context.Canceled) || errors.Is(getErr, context.DeadlineExceeded) {
					interrupted.Add(1)
					return nil
				}
				// Secret name and project only; never the value.
				d.log.Warn(callerName+": failed to get project secret for ProvisionCredentials",
					"agent_id", agent.ID, "project_id", agent.ProjectID, "secret", sm.Name, "error", getErr)
				return nil // don't fail the group for individual secrets
			}
			if sv != nil && sv.Value != "" {
				fetched[i] = namedValue{sm.Name, sv.Value}
			}
			return nil
		})
	}
	_ = g.Wait()
	if n := interrupted.Load(); n > 0 {
		cause := ctx.Err()
		if cause == nil {
			cause = context.Canceled
		}
		d.log.Warn(callerName+": ProvisionCredentials fetch stopped early; some project secrets were not fetched",
			"agent_id", agent.ID, "project_id", agent.ProjectID, "not_fetched", n, "error", cause)
	}

	creds := make(map[string]string)
	for _, nv := range fetched {
		if nv.name != "" {
			creds[nv.name] = nv.value
		}
	}
	d.log.Info(callerName+": ProvisionCredentials resolved",
		"agent_id", agent.ID, "project_id", agent.ProjectID, "count", len(creds))
	if len(creds) == 0 {
		return nil
	}
	return creds
}

// buildCreateRequest builds a RemoteCreateAgentRequest from the agent's store record.
// This is shared between DispatchAgentCreate and DispatchAgentProvision.
func (d *HTTPAgentDispatcher) buildCreateRequest(ctx context.Context, agent *store.Agent, callerName string) (*RemoteCreateAgentRequest, error) {
	buildRequestStart := time.Now()
	projectInfo, err := d.resolveDispatchProjectInfo(ctx, agent)
	if err != nil {
		return nil, err
	}

	// Build the remote create request
	//
	// Name carries agent.Slug, not agent.Name: the broker's create/replay
	// path (handlers.go's createAgent, and everything downstream of it,
	// including GetAgentDir) addresses the agent's on-disk directory using
	// this Name field directly, exactly like the Slug that start, stop,
	// restart and delete already send. Slug is immutable post-create and is
	// validated at creation time; Name is a freely PATCHable display field
	// (see applyAgentUpdate) that must never reach a path computation on its
	// own, including for a row whose Name and Slug have diverged.
	req := &RemoteCreateAgentRequest{
		RequestID:     api.NewUUID(),
		ID:            agent.ID,
		Slug:          agent.Slug,
		Name:          agent.Slug,
		ProjectID:     agent.ProjectID,
		UserID:        agent.OwnerID,
		HubEndpoint:   d.effectiveAgentHubEndpoint(),
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
			"hubEndpoint", d.effectiveAgentHubEndpoint(),
			"hasTokenGenerator", d.tokenGenerator != nil,
		)
	}

	// Generate agent token if token generator is available. A mint error
	// stops the dispatch before any broker request.
	if d.tokenGenerator != nil {
		token, err := mintAgentTokenAt(ctx, d.tokenGenerator, d.store, agent, mintSiteCreate)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", callerName, err)
		}
		req.AgentToken = token
		if d.debug {
			d.log.Debug("Generated agent token", "length", len(token))
		}
	} else if d.debug {
		d.log.Debug("No token generator configured - agent will not have Hub credentials")
	}

	// An agent written before ExplicitTimezone existed may still carry TZ in
	// its env records; adopt it as a pin before the env is copied below.
	adoptLegacyTZ(agent.AppliedConfig)

	// Add configuration if available
	if agent.AppliedConfig != nil {
		// effectiveDispatchWorkspace applies the "a linked local provider
		// clears an absolute workspace" rule -- see its doc comment. GitClone
		// and Branch are threaded through workspaceSpecFor instead (#1931),
		// so create and start dispatch from the single shared builder and
		// cannot drift from each other; GitClone is kept regardless of
		// whether the workspace was cleared: all hub-linked projects with a
		// git remote use clone-based provisioning (HTTPS + GitHub token)
		// rather than worktree-based, ensuring a consistent workspace
		// strategy regardless of whether the broker happens to have the
		// repo locally.
		workspace := effectiveDispatchWorkspace(agent.AppliedConfig.Workspace, projectInfo.projectPath)
		wsSpec := workspaceSpecFor(agent, projectInfo.workspaceMode)
		var remoteGCPIdentity *RemoteGCPIdentityConfig
		if gcpID := agent.AppliedConfig.GCPIdentity; gcpID != nil {
			remoteGCPIdentity = &RemoteGCPIdentityConfig{
				MetadataMode:        gcpID.MetadataMode,
				SAEmail:             gcpID.ServiceAccountEmail,
				ProjectID:           gcpID.ProjectID,
				RequireLocalRuntime: gcpID.RequireLocalRuntime,
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
			Branch:                    wsSpec.Branch,
			TemplateID:                agent.AppliedConfig.TemplateID,
			TemplateHash:              agent.AppliedConfig.TemplateHash,
			HarnessConfigID:           agent.AppliedConfig.HarnessConfigID,
			HarnessConfigHash:         agent.AppliedConfig.HarnessConfigHash,
			GitClone:                  wsSpec.GitClone,
			SharedWorkspace:           projectInfo.sharedWorkspace,
			GCPIdentity:               remoteGCPIdentity,
			ProjectPreStartHookScript: agent.AppliedConfig.ProjectPreStartHookScript,
		}

		// Hub operational agent_defaults (limits/resources) travel in their
		// own low-rank slot, NOT in InlineConfig: InlineConfig lands in the
		// override position at provision.go's merge and would let a hub-wide
		// floor outrank a template's explicit max_turns. The broker applies
		// these below the template and above its own settings.yaml defaults.
		// The limit/resource fields are empty in file mode — see
		// remoteHubAgentDefaults.
		//
		// The auto-expose default rides the same slot (never AppliedConfig.Env
		// or InlineConfig.Env, both of which outrank template and
		// harness-config env at the broker) and lands at buildAgentEnv's
		// lowest tier.
		var hubDefaults opsettings.AgentDefaultsSettings
		if d.hubAgentDefaultsProvider != nil {
			hubDefaults = d.hubAgentDefaultsProvider()
		}
		req.Config.HubAgentDefaults = remoteHubAgentDefaults(hubDefaults, d.autoExposePortsDefault())

		req.ResolvedEnv = agent.AppliedConfig.Env
		// Classify config-level env vars as plain. Env-type secrets that
		// were pre-merged into AppliedConfig.Env will be reclassified as
		// secret-fetchable when resolveSecrets injects them below.
		classifyEnvKeys(&req.EnvClassifications, agent.AppliedConfig.Env, api.EnvKindPlain)

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
	if _, ok := req.ResolvedEnv["SCION_MODEL"]; ok {
		classifyEnv(&req.EnvClassifications, "SCION_MODEL", api.EnvKindPlain)
	}
	injectThinkingLevelEnv(req.ResolvedEnv, agent.AppliedConfig)
	if _, ok := req.ResolvedEnv["SCION_THINKING_LEVEL"]; ok {
		classifyEnv(&req.EnvClassifications, "SCION_THINKING_LEVEL", api.EnvKindPlain)
	}

	// TZ comes only from the agent TZ resolver (pin > storage env > hub
	// default > none). Any other TZ in the env built so far is dropped; the
	// writers below (storage merge, env secrets) skip the key.
	setResolvedAgentTZ(req.ResolvedEnv, &req.EnvClassifications, d.resolveAgentTZ(ctx, agent, false))

	// Inject hub name so agents can label their Cloud Logging entries with the
	// hub identity, matching the hub-scoped log query filter (labels.hub).
	if d.hubName != "" {
		req.ResolvedEnv["SCION_HUB_NAME"] = d.hubName
		classifyEnv(&req.EnvClassifications, "SCION_HUB_NAME", api.EnvKindPlain)
	}

	// Resolve env vars from Hub storage (user/project/broker scopes) and merge.
	// Storage env vars fill in keys not already set (with a non-empty value)
	// by explicit config env vars. Empty-value config entries are passthrough
	// markers and should be overridden by storage values.
	envFromStorage, envFromStoragePlain, err := d.resolveEnvFromStorage(ctx, agent)
	if err != nil {
		if d.debug {
			d.log.Warn("buildCreateRequest: failed to resolve env from storage", "agent_id", agent.ID, "error", err)
		}
	} else if len(envFromStorage) > 0 {
		if req.ResolvedEnv == nil {
			req.ResolvedEnv = make(map[string]string)
		}
		for k, v := range envFromStorage {
			if k == agentTZEnvKey {
				continue // the resolver owns TZ
			}
			if existing, exists := req.ResolvedEnv[k]; !exists || existing == "" {
				req.ResolvedEnv[k] = v
				if envFromStoragePlain[k] {
					classifyEnv(&req.EnvClassifications, k, api.EnvKindPlain)
				} else {
					classifyEnv(&req.EnvClassifications, k, api.EnvKindSecretFetchable)
				}
			}
		}
	}

	// SCION_METADATA_MODE must be hub-authoritative, never sourced from
	// storage: overwrite whatever the fill-absent merge above put there.
	// req.Config.GCPIdentity (set above from agent.AppliedConfig.GCPIdentity)
	// is what the broker primarily trusts, but that struct is nil when the
	// agent has no GCP identity configured, and the broker then falls back to
	// this env var. Without this authoritative overwrite, a stored env var of
	// this name would decide the metadata mode on that fallback path.
	gcpMetadataMode := store.GCPMetadataModeBlock
	if req.Config != nil && req.Config.GCPIdentity != nil {
		gcpMetadataMode = req.Config.GCPIdentity.MetadataMode
	}
	req.ResolvedEnv["SCION_METADATA_MODE"] = gcpMetadataMode
	classifyEnv(&req.EnvClassifications, "SCION_METADATA_MODE", api.EnvKindPlain)
	// Marks the mode above as this hub's own authoritative write, not a value
	// that survived from a storage/secret merge. A broker that predates this
	// marker ignores it (harmless); a broker that checks it only trusts an
	// elevated (non-block) mode from resolvedEnv when this is present, which
	// is what closes the fallback path for a broker talking to an older hub
	// that never sends this marker at all.
	req.ResolvedEnv["SCION_METADATA_MODE_SOURCE"] = "hub"
	classifyEnv(&req.EnvClassifications, "SCION_METADATA_MODE_SOURCE", api.EnvKindPlain)

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

	// Resolve Hub-registry skills as the agent's creator. The broker's own
	// identity cannot read non-public skills (#1784).
	if d.skillPreResolver != nil {
		req.PreResolvedSkills = d.skillPreResolver(ctx, agent)
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
				// From secret store → secret-fetchable.
				classifyEnv(&req.EnvClassifications, "GITHUB_TOKEN", api.EnvKindSecretFetchable)
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
					// From secret store → secret-fetchable.
					classifyEnv(&req.EnvClassifications, "GITHUB_TOKEN", api.EnvKindSecretFetchable)
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
		resolvedSecrets, asNeededKeys, err := d.resolveSecrets(ctx, agent)
		resolvedSecrets = d.dropTZTargetedSecrets(ctx, agent, resolvedSecrets)
		asNeededKeys = withoutTZKey(asNeededKeys)
		if err != nil {
			d.log.ErrorContext(ctx, "Failed to resolve secrets; agent will start without injected secrets",
				"agent_id", agent.ID, "error", err)
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
						// From secret store → secret-fetchable.
						classifyEnv(&req.EnvClassifications, s.Target, api.EnvKindSecretFetchable)
					}
				}
			}
		}
		// Populate as_needed env-type secret targets so the broker's
		// autodetect can consider them when selecting auth type (#1447).
		if len(asNeededKeys) > 0 {
			req.AvailableAsNeededKeys = asNeededKeys
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
					classifyEnv(&req.EnvClassifications, "SCION_USER_GITHUB_TOKEN", api.EnvKindPlain)
					// Still enable the GitHub App machinery so the credential
					// helper can mint tokens for git push/pull operations.
					req.ResolvedEnv["SCION_GITHUB_APP_ENABLED"] = "true"
					classifyEnv(&req.EnvClassifications, "SCION_GITHUB_APP_ENABLED", api.EnvKindPlain)
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
						// GitHub App minted token — ephemeral, not in secret store.
						// This MUST run AFTER the secret-store GITHUB_TOKEN injection
						// (H6/H7/H8 above) so the classification overwrites
						// secret-fetchable → secret-injected when the App token wins.
						// A test pins this ordering dependency.
						req.ResolvedEnv["GITHUB_TOKEN"] = token
						classifyEnv(&req.EnvClassifications, "GITHUB_TOKEN", api.EnvKindSecretInjected)
						req.ResolvedEnv["SCION_GITHUB_APP_ENABLED"] = "true"
						classifyEnv(&req.EnvClassifications, "SCION_GITHUB_APP_ENABLED", api.EnvKindPlain)
						req.ResolvedEnv["SCION_GITHUB_TOKEN_EXPIRY"] = expiry
						classifyEnv(&req.EnvClassifications, "SCION_GITHUB_TOKEN_EXPIRY", api.EnvKindPlain)
						req.ResolvedEnv["SCION_GITHUB_TOKEN_PATH"] = "/tmp/.github-token"
						classifyEnv(&req.EnvClassifications, "SCION_GITHUB_TOKEN_PATH", api.EnvKindPlain)
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
	if creds := d.resolveProvisionCredentials(ctx, agent, "buildCreateRequest"); len(creds) > 0 {
		req.ProvisionCredentials = creds
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
		classifyEnv(&req.EnvClassifications, "SCION_DEV_TOKEN", api.EnvKindSecretInjected)
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
			// Bootstrap: IN argv. No diversion exists. Google-signed OIDC, 1h,
			// lifetime NOT boundable (GenerateIdTokenRequest has no Lifetime field).
			classifyEnv(&req.EnvClassifications, "SCION_TRANSPORT_TOKEN", api.EnvKindSecretBootstrap)
			req.ResolvedEnv["SCION_TRANSPORT_AUDIENCE"] = d.transportAudience
			classifyEnv(&req.EnvClassifications, "SCION_TRANSPORT_AUDIENCE", api.EnvKindPlain)
			req.ResolvedEnv["SCION_TRANSPORT_TOKEN_EXPIRY"] = tExpiry.UTC().Format(time.RFC3339)
			classifyEnv(&req.EnvClassifications, "SCION_TRANSPORT_TOKEN_EXPIRY", api.EnvKindPlain)
			if d.transportMode != "" {
				req.ResolvedEnv["SCION_TRANSPORT_MODE"] = d.transportMode
				classifyEnv(&req.EnvClassifications, "SCION_TRANSPORT_MODE", api.EnvKindPlain)
			}
		}
	}

	resolvedSkillsCount := 0
	if req.PreResolvedSkills != nil {
		resolvedSkillsCount = len(req.PreResolvedSkills.Resolved)
	}
	d.log.Info("buildCreateRequest complete",
		"agent_id", agent.ID, "caller", callerName,
		"elapsed_ms", time.Since(buildRequestStart).Milliseconds(),
		"resolvedSecretsCount", len(req.ResolvedSecrets),
		"resolvedSkillsCount", resolvedSkillsCount)
	return req, nil
}

// effectiveDispatchWorkspace applies the rule buildCreateRequest has always
// used for the wire request's Workspace field, factored out so the Hub's
// reincarnate eligibility gate (handleReincarnateAgent) can evaluate the
// same effective value instead of the raw AppliedConfig.Workspace (design
// §3.4 Amendment A23.1, review p1b-r1 R1): when the target broker has a
// registered local provider path for the project (linkedProjectPath, from
// resolveDispatchProjectInfo's ProjectProvider.LocalPath lookup), the broker
// derives its own workspace location from that path, so an absolute
// AppliedConfig.Workspace is never actually sent — it is cleared here,
// exactly as it always was inline in buildCreateRequest. A relative
// workspace is kept: the broker joins it with its own project root.
//
// Without this, the Hub's gate and the broker's actual dispatched request
// could disagree — a shared-workspace project linked to a broker via a
// ProjectProvider looks eligible to the Hub (Workspace is non-empty) but
// arrives at the broker as Workspace="", GitClone=nil, which
// Manager.Reprovision correctly refuses only after the agent has already
// been stopped.
func effectiveDispatchWorkspace(workspace, linkedProjectPath string) string {
	if linkedProjectPath != "" {
		if workspace == "" || filepath.IsAbs(workspace) {
			return ""
		}
		// else: relative workspace -- keep it; broker joins with its own project root
	}
	return workspace
}

// projectDispatchInfo contains resolved project information for dispatching agent requests.
type projectDispatchInfo struct {
	projectPath     string
	projectSlug     string
	sharedDirs      []api.SharedDir
	sharedWorkspace bool   // true for git-workspace hybrid projects
	workspaceMode   string // resolved workspace mode label (e.g. "shared", "worktree-per-agent")
}

// resolveDispatchProjectInfo resolves the project facts a dispatch carries.
// A project that no longer exists yields empty info (nothing to resolve); any
// other project lookup error is returned so the dispatch fails closed rather
// than sending an empty workspace mode — for an empty-per-agent project that
// would let a capable broker fall back to mounting the shared project
// directory (design #2703 D3).
func (d *HTTPAgentDispatcher) resolveDispatchProjectInfo(ctx context.Context, agent *store.Agent) (projectDispatchInfo, error) {
	// Look up the local path for this project on the target runtime broker.
	// A provider LocalPath (linked project) takes precedence over hub-native
	// slug resolution, even for projects without a git remote. Only when there
	// is no provider path and no git remote do we fall back to projectSlug so
	// the broker resolves the conventional ~/.scion/projects/<slug> path.
	if agent.ProjectID == "" {
		return projectDispatchInfo{}, nil
	}

	var info projectDispatchInfo

	project, err := d.store.GetProject(ctx, agent.ProjectID)
	if errors.Is(err, store.ErrNotFound) {
		return projectDispatchInfo{}, nil
	}
	if err != nil {
		return projectDispatchInfo{}, fmt.Errorf("resolve project %s for dispatch: %w", agent.ProjectID, err)
	}

	info.sharedDirs = project.SharedDirs
	info.sharedWorkspace = project.IsSharedWorkspace()
	info.workspaceMode = dispatchWorkspaceMode(project)

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
	return info, nil
}

// applyBrokerResponse updates agent fields from the broker's response and
// relays the broker's hub-only env warnings to the dispatch warnings
// collector on ctx, if the caller attached one.
func (d *HTTPAgentDispatcher) applyBrokerResponse(ctx context.Context, agent *store.Agent, resp *RemoteAgentResponse) {
	d.forgetRuntimeTarget(ctx, agent)
	if resp == nil {
		return
	}
	if resp.Agent != nil {
		addDispatchWarnings(ctx, resp.Agent.Warnings...)
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
		applyBrokerAgentConfig(agent, resp.Agent)
	} else if d.debug {
		d.log.Debug("applyBrokerResponse: broker response has nil Agent",
			"agentName", agent.Name,
		)
	}
}

// applyBrokerAgentConfig copies the non-status fields of a broker's agent
// answer (template, harness, image, profile, runtime) onto agent.
func applyBrokerAgentConfig(agent *store.Agent, info *RemoteAgentInfo) {
	if info.Template != "" {
		agent.Template = info.Template
	}
	if agent.AppliedConfig != nil {
		if info.HarnessConfig != "" {
			agent.AppliedConfig.HarnessConfig = info.HarnessConfig
		}
		if info.HarnessAuth != "" {
			agent.AppliedConfig.HarnessAuth = info.HarnessAuth
		}
		if info.Image != "" {
			agent.AppliedConfig.Image = info.Image
		}
		if info.Profile != "" {
			agent.AppliedConfig.Profile = info.Profile
		}
	}
	if info.Runtime != "" {
		agent.Runtime = info.Runtime
	}
}

// forgetRuntimeTarget drops the agent's recorded runtime target (and any
// candidate), in memory and in the store, after a create, start or restart
// was accepted by the broker. The broker may have placed the agent on a
// different runtime target than the one last recorded, so the
// missing-container reconcile must not consider the agent until heartbeats
// list it again and record its target. The store write is targeted (it does
// not depend on the caller persisting the agent), because the lifecycle start
// path only writes status fields. The clear bumps state_version; when the
// in-memory agent was current before the clear it adopts the new version, so
// the caller's own later UpdateAgent still succeeds, while any other holder
// of a pre-clear read gets a version conflict instead of writing the old
// target back.
func (d *HTTPAgentDispatcher) forgetRuntimeTarget(ctx context.Context, agent *store.Agent) {
	if agent.AppliedConfig != nil {
		agent.AppliedConfig.RuntimeTarget = ""
		agent.AppliedConfig.RuntimeTargetCandidate = ""
	}
	if d.store == nil || agent.ID == "" {
		return
	}
	cleared, newVersion, err := d.store.ClearAgentRuntimeTarget(ctx, agent.ID)
	if err != nil {
		d.log.Warn("Failed to clear the recorded runtime target after dispatch",
			"agent_id", agent.ID, "error", err)
		return
	}
	if cleared && agent.StateVersion == newVersion-1 {
		agent.StateVersion = newVersion
	}
}

// DispatchAgentCreate creates and starts an agent on the runtime broker.
func (d *HTTPAgentDispatcher) DispatchAgentCreate(ctx context.Context, agent *store.Agent) (_ *CreateDispatchResult, err error) {
	ctx, span := tracer.Start(ctx, "hub.dispatch.create")
	defer span.End()
	span.SetAttributes(
		attribute.String("scion.agent.id", agent.ID),
		attribute.String("scion.broker.id", agent.RuntimeBrokerID),
	)

	if err := requireRuntimeBrokerAssigned(agent); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	endpoint, err := d.getProvisioningBrokerEndpoint(ctx, agent)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	req, err := d.buildCreateRequest(ctx, agent, "DispatchAgentCreate")
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	// buildCreateRequest mints (and best-effort persists) a fresh agent
	// credential before returning — but only when a token generator is
	// configured; with none it carries on without one. A mint error has
	// already returned above, before any broker request, and minted nothing,
	// so there is nothing to revoke on that path. Any failure from here on
	// must revoke the credential, but only if this call actually minted one
	// — see revokeAgentCredentialsBestEffort's doc comment.
	issued := req.AgentToken != ""
	defer func() {
		if err != nil && issued {
			revokeAgentCredentialsBestEffort(ctx, d.store, agent.ID, agentCredentialRevokeReasonCreateFailed)
		}
	}()

	send := func(ctx context.Context, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
		resp, err := d.client.CreateAgent(ctx, agent.RuntimeBrokerID, endpoint, req)
		if isHashMismatchError(err) {
			if repairErr := d.repairHashMismatch(ctx, agent, err); repairErr == nil {
				resp, err = d.client.CreateAgent(ctx, agent.RuntimeBrokerID, endpoint, req)
			}
		}
		return resp, nil, err
	}
	resp, _, launch, err := d.dispatchLaunching(ctx, agent, endpoint, req, false, send)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	if launch != nil {
		d.applyAcceptedLaunchResponse(ctx, agent, resp)
		return &CreateDispatchResult{Launch: launch}, nil
	}

	d.applyBrokerResponse(ctx, agent, resp)
	return nil, nil
}

// DispatchAgentProvision provisions an agent on the runtime broker without starting it.
// It uses the same GatherEnv two-pass mechanism as DispatchAgentCreateWithGather so
// that as_needed env vars (e.g. GOOGLE_CLOUD_PROJECT, GOOGLE_CLOUD_REGION) are
// resolved before auth provisioning runs on the broker.
func (d *HTTPAgentDispatcher) DispatchAgentProvision(ctx context.Context, agent *store.Agent) error {
	return d.dispatchProvision(ctx, agent, "DispatchAgentProvision", false)
}

// DispatchAgentReprovision re-renders an EXISTING agent's on-disk config
// (scion-agent.json, agent-info.json, home dotfiles, skills) on the runtime
// broker from the agent's current AppliedConfig, for a `scion reincarnate`
// request (design §3.4). The caller (the reincarnate worker) must have
// already replaced agent.AppliedConfig with a freshly resolved config before
// calling this — buildCreateRequest below reads directly from it, the same
// way it does for a brand new agent.
//
// Unlike DispatchAgentProvision, this always sets Reprovision on the wire
// request, which tells the broker to overwrite the persisted config rather
// than reuse it (see runtimebroker.CreateAgentRequest.Reprovision and
// agent.Manager.Reprovision). It does not start the container; the caller
// does that separately via DispatchAgentStart. Precondition: the agent's
// container is already stopped.
func (d *HTTPAgentDispatcher) DispatchAgentReprovision(ctx context.Context, agent *store.Agent) error {
	return d.dispatchProvision(ctx, agent, "DispatchAgentReprovision", true)
}

// dispatchProvision is the shared implementation behind DispatchAgentProvision
// and DispatchAgentReprovision: build a provision-only create request, dispatch
// it with the GatherEnv two-pass mechanism, and merge any resolved storage env
// back into AppliedConfig.
func (d *HTTPAgentDispatcher) dispatchProvision(ctx context.Context, agent *store.Agent, callerName string, reprovision bool) (err error) {
	if err := requireRuntimeBrokerAssigned(agent); err != nil {
		return err
	}

	endpoint, err := d.getProvisioningBrokerEndpoint(ctx, agent)
	if err != nil {
		return err
	}

	req, err := d.buildCreateRequest(ctx, agent, callerName)
	if err != nil {
		return err
	}
	// See DispatchAgentCreate's identical defer (gated on issued, below):
	// buildCreateRequest already minted a credential by this point if
	// issued is true, so every error return below — including the
	// reprovision-specific hard-fail checks further down — must revoke it.
	issued := req.AgentToken != ""
	defer func() {
		if err != nil && issued {
			revokeAgentCredentialsBestEffort(ctx, d.store, agent.ID, agentCredentialRevokeReasonCreateFailed)
		}
	}()
	req.ProvisionOnly = true
	req.Reprovision = reprovision
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
		return fmt.Errorf("%s not supported for cross-node broker: %w", callerName, err)
	} else if err != nil {
		return err
	} else if resp != nil {
		d.applyBrokerResponse(ctx, agent, resp)
	}

	finalResp := resp
	finalNeeds := envReqs

	// Second pass: if the broker reported needed keys, check whether any can
	// be satisfied by as_needed env vars or secrets — mirroring the pattern in
	// DispatchAgentCreateWithGather. We inline this instead of calling
	// DispatchFinalizeEnv because the finalize path does not set ProvisionOnly.
	// TZ is never a gathered key: drop it from the needs and, if an older
	// broker asked for it, answer it with the resolver in the replay.
	tzNeeded := takeTZGatherNeed(envReqs)
	if envReqs != nil && (len(envReqs.Needs) > 0 || tzNeeded) {
		var asNeededEnv map[string]string
		if len(envReqs.Needs) > 0 {
			asNeededEnv = d.resolveAsNeededForKeys(ctx, agent, envReqs.Needs, envReqs.Alternatives)
		}
		if len(asNeededEnv) > 0 || tzNeeded {
			if req.ResolvedEnv == nil {
				req.ResolvedEnv = make(map[string]string)
			}
			for k, v := range asNeededEnv {
				req.ResolvedEnv[k] = v
			}
			if tzNeeded {
				setResolvedAgentTZ(req.ResolvedEnv, &req.EnvClassifications, d.resolveAgentTZ(ctx, agent, true))
			}
			req.EnvSources = d.buildEnvSources(ctx, agent, req.ResolvedEnv)
			// Design §3.4 Amendment A2.3: a fresh RequestID for the replay. Reusing the first
			// pass's ID would let the broker's attempt cache (keyed by
			// RequestID) replay the stored 202 instead of reprocessing with
			// the newly resolved env — silently defeating this whole retry.
			req.RequestID = api.NewUUID()

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
			takeTZGatherNeed(envReqs2)
			if envReqs2 != nil && len(envReqs2.Needs) > 0 {
				d.log.Warn(callerName+": env vars still missing after second pass",
					"agent", agent.Name, "needs", envReqs2.Needs)
			}
			if resp2 != nil {
				d.applyBrokerResponse(ctx, agent, resp2)
			}
			finalResp = resp2
			finalNeeds = envReqs2
		}
	}

	// Design §3.4 Amendment A2.3: for a reprovision specifically, a final 202-with-Needs (env
	// still missing) or a missing 201 response is a hard failure, never
	// "warn and continue". DispatchAgentProvision's more forgiving behavior
	// exists because a follow-up start re-gathers env on that path; the
	// reincarnate worker goes straight from this call to DispatchAgentStart,
	// so "warn and continue" here would silently start the new generation on
	// an incomplete or unreplaced config and still report AC-1 success.
	if reprovision {
		if finalNeeds != nil && len(finalNeeds.Needs) > 0 {
			return fmt.Errorf("%s: required env still missing after reprovision: %v", callerName, finalNeeds.Needs)
		}
		if finalResp == nil {
			return fmt.Errorf("%s: broker returned no confirmation for the reprovision request", callerName)
		}
		// Design §3.4 Amendment A2.2(a): mandatory echo. An old broker has no concept of
		// Reprovision, so it silently ran a plain Provision and returned 201
		// with Reprovisioned unset — the persisted config was NOT replaced,
		// so this must fail exactly like any other reprovision failure.
		if !finalResp.Reprovisioned {
			return fmt.Errorf("%s: broker did not confirm the reprovision (it may not support reincarnate; its reported capabilities may be stale)", callerName)
		}
	}

	// Merge resolved storage env vars back into AppliedConfig so they are
	// visible in the advanced config form. Exclude internal SCION_* vars
	// which are injected at start time. This runs after both passes so that
	// as_needed vars resolved in the second pass are included.
	//
	// Only keys explicitly classified api.EnvKindPlain in req.EnvClassifications
	// are persisted (see shouldPersistResolvedEnvKey). A key that is merely
	// absent from the classification map is NOT plain by default -- it is
	// dropped, not kept. Every key merged into req.ResolvedEnv anywhere in
	// this dispatcher must have a matching classifyEnv/classifyEnvKeys call
	// (enforced by TestBuildCreateRequestClassifiesEveryResolvedEnvKey) or it
	// will silently stop showing up here; that is the intended fail-closed
	// behavior, not a bug to work around by classifying it Plain.
	//
	// Not on reprovision: the fresh config the reincarnate worker built is
	// authoritative, and resolved env is re-resolved on every dispatch, so
	// merging it back would only make the next reincarnation plan show a
	// false env diff. The skip applies before the classification check, so
	// it holds even for keys classified api.EnvKindPlain.
	if !reprovision && agent.AppliedConfig != nil && len(req.ResolvedEnv) > 0 {
		if agent.AppliedConfig.Env == nil {
			agent.AppliedConfig.Env = make(map[string]string)
		}
		for k, v := range req.ResolvedEnv {
			if !shouldPersistResolvedEnvKey(k, req.EnvClassifications) {
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
func (d *HTTPAgentDispatcher) DispatchAgentCreateWithGather(ctx context.Context, agent *store.Agent) (_ *CreateDispatchResult, err error) {
	dispatchStart := time.Now()
	if err := requireRuntimeBrokerAssigned(agent); err != nil {
		return nil, err
	}

	endpoint, err := d.getProvisioningBrokerEndpoint(ctx, agent)
	if err != nil {
		return nil, err
	}

	req, err := d.buildCreateRequest(ctx, agent, "DispatchAgentCreateWithGather")
	if err != nil {
		return nil, err
	}
	// buildCreateRequest already minted a credential if issued is true. A
	// nil error here can still mean failure from the caller's point of view
	// (envReqs.Needs non-empty on the non-gather path) — that case does not
	// go through this defer because it is not an error return; the caller
	// revokes explicitly instead (handlers_agents_core.go). Every actual
	// error return below, including through the cross-node
	// deferredCreateWithGather fallback, does go through this defer.
	issued := req.AgentToken != ""
	defer func() {
		if err != nil && issued {
			revokeAgentCredentialsBestEffort(ctx, d.store, agent.ID, agentCredentialRevokeReasonCreateFailed)
		}
	}()
	req.GatherEnv = true

	// Track which scope provided each key
	req.EnvSources = d.buildEnvSources(ctx, agent, req.ResolvedEnv)

	d.log.Info("Dispatcher: request built, sending to broker",
		"agent_id", agent.ID, "agent", agent.Name,
		"broker", agent.RuntimeBrokerID, "buildElapsed", time.Since(dispatchStart).String())
	brokerCallStart := time.Now()
	send := func(ctx context.Context, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
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
		return resp, envReqs, err
	}
	resp, envReqs, launch, err := d.dispatchLaunching(ctx, agent, endpoint, req, true, send)
	if err == nil && launch != nil {
		d.applyAcceptedLaunchResponse(ctx, agent, resp)
		return &CreateDispatchResult{Launch: launch}, nil
	}
	if errors.Is(err, ErrLifecycleDeferred) {
		var deferred *CreateDispatchResult
		deferred, err = d.deferredCreateWithGather(ctx, agent)
		if err != nil {
			return nil, err
		}
		if deferred.AcceptedLaunch() != nil {
			// The owner node accepted the create for asynchronous launch;
			// return its result unchanged.
			return deferred, nil
		}
		envReqs = deferred.EnvRequirements()
		// Fall through to the second-pass as_needed resolution below.
	} else if err != nil {
		return nil, err
	} else if resp != nil {
		d.applyBrokerResponse(ctx, agent, resp)
	}

	// Second pass: if the broker reported needed keys, check whether any can
	// be satisfied by as_needed env vars or secrets. If so, finalize them
	// transparently without requiring CLI intervention. TZ is never a
	// gathered key: it is removed from the needs here and, if an older
	// broker asked for it, answered by the resolver in the finalize pass.
	tzNeeded := takeTZGatherNeed(envReqs)
	if envReqs != nil && (len(envReqs.Needs) > 0 || tzNeeded) {
		var asNeededEnv map[string]string
		if len(envReqs.Needs) > 0 {
			asNeededEnv = d.resolveAsNeededForKeys(ctx, agent, envReqs.Needs, envReqs.Alternatives)
		}
		if len(asNeededEnv) > 0 || tzNeeded {
			finalized, err := d.finalizeEnv(ctx, agent, asNeededEnv, tzNeeded)
			if err == nil {
				// All needs satisfied by as_needed entries. finalized carries
				// Launch when that send was accepted for asynchronous launch.
				return finalized, nil
			}
			var stillMissing *ErrEnvStillMissing
			if errors.As(err, &stillMissing) {
				return envReqsResult(stillMissing.Requirements), nil // Partial; remaining needs returned
			}
			return nil, err
		}
	}

	return envReqsResult(envReqs), nil
}

// deferredCreateWithGather handles a cross-node create-with-gather via durable dispatch.
func (d *HTTPAgentDispatcher) deferredCreateWithGather(ctx context.Context, agent *store.Agent) (*CreateDispatchResult, error) {
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
	if cr.Launch != nil {
		return &CreateDispatchResult{Launch: cr.Launch}, nil
	}
	return envReqsResult(cr.EnvRequirements), nil
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
func (d *HTTPAgentDispatcher) DispatchFinalizeEnv(ctx context.Context, agent *store.Agent, env map[string]string) (*CreateDispatchResult, error) {
	return d.finalizeEnv(ctx, agent, env, false)
}

// finalizeEnv implements DispatchFinalizeEnv. TZ in the caller env is
// dropped: only the resolver writes TZ. answerTZ sends the resolver's
// gather answer for TZ (UTC rather than no TZ) because the broker listed TZ
// as needed; a TZ need reported by this replay is answered the same way.
func (d *HTTPAgentDispatcher) finalizeEnv(ctx context.Context, agent *store.Agent, env map[string]string, answerTZ bool) (_ *CreateDispatchResult, err error) {
	// Defense in depth for a caller-supplied env map that does not go through
	// the submitAgentEnv reserved-target check (or reaches this dispatch path
	// some other way): never let it override a scion control-plane env var.
	// This reassigns the local env variable, so both the merge below and the
	// deferred-replay call further down inherit the sanitized map — and a
	// deferred op queued before this check existed is sanitized fresh the
	// next time it is replayed through this same function.
	if len(env) > 0 {
		sanitized := make(map[string]string, len(env))
		for k, v := range env {
			if secret.IsReservedEnvTarget(k) {
				if d.debug {
					d.log.Debug("DispatchFinalizeEnv: dropping reserved-target caller env", "agent_id", agent.ID, "key", k)
				}
				continue
			}
			sanitized[k] = v
		}
		env = sanitized
	}

	env = d.withoutCallerTZ(ctx, agent, env)
	if err := requireRuntimeBrokerAssigned(agent); err != nil {
		return nil, err
	}

	endpoint, err := d.getProvisioningBrokerEndpoint(ctx, agent)
	if err != nil {
		return nil, err
	}

	req, err := d.buildCreateRequest(ctx, agent, "DispatchFinalizeEnv")
	if err != nil {
		return nil, err
	}
	// See DispatchAgentCreate's identical defer (gated on issued, below):
	// buildCreateRequest already minted a credential by this point if
	// issued is true, including ErrEnvStillMissing below, which is a real
	// error return on this path (unlike DispatchAgentCreateWithGather's
	// non-error needs-still-missing case).
	issued := req.AgentToken != ""
	defer func() {
		if err != nil && issued {
			revokeAgentCredentialsBestEffort(ctx, d.store, agent.ID, agentCredentialRevokeReasonCreateFailed)
		}
	}()
	req.GatherEnv = true

	if req.ResolvedEnv == nil {
		req.ResolvedEnv = map[string]string{}
	}
	for k, v := range env {
		req.ResolvedEnv[k] = v
	}
	// Classify the caller-provided env as plain config vars.
	classifyEnvKeys(&req.EnvClassifications, env, api.EnvKindPlain)
	if answerTZ {
		setResolvedAgentTZ(req.ResolvedEnv, &req.EnvClassifications, d.resolveAgentTZ(ctx, agent, true))
	}

	req.EnvSources = d.buildEnvSources(ctx, agent, req.ResolvedEnv)

	send := func(ctx context.Context, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
		resp, envReqs, err := d.client.CreateAgentWithGather(ctx, agent.RuntimeBrokerID, endpoint, req)
		if isHashMismatchError(err) {
			if repairErr := d.repairHashMismatch(ctx, agent, err); repairErr == nil {
				resp, envReqs, err = d.client.CreateAgentWithGather(ctx, agent.RuntimeBrokerID, endpoint, req)
			}
		}
		return resp, envReqs, err
	}
	resp, envReqs, launch, err := d.dispatchLaunching(ctx, agent, endpoint, req, true, send)
	if errors.Is(err, ErrLifecycleDeferred) {
		return d.deferredFinalizeEnv(ctx, agent, env)
	}
	if err != nil {
		return nil, err
	}
	if launch != nil {
		d.applyAcceptedLaunchResponse(ctx, agent, resp)
		return &CreateDispatchResult{Launch: launch}, nil
	}

	tzNeeded := takeTZGatherNeed(envReqs) && !answerTZ
	if envReqs != nil && (len(envReqs.Needs) > 0 || tzNeeded) {
		// Second pass: try to satisfy remaining needs with as_needed entries,
		// mirroring the pattern in DispatchAgentCreateWithGather.
		var asNeededEnv map[string]string
		if len(envReqs.Needs) > 0 {
			asNeededEnv = d.resolveAsNeededForKeys(ctx, agent, envReqs.Needs, envReqs.Alternatives)
		}
		if len(asNeededEnv) > 0 || tzNeeded {
			for k, v := range asNeededEnv {
				req.ResolvedEnv[k] = v
			}
			// as_needed env comes from storage — classify as secret-fetchable.
			classifyEnvKeys(&req.EnvClassifications, asNeededEnv, api.EnvKindSecretFetchable)
			if tzNeeded {
				setResolvedAgentTZ(req.ResolvedEnv, &req.EnvClassifications, d.resolveAgentTZ(ctx, agent, true))
			}
			// A fresh RequestID for the replay: reusing the first send's ID
			// would let the broker's attempt cache replay the stored 202
			// instead of processing the newly resolved env (and, for an
			// async launch, the new launch ID).
			req.RequestID = api.NewUUID()
			resp2, envReqs2, launch2, err2 := d.dispatchLaunching(ctx, agent, endpoint, req, true, func(ctx context.Context, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
				return d.client.CreateAgentWithGather(ctx, agent.RuntimeBrokerID, endpoint, req)
			})
			if err2 != nil {
				return nil, err2
			}
			takeTZGatherNeed(envReqs2)
			if envReqs2 != nil && len(envReqs2.Needs) > 0 {
				return nil, &ErrEnvStillMissing{Requirements: envReqs2}
			}
			if launch2 != nil {
				d.applyAcceptedLaunchResponse(ctx, agent, resp2)
				return &CreateDispatchResult{Launch: launch2}, nil
			}
			if resp2 != nil {
				d.applyBrokerResponse(ctx, agent, resp2)
			}
			return nil, nil
		}
		return nil, &ErrEnvStillMissing{Requirements: envReqs}
	}

	if resp != nil {
		d.applyBrokerResponse(ctx, agent, resp)
	}
	return nil, nil
}

// deferredFinalizeEnv handles a cross-node finalize_env via durable dispatch.
// The owner's result carries Launch when its send was accepted for
// asynchronous launch.
func (d *HTTPAgentDispatcher) deferredFinalizeEnv(ctx context.Context, agent *store.Agent, env map[string]string) (*CreateDispatchResult, error) {
	result, err := d.deferredDataOpResult(ctx, agent, "finalize_env", &FinalizeEnvDispatchArgs{Env: env})
	if err != nil {
		return nil, err
	}
	if result.Result == "" {
		return nil, nil
	}
	var fr FinalizeEnvResult
	if err := json.Unmarshal([]byte(result.Result), &fr); err != nil {
		return nil, fmt.Errorf("unmarshal finalize_env result: %w", err)
	}
	if fr.Launch != nil {
		return &CreateDispatchResult{Launch: fr.Launch}, nil
	}
	return nil, nil
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
// applies to the agent and returns a merged map, plus a companion map
// recording which of those keys came from a storage entry with Secret==false
// ("plain"). A key absent from the plain map (or present with false) is not
// known plain and must not be treated as one by a caller deciding what to
// persist. Scopes are applied lowest precedence first; the order itself is
// stated in exactly one place, envScopePrecedence above.
//
// The caller then overlays explicit agent config env on top of the result, so
// agent config outranks every storage scope (see buildCreateRequest).
func (d *HTTPAgentDispatcher) resolveEnvFromStorage(ctx context.Context, agent *store.Agent) (map[string]string, map[string]bool, error) {
	result := make(map[string]string)
	plain := make(map[string]bool)
	if agent == nil {
		return result, plain, nil
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
			// Defense in depth for rows that predate the create/patch
			// reserved-target check, or that reached the store through some
			// other path: never let a stored env var decide a scion
			// control-plane env var's value.
			if secret.IsReservedEnvTarget(v.Key) {
				continue
			}
			result[v.Key] = v.Value
			plain[v.Key] = !v.Secret
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
				if secret.IsReservedEnvTarget(v.Key) {
					continue
				}
				if _, exists := result[v.Key]; exists {
					continue // higher-precedence scope already set this key
				}
				result[v.Key] = v.Value
				plain[v.Key] = !v.Secret
			}
		}
	}

	return result, plain, nil
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

	// TZ is never an as_needed key: only the agent TZ resolver supplies it.
	delete(keySet, agentTZEnvKey)

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
			// Defense in depth, matching resolveSecrets: never satisfy an
			// as-needed request for a scion control-plane env var from a
			// stored plain env var either.
			if secret.IsReservedEnvTarget(v.Key) {
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
			// The synthetic identity must carry the agent's real scopes.
			// Agent authority is derived from JWT scopes (buildAgentSyntheticBindings),
			// and agentScopeRestriction denies everything when the scope list is
			// empty, so an identity built without them can never be allowed.
			role, additionalScopes := agentRoleAndScopes(agent)
			scopes := append(ScopesForRole(role), additionalScopes...)
			resolveOpts = &secret.ResolveOpts{
				AgentAncestry: ancestry,
				AuthzCheck: func(s secret.SecretMeta) bool {
					ident := &agentIdentityWrapper{
						AgentTokenClaims: &AgentTokenClaims{
							Claims:    jwt.Claims{Subject: agentID},
							ProjectID: agent.ProjectID,
							Ancestry:  ancestry,
							Scopes:    scopes,
						},
					}
					// Name the permission explicitly. (resource="secret",
					// action="read") has no registry entry - agent secret access is
					// registered as project.secret_read on ResourceProject - so
					// resolveResourcePermission cannot resolve the pair on its own.
					decision := d.authzService.Decide(ctx, AuthzRequest{
						Principal:  principalContextForIdentity(ident),
						Credential: credentialContextForIdentity(ident),
						Resource:   Resource{Type: "secret", ID: s.ID},
						Action:     ActionRead,
						Permission: permissionProjectSecretRead,
					})
					if !decision.Allowed && d.debug {
						d.log.Debug("progeny secret denied by authz",
							"agent_id", agentID, "secret", s.Name, "secret_id", s.ID,
							"reason", decision.Reason, "scopes", len(scopes))
					}
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
				// Defense in depth, matching resolveSecrets: never satisfy an
				// as-needed request for a scion control-plane env var from a
				// stored secret.
				if secret.IsReservedEnvTarget(target) {
					continue
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

	delete(result, agentTZEnvKey)
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

	// TZ is labelled with the rung of the agent TZ chain that supplied it,
	// whatever storage or config entries share the key.
	delete(sources, agentTZEnvKey)
	if _, inResolved := resolvedEnv[agentTZEnvKey]; inResolved {
		sources[agentTZEnvKey] = d.resolveAgentTZ(ctx, agent, false).Source
	}

	return sources
}

func (d *HTTPAgentDispatcher) injectLifecycleGitHubToken(
	ctx context.Context,
	agent *store.Agent,
	resolvedEnv map[string]string,
	envClassifications *map[string]api.EnvKind,
	operation string,
) {
	if d.githubAppMinter == nil || agent.ProjectID == "" {
		return
	}

	project, err := d.store.GetProject(ctx, agent.ProjectID)
	if err != nil {
		return
	}
	mintProject := project
	if project.GitHubInstallationID == nil {
		if sourceProjectID := agent.Labels["scion.dev/github-token-source-project"]; sourceProjectID != "" {
			if sourceProject, sourceErr := d.store.GetProject(ctx, sourceProjectID); sourceErr == nil && sourceProject.GitHubInstallationID != nil {
				mintProject = sourceProject
			}
		}
	}
	if mintProject.GitHubInstallationID == nil {
		return
	}

	if resolvedEnv["GITHUB_TOKEN"] != "" {
		d.log.Warn(operation+": user GITHUB_TOKEN takes precedence over GitHub App token — user token will be used for gh CLI, GitHub App for git credential helper",
			"project_id", agent.ProjectID)
		resolvedEnv["SCION_USER_GITHUB_TOKEN"] = "true"
		classifyEnv(envClassifications, "SCION_USER_GITHUB_TOKEN", api.EnvKindPlain)
		resolvedEnv["SCION_GITHUB_APP_ENABLED"] = "true"
		classifyEnv(envClassifications, "SCION_GITHUB_APP_ENABLED", api.EnvKindPlain)
		return
	}

	token, expiry, err := d.githubAppMinter.MintGitHubAppTokenForProject(ctx, mintProject)
	if err != nil {
		if d.debug {
			d.log.Warn(operation+": GitHub App token minting failed", "error", err, "project_id", agent.ProjectID)
		}
		return
	}
	if token == "" {
		return
	}

	resolvedEnv["GITHUB_TOKEN"] = token
	classifyEnv(envClassifications, "GITHUB_TOKEN", api.EnvKindSecretInjected)
	resolvedEnv["SCION_GITHUB_APP_ENABLED"] = "true"
	classifyEnv(envClassifications, "SCION_GITHUB_APP_ENABLED", api.EnvKindPlain)
	resolvedEnv["SCION_GITHUB_TOKEN_EXPIRY"] = expiry
	classifyEnv(envClassifications, "SCION_GITHUB_TOKEN_EXPIRY", api.EnvKindPlain)
	resolvedEnv["SCION_GITHUB_TOKEN_PATH"] = "/tmp/.github-token"
	classifyEnv(envClassifications, "SCION_GITHUB_TOKEN_PATH", api.EnvKindPlain)
}

// startEnvResult bundles everything buildStartEnv assembles. DispatchAgentStart
// consumes all of it (env, classifications and secrets for the broker call,
// projectInfo for projectPath/projectSlug/sharedDirs/sharedWorkspace, and
// workspace for StartExtras.Workspace); DispatchAgentRestart consumes only
// env and classifications, since RestartAgent takes neither secrets nor a
// project path, and restart never recreates the workspace.
type startEnvResult struct {
	env             map[string]string
	classifications map[string]api.EnvKind
	secrets         []ResolvedSecret
	storageEnvCount int
	projectInfo     projectDispatchInfo
	workspace       WorkspaceDispatchSpec
	// tokenIssued reports whether this call actually minted a fresh Hub auth
	// token (d.tokenGenerator succeeded and returned a non-empty token), as
	// opposed to carrying on without one because no generator is configured.
	// A mint error never reaches this struct: buildStartEnv returns the error
	// instead. DispatchAgentStart's revoke-on-failure defer must only arm
	// when this call issued a credential to revoke.
	tokenIssued bool
}

// buildStartEnv assembles the full resolved environment used to start or
// restart an agent on the runtime broker: applied-config env, model/
// thinking-level overrides, Hub-storage env (user/project/hub/broker scopes,
// plus progeny), type-aware secrets (environment-type ones merged into env),
// agent identity and hub-connectivity vars, workspace sharing mode and
// git-ness, GCP identity vars, a fresh Hub auth token, a transport token, and
// any GitHub App lifecycle token.
//
// DispatchAgentStart and DispatchAgentRestart previously each assembled this
// independently; this is the single assembly both now call, so the two
// dispatch paths cannot drift from each other on precedence or on a
// warning's wording.
//
// caller is the log-message prefix ("DispatchAgentStart" or
// "DispatchAgentRestart"). site is the caller's mint site, mintSiteStart or
// mintSiteRestart; it names the site in the mint audit record and gives the
// verb of the secrets-resolution failure message, the one warning whose
// wording differs (agent will <verb> without injected secrets) between the
// two callers. Any other site is an error before any work is done. It
// returns an error only for such a site, when the agent's project cannot be
// loaded (checked before any token is minted), or when the agent token is
// not issued.
func (d *HTTPAgentDispatcher) buildStartEnv(ctx context.Context, agent *store.Agent, caller string, site mintSite) (startEnvResult, error) {
	var startedVerb string
	switch site {
	case mintSiteStart:
		startedVerb = "start"
	case mintSiteRestart:
		startedVerb = "restart"
	default:
		return startEnvResult{}, fmt.Errorf("%s: unsupported mint site %q for a start", caller, site)
	}
	resolvedEnv := make(map[string]string)
	var envClassifications map[string]api.EnvKind

	// An agent written before ExplicitTimezone existed may still carry TZ in
	// its env records; adopt it as a pin before the env is copied below.
	adoptLegacyTZ(agent.AppliedConfig)

	// Start with agent's applied config env (template/config-level vars).
	if agent.AppliedConfig != nil {
		for k, v := range agent.AppliedConfig.Env {
			resolvedEnv[k] = v
		}
		classifyEnvKeys(&envClassifications, agent.AppliedConfig.Env, api.EnvKindPlain)
	}

	injectModelEnv(resolvedEnv, agent.AppliedConfig)
	if _, ok := resolvedEnv["SCION_MODEL"]; ok {
		classifyEnv(&envClassifications, "SCION_MODEL", api.EnvKindPlain)
	}
	injectThinkingLevelEnv(resolvedEnv, agent.AppliedConfig)
	if _, ok := resolvedEnv["SCION_THINKING_LEVEL"]; ok {
		classifyEnv(&envClassifications, "SCION_THINKING_LEVEL", api.EnvKindPlain)
	}

	// Merge env vars from Hub storage; storage vars fill in keys not already
	// set (with a non-empty value) by explicit config env vars.
	// Empty-value config entries are passthrough markers — storage values
	// should override them so that hub-stored secrets (API keys, etc.) are
	// available to the agent.
	envFromStorage, envFromStoragePlain, err := d.resolveEnvFromStorage(ctx, agent)
	if err != nil {
		if d.debug {
			d.log.Warn(caller+": failed to resolve env from storage", "error", err)
		}
	} else if len(envFromStorage) > 0 {
		for k, v := range envFromStorage {
			if k == agentTZEnvKey {
				continue // the resolver owns TZ
			}
			if existing, exists := resolvedEnv[k]; !exists || existing == "" {
				resolvedEnv[k] = v
				if envFromStoragePlain[k] {
					classifyEnv(&envClassifications, k, api.EnvKindPlain)
				} else {
					classifyEnv(&envClassifications, k, api.EnvKindSecretFetchable)
				}
			}
		}
	}

	// Resolve type-aware secrets and inject environment-type secrets.
	resolvedSecrets, _, err := d.resolveSecrets(ctx, agent)
	resolvedSecrets = d.dropTZTargetedSecrets(ctx, agent, resolvedSecrets)
	if err != nil {
		d.log.ErrorContext(ctx, caller+": failed to resolve secrets; agent will "+startedVerb+" without injected secrets",
			"agent_id", agent.ID, "error", err)
	} else {
		for _, s := range resolvedSecrets {
			if (s.Type == "environment" || s.Type == "") && s.Target != "" {
				if existing, exists := resolvedEnv[s.Target]; !exists || existing == "" {
					resolvedEnv[s.Target] = s.Value
					classifyEnv(&envClassifications, s.Target, api.EnvKindSecretFetchable)
				}
			}
		}
	}

	// TZ comes only from the agent TZ resolver, read live on every start
	// and restart so a storage or hub-default edit reaches unpinned agents.
	setResolvedAgentTZ(resolvedEnv, &envClassifications, d.resolveAgentTZ(ctx, agent, false))

	// Include agent identity and hub connectivity so the container can
	// report status to the Hub. The createAgent path sets these via the
	// request body, but the startAgent/restartAgent path on the broker
	// doesn't — so we inject them here as resolved env vars.
	if agent.ID != "" {
		resolvedEnv["SCION_AGENT_ID"] = agent.ID
		classifyEnv(&envClassifications, "SCION_AGENT_ID", api.EnvKindPlain)
	}
	if agent.ProjectID != "" {
		resolvedEnv["SCION_PROJECT_ID"] = agent.ProjectID
		classifyEnv(&envClassifications, "SCION_PROJECT_ID", api.EnvKindPlain)
	}
	if agent.Slug != "" {
		resolvedEnv["SCION_AGENT_SLUG"] = agent.Slug
		classifyEnv(&envClassifications, "SCION_AGENT_SLUG", api.EnvKindPlain)
	}
	// Include hub endpoint so the broker can inject it into the container.
	// The createAgent path sends this as req.HubEndpoint, but the
	// startAgent/restartAgent path relies on the broker's own config which
	// may be empty for standalone brokers. Including it here ensures the
	// broker always has the endpoint.
	if ep := d.effectiveAgentHubEndpoint(); ep != "" {
		resolvedEnv["SCION_HUB_ENDPOINT"] = ep
		classifyEnv(&envClassifications, "SCION_HUB_ENDPOINT", api.EnvKindPlain)
	}
	// Include hub name so agents can label their Cloud Logging entries with
	// the hub identity, matching the hub-scoped log query filter (labels.hub).
	if d.hubName != "" {
		resolvedEnv["SCION_HUB_NAME"] = d.hubName
		classifyEnv(&envClassifications, "SCION_HUB_NAME", api.EnvKindPlain)
	}

	// Inject canonical workspace sharing mode and git-ness so the broker can
	// surface them in the container env on the start/restart path. The
	// createAgent path carries these via WorkspaceMode in the request body;
	// the startAgent/restartAgent path relies on resolvedEnv injection (this
	// block) following the existing SCION_AGENT_ID / SCION_METADATA_MODE
	// pattern.
	//
	// Resolve once so the switch below uses canonical constants —
	// unrecognized or future wire labels safely fall back to shared-plain
	// behavior.
	//
	// A project lookup error fails the start/restart here, before any agent
	// or transport token is minted below, so there is nothing to revoke.
	projectInfo, err := d.resolveDispatchProjectInfo(ctx, agent)
	if err != nil {
		return startEnvResult{}, err
	}
	resolvedMode := store.ResolveWorkspaceSharingMode(projectInfo.workspaceMode)
	if projectInfo.workspaceMode != "" {
		resolvedEnv["SCION_WORKSPACE_MODE"] = string(resolvedMode)
		classifyEnv(&envClassifications, "SCION_WORKSPACE_MODE", api.EnvKindPlain)
	}
	wsSpec := workspaceSpecFor(agent, projectInfo.workspaceMode)
	switch resolvedMode {
	case store.SharingModeClonePerAgent, store.SharingModeWorktreePerAgent:
		resolvedEnv["SCION_WORKSPACE_GIT"] = "true"
		classifyEnv(&envClassifications, "SCION_WORKSPACE_GIT", api.EnvKindPlain)
	case store.SharingModeEmptyPerAgent:
		// Never git: a private, initially empty directory. Kept as an
		// explicit case so it cannot fall into the shared-plain GitClone
		// sniff below (design #2703 §2.3).
	case store.SharingModeSharedPlain:
		// For shared-plain, git-ness is detected from the applied GitClone config.
		// Note: broker-local linked projects where the workspace is already a
		// git repo on disk but has no HTTPS GitClone config cannot be detected
		// as git-backed here. The broker's on-disk util.IsGitRepoDir check in
		// buildStartContext covers this for the create path; on start/restart paths
		// SCION_WORKSPACE_GIT will be absent for such workspaces. This is an
		// acknowledged limitation noted in the design doc.
		if wsSpec.GitClone != nil {
			resolvedEnv["SCION_WORKSPACE_GIT"] = "true"
			classifyEnv(&envClassifications, "SCION_WORKSPACE_GIT", api.EnvKindPlain)
		}
	}

	// Inject GCP identity env vars so the broker can configure the
	// metadata-server sidecar correctly on (re-)start. During the
	// createAgent path this information travels inside CreateAgentConfig,
	// but the startAgent/restartAgent path doesn't carry that struct, so we
	// surface the values through resolvedEnv instead.
	//
	// This write is unconditional and always the last word on
	// SCION_METADATA_MODE (identity vars are set after the storage/secrets
	// merge above, at "highest precedence" per the comment on SCION_AGENT_ID
	// et al.): when the agent has no GCP identity configured at all, the mode
	// still needs to be authoritatively set to the secure default rather than
	// left for whatever a lower-precedence merge put in resolvedEnv.
	gcpMetadataMode := store.GCPMetadataModeBlock
	if agent.AppliedConfig != nil {
		if gcpID := agent.AppliedConfig.GCPIdentity; gcpID != nil {
			gcpMetadataMode = gcpID.MetadataMode
			if gcpMetadataMode == store.GCPMetadataModeAssign {
				resolvedEnv["SCION_METADATA_SA_EMAIL"] = gcpID.ServiceAccountEmail
				classifyEnv(&envClassifications, "SCION_METADATA_SA_EMAIL", api.EnvKindPlain)
				resolvedEnv["SCION_METADATA_PROJECT_ID"] = gcpID.ProjectID
				classifyEnv(&envClassifications, "SCION_METADATA_PROJECT_ID", api.EnvKindPlain)
			}
			// RequireLocalRuntime doesn't travel inside CreateAgentConfig on
			// this path either (see above), so surface it the same way: the
			// broker re-checks a hub-default-granted passthrough against the
			// runtime it resolves for this (re)start and downgrades to block
			// itself if that runtime turns out not to be a local container
			// runtime. Absent when false, matching this env's own convention
			// — cleared, not just left unset, so that only this grant, not a
			// value merged in above from stored env or a secret (both fill
			// absent keys only; resolvedEnv itself is rebuilt fresh on every
			// dispatch), can set it.
			if gcpID.RequireLocalRuntime {
				resolvedEnv["SCION_METADATA_REQUIRE_LOCAL_RUNTIME"] = "true"
				classifyEnv(&envClassifications, "SCION_METADATA_REQUIRE_LOCAL_RUNTIME", api.EnvKindPlain)
			} else {
				delete(resolvedEnv, "SCION_METADATA_REQUIRE_LOCAL_RUNTIME")
			}
		}
	}
	resolvedEnv["SCION_METADATA_MODE"] = gcpMetadataMode
	classifyEnv(&envClassifications, "SCION_METADATA_MODE", api.EnvKindPlain)
	// See buildCreateRequest for why this marker travels alongside the mode.
	resolvedEnv["SCION_METADATA_MODE_SOURCE"] = "hub"
	classifyEnv(&envClassifications, "SCION_METADATA_MODE_SOURCE", api.EnvKindPlain)

	// Generate a fresh agent token for Hub authentication. A mint error
	// stops the start or restart before any broker request; nothing was
	// minted in that case, so tokenIssued stays false.
	tokenIssued := false
	if d.tokenGenerator != nil {
		token, err := mintAgentTokenAt(ctx, d.tokenGenerator, d.store, agent, site)
		if err != nil {
			return startEnvResult{}, fmt.Errorf("%s: %w", caller, err)
		}
		if token != "" {
			resolvedEnv["SCION_AUTH_TOKEN"] = token
			tokenIssued = true
			// Bootstrap: NOT in argv. Diverted to ~/.scion/scion-token by
			// pkg/agent/run.go:761-777; read by pkg/hubsync/sync.go:1329.
			classifyEnv(&envClassifications, "SCION_AUTH_TOKEN", api.EnvKindSecretBootstrap)
		}
	}

	// Transport token minting for platform-layer auth (IAP / Cloud Run invoker).
	if d.transportMinter != nil && d.transportAudience != "" {
		tToken, tExpiry, tErr := d.transportMinter.MintIDToken(ctx, d.transportAudience)
		if tErr != nil {
			if d.debug {
				d.log.Warn(caller+": failed to mint transport token", "error", tErr)
			}
		} else if tToken != "" {
			resolvedEnv["SCION_TRANSPORT_TOKEN"] = tToken
			// Bootstrap: IN argv. No diversion exists. Google-signed OIDC, 1h,
			// lifetime NOT boundable (GenerateIdTokenRequest has no Lifetime field).
			classifyEnv(&envClassifications, "SCION_TRANSPORT_TOKEN", api.EnvKindSecretBootstrap)
			resolvedEnv["SCION_TRANSPORT_AUDIENCE"] = d.transportAudience
			classifyEnv(&envClassifications, "SCION_TRANSPORT_AUDIENCE", api.EnvKindPlain)
			resolvedEnv["SCION_TRANSPORT_TOKEN_EXPIRY"] = tExpiry.UTC().Format(time.RFC3339)
			classifyEnv(&envClassifications, "SCION_TRANSPORT_TOKEN_EXPIRY", api.EnvKindPlain)
			if d.transportMode != "" {
				resolvedEnv["SCION_TRANSPORT_MODE"] = d.transportMode
				classifyEnv(&envClassifications, "SCION_TRANSPORT_MODE", api.EnvKindPlain)
			}
		}
	}

	d.injectLifecycleGitHubToken(ctx, agent, resolvedEnv, &envClassifications, caller)

	return startEnvResult{
		env:             resolvedEnv,
		classifications: envClassifications,
		secrets:         resolvedSecrets,
		storageEnvCount: len(envFromStorage),
		projectInfo:     projectInfo,
		workspace:       wsSpec,
		tokenIssued:     tokenIssued,
	}, nil
}

// DispatchAgentStart starts an agent on the runtime broker. When resume is
// true, the harness is asked to continue its prior session (e.g. Claude
// --continue) instead of starting a fresh conversation. The hub is the source
// of truth for resume: callers compute it from the agent's stored phase
// (suspended → resume).
func (d *HTTPAgentDispatcher) DispatchAgentStart(ctx context.Context, agent *store.Agent, task string, resume bool) (err error) {
	ctx, span := tracer.Start(ctx, "hub.dispatch.start")
	defer span.End()
	span.SetAttributes(
		attribute.String("scion.agent.id", agent.ID),
		attribute.String("scion.broker.id", agent.RuntimeBrokerID),
	)

	// Start guard: no broker call
	// while a create launch is in flight, or after one did not complete.
	if err := d.launchGuardError(ctx, agent, "DispatchAgentStart"); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	if err := requireRuntimeBrokerAssigned(agent); err != nil {
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	endpoint, err := d.getProvisioningBrokerEndpoint(ctx, agent)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	// Capture the phase and launch-error this dispatch found the agent in,
	// before buildStartEnv/applyBrokerResponse can change either. The
	// revoke-arming decision below must reflect this call's starting point:
	// the lifecycle Start action also dispatches here for an
	// already-running agent (resume-in-place, and a start called again on a
	// running agent per the handler's own comment), and the user-facing
	// Restart action is stop-then-start and tolerates a failed stop, so by
	// the time this start leg runs the container may still be up even
	// though Restart never changed agent.Phase away from "running" before
	// calling in.
	priorPhase := agent.Phase
	priorLaunchError := agent.LaunchError

	// If no explicit task provided, fall back to the agent's applied config
	// task. Skip this on a pure resume (no new message): the harness should
	// just continue its prior session rather than be re-handed the original
	// creation task. A wake-with-message still passes that message as task.
	if task == "" && !resume && agent.AppliedConfig != nil {
		task = agent.AppliedConfig.Task
	}

	// Assemble the resolved env (shared with DispatchAgentRestart; see
	// buildStartEnv).
	startEnv, err := d.buildStartEnv(ctx, agent, "DispatchAgentStart", mintSiteStart)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	resolvedEnv := startEnv.env
	envClassifications := startEnv.classifications
	resolvedSecrets := startEnv.secrets
	projectInfo := startEnv.projectInfo
	projectPath := projectInfo.projectPath
	projectSlug := projectInfo.projectSlug

	// A failure from here on must revoke the credential buildStartEnv just
	// minted above, but only when it actually minted one — buildStartEnv
	// carries on without a credential when no tokenGenerator is configured,
	// and a mint error has already returned above, before any broker request
	// and before this defer is armed, having minted nothing to revoke. Arm the
	// revoke only when all of these hold:
	//  - priorPhase (captured above, before buildStartEnv/applyBrokerResponse
	//    could change it) is a confirmed non-running phase
	//    (isConfirmedNonRunningPhase: created, provisioning, stopped,
	//    suspended, error, or unset). Every other phase is excluded,
	//    including "running" and the transitional "starting"/"stopping":
	//    the lifecycle Start action can dispatch here against an
	//    already-running agent (resume-in-place, and a start called again on
	//    a running agent), the user-facing Restart action is stop-then-start
	//    and tolerates a failed stop, wake_dm leaves an agent in "starting"
	//    after a readiness-wait timeout with its container possibly still
	//    up, and reincarnate_worker.go writes "stopping" before its own stop
	//    dispatch has confirmed the container is actually gone. In every one
	//    of these cases the container this start leg targets may, in fact,
	//    still be up, and revoking by agent would take down the live
	//    container's own credential over a dispatch failure that says
	//    nothing about that container's health. See isConfirmedNonRunningPhase's
	//    doc comment for the empty-phase case.
	//  - priorLaunchError is not one the Hub declared on silence rather than
	//    on broker confirmation (isUnconfirmedLaunchError): the same risk
	//    applies there, since the prior credential may still be in use by a
	//    container the Hub has simply lost contact with. See that
	//    function's doc comment.
	revokeArmed := isConfirmedNonRunningPhase(priorPhase) && !isUnconfirmedLaunchError(priorLaunchError) && startEnv.tokenIssued
	defer func() {
		if err != nil && revokeArmed {
			revokeAgentCredentialsBestEffort(ctx, d.store, agent.ID, agentCredentialRevokeReasonStartFailed)
		}
	}()

	if d.debug {
		configEnvCount := 0
		if agent.AppliedConfig != nil {
			configEnvCount = len(agent.AppliedConfig.Env)
		}
		d.log.Debug("DispatchAgentStart: env resolution summary",
			"configEnvCount", configEnvCount,
			"storageEnvCount", startEnv.storageEnvCount,
			"totalResolvedEnv", len(resolvedEnv),
		)
	}

	// Use agent name as identifier (runtime broker uses name or ID)
	// Pass the agent's harness config so the broker starts with the correct harness.
	harnessConfig := ""
	if agent.AppliedConfig != nil {
		harnessConfig = agent.AppliedConfig.HarnessConfig
	}

	// Thread HarnessConfigID and HarnessConfigHash so the broker can resolve the
	// harness config via hash-verified hydration instead of falling back to the
	// potentially-stale on-disk search (fix for #1667).
	var harnessConfigID, harnessConfigHash string
	if agent.AppliedConfig != nil {
		harnessConfigID = agent.AppliedConfig.HarnessConfigID
		harnessConfigHash = agent.AppliedConfig.HarnessConfigHash
	}

	// Thread through updated InlineConfig so the broker can apply config
	// changes (e.g. max_turns) made after initial provisioning.
	var inlineConfig *api.ScionConfig
	if agent.AppliedConfig != nil {
		inlineConfig = agent.AppliedConfig.InlineConfig
	}

	// TODO(#1350): Thread envClassifications to broker via client.StartAgent.
	// PRECONDITION for P3b: without this, the broker receives nil (state 3,
	// "classification unavailable") on the start path. P3b must not land
	// until #1350 is done — otherwise fail-closed + nil map = total outage.
	// This includes SCION_GIT_CLONE_URL: the broker classifies it as
	// EnvKindSecretInjected on its own side (start_context.go), but that
	// classification does not reach the Hub until #1350 wires it back.
	_ = envClassifications // avoid unused-variable error until #1350 wire threading

	// Carry the same dispatch metadata the create path sends so that a
	// re-provision reached via start (e.g. after the broker deletes a stale
	// agent dir) can resolve required skills exactly as create does (#1960):
	// the same PreResolvedSkills the Hub resolves as the agent's creator (so
	// non-public hub-registry skills are never resolved with the broker's own
	// identity), the same project-scope ProvisionCredentials for gh:// skill
	// resolution, and the owning user's ID for user-scope resolution.
	// PreResolvedSkills always resolves as the agent's creator regardless of
	// who is dispatching this start (ptone/scion#1994), so the resolved set
	// for a given agent is the same whether the creator, an admin, or a
	// project owner starts it.
	extras := StartExtras{
		HubEndpoint:          d.effectiveAgentHubEndpoint(),
		UserID:               agent.OwnerID,
		ProvisionCredentials: d.resolveProvisionCredentials(ctx, agent, "DispatchAgentStart"),
		Workspace:            startEnv.workspace,
		HubAgentDefaults:     startHubAgentDefaults(d.autoExposePortsDefault()),
	}
	if d.creatorSkillPreResolver != nil {
		extras.PreResolvedSkills = d.creatorSkillPreResolver(ctx, agent)
	}

	resp, err := d.client.StartAgent(ctx, agent.RuntimeBrokerID, endpoint, agent.Slug, agent.ProjectID, task, projectPath, projectSlug, harnessConfig, harnessConfigID, harnessConfigHash, resolvedEnv, resolvedSecrets, inlineConfig, projectInfo.sharedDirs, projectInfo.sharedWorkspace, resume, extras)
	if isHashMismatchError(err) {
		if repairErr := d.repairHashMismatch(ctx, agent, err); repairErr == nil {
			resp, err = d.client.StartAgent(ctx, agent.RuntimeBrokerID, endpoint, agent.Slug, agent.ProjectID, task, projectPath, projectSlug, harnessConfig, harnessConfigID, harnessConfigHash, resolvedEnv, resolvedSecrets, inlineConfig, projectInfo.sharedDirs, projectInfo.sharedWorkspace, resume, extras)
		}
	}
	if errors.Is(err, ErrLifecycleDeferred) {
		// Cross-node hand-off: the owning node dispatches its own
		// DispatchAgentStart, which mints its own credential and carries its
		// own revoke-on-failure defer above. deferredStart only carries
		// Task/Resume, never the credential minted by this node's
		// buildStartEnv call, so that credential is never the one the
		// eventually-started container uses. Disarm this node's defer before
		// waiting on the cross-node result: a timeout or a closed event
		// channel here says nothing about whether the owner's own start (and
		// its own credential) succeeded, and this node revoking by agent on
		// that ambiguity can only take down a credential the owner is
		// actively relying on. The credential this node minted and never
		// sent is simply left alone — not revoked, not reused — and stays
		// valid until its own TTL expires.
		revokeArmed = false
		return d.deferredStart(ctx, agent, &StartDispatchArgs{
			Task:   task,
			Resume: resume,
		})
	}
	if err != nil && !isConfirmedStartNotActedOnError(err) {
		// The failure does not fall into one of the known-safe cases (the
		// request never reached the broker, or the broker explicitly
		// rejected it): it may be a timeout, a dropped connection, or a
		// response the broker did send that the Hub simply failed to read
		// or decode. Disarm the revoke the same way an unconfirmed
		// LaunchError does: the broker may already have started the
		// container the credential minted above belongs to. See
		// isConfirmedStartNotActedOnError's doc comment for the full list of
		// what counts as confirmed-safe and what does not.
		revokeArmed = false
	}
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	if resp != nil {
		d.applyBrokerResponse(ctx, agent, resp)
	} else {
		// The broker accepted the start without a parseable body; the
		// recorded target is stale all the same.
		d.forgetRuntimeTarget(ctx, agent)
	}
	return nil
}

// DispatchAgentStop stops an agent on the runtime broker.
func (d *HTTPAgentDispatcher) DispatchAgentStop(ctx context.Context, agent *store.Agent) error {
	if err := requireRuntimeBrokerAssigned(agent); err != nil {
		return err
	}
	ctx = withRecordedRuntime(ctx, agent.Runtime)

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
	// Start guard.
	if err := d.launchGuardError(ctx, agent, "DispatchAgentRestart"); err != nil {
		return err
	}
	if err := requireRuntimeBrokerAssigned(agent); err != nil {
		return err
	}
	ctx = withRecordedRuntime(ctx, agent.Runtime)

	endpoint, err := d.getProvisioningBrokerEndpoint(ctx, agent)
	if err != nil {
		return err
	}

	// Build resolved env with all env vars, secrets, and a fresh auth token
	// so the restarted container has full credentials and Hub connectivity.
	// This mirrors the resolution in DispatchAgentStart — without it, env vars
	// like GOOGLE_CLOUD_PROJECT are missing and auth provisioning fails.
	startEnv, err := d.buildStartEnv(ctx, agent, "DispatchAgentRestart", mintSiteRestart)
	if err != nil {
		return err
	}
	resolvedEnv := startEnv.env
	envClassifications := startEnv.classifications

	// TODO(#1350): Thread envClassifications to broker via client.RestartAgent.
	// PRECONDITION for P3b: without this, the broker receives nil (state 3,
	// "classification unavailable") on the restart path. P3b must not land
	// until #1350 is done — otherwise fail-closed + nil map = total outage.
	_ = envClassifications // avoid unused-variable error until #1350 wire threading

	// Carry the same dispatch metadata as DispatchAgentStart (see comment
	// there) so a re-provision reached via restart resolves required skills
	// exactly as create does (#1960), always as the agent's creator
	// regardless of who is dispatching this restart (ptone/scion#1994).
	extras := StartExtras{
		HubEndpoint:          d.effectiveAgentHubEndpoint(),
		UserID:               agent.OwnerID,
		ProvisionCredentials: d.resolveProvisionCredentials(ctx, agent, "DispatchAgentRestart"),
		HubAgentDefaults:     startHubAgentDefaults(d.autoExposePortsDefault()),
	}
	if d.creatorSkillPreResolver != nil {
		extras.PreResolvedSkills = d.creatorSkillPreResolver(ctx, agent)
	}

	err = d.client.RestartAgent(ctx, agent.RuntimeBrokerID, endpoint, agent.Slug, agent.ProjectID, resolvedEnv, extras)
	if errors.Is(err, ErrLifecycleDeferred) {
		return d.deferredRestart(ctx, agent)
	}
	if err == nil {
		d.forgetRuntimeTarget(ctx, agent)
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
	ctx = withRecordedRuntime(ctx, agent.Runtime)

	endpoint, err := d.getBrokerEndpoint(ctx, agent.RuntimeBrokerID)
	if err != nil {
		return err
	}

	var token string
	if d.tokenGenerator != nil {
		token, err = mintAgentTokenAt(ctx, d.tokenGenerator, d.store, agent, mintSiteResetAuth)
		if err != nil {
			return fmt.Errorf("DispatchAgentResetAuth: failed to generate agent token: %w", err)
		}
	}
	if token == "" {
		return fmt.Errorf("DispatchAgentResetAuth: no token generated for agent %s", agent.ID)
	}

	// Also push a fresh transport token when the hub mints them, so a
	// reset recovers an agent whose transport token has already expired
	// (its own refresh cannot reach the hub through the platform guard).
	// A mint failure does not block the app-token reset.
	var transportToken string
	if d.transportMinter != nil && d.transportAudience != "" {
		tToken, _, tErr := d.transportMinter.MintIDToken(ctx, d.transportAudience)
		if tErr != nil {
			d.log.Warn("DispatchAgentResetAuth: failed to mint transport token; resetting app token only",
				"agent_id", agent.ID, "error", tErr)
		} else {
			transportToken = tToken
		}
	}

	return d.client.ResetAuthAgent(ctx, agent.RuntimeBrokerID, endpoint, agent.Slug, agent.ProjectID, token, transportToken)
}

// DispatchAgentDelete deletes an agent from the runtime broker.
func (d *HTTPAgentDispatcher) DispatchAgentDelete(ctx context.Context, agent *store.Agent, deleteFiles, removeBranch, softDelete bool, deletedAt time.Time) error {
	if err := requireRuntimeBrokerAssigned(agent); err != nil {
		return err
	}
	ctx = withRecordedRuntime(ctx, agent.Runtime)

	endpoint, err := d.getBrokerEndpoint(ctx, agent.RuntimeBrokerID)
	if err != nil {
		return err
	}

	// For a linked project, tell the broker where the project lives on its
	// filesystem so it can find a file-only agent (container gone) there.
	// The broker checks the path's project identity before using it.
	// Best-effort: delete proceeds without a project path on lookup failure.
	if info, _ := d.resolveDispatchProjectInfo(ctx, agent); info.projectPath != "" {
		ctx = withDeleteProjectPath(ctx, info.projectPath)
	}

	err = d.client.DeleteAgent(ctx, agent.RuntimeBrokerID, endpoint, agent.Slug, agent.ProjectID, deleteFiles, removeBranch, softDelete, deletedAt)
	if errors.Is(err, ErrLifecycleDeferred) {
		return d.deferredDelete(ctx, agent, deleteFiles, removeBranch, softDelete, deletedAt)
	}
	return err
}

// ErrRawDispatchRefused is returned by DispatchAgentMessage when the
// message being dispatched carries Raw == true. This is a documented
// contract invariant every AgentDispatcher.DispatchAgentMessage
// implementation must uphold (today exactly this one production
// implementation exists, per contract §6.1(a)); nothing in the Go type
// system enforces it on a future second implementation, so a reviewer
// adding one must apply the same check at its own chokepoint.
//
// From task 2.3 onward (contract .design/agent-keys-contract.md §6.1 "(a)
// Dispatch-layer backstop"), legacy raw keystroke delivery through the
// message/broadcast/DM dispatch path is refused unconditionally at this
// chokepoint -- zero broker calls, never mgr.MessageRaw -- regardless of
// which call site reached it (direct dispatch, dispatchWithBrokerRetry, or
// any broker-proxy-published message). A correctly operating Hub never
// produces a Raw==true structuredMsg at this layer: the message-handler
// bridge (task 2.3, agent_keys_message_bridge.go) and ptone/scion#2218's
// (task 0.2's) ingress guards both intercept raw before persistence or
// dispatch on every production ingress. Reaching this point therefore
// signals an implementation defect in one of those layers, not a normal
// caller error (contract's AK-55).
//
// This layer cannot undo a persisted store.Message row or an already-
// published SSE/observer event: on every call site, those side effects (if
// any) already happened before dispatch runs. Returning this error is
// deliberately just an ordinary dispatch error to the caller -- every
// existing caller already marks a persisted row failed through its own
// existing failure path (e.g. ExecuteAgentDM's markFailed) and returns a
// generic, non-keys-specific failure to any synchronous caller -- so this
// chokepoint requires no new error-handling code path anywhere else.
var ErrRawDispatchRefused = errors.New("agent dispatch: raw message delivery refused (post-2.3 backstop)")

// DispatchAgentMessage sends a message to an agent on the runtime broker.
func (d *HTTPAgentDispatcher) DispatchAgentMessage(ctx context.Context, agent *store.Agent, message string, interrupt bool, structuredMsg *messages.StructuredMessage) error {
	// A nil agent has no broker to deliver to; reject it before the raw
	// backstop log below dereferences agent.ID.
	if agent == nil {
		return requireRuntimeBrokerAssigned(agent)
	}
	if structuredMsg != nil && structuredMsg.Raw {
		slog.Error("agent dispatch: raw message delivery refused at the backstop",
			"agent_id", agent.ID, "defect", "raw_reached_dispatch_layer")
		return ErrRawDispatchRefused
	}

	if err := requireRuntimeBrokerAssigned(agent); err != nil {
		return err
	}
	ctx = withRecordedRuntime(ctx, agent.Runtime)

	endpoint, err := d.getBrokerEndpoint(ctx, agent.RuntimeBrokerID)
	if err != nil {
		return err
	}

	return d.client.MessageAgent(ctx, agent.RuntimeBrokerID, endpoint, agent.Slug, agent.ProjectID, message, interrupt, structuredMsg)
}

// DispatchAgentKeys implements agentkeys.Dispatcher: it looks up the target
// broker's endpoint (the same store lookup DispatchAgentMessage already
// performs via getBrokerEndpoint — the only store access this method
// performs), builds the internal agentkeys.BrokerRequest from target plus
// this call's own operationID/executeBefore/keys arguments, and hands it to
// the configured broker client's agentkeys.BrokerClient implementation. It
// does not re-resolve or re-authorize target: the caller (task 2.2's
// ExecuteAgentKeys) passes the already-resolved, already-authorized facts it
// gathered, per .design/agent-keys-contract.md §4.4.
//
// If the configured client does not implement agentkeys.BrokerClient (a
// wiring defect, not a runtime condition — every production client this task
// ships does), or the broker endpoint lookup itself fails, this is reported
// as agentkeys.ErrNotDispatched: both failures are proven, Hub-side, before
// any request could have reached a broker.
func (d *HTTPAgentDispatcher) DispatchAgentKeys(ctx context.Context, target agentkeys.Target, operationID string, executeBefore time.Time, keys string) (agentkeys.BrokerResult, error) {
	// Fail closed on a missing or already-past deadline before spending a
	// network round trip on it: BrokerRequest.ExecuteBefore's doc requires a
	// zero value to fail closed, and contract §4.3 lists "the Hub's own
	// pre-send check found ExecuteBefore already past" as an ErrNotDispatched
	// source. The broker enforces this independently at admission (task
	// 1.1) — this is a cheap, redundant guard, not a substitute for that.
	if executeBefore.IsZero() || !time.Now().Before(executeBefore) {
		return agentkeys.BrokerResult{}, fmt.Errorf("%w: execute-before deadline is zero or already past", agentkeys.ErrNotDispatched)
	}
	// An empty operationID would make decodeBrokerKeysResponse's success-path
	// echo check ("OperationID == expectedOperationID") vacuous — an empty
	// echo would satisfy an empty expectation, silently discarding the audit
	// correlation the echo check exists to enforce. The operation ID is a
	// mandatory body field per contract §4.1; a caller (task 2.2) always
	// mints one before calling this method, so an empty value here is a
	// caller bug, proven before any request could be built.
	if operationID == "" {
		return agentkeys.BrokerResult{}, fmt.Errorf("%w: operation ID is required", agentkeys.ErrNotDispatched)
	}
	// An empty AgentSlug builds a path like "/api/v1/agents//keys": the
	// broker's mux would answer with a path-clean redirect, which keysClient
	// correctly does not follow, so this would otherwise surface as an
	// uncertain "may have run" outcome for a request that never reached a
	// handler. An empty AgentID, ProjectID or RuntimeBrokerID are caller bugs
	// too — task 2.2 always passes these fields from an already-resolved
	// *store.Agent — and are proven-empty before any request is built, the
	// same standard as the deadline and operation-ID guards above. Checking
	// RuntimeBrokerID here too (rather than only implicitly via the
	// getBrokerEndpoint call below) fails fast and avoids an unnecessary
	// store read for an already-known-invalid target. This is an
	// input-shape check, not re-resolution or re-authorization of target, so
	// it does not conflict with the "Dispatcher does not re-resolve" rule in
	// contract §4.4.
	if target.AgentSlug == "" || target.AgentID == "" || target.ProjectID == "" || target.RuntimeBrokerID == "" {
		return agentkeys.BrokerResult{}, fmt.Errorf("%w: target agent slug, agent ID, project ID and runtime broker ID are all required", agentkeys.ErrNotDispatched)
	}

	keysClient, ok := d.client.(agentkeys.BrokerClient)
	if !ok {
		return agentkeys.BrokerResult{}, fmt.Errorf("%w: configured broker client does not support keys dispatch", agentkeys.ErrNotDispatched)
	}

	endpoint, err := d.getBrokerEndpoint(ctx, target.RuntimeBrokerID)
	if err != nil {
		return agentkeys.BrokerResult{}, fmt.Errorf("%w: %w", agentkeys.ErrNotDispatched, err)
	}

	ctx = withRecordedRuntime(ctx, target.Runtime)
	req := agentkeys.BrokerRequest{
		ProjectID:     target.ProjectID,
		AgentID:       target.AgentID,
		OperationID:   operationID,
		ExecuteBefore: executeBefore.UTC(),
		Keys:          keys,
	}
	return keysClient.ExecuteKeys(ctx, target.RuntimeBrokerID, endpoint, target.AgentSlug, req)
}

// DispatchAgentLogs retrieves agent.log content from the runtime broker.
func (d *HTTPAgentDispatcher) DispatchAgentLogs(ctx context.Context, agent *store.Agent, tail int) (string, error) {
	if err := requireRuntimeBrokerAssigned(agent); err != nil {
		return "", err
	}
	ctx = withRecordedRuntime(ctx, agent.Runtime)

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
	ctx = withRecordedRuntime(ctx, agent.Runtime)

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
	ctx = withRecordedRuntime(ctx, agent.Runtime)

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
	err := d.deferredLifecycle(ctx, agent, "start", args, isStartTerminal)
	return d.deferredLaunchGuardError(ctx, agent, "deferredStart", err)
}

// deferredStop handles a cross-node agent stop.
func (d *HTTPAgentDispatcher) deferredStop(ctx context.Context, agent *store.Agent) error {
	return d.deferredLifecycle(ctx, agent, "stop", &StopDispatchArgs{}, isStopTerminal)
}

// deferredRestart handles a cross-node agent restart.
func (d *HTTPAgentDispatcher) deferredRestart(ctx context.Context, agent *store.Agent) error {
	err := d.deferredLifecycle(ctx, agent, "restart", &RestartDispatchArgs{}, isStartTerminal)
	return d.deferredLaunchGuardError(ctx, agent, "deferredRestart", err)
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
	setBrokerDispatchInitiator(ctx, dispatch)
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
	// Delete operations use a shorter timeout (dispatchDeleteTimeout) since
	// they are lightweight broker-side operations and should not block the
	// caller for 90 seconds — unless the caller passed an explicit budget
	// with withDeleteWaitBudget (the delete engine, design ptone/scion#2483
	// §2.3.1), which then is the wait's only deadline.
	var timeoutOverrides []time.Duration
	if op == "delete" {
		timeoutOverrides = append(timeoutOverrides, deleteWaitTimeoutFn(ctx))
	}
	result, err := waitForDispatchDone(ctx, eventCh, unsub, d.store, dispatchID, timeoutOverrides...)
	if err != nil {
		return nil, err
	}
	if result.State == store.DispatchStateFailed {
		return nil, dispatchFailureError(result)
	}
	return result, nil
}

// deferredLifecycle is the common flow for cross-node start/stop/restart:
//  1. Mint the dispatch ID, then subscribe to agent.<id>.status and
//     broker.dispatch.<dispatchID>.done BEFORE writing intent, so no event
//     for the row can be missed and the row cannot be terminal yet
//  2. InsertBrokerDispatch with serialized resolved args
//  3. Best-effort SignalBrokerCmd (the row is durable; reconnect-drain backstop)
//  4. waitForLifecycleOutcome: nil on the op's success phase; the row's
//     failure (the broker's typed error when recorded) once the row fails;
//     ErrDispatchFailed on timeout; a generic error on an error phase with
//     no failed row
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

	dispatchID := uuid.NewString()

	// 1. Subscribe BEFORE writing intent so we don't miss events.
	eventCh, unsub := d.events.Subscribe("agent."+agent.ID+".status", "broker.dispatch."+dispatchID+".done")

	// 2. Serialize args and insert the durable intent row.
	argsJSON, err := MarshalDispatchArgs(args)
	if err != nil {
		unsub()
		return fmt.Errorf("marshal dispatch args: %w", err)
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
	setBrokerDispatchInitiator(ctx, dispatch)
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

	// 4. Wait for the outcome.
	return waitForLifecycleOutcome(ctx, eventCh, unsub, d.store, dispatchID, op, terminal)
}

// resolveSecrets queries secrets from all applicable scopes and merges them
// into a flat list. Higher scopes override lower:
//
//	runtime_broker  <  hub  <  project  <  user
//
// This matches envScopePrecedence (see above). The divergence previously
// tracked in issue #624 was corrected in PR #1227.
func (d *HTTPAgentDispatcher) resolveSecrets(ctx context.Context, agent *store.Agent) ([]ResolvedSecret, []string, error) {
	if d.secretBackend == nil {
		if d.debug {
			d.log.Debug("resolveSecrets: secretBackend is nil, skipping secret resolution")
		}
		return nil, nil, nil
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
		// The synthetic identity must carry the agent's real scopes.
		// Agent authority is derived from JWT scopes (buildAgentSyntheticBindings),
		// and agentScopeRestriction denies everything when the scope list is
		// empty, so an identity built without them can never be allowed.
		role, additionalScopes := agentRoleAndScopes(agent)
		scopes := append(ScopesForRole(role), additionalScopes...)
		if d.debug {
			d.log.Debug("resolveSecrets: progeny resolution enabled",
				"agent_id", agentID, "ancestry_len", len(ancestry),
				"role", string(role), "scopes", len(scopes))
		}
		resolveOpts = &secret.ResolveOpts{
			AgentAncestry: ancestry,
			AuthzCheck: func(s secret.SecretMeta) bool {
				ident := &agentIdentityWrapper{
					AgentTokenClaims: &AgentTokenClaims{
						Claims:    jwt.Claims{Subject: agentID},
						ProjectID: agent.ProjectID,
						Ancestry:  ancestry,
						Scopes:    scopes,
					},
				}
				// Name the permission explicitly. (resource="secret",
				// action="read") has no registry entry - agent secret access is
				// registered as project.secret_read on ResourceProject - so
				// resolveResourcePermission cannot resolve the pair on its own.
				decision := d.authzService.Decide(ctx, AuthzRequest{
					Principal:  principalContextForIdentity(ident),
					Credential: credentialContextForIdentity(ident),
					Resource:   Resource{Type: "secret", ID: s.ID},
					Action:     ActionRead,
					Permission: permissionProjectSecretRead,
				})
				if !decision.Allowed && d.debug {
					d.log.Debug("progeny secret denied by authz",
						"agent_id", agentID, "secret", s.Name, "secret_id", s.ID,
						"reason", decision.Reason, "scopes", len(scopes))
				}
				return decision.Allowed
			},
		}
	}

	resolved, err := d.secretBackend.Resolve(ctx, agent.OwnerID, agent.ProjectID, agent.RuntimeBrokerID, resolveOpts)
	if err != nil {
		return nil, nil, err
	}
	result := make([]ResolvedSecret, 0, len(resolved))
	var asNeededKeys []string
	for _, sv := range resolved {
		// Defense in depth for rows that predate the create/patch reserved-
		// target check, or that reached the store through some other path:
		// drop any environment-type secret whose target is reserved for
		// scion's own control-plane env vars before it is attached to the
		// dispatch request at all, in either injection mode.
		if (sv.SecretType == store.SecretTypeEnvironment || sv.SecretType == "") && secret.IsReservedEnvTarget(sv.Target) {
			if d.debug {
				d.log.Debug("resolveSecrets: dropping reserved-target secret", "name", sv.Name, "target", sv.Target, "scope", sv.Scope)
			}
			continue
		}
		// Only skip as_needed environment-type secrets (handled by the
		// two-pass env-gather flow). File-type and variable-type secrets
		// should always be placed regardless of injection mode — the
		// as_needed concept does not apply to them.
		if sv.InjectionMode == store.InjectionModeAsNeeded && (sv.SecretType == store.SecretTypeEnvironment || sv.SecretType == "") {
			// Collect the target key name so the broker's autodetect can
			// consider it when selecting auth type (closes #1447).
			target := sv.Target
			if target == "" {
				target = sv.Name
			}
			if target != "" {
				asNeededKeys = append(asNeededKeys, target)
			}
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
		d.log.Debug("resolveSecrets: resolved secrets", "count", len(result), "names", names,
			"asNeededKeys", asNeededKeys)
	}
	return result, asNeededKeys, nil
}

// classifyEnv sets the classification for an env key in the given map,
// initialising the map on the pointer if nil. This is the write-side helper
// for the parallel classification map (#127, P3a). Every site that writes
// a key into ResolvedEnv must also call classifyEnv (or classifyEnvKeys for
// bulk operations) so that no key is unclassified when P3a lands.
func classifyEnv(m *map[string]api.EnvKind, key string, kind api.EnvKind) {
	if *m == nil {
		*m = make(map[string]api.EnvKind)
	}
	(*m)[key] = kind
}

// classifyEnvKeys sets the classification for every key in a map.
func classifyEnvKeys(m *map[string]api.EnvKind, keys map[string]string, kind api.EnvKind) {
	if len(keys) == 0 {
		return
	}
	if *m == nil {
		*m = make(map[string]api.EnvKind)
	}
	for k := range keys {
		(*m)[k] = kind
	}
}

// shouldPersistResolvedEnvKey decides whether a key from req.ResolvedEnv is
// allowed into the durable AppliedConfig.Env record. This is an allowlist,
// not a denylist: a key is persisted only when it is explicitly classified
// api.EnvKindPlain. Every other outcome -- classified as any non-plain kind,
// or simply absent from the classifications map -- is rejected. GITHUB_TOKEN
// is rejected unconditionally regardless of its classification, since it is
// never a legitimate value to keep in the durable config record.
//
// This mirrors the three-state lookup documented on api.EnvKind: a nil map
// and an "absent key in a non-nil map" both fail the check here, which is
// the correct fail-closed behavior for a persistence gate (contrast with a
// gate that would treat "nil map" as "trust everything" -- that would be
// backwards for this call site).
func shouldPersistResolvedEnvKey(key string, classifications map[string]api.EnvKind) bool {
	if key == "GITHUB_TOKEN" {
		return false
	}
	// TZ is resolved on every dispatch and recorded only as
	// ExplicitTimezone; persisting it into AppliedConfig.Env would turn a
	// resolved value into a stale env record.
	if key == agentTZEnvKey {
		return false
	}
	if strings.HasPrefix(key, "SCION_") {
		return false
	}
	kind, ok := api.ClassifyEnvKey(classifications, key)
	return ok && kind == api.EnvKindPlain
}

type deleteProjectPathKey struct{}

// withDeleteProjectPath attaches the broker-local project path (a linked
// project's provider LocalPath) to a delete request context. The broker
// transports send it as the projectPath query parameter. It is carried on the
// context rather than as a parameter to keep the RuntimeBrokerClient
// interface unchanged.
func withDeleteProjectPath(ctx context.Context, projectPath string) context.Context {
	return context.WithValue(ctx, deleteProjectPathKey{}, projectPath)
}

// deleteProjectPathQuery returns "&projectPath=<escaped>" if a project path
// was attached with withDeleteProjectPath, otherwise "".
func deleteProjectPathQuery(ctx context.Context) string {
	if pp, _ := ctx.Value(deleteProjectPathKey{}).(string); pp != "" {
		return "&projectPath=" + url.QueryEscape(pp)
	}
	return ""
}

type recordedRuntimeKey struct{}

// dispatchRecordedRuntime returns the runtime type to send to the broker for
// an agent whose store record carries runtime: the broker-reported runtime
// type, or "" when there is none or the agent is a managed agent (those have
// no broker runtime).
func dispatchRecordedRuntime(runtime string) string {
	runtime = strings.TrimSpace(runtime)
	if runtime == "" || isManagedAgentRuntime(runtime) {
		return ""
	}
	return runtime
}

// withRecordedRuntime attaches the agent's recorded runtime type to an
// existing-agent broker request context (ptone/scion#2748). The broker
// transports send it as the api.RecordedRuntimeQueryParam query parameter,
// and the broker then looks for the agent only in runtimes of that type,
// answering 503 with Retry-After when none is registered instead of looking
// in its default runtime. Like withDeleteProjectPath it is carried on the
// context to keep the RuntimeBrokerClient interface unchanged; an empty
// runtime leaves ctx unchanged and the broker behaves as before.
func withRecordedRuntime(ctx context.Context, runtime string) context.Context {
	if rt := dispatchRecordedRuntime(runtime); rt != "" {
		return context.WithValue(ctx, recordedRuntimeKey{}, rt)
	}
	return ctx
}

// recordedRuntimeParam returns "runtime=<escaped>" if a recorded runtime was
// attached with withRecordedRuntime, otherwise "".
func recordedRuntimeParam(ctx context.Context) string {
	if rt, _ := ctx.Value(recordedRuntimeKey{}).(string); rt != "" {
		return api.RecordedRuntimeQueryParam + "=" + url.QueryEscape(rt)
	}
	return ""
}

// withRecordedRuntimeQuery appends the recorded runtime parameter (if any) to
// a raw query string, as used by the control channel transport.
func withRecordedRuntimeQuery(ctx context.Context, query string) string {
	param := recordedRuntimeParam(ctx)
	switch {
	case param == "":
		return query
	case query == "":
		return param
	default:
		return query + "&" + param
	}
}

// withRecordedRuntimeURL appends the recorded runtime parameter (if any) to a
// full request URL, as used by the HTTP transport.
func withRecordedRuntimeURL(ctx context.Context, endpoint string) string {
	param := recordedRuntimeParam(ctx)
	if param == "" {
		return endpoint
	}
	if strings.Contains(endpoint, "?") {
		return endpoint + "&" + param
	}
	return endpoint + "?" + param
}
