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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/agentkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/gcp"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/templatecache"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

var tracer = otel.Tracer("scion-broker")

// matchesAgent checks whether an agent matches the given id and optional projectID.
// When projectID is provided, it must match for uniqueness across projects.
// When projectID is empty, matching falls back to name/containerID/slug only
// (backward compatible with solo/CLI mode and pre-existing containers).
func matchesAgent(a api.AgentInfo, id, projectID string) bool {
	nameMatch := a.Name == id || a.ContainerID == id || a.Slug == id
	if !nameMatch {
		return false
	}
	if projectID == "" {
		return true
	}
	return matchesAgentProject(a, projectID)
}

func matchesAgentProject(a api.AgentInfo, projectID string) bool {
	// Check the runtime's project_id label first, then the ProjectID field.
	if labelProjectID := projectkeys.ProjectIDFromLabels(a.Labels); labelProjectID != "" {
		return labelProjectID == projectID
	}
	if a.ProjectID != "" {
		return a.ProjectID == projectID
	}
	// No project_id on container — match anyway for backward compatibility
	// with containers created before project_id labeling was added.
	return true
}

// ============================================================================
// Health Endpoints
// ============================================================================

// GetHealthInfo returns the current health status of the Runtime Broker server.
// This can be called directly by co-located components (e.g., the WebServer)
// to build composite health responses without making an HTTP round-trip.
func (s *Server) GetHealthInfo(ctx context.Context) *HealthResponse {
	checks := make(map[string]string)

	// Check runtime availability
	if s.runtime != nil {
		checks[s.runtime.Name()] = "available"
	} else {
		checks["runtime"] = "unavailable"
	}

	status := "healthy"
	for _, v := range checks {
		if v != "available" && v != "healthy" {
			status = "degraded"
			break
		}
	}

	// NFS mount health is always reported per share. It degrades the
	// overall status only when the broker owns the mounts and a dispatch
	// would be refused: see nfsHealthDegradesStatus.
	if s.nfsMountReconciler != nil {
		checks["nfs_mounts"] = s.nfsMountReconciler.HealthCheckString()
		if s.nfsHealthDegradesStatus() {
			status = degradeHealthStatus(status)
		}
	}

	return &HealthResponse{
		Status:  status,
		Version: s.version,
		Uptime:  time.Since(s.startTime).Round(time.Second).String(),
		Checks:  checks,
	}
}

// degradeHealthStatus lowers a healthy status to degraded. Any other
// status (already degraded, or worse) is returned unchanged, so a
// degrading check never raises a worse status back to degraded.
func degradeHealthStatus(status string) string {
	if status == "healthy" {
		return "degraded"
	}
	return status
}

// nfsHealthDegradesStatus reports whether an unhealthy NFS share should
// mark the broker degraded. It does only when the broker mounts the shares
// itself (auto_mount on, and the default runtime is not Kubernetes or Cloud
// Run, where the platform mounts the export and dispatch is not gated), the
// first reconcile pass has finished (a pending check is not a failure), and
// a share is unhealthy. Otherwise the broker only verifies mounts it does
// not manage, so a missing mount is reported in nfs_mounts without changing
// the overall status.
func (s *Server) nfsHealthDegradesStatus() bool {
	r := s.nfsMountReconciler
	if r == nil || !r.MountsShares() || r.IsHealthy() {
		return false
	}
	if s.nfsStartupReconcileDone != nil {
		select {
		case <-s.nfsStartupReconcileDone:
		default:
			return false // first pass still pending
		}
	}
	return true
}

// NFSWarnOnlyRuntime reports whether name is a runtime type on which the
// platform, not the broker, mounts the NFS export into the agent: the
// Kubernetes family (the kubelet mounts the volume) and the Cloud Run
// family. For these the broker never mounts a share and never refuses a
// dispatch over NFS state; it logs a warning instead. When it is the
// broker's default runtime the broker only verifies its shares, and
// scion doctor reports an unmounted share as a warning.
func NFSWarnOnlyRuntime(name string) bool {
	if isKubernetesRuntimeName(name) {
		return true
	}
	switch name {
	case "cloudrun", "cloudrun-instances", "cloudrun-sandbox":
		return true
	}
	return false
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	resp := s.GetHealthInfo(r.Context())
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	// Check if we have a functional runtime
	if s.runtime == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "not_ready",
			"reason": "no runtime available",
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"status": "ready",
	})
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	runtimeType := "unknown"
	if s.runtime != nil {
		runtimeType = s.runtime.Name()
	}

	resp := BrokerInfoResponse{
		BrokerID: s.config.BrokerID,
		Name:     s.config.BrokerName,
		Version:  s.version,
		Capabilities: &BrokerCapabilities{
			WebPTY: false, // TODO: Implement WebSocket PTY
			Sync:   true,
			// Attach reflects the default runtime's own capability
			// (scionrt.HasAttachSupport) rather than a blanket true, so a
			// runtime that opts out via the optional AttachCapableRuntime
			// interface is reported accurately here too.
			Attach:      scionrt.HasAttachSupport(s.runtime),
			Exec:        true,
			Reprovision: true,
			AsyncLaunch: true,
		},
		Profiles: s.buildInfoProfiles(runtimeType),
	}

	writeJSON(w, http.StatusOK, resp)
}

// isLocalOnlyRuntime returns true for runtime types that require a local daemon
// or hardware and cannot function in a hosted cloud environment.
func isLocalOnlyRuntime(runtimeType string) bool {
	switch runtimeType {
	case "docker", "podman", "container":
		return true
	}
	return false
}

// buildInfoProfiles enumerates configured profiles from effective settings.
// Falls back to a single "default" profile when no profiles are configured.
//
// Each profile's advertised Type is its resolved runtime type
// (VersionedSettings.ResolveRuntime: the runtime entry's explicit type,
// else its runtimes-map key), not the map key itself, so a profile whose
// runtime entry is keyed differently from its type (e.g. an entry
// "k8s-staging" of type kubernetes) is advertised, and filtered, as the
// type it actually runs.
func (s *Server) buildInfoProfiles(defaultRuntimeType string) []BrokerProfile {
	vs, _, err := config.LoadEffectiveSettings("")
	if err != nil || len(vs.Profiles) == 0 {
		return []BrokerProfile{
			{Name: "default", Type: defaultRuntimeType, Available: true, Attach: s.attachSupportedForProfile(defaultRuntimeType, defaultRuntimeType)},
		}
	}

	names := make([]string, 0, len(vs.Profiles))
	for name := range vs.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)

	var profiles []BrokerProfile
	for _, name := range names {
		rtType, rtCfg := resolveInfoProfileRuntime(vs, name, defaultRuntimeType)

		if !isLocalOnlyRuntime(defaultRuntimeType) && isLocalOnlyRuntime(rtType) {
			continue
		}

		profiles = append(profiles, BrokerProfile{
			Name:      name,
			Type:      rtType,
			Available: true,
			Context:   rtCfg.Context,
			Namespace: rtCfg.Namespace,
			Attach:    s.attachSupportedForProfile(rtType, defaultRuntimeType),
		})
	}

	if len(profiles) == 0 {
		return []BrokerProfile{
			{Name: "default", Type: defaultRuntimeType, Available: true, Attach: s.attachSupportedForProfile(defaultRuntimeType, defaultRuntimeType)},
		}
	}

	return profiles
}

// resolveLiveRuntimeInstance returns the already-built Runtime instance
// backing a profile resolving to rtType, without constructing anything new:
// s.runtime for the default type, or an auxiliary runtime some prior
// request already built and cached for that type (findAuxiliaryRuntimeByType
// — auxiliaryRuntimes is keyed by resolved IDENTITY, not type, so more than
// one instance can share rtType; this picks the lexicographically smallest
// identity, the same deterministic-but-coarse choice ForceRuntime's lookup
// makes). ok is false when no live instance exists yet for rtType — most
// commonly an auxiliary runtime type no request has resolved yet — and
// callers must leave the capability unknown in that case rather than guess
// from the type string alone (a named profile on a broker with a different
// default type is not "probably fine" just because it's not the default
// type).
func (s *Server) resolveLiveRuntimeInstance(rtType, defaultRuntimeType string) (rt scionrt.Runtime, ok bool) {
	if rtType == defaultRuntimeType {
		if s.runtime == nil {
			return nil, false
		}
		return s.runtime, true
	}
	aux, found := s.findAuxiliaryRuntimeByType(rtType)
	if !found || aux.Runtime == nil {
		return nil, false
	}
	return aux.Runtime, true
}

// attachSupportedForProfile reports whether a profile resolving to rtType
// supports attach, asking resolveLiveRuntimeInstance for the live instance
// that actually backs it. nil means unknown (no live instance to ask, per
// resolveLiveRuntimeInstance) — callers, and every consumer of the
// resulting BrokerProfile.Attach, must read nil as supported.
func (s *Server) attachSupportedForProfile(rtType, defaultRuntimeType string) *bool {
	rt, ok := s.resolveLiveRuntimeInstance(rtType, defaultRuntimeType)
	if !ok {
		return nil
	}
	v := scionrt.HasAttachSupport(rt)
	return &v
}

// resolveInfoProfileRuntime returns the runtime type and runtime config
// buildInfoProfiles advertises for the named profile:
//
//   - a profile naming a runtime entry that exists resolves through
//     VersionedSettings.ResolveRuntime (explicit type, else the map key);
//   - a profile naming no runtime uses the broker's default runtime type,
//     with that type's runtimes-map entry if one exists;
//   - a profile naming a runtime entry that doesn't exist falls back to the
//     name as given, with no runtime config.
func resolveInfoProfileRuntime(vs *config.VersionedSettings, name, defaultRuntimeType string) (string, config.V1RuntimeConfig) {
	key := vs.Profiles[name].Runtime
	if key == "" {
		return defaultRuntimeType, vs.Runtimes[defaultRuntimeType]
	}
	if rtCfg, rtType, err := vs.ResolveRuntime(name); err == nil {
		return rtType, rtCfg
	}
	return key, config.V1RuntimeConfig{}
}

// handleHubConnections returns live status of all hub connections.
func (s *Server) handleHubConnections(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	s.hubMu.RLock()
	defer s.hubMu.RUnlock()

	mode := "single-hub"
	if len(s.hubConnections) > 1 {
		mode = "multi-hub"
	}

	connections := make([]HubConnectionInfo, 0, len(s.hubConnections))
	for _, conn := range s.hubConnections {
		info := HubConnectionInfo{
			Name:              conn.Name,
			HubEndpoint:       conn.HubEndpoint,
			BrokerID:          conn.BrokerID,
			AuthMode:          string(conn.AuthMode),
			Status:            string(conn.GetStatus()),
			IsColocated:       conn.IsColocated,
			HasHeartbeat:      conn.Heartbeat != nil,
			HasControlChannel: conn.ControlChannel != nil,
		}
		connections = append(connections, info)
	}

	resp := HubConnectionStatusResponse{
		Connections: connections,
		Mode:        mode,
	}

	writeJSON(w, http.StatusOK, resp)
}

// ============================================================================
// Agent Endpoints
// ============================================================================

func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listAgents(w, r)
	case http.MethodPost:
		s.createAgent(w, r)
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	query := r.URL.Query()

	filter := map[string]string{
		"scion.agent": "true",
	}

	// Add an optional project filter.
	if projectID := query.Get("projectId"); projectID != "" {
		filter["scion.project_id"] = projectID
	}
	if status := query.Get("status"); status != "" {
		filter["status"] = status
	}

	agents, err := s.manager.List(ctx, filter)
	if err != nil {
		RuntimeError(w, "Failed to list agents: "+err.Error())
		return
	}

	// Also list agents from auxiliary runtimes (e.g. Kubernetes), in a
	// deterministic (identity-sorted) order.
	auxRuntimes := s.sortedAuxiliaryRuntimes()

	// Dedup by name+projectID to prevent collision across projects while still
	// deduplicating the same agent found on multiple runtimes.
	agentKey := func(a api.AgentInfo) string {
		pid := a.ProjectID
		if pid == "" {
			pid = projectkeys.ProjectIDFromLabels(a.Labels)
		}
		return a.Name + "\x00" + pid
	}
	seen := make(map[string]bool)
	for _, ag := range agents {
		seen[agentKey(ag)] = true
	}
	for _, aux := range auxRuntimes {
		auxAgents, auxErr := aux.Manager.List(ctx, filter)
		if auxErr != nil {
			continue
		}
		for _, ag := range auxAgents {
			k := agentKey(ag)
			if !seen[k] {
				seen[k] = true
				agents = append(agents, ag)
			}
		}
	}

	// Convert to API response format
	responses := make([]AgentResponse, 0, len(agents))
	for _, agent := range agents {
		responses = append(responses, AgentInfoToResponse(agent))
	}

	// Apply pagination
	limit := 50
	if l := query.Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 {
			limit = parsed
		}
	}

	totalCount := len(responses)
	if len(responses) > limit {
		responses = responses[:limit]
	}

	writeJSON(w, http.StatusOK, ListAgentsResponse{
		Agents:     responses,
		TotalCount: totalCount,
	})
}

// skillResolverInputs bundles the dispatch-provided fields needed to attach a
// working skill resolver to a provisioning context. Populated from the
// create request body on the create path and from the start/restart request
// bodies (plus the URL-scoped projectID) on those paths, so every path that
// can reach ProvisionAgent carries the same resolver (#1960).
type skillResolverInputs struct {
	// HubEndpoint is used only as a fallback for rewriting relative
	// pre-resolved-skill URLs when the Hub connection doesn't already carry
	// its own endpoint.
	HubEndpoint string
	// ResolvedEnv supplies the default GITHUB_TOKEN used by the GitHub skill
	// resolver and as the install-phase credential for gh:// skill downloads.
	ResolvedEnv map[string]string
	// ProvisionCredentials supplies additional named GitHub credentials
	// (gh:// convention tokens like GH_{OWNER}) for private-repo skill
	// resolution. Never forwarded to the agent container environment.
	ProvisionCredentials map[string]string
	// PreResolvedSkills carries the Hub-registry skills the Hub resolved at
	// dispatch as the agent's creator (#1784); the broker's own identity
	// cannot read non-public skills.
	PreResolvedSkills *hubclient.ResolveSkillsResponse
	// ProjectID and UserID scope resolution of project/user-scope skill URIs.
	ProjectID string
	UserID    string
}

// attachSkillResolver builds the hub/GitHub/GCP skill resolver router
// (wrapped in the caching resolver and, when the Hub pre-resolved
// creator-scoped skills, the outermost PreResolvedSkillResolver) and installs
// it — along with the GitHub token and project/user IDs used during
// resolution — onto ctx. Returns ctx unchanged when there is neither a usable
// Hub connection nor pre-resolved skills, matching the pre-#1960 create
// behavior for that case.
//
// Shared by createAgent, startAgent, and restartAgent: every path that can
// reach ProvisionAgent must carry the same resolver, or a required gh://
// skill fails closed with "no skill resolver available" on retry (#1960).
func (s *Server) attachSkillResolver(ctx context.Context, r *http.Request, in skillResolverInputs) context.Context {
	conn := s.resolveHubConnection(r)
	if conn != nil && conn.HubClient != nil {
		hubResolver := agent.NewHubSkillResolver(conn.HubClient.Skills())
		defaultGHToken := in.ResolvedEnv["GITHUB_TOKEN"]
		ghResolver := agent.NewGitHubSkillResolverWithCredentials(defaultGHToken, in.ProvisionCredentials, s.ghResolutionCache)

		// GCP resolver uses Hub API for registry alias lookup.
		registrySvc := conn.HubClient.SkillRegistries()
		gcpLookup := func(ctx context.Context, name string) (*agent.RegistryLookupResult, error) {
			reg, err := registrySvc.Get(ctx, name)
			if err != nil {
				return nil, err
			}
			if reg == nil {
				return nil, fmt.Errorf("registry %q not found", name)
			}
			return &agent.RegistryLookupResult{
				Name:     reg.Name,
				Endpoint: reg.Endpoint,
				Type:     reg.Type,
				Status:   reg.Status,
			}, nil
		}

		router := buildSkillRouter(hubResolver, ghResolver, agent.NewGCPSkillResolver(gcpLookup))

		var resolver agent.SkillResolver = router
		if s.skCache != nil {
			resolver = agent.NewCachingSkillResolver(resolver, s.skCache)
		}
		// Skills the Hub resolved at dispatch as the agent's creator are
		// served first: the broker's own identity cannot read non-public
		// skills (#1784). Outermost so pre-resolved results never enter the
		// broker's resolution cache.
		if in.PreResolvedSkills != nil {
			resolver = agent.NewPreResolvedSkillResolver(in.PreResolvedSkills, resolver, preResolvedHubEndpoint(conn, in.HubEndpoint))
		}
		ctx = agent.ContextWithSkillResolver(ctx, resolver)
		// Credentials for install-phase downloads, only ever sent to GitHub
		// hosts: the default credential for gh:// skills resolved by the Hub,
		// which returns raw.githubusercontent.com URLs but not the credential
		// behind them; and, for gh:// skills the GitHub resolver served from
		// its on-disk cache (no file content kept), a lookup of the same
		// credential that resolver uses for the ref.
		ctx = ghResolver.WithInstallCredentials(ctx, defaultGHToken)
		if in.ProjectID != "" {
			ctx = agent.ContextWithResolveProjectID(ctx, in.ProjectID)
		}
		if in.UserID != "" {
			ctx = agent.ContextWithResolveUserID(ctx, in.UserID)
		}
	} else if in.PreResolvedSkills != nil {
		// No usable Hub connection for this request, but the Hub already
		// resolved its skills: install those, and fail closed (per skill)
		// for anything it did not cover.
		ctx = agent.ContextWithSkillResolver(ctx,
			agent.NewPreResolvedSkillResolver(in.PreResolvedSkills, nil, preResolvedHubEndpoint(conn, in.HubEndpoint)))
		if in.ProjectID != "" {
			ctx = agent.ContextWithResolveProjectID(ctx, in.ProjectID)
		}
		if in.UserID != "" {
			ctx = agent.ContextWithResolveUserID(ctx, in.UserID)
		}
	}
	return ctx
}

// isSingleCleanPathElement reports whether name is safe to join onto a
// directory as exactly one path segment: no path separator or NUL byte,
// not "." or "..", and unchanged by filepath.Clean (which also rejects an
// empty string). Every agent-addressing path built from a request-supplied name
// in this package and in pkg/agent joins that name onto a root this way, so
// an identifier that fails this check must never reach one of those joins.
//
// Both '/' and '\' are rejected explicitly and unconditionally, regardless
// of GOOS: relying on os.PathSeparator would only reject '\' when built for
// Windows, letting a name containing '\' slip through on every other
// platform even though it is a path separator there.
//
// It does not decode or interpret name in any way — a value like "%2e%2e"
// is a literal, ordinary-looking directory name, and passes.
func isSingleCleanPathElement(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.ContainsRune(name, '/') || strings.ContainsRune(name, '\\') || strings.ContainsRune(name, 0) {
		return false
	}
	return filepath.Clean(name) == name
}

func (s *Server) createAgent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	createStart := time.Now()

	var req CreateAgentRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	ctx, span := tracer.Start(ctx, "broker.agent.create")
	defer span.End()
	span.SetAttributes(attribute.String("scion.agent.name", req.Name))

	// Validate required fields
	if req.Name == "" {
		ValidationError(w, "name is required", nil)
		return
	}
	// req.Name is joined onto a directory as a single path segment all the
	// way down (buildStartContext -> opts.Name -> GetAgentDir), so it must
	// be exactly that: reject anything that would join as more than one
	// segment, or as ".." / ".", before it reaches any of those joins.
	// GetAgentDir/GetAgent enforce the same constraint independently as the
	// backstop in front of their own stale-directory removal branch; this
	// rejects the same shape earlier, at the request boundary.
	if !isSingleCleanPathElement(req.Name) {
		ValidationError(w, "name must be a single path element", nil)
		return
	}

	// ProjectID reaches filesystem paths further on (the project-marker
	// block in buildStartContext, and worktree provisioning); an empty
	// value is a normal, valid case (not every deployment sends one), but a
	// non-empty value must be a single path element.
	if req.ProjectID != "" && !isSingleCleanPathElement(req.ProjectID) {
		ValidationError(w, "invalid projectId", nil)
		return
	}

	// ProjectSlug is joined onto the global projects directory below (to
	// resolve req.ProjectPath) and again later to compute the GCS-bootstrap
	// workspace directory, in both cases as a single path segment: a value
	// of ".." would resolve either join to the projects directory's own
	// parent (~/.scion itself), not a real per-project directory.
	if req.ProjectSlug != "" && !isSingleCleanPathElement(req.ProjectSlug) {
		ValidationError(w, "invalid projectSlug", nil)
		return
	}

	// Slug names the agent's launch marker file (launch_marker.go) on both
	// the synchronous and the async create path, joined onto the markers
	// directory as a single path segment. Empty is valid (Name is used
	// instead, and is checked above); a non-empty value must be a single
	// path element.
	if req.Slug != "" && !isSingleCleanPathElement(req.Slug) {
		ValidationError(w, "invalid slug", nil)
		return
	}

	agentKey := req.ID
	if agentKey == "" {
		agentKey = req.Name
	}

	var attempt *dispatchAttempt
	if req.RequestID != "" {
		s.dispatchAttemptsMu.Lock()
		newAttempt, existingAttempt := s.beginCreateAttempt(req.RequestID, agentKey)
		if existingAttempt != nil {
			switch existingAttempt.Status {
			case dispatchAttemptSucceeded:
				if existingAttempt.CreatedResponse != nil {
					status := existingAttempt.HTTPStatus
					if status == 0 {
						status = http.StatusCreated
					}
					respCopy := *existingAttempt.CreatedResponse
					s.dispatchAttemptsMu.Unlock()
					writeJSON(w, status, respCopy)
					return
				}
				if existingAttempt.EnvResponse != nil {
					respCopy := *existingAttempt.EnvResponse
					s.dispatchAttemptsMu.Unlock()
					writeJSON(w, http.StatusAccepted, respCopy)
					return
				}
			case dispatchAttemptInProgress:
				s.dispatchAttemptsMu.Unlock()
				writeError(w, http.StatusConflict, ErrCodeConflict, "create request already in progress", map[string]interface{}{
					"requestId": req.RequestID,
				})
				return
			case dispatchAttemptFailed:
				existingAttempt.Status = dispatchAttemptInProgress
				existingAttempt.Error = ""
				existingAttempt.UpdatedAt = time.Now()
				s.completeAttempt(existingAttempt, dispatchAttemptInProgress, 0, nil, nil, "")
				attempt = existingAttempt
			}
		} else {
			attempt = newAttempt
		}
		s.dispatchAttemptsMu.Unlock()
	}

	markAttemptFailed := func(httpStatus int, message string) {
		if attempt == nil {
			return
		}
		s.dispatchAttemptsMu.Lock()
		s.completeAttempt(attempt, dispatchAttemptFailed, httpStatus, nil, nil, message)
		s.dispatchAttemptsMu.Unlock()
	}

	// Debug log incoming request
	if s.config.Debug {
		s.agentLifecycleLog.Debug("Creating agent", "agent_id", req.ID, "project_id", req.ProjectID, "name", req.Name, "slug", req.Slug)
		s.agentLifecycleLog.Debug("Hub credentials",
			"agent_id", req.ID,
			"project_id", req.ProjectID,
			"hubEndpoint", req.HubEndpoint,
			"hasToken", req.AgentToken != "",
			"slug", req.Slug,
		)
		if req.Config != nil {
			s.agentLifecycleLog.Debug("Agent configuration",
				"agent_id", req.ID,
				"project_id", req.ProjectID,
				"template", req.Config.Template,
				"image", req.Config.Image,
				"templateID", req.Config.TemplateID,
			)
		}
	}

	// Resolve project path early for env-gather (needs settings access before buildStartContext)
	if req.ProjectSlug != "" && req.ProjectPath == "" {
		globalDir, err := config.GetGlobalDir()
		if err != nil {
			markAttemptFailed(http.StatusInternalServerError, "failed to resolve global dir")
			span.SetStatus(codes.Error, err.Error())
			RuntimeError(w, "Failed to get global dir: "+err.Error())
			return
		}
		req.ProjectPath = filepath.Join(globalDir, "projects", req.ProjectSlug)
	}

	// Env-gather: if GatherEnv is true, evaluate env completeness before building full context.
	// This needs the resolved project path and merged env to determine which keys are missing.
	if req.GatherEnv && !req.NoAuth {
		// Build a preliminary merged env for env-gather evaluation. Shared
		// with extractRequiredEnvKeys's own requestEnv so both checks start
		// from the identical presence-aware base (see buildRequestEnv).
		env := buildRequestEnv(req)

		// Hydrate a Hub-managed template before extracting required keys.
		// launch's own dispatch path (start_context.go) does the same
		// hydration and replaces opts.Template with the result before
		// ProvisionAgent resolves the harness-config template chain from
		// it — so scoring the on-disk slug here instead could score a
		// stale local template of the same name that launch will never
		// use. Unlike the harness-config hydration below, a failure here
		// is NOT a fall-back case: launch returns the same startContextError
		// mapping (hub_unreachable 503 / template_error 500, see
		// writeStartContextError) on a hydration error, so the preflight
		// must not be more lenient than launch by silently scoring a slug
		// launch would never actually reach. No timeout is applied here,
		// for the same reason: launch hydrates with the request context and
		// no cap (start_context.go), and a fixed cap shorter than a cold
		// download (DefaultHydratorConfig's DownloadTimeout is 5 minutes)
		// would fail creates here that launch would have completed.
		//
		// This hydration runs ahead of the global-project 409, the
		// harness-config policy refusal, and the NFS check below, so a
		// request those would otherwise reject can pay for a hub round trip
		// first. Kept in this order to sit next to the harness-config
		// hydration it mirrors; revisit if that ordering cost matters.
		var hydratedTemplatePath string
		if req.Config != nil && (req.Config.TemplateID != "" || req.Config.TemplateHash != "") {
			hubConn := s.resolveHubConnection(r)
			if hubConn != nil {
				tplPath, err := s.hydrateTemplate(ctx, req.Config, hubConn)
				if err != nil {
					sce := &startContextError{
						Status:      http.StatusInternalServerError,
						Message:     "Failed to hydrate template: " + err.Error(),
						IsHubError:  true,
						OriginalErr: err,
					}
					markAttemptFailed(http.StatusInternalServerError, sce.Message)
					span.SetStatus(codes.Error, sce.Message)
					writeStartContextError(w, sce)
					return
				}
				hydratedTemplatePath = tplPath
			}
		}

		// Hydrate hub-managed harness-config before extracting required keys
		// so that config-driven auth metadata is available during env-gather.
		// Graceful degradation: if hydration fails, fall back to on-disk only.
		var hydratedHCPath string
		if req.Config != nil && (req.Config.HarnessConfigID != "" || req.Config.HarnessConfigHash != "") {
			hubConn := s.resolveHubConnection(r)
			if hubConn != nil {
				hydrateCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				hcPath, err := s.hydrateHarnessConfig(hydrateCtx, req.Config, hubConn)
				cancel()
				if err != nil {
					if s.config.Debug {
						s.envSecretLog.Debug("Env-gather: harness-config hydration failed, falling back to on-disk",
							"error", err.Error(),
						)
					}
				} else {
					hydratedHCPath = hcPath
				}
			}
		}

		required, secretInfo, alternatives, settingsEnv := s.extractRequiredEnvKeys(req, hydratedTemplatePath, hydratedHCPath)
		// Fold in the settings/harness-config-directory env
		// extractRequiredEnvKeys resolved (its withDir, minus whatever
		// requestEnv already decided — see settingsEnvSatisfied there), but
		// only to fill genuine gaps: env above is requestEnv, which already
		// reflects what outranks settings/dir for the pod, so an existing
		// entry — even an empty one — must not be overwritten here.
		for k, v := range settingsEnv {
			if _, exists := env[k]; !exists {
				env[k] = v
			}
		}
		if s.config.Debug {
			s.envSecretLog.Debug("Env-gather: evaluating env completeness",
				"gatherEnv", req.GatherEnv,
				"projectPath", req.ProjectPath,
				"requiredKeys", len(required),
				"required", required,
			)
		}
		if len(required) > 0 {
			// Build lookup set of keys satisfied by resolved secrets
			secretTargets := make(map[string]struct{})
			for _, s := range req.ResolvedSecrets {
				if s.Type == "environment" || s.Type == "" {
					target := s.Target
					if target == "" {
						target = s.Name
					}
					if target != "" {
						secretTargets[target] = struct{}{}
					}
				}
				if s.Type == "file" {
					secretTargets[s.Name] = struct{}{}
				}
			}

			if s.config.Debug {
				targetKeys := make([]string, 0, len(secretTargets))
				for k := range secretTargets {
					targetKeys = append(targetKeys, k)
				}
				s.envSecretLog.Debug("Env-gather: resolved secret targets available",
					"secretTargetKeys", targetKeys,
					"resolvedSecretsCount", len(req.ResolvedSecrets),
				)
			}

			var hubHas, needs []string
			for _, key := range required {
				val, hasVal := env[key]
				if hasVal && val != "" {
					hubHas = append(hubHas, key)
				} else if _, fromSecret := secretTargets[key]; fromSecret {
					hubHas = append(hubHas, key)
				} else {
					needs = append(needs, key)
				}
			}

			if len(needs) > 0 {
				if s.config.Debug {
					s.envSecretLog.Debug("Env-gather: returning 202 with requirements",
						"required", required,
						"hubHas", hubHas,
						"needs", needs,
					)
				}

				// Build SecretInfo for needed keys only
				var respSecretInfo map[string]api.SecretKeyInfo
				for _, key := range needs {
					if info, ok := secretInfo[key]; ok {
						if respSecretInfo == nil {
							respSecretInfo = make(map[string]api.SecretKeyInfo)
						}
						respSecretInfo[key] = info
					}
				}

				// Build alternatives for needed keys only
				var respAlternatives map[string][]string
				for _, key := range needs {
					if alts, ok := alternatives[key]; ok {
						if respAlternatives == nil {
							respAlternatives = make(map[string][]string)
						}
						respAlternatives[key] = alts
					}
				}

				resp := EnvRequirementsResponse{
					AgentID:      agentKey,
					Required:     required,
					HubHas:       hubHas,
					Needs:        needs,
					SecretInfo:   respSecretInfo,
					Alternatives: respAlternatives,
				}
				if attempt != nil {
					s.dispatchAttemptsMu.Lock()
					s.completeAttempt(attempt, dispatchAttemptSucceeded, http.StatusAccepted, nil, &resp, "")
					s.dispatchAttemptsMu.Unlock()
				}
				writeJSON(w, http.StatusAccepted, resp)
				return
			}

			if s.config.Debug {
				s.envSecretLog.Debug("Env-gather: all required keys satisfied, proceeding with start",
					"required", required,
					"hubHas", hubHas,
				)
			}
		}
	}

	// Debug log project path
	if s.config.Debug && req.ProjectPath != "" {
		s.agentLifecycleLog.Debug("Using project path from Hub", "agent_id", req.ID, "path", req.ProjectPath)
	}

	// Reject global project in multi-hub mode
	if s.isMultiHubMode() && s.isGlobalProject(req.ProjectID, req.ProjectPath) {
		writeJSON(w, http.StatusConflict, map[string]interface{}{
			"error": map[string]string{
				"code":    "global_project_disabled",
				"message": "Global project is disabled when broker is connected to multiple hubs",
			},
		})
		return
	}

	// Phase 3 broker policy: refuse container-script harness dispatches
	// unless the broker has opted in. We check before buildStartContext so
	// the failure happens before the broker mounts project state, downloads
	// workspaces, or projects secrets.
	if name, entry, ok := s.lookupHarnessConfigForPolicy(req); ok {
		if d := s.evaluateHarnessConfigPolicy(name, entry); !d.OK {
			markAttemptFailed(d.HTTPStatus, d.Message)
			writeError(w, d.HTTPStatus, d.Code, d.Message, nil)
			return
		}
	}

	// N1-7: with nfs.auto_mount, ensure NFS shares are mounted before an
	// NFS-backed dispatch. No-op without NFS config, with auto_mount off, or
	// for a project that does not use the nfs workspace backend.
	nfsProfile := ""
	if req.Config != nil {
		nfsProfile = req.Config.Profile
	}
	if err := s.checkNFSForDispatch(r.Context(), req.Name, req.ProjectPath, req.ProjectSlug, nfsProfile); err != nil {
		markAttemptFailed(http.StatusServiceUnavailable, "NFS mount check failed: "+err.Error())
		span.SetStatus(codes.Error, "NFS workspace storage is not available: "+err.Error())
		writeError(w, http.StatusServiceUnavailable, "nfs_unavailable",
			"NFS workspace storage is not available: "+err.Error(), nil)
		return
	}

	// Design §3.4 Amendment A23.1 (review p1b-r1, R2): refuse a reprovision
	// request for a worktree-per-agent project before buildStartContext runs.
	// buildStartContext's tryProvisionWorktree finds or creates the agent's
	// worktree and sets opts.Workspace to its path with opts.GitClone left
	// nil -- exactly the explicit-mount shape Manager.Reprovision now
	// accepts, which would let a worktree-per-agent reprovision reach
	// ProvisionAgent's Case 1 despite that mode never having been designed
	// or reviewed for reincarnation (A23's contract excludes it). The Hub
	// already refuses worktree-per-agent before dispatching (A2/A4); this is
	// the broker's own independent defense, and it also keeps
	// tryProvisionWorktree off the reprovision path entirely.
	if req.Reprovision && req.WorkspaceMode == store.WorkspaceModeWorktreePerAgent {
		const msg = "reprovision refused: worktree-per-agent workspaces are not supported by reincarnate"
		// O-a (review p1b-r2): use the same dispatch-attempt message as the
		// sibling A4.2 refusal below, so dispatch-attempt consumers can match
		// one string regardless of which precondition refused the request.
		markAttemptFailed(http.StatusConflict, "reprovision refused")
		span.SetStatus(codes.Error, msg)
		Conflict(w, "Failed to provision agent: "+msg)
		return
	}

	// Build unified start context (project path, env, template, git-clone, secrets, manager)
	s.agentLifecycleLog.Info("Agent dispatch: pre-flight complete",
		"agent_id", req.ID, "name", req.Name, "elapsed", time.Since(createStart).String())
	buildCtxStart := time.Now()
	sc, err := s.buildStartContext(ctx, startContextInputs{
		Name:               req.Name,
		AgentID:            req.ID,
		Slug:               req.Slug,
		ProjectPath:        req.ProjectPath,
		ProjectSlug:        req.ProjectSlug,
		ProjectID:          req.ProjectID,
		Config:             req.Config,
		InlineConfig:       req.InlineConfig,
		SharedDirs:         req.SharedDirs,
		HubEndpoint:        req.HubEndpoint,
		AgentToken:         req.AgentToken,
		CreatorName:        req.CreatorName,
		ResolvedEnv:        req.ResolvedEnv,
		EnvClassifications: req.EnvClassifications,
		ResolvedSecrets:    req.ResolvedSecrets,
		NoAuth:             req.NoAuth,
		Attach:             req.Attach,
		WorkspaceMode:      req.WorkspaceMode,
		HTTPRequest:        r,
		Operation:          opCreate,
	})
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		status := writeStartContextError(w, err)
		markAttemptFailed(status, err.Error())
		return
	}
	opts := sc.Opts
	// Reincarnation (Reprovision) targets an existing agent's workspace, so
	// FreshProvision — which permits GetAgent to wipe and re-clone a leftover
	// populated workspace — must never be set for it, regardless of what
	// buildStartContext computed for opCreate.
	if req.Reprovision {
		opts.FreshProvision = false
	}
	s.agentLifecycleLog.Info("Agent dispatch: buildStartContext complete",
		"agent_id", req.ID, "name", req.Name, "elapsed", time.Since(buildCtxStart).String())

	// Inject skill resolver from Hub connection for skill provisioning.
	ctx = s.attachSkillResolver(ctx, r, skillResolverInputs{
		HubEndpoint:          req.HubEndpoint,
		ResolvedEnv:          req.ResolvedEnv,
		ProvisionCredentials: req.ProvisionCredentials,
		PreResolvedSkills:    req.PreResolvedSkills,
		ProjectID:            req.ProjectID,
		UserID:               req.UserID,
	})

	// Carry the hub's operational agent_defaults into provisioning. No-op when
	// the hub sent none, which is every local and file-mode dispatch.
	ctx = withHubAgentDefaults(ctx, req.Config)

	// Non-blocking create (design t1-async-create-v11.md §3.8.2, §7 P1b-1).
	// ProvisionOnly and Reprovision always stay synchronous (design §3.2).
	// With AsyncLaunch absent, no LaunchID to track the launch by, or a
	// LaunchTimeoutSeconds too small to leave any budget after the broker's
	// 20s abort margin (ctx' would already be expired when the 201 is sent),
	// fall back to the synchronous path rather than accept a launch that
	// cannot possibly succeed. Behavior is unchanged from here down for all
	// of these non-conforming cases.
	if req.AsyncLaunch && !req.ProvisionOnly && !req.Reprovision {
		switch {
		case req.LaunchID == "":
			s.agentLifecycleLog.Warn("async launch requested with no launchId; falling back to synchronous create",
				"agent_id", req.ID, "name", req.Name)
		case req.LaunchTimeoutSeconds <= minAsyncLaunchTimeoutSeconds:
			s.agentLifecycleLog.Warn("async launch requested with too small a launchTimeoutSeconds; falling back to synchronous create",
				"agent_id", req.ID, "name", req.Name, "launch_timeout_seconds", req.LaunchTimeoutSeconds)
		default:
			s.beginAsyncLaunch(w, r, ctx, req, opts, sc.Manager, attempt, markAttemptFailed, span, createStart)
			return
		}
	}

	// If WorkspaceStoragePath is set, download workspace from GCS (non-git
	// bootstrap). Factored into downloadWorkspaceFromGCS so the async-launch
	// path (runLaunch) performs exactly the same step, in its own goroutine
	// (design §3.1: "Launch is the GCS workspace download ... plus
	// Manager.Start, in a goroutine"). This only runs here on paths that fall
	// through the async gate above (flag absent, ProvisionOnly, Reprovision,
	// no LaunchID, or a too-small LaunchTimeoutSeconds), so an eligible async
	// create never downloads twice.
	if req.WorkspaceStoragePath != "" {
		var attemptMsg, httpMessage string
		var dlErr error
		opts, attemptMsg, httpMessage, dlErr = s.downloadWorkspaceFromGCS(ctx, req, opts)
		if dlErr != nil {
			span.SetStatus(codes.Error, dlErr.Error())
			if errors.Is(dlErr, errInvalidWorkspaceDir) {
				markAttemptFailed(http.StatusBadRequest, attemptMsg)
				BadRequest(w, httpMessage)
				return
			}
			markAttemptFailed(http.StatusInternalServerError, attemptMsg)
			RuntimeError(w, httpMessage)
			return
		}
	}

	// A full synchronous start registers itself under the agent's name and
	// takes the name's launch marker before provisioning anything, as an
	// async launch does (runLaunch). This runs after the GCS workspace
	// download above, so that download's own path validation is still the
	// first thing to touch the project directory. The registration lets a
	// delete or stop of the agent on this broker cancel the start, instead
	// of it running on (for example, blocked in skill resolution) and
	// failing minutes later; the marker lets the failure cleanup below
	// confirm it still owns the name before removing files addressed by
	// that name.
	var ss *syncStart
	if !req.ProvisionOnly {
		var startCtx context.Context
		var ssErr error
		startCtx, ss, ssErr = s.beginSyncStart(ctx, req, opts)
		if ssErr != nil {
			s.agentLifecycleLog.Error("Agent create failed before start",
				"agent_id", req.ID, "project_id", req.ProjectID,
				"name", req.Name, "slug", req.Slug, "error", ssErr)
			span.SetStatus(codes.Error, ssErr.Error())
			if errors.Is(ssErr, errInvalidLaunchSlug) {
				markAttemptFailed(http.StatusBadRequest, "invalid slug")
				ValidationError(w, "invalid slug", nil)
				return
			}
			markAttemptFailed(http.StatusInternalServerError, "failed to create agent")
			RuntimeError(w, "Failed to create agent: "+ssErr.Error())
			return
		}
		defer ss.finish()
		ctx = startCtx
	}

	// Branch based on provision-only flag
	if req.ProvisionOnly {
		// Provision only: set up dirs, worktree, templates without starting the container.
		// Reprovision (reincarnation) forces a fresh render of an existing agent's
		// config instead of reusing what's persisted — see Manager.Reprovision.
		var cfg *api.ScionConfig
		var err error
		if req.Reprovision {
			cfg, err = sc.Manager.Reprovision(ctx, opts)
		} else {
			cfg, err = sc.Manager.Provision(ctx, opts)
		}
		if err != nil {
			// Design §3.4 Amendment A4.2: Reprovision wraps every refusal in
			// agent.ErrReprovisionRefused (workspace preconditions, running-container
			// check). Surface those as 409 Conflict rather than a generic 500 so
			// callers — and the reincarnate worker's failure message — can tell
			// "refused to run" apart from an actual provisioning error.
			if errors.Is(err, agent.ErrReprovisionRefused) {
				markAttemptFailed(http.StatusConflict, "reprovision refused")
				span.SetStatus(codes.Error, err.Error())
				Conflict(w, "Failed to provision agent: "+err.Error())
				return
			}
			span.SetStatus(codes.Error, err.Error())
			if errors.Is(err, config.ErrHarnessConfigNotFound) || errors.Is(err, config.ErrTemplateNotFound) {
				markAttemptFailed(http.StatusNotFound, "failed to provision agent")
				writeError(w, http.StatusNotFound, ErrCodeNotFound, "Failed to provision agent: "+err.Error(), nil)
				return
			}
			// A required skill reference that could not be resolved is mapped
			// to the status matching its cause (404 not found, 429 rate
			// limited, 504 timeout, 502 upstream/unreachable) rather than a
			// blanket 500/502, so the caller gets an actionable response (#2546).
			var skillErr *agent.SkillResolutionError
			if errors.As(err, &skillErr) {
				markAttemptFailed(skillResolutionHTTPStatus(skillErr.Code), "failed to provision agent")
				SkillResolutionFailed(w, skillErr)
				return
			}
			markAttemptFailed(http.StatusInternalServerError, "failed to provision agent")
			RuntimeError(w, "Failed to provision agent: "+err.Error())
			return
		}

		s.agentLifecycleLog.Info("Agent provisioned",
			"agent_id", req.ID, "project_id", req.ProjectID,
			"name", req.Name, "slug", req.Slug,
			"reprovision", req.Reprovision,
			"phase", string(state.PhaseCreated))

		// Build a response with "created" status (no container launched)
		agentResp := &AgentResponse{
			ID:     req.ID,
			Slug:   req.Slug,
			Name:   req.Name,
			Status: string(state.PhaseCreated),
			Phase:  string(state.PhaseCreated),
		}
		if cfg != nil {
			agentResp.HarnessConfig = cfg.HarnessConfig
			agentResp.Image = cfg.Image
		}
		// Report the runtime this dispatch resolved to (its profile), not
		// the broker's default runtime: the hub records this value as the
		// agent's runtime.
		agentResp.RuntimeType = sc.RuntimeType
		if agentResp.RuntimeType == "" && s.runtime != nil {
			agentResp.RuntimeType = s.runtime.Name()
		}

		resp := CreateAgentResponse{
			Agent:   agentResp,
			Created: true,
			// Design §3.4 Amendment A2.2(a): echo Reprovision only when this branch actually
			// ran Manager.Reprovision, never merely because the request
			// asked for it — the hub's dispatch fails closed when it asked
			// for a reprovision and did not get this echo back.
			Reprovisioned: req.Reprovision,
		}
		if attempt != nil {
			s.dispatchAttemptsMu.Lock()
			s.completeAttempt(attempt, dispatchAttemptSucceeded, http.StatusCreated, &resp, nil, "")
			s.dispatchAttemptsMu.Unlock()
		}
		writeJSON(w, http.StatusCreated, resp)
		return
	}

	// Full start: provision and launch the container
	startOpStart := time.Now()
	agentInfo, err := sc.Manager.Start(ctx, opts)
	if err != nil {
		// An unresolvable named resource (harness-config or template) is a
		// naming problem the caller can act on, not an infrastructure
		// failure — track and report it as a 404 instead of folding it into
		// the generic 502 the hub maps RuntimeError to (ptone/scion#1316
		// fault 3).
		notFoundErr := errors.Is(err, config.ErrHarnessConfigNotFound) || errors.Is(err, config.ErrTemplateNotFound)
		// A required skill reference that could not be resolved is mapped to
		// the status matching its cause (404 not found, 429 rate limited,
		// 504 timeout, 502 upstream/unreachable) rather than a blanket
		// 500/502, so the caller gets an actionable response (#2546).
		var skillErr *agent.SkillResolutionError
		isSkillErr := errors.As(err, &skillErr)
		switch {
		case notFoundErr:
			markAttemptFailed(http.StatusNotFound, "failed to create agent")
		case isSkillErr:
			markAttemptFailed(skillResolutionHTTPStatus(skillErr.Code), "failed to create agent")
		default:
			markAttemptFailed(http.StatusInternalServerError, "failed to create agent")
		}

		s.agentLifecycleLog.Error("Agent create failed",
			"agent_id", req.ID, "project_id", req.ProjectID,
			"name", req.Name, "slug", req.Slug,
			"error", err)

		// Clean up provisioned agent files so they don't become orphans --
		// but only while this start still owns the agent name. The files
		// are addressed by name, and a newer agent with the same name may
		// have started since (for example, this agent was deleted while
		// its start was blocked and the name was reused); its files must
		// survive this start's failure. ownsName is a fresh read of the
		// name's marker, taken right before the removal. Subject to that
		// guard, this covers every Start failure, including a skill
		// resolution failure above (#2546). It covers broker files only,
		// not the hub's agent record.
		if opts.ProjectPath != "" && !ss.ownsName() {
			s.agentLifecycleLog.Info("Skipped agent file cleanup after start failure: the agent name is now owned by a newer start",
				"agent_id", req.ID, "project_id", req.ProjectID, "agent", opts.Name)
		} else if opts.ProjectPath != "" {
			if _, cleanupErr := agent.DeleteAgentFiles(opts.Name, opts.ProjectPath, true); cleanupErr != nil {
				s.agentLifecycleLog.Warn("Failed to clean up agent files after start failure",
					"agent_id", req.ID, "project_id", req.ProjectID, "agent", opts.Name, "error", cleanupErr)
			} else {
				s.agentLifecycleLog.Info("Cleaned up provisioned agent files after start failure",
					"agent_id", req.ID, "project_id", req.ProjectID, "agent", opts.Name)
			}
		}
		span.SetStatus(codes.Error, err.Error())
		switch {
		case errors.Is(err, agent.ErrContainerNameInUse):
			Conflict(w, err.Error())
		case notFoundErr:
			writeError(w, http.StatusNotFound, ErrCodeNotFound, "Failed to create agent: "+err.Error(), nil)
		case isSkillErr:
			SkillResolutionFailed(w, skillErr)
		default:
			RuntimeError(w, "Failed to create agent: "+err.Error())
		}
		return
	}

	s.agentLifecycleLog.Info("Agent dispatch: start complete",
		"agent_id", req.ID, "name", req.Name,
		"startElapsed", time.Since(startOpStart).String(),
		"totalElapsed", time.Since(createStart).String())
	s.agentLifecycleLog.Info("Agent created",
		"agent_id", req.ID, "project_id", req.ProjectID,
		"name", req.Name, "slug", req.Slug,
		"phase", string(state.PhaseRunning),
		"container_status", agentInfo.ContainerStatus)

	// Log auth resolution info visible in broker logs
	for _, w := range agentInfo.Warnings {
		if strings.HasPrefix(w, "Auth:") {
			s.agentLifecycleLog.Info("Agent auth resolution", "agent_id", req.ID, "project_id", req.ProjectID, "agent", req.Name, "result", w)
		}
	}

	resp := CreateAgentResponse{
		Agent:   agentInfoPtr(AgentInfoToResponse(*agentInfo)),
		Created: true,
	}
	if attempt != nil {
		s.dispatchAttemptsMu.Lock()
		s.completeAttempt(attempt, dispatchAttemptSucceeded, http.StatusCreated, &resp, nil, "")
		s.dispatchAttemptsMu.Unlock()
	}

	writeJSON(w, http.StatusCreated, resp)
}

// errInvalidWorkspaceDir marks a downloadWorkspaceFromGCS error caused by a
// workspace directory that fails scionrt.ValidateWorkspaceSource: a client
// error (400) on the synchronous path rather than a runtime error.
var errInvalidWorkspaceDir = errors.New("invalid workspace directory")

// downloadWorkspaceFromGCS performs the non-git GCS workspace bootstrap when
// req.WorkspaceStoragePath is set. It is a pure extraction of createAgent's
// original inline admission step (no behavior change), factored out so the
// async-launch path (runLaunch) can perform exactly the same step in its
// goroutine (design t1-async-create-v11.md §3.1: "Launch is the GCS
// workspace download ... plus Manager.Start, in a goroutine").
//
// Returns opts unchanged when WorkspaceStoragePath is empty. On error it
// returns: the short status string the synchronous caller records on the
// dispatch attempt; httpMessage, the exact user-facing text the synchronous
// path wrote with RuntimeError before this was extracted (byte-identical,
// capitalized, no wrapped error — design's "byte-identical to today" for the
// asyncLaunch-absent path); and err, a normal lowercase Go error for the
// async path's failure report and logging.
func (s *Server) downloadWorkspaceFromGCS(ctx context.Context, req CreateAgentRequest, opts api.StartOptions) (updated api.StartOptions, attemptMsg string, httpMessage string, err error) {
	if req.WorkspaceStoragePath == "" {
		return opts, "", "", nil
	}

	// Validate (inside resolveGCSWorkspaceDir) before anything below
	// creates or writes under the directory.
	workspaceDir, attemptMsg, httpMessage, err := s.resolveGCSWorkspaceDir(req)
	if err != nil {
		return opts, attemptMsg, httpMessage, err
	}

	if mkErr := os.MkdirAll(workspaceDir, 0755); mkErr != nil {
		return opts, "failed to create workspace directory", "Failed to create workspace directory: " + mkErr.Error(),
			fmt.Errorf("failed to create workspace directory: %w", mkErr)
	}

	bucket := s.config.StorageBucket
	if bucket == "" {
		return opts, "storage bucket not configured", "Storage bucket not configured for workspace bootstrap",
			errors.New("storage bucket not configured for workspace bootstrap")
	}

	if s.config.Debug {
		s.agentLifecycleLog.Debug("Downloading workspace from GCS", "agent_id", req.ID,
			"bucket", bucket,
			"storagePath", req.WorkspaceStoragePath+"/files",
			"workspaceDir", workspaceDir,
			"projectSlug", req.ProjectSlug,
		)
	}

	if syncErr := gcp.SyncFromGCS(ctx, bucket, req.WorkspaceStoragePath+"/files", workspaceDir); syncErr != nil {
		return opts, "failed to download workspace from GCS", "Failed to download workspace from GCS: " + syncErr.Error(),
			fmt.Errorf("failed to download workspace from GCS: %w", syncErr)
	}

	opts.Workspace = workspaceDir
	// Keep opts.ProjectPath so that ProvisionAgent resolves the correct
	// agent directory. The explicit workspace takes precedence over the
	// worktree logic in ProvisionAgent, so no worktree will be created.

	// Write a workspace marker so in-container CLI
	// can discover the project context and use the Hub API.
	if req.ProjectID != "" && req.ProjectSlug != "" {
		if writeErr := config.WriteWorkspaceMarker(workspaceDir, req.ProjectID, req.ProjectSlug, req.ProjectSlug); writeErr != nil {
			s.agentLifecycleLog.Warn("Failed to write workspace marker", "agent_id", req.ID, "project_id", req.ProjectID, "error", writeErr)
		}
	}
	return opts, "", "", nil
}

// resolveGCSWorkspaceDir computes the directory a GCS workspace bootstrap
// downloads into and validates it as a workspace source. It only reads the
// filesystem (symlink resolution), so the async admission path can run it
// before accepting a launch, and downloadWorkspaceFromGCS runs it again
// right before creating the directory. Errors use the same three-part shape
// as downloadWorkspaceFromGCS; a validation failure wraps
// errInvalidWorkspaceDir.
func (s *Server) resolveGCSWorkspaceDir(req CreateAgentRequest) (resolvedDir string, attemptMsg string, httpMessage string, err error) {
	// For hub-managed projects (ProjectSlug set), use the conventional path
	// ~/.scion/projects/<slug>/ instead of the worktree-based path.
	var workspaceDir string
	var workspaceRoot string
	if req.ProjectSlug != "" {
		globalDir, gdErr := config.GetGlobalDir()
		if gdErr != nil {
			return "", "failed to resolve global dir", "Failed to get global dir: " + gdErr.Error(),
				fmt.Errorf("failed to get global dir: %w", gdErr)
		}
		workspaceRoot = filepath.Join(globalDir, "projects")
		workspaceDir = filepath.Join(workspaceRoot, req.ProjectSlug)
	} else {
		workspaceRoot = s.config.WorktreeBase
		workspaceDir = filepath.Join(workspaceRoot, req.Name, "workspace")
	}

	// Validate before anything is created or written: req.ProjectSlug
	// and req.Name are already constrained to a single path element by the
	// caller, but this still runs independently, the same gate every other
	// workspace source goes through, before MkdirAll/SyncFromGCS ever touch
	// the filesystem. Callers use the resolved, symlink-free path it
	// returns, not the original join.
	resolvedWorkspaceDir, verr := scionrt.ValidateWorkspaceSource(workspaceDir, workspaceRoot)
	if verr != nil {
		return "", "invalid workspace directory", "Invalid workspace directory: " + verr.Error(),
			fmt.Errorf("%w: %w", errInvalidWorkspaceDir, verr)
	}
	return resolvedWorkspaceDir, "", "", nil
}

// hydrateTemplate resolves a Hub template to a local directory for provisioning.
// Returns the local template path, or empty string if no Hub template was specified.
//
// Resolution always goes through the connection's storage backend — there is a
// single read path for every topology. When the backend is the local filesystem
// (co-located workstation mode) the broker reads the resource directly from the
// backend's on-disk location; otherwise it hydrates from remote storage via
// signed URLs and the content-addressed cache.
func (s *Server) hydrateTemplate(ctx context.Context, cfg *CreateAgentConfig, conn *HubConnection) (string, error) {
	// Check if we have template info from Hub
	if cfg.TemplateID == "" && cfg.TemplateHash == "" {
		// No Hub template info provided, use local template handling
		return "", nil
	}

	// Local-backend direct read: the backend is the filesystem, so resolution is
	// a local path read — no HTTP, no cache.
	if conn.LocalStorage != nil {
		ref := cfg.TemplateID
		if ref == "" {
			ref = cfg.Template
		}
		path, err := s.resolveLocalResource(ctx, storage.ResourceKindTemplate, ref, conn)
		if err != nil {
			return "", err
		}
		if path != "" {
			return path, nil
		}
		// Not present in the backend yet — fall through to hydration.
	}

	hydrator := conn.Hydrator
	if hydrator == nil {
		return "", nil
	}

	// If we have a template hash, try to use it for cache lookup
	if cfg.TemplateHash != "" && cfg.TemplateID != "" {
		return hydrator.HydrateWithHash(ctx, cfg.TemplateID, cfg.TemplateHash)
	}

	// Just have template ID, do full hydration
	if cfg.TemplateID != "" {
		return hydrator.Hydrate(ctx, cfg.TemplateID)
	}

	return "", nil
}

// hydrateHarnessConfig resolves a Hub harness-config to a local directory for
// provisioning, mirroring hydrateTemplate. Returns the local directory path, or
// an empty string when no Hub harness-config was specified (the broker then
// falls back to its on-disk harness-config search). This is the §7.3 step-4
// consume path that makes harness-configs usable from a broker that lacks the
// config on its local filesystem.
func (s *Server) hydrateHarnessConfig(ctx context.Context, cfg *CreateAgentConfig, conn *HubConnection) (string, error) {
	if cfg == nil || (cfg.HarnessConfigID == "" && cfg.HarnessConfigHash == "") {
		if cfg != nil && cfg.HarnessConfig != "" {
			s.agentLifecycleLog.Warn("Harness-config hydration skipped: dispatch names harness-config but carries no config ID or hash; broker will fall back to on-disk search",
				"harness_config", cfg.HarnessConfig)
		}
		return "", nil
	}

	// Local-backend direct read (co-located workstation mode).
	//
	// Before returning the local path, verify that the dispatch's content
	// hash still matches the hub's authoritative hash. After a hub
	// re-bootstrap the DB hash changes, but the dispatch that created this
	// agent may carry the pre-bootstrap hash. In that window the on-disk
	// files are already updated (bootstrap writes storage before the DB),
	// so the local path is correct — but if the hashes diverge we must
	// re-fetch metadata to confirm, preventing a stale read if the storage
	// write is still in flight.
	if conn.LocalStorage != nil {
		ref := cfg.HarnessConfigID
		if ref == "" {
			ref = cfg.HarnessConfig
		}

		// Verify the dispatch hash is current by fetching live metadata
		// from the hub. For a co-located hub this is a cheap loopback DB
		// read. If the hashes match, the local storage path is
		// authoritative; if they differ the resource was re-bootstrapped
		// and we must use the current state.
		localPathOK := true
		var currentHC *hubclient.HarnessConfig
		if conn.HubClient != nil && cfg.HarnessConfigHash != "" && ref != "" {
			var metaErr error
			currentHC, metaErr = conn.HubClient.HarnessConfigs().Get(ctx, ref)
			if metaErr != nil {
				return "", wrapResourceMetaErr(metaErr, "harness-config")
			}
			if currentHC != nil && currentHC.ContentHash != cfg.HarnessConfigHash {
				s.agentLifecycleLog.Info("harness-config content hash changed since dispatch; using current version",
					"ref", ref,
					"dispatch_hash", cfg.HarnessConfigHash,
					"current_hash", currentHC.ContentHash)
				localPathOK = false
			}
		}

		if localPathOK {
			if currentHC != nil {
				// We already fetched metadata above; resolve the local
				// path directly to avoid a duplicate hub round-trip.
				if resolver, ok := conn.LocalStorage.(localObjectResolver); ok {
					objectPath := currentHC.StoragePath
					if objectPath == "" {
						objectPath = storage.ResourceStoragePath("", storage.ResourceKindHarnessConfig, currentHC.Scope, currentHC.ScopeID, currentHC.Slug)
					}
					dir := resolver.ObjectFSPath(objectPath)
					info, statErr := os.Stat(dir)
					if statErr == nil && info.IsDir() {
						return dir, nil
					}
				}
			} else {
				// Hash check was skipped (no hub client or no dispatch
				// hash); resolve via the standard path which fetches
				// metadata itself.
				path, err := s.resolveLocalResource(ctx, storage.ResourceKindHarnessConfig, ref, conn)
				if err != nil {
					return "", err
				}
				if path != "" {
					return path, nil
				}
			}
		}
		// Not present in the backend yet or hash mismatch — fall through to hydration.
	}

	resolver := conn.HCResolver
	if resolver == nil {
		return "", nil
	}

	// Always resolve via the hub's current metadata rather than trusting
	// the dispatch hash, which may be stale after a re-bootstrap.
	if cfg.HarnessConfigID != "" {
		return resolver.Resolve(ctx, cfg.HarnessConfigID)
	}

	return "", nil
}

// localObjectResolver is implemented by storage backends (the local filesystem
// backend) that can map an object path to an absolute on-disk path. This is the
// LocalDirBackend seam from §7.3: one assertion, used for every resource kind.
type localObjectResolver interface {
	ObjectFSPath(objectPath string) string
}

// resolveLocalResource resolves a resource of the given kind directly from a
// co-located local storage backend. It returns the on-disk directory backing the
// resource, or an empty string if the backend cannot serve it directly (caller
// then falls back to hydration). Metadata is fetched from the Hub to learn the
// resource's scope/slug; over a co-located loopback connection this is a cheap
// DB read.
func (s *Server) resolveLocalResource(ctx context.Context, kind storage.ResourceKind, ref string, conn *HubConnection) (string, error) {
	resolver, ok := conn.LocalStorage.(localObjectResolver)
	if !ok || conn.HubClient == nil || ref == "" {
		return "", nil
	}

	objectPath, err := s.resourceObjectPath(ctx, kind, ref, conn)
	if err != nil {
		return "", err
	}
	if objectPath == "" {
		return "", nil
	}

	dir := resolver.ObjectFSPath(objectPath)
	info, statErr := os.Stat(dir)
	if statErr != nil || !info.IsDir() {
		// Backend doesn't have the files on disk; let the caller hydrate.
		return "", nil
	}
	return dir, nil
}

// resourceObjectPath fetches resource metadata over the hub connection and
// returns its storage object path, falling back to the kind-keyed scope layout
// when the record carries no explicit StoragePath.
func (s *Server) resourceObjectPath(ctx context.Context, kind storage.ResourceKind, ref string, conn *HubConnection) (string, error) {
	switch kind {
	case storage.ResourceKindHarnessConfig:
		hc, err := conn.HubClient.HarnessConfigs().Get(ctx, ref)
		if err != nil {
			return "", wrapResourceMetaErr(err, "harness-config")
		}
		if hc == nil {
			return "", nil
		}
		if hc.StoragePath != "" {
			return hc.StoragePath, nil
		}
		return storage.ResourceStoragePath("", kind, hc.Scope, hc.ScopeID, hc.Slug), nil
	default:
		tmpl, err := conn.HubClient.Templates().Get(ctx, ref)
		if err != nil {
			return "", wrapResourceMetaErr(err, "template")
		}
		if tmpl == nil {
			return "", nil
		}
		if tmpl.StoragePath != "" {
			return tmpl.StoragePath, nil
		}
		scopeID := tmpl.ScopeID
		if scopeID == "" {
			scopeID = tmpl.ProjectID
		}
		return storage.ResourceStoragePath("", kind, tmpl.Scope, scopeID, tmpl.Slug), nil
	}
}

// wrapResourceMetaErr normalizes a hub metadata-fetch error, preserving the
// HubConnectivityError signal used by the provision path.
func wrapResourceMetaErr(err error, label string) error {
	if templatecache.IsHubConnectivityError(err) {
		return &templatecache.HubConnectivityError{Cause: err}
	}
	return fmt.Errorf("failed to get %s metadata: %w", label, err)
}

func (s *Server) handleAgentByID(w http.ResponseWriter, r *http.Request) {
	id, action := extractAction(r, "/api/v1/agents")

	if id == "" {
		NotFound(w, "Agent")
		return
	}
	// The ID is a single path segment (the agent's slug) that reaches
	// filesystem, container-name, and git-branch identity downstream, for
	// every action this handler dispatches to. Reject anything else here,
	// once, rather than at each individual action.
	//
	// Exception: a bare DELETE (no action sub-path) is exempted from this
	// gate. deleteAgent performs its own validation, gating only deletes
	// that actually touch files (deleteFiles or a soft delete); a
	// container-only removal (the default) never joins id onto a path, so
	// it must stay possible even for an existing agent recorded under a bad
	// id, and deleteAgent's own gate still applies whenever a file operation
	// is requested.
	isBareDelete := action == "" && r.Method == http.MethodDelete
	if !isBareDelete && !isSingleCleanPathElement(id) {
		BadRequest(w, "invalid agent id")
		return
	}

	// Extract projectId from query params for project-scoped agent resolution.
	// This prevents cross-project agent collision when two agents with the same
	// name exist in different projects on the same broker.
	projectID := r.URL.Query().Get("projectId")

	// Handle WebSocket attach for PTY
	if action == "attach" && isPTYWebSocketUpgrade(r) {
		s.handleAgentAttach(w, r)
		return
	}

	// Handle actions
	if action != "" {
		s.handleAgentAction(w, r, id, projectID, action)
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getAgent(w, r, id, projectID)
	case http.MethodDelete:
		s.deleteAgent(w, r, id, projectID)
	default:
		MethodNotAllowed(w)
	}
}

func (s *Server) getAgent(w http.ResponseWriter, r *http.Request, id, projectID string) {
	ctx := r.Context()

	// Resolve the correct manager (checks auxiliary runtimes if needed)
	mgr := s.resolveManagerForAgent(ctx, id, projectID)

	agents, err := mgr.List(ctx, map[string]string{"scion.agent": "true"})
	if err != nil {
		RuntimeError(w, "Failed to list agents: "+err.Error())
		return
	}

	for _, agent := range agents {
		if matchesAgent(agent, id, projectID) {
			writeJSON(w, http.StatusOK, AgentInfoToResponse(agent))
			return
		}
	}

	NotFound(w, "Agent")
}

func (s *Server) deleteAgent(w http.ResponseWriter, r *http.Request, id, projectID string) {
	ctx := r.Context()

	ctx, span := tracer.Start(ctx, "broker.agent.delete")
	defer span.End()
	span.SetAttributes(
		attribute.String("scion.agent.id", id),
		attribute.String("scion.project.id", projectID),
	)

	query := r.URL.Query()

	deleteFiles := query.Get("deleteFiles") == "true"
	removeBranch := query.Get("removeBranch") == "true"
	softDelete := query.Get("softDelete") == "true"

	// Cancel any in-flight start of this agent on this broker first, before
	// resolving the delete target: a start still blocked in provisioning
	// (for example, skill resolution) may have no container or listable
	// entry yet, so resolution can 404 before reaching the CancelLocal
	// below, leaving the start to run on and fail long after the agent is
	// gone. A key that matches no in-flight start is a harmless no-op.
	s.cancelLocalLaunch(launchKey{ProjectID: projectID, Slug: id})

	// Resolve the exact entry to delete, scoped to the requested project,
	// across the default and every auxiliary runtime (ptone/scion#1819).
	// Everything below acts on this entry only: the runtime operation uses
	// its container ID and the file deletion uses its project path. Nothing
	// re-resolves by bare slug, so a same-slug agent in another project can
	// never be touched, whatever the runtime.
	// projectPath is an optional hint from the hub (the provider's LocalPath
	// for a linked project); resolveDeleteTarget verifies it belongs to
	// projectID before using it.
	// The project path is needed both to delete files and to mark
	// agent-info.json deleted on a soft delete.
	target, err := s.resolveDeleteTarget(ctx, id, projectID, query.Get("projectPath"), deleteFiles || softDelete)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		if errors.Is(err, errDeleteTargetNotFound) {
			// No container and no files, but per-agent runtime objects
			// may remain when the container was removed outside scion.
			// Beyond that, no side effects: the hub's broker clients
			// treat a 404 on delete as an idempotent success.
			s.cleanupLeftoverAgentResources(ctx, id, projectID)
			s.agentLifecycleLog.Info("Agent delete: no matching agent in project",
				"agent_id", id, "project_id", projectID)
			NotFound(w, "Agent")
			return
		}
		if errors.Is(err, errDeleteTargetUnknown) {
			RuntimeError(w, "Failed to delete agent: "+err.Error())
			return
		}
		Conflict(w, "Failed to delete agent: "+err.Error())
		return
	}
	projectPath := target.projectPath
	agentProjectID := target.projectID

	// Wake any local launch waiting on this agent (design §3.8.1): purely a
	// local optimisation (the Hub's answer to the launch's next report is
	// what actually ends it), so a key that doesn't match an in-flight
	// launch is a harmless no-op.
	s.cancelLocalLaunch(launchKey{ProjectID: agentProjectID, Slug: target.name})

	filesToDelete := deleteFiles
	if deleteFiles && projectPath == "" && projectID != "" {
		// The matched entry has no project path and none could be resolved
		// for this project. Do not let DeleteAgentFiles fall back to the
		// broker's CWD project, which may belong to someone else.
		s.agentLifecycleLog.Warn("Agent delete: no project path for matched agent; skipping file cleanup",
			"agent_id", id, "project_id", projectID)
		filesToDelete = false
	}

	// target.name is joined onto a project directory as a single path
	// segment by every file operation below -- the soft-delete
	// agent-info.json update and DeleteTarget's eventual filesystem cleanup
	// -- so before either one runs it must be exactly that, and the
	// resolved directory must still be contained under this project's
	// agents root. A delete that touches no files at all (deleteFiles=false
	// and not a soft-delete, the default) never joins target.name onto
	// anything, so it is intentionally left ungated here: rejecting it too
	// would make a legacy agent with a bad name impossible to remove by
	// container ID alone, when nothing about that request ever touches the
	// filesystem.
	if (softDelete && projectPath != "") || filesToDelete {
		if !isSingleCleanPathElement(target.name) {
			span.SetStatus(codes.Error, "invalid agent name")
			ValidationError(w, "agent id must be a single path element", nil)
			return
		}
		if projectPath != "" {
			if projectDir, dirErr := config.GetResolvedProjectDir(projectPath); dirErr == nil {
				if _, containErr := agent.CheckAgentDirContained(projectDir, target.name, false); containErr != nil {
					span.SetStatus(codes.Error, containErr.Error())
					ValidationError(w, "agent id resolves outside the project's agent directory", nil)
					return
				}
			}
		}
	}

	// If this is a soft-delete, mark agent-info.json with deleted status before cleanup
	if softDelete && projectPath != "" {
		deletedAtStr := query.Get("deletedAt")
		if err := agent.UpdateAgentConfig(target.name, projectPath, "deleted", "", ""); err != nil {
			s.agentLifecycleLog.Warn("Failed to mark agent as deleted in agent-info.json", "agent_id", id, "error", err)
		}
		if deletedAtStr != "" {
			if deletedAt, err := time.Parse(time.RFC3339, deletedAtStr); err == nil {
				if err := agent.UpdateAgentDeletedAt(target.name, projectPath, deletedAt); err != nil {
					s.agentLifecycleLog.Warn("Failed to write deletedAt to agent-info.json", "agent_id", id, "error", err)
				}
			}
		}
	}

	_, err = target.mgr.DeleteTarget(ctx, target.name, target.containerID, filesToDelete, projectPath, removeBranch)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		RuntimeError(w, "Failed to delete agent: "+err.Error())
		return
	}
	if target.containerID == "" {
		// The container was already gone, so DeleteTarget made no runtime
		// call and the runtime never removed the objects it created with
		// the container.
		s.cleanupLeftoverAgentResources(ctx, target.name, projectID)
	}

	// On the NFS workspace, the agent's worktree (worktree-per-agent) or
	// its own workspace (clone-per-agent) lives on the export rather than
	// in the agent's files, so remove it here too. A failure does not fail
	// the delete; what failed is then left in place, and an agent created
	// again with the same name reuses it. The agent's branch is always
	// kept, whatever removeBranch says.
	if filesToDelete && agentProjectID != "" {
		if remover, ok := target.mgr.(nfsAgentFilesRemover); ok {
			if paths, rmErr := remover.RemoveNFSAgentFiles(ctx, projectPath, agentProjectID, target.name); rmErr != nil {
				s.agentLifecycleLog.Warn("Agent delete: could not remove the agent's files on the NFS workspace; left in place",
					"agent_id", id, "project_id", agentProjectID, "paths", paths, "error", rmErr)
			}
		}
	}

	if softDelete {
		s.agentLifecycleLog.Info("Agent soft-deleted",
			"agent_id", id, "project_id", agentProjectID,
			"delete_files", deleteFiles, "remove_branch", removeBranch)
	} else {
		s.agentLifecycleLog.Info("Agent deleted",
			"agent_id", id, "project_id", agentProjectID,
			"delete_files", deleteFiles, "remove_branch", removeBranch)
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAgentAction(w http.ResponseWriter, r *http.Request, id, projectID, action string) {
	method, ok := api.RuntimeBrokerAgentActionMethod(action)
	if !ok {
		NotFound(w, "Action")
		return
	}
	if r.Method != method {
		MethodNotAllowed(w)
		return
	}

	switch action {
	case api.AgentActionStart:
		s.startAgent(w, r, id, projectID)
	case api.AgentActionStop:
		s.stopAgent(w, r, id, projectID)
	case api.AgentActionSuspend:
		s.stopAgent(w, r, id, projectID)
	case api.AgentActionRestart:
		s.restartAgent(w, r, id, projectID)
	case api.AgentActionMessage:
		s.sendMessage(w, r, id, projectID)
	case api.AgentActionKeys:
		s.sendKeys(w, r, id, projectID)
	case api.AgentActionExec:
		s.execCommand(w, r, id, projectID)
	case api.AgentActionResetAuth:
		s.resetAuth(w, r, id, projectID)
	case api.AgentActionLogs:
		s.getLogs(w, r, id, projectID)
	case api.AgentActionStats:
		s.getStats(w, r, id, projectID)
	case api.AgentActionHasPrompt:
		s.checkAgentPrompt(w, r, id, projectID)
	}
}

func (s *Server) startAgent(w http.ResponseWriter, r *http.Request, id, projectID string) {
	ctx := r.Context()

	// ProjectID reaches filesystem paths further on (the project-marker
	// block in buildStartContext, and worktree provisioning); an empty
	// value is a normal, valid case, but a non-empty value must be a single
	// path element.
	if projectID != "" && !isSingleCleanPathElement(projectID) {
		BadRequest(w, "invalid projectId")
		return
	}

	ctx, span := tracer.Start(ctx, "broker.agent.start")
	defer span.End()
	span.SetAttributes(
		attribute.String("scion.agent.id", id),
		attribute.String("scion.project.id", projectID),
	)

	// id is joined onto a project directory as a single path segment by
	// every file operation below (applyInlineConfigUpdate, GetSavedProfile,
	// GetSavedPhase, and ultimately GetAgentDir via mgr.Start), so it must be
	// exactly that: reject anything that would join as more than one
	// segment, or as ".." / ".", before any of those joins happens.
	if !isSingleCleanPathElement(id) {
		span.SetStatus(codes.Error, "invalid agent id")
		ValidationError(w, "agent id must be a single path element", nil)
		return
	}

	// Read optional task, projectPath, projectSlug, harnessConfig, and resolvedEnv from request body
	var startReq struct {
		Task               string                 `json:"task"`
		ProjectPath        string                 `json:"projectPath"`
		ProjectSlug        string                 `json:"projectSlug"`
		HarnessConfig      string                 `json:"harnessConfig"`
		HarnessConfigID    string                 `json:"harnessConfigId"`
		HarnessConfigHash  string                 `json:"harnessConfigHash"`
		ResolvedEnv        map[string]string      `json:"resolvedEnv"`
		EnvClassifications map[string]api.EnvKind `json:"envClassifications,omitempty"`
		ResolvedSecrets    []api.ResolvedSecret   `json:"resolvedSecrets,omitempty"`
		InlineConfig       *api.ScionConfig       `json:"inlineConfig,omitempty"`
		SharedDirs         []api.SharedDir        `json:"sharedDirs,omitempty"`
		// SharedWorkspace must be re-sent on every start: hub-project agents
		// share a single git checkout instead of being given a worktree, and
		// without this flag the broker would create a worktree on restart.
		SharedWorkspace bool `json:"sharedWorkspace,omitempty"`
		// Resume requests harness session continuation (e.g. Claude
		// --continue). The hub is the source of truth and sets this from the
		// agent's stored phase; when unset we fall back to GetSavedPhase below.
		Resume bool `json:"resume,omitempty"`
		// HubEndpoint, UserID, ProvisionCredentials, and PreResolvedSkills
		// carry the same dispatch-time metadata the create path sends, so a
		// re-provision reached via start (e.g. after the broker deletes a
		// stale agent dir) can resolve required skills exactly as create does
		// instead of failing closed with "no skill resolver available"
		// (#1960). ProjectID is not repeated here: it is already scoped via
		// the projectId query parameter on this endpoint.
		HubEndpoint          string                           `json:"hubEndpoint,omitempty"`
		UserID               string                           `json:"userId,omitempty"`
		ProvisionCredentials map[string]string                `json:"provisionCredentials,omitempty"`
		PreResolvedSkills    *hubclient.ResolveSkillsResponse `json:"preResolvedSkills,omitempty"`
		// GitClone, Branch, and WorkspaceMode carry the workspace-recreation
		// inputs the Hub sends on start (GoogleCloudPlatform/scion#1931), so
		// an agent's workspace can be recreated on a runtime that does not
		// keep it between stops. Threaded into cfg the same way create's
		// Config.GitClone and Config.Branch are, and into WorkspaceMode the
		// same way create's RemoteCreateAgentRequest.WorkspaceMode is.
		GitClone      *api.GitCloneConfig `json:"gitClone,omitempty"`
		Branch        string              `json:"branch,omitempty"`
		WorkspaceMode string              `json:"workspaceMode,omitempty"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&startReq); err != nil {
			s.agentLifecycleLog.Debug("No task in start request body (ignoring decode error)", "agent_id", id, "error", err)
		}
	}
	// Inject skill resolver from Hub connection for skill provisioning, same
	// as createAgent (#1960). ProjectID comes from the URL-scoped function
	// argument since start doesn't repeat it in the body.
	ctx = s.attachSkillResolver(ctx, r, skillResolverInputs{
		HubEndpoint:          startReq.HubEndpoint,
		ResolvedEnv:          startReq.ResolvedEnv,
		ProvisionCredentials: startReq.ProvisionCredentials,
		PreResolvedSkills:    startReq.PreResolvedSkills,
		ProjectID:            projectID,
		UserID:               startReq.UserID,
	})

	s.agentLifecycleLog.Debug("startAgent called", "agent_id", id, "task", startReq.Task, "projectPath", startReq.ProjectPath, "projectSlug", startReq.ProjectSlug, "harnessConfig", startReq.HarnessConfig, "resolvedEnvCount", len(startReq.ResolvedEnv))

	// Build config for buildStartContext (startAgent uses a subset of CreateAgentConfig)
	var cfg *CreateAgentConfig
	if startReq.Task != "" || startReq.HarnessConfig != "" || startReq.HarnessConfigID != "" || startReq.HarnessConfigHash != "" || len(startReq.SharedDirs) > 0 || startReq.SharedWorkspace || startReq.GitClone != nil || startReq.Branch != "" {
		cfg = &CreateAgentConfig{
			Task:              startReq.Task,
			HarnessConfig:     startReq.HarnessConfig,
			HarnessConfigID:   startReq.HarnessConfigID,
			HarnessConfigHash: startReq.HarnessConfigHash,
			SharedDirs:        startReq.SharedDirs,
			SharedWorkspace:   startReq.SharedWorkspace,
			GitClone:          startReq.GitClone,
			Branch:            startReq.Branch,
		}
	}

	// Parity with the create path: populate the dedicated AgentToken field from
	// the hub-minted token in ResolvedEnv so buildStartContext treats it as an
	// explicit token rather than relying on the resolvedEnv-kept fallback. The
	// precedence in buildStartContext step 3 keeps this token regardless, but
	// setting it here makes the start path behave like create.
	startContextAgentToken := startReq.ResolvedEnv["SCION_AUTH_TOKEN"]

	// The URL path segment (id) is the agent's slug, not the Hub UUID: the
	// broker's own dispatch client sends agent.Slug there. It is also the
	// stable on-disk identity: create keys the agent directory, container
	// name, and worktree branch/sharer-registry entry on the slug, so start
	// must use the same value — never a hub-editable display name, which can
	// change independently of the slug and is not safe to use as a
	// filesystem path component. The Hub UUID is only available via the
	// resolvedEnv the Hub injects on every start dispatch
	// (pkg/hub/httpdispatcher.go's DispatchAgentStart), which sets
	// SCION_AGENT_ID/SCION_PROJECT_ID. AgentID and ProjectID must reach
	// buildStartContext so a worktree-per-agent start resolves this agent's
	// own worktree path, not the shared "worktrees" parent directory every
	// agent's worktree lives under.
	startAgentID := startReq.ResolvedEnv["SCION_AGENT_ID"]

	// If the request names no project, recover the project path from the
	// agent's existing container, found by the same project-scoped search
	// across every runtime this broker has that stop and restart use. This
	// runs before buildStartContext so its runtime resolution reads the
	// agent's project settings and saved profile. A recovered path is the
	// project's .scion directory as the container recorded it, not a
	// project root, so buildStartContext is told where it came from.
	startProjectPath := startReq.ProjectPath
	var startProjectPathFromContainer bool
	if startReq.ProjectPath == "" && startReq.ProjectSlug == "" {
		m, err := s.lookupAgentMatch(ctx, id, projectID)
		switch {
		case m.matched:
			startProjectPath = m.entry.ProjectPath
			startProjectPathFromContainer = startProjectPath != ""
		case errors.Is(err, ErrAgentNotFound):
			// No container to recover from; resolve as a request that names
			// no project.
		case errors.Is(err, errAuxiliaryRuntimeList):
			// The default runtime listed no match and an auxiliary runtime
			// could not list; resolve as a request that names no project
			// rather than failing a start that may not involve that runtime.
			s.agentLifecycleLog.Warn("Start agent: auxiliary runtime list failed while recovering the project path",
				"agent_id", id, "error", err)
		case errors.Is(err, ErrAgentListUnavailable):
			span.SetStatus(codes.Error, err.Error())
			AgentLookupUnavailable(w, err, id, "start", "")
			return
		default:
			// More than one container matches: none of them is taken as the
			// agent's project; resolve as a request that names no project.
			s.agentLifecycleLog.Warn("Start agent: project path not recovered", "agent_id", id, "error", err)
		}
	}

	sc, err := s.buildStartContext(ctx, startContextInputs{
		Name:                     id,
		AgentID:                  startAgentID,
		ProjectID:                projectID,
		ProjectPath:              startProjectPath,
		ProjectPathFromContainer: startProjectPathFromContainer,
		ProjectSlug:              startReq.ProjectSlug,
		Config:                   cfg,
		InlineConfig:             startReq.InlineConfig,
		HubEndpoint:              startReq.HubEndpoint,
		ResolvedEnv:              startReq.ResolvedEnv,
		EnvClassifications:       startReq.EnvClassifications,
		ResolvedSecrets:          startReq.ResolvedSecrets,
		SharedDirs:               startReq.SharedDirs,
		AgentToken:               startContextAgentToken,
		WorkspaceMode:            startReq.WorkspaceMode,
		HTTPRequest:              r,
		Operation:                opHTTPStart,
	})
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		writeStartContextError(w, err)
		return
	}
	opts := sc.Opts

	// Once ProjectPath is resolved, confirm id's agent directory actually
	// resolves under this project's agents root before applyInlineConfigUpdate,
	// GetSavedProfile, or GetSavedPhase touch it below -- the same
	// defense-in-depth pairing checkAgentDirContained already provides
	// against isSingleCleanPathElement inside ProvisionAgent and GetAgent.
	if opts.ProjectPath != "" {
		if projectDir, dirErr := config.GetResolvedProjectDir(opts.ProjectPath); dirErr == nil {
			if _, containErr := agent.CheckAgentDirContained(projectDir, id, startReq.SharedWorkspace); containErr != nil {
				span.SetStatus(codes.Error, containErr.Error())
				ValidationError(w, "agent id resolves outside the project's agent directory", nil)
				return
			}
		}
	}

	// Resolve saved profile for runtime selection, and re-resolve the
	// manager against it. This resolution is the authoritative one for what
	// actually starts, so the hub-default passthrough re-check runs again
	// here (recheckHubDefaultPassthrough), and so does the Kubernetes/"block"
	// rejection (rejectKubernetesBlock, start_context.go, ptone/scion#2328):
	// a saved profile buildStartContext could not see may resolve to
	// Kubernetes only at this later point. This runs before any side effect
	// below (applyInlineConfigUpdate's scion-agent.json write), so a
	// rejection here does not leave a partial update applied.
	if opts.ProjectPath != "" {
		opts.Profile = agent.GetSavedProfile(id, opts.ProjectPath)
	}
	mgr, resolvedRuntimeType := s.resolveManagerForOpts(opts)
	recheckHubDefaultPassthrough(opts.Env, sc.EnvClassifications, resolvedRuntimeType)
	if sce := rejectKubernetesBlock(resolvedRuntimeType, opts.Env["SCION_METADATA_MODE"]); sce != nil {
		writeStartContextError(w, sce)
		return
	}

	// Apply updated InlineConfig to scion-agent.json before starting.
	if startReq.InlineConfig != nil && opts.ProjectPath != "" {
		s.applyInlineConfigUpdate(id, opts.ProjectPath, startReq.InlineConfig, startReq.SharedWorkspace)
	}

	// The hub is the source of truth for resume intent: when it sets
	// req.Resume the harness must continue its prior session. We still fall
	// back to reading the saved phase from disk when the hub did not specify
	// resume (e.g. the local CLI path, which does not send this flag).
	if startReq.Resume {
		opts.Resume = true
	} else if opts.ProjectPath != "" {
		savedPhase := agent.GetSavedPhase(id, opts.ProjectPath)
		if savedPhase == string(state.PhaseSuspended) {
			opts.Resume = true
		}
	}

	agentInfo, err := mgr.Start(ctx, opts)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		s.agentLifecycleLog.Error("Agent start failed",
			"agent_id", id, "error", err)
		if errors.Is(err, agent.ErrContainerNameInUse) {
			Conflict(w, err.Error())
		} else {
			RuntimeError(w, "Failed to start agent: "+err.Error())
		}
		return
	}

	s.agentLifecycleLog.Info("Agent started",
		"agent_id", id, "project_id", agentInfo.ProjectID,
		"name", agentInfo.Name, "slug", agentInfo.Slug,
		"phase", string(state.PhaseRunning),
		"container_status", agentInfo.ContainerStatus)

	// Send an immediate heartbeat so the hub gets the updated container status
	s.forceHeartbeatAll("start", id)

	agentResp := AgentInfoToResponse(*agentInfo)
	writeJSON(w, http.StatusAccepted, CreateAgentResponse{
		Agent:   &agentResp,
		Created: false,
	})
}

// applyInlineConfigUpdate merges the updated InlineConfig into the agent's
// scion-agent.json. This ensures config changes made via the Hub (e.g. limits
// set in the web configure form) are applied before the agent starts.
//
// sharedWorkspace branches the path: shared-workspace agents store
// scion-agent.json externally (~/.scion.project-configs/<slug>__<uuid>/.scion/
// agents/<name>/) so siblings cannot read it via /workspace.
func (s *Server) applyInlineConfigUpdate(agentName, projectPath string, inlineConfig *api.ScionConfig, sharedWorkspace bool) {
	projectDir, err := config.GetResolvedProjectDir(projectPath)
	if err != nil {
		s.agentLifecycleLog.Warn("applyInlineConfigUpdate: failed to resolve project dir", "agent", agentName, "error", err)
		return
	}
	agentDir := config.GetAgentDir(projectDir, agentName, sharedWorkspace)
	cfgPath := filepath.Join(agentDir, "scion-agent.json")

	// Load existing config
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		s.agentLifecycleLog.Warn("applyInlineConfigUpdate: failed to read scion-agent.json", "agent", agentName, "path", cfgPath, "error", err)
		return
	}
	var existing api.ScionConfig
	if err := json.Unmarshal(data, &existing); err != nil {
		s.agentLifecycleLog.Warn("applyInlineConfigUpdate: failed to parse scion-agent.json", "agent", agentName, "error", err)
		return
	}

	// Merge inline config over existing. MergeScionConfig appends skills, and
	// the Hub sends the agent's full skill list on every start and restart,
	// so collapse references that name the same skill and destination.
	merged := config.MergeScionConfig(&existing, inlineConfig)
	merged.Skills = dedupeSkillReferences(merged.Skills)

	// Write back
	updated, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		s.agentLifecycleLog.Warn("applyInlineConfigUpdate: failed to marshal updated config", "agent", agentName, "error", err)
		return
	}
	if err := os.WriteFile(cfgPath, updated, 0644); err != nil {
		s.agentLifecycleLog.Warn("applyInlineConfigUpdate: failed to write scion-agent.json", "agent", agentName, "error", err)
		return
	}
	if s.config.Debug {
		s.agentLifecycleLog.Debug("applyInlineConfigUpdate: applied inline config update",
			"agent", agentName, "maxTurns", inlineConfig.MaxTurns, "maxModelCalls", inlineConfig.MaxModelCalls)
	}
}

// dedupeSkillReferences keeps only the final occurrence of each skill
// reference key (URI plus As) and drops earlier ones, preserving the relative
// order of the references that remain. The Hub resolver and the provision
// step's required-skill check collapse references that share a URI
// last-wins, taking As, Scope and Optional from the last one. The last
// reference for a URI is always the final occurrence of its own key, so it
// survives and stays last among that URI's references, and on that path the
// installed result is unchanged. Resolvers that resolve each reference on
// its own (gcp-skill://, and gh:// when the Hub is unavailable) install the
// same files under the same name; the surviving entry now carries the latest
// Scope and Optional, matching the Hub path. References with the same URI
// but different As are kept as separate entries, because the dedupe does not
// merge different install names (the per-reference resolvers install both).
func dedupeSkillReferences(refs []api.SkillReference) []api.SkillReference {
	if len(refs) < 2 {
		return refs
	}
	type key struct{ uri, as string }
	last := make(map[key]int, len(refs))
	for i, ref := range refs {
		last[key{ref.URI, ref.As}] = i
	}
	out := make([]api.SkillReference, 0, len(last))
	for i, ref := range refs {
		if last[key{ref.URI, ref.As}] == i {
			out = append(out, ref)
		}
	}
	return out
}

// isContainerStopTolerable returns true if the error from stopping a container
// indicates the container is already stopped, exited, or doesn't exist. This
// covers both Docker and Podman error messages and exit codes.
func isContainerStopTolerable(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "not found") ||
		strings.Contains(msg, "no such") ||
		strings.Contains(msg, "No such") ||
		strings.Contains(msg, "not running") ||
		strings.Contains(msg, "is not running") ||
		strings.Contains(msg, "exit status 125")
}

// agentsWithoutProjectLabel returns the subset of agents that carry no
// scion.project_id label. The project-scoped lookups fall back to a
// slug-only search for backward compatibility with pre-existing / solo-mode
// containers that predate project labels; that fallback must only match such
// genuinely unlabeled containers. A container labeled for a *different*
// project must never satisfy a project-scoped request, or same-slug agents
// across projects would collide.
func agentsWithoutProjectLabel(agents []api.AgentInfo) []api.AgentInfo {
	filtered := make([]api.AgentInfo, 0, len(agents))
	for _, a := range agents {
		if a.Labels["scion.project_id"] == "" {
			filtered = append(filtered, a)
		}
	}
	return filtered
}

// projectScopedTarget resolves an agent slug to its project-scoped container
// identifier (container ID for docker/podman, pod name for k8s) via
// lookupAgentTarget, together with the agent.Manager whose runtime reported
// that identifier. When multiple projects on this broker have an agent with
// the same slug, this ensures single-container operations act on the agent
// in the requested project rather than whichever the slug matches first,
// and the returned manager is the one the identifier came from, so the
// operation is dispatched to that same runtime.
//
// When a projectID is supplied but the agent cannot be resolved, it returns
// "" and a nil manager — callers must treat that as "not found in this
// project" rather than falling back to the bare slug, which would risk
// acting on a same-slug agent in a different project. Only when no
// projectID is supplied and the lookup genuinely found nothing does it
// degrade to the original id (and the manager resolveManagerForAgent
// picks) for backward compatibility (solo/CLI mode, unlabeled containers).
//
// With or without a projectID, a lookup failure other than a genuine
// ErrAgentNotFound (a runtime listing error — including an auxiliary
// runtime's, when no other runtime matched — or an ambiguous match) is
// returned to the caller rather than silently treated as "not found" —
// callers must surface it as a real error instead of reporting a successful
// stop/restart, and must never proceed with the bare slug, which a second,
// independent lookup could resolve to a same-slug agent in another context
// (ptone/scion#2549). This matches execCommand and resetAuth.
// ErrAgentNotFound also covers a matching agent record with no resolvable
// container id (e.g. a malformed or partial runtime entry that carries no
// container id — nothing addressable to stop): that case is folded into
// the same "not found" outcome as a genuine no-match.
func (s *Server) projectScopedTarget(ctx context.Context, id, projectID string) (string, agent.Manager, error) {
	m, err := s.lookupAgentMatch(ctx, id, projectID)
	return s.projectScopedTargetFrom(ctx, id, projectID, m, err)
}

// projectScopedTargetFrom applies projectScopedTarget's rules to a
// lookupAgentMatch result the caller already has, so restart can read the
// agent's project path from the same entry it stops.
func (s *Server) projectScopedTargetFrom(ctx context.Context, id, projectID string, m agentMatch, err error) (string, agent.Manager, error) {
	if err == nil && m.containerID != "" {
		return m.containerID, m.manager, nil
	}
	if err != nil && !errors.Is(err, ErrAgentNotFound) {
		return "", nil, err
	}
	if projectID != "" {
		return "", nil, nil
	}
	return id, s.resolveManagerForAgent(ctx, id, projectID), nil
}

func (s *Server) stopAgent(w http.ResponseWriter, r *http.Request, id, projectID string) {
	ctx := r.Context()

	ctx, span := tracer.Start(ctx, "broker.agent.stop")
	defer span.End()
	span.SetAttributes(
		attribute.String("scion.agent.id", id),
		attribute.String("scion.project.id", projectID),
	)

	// Wake any local launch waiting on this agent (design §3.8.1); see the
	// identical comment in deleteAgent.
	s.cancelLocalLaunch(launchKey{ProjectID: projectID, Slug: id})

	// Resolve the project-scoped container so that same-slug agents in
	// different projects on this broker don't collide. An empty target means
	// the agent isn't present in this project; treat that as an idempotent
	// no-op rather than stopping a same-slug agent in another project.
	//
	// A lookup error other than ErrAgentNotFound (a runtime listing failed
	// with no match found on any other runtime, or the match was ambiguous)
	// is a real failure, not a successful stop: a failed List on the one
	// runtime that may hold the agent must not be misread as "not found".
	// The same lookup also supplies the manager Stop is dispatched through
	// below — the manager whose List call actually produced the matched
	// entry, not one re-resolved by a second, independent lookup that could
	// land on a different runtime than the one the target came from.
	target, mgr, err := s.projectScopedTarget(ctx, id, projectID)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		if errors.Is(err, ErrAgentListUnavailable) {
			AgentLookupUnavailable(w, err, id, "stop", "")
			return
		}
		s.agentLifecycleLog.Warn("Stop agent: lookup failed", "agent_id", id, "error", err)
		RuntimeError(w, "Failed to stop agent")
		return
	}
	if target == "" {
		s.agentLifecycleLog.Info("Agent stopped (not found in project)",
			"agent_id", id,
			"phase", string(state.PhaseStopped))
		s.forceHeartbeatAll("stop", id)
		writeJSON(w, http.StatusAccepted, map[string]string{
			"status":  "accepted",
			"message": "Stop operation accepted",
		})
		return
	}
	if err := mgr.Stop(ctx, target, ""); err != nil {
		if isContainerStopTolerable(err) {
			// Container doesn't exist, is already stopped, or podman/docker can't find it.
			// Treat as success so the hub can update its state.
			s.agentLifecycleLog.Info("Agent stopped (already stopped/not found)",
				"agent_id", id,
				"phase", string(state.PhaseStopped))
		} else {
			span.SetStatus(codes.Error, err.Error())
			RuntimeError(w, "Failed to stop agent: "+err.Error())
			return
		}
	} else {
		s.agentLifecycleLog.Info("Agent stopped",
			"agent_id", id,
			"phase", string(state.PhaseStopped))
	}

	s.forceHeartbeatAll("stop", id)

	writeJSON(w, http.StatusAccepted, map[string]string{
		"status":  "accepted",
		"message": "Stop operation accepted",
	})
}

func (s *Server) restartAgent(w http.ResponseWriter, r *http.Request, id, projectID string) {
	ctx := r.Context()

	// Read optional resolvedEnv from request body (hub sends fresh auth token)
	var restartReq struct {
		ResolvedEnv        map[string]string      `json:"resolvedEnv"`
		EnvClassifications map[string]api.EnvKind `json:"envClassifications,omitempty"`
		// HubEndpoint, UserID, ProvisionCredentials, and PreResolvedSkills
		// mirror the same fields on the start path (#1960): they let a
		// re-provision reached via restart resolve required skills exactly
		// as create does instead of failing closed.
		HubEndpoint          string                           `json:"hubEndpoint,omitempty"`
		UserID               string                           `json:"userId,omitempty"`
		ProvisionCredentials map[string]string                `json:"provisionCredentials,omitempty"`
		PreResolvedSkills    *hubclient.ResolveSkillsResponse `json:"preResolvedSkills,omitempty"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&restartReq); err != nil {
			s.agentLifecycleLog.Debug("No resolvedEnv in restart request body (ignoring decode error)", "agent_id", id, "error", err)
		}
	}

	// Inject skill resolver from Hub connection for skill provisioning, same
	// as createAgent/startAgent (#1960).
	ctx = s.attachSkillResolver(ctx, r, skillResolverInputs{
		HubEndpoint:          restartReq.HubEndpoint,
		ResolvedEnv:          restartReq.ResolvedEnv,
		ProvisionCredentials: restartReq.ProvisionCredentials,
		PreResolvedSkills:    restartReq.PreResolvedSkills,
		ProjectID:            projectID,
		UserID:               restartReq.UserID,
	})

	// Look up the agent with the same project-scoped search across every
	// runtime this broker has that the stop below uses, and read its name
	// and project path from the matched entry; the project path is what
	// lets the runtime resolution below read the agent's project settings
	// and saved profile. A lookup error leaves the agent unresolved here and
	// is reported where the stop target is resolved, after the
	// runtime checks.
	agentName := id
	var projectPath string
	match, matchErr := s.lookupAgentMatch(ctx, id, projectID)
	if match.matched {
		if match.entry.Name != "" {
			agentName = match.entry.Name
		}
		projectPath = match.entry.ProjectPath
	}

	sc, err := s.buildStartContext(ctx, startContextInputs{
		Name:                     agentName,
		ProjectPath:              projectPath,
		ProjectPathFromContainer: projectPath != "",
		HubEndpoint:              restartReq.HubEndpoint,
		ResolvedEnv:              restartReq.ResolvedEnv,
		EnvClassifications:       restartReq.EnvClassifications,
		HTTPRequest:              r,
		Operation:                opHTTPRestart,
	})
	if err != nil {
		writeStartContextError(w, err)
		return
	}
	opts := sc.Opts

	if opts.ProjectPath != "" {
		opts.Profile = agent.GetSavedProfile(id, opts.ProjectPath)
	}

	// Re-resolve manager after the profile update above, and re-run the
	// hub-default-passthrough downgrade and the Kubernetes/block rejection
	// against this later, authoritative resolution — before the stop below,
	// a real side effect. A rejection here must leave the agent exactly as
	// it was; running this after the stop would return 400 with the agent
	// already stopped. See the identical re-check and comment in startAgent.
	mgr, resolvedRuntimeType := s.resolveManagerForOpts(opts)
	recheckHubDefaultPassthrough(opts.Env, sc.EnvClassifications, resolvedRuntimeType)
	if sce := rejectKubernetesBlock(resolvedRuntimeType, opts.Env["SCION_METADATA_MODE"]); sce != nil {
		writeStartContextError(w, sce)
		return
	}

	// Stop then start — tolerate stop errors since the container may already
	// be exited and the subsequent start will handle cleanup.
	// The lookup also returns the manager whose runtime reported the
	// target, so the stop goes to that same runtime (default or auxiliary).
	stopTarget, stopMgr, err := s.projectScopedTargetFrom(ctx, id, projectID, match, matchErr)
	if err != nil {
		// A lookup error other than "not found" must abort the restart
		// without starting a second container — otherwise a runtime hiccup
		// during the stop-target lookup would leave two containers running
		// for the same agent.
		if errors.Is(err, ErrAgentListUnavailable) {
			AgentLookupUnavailable(w, err, id, "restart", "")
			return
		}
		s.agentLifecycleLog.Warn("Restart agent: lookup failed", "agent_id", id, "error", err)
		RuntimeError(w, "Failed to restart agent")
		return
	}
	// An empty target means the agent isn't present in this project — skip the
	// stop (don't risk stopping a same-slug agent in another project) and let
	// the start below create it.
	if stopTarget == "" {
		s.agentLifecycleLog.Warn("Restart: agent not found in project, proceeding with start", "agent_id", id)
	} else if err := stopMgr.Stop(ctx, stopTarget, ""); err != nil {
		if isContainerStopTolerable(err) {
			s.agentLifecycleLog.Warn("Restart: stop target not found or already stopped, proceeding with start", "agent_id", id, "error", err)
		} else {
			s.agentLifecycleLog.Warn("Restart: stop failed, proceeding with start anyway", "agent_id", id, "error", err)
		}
	}

	agentInfo, err := mgr.Start(ctx, opts)
	if err != nil {
		s.agentLifecycleLog.Error("Agent restart failed",
			"agent_id", id, "error", err)
		if strings.Contains(err.Error(), "not found") {
			NotFound(w, "Agent")
			return
		}
		RuntimeError(w, "Failed to restart agent: "+err.Error())
		return
	}

	s.agentLifecycleLog.Info("Agent restarted",
		"agent_id", id, "project_id", agentInfo.ProjectID,
		"name", agentInfo.Name, "slug", agentInfo.Slug,
		"phase", string(state.PhaseRunning),
		"container_status", agentInfo.ContainerStatus)

	s.forceHeartbeatAll("restart", id)

	agentResp := AgentInfoToResponse(*agentInfo)
	writeJSON(w, http.StatusAccepted, CreateAgentResponse{
		Agent:   &agentResp,
		Created: false,
	})
}

func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request, id, projectID string) {
	ctx := r.Context()

	ctx, span := tracer.Start(ctx, "broker.message.inject")
	defer span.End()
	span.SetAttributes(attribute.String("scion.agent.id", id))

	var req MessageRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	// Determine the message to deliver.
	// Empty messages (no body) are sent as an empty string, which the agent
	// manager delivers as a plain tmux Enter keypress to trigger confirmations.
	//
	// Phase 9b: when the hub has pre-rendered the delivery envelope,
	// deliver it verbatim. The broker performs no formatting.
	// Preference order:
	//   1. req.DeliveryText (top-level wire field, Phase 9b(i))
	//   2. req.StructuredMessage.DeliveryText (carrier on StructuredMessage)
	//   3. FormatForDelivery(req.StructuredMessage) (legacy fallback)
	//   4. req.Message (plain text fallback)
	var deliveryText string
	if req.DeliveryText != "" {
		deliveryText = req.DeliveryText
	} else if req.StructuredMessage != nil && req.StructuredMessage.DeliveryText != "" {
		deliveryText = req.StructuredMessage.DeliveryText
	} else if req.StructuredMessage != nil {
		deliveryText = messages.FormatForDelivery(req.StructuredMessage)
	} else {
		deliveryText = req.Message
	}

	// Resolve the correct manager for this agent (may be on an auxiliary runtime like K8s)
	mgr := s.resolveManagerForAgent(ctx, id, projectID)

	// Raw messages bypass the paste buffer and debounce, sending literal
	// bytes via tmux send-keys with no trailing Enter keypresses.
	isRaw := req.StructuredMessage != nil && req.StructuredMessage.Raw
	if isRaw {
		if err := mgr.MessageRaw(ctx, id, projectID, deliveryText); err != nil {
			span.SetStatus(codes.Error, err.Error())
			if strings.Contains(err.Error(), "not found") {
				NotFound(w, "Agent")
				return
			}
			RuntimeError(w, "Failed to send raw message: "+err.Error())
			return
		}
	} else {
		msgCtx := ctx
		if !req.Interrupt && req.MessageID != "" {
			// Non-interrupt messages are buffered and delivered after this
			// handler has already answered 200. Register a callback so a
			// later delivery failure is reported back to the hub, which
			// then marks the message failed instead of "dispatched" (#1820).
			failure := hubclient.MessageFailure{
				MessageID: req.MessageID,
				AgentID:   id,
				ProjectID: projectID,
			}
			if failure.ProjectID == "" {
				failure.ProjectID = req.ProjectID
			}
			connName := r.Header.Get("X-Scion-Hub-Connection")
			msgCtx = agent.WithDeliveryFailureHandler(ctx, func(deliveryErr error) {
				f := failure
				f.Reason = "broker delivery failed: " + deliveryErr.Error()
				go s.reportMessageFailure(connName, f)
			})
		}
		if err := mgr.Message(msgCtx, id, projectID, deliveryText, req.Interrupt); err != nil {
			span.SetStatus(codes.Error, err.Error())
			if strings.Contains(err.Error(), "not found") {
				NotFound(w, "Agent")
				return
			}
			RuntimeError(w, "Failed to send message: "+err.Error())
			return
		}
	}

	// Log message acceptance. Non-interrupt messages are buffered with a
	// debounce delay before actual tmux delivery, so we log "accepted"
	// rather than "delivered". Interrupt messages bypass the buffer and
	// are delivered immediately. Raw messages are always delivered immediately.
	logMsg := "message accepted (buffered)"
	if isRaw {
		logMsg = "message delivered (raw, unbuffered)"
	} else if req.Interrupt {
		logMsg = "message delivered (interrupt, unbuffered)"
	}
	logAttrs := []any{"agent_id", id}
	if req.ProjectID != "" {
		logAttrs = append(logAttrs, "project_id", req.ProjectID)
	}
	if req.StructuredMessage != nil {
		logAttrs = append(logAttrs, req.StructuredMessage.LogAttrs()...)
	}
	if s.dedicatedMessageLog != nil {
		s.dedicatedMessageLog.Info(logMsg, logAttrs...)
	} else {
		s.messageLog.Info(logMsg, logAttrs...)
	}

	w.WriteHeader(http.StatusOK)
}

// sendKeys is the dedicated broker handler for the agent-keys terminal-
// injection route (POST /api/v1/agents/{id}/keys), frozen by
// .design/agent-keys-contract.md §4. Per decision 1 (Option B), it shares no
// code with sendMessage above: it never constructs or logs a
// StructuredMessage, never calls mgr.Message/mgr.MessageRaw, and its
// delivery never goes through the message debounce buffer — only the
// dedicated mgr.SendKeys primitive.
//
// id is the agent slug (BrokerRoutePath's "{id}" path segment, resolved the
// same way every other broker route resolves it); projectID is the
// "projectId" query parameter. Both are also carried, redundantly, inside
// the decoded agentkeys.BrokerRequest body (ProjectID) — matching the
// existing MessageAgent convention of duplicating project_id into the body —
// but resolution uses the path/query values, exactly like every other
// broker route, not the body's copy.
func (s *Server) sendKeys(w http.ResponseWriter, r *http.Request, id, projectID string) {
	ctx := r.Context()

	ctx, span := tracer.Start(ctx, "broker.keys.inject")
	defer span.End()
	span.SetAttributes(attribute.String("scion.agent.slug", id))

	req, ok := readKeysRequest(w, r)
	if !ok {
		span.SetStatus(codes.Error, "invalid keys request body")
		return
	}

	// admittedAt anchors every subsequent audit line's duration, including
	// the validation/scope rejections below — ptone/scion#2184's execution
	// order requires content-free audit on denial/validation paths too,
	// wherever actor/target can already be established, not only on
	// post-admission outcomes.
	admittedAt := time.Now().UTC()

	// Reject malformed/oversized/empty key content before any resolution or
	// dispatch — the AC's "empty/NUL/oversize/invalid shapes never execute".
	// A plain (non-BrokerResult) envelope, at the status the contract's
	// Outcome table assigns the validation failure: neither
	// OutcomeInvalidRequest nor OutcomePayloadTooLarge is broker-assertable
	// (agentkeys.ValidBrokerOutcome), so this is not the broker deciding a
	// keys Outcome — it is the same kind of pre-admission validation
	// rejection the public route also performs, just re-checked here because
	// this handler is itself an execution point, not merely a relay.
	if verr := agentkeys.ValidateKeys(req.Keys); verr != nil {
		outcome := agentkeys.OutcomeInvalidRequest
		status := http.StatusBadRequest
		if ve, ok := agentkeys.AsValidationError(verr); ok {
			outcome = ve.Outcome
			if st, ok := agentkeys.HTTPStatus(ve.Outcome); ok {
				status = st
			}
		}
		span.SetStatus(codes.Error, "invalid keys value")
		s.logKeysOutcome(req, outcome, time.Since(admittedAt))
		writeError(w, status, ErrCodeInvalidRequest, "Invalid keys value", nil)
		return
	}

	// Require authoritative project and agent identity; reject unscoped
	// target fallback (#2193 scope). The query "projectId" is the value
	// every other broker route resolves against (matchesAgent's
	// project-label/field match); the body's project_id must agree with it
	// rather than silently winning on its own — a disagreement here would
	// mean the Hub's routing decision and its own dispatch payload disagree
	// about which project owns the target, which is exactly the ambiguity
	// "reject unscoped target fallback" exists to close, not something to
	// resolve by picking one side. AgentID must also be present: it is the
	// hard identity-binding input SendKeys requires (see
	// agentkeys.BrokerRequest.AgentID's doc comment) and an empty value
	// would default to no binding at all.
	if projectID == "" || req.ProjectID == "" || req.AgentID == "" || projectID != req.ProjectID {
		span.SetStatus(codes.Error, "invalid keys target scope")
		s.logKeysOutcome(req, agentkeys.OutcomeInvalidRequest, time.Since(admittedAt))
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "Invalid keys target scope", nil)
		return
	}

	// Admission check: cap the Hub-issued deadline at the frozen admission
	// window (#2193 scope, "Cap admission at 30 seconds/request deadline")
	// and reject an already-expired (or missing/invalid) result before doing
	// any further work — contract §4.2's first enforcement point ("at
	// broker admission"). The broker only ever shrinks this deadline, never
	// extends it (CapExecuteBefore's own contract).
	deadline, capErr := agentkeys.CapExecuteBefore(admittedAt, req.ExecuteBefore, agentkeys.DefaultAdmissionWindow)
	if capErr != nil || !admittedAt.Before(deadline) {
		span.SetStatus(codes.Error, "admission deadline expired")
		s.logKeysOutcome(req, agentkeys.OutcomeKeysUnavailable, time.Since(admittedAt))
		writeKeysResult(w, req.OperationID, agentkeys.OutcomeKeysUnavailable, "admission deadline already passed")
		return
	}

	// Bind ctx to the capped deadline so SendKeys's own internal checks
	// (after its target-lock wait, and immediately before Exec — contract
	// §4.2's remaining two enforcement points) observe it.
	execCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	mgr := s.resolveManagerForAgent(ctx, id, projectID)

	// projectID (the query value, already reconciled against req.ProjectID
	// above) is passed through, not req.ProjectID: SendKeys's own doc
	// comment requires resolution to use the path/query values like every
	// other broker route, and this is also the value the existing
	// message/interrupt injection path resolves its own injection-lock
	// scope against — passing the body's copy here even when the two agree
	// in value would still be the wrong field to depend on if that
	// reconciliation check above is ever weakened later.
	err := mgr.SendKeys(execCtx, projectID, id, req.AgentID, req.Keys)
	duration := time.Since(admittedAt)

	outcome := agentkeys.OutcomeDispatched
	switch {
	case err == nil:
		// success — outcome already set above.
	case errors.Is(err, agentkeys.ErrTargetNotFound):
		outcome = agentkeys.OutcomeNotFound
	case errors.Is(err, agentkeys.ErrAgentNotRunning):
		outcome = agentkeys.OutcomeAgentNotRunning
	case errors.Is(err, agentkeys.ErrTerminalNotReady):
		outcome = agentkeys.OutcomeTerminalNotReady
	case errors.Is(err, agent.ErrKeysUnsupported):
		// This backend does not support keys delivery.
		outcome = agentkeys.OutcomeKeysUnsupported
	case errors.Is(err, agent.ErrKeysNotStarted):
		// SendKeys wraps agent.ErrKeysNotStarted (by identity, via %w) only
		// at its own pre-delivery checkpoints — the lock wait, the post-lock
		// recheck, a readiness-probe failure that coincides with ctx expiry,
		// a ctx expiry discovered when the target re-verification's own
		// resolution fails, and the recheck immediately before delivery —
		// proven not to have started. (The tmux version gate, and a target
		// re-verification that resolves cleanly but finds a mismatch,
		// return ErrKeysUnsupported or agentkeys.ErrTargetNotFound instead,
		// handled by their own cases above.) Matching by
		// this sentinel's identity, rather than by
		// errors.Is(err, context.DeadlineExceeded/Canceled), is required:
		// the delivery-call failure path below deliberately
		// does not wrap its underlying error with %w, so a backend error
		// that happens to wrap a context error *after* the delivery call
		// began (e.g. a stream cancelled mid-call) can never match this
		// case by accident and be misreported as "definitely did not
		// start."
		outcome = agentkeys.OutcomeKeysUnavailable
	default:
		// An unclassified failure — including one where the tmux call may
		// have partially run. Never echo err.Error() into a log or
		// response: runtime stderr/argv can carry the injected keys
		// (contract §5). Respond with the ordinary (non-BrokerResult) error
		// envelope so a Hub-side adapter's "any other malformed/disagreeing
		// response" rule (agentkeys.BrokerOutcomeError's doc) classifies
		// this as keys_outcome_unknown, rather than the broker asserting an
		// outcome it is not positioned to decide.
		span.SetStatus(codes.Error, "keys dispatch failed")
		s.logKeysOutcome(req, "", duration)
		RuntimeError(w, "Failed to send keys")
		return
	}

	s.logKeysOutcome(req, outcome, duration)

	message := ""
	switch {
	case outcome == agentkeys.OutcomeKeysUnavailable && err != nil:
		message = "admission deadline expired before dispatch"
	case outcome == agentkeys.OutcomeKeysUnsupported:
		message = "this backend does not support keys delivery"
	}
	writeKeysResult(w, req.OperationID, outcome, message)
}

// readKeysRequest decodes a POST .../keys body into an agentkeys.BrokerRequest,
// bounding the read at agentkeys.MaxHTTPBodyBytes (#2193 scope, "Validate
// body/input limits") and rejecting unknown fields and trailing content —
// readJSON's bare json.Decode accepts both, which this route's execution
// authority does not warrant being lenient about. Writes the response itself
// and returns ok=false on any failure; callers must not do any further work
// in that case.
//
// This still relies on encoding/json's own (lenient) string/field decoding
// rather than agentkeys.ValidateBody's stricter token-stream approach — a
// duplicate "keys" field keeps the last occurrence, and "KEYS" matches
// case-insensitively. Contract §2.2/§5(b) forbid that leniency for the
// public request body; it is deliberately not extended to BrokerRequest
// here, for two reasons: first, this body is Hub-generated, not directly
// client-supplied, so this decode is a second, defense-in-depth check on an
// already-authorized, already-validated internal contract, not the
// authoritative validation boundary (the Hub already ran the strict check
// once); second, ValidateBody's decoder is shaped for the public
// single-string-field {"keys":...} envelope specifically, and BrokerRequest
// carries four additional fields including a time.Time, so reusing it
// as-is is not a direct fit. A key/agent/project-ID/operation-ID field is
// exceedingly unlikely to ever legitimately arrive twice or case-varied
// from the Hub's own encoder; the value-level checks below (ValidateKeys,
// the project/agent-ID presence and agreement check) still hold whatever
// this decode step produces.
func readKeysRequest(w http.ResponseWriter, r *http.Request) (agentkeys.BrokerRequest, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, agentkeys.MaxHTTPBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	var req agentkeys.BrokerRequest
	if err := dec.Decode(&req); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, ErrCodeInvalidRequest, "Keys request body too large", nil)
			return agentkeys.BrokerRequest{}, false
		}
		// Never echo the decode error verbatim: on this route it could in
		// principle quote a fragment of the request body, which may carry
		// the "keys" content itself (contract §5's redaction rule covers
		// "request JSON" explicitly).
		BadRequest(w, "Invalid keys request body")
		return agentkeys.BrokerRequest{}, false
	}
	// Reject trailing content after the single top-level JSON value: decode
	// again into a throwaway value and require io.EOF, the standard
	// encoding/json idiom for detecting extra bytes on a Decoder.
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		BadRequest(w, "Invalid keys request body")
		return agentkeys.BrokerRequest{}, false
	}
	return req, true
}

// logKeysOutcome writes a content-free audit line for a keys dispatch
// admission or outcome decision, following .design/agent-keys-contract.md
// §5's audit field list: never the keys payload, argv, runtime output, or
// any other input-bearing value. The broker-internal request carries no
// caller identity (agentkeys.BrokerRequest's doc comment — "strictly less
// than the public Request"), so actor kind/ID and credential ID/kind, which
// the full field list also calls for, are logged at the Hub layer (task
// 2.2) instead; this line covers exactly what is available on the broker
// side of the boundary.
//
// outcome == "" logs a generic warning for an unclassified failure the
// broker is not positioned to assert a contract Outcome for (see sendKeys's
// default case) rather than inventing one.
func (s *Server) logKeysOutcome(req agentkeys.BrokerRequest, outcome agentkeys.Outcome, duration time.Duration) {
	attrs := []any{
		"agent_id", req.AgentID,
		"project_id", req.ProjectID,
		"operation_id", req.OperationID,
		"route", "keys",
		"input_bytes", len(req.Keys),
		"duration", duration,
	}
	if outcome == "" {
		s.agentLifecycleLog.Warn("keys dispatch failed", attrs...)
		return
	}
	attrs = append(attrs, "outcome", string(outcome))
	s.agentLifecycleLog.Info("keys dispatch outcome", attrs...)
}

// writeKeysResult writes the frozen agentkeys.BrokerResult wire shape at the
// HTTP status the contract assigns to outcome (agentkeys.HTTPStatus).
// outcome must be OutcomeDispatched or one of the five broker-assertable
// failure outcomes (agentkeys.ValidBrokerOutcome) — see the contract §4.1
// "Outcome channel" paragraph. message must never contain key content,
// runtime argv, or runtime stdout/stderr (BrokerResult.Message's doc
// comment).
func writeKeysResult(w http.ResponseWriter, operationID string, outcome agentkeys.Outcome, message string) {
	status, ok := agentkeys.HTTPStatus(outcome)
	if !ok {
		// OutcomeDispatched has no HTTPStatus table entry (success has its
		// own 200 rather than an error status) — handle it explicitly
		// rather than trusting the table for the one value it deliberately
		// omits.
		status = http.StatusOK
	}
	writeJSON(w, status, agentkeys.BrokerResult{
		OperationID: operationID,
		Outcome:     outcome,
		Message:     message,
	})
}

func (s *Server) execCommand(w http.ResponseWriter, r *http.Request, id, projectID string) {
	ctx := r.Context()

	var req ExecRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if len(req.Command) == 0 {
		ValidationError(w, "command is required", nil)
		return
	}

	// Apply timeout if specified.
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(req.Timeout)*time.Second)
		defer cancel()
	}

	// Resolve the project-scoped container identifier (container ID for
	// docker/podman, pod name for k8s) and exec against the runtime whose
	// List call produced it, rather than a separately (and
	// non-deterministically) resolved one. Without this, rt.Exec resolves
	// the slug to a container across all projects and can target the wrong
	// agent — e.g. "scion look coordinator" in project A showing project
	// B's terminal — or, when the target and the runtime it executes
	// against are resolved by two independent lookups, dispatch a correct
	// container ID to the wrong runtime entirely. lookupAgentTarget pairs
	// the manager and runtime at the stage that produced the match, so
	// there is no independent runtime resolution to disagree with the
	// target. Mirrors the project-scoped lookup used by the PTY attach
	// handler and the single-lookup manager dispatch already applied to
	// stop/restart. lookupAgentTarget can return an empty identifier
	// without an error (e.g. a matching agent record with no resolvable
	// container), so guard against both — execing an empty target would
	// fall back to slug resolution inside the runtime and reintroduce the
	// cross-project collision.
	target, _, rt, err := s.lookupAgentTarget(ctx, id, projectID)
	if errors.Is(err, ErrAgentListUnavailable) {
		// The container runtime itself failed to respond, not "no such
		// agent" — tell the caller to retry rather than reporting the agent
		// missing (mirrors the PTY attach path in pty_handlers.go).
		AgentLookupUnavailable(w, err, id, "exec", "")
		return
	}
	// A lookup failure other than a genuine ErrAgentNotFound (e.g. an
	// ambiguous match) reflects a real problem resolving the agent and must
	// be surfaced as an error rather than reported as "not found", mirroring
	// projectScopedTarget's use by stopAgent and restartAgent. The
	// underlying err is logged server-side only, never echoed into the
	// response body.
	if err != nil && !errors.Is(err, ErrAgentNotFound) {
		s.agentLifecycleLog.Warn("Exec: lookup failed", "agent_id", id, "error", err)
		RuntimeError(w, "Failed to execute command")
		return
	}
	if err != nil || target == "" {
		NotFound(w, "Agent")
		return
	}
	if rt == nil {
		// Defensive only: lookupAgentTarget pairs a runtime with every
		// successful match, so this should be unreachable. Fail closed
		// rather than fall back to a default runtime that could differ
		// from the one the target actually came from.
		RuntimeUnavailable(w, "Agent runtime unavailable")
		return
	}

	output, err := rt.Exec(ctx, target, req.Command)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			NotFound(w, "Agent")
			return
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			writeJSON(w, http.StatusOK, ExecResponse{
				Output:   output,
				ExitCode: exitErr.ExitCode(),
			})
			return
		}
		RuntimeError(w, "Failed to execute command: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, ExecResponse{
		Output:   output,
		ExitCode: 0,
	})
}

// resetAuth writes a fresh token into a running agent's container and signals
// sciontool init (PID 1) to restart its token refresh loop via SIGUSR2.
func (s *Server) resetAuth(w http.ResponseWriter, r *http.Request, id, projectID string) {
	ctx := r.Context()

	var req ResetAuthRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if req.Token == "" {
		ValidationError(w, "token is required", nil)
		return
	}

	// The runtime whose List call produced the container ID is paired with
	// it by lookupAgentTarget itself, so the token write and the PID 1
	// signal below dispatch to the same backend the target came from
	// rather than a separately (and non-deterministically) resolved one —
	// see execCommand's matching comment for why that matters.
	target, _, rt, err := s.lookupAgentTarget(ctx, id, projectID)
	if errors.Is(err, ErrAgentListUnavailable) {
		// The container runtime itself failed to respond, not "no such
		// agent" — tell the caller to retry rather than reporting the agent
		// missing (mirrors the PTY attach path in pty_handlers.go).
		AgentLookupUnavailable(w, err, id, "reset_auth", "")
		return
	}
	// A lookup failure other than a genuine ErrAgentNotFound (e.g. an
	// ambiguous match) reflects a real problem resolving the agent and must
	// be surfaced as an error rather than reported as "not found", mirroring
	// projectScopedTarget's use by stopAgent and restartAgent. The
	// underlying err is logged server-side only, never echoed into the
	// response body.
	if err != nil && !errors.Is(err, ErrAgentNotFound) {
		s.agentLifecycleLog.Warn("Reset auth: lookup failed", "agent_id", id, "error", err)
		RuntimeError(w, "Failed to reset auth")
		return
	}
	if err != nil || target == "" {
		NotFound(w, "Agent")
		return
	}
	if rt == nil {
		// Defensive only: see execCommand's matching guard.
		RuntimeUnavailable(w, "Agent runtime unavailable")
		return
	}

	// Write the token to the canonical file atomically via temp+rename.
	// The token is delivered over the exec's stdin rather than embedded in
	// the script text: argv (including a heredoc body passed via `sh -c`)
	// becomes part of the outer host process's command line and is readable
	// via /proc/<pid>/cmdline for the lifetime of the exec, while stdin is
	// not. See #1355.
	writeCmd := []string{"sh", "-c",
		"TOKEN_DIR=\"$(getent passwd scion 2>/dev/null | cut -d: -f6 || echo /home/scion)/.scion\" && " +
			"mkdir -p \"$TOKEN_DIR\" && " +
			"cat > \"$TOKEN_DIR/scion-token.tmp\" && " +
			"mv \"$TOKEN_DIR/scion-token.tmp\" \"$TOKEN_DIR/scion-token\"",
	}

	if _, err := rt.ExecWithStdin(ctx, target, writeCmd, strings.NewReader(req.Token)); err != nil {
		s.agentLifecycleLog.Error("reset-auth: failed to write token file", "agent_id", id, "error", err)
		RuntimeError(w, "Failed to write token file: "+err.Error())
		return
	}

	// Signal sciontool init (PID 1) to re-read the token and restart its refresh
	// loop immediately. The token was already written above, and the agent also
	// polls the token file as a UID-safe fallback, so it recovers within a few
	// seconds even if this signal fails. In rootless containers the broker execs
	// as the scion user and `kill -USR2 1` against the root-owned PID 1 fails
	// with EPERM — this is expected and not an error since the token is on disk.
	signalCmd := []string{"kill", "-USR2", "1"}
	signaled := true
	if _, err := rt.Exec(ctx, target, signalCmd); err != nil {
		signaled = false
		s.agentLifecycleLog.Warn("reset-auth: failed to signal PID 1 (token still written, poller will reload)", "agent_id", id, "error", err)
	}

	s.agentLifecycleLog.Info("Auth reset completed", "agent_id", id, "signaled", signaled)

	s.forceHeartbeatAll("reset-auth", id)

	msg := "Auth reset: token written and init signaled"
	if !signaled {
		msg = "Auth reset: token written; signal failed (poller will reload)"
	}
	writeJSON(w, http.StatusOK, ResetAuthResponse{
		Message: msg,
	})
}

func (s *Server) getLogs(w http.ResponseWriter, r *http.Request, id, projectID string) {
	ctx := r.Context()

	// Resolve the correct manager for this agent (may be on an auxiliary runtime like K8s)
	mgr := s.resolveManagerForAgent(ctx, id, projectID)

	// Try to read agent.log from the filesystem first (preferred source).
	agents, err := mgr.List(ctx, map[string]string{"scion.agent": "true"})
	if err != nil {
		RuntimeError(w, "Failed to list agents: "+err.Error())
		return
	}

	var found *api.AgentInfo
	for i := range agents {
		if matchesAgent(agents[i], id, projectID) {
			found = &agents[i]
			break
		}
	}

	if found == nil {
		NotFound(w, "Agent")
		return
	}

	if found.ProjectPath != "" {
		agentSlug := found.Slug
		if agentSlug == "" {
			agentSlug = found.Name
		}
		agentLogPath := filepath.Join(config.GetAgentHomePath(
			found.ProjectPath, agentSlug,
		), "agent.log")
		if data, err := os.ReadFile(agentLogPath); err == nil {
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
			return
		}
		// Fall through to container logs if agent.log not found
	}

	// Fallback: read container stdout logs (resolve runtime for auxiliary runtimes)
	rt := s.resolveRuntimeForAgent(ctx, id, projectID)
	containerID := found.ContainerID
	if containerID == "" {
		containerID = id
	}
	logs, err := rt.GetLogs(ctx, containerID)
	if err != nil {
		if errors.Is(err, scionrt.ErrLogsNotSupported) {
			// The sentinel's own fixed text, never err.Error(): errors.Is also
			// matches a wrapped error, and a wrapping fmt.Errorf could carry a
			// runtime-specific scope or actor id that must never reach this
			// response body.
			RuntimeLogsUnsupported(w, scionrt.ErrLogsNotSupported.Error())
			return
		}
		RuntimeError(w, "Failed to get logs: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(logs))
}

func (s *Server) getStats(w http.ResponseWriter, r *http.Request, id, projectID string) {
	// TODO: Implement real stats from runtime
	// For now, return placeholder data
	writeJSON(w, http.StatusOK, StatsResponse{
		CPUUsagePercent:  0.0,
		MemoryUsageBytes: 0,
	})
}

// HasPromptResponse is the response for the has-prompt action.
type HasPromptResponse struct {
	HasPrompt bool `json:"hasPrompt"`
}

func (s *Server) checkAgentPrompt(w http.ResponseWriter, r *http.Request, id, projectID string) {
	ctx := r.Context()

	// Find the agent to get its project path
	agents, err := s.manager.List(ctx, map[string]string{"scion.agent": "true"})
	if err != nil {
		RuntimeError(w, "Failed to list agents: "+err.Error())
		return
	}

	var agent *api.AgentInfo
	for i := range agents {
		if matchesAgent(agents[i], id, projectID) {
			agent = &agents[i]
			break
		}
	}

	if agent == nil {
		NotFound(w, "Agent")
		return
	}

	if agent.ProjectPath == "" {
		// No project path means we can't check prompt.md
		writeJSON(w, http.StatusOK, HasPromptResponse{HasPrompt: false})
		return
	}

	// Check if prompt.md exists and has content. The mode (worktree vs
	// shared-workspace) isn't carried on the request, so probe both
	// locations via ResolveAgentDir.
	projectDir, _ := config.GetResolvedProjectDir(agent.ProjectPath)
	if projectDir == "" {
		projectDir = agent.ProjectPath
	}
	promptPath := filepath.Join(config.ResolveAgentDir(projectDir, agent.Name), "prompt.md")
	content, err := os.ReadFile(promptPath)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusOK, HasPromptResponse{HasPrompt: false})
			return
		}
		// Log the error but return false
		s.agentLifecycleLog.Warn("Failed to read prompt.md", "agent_id", id, "path", promptPath, "error", err)
		writeJSON(w, http.StatusOK, HasPromptResponse{HasPrompt: false})
		return
	}

	hasPrompt := len(strings.TrimSpace(string(content))) > 0
	writeJSON(w, http.StatusOK, HasPromptResponse{HasPrompt: hasPrompt})
}

// extractRequiredEnvKeys determines the set of env keys required by the agent's
// harness, auth type, and settings profile. It uses a multi-phase approach:
//
// Phase 1 (auth-aware): Resolves the harness type and auth_selected_type from
// on-disk harness-config and settings, then calls RequiredAuthEnvKeysFromConfig()
// to get intrinsic credential requirements for the (harness, authType) pair.
//
// Phase 2 (settings-based): Extracts keys with empty values from settings
// harness_configs[*].env and profiles[*].env, allowing users to declare custom
// env requirements.
//
// Phase 3 (secrets): Collects explicitly-declared secrets from settings and templates.
//
// hydratedHarnessConfigPath, when non-empty, points to a hub-hydrated harness-
// config directory that supplements the on-disk search. This allows env-gather
// to see auth metadata from hub-managed harness-configs that haven't been
// downloaded to the standard on-disk locations yet.
func (s *Server) extractRequiredEnvKeys(req CreateAgentRequest, hydratedTemplatePath string, hydratedHarnessConfigPath ...string) ([]string, map[string]api.SecretKeyInfo, map[string][]string, map[string]string) {
	required := make(map[string]struct{})
	alternatives := make(map[string][]string)
	// settingsEnvSatisfied holds the non-empty env values that settings
	// (harness_configs/harness_overrides) and the harness-config directory
	// contribute to the pod. Populated below when harnessConfigName resolves;
	// returned so callers can fold it into their own "is this key satisfied"
	// checks (see the outer env-gather check in CreateAgent).
	settingsEnvSatisfied := make(map[string]string)

	var settings *config.VersionedSettings
	settingsPath := req.ProjectPath
	if settingsPath == "" {
		// Fall back to the broker's global .scion directory for settings
		// resolution. This matches what agent.Start → GetResolvedProjectDir("")
		// does when projectPath is empty (e.g., hub-only git projects without a
		// linked local path on the broker).
		if globalDir, err := config.GetGlobalDir(); err == nil {
			settingsPath = globalDir
			if s.config.Debug {
				s.envSecretLog.Debug("extractRequiredEnvKeys: projectPath empty, using global dir",
					"globalDir", globalDir,
				)
			}
		}
	}
	if settingsPath != "" {
		vs, _, err := config.LoadEffectiveSettings(settingsPath)
		if err == nil {
			settings = vs
			if s.config.Debug {
				s.envSecretLog.Debug("extractRequiredEnvKeys: loaded settings",
					"path", settingsPath,
					"defaultHarnessConfig", vs.DefaultHarnessConfig,
					"harnessConfigCount", len(vs.HarnessConfigs),
				)
			}
		} else if s.config.Debug {
			s.envSecretLog.Debug("extractRequiredEnvKeys: failed to load settings",
				"path", settingsPath,
				"error", err.Error(),
			)
		}
	}

	profileName := ""
	if req.Config != nil {
		profileName = req.Config.Profile
	}
	if profileName == "" && settings != nil {
		profileName = settings.ActiveProfile
	}

	// Phase 1: Auth-aware env key extraction
	// Resolve harness type and auth_selected_type, then derive required keys.
	secretInfo := make(map[string]api.SecretKeyInfo)
	harnessConfigName := s.resolveHarnessConfigForEnvGather(req, settings)
	if s.config.Debug {
		s.envSecretLog.Debug("extractRequiredEnvKeys: harness resolution",
			"harnessConfigName", harnessConfigName,
			"hasSettings", settings != nil,
			"projectPath", req.ProjectPath,
		)
	}
	if harnessConfigName != "" {
		var harnessType, authType string
		// authMeta is the declarative auth block from the resolved harness-
		// config (Phase 1). When non-nil, the Phase-3 config-driven preflight
		// path is used; otherwise the broker falls back to the legacy compiled
		// per-harness tables in pkg/harness/auth.go.
		var authMeta *config.HarnessAuthMetadata
		// hcDirEnv is the resolved harness-config directory's own `env:`
		// block (hydrated or on-disk), captured alongside harnessType so it
		// can feed the launch-mirroring env merge below.
		var hcDirEnv map[string]string

		// Try on-disk harness-config directory first (check projectPath,
		// then fall back to global dir for hub-dispatched agents without a local project)
		harnessConfigSearchPath := req.ProjectPath
		if harnessConfigSearchPath == "" {
			harnessConfigSearchPath = settingsPath
		}
		// A template-bundled harness-config of the same name outranks the
		// project/global one: FindHarnessConfigDir checks template paths
		// first (pkg/config/harness_config.go), and launch resolves the
		// same template chain via resolveHarnessConfigDir
		// (pkg/agent/provision.go). Resolve it here too so the preflight
		// doesn't score a project/global `env:` block launch will not use.
		//
		// For a hub-dispatched agent, launch resolves the chain from the
		// hydrated template's local path, not the slug (start_context.go
		// replaces opts.Template with it before ProvisionAgent builds the
		// chain) — a hub-managed template of the same name as a stale local
		// one would otherwise be invisible here. hydratedTemplatePath is
		// that same path, hydrated by the caller (createAgent) alongside
		// the harness-config hydration below; it replaces the slug
		// whenever the caller supplied one, and GetTemplateChainInProject
		// resolves an absolute path directly (FindTemplateInProjectPath),
		// so this does not depend on the slug matching anything on disk.
		templateForChain := hydratedTemplatePath
		if templateForChain == "" && req.Config != nil {
			templateForChain = req.Config.Template
		}
		var harnessConfigTemplatePaths []string
		if templateForChain != "" && harnessConfigSearchPath != "" {
			if chain, err := config.GetTemplateChainInProject(templateForChain, harnessConfigSearchPath); err == nil {
				for _, tpl := range chain {
					harnessConfigTemplatePaths = append(harnessConfigTemplatePaths, tpl.Path)
				}
			}
		}
		if harnessConfigSearchPath != "" {
			if hcDir, err := config.FindHarnessConfigDir(harnessConfigName, harnessConfigSearchPath, harnessConfigTemplatePaths...); err == nil {
				harnessType = hcDir.Config.Harness
				authType = hcDir.Config.AuthSelectedType
				if hcDir.Config.Auth != nil {
					authMeta = hcDir.Config.Auth
				}
				hcDirEnv = hcDir.Config.Env
			}
		}

		// The hydrated hub-managed harness-config, when supplied, is what
		// launch actually reads: resolveHarnessConfigDir
		// (pkg/agent/provision.go) prefers the dispatch-context hydrated
		// copy unconditionally over an on-disk one of the same name — it
		// never merges the two. Harness/auth metadata still only falls back
		// to the hydrated copy when the on-disk search found nothing
		// (existing cascade), but hcDirEnv always takes the hydrated copy's
		// value (even absent) once a hydrated copy loads, so the preflight
		// never scores an on-disk `env:` block that launch will not use.
		if len(hydratedHarnessConfigPath) > 0 && hydratedHarnessConfigPath[0] != "" {
			if hcDir, err := config.LoadHarnessConfigDir(hydratedHarnessConfigPath[0]); err == nil && hcDir != nil {
				if harnessType == "" {
					harnessType = hcDir.Config.Harness
					authType = hcDir.Config.AuthSelectedType
					if hcDir.Config.Auth != nil {
						authMeta = hcDir.Config.Auth
					}
				}
				hcDirEnv = hcDir.Config.Env
			}
		}

		// Build the same layered env launch produces, so the preflight can
		// check satisfaction against exactly what the pod will receive:
		//  1. requestEnv: req.ResolvedEnv, then req.Config.Env overriding it
		//     — the same "opts.Env" launch starts from. Presence is kept
		//     even for an empty value, because resolveAuthEnvOverlay
		//     (pkg/agent/run.go) only fills a key that is entirely ABSENT
		//     from opts.Env; an explicit empty value here blocks
		//     resolveAuthEnvOverlay's fill of opts.Env. For an
		//     auth-candidate key the preflight treats this as blocking
		//     conservatively rather than as a launch-exact mirror — see
		//     authCandidateKeyValue.
		//  2. withSettings: requestEnv, with the resolved harness-config env
		//     (harness_configs.<h>.env plus
		//     profiles.<p>.harness_overrides.<h>.env, merged by
		//     ResolveHarnessConfig) filling only keys absent from requestEnv
		//     — mirroring resolveAuthEnvOverlay exactly.
		//  3. withDir: withSettings, with the harness-config directory's own
		//     env (pre-expanded once into expandedDirEnv, the way
		//     buildAgentEnv, pkg/agent/run.go, expands it: ${VAR}
		//     references, empty-after-expansion falls back to a host-env
		//     passthrough) filling only keys still absent — since the
		//     directory's env is the lowest-ranked source and only reaches
		//     the container via finalScionCfg.Env when nothing
		//     higher-ranked set the key.
		//
		// This "an empty entry blocks every lower layer" rule is correct for
		// an ordinary key, but NOT for a GCP-shared or harness-auth
		// "auth-candidate" key: run.go's Start deletes an empty auth-candidate
		// entry from opts.Env after auth resolves (its resolved.EnvVars only
		// ever carries non-empty values), clearing the way for
		// finalScionCfg.Env to supply the value instead. The auth-key-group
		// check below uses authCandidateKeySatisfied for that reason, rather
		// than withDir, for every key it evaluates.
		requestEnv := buildRequestEnv(req)
		rawSettingsEnv := rawSettingsHarnessEnv(settings, profileName, harnessConfigName)
		withSettings := fillAbsentEnv(requestEnv, rawSettingsEnv)
		// expandedDirEnv is built once so fillAbsentDirEnv and
		// authCandidateKeyValue consult the identical expanded view,
		// so both index the directory by the expanded key, as
		// buildAgentEnv does.
		expandedDirEnv := expandDirEnv(hcDirEnv)
		withDir := fillAbsentDirEnv(withSettings, expandedDirEnv)

		// Settings harness_configs entry can provide/override
		if settings != nil {
			if hcfg, ok := settings.HarnessConfigs[harnessConfigName]; ok {
				if harnessType == "" {
					harnessType = hcfg.Harness
				}
				if authType == "" {
					authType = hcfg.AuthSelectedType
				}
				if authMeta == nil && hcfg.Auth != nil {
					authMeta = hcfg.Auth
				}
			}
		}

		_ = harnessType // used only for cascade resolution; final value intentionally unused

		// Profile harness_overrides can override auth type
		if profileName != "" && settings != nil {
			if profile, ok := settings.Profiles[profileName]; ok {
				if override, ok := profile.HarnessOverrides[harnessConfigName]; ok {
					if override.AuthSelectedType != "" {
						authType = override.AuthSelectedType
					}
				}
			}
		}

		// Template-level auth_selectedType takes high precedence
		if req.Config != nil && req.Config.Template != "" && req.ProjectPath != "" {
			if tmpl, err := config.FindTemplateInProjectPath(req.Config.Template, req.ProjectPath); err == nil {
				if cfg, err := tmpl.LoadConfig(); err == nil && cfg != nil && cfg.AuthSelectedType != "" {
					authType = cfg.AuthSelectedType
				}
			}
		}

		// --harness-auth CLI flag takes ultimate precedence
		if req.Config != nil && req.Config.HarnessAuth != "" {
			authType = req.Config.HarnessAuth
		}

		// Determine if GCP credentials are available via identity config.
		// Both "assign" (broker-managed SA) and "passthrough" (ambient GCE
		// metadata) provide credentials, so either satisfies GCP auth needs.
		// This includes Kubernetes' own runtime-aware default of passthrough
		// when nothing is configured (ptone/scion#2328) — resolved the same
		// way buildStartContext resolves it at actual dispatch time
		// (resolveRuntimeNameForOpts mirrors resolveManagerForOpts,
		// effectiveGCPMetadataMode in start_context.go), so this preflight
		// and the dispatch agree on whether GCP credentials will be
		// available. Without this, an unconfigured Kubernetes agent using
		// ADC or vertex-ai could fail this preflight, or auto-detect the
		// wrong auth type, despite ADC actually being available once
		// dispatched.
		//
		// This preflight only ever runs on create (req is a
		// CreateAgentRequest), so, like buildStartContext's own early
		// resolution for a create dispatch, the profile here is
		// req.Config.Profile with no saved-profile fallback — there is
		// nothing to fall back to differently than buildStartContext would.
		//
		// resolveRuntimeNameForOpts, not resolveManagerForOpts: this only
		// needs the resolved runtime's name, not a manager to dispatch
		// through, and resolveManagerForOpts's settings-differs branch
		// builds and caches a real client (a Kubernetes or Cloud Run client,
		// for example) to get one. Calling that here would build the client
		// a second time (buildStartContext builds its own later) and
		// overwrite the auxiliary-runtime cache entry with a throwaway one.
		preflightProfile := ""
		if req.Config != nil {
			preflightProfile = req.Config.Profile
		}
		dispatchRuntimeName := s.resolveRuntimeNameForOpts(api.StartOptions{
			Name:        req.Name,
			ProjectPath: req.ProjectPath,
			Profile:     preflightProfile,
		})
		effectiveGCPMode := effectiveGCPMetadataMode(isKubernetesRuntimeName(dispatchRuntimeName), req.Config, req.ResolvedEnv)
		gcpSAAssigned := effectiveGCPMode == store.GCPMetadataModeAssign || effectiveGCPMode == store.GCPMetadataModePassthrough

		// When auth type is unset (auto-detect), check if resolved file secrets
		// or a GCP service account can satisfy an alternative auth method before
		// defaulting to api-key. This mirrors the auto-detect priority in each
		// harness's ResolveAuth.
		//
		// All auth preflight uses the config-driven path. AutoDetectAuthType
		// is safe to call with nil authMeta (returns "").
		if authType == "" {
			fileSecretNames := make(map[string]struct{})
			for _, sec := range req.ResolvedSecrets {
				if sec.Type == "file" {
					fileSecretNames[sec.Name] = struct{}{}
				}
			}
			// withSettings (requestEnv layered with the resolved settings
			// env) is the right view here, not withDir: launch's own
			// auto-detect (autoDetectAuthSelectedType, pkg/agent/run.go)
			// runs after resolveAuthEnvOverlay has filled opts.Env from
			// settings, but the harness-config directory's env never
			// reaches opts.Env — it only reaches finalScionCfg.Env, which
			// auto-detect does not see.
			resolvedEnvKeys := make(map[string]struct{})
			for k, v := range withSettings {
				if v != "" {
					resolvedEnvKeys[k] = struct{}{}
				}
			}
			for _, sec := range req.ResolvedSecrets {
				if sec.Type == "environment" || sec.Type == "" {
					target := sec.Target
					if target == "" {
						target = sec.Name
					}
					if target != "" {
						resolvedEnvKeys[target] = struct{}{}
					}
				}
			}
			// Include as_needed secret targets the hub could resolve in
			// pass 2. Without this, autodetect cannot see deferred keys
			// and falls back to default_type, losing the credentials (#1447).
			for _, k := range req.AvailableAsNeededKeys {
				resolvedEnvKeys[k] = struct{}{}
			}
			// AutoDetectAuthType runs the file -> env -> identity chain as a
			// single call so a present default-type credential (e.g.
			// ANTHROPIC_API_KEY for claude) always wins over the identity
			// leg — shared with pkg/agent's Start(), which uses the same
			// function to select the auth type actually wired into the
			// container.
			if detected := harness.AutoDetectAuthType(authMeta, fileSecretNames, resolvedEnvKeys, gcpSAAssigned); detected != "" {
				authType = detected
			}
		}

		// Resolve auth key groups and check satisfaction
		keyGroups := harness.RequiredAuthEnvKeysFromConfig(authMeta, authType)
		if len(keyGroups) > 0 {
			// Every key here is, by construction, an auth-candidate key (a
			// GCP shared name, or one of this harness's own required_env
			// names), so authCandidateKeySatisfied's launch-mirroring
			// fall-through (see its doc comment) applies to all of them —
			// unlike withDir, which is correct for an ordinary key but not
			// for these.
			secretEnvKeys := make(map[string]struct{})
			for _, sec := range req.ResolvedSecrets {
				if sec.Type == "environment" || sec.Type == "" {
					target := sec.Target
					if target == "" {
						target = sec.Name
					}
					if target != "" {
						secretEnvKeys[target] = struct{}{}
					}
				}
			}

			for _, group := range keyGroups {
				satisfied := false
				for _, key := range group {
					if _, ok := secretEnvKeys[key]; ok {
						satisfied = true
						break
					}
					if authCandidateKeySatisfied(key, requestEnv, rawSettingsEnv, expandedDirEnv) {
						satisfied = true
						break
					}
				}
				if !satisfied {
					// Add the canonical (first) key as required
					canonicalKey := group[0]
					required[canonicalKey] = struct{}{}
					secretInfo[canonicalKey] = api.SecretKeyInfo{Source: "auth"}
					// Record alternative key names so the hub can match
					// as_needed env vars stored under non-canonical names.
					if len(group) > 1 {
						alternatives[canonicalKey] = group[1:]
					}
				}
			}
		}

		// settingsEnvSatisfied is what the outer needs/hubHas check
		// (createAgent) folds into its own requestEnv-based env, to fill
		// keys requestEnv did not already decide (and only those — an empty
		// ResolvedEnv/Config.Env entry is never overwritten there). Built
		// per-key rather than uniformly from withDir, because an
		// auth-candidate key (GOOGLE_CLOUD_PROJECT and friends, or one of
		// this harness's own required_env names — see
		// authCandidateKeySatisfied) needs the same launch-mirroring
		// fall-through the auth-key-group check above uses: a Phase 2/3
		// requirement can name an auth-candidate key too (e.g. a settings
		// harness_configs entry other than the selected one declaring
		// GOOGLE_CLOUD_PROJECT empty), and that requirement is checked by
		// the outer code, not by the keyGroups loop above.
		authKeys := authCandidateEnvKeys(authMeta)
		settingsEnvSatisfied = make(map[string]string)
		for k := range withDir {
			if _, existed := requestEnv[k]; existed {
				continue
			}
			if _, isAuthKey := authKeys[k]; isAuthKey {
				if v, satisfied := authCandidateKeyValue(k, requestEnv, rawSettingsEnv, expandedDirEnv); satisfied {
					settingsEnvSatisfied[k] = v
				}
				continue
			}
			if v := withDir[k]; v != "" {
				settingsEnvSatisfied[k] = v
			}
		}

		// Phase 1b: Auth-required file secrets (e.g. ADC for vertex-ai).
		// When a GCP service account is assigned, the metadata server provides
		// credentials, so the ADC file is not required.
		authSecrets := harness.RequiredAuthSecretsFromConfig(authMeta, authType, gcpSAAssigned)
		if len(authSecrets) > 0 {
			// Build lookup of file-type resolved secrets by Name and Target suffix
			fileSecrets := make(map[string]struct{})
			for _, sec := range req.ResolvedSecrets {
				if sec.Type == "file" {
					fileSecrets[sec.Name] = struct{}{}
					if sec.Target != "" {
						fileSecrets[sec.Target] = struct{}{}
					}
				}
			}

			for _, as := range authSecrets {
				if _, ok := fileSecrets[as.Key]; !ok {
					// Check if any alternative env keys satisfy this requirement
					// via resolved env, inline config env, or process env (the
					// latter covers workstation mode where PR #719 sets
					// GOOGLE_APPLICATION_CREDENTIALS at startup).
					altSatisfied := false
					for _, altKey := range as.AlternativeEnvKeys {
						if v, ok := req.ResolvedEnv[altKey]; ok && v != "" {
							altSatisfied = true
							break
						}
						if v := os.Getenv(altKey); v != "" {
							altSatisfied = true
							break
						}
					}
					if !altSatisfied {
						required[as.Key] = struct{}{}
						secretInfo[as.Key] = api.SecretKeyInfo{
							Description: as.Description,
							Source:      "auth",
							Type:        "file",
						}
					}
				}
			}
		}
	}

	// Phase 2: Settings-based empty-value env key extraction
	if settings != nil {
		// Get profile harness override env keys
		if profileName != "" && settings.Profiles != nil {
			if profile, ok := settings.Profiles[profileName]; ok {
				for _, override := range profile.HarnessOverrides {
					for k, v := range override.Env {
						if v == "" {
							required[k] = struct{}{}
						}
					}
				}
			}
		}

		// Get harness config env keys
		for _, hcfg := range settings.HarnessConfigs {
			for k, v := range hcfg.Env {
				if v == "" {
					required[k] = struct{}{}
				}
			}
		}
	}

	// Phase 3: Secrets declarations from settings and template

	// 3a: Settings-derived empty-value env keys are secret-eligible
	for k := range required {
		if _, exists := secretInfo[k]; !exists {
			secretInfo[k] = api.SecretKeyInfo{Source: "settings"}
		}
	}

	// 3b: Settings harness_configs[*].secrets
	if settings != nil {
		for _, hcfg := range settings.HarnessConfigs {
			for _, sec := range hcfg.Secrets {
				required[sec.Key] = struct{}{}
				secretInfo[sec.Key] = api.SecretKeyInfo{
					Description: sec.Description,
					Source:      "settings",
					Type:        sec.Type,
				}
			}
		}

		// 3c: Profile secrets
		if profileName != "" && settings.Profiles != nil {
			if profile, ok := settings.Profiles[profileName]; ok {
				for _, sec := range profile.Secrets {
					required[sec.Key] = struct{}{}
					secretInfo[sec.Key] = api.SecretKeyInfo{
						Description: sec.Description,
						Source:      "settings",
						Type:        sec.Type,
					}
				}
			}
		}
	}

	// 3d: Template secrets (from request or local template)
	for _, sec := range req.RequiredSecrets {
		required[sec.Key] = struct{}{}
		secretInfo[sec.Key] = api.SecretKeyInfo{
			Description: sec.Description,
			Source:      "template",
			Type:        sec.Type,
		}
	}
	// Also try loading local template config
	if req.Config != nil && req.Config.Template != "" && req.ProjectPath != "" {
		if tmpl, err := config.FindTemplateInProjectPath(req.Config.Template, req.ProjectPath); err == nil {
			if cfg, err := tmpl.LoadConfig(); err == nil && cfg != nil {
				for _, sec := range cfg.Secrets {
					required[sec.Key] = struct{}{}
					if _, exists := secretInfo[sec.Key]; !exists {
						secretInfo[sec.Key] = api.SecretKeyInfo{
							Description: sec.Description,
							Source:      "template",
							Type:        sec.Type,
						}
					}
				}
			}
		}
	}

	// Hub-only keys (TZ) are never required: an empty value means "unset",
	// never "ask". Only the hub supplies them for a hub-dispatched agent, so
	// asking for one would let a laptop or parent agent fill it in.
	for k := range required {
		if agent.IsHubOnlyEnvKey(k) {
			delete(required, k)
		}
	}
	for k := range secretInfo {
		if agent.IsHubOnlyEnvKey(k) {
			delete(secretInfo, k)
		}
	}
	for k := range alternatives {
		if agent.IsHubOnlyEnvKey(k) {
			delete(alternatives, k)
		}
	}

	keys := make([]string, 0, len(required))
	for k := range required {
		keys = append(keys, k)
	}
	return keys, secretInfo, alternatives, settingsEnvSatisfied
}

// buildRequestEnv returns req.ResolvedEnv overridden per-key by req.Config.Env
// ("KEY=VALUE" entries) — the same starting point launch uses for opts.Env
// before resolveAuthEnvOverlay (pkg/agent/run.go) runs. Presence is kept even
// for an empty value: an explicit empty entry here blocks
// resolveAuthEnvOverlay's fill of opts.Env. For an auth-candidate key the
// preflight treats this as blocking conservatively rather than as a
// launch-exact mirror — see authCandidateKeyValue.
func buildRequestEnv(req CreateAgentRequest) map[string]string {
	env := make(map[string]string, len(req.ResolvedEnv))
	for k, v := range req.ResolvedEnv {
		env[k] = v
	}
	if req.Config != nil {
		for _, e := range req.Config.Env {
			parts := strings.SplitN(e, "=", 2)
			if len(parts) == 2 {
				env[parts[0]] = parts[1]
			}
		}
	}
	return env
}

// rawSettingsHarnessEnv returns the resolved harness-config env
// (harness_configs.<h>.env plus profiles.<p>.harness_overrides.<h>.env,
// merged by settings.ResolveHarnessConfig) unfiltered — including any
// explicitly empty values, which matter to fillAbsentEnv's presence check.
func rawSettingsHarnessEnv(settings *config.VersionedSettings, profileName, harnessConfigName string) map[string]string {
	if settings == nil || harnessConfigName == "" {
		return nil
	}
	hcEntry, err := settings.ResolveHarnessConfig(profileName, harnessConfigName)
	if err != nil {
		return nil
	}
	return hcEntry.Env
}

// fillAbsentEnv returns base with each key from fill added, but only when
// base does not already contain that key — even an empty value in base
// counts as present and blocks the fill. This mirrors resolveAuthEnvOverlay
// (pkg/agent/run.go), which writes a settings-resolved env value into
// opts.Env only when the key is not already there.
func fillAbsentEnv(base, fill map[string]string) map[string]string {
	merged := make(map[string]string, len(base)+len(fill))
	for k, v := range base {
		merged[k] = v
	}
	for k, v := range fill {
		if _, exists := merged[k]; !exists {
			merged[k] = v
		}
	}
	return merged
}

// fillAbsentDirEnv is fillAbsentEnv for the harness-config directory's own
// env. expandedDirEnv must already be expanded (expandDirEnv) — callers
// share one expanded view between this function and authCandidateKeyValue
// rather than each re-deriving it, so both agree on what a dir key expands
// to.
func fillAbsentDirEnv(base, expandedDirEnv map[string]string) map[string]string {
	merged := make(map[string]string, len(base)+len(expandedDirEnv))
	for k, v := range base {
		merged[k] = v
	}
	for k, v := range expandedDirEnv {
		if _, exists := merged[k]; !exists {
			merged[k] = v
		}
	}
	return merged
}

// expandDirEnv expands every harness-config-directory env entry once, the
// way buildAgentEnv expands them (expandDirEnvEntry), so callers that need
// more than one view of the directory's env (fillAbsentDirEnv,
// authCandidateKeyValue) consult the identical result instead of each
// re-deriving it per key. An entry expandDirEnvEntry drops (an empty
// expanded key, or an unresolved variable reference) is simply absent here.
func expandDirEnv(dirEnv map[string]string) map[string]string {
	expanded := make(map[string]string, len(dirEnv))
	for k, v := range dirEnv {
		expandedKey, expandedValue, ok := expandDirEnvEntry(k, v)
		if !ok {
			continue
		}
		expanded[expandedKey] = expandedValue
	}
	return expanded
}

// expandDirEnvEntry mirrors buildAgentEnv's treatment of a single
// harness-config-directory env entry (pkg/agent/run.go): the key and value
// may reference ${VAR}; a value left empty after expansion is an implicit
// host-env passthrough. ok is false when the entry should be dropped
// entirely — an empty expanded key, or an unresolved variable reference that
// buildAgentEnv would also warn about and skip.
func expandDirEnvEntry(key, value string) (expandedKey, expandedValue string, ok bool) {
	expandedKey, _ = quietExpandEnv(key)
	if expandedKey == "" {
		return "", "", false
	}
	var warned bool
	expandedValue, warned = quietExpandEnv(value)
	if expandedValue == "" {
		if warned {
			return "", "", false
		}
		if hostVal, hasHost := os.LookupEnv(expandedKey); hasHost && hostVal != "" {
			expandedValue = hostVal
		}
	}
	return expandedKey, expandedValue, true
}

// quietExpandEnv mirrors util.ExpandEnv (pkg/util/env.go: os.Expand with an
// os.LookupEnv closure) without its side-effecting stderr warning. The
// preflight may evaluate the same ${VAR} dir-env reference on every
// gather-env request, including ones that end in 202 and are retried, and
// launch prints the same warning again when it expands the value for real —
// so the preflight's own copy would be pure duplicate noise on the broker's
// stderr, not new information.
func quietExpandEnv(s string) (expanded string, warned bool) {
	expanded = os.Expand(s, func(key string) string {
		val, ok := os.LookupEnv(key)
		if !ok {
			warned = true
			return ""
		}
		return val
	})
	return expanded, warned
}

// authCandidateKeyValue returns the value key — a GCP-shared name or one of
// the harness's own auth required_env names — would have in the container,
// resolved the way launch actually resolves it, which differs from an
// ordinary key's requestEnv/settings/dir layering (withDir,
// fillAbsentDirEnv). satisfied reports whether that value is non-empty.
// expandedDirEnv must already be expanded (expandDirEnv), the same map
// fillAbsentDirEnv consults, so both agree on what a dir key expands to.
//
// After auth resolves, run.go's Start deletes any such key from opts.Env
// whenever its value did not make it into the resolved auth's EnvVars —
// which only ever carries non-empty values (pkg/agent/run.go, the
// isAuthEnvKey / configAuthEnvKeySet deletion loop; copied here rather than
// imported to avoid depending on that package's unexported details — keep
// in sync if that list changes). So an empty settings value for one of
// these keys does not block the directory the way it would for an ordinary
// key: deleting it from opts.Env clears the way for finalScionCfg.Env — the
// directory, or, when the directory has no entry, the settings value
// itself, both expanded the way buildAgentEnv expands them — to supply the
// container's value.
//
// An empty requestEnv (ResolvedEnv/Config.Env) entry is still treated as
// blocking, conservatively, rather than as a launch-exact mirror: at launch
// the same deletion applies to an empty opts.Env entry regardless of where
// it came from, so the pod may still receive the settings or directory
// value — or the broker process's own value, via host passthrough — even
// when requestEnv itself is empty. In a real Hub dispatch, though, such an
// entry usually also comes from template or inline config env, which also
// outranks the directory and settings inside finalScionCfg, so the
// container would usually end up empty anyway — and the preflight has no
// way to tell that case apart from one where it would not. Getting this
// wrong can only produce a false-missing report (the preflight asks for a
// value the pod would have had), never a false-satisfied one.
func authCandidateKeyValue(key string, requestEnv, rawSettingsEnv, expandedDirEnv map[string]string) (value string, satisfied bool) {
	if v, ok := requestEnv[key]; ok {
		return v, v != ""
	}
	if v, ok := rawSettingsEnv[key]; ok && v != "" {
		return v, true
	}
	if v, ok := expandedDirEnv[key]; ok {
		return v, v != ""
	}
	if v, ok := rawSettingsEnv[key]; ok {
		_, expanded, ok := expandDirEnvEntry(key, v)
		return expanded, ok && expanded != ""
	}
	return "", false
}

// authCandidateKeySatisfied is authCandidateKeyValue's satisfaction check,
// for callers that only need the bool.
func authCandidateKeySatisfied(key string, requestEnv, rawSettingsEnv, expandedDirEnv map[string]string) bool {
	_, satisfied := authCandidateKeyValue(key, requestEnv, rawSettingsEnv, expandedDirEnv)
	return satisfied
}

// authCandidateEnvKeys returns the set of env keys run.go's Start treats as
// "auth-candidate" for the empty-value deletion authCandidateKeyValue
// describes: the GCP shared names, plus every required_env name declared
// across the harness's auth metadata (mirrors pkg/agent's
// configAuthEnvKeySet, copied for the same reason as authCandidateKeyValue).
func authCandidateEnvKeys(authMeta *config.HarnessAuthMetadata) map[string]struct{} {
	keys := map[string]struct{}{
		"GOOGLE_CLOUD_PROJECT":        {},
		"GCP_PROJECT":                 {},
		"ANTHROPIC_VERTEX_PROJECT_ID": {},
		"GOOGLE_CLOUD_REGION":         {},
		"CLOUD_ML_REGION":             {},
		"GOOGLE_CLOUD_LOCATION":       {},
	}
	if authMeta != nil {
		for _, authType := range authMeta.Types {
			for _, req := range authType.RequiredEnv {
				for _, k := range req.AnyOf {
					keys[k] = struct{}{}
				}
			}
		}
	}
	return keys
}

// resolveHarnessConfigForEnvGather determines the harness-config name for the
// env-gather flow (pre-provisioning secret key extraction). It uses the unified
// config.ResolveHarnessConfigName with a broker-specific fallback: if the
// template name matches a valid harness-config directory or settings entry,
// it is used as the harness-config name.
func (s *Server) resolveHarnessConfigForEnvGather(req CreateAgentRequest, settings *config.VersionedSettings) string {
	// Broker-specific: treat template name as harness-config if it matches a
	// known harness-config directory or settings entry.
	cliFlag := ""
	if req.Config != nil {
		cliFlag = req.Config.HarnessConfig
	}
	if cliFlag == "" && req.Config != nil && req.Config.Template != "" {
		tpl := req.Config.Template
		if req.ProjectPath != "" {
			if _, err := config.FindHarnessConfigDir(tpl, req.ProjectPath); err == nil {
				cliFlag = tpl
			}
		}
		if cliFlag == "" && settings != nil {
			if _, ok := settings.HarnessConfigs[tpl]; ok {
				cliFlag = tpl
			}
		}
	}

	profileName := ""
	if req.Config != nil {
		profileName = req.Config.Profile
	}

	res, err := config.ResolveHarnessConfigName(config.HarnessConfigInputs{
		CLIFlag:     cliFlag,
		Settings:    settings,
		ProfileName: profileName,
	})
	if err != nil {
		return ""
	}
	return res.Name
}

func hasAgentInProjectOrUnlabeled(agents []api.AgentInfo, projectID string) bool {
	for _, info := range agents {
		if matchesAgentProject(info, projectID) {
			return true
		}
	}
	return false
}

// resolveAgentRuntimeTarget finds the manager/runtime pair that contains an
// existing agent. Keeping the pair together prevents manager-based and direct
// runtime operations from drifting to different backends.
func (s *Server) resolveAgentRuntimeTarget(ctx context.Context, id, projectID string) (agent.Manager, scionrt.Runtime) {
	slug := strings.ToLower(id)
	filter := map[string]string{"scion.name": slug}
	if projectID != "" {
		filter["scion.project_id"] = projectID
	}

	// Try the default runtime first.
	agents, err := s.manager.List(ctx, filter)
	if err == nil && len(agents) > 0 {
		return s.manager, s.runtime
	}

	// Snapshot the auxiliary runtimes, sorted by identity, so manager calls
	// happen without the lock and in a deterministic order — more than one
	// auxiliary runtime can share a type (see auxiliaryRuntimeIdentity).
	auxRuntimes := s.sortedAuxiliaryRuntimes()

	for _, aux := range auxRuntimes {
		auxAgents, auxErr := aux.Manager.List(ctx, filter)
		if auxErr == nil && len(auxAgents) > 0 {
			return aux.Manager, aux.Runtime
		}
	}

	// If project-scoped lookup found nothing, retry without project filter.
	// This supports pre-existing containers and solo/CLI mode.
	if projectID != "" {
		fallbackFilter := map[string]string{"scion.name": slug}
		agents, err = s.manager.List(ctx, fallbackFilter)
		if err == nil && hasAgentInProjectOrUnlabeled(agents, projectID) {
			return s.manager, s.runtime
		}
		for _, aux := range auxRuntimes {
			auxAgents, auxErr := aux.Manager.List(ctx, fallbackFilter)
			if auxErr == nil && hasAgentInProjectOrUnlabeled(auxAgents, projectID) {
				return aux.Manager, aux.Runtime
			}
		}
	}

	// Default fallback — the agent may have already been removed or the
	// runtime is genuinely the default one (e.g. pod already deleted).
	return s.manager, s.runtime
}

// resolveManagerForAgent returns the manager for an existing agent so
// lifecycle operations target the backend where that agent is running.
func (s *Server) resolveManagerForAgent(ctx context.Context, id, projectID string) agent.Manager {
	manager, _ := s.resolveAgentRuntimeTarget(ctx, id, projectID)
	return manager
}

// allManagers returns the default manager plus every distinct auxiliary
// runtime's manager, in deterministic (sorted-by-identity) order. Used by
// resolveDeleteTarget to search every registered runtime rather than only
// the one a slug-based lookup happens to resolve to first.
func (s *Server) allManagers() []agent.Manager {
	managers := []agent.Manager{s.manager}
	s.auxiliaryRuntimesMu.RLock()
	auxNames := make([]string, 0, len(s.auxiliaryRuntimes))
	for name := range s.auxiliaryRuntimes {
		auxNames = append(auxNames, name)
	}
	sort.Strings(auxNames)
	for _, name := range auxNames {
		if aux := s.auxiliaryRuntimes[name]; aux.Manager != nil && aux.Manager != s.manager {
			managers = append(managers, aux.Manager)
		}
	}
	s.auxiliaryRuntimesMu.RUnlock()
	return managers
}

// resolveRuntimeForAgent returns the runtime for direct operations such as
// exec and log retrieval.
func (s *Server) resolveRuntimeForAgent(ctx context.Context, id, projectID string) scionrt.Runtime {
	_, runtime := s.resolveAgentRuntimeTarget(ctx, id, projectID)
	return runtime
}

// resolveRuntimeNameForOpts returns the settings-declared runtime type opts
// would resolve to (e.g. "docker", "kubernetes", "remote"), without
// resolveManagerForOpts's side effects when the resolved runtime differs
// from the broker's default: building a real runtime client (a Kubernetes or
// Cloud Run client, for example, via resolveAuxiliaryRuntime) and caching it in
// auxiliaryRuntimes. Callers that only need the name — currently just the
// auth preflight (extractRequiredEnvKeys) — use this instead, so a create to
// a non-default runtime does not build and cache a throwaway client purely
// to read its name; buildStartContext's own later, authoritative resolution
// (via resolveManagerForOpts) builds the real one.
//
// This mirrors resolveManagerForOpts's own ForceRuntime-then-settings logic
// rather than sharing it, specifically so resolveManagerForOpts itself can
// stay identical to its upstream counterpart. Keep the two in sync by hand:
// a change to one of ForceRuntime handling, the settings load, or
// vs.ResolveRuntime's call here should be mirrored in the other.
//
// Unlike resolveManagerForOpts, this never calls runtime.GetRuntime
// (factory.go), so it returns settings.yaml's raw type rather than
// GetRuntime's normalized or auto-detected name. The two names can differ —
// "remote"/"k8s" where GetRuntime would normalize to "kubernetes",
// "local"/"auto" where GetRuntime would auto-detect a concrete runtime, a
// Cloud Run override of a "docker" profile, or an unrecognized type string —
// but isKubernetesRuntimeName classifies all of these identically either
// way. The one case where the classification itself can differ is a
// Kubernetes-type profile whose client fails to build: GetRuntime then
// returns the sentinel type "error" (not Kubernetes), while this function
// still returns the original Kubernetes-type string, so the preflight sees
// Kubernetes (passthrough) where the real dispatch would not. This is
// harmless: that dispatch fails on the unusable "error" runtime regardless,
// so the caller never runs with credentials the preflight shouldn't have
// promised — it only turns a would-be preflight message into a later
// runtime-error message instead.
func (s *Server) resolveRuntimeNameForOpts(opts api.StartOptions) string {
	if s.config.ForceRuntime != "" {
		if s.config.ForceRuntime == s.runtime.Name() {
			return s.runtime.Name()
		}
		if aux, ok := s.findAuxiliaryRuntimeByType(s.config.ForceRuntime); ok {
			return aux.Runtime.Name()
		}
		s.agentLifecycleLog.Warn("ForceRuntime does not match default runtime, falling back to settings resolution", "force", s.config.ForceRuntime, "default", s.runtime.Name())
	}

	projectDir, _ := config.GetResolvedProjectDir(opts.ProjectPath)
	vs, _, err := config.LoadEffectiveSettings(projectDir)
	if err != nil {
		s.agentLifecycleLog.Warn("failed to load project settings for runtime resolution; using broker default runtime",
			"projectDir", projectDir, "error", err)
	}
	if vs == nil {
		return s.runtime.Name()
	}

	// ResolveRuntime("") uses vs.ActiveProfile as the fallback.
	_, runtimeType, err := vs.ResolveRuntime(opts.Profile)
	if err != nil {
		// Profile or its runtime not found in settings; use default
		return s.runtime.Name()
	}
	return runtimeType
}

// resolveManagerForOpts returns the appropriate agent.Manager for the given
// start options, along with the name of the runtime type it resolved to
// (e.g. "docker", "kubernetes" — the same string runtime.Runtime.Name()
// returns). It loads the project's settings to determine the effective
// runtime. If the resolved runtime differs from the broker's default, a
// temporary manager is created and cached. Otherwise the broker's shared
// manager is returned.
//
// The resolved runtime-type string is also the single source of truth for
// every runtime-conditional decision made elsewhere for the same dispatch
// (e.g. buildStartContext's Kubernetes/GCP-identity-mode check,
// start_context.go, ptone/scion#2328). A broker can register more than one
// profile (e.g. a "docker" and a "kubernetes" profile on the same broker),
// so the profile this specific dispatch names — not the broker's default
// runtime — decides which one is actually used. buildStartContext itself
// calls this once and reuses the result for both the GCP-identity check and
// the manager it returns; start/restart call it again after their own,
// later saved-profile resolution (handlers.go), since that can resolve
// differently from buildStartContext's own earlier call. The auth preflight
// needs only the name, not a manager — see resolveRuntimeNameForOpts, above.
//
// When opts.Profile is empty, the project's active profile (from settings.yaml)
// is used. This ensures the broker respects the project's configured runtime
// even when no explicit --profile flag is passed.
func (s *Server) resolveManagerForOpts(opts api.StartOptions) (agent.Manager, string) {
	if s.config.ForceRuntime != "" {
		if s.config.ForceRuntime == s.runtime.Name() {
			// A ForceRuntime naming the default runtime returns s.manager
			// here, bypassing the per-profile resolution below entirely —
			// a second profile of the same runtime type with its own
			// runtime config is not reachable under ForceRuntime.
			return s.manager, s.runtime.Name()
		}
		if aux, ok := s.findAuxiliaryRuntimeByType(s.config.ForceRuntime); ok {
			return aux.Manager, aux.Runtime.Name()
		}
		s.agentLifecycleLog.Warn("ForceRuntime does not match default runtime, falling back to settings resolution", "force", s.config.ForceRuntime, "default", s.runtime.Name())
	}

	// Load settings to check if the profile/active-profile specifies a
	// different runtime than the broker's auto-detected default. Any
	// decode error is logged instead of being swallowed: resolution falls
	// back to the broker's default runtime either way, but a malformed
	// settings file should leave a trace.
	projectDir, _ := config.GetResolvedProjectDir(opts.ProjectPath)
	vs, _, err := config.LoadEffectiveSettings(projectDir)
	if err != nil {
		s.agentLifecycleLog.Warn("failed to load project settings for runtime resolution; using broker default runtime",
			"projectDir", projectDir, "error", err)
	}
	if vs == nil {
		return s.manager, s.runtime.Name()
	}

	// ResolveRuntime("") uses vs.ActiveProfile as the fallback. The
	// settings-level type (plus, for Kubernetes, context/namespace) feeds
	// the cheap pre-check just below, but that pre-check is a conservative
	// shortcut: it can only PROVE a match, never a mismatch, and a false
	// result falls through to full resolution rather than being treated as
	// conclusive. The authoritative comparison is by resolved IDENTITY (see
	// auxiliaryRuntimeIdentity) after that resolution, which is what
	// actually handles aliased type spellings ("k8s" vs "kubernetes") and
	// tells apart a profile of the SAME type but a genuinely different
	// instance (e.g. a different Kubernetes cluster, when the broker's own
	// default is also Kubernetes).
	rtConfig, runtimeType, err := vs.ResolveRuntime(opts.Profile)
	if err != nil {
		// Profile or its runtime not found in settings; use default
		return s.manager, s.runtime.Name()
	}

	// Cheap pre-check: a profile matching the broker's own default runtime
	// (type, and for Kubernetes also context/namespace) returns the shared
	// manager WITHOUT fully resolving the profile — in particular, without
	// the live Client.Verify() API round trip runtime.GetRuntime makes for
	// Kubernetes. Every agent start goes through this path, so paying that
	// network cost unconditionally, and risking a transient Verify failure
	// on a start that targets the broker's own healthy runtime, would be
	// worse than the settings-string comparison this performs instead.
	//
	// A runtime whose instances are bound to one profile's configuration
	// (the optional PerProfileInstancesRuntime capability) can't be shared
	// that way even on an otherwise-provable match — a second profile of the
	// same type (and, for Kubernetes, the same context/namespace) may still
	// carry its own runtime config — so when the default runtime reports
	// that capability, this shortcut is skipped unconditionally and the
	// profile is always fully resolved below (the ForceRuntime branch above
	// still takes the type-only shortcut). The broker does not keep the
	// default runtime's profile identity, so it cannot tell whether the
	// requested profile is the one the default runtime was built from; it
	// relies on the capability instead.
	if s.defaultRuntimeMatchesProfile(runtimeType, rtConfig) && !scionrt.HasPerProfileInstances(s.runtime) {
		return s.manager, s.runtime.Name()
	}

	// Resolve the profile's runtime so its true identity can be compared
	// against the default's — a settings-level type-string comparison
	// can't tell two distinct instances of one type apart (see the comment
	// above). Cache it as an auxiliary manager so LookupContainerID can find
	// agents created on non-default runtimes (e.g. K8s pods when default is
	// docker).
	//
	// Always resolved fresh here, never read back from that cache: a
	// runtime is constructed from the profile's own config (context,
	// namespace, GKE or Cloud Run settings — see runtime.GetRuntime), which
	// differs by project and profile even when the type is the same, so a
	// cached read keyed on anything coarser than the fully resolved
	// identity could hand one project's or profile's client to another.
	//
	// Use opts.Profile for ResolveRuntime so it picks up the same profile
	// that was just checked. When empty, GetRuntime falls back to settings
	// the same way ResolveRuntime does.
	//
	// resolveAuxiliaryRuntime is set to agent.ResolveRuntime by New() and
	// should never be nil in production; this falls back to the same
	// function so a Server built without New() (e.g. a test literal)
	// resolves identically to production instead of panicking.
	resolver := s.resolveAuxiliaryRuntime
	if resolver == nil {
		resolver = agent.ResolveRuntime
	}
	resolved := resolver(opts.ProjectPath, opts.Name, opts.Profile)

	if s.config.Debug {
		s.agentLifecycleLog.Debug("Resolved runtime for start options",
			"agent", opts.Name, "profile", opts.Profile,
			"activeProfile", vs.ActiveProfile,
			"defaultRuntime", s.runtime.Name(),
			"resolvedRuntime", resolved.Name(),
		)
	}

	if resolved.Name() == "error" {
		// Resolution failed outright (e.g. cluster unreachable): there is no
		// identity to compare or register. Wrap it in a manager so the
		// caller's calls surface the underlying error, the same contract
		// callers get for any other runtime construction failure.
		return agent.NewManager(resolved), resolved.Name()
	}

	// A PerProfileInstancesRuntime default never short-circuits here either,
	// for the same reason the pre-check above skips it: auxiliaryRuntimeIdentity
	// has no generic per-profile-config field to compare (only Kubernetes
	// carries context/namespace), so two distinct same-type instances of such
	// a runtime would otherwise collapse to one identity and incorrectly
	// share the default manager.
	if !scionrt.HasPerProfileInstances(s.runtime) && auxiliaryRuntimeIdentity(resolved) == auxiliaryRuntimeIdentity(s.runtime) {
		return s.manager, s.runtime.Name()
	}

	// Keyed by resolved IDENTITY, not type (see auxiliaryRuntimeIdentity):
	// two profiles resolving to distinct runtimes of the same type (e.g.
	// two Kubernetes clusters) must not overwrite each other's entry.
	identity := auxiliaryRuntimeIdentity(resolved)
	mgr := agent.NewManager(resolved)
	s.auxiliaryRuntimesMu.Lock()
	s.auxiliaryRuntimes[identity] = auxiliaryRuntime{Runtime: resolved, Manager: mgr}
	s.auxiliaryRuntimesMu.Unlock()

	return mgr, resolved.Name()
}

// recheckHubDefaultPassthrough re-runs the hub-default passthrough gate's
// downgrade check (downgradeUnverifiedHubDefaultPassthrough, start_context.go)
// against resolvedRuntimeType, reading the current mode and
// RequireLocalRuntime flag out of env itself. startAgent and restartAgent
// call this once, immediately after they re-resolve the manager following
// buildStartContext's own resolution — a later, more specific resolution
// (after a saved-profile lookup) that buildStartContext cannot see — so the
// check runs against the runtime that actually starts, not just the one
// buildStartContext saw.
func recheckHubDefaultPassthrough(env map[string]string, envCls map[string]api.EnvKind, resolvedRuntimeType string) {
	downgradeUnverifiedHubDefaultPassthrough(env, envCls,
		env["SCION_METADATA_MODE"], env["SCION_METADATA_REQUIRE_LOCAL_RUNTIME"] == "true", resolvedRuntimeType)
}

// findAuxiliaryRuntimeByType returns a registered auxiliary runtime whose
// resolved type matches runtimeType. Auxiliary runtimes are keyed by
// resolved IDENTITY (see auxiliaryRuntimeIdentity), not by type, since more
// than one distinct runtime instance (e.g. two Kubernetes clusters) can
// share a type. ForceRuntime, however, is a config surface that only names a
// type, so when several auxiliary runtimes share the requested type this
// picks the one with the lexicographically smallest identity key — a
// deterministic, if coarse, choice rather than one dependent on map
// iteration order. Extending ForceRuntime to name a specific instance,
// rather than just a type, is a follow-up decision, not made here.
func (s *Server) findAuxiliaryRuntimeByType(runtimeType string) (auxiliaryRuntime, bool) {
	s.auxiliaryRuntimesMu.RLock()
	defer s.auxiliaryRuntimesMu.RUnlock()

	var matchKeys []string
	for key, aux := range s.auxiliaryRuntimes {
		if aux.Runtime != nil && aux.Runtime.Name() == runtimeType {
			matchKeys = append(matchKeys, key)
		}
	}
	if len(matchKeys) == 0 {
		return auxiliaryRuntime{}, false
	}
	sort.Strings(matchKeys)
	return s.auxiliaryRuntimes[matchKeys[0]], true
}

// Helper functions

// resolveProjectSettingsDir returns the directory containing settings.yaml for a project.
// For linked projects, projectPath already points to the .scion directory.
// For hub-managed projects, projectPath is the workspace parent, so settings
// live in the .scion subdirectory.
func resolveProjectSettingsDir(projectPath string) string {
	if config.GetSettingsPath(projectPath) != "" {
		return projectPath
	}
	candidate := filepath.Join(projectPath, ".scion")
	if config.GetSettingsPath(candidate) != "" {
		return candidate
	}
	return projectPath // fallback to original
}

// forceHeartbeatAll sends an immediate heartbeat on all hub connections so the
// hub gets updated container status without waiting for the next periodic interval.
func (s *Server) forceHeartbeatAll(action, agentID string) {
	s.hubMu.RLock()
	defer s.hubMu.RUnlock()
	for _, conn := range s.hubConnections {
		if conn.Heartbeat != nil {
			hb := conn.Heartbeat
			go func() {
				if err := hb.ForceHeartbeat(context.Background()); err != nil {
					s.agentLifecycleLog.Error("Failed to send forced heartbeat after "+action, "agent_id", agentID, "error", err)
				}
			}()
		}
	}
}

func boolPtr(b bool) *bool {
	return &b
}

func agentInfoPtr(a AgentResponse) *AgentResponse {
	return &a
}

// ============================================================================
// Project Endpoints
// ============================================================================

// handleProjectBySlug routes requests to /api/v1/projects/{slug}.
func (s *Server) handleProjectBySlug(w http.ResponseWriter, r *http.Request) {
	slug := extractID(r, "/api/v1/projects")
	if slug == "" {
		NotFound(w, "project")
		return
	}

	switch r.Method {
	case http.MethodDelete:
		s.deleteProject(w, r, slug)
	default:
		MethodNotAllowed(w)
	}
}

// deleteProject removes the local hub-managed project directory for the given slug.
// Returns 204 on success (including when the directory doesn't exist).
func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request, slug string) {
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		RuntimeError(w, "Failed to get global dir: "+err.Error())
		return
	}

	projectPath := filepath.Join(globalDir, "projects", slug)

	// Path traversal protection: ensure the resolved path stays inside the
	// projects base directory.
	projectsBase := filepath.Join(globalDir, "projects")
	absProject, err := filepath.Abs(projectPath)
	if err != nil {
		RuntimeError(w, "Failed to resolve project path: "+err.Error())
		return
	}
	absProjectsBase, err := filepath.Abs(projectsBase)
	if err != nil {
		RuntimeError(w, "Failed to resolve base path: "+err.Error())
		return
	}
	if !strings.HasPrefix(absProject, absProjectsBase+string(filepath.Separator)) {
		s.agentLifecycleLog.Warn("project cleanup path traversal blocked", "slug", slug, "resolved", absProject)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if _, err := os.Stat(projectPath); os.IsNotExist(err) {
		// Already gone — idempotent success
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if err := os.RemoveAll(projectPath); err != nil {
		s.agentLifecycleLog.Warn("failed to remove project directory", "slug", slug, "path", projectPath, "error", err)
		RuntimeError(w, "Failed to remove project directory: "+err.Error())
		return
	}

	s.agentLifecycleLog.Info("Removed hub-managed project directory", "slug", slug, "path", projectPath)
	w.WriteHeader(http.StatusNoContent)
}

// errDeleteTargetNotFound means no agent with the requested slug exists in
// the requested project on any runtime of this broker, nor as files in that
// project's hub-managed directory.
var errDeleteTargetNotFound = errors.New("agent not found in project")

// errDeleteTargetUnknown means the agent could not be resolved because a
// runtime listing failed.
var errDeleteTargetUnknown = errors.New("could not list agents to resolve delete target")

// nfsAgentFilesRemover is implemented by agent managers that can remove an
// agent's worktree or own workspace from the NFS workspace export on
// delete.
type nfsAgentFilesRemover interface {
	RemoveNFSAgentFiles(ctx context.Context, projectPath, projectID, agentName string) (paths []string, err error)
}

var _ nfsAgentFilesRemover = (*agent.AgentManager)(nil)

// deleteTarget is the single, project-matched agent a delete acts on.
type deleteTarget struct {
	mgr         agent.Manager
	name        string // agent directory / slug name used for file cleanup
	containerID string // empty for a file-only (never started / container gone) agent
	projectPath string
	projectID   string
}

// agentNameMatches reports whether a runtime entry is the agent named id.
func agentNameMatches(a api.AgentInfo, id string) bool {
	return a.Name == id || a.ContainerID == id || a.Slug == id ||
		strings.TrimPrefix(a.Name, "/") == id
}

// agentInProjectStrict reports whether an entry is positively identified as
// belonging to projectID (label first, then the ProjectID field). Unlike
// matchesAgentProject, an entry with no project identity does not match.
func agentInProjectStrict(a api.AgentInfo, projectID string) bool {
	if labelProjectID := projectkeys.ProjectIDFromLabels(a.Labels); labelProjectID != "" {
		return labelProjectID == projectID
	}
	return a.ProjectID != "" && a.ProjectID == projectID
}

// agentHasNoProjectIdentity reports whether an entry carries no project ID in
// either labels or fields (a pre-label legacy container).
func agentHasNoProjectIdentity(a api.AgentInfo) bool {
	return projectkeys.ProjectIDFromLabels(a.Labels) == "" && a.ProjectID == ""
}

// resolveDeleteTarget finds the one agent entry a delete of id in projectID
// must act on, searching the default runtime and every auxiliary runtime.
//
// With a projectID:
//   - runtime entries positively labelled for projectID are preferred; the
//     List call carries the project scope label;
//   - if none exist, a legacy container carrying no project identity at all
//     is accepted only if its recorded project path identifies as projectID
//     (pre-label containers). File-only entries synthesised from the
//     broker's CWD project are never accepted this way;
//   - a runtime entry's project path is used for files only if it verifiably
//     belongs to projectID;
//   - if no runtime entry matches, agent files are looked for only in the
//     project directory (hub-managed, or linked via the hub's path hint or
//     the broker's working project) whose recorded project ID is projectID;
//   - otherwise errDeleteTargetNotFound.
//
// Without a projectID (solo/CLI), any same-named entry matches, and the
// hub-managed directory scan must find exactly one project.
//
// More than one distinct match is an error (fail closed) rather than a guess.
func (s *Server) resolveDeleteTarget(ctx context.Context, id, projectID, projectPathHint string, needProjectPath bool) (*deleteTarget, error) {
	type candidate struct {
		mgr   agent.Manager
		entry api.AgentInfo
	}
	managers := s.allManagers()

	var listErr error
	collect := func(filter map[string]string, accept func(api.AgentInfo) bool) []candidate {
		var out []candidate
		seen := map[string]bool{}
		for _, mgr := range managers {
			if mgr == nil {
				continue
			}
			agents, err := mgr.List(ctx, filter)
			if err != nil {
				s.agentLifecycleLog.Warn("Agent delete: runtime list failed", "agent_id", id, "error", err)
				listErr = err
				continue
			}
			for _, a := range agents {
				if !agentNameMatches(a, id) || !accept(a) {
					continue
				}
				key := a.ContainerID
				if key == "" {
					key = "path:" + a.ProjectPath + "|" + a.Name
				}
				if seen[key] {
					continue
				}
				seen[key] = true
				out = append(out, candidate{mgr: mgr, entry: a})
			}
		}
		return out
	}

	var matches []candidate
	if projectID != "" {
		matches = collect(map[string]string{
			"scion.agent":              "true",
			projectkeys.LabelProjectID: projectID,
		}, func(a api.AgentInfo) bool { return agentInProjectStrict(a, projectID) })
		if len(matches) == 0 {
			// Legacy (pre-label) containers carry no project ID. Accept one
			// only if its recorded project path positively identifies as
			// projectID; otherwise a same-named legacy container from another
			// project could be deleted.
			matches = collect(map[string]string{"scion.agent": "true"}, func(a api.AgentInfo) bool {
				return a.ContainerID != "" && agentHasNoProjectIdentity(a) &&
					pathIdentifiesAs(a.ProjectPath, projectID)
			})
		}
	} else {
		matches = collect(map[string]string{"scion.agent": "true"}, func(api.AgentInfo) bool { return true })
	}

	switch {
	case len(matches) > 1:
		return nil, fmt.Errorf("agent '%s' is ambiguous: %d agents match in project %q", id, len(matches), projectID)
	case len(matches) == 1:
		m := matches[0]
		t := &deleteTarget{
			mgr:         m.mgr,
			name:        id,
			containerID: m.entry.ContainerID,
			projectPath: m.entry.ProjectPath,
			projectID:   m.entry.ProjectID,
		}
		if t.projectID == "" {
			t.projectID = m.entry.Project
		}
		if t.projectPath != "" && !trustedEntryProjectPath(t.projectPath, projectID) {
			// The path comes from a runtime label/annotation, which may be
			// stale or crafted. Never delete files there unless the path is
			// verifiably this project's; fall back to the broker's own,
			// identity-checked resolution below.
			s.agentLifecycleLog.Warn("Agent delete: ignoring runtime project path that does not belong to the project",
				"agent_id", id, "project_id", projectID, "path", t.projectPath)
			t.projectPath = ""
		}
		if t.projectPath == "" && needProjectPath {
			// The runtime entry carries no project path (e.g. no
			// annotation). Resolve it only from this project's own
			// directory (hub-managed or linked), never by a project-blind
			// scan.
			resolved, err := s.findAgentProjectDir(id, projectID, projectPathHint)
			if err != nil {
				return nil, err
			}
			t.projectPath = resolved
		}
		return t, nil
	}

	// No runtime entry was found. If a runtime could not be listed, that is
	// not known to be true: its container may still be running. Fail rather
	// than delete only the files (orphaning the container) or report a 404
	// (which the hub treats as a completed delete).
	if listErr != nil {
		return nil, fmt.Errorf("%w: %v", errDeleteTargetUnknown, listErr)
	}

	// The agent may exist only as files (never started, or its container is
	// gone). Look only in this project's directory.
	resolved, err := s.findAgentProjectDir(id, projectID, projectPathHint)
	if err != nil {
		return nil, err
	}
	if resolved == "" {
		return nil, errDeleteTargetNotFound
	}
	s.agentLifecycleLog.Debug("Resolved agent project path for file-only delete",
		"agent_id", id, "project_id", projectID, "path", resolved)
	return &deleteTarget{
		mgr:         s.manager,
		name:        id,
		projectPath: resolved,
		projectID:   projectID,
	}, nil
}

// agentResourceCleaner is implemented by managers whose runtime can remove
// per-agent objects left behind after the agent's container is gone (see
// runtime.AgentResourceCleaner and agent.AgentManager.CleanupAgentResources).
type agentResourceCleaner interface {
	CleanupAgentResources(ctx context.Context, agentName, projectID string) error
}

// cleanupLeftoverAgentResources removes the per-agent runtime objects of an
// agent whose container no longer exists, across the default and every
// auxiliary runtime, since the container may have run on any of them. It is
// scoped to projectID and does nothing without one. It is best effort: a
// failure is logged and does not fail the delete, matching the cleanup that
// runtime Delete does when the container still exists.
func (s *Server) cleanupLeftoverAgentResources(ctx context.Context, agentName, projectID string) {
	if projectID == "" {
		return
	}
	slug := api.Slugify(agentName)
	for _, mgr := range s.allManagers() {
		c, ok := mgr.(agentResourceCleaner)
		if !ok {
			continue
		}
		if err := c.CleanupAgentResources(ctx, slug, projectID); err != nil {
			s.agentLifecycleLog.Warn("Agent delete: failed to remove leftover runtime objects",
				"agent_id", agentName, "project_id", projectID, "error", err)
		}
	}
}

// findAgentProjectDir returns the .scion dir of the project that owns the
// agent's files, or "" if none. It checks the hub-managed project directories
// first. When projectID is set it then checks linked (non hub-managed)
// projects: the path the hub registered for this project on this broker
// (projectPathHint, i.e. the provider's LocalPath) and the broker's own
// working project. A linked path is used only if its recorded project identity
// equals projectID, so a stale or wrong hint can never redirect deletion to
// another project's files.
func (s *Server) findAgentProjectDir(agentName, projectID, projectPathHint string) (string, error) {
	resolved, err := findAgentInHubManagedProjects(agentName, projectID)
	if err != nil || resolved != "" || projectID == "" {
		return resolved, err
	}
	candidates := []string{projectPathHint}
	if cwdProject, err := config.GetResolvedProjectDir(""); err == nil {
		candidates = append(candidates, cwdProject)
	}
	for _, c := range candidates {
		if dir := linkedProjectAgentDir(c, agentName, projectID); dir != "" {
			return dir, nil
		}
	}
	return "", nil
}

// linkedProjectAgentDir returns the resolved .scion dir of the linked project
// at path if that project's identity is projectID and it holds files for
// agentName, otherwise "". path may be the project root or its .scion entry.
// Identity comes from the project-id file (git projects, .scion directory) or
// the marker file (non-git projects, .scion file).
func linkedProjectAgentDir(path, agentName, projectID string) string {
	if path == "" || projectID == "" {
		return ""
	}
	if !pathIdentifiesAs(path, projectID) {
		return ""
	}
	scionDir, err := config.GetResolvedProjectDir(scionEntry(path))
	if err != nil || scionDir == "" {
		return ""
	}
	if !hubManagedProjectHasAgent(scionDir, agentName) {
		return ""
	}
	return scionDir
}

// scionEntry returns the .scion entry for path, which may be the project root
// or the .scion entry itself.
func scionEntry(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	if filepath.Base(abs) == config.DotScion {
		return abs
	}
	return filepath.Join(abs, config.DotScion)
}

// projectIDAtPath returns the project identity recorded at path, or "".
// For a .scion directory it is the project-id (or legacy grove-id) file. For a
// .scion marker file (non-git linked project) it is the marker's project ID.
// If path is a project's external config dir, which has no project-id file,
// the result is "".
func projectIDAtPath(path string) string {
	if path == "" {
		return ""
	}
	marker := scionEntry(path)
	if marker == "" {
		return ""
	}
	info, err := os.Stat(marker)
	if err != nil {
		return ""
	}
	if info.IsDir() {
		id, _ := config.ReadProjectID(marker)
		return id
	}
	if m, err := config.ReadProjectMarker(marker); err == nil {
		return m.ProjectID
	}
	return ""
}

// pathIdentifiesAs reports whether the project at path verifiably belongs to
// projectID: either its recorded identity (projectIDAtPath) equals projectID,
// or path is the project's external config dir
// ~/.scion/project-configs/<slug>__<short-id>/.scion, whose name encodes the
// project ID (non-git linked projects record that dir as the agent's project
// path).
func pathIdentifiesAs(path, projectID string) bool {
	if path == "" || projectID == "" {
		return false
	}
	if projectIDAtPath(path) == projectID {
		return true
	}
	short, ok := externalConfigShortID(path)
	return ok && short == (config.ProjectMarker{ProjectID: projectID}).ShortUUID()
}

// externalConfigShortID returns the short project ID encoded in path if path
// is an external project config dir ~/.scion/project-configs/<slug>__<short-id>/.scion.
// path is resolved through symlinks before it is split, so a path reached
// through a pre-rename entry (config.MigrateLegacyGlobalLayout leaves a
// symlink at each individual entry it moves, e.g.
// ~/.scion/grove-configs/<name> -> ../project-configs/<name>, while the
// legacy root itself stays a real directory) still identifies as this
// project's external config dir.
func externalConfigShortID(path string) (string, bool) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}
	abs = projectkeys.ResolvePathForCompare(abs)
	if filepath.Base(abs) != config.DotScion {
		return "", false
	}
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return "", false
	}
	projectDir := filepath.Dir(abs)
	parent := filepath.Dir(projectDir)
	if !projectkeys.ResolvedPathEqual(parent, filepath.Join(globalDir, config.ProjectConfigsDir)) {
		return "", false
	}
	name := filepath.Base(projectDir)
	i := strings.LastIndex(name, "__")
	if i <= 0 || i+2 >= len(name) {
		return "", false
	}
	return name[i+2:], true
}

// trustedEntryProjectPath reports whether a project path taken from a runtime
// entry may be used for file operations. With a projectID, the path must
// identify as that project (pathIdentifiesAs). Without one, the path must
// carry some project identity, be an external project config dir, or be the
// global project directory.
func trustedEntryProjectPath(path, projectID string) bool {
	if projectID != "" {
		return pathIdentifiesAs(path, projectID)
	}
	if projectIDAtPath(path) != "" {
		return true
	}
	if _, ok := externalConfigShortID(path); ok {
		return true
	}
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return false
	}
	abs, err := filepath.Abs(path)
	return err == nil && filepath.Clean(abs) == filepath.Clean(globalDir)
}

// findAgentInHubManagedProjects scans hub-managed project directories
// (~/.scion/projects/<slug>/.scion/) for an agent directory matching the
// given name and returns that project's .scion dir path, or "" if none. A
// project moved from its pre-rename location by config.MigrateLegacyGlobalLayout
// is found here directly, since the migration runs at broker boot, before this
// function is ever reached.
//
// When projectID is set, only a project directory whose recorded project ID
// (the project-id file) equals projectID is considered, so a same-named
// agent in another project is never returned (ptone/scion#1819). When
// projectID is empty, the name must be found in exactly one project; more
// than one is reported as an ambiguity error rather than a guess.
//
// Probes both the in-project location (worktree-mode agents) and the external
// per-agent state dir under ~/.scion/project-configs/ (shared-workspace agents,
// whose state lives external to the shared checkout).
func findAgentInHubManagedProjects(agentName, projectID string) (string, error) {
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		// Fail closed: without the global dir the agent's absence is not
		// known, and a 404 would let the hub treat the delete as done.
		return "", fmt.Errorf("%w: resolve global dir: %v", errDeleteTargetUnknown, err)
	}
	var found []string
	baseDir := filepath.Join(globalDir, "projects")
	entries, err := os.ReadDir(baseDir)
	if err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			scionDir := filepath.Join(baseDir, entry.Name(), ".scion")
			if projectID != "" {
				recorded, err := config.ReadProjectID(scionDir)
				if err != nil || recorded != projectID {
					continue
				}
			}
			if hubManagedProjectHasAgent(scionDir, agentName) {
				found = append(found, scionDir)
			}
		}
	}
	switch len(found) {
	case 0:
		return "", nil
	case 1:
		return found[0], nil
	default:
		return "", fmt.Errorf("agent '%s' found in %d hub-managed projects; specify the project", agentName, len(found))
	}
}

func hubManagedProjectHasAgent(scionDir, agentName string) bool {
	if _, err := os.Stat(filepath.Join(scionDir, "agents", agentName)); err == nil {
		return true
	}
	// Shared-workspace agents have no in-project agentDir — probe the
	// external split-storage path.
	if extDir, err := config.GetGitProjectExternalAgentsDir(scionDir); err == nil && extDir != "" {
		if _, err := os.Stat(filepath.Join(extDir, agentName)); err == nil {
			return true
		}
	}
	return false
}

// isLocalhostEndpoint returns true if the given endpoint URL refers to a
// loopback address (localhost, 127.0.0.1, [::1], etc.). This is used to
// decide whether the ContainerHubEndpoint bridge address should be
// substituted — containers can reach external hosts directly but need a
// bridge address to reach services on the host's loopback interface.
func isLocalhostEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// checkNFSForDispatch is the NFS pre-flight for an agent create. It
// applies only when NFS is configured with nfs.auto_mount on (otherwise
// mounts are managed externally and the broker never gates on them) and the
// project's effective settings select the nfs workspace backend (projects
// on any other backend are never affected by NFS state).
//
// It is decided by the dispatch's resolved runtime, which is checked before
// anything is mounted:
//   - Kubernetes or Cloud Run (NFSWarnOnlyRuntime): the platform
//     mounts the export into the agent. The broker reads the share's last
//     recorded status, logs a warning if it is unhealthy, and continues; it
//     never mounts and never refuses the dispatch.
//   - Any other (local-container) runtime: the share is checked and, when
//     the broker mounts shares, mounted. If it is not mounted the dispatch
//     is refused, since the workspace would otherwise silently land on
//     local disk under the mount point.
func (s *Server) checkNFSForDispatch(ctx context.Context, name, projectPath, projectSlug, profile string) error {
	r := s.nfsMountReconciler
	if r == nil || !r.AutoMount() {
		return nil
	}
	nfsCfg := s.config.NFSConfig
	if nfsCfg == nil || len(nfsCfg.Shares) == 0 {
		return nil
	}
	projectDir := resolveDispatchProjectDir(projectPath, projectSlug)
	if !dispatchUsesNFSWorkspace(projectDir) {
		return nil
	}
	// The nfs workspace backend places workspaces on the first share only
	// (runtime.NFSWorkspaceBackend), so that is the share a dispatch needs.
	shareID := nfsCfg.Shares[0].ID

	_, runtimeType := s.resolveManagerForOpts(api.StartOptions{
		Name:        name,
		ProjectPath: projectDir,
		Profile:     profile,
	})
	if NFSWarnOnlyRuntime(runtimeType) {
		if st, ok := r.ShareStatus(shareID); !ok || !st.Healthy {
			detail := "not checked yet"
			if ok {
				detail = st.Message
			}
			s.agentLifecycleLog.Warn("NFS share not mounted on the broker; continuing dispatch, the runtime mounts the export into the agent",
				"agent", name, "runtime", runtimeType, "share", shareID, "detail", detail)
		}
		return nil
	}
	// ctx (the request context) bounds the wait and any mount command.
	return r.EnsureShareMounted(ctx, shareID)
}

// resolveDispatchProjectDir mirrors buildStartContext's hub-managed project
// path resolution (a slug with no path resolves under the global projects
// directory) followed by config.GetResolvedProjectDir, the directory the
// agent manager loads effective settings from.
func resolveDispatchProjectDir(projectPath, projectSlug string) string {
	if projectPath == "" && projectSlug != "" {
		if globalDir, err := config.GetGlobalDir(); err == nil {
			projectPath = filepath.Join(globalDir, "projects", projectSlug)
		}
	}
	projectDir, _ := config.GetResolvedProjectDir(projectPath)
	return projectDir
}

// dispatchUsesNFSWorkspace reports whether the effective settings for
// projectDir select the nfs workspace backend, the same condition the agent
// manager uses (pkg/agent run.go). If the settings cannot be loaded it
// returns true: the caller only asks when the broker itself is configured
// for NFS, so that is the likely backend.
func dispatchUsesNFSWorkspace(projectDir string) bool {
	vs, _, err := config.LoadEffectiveSettings(projectDir)
	if err != nil || vs == nil {
		return true
	}
	return vs.Server != nil && vs.Server.WorkspaceStorage != nil &&
		vs.Server.WorkspaceStorage.Backend == "nfs"
}

// preResolvedHubEndpoint picks the Hub base URL used to absolutize the
// Hub-relative download URLs the Hub emits for local-storage skills in
// PreResolvedSkills: the broker's own connection endpoint when known (the
// address this broker actually reaches the Hub on), else the endpoint the
// Hub advertised in the request.
func preResolvedHubEndpoint(conn *HubConnection, advertised string) string {
	if conn != nil && conn.HubEndpoint != "" {
		return conn.HubEndpoint
	}
	return advertised
}
