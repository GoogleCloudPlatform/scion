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

// Package runtimebroker provides the Scion Runtime Broker API server.
// The Runtime Broker API exposes agent lifecycle management over HTTP,
// allowing the Scion Hub to remotely manage agents on this compute node.
package runtimebroker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/templatecache"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth/adcsource"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// ServerConfig holds configuration for the Runtime Broker API server.
type ServerConfig struct {
	// Port is the HTTP port to listen on.
	Port int
	// Host is the address to bind to (e.g., "0.0.0.0" or "127.0.0.1").
	Host string
	// ReadTimeout is the maximum duration for reading the entire request.
	ReadTimeout time.Duration
	// WriteTimeout is the maximum duration before timing out writes.
	WriteTimeout time.Duration

	// HubEndpoint is the Hub API endpoint for reporting (optional).
	HubEndpoint string
	// ContainerHubEndpoint overrides HubEndpoint when injecting the Hub URL
	// into agent containers. Used for local development where containers
	// need a bridge address (e.g. host.containers.internal) instead of localhost.
	ContainerHubEndpoint string
	// HubListenPort is the port the co-located hub HTTP server is listening
	// on (e.g. 8080 for the combined web+API server). Used by cloudrun-sandbox
	// to construct the link-local hub endpoint for sandboxes. Zero means the
	// hub listen port is not known (remote broker, non-colocated mode).
	HubListenPort int

	// BrokerID is a unique identifier for this runtime broker.
	BrokerID string
	// BrokerName is a human-readable name for this runtime broker.
	BrokerName string

	// CORS settings
	CORSEnabled        bool
	CORSAllowedOrigins []string
	CORSAllowedMethods []string
	CORSAllowedHeaders []string
	CORSMaxAge         int

	// Debug enables verbose debug logging.
	Debug bool
	// SlowRequestThreshold is the duration after which an HTTP request is
	// logged as slow. Zero uses logging.DefaultSlowRequestThreshold.
	SlowRequestThreshold time.Duration

	// Hub integration settings
	// HubEnabled indicates whether this Runtime Broker should connect to a Hub
	// for template hydration and other centralized services.
	HubEnabled bool
	// HubToken is the authentication token for the Hub API.
	HubToken string

	// Template cache settings
	// TemplateCacheDir is the directory for caching templates fetched from the Hub.
	// Defaults to ~/.scion/cache/templates if not specified.
	TemplateCacheDir string
	// TemplateCacheMaxSize is the maximum size of the template cache in bytes.
	// Defaults to 100MB if not specified.
	TemplateCacheMaxSize int64

	// Broker credentials settings
	// BrokerCredentialsPath is the path to the broker credentials file.
	// If set, HMAC authentication will be used instead of bearer tokens.
	// Defaults to ~/.scion/broker-credentials.json if not specified.
	BrokerCredentialsPath string

	// InMemoryCredentials allows injecting credentials directly without a file.
	// Used for co-located Hub+RuntimeBroker mode where credentials are generated
	// in-memory and shared between the Hub and RuntimeBroker in the same process.
	// Takes precedence over BrokerCredentialsPath if set.
	InMemoryCredentials *brokercredentials.BrokerCredentials

	// BrokerAuthEnabled enables HMAC verification for incoming requests from the Hub.
	BrokerAuthEnabled bool
	// BrokerAuthStrictMode, when true, requires all requests to be authenticated.
	// When false (default), unauthenticated requests are allowed for transition periods.
	BrokerAuthStrictMode bool

	// Heartbeat settings
	// HeartbeatEnabled enables periodic heartbeats to the Hub.
	HeartbeatEnabled bool
	// HeartbeatInterval is the time between heartbeats.
	// Defaults to 30 seconds if not specified.
	HeartbeatInterval time.Duration

	// Control channel settings
	// ControlChannelEnabled enables the WebSocket control channel to the Hub.
	// This allows NAT traversal for brokers behind firewalls.
	ControlChannelEnabled bool

	// Workspace sync settings
	// StorageBucket is the GCS bucket name for workspace storage.
	// Used when workspace sync requests don't specify a bucket.
	StorageBucket string
	// WorktreeBase is the base directory for agent worktrees.
	// Used as a fallback when resolving workspace paths.
	WorktreeBase string

	// ForceRuntime overrides profile resolution and forces the specified runtime.
	// Used in tests to ensure mock runtime is always used.
	ForceRuntime string

	// StateDir is the directory for broker runtime state (pending env-gather,
	// dispatch attempts). Defaults to ~/.scion/runtime-broker-state/<broker-id>.
	StateDir string

	// AllowContainerScriptHarnesses controls whether the broker will dispatch
	// agents whose resolved harness-config declares a provisioner block.
	// Provisioners execute scripts inside the agent container with access to
	// projected secrets. Defaults to true; set false to block such dispatches.
	AllowContainerScriptHarnesses bool

	// NFSConfig holds NFS workspace storage settings for this broker. The
	// server command sets it from server.workspace_storage when the backend
	// is "nfs" (see brokerNFSConfig in cmd). When non-nil with shares
	// configured, the broker constructs an NFSMountReconciler that mounts
	// each share at <MountRoot>/<share.ID> in the background at startup,
	// reports per-share state in /healthz, and re-checks the shares before
	// NFS-backed agent dispatches. Nil leaves all NFS handling off.
	NFSConfig *config.V1NFSConfig

	// NFSMountChecker overrides the mount layer the NFS reconciler uses.
	// Nil selects ExecMountChecker (mount(8)/umount(8)); tests set a fake.
	NFSMountChecker MountChecker

	// ColocatedStorage is the storage backend of a Hub running co-located in the
	// same process. When set and backed by the local filesystem, the broker
	// resolves resources for the co-located connection by reading directly from
	// disk instead of hydrating over HTTP. Leave nil for remote-only brokers or
	// when the co-located Hub uses a non-local backend.
	ColocatedStorage storage.Storage
}

// DefaultServerConfig returns the default server configuration.
func DefaultServerConfig() ServerConfig {
	return ServerConfig{
		Port:                 9800,
		Host:                 "0.0.0.0",
		ReadTimeout:          30 * time.Second,
		WriteTimeout:         120 * time.Second,
		CORSEnabled:          true,
		CORSAllowedOrigins:   []string{"*"},
		CORSAllowedMethods:   []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		CORSAllowedHeaders:   []string{"Authorization", "Content-Type", "X-Scion-Broker-Token", "X-API-Key", "X-Scion-Broker-ID", "X-Scion-Timestamp", "X-Scion-Nonce", "X-Scion-Signature", "X-Scion-Signed-Headers"},
		CORSMaxAge:           3600,
		BrokerAuthEnabled:    true,
		BrokerAuthStrictMode: true,
	}
}

// Server is the Runtime Broker API HTTP server.
type Server struct {
	config     ServerConfig
	manager    agent.Manager
	runtime    scionrt.Runtime
	httpServer *http.Server
	mux        *http.ServeMux
	mu         sync.RWMutex
	startTime  time.Time
	version    string

	// Hub connections (replaces single hubClient, heartbeat, controlChannel, etc.)
	hubConnections map[string]*HubConnection // keyed by connection name
	hubMu          sync.RWMutex

	// Shared template cache (content-addressed, hub-neutral)
	cache *templatecache.Cache

	// Shared harness-config cache (content-addressed). Partitioned from the
	// template cache by directory so the two kinds never share eviction
	// accounting or collide on identical-content hashes.
	hcCache *templatecache.Cache

	// Shared skill cache (content-addressed). Independent from templates and
	// harness-configs so skill eviction doesn't affect other resource kinds.
	skCache *templatecache.Cache

	// Shared GitHub resolution cache (singleton per broker instance).
	// Caches GitHub API resolution results to avoid redundant API calls.
	ghResolutionCache *agent.GitHubResolutionCache

	// Multi-key auth middleware
	brokerAuthMiddleware *MultiKeyBrokerAuthMiddleware

	// Credential watching (watches MultiStore directory)
	multiCredStore  *brokercredentials.MultiStore
	credLastScan    time.Time
	credWatcherStop chan struct{}

	// dispatchAttempts tracks request-id based create-attempt state for
	// idempotency and auditability.
	dispatchAttempts   map[string]*dispatchAttempt
	dispatchAttemptsMu sync.Mutex

	// launchRegistry is this replica's local bookkeeping for in-flight async
	// launches (design t1-async-create-v11.md §3.8.1, §7 P1b-1). It is an
	// optimisation only -- correctness comes from the Hub's answers.
	launchRegistry *launchRegistry
	// startsInFlight tracks the starts running on the start, restart and
	// synchronous create handlers (start_tracker.go). The heartbeat reports
	// them, stop waits for its agent's, and Shutdown waits for all.
	startsInFlight *startTracker
	// launchInstanceID identifies this broker process as a launch owner
	// (design §3.2's LaunchInstanceID / launch_owner), generated once here at
	// startup.
	launchInstanceID string

	// syncStartSupersedeWait overrides defaultSyncStartSupersedeWait when
	// positive (see beginSyncStart). Zero in production.
	syncStartSupersedeWait time.Duration

	stateDir string

	// auxiliaryRuntimes holds runtime+manager pairs for non-default runtimes
	// created via profile resolution (e.g. kubernetes when default is docker).
	// Used by LookupContainerID/LookupAgent as a fallback when the default
	// manager can't find an agent.
	//
	// Keyed by auxiliaryRuntimeIdentity (a per-instance key, not the runtime
	// type); read through sortedAuxiliaryRuntimes or
	// findAuxiliaryRuntimeByType rather than indexing by type.
	auxiliaryRuntimes   map[string]auxiliaryRuntime
	auxiliaryRuntimesMu sync.RWMutex

	// resolveAuxiliaryRuntime resolves the runtime.Runtime for a project path
	// and profile name when discovering or creating auxiliary runtimes, and
	// in resolveManagerForOpts when settings resolve to something other than
	// the broker's default runtime. It is a field (defaulting to
	// agent.ResolveRuntime, set in New) rather than a direct call, so tests
	// can substitute a stub that does not require live cluster/network
	// access — resolving a real Kubernetes runtime calls Verify() against
	// the API server.
	resolveAuxiliaryRuntime func(projectPath, agentName, profileFlag string) scionrt.Runtime

	// projectProvisionMu serializes worktree provisioning per project on this
	// node. Without this, concurrent agent creations for the same project could
	// race inside ProvisionShared (double-clone / corrupt .git state).
	// Key: ProjectID (or ProjectPath if ID is empty).
	projectProvisionMu sync.Map

	// NFS mount reconciler (nil when backend != "nfs")
	nfsMountReconciler *NFSMountReconciler
	// NFS reconcile loop state (all unused when nfsMountReconciler is nil).
	// nfsStartupReconcileDone is closed once the loop's first pass has
	// finished; nfsReconcileStopped is closed when the loop exits.
	nfsStartupReconcileDone chan struct{}
	nfsReconcileStopped     chan struct{}
	nfsReconcileOnce        sync.Once
	nfsReconcileCancel      context.CancelFunc
	// nfsReconcileInterval overrides DefaultNFSReconcileInterval (tests).
	nfsReconcileInterval time.Duration

	// Dedicated request logger (nil = disabled)
	requestLogger *slog.Logger

	// Dedicated message logger for message audit trail (nil = uses messageLog fallback)
	dedicatedMessageLog *slog.Logger

	// Subsystem loggers for handler methods
	agentLifecycleLog *slog.Logger
	messageLog        *slog.Logger
	envSecretLog      *slog.Logger
}

// auxiliaryRuntime pairs a runtime with its manager for non-default runtimes.
type auxiliaryRuntime struct {
	Runtime scionrt.Runtime
	Manager agent.Manager
}

type dispatchAttempt struct {
	RequestID  string
	Operation  string
	AgentID    string
	Status     string
	HTTPStatus int
	Error      string
	CreatedAt  time.Time
	UpdatedAt  time.Time

	CreatedResponse *CreateAgentResponse
	EnvResponse     *EnvRequirementsResponse
}

// New creates a new Runtime Broker API server.
func New(cfg ServerConfig, mgr agent.Manager, rt scionrt.Runtime) *Server {
	// Enable util debug logging when broker debug mode is on,
	// so that debug messages from pkg/agent (which use util.Debugf)
	// are visible in the broker's logs.
	if cfg.Debug {
		util.EnableDebug()
	}

	srv := &Server{
		config:            cfg,
		manager:           mgr,
		runtime:           rt,
		mux:               http.NewServeMux(),
		startTime:         time.Now(),
		version:           "0.1.0", // TODO: Get from build info
		hubConnections:    make(map[string]*HubConnection),
		dispatchAttempts:  make(map[string]*dispatchAttempt),
		auxiliaryRuntimes: make(map[string]auxiliaryRuntime),

		// Defaults to the real profile-resolution path; tests substitute a
		// stub that does not require live cluster/network access (see
		// discoverAuxiliaryRuntimesForProjects and resolveManagerForOpts).
		resolveAuxiliaryRuntime: agent.ResolveRuntime,
		launchRegistry:          newLaunchRegistry(),
		startsInFlight:          newStartTracker(),
		launchInstanceID:        uuid.NewString(),

		// Subsystem loggers
		agentLifecycleLog: logging.Subsystem("broker.agent-lifecycle"),
		messageLog:        logging.Subsystem("broker.messages"),
		envSecretLog:      logging.Subsystem("broker.env-secrets"),
	}

	srv.stateDir = cfg.StateDir
	if srv.stateDir == "" {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			slog.Warn("Failed to resolve user home directory for state dir", "error", err)
		} else {
			brokerDir := cfg.BrokerID
			if brokerDir == "" {
				brokerDir = "default"
			}
			srv.stateDir = filepath.Join(homeDir, ".scion", "runtime-broker-state", brokerDir)
		}
	}
	if srv.stateDir != "" {
		if err := srv.initStateStore(); err != nil {
			slog.Warn("Failed to initialize runtime broker state store", "error", err, "stateDir", srv.stateDir)
		}
	}

	// Initialize NFS mount reconciler when NFS storage is configured.
	// This only constructs the reconciler; its loop is started in Start().
	if cfg.NFSConfig != nil && len(cfg.NFSConfig.Shares) > 0 {
		nfsLog := logging.Subsystem("broker.nfs-mount")
		checker := cfg.NFSMountChecker
		if checker == nil {
			checker = NewExecMountChecker(nfsLog)
		}
		srv.nfsMountReconciler = NewNFSMountReconciler(cfg.NFSConfig, checker, nfsLog)
		// On Kubernetes and Cloud Run the platform mounts the export into
		// the agent, so the broker never mounts it; it only verifies.
		if rt != nil && NFSWarnOnlyRuntime(rt.Name()) {
			srv.nfsMountReconciler.SetVerifyOnly(fmt.Sprintf(
				"the broker's default runtime is %s, so the broker does not mount it", rt.Name()))
		}
		srv.nfsStartupReconcileDone = make(chan struct{})
		srv.nfsReconcileStopped = make(chan struct{})
		slog.Info("NFS mount reconciler initialized",
			"shares", len(cfg.NFSConfig.Shares),
			"mountRoot", cfg.NFSConfig.MountRoot,
			"autoMount", cfg.NFSConfig.AutoMount,
			"brokerMounts", srv.nfsMountReconciler.MountsShares())
		if srv.nfsMountReconciler.MountsShares() {
			if err := srv.nfsMountReconciler.mountPrivilegeError(); err != nil {
				slog.Warn("server.workspace_storage.nfs.auto_mount is on but the broker cannot mount; shares are checked only",
					"reason", err)
			}
		}
	}

	// Initialize Hub integration if enabled
	if cfg.HubEnabled && (cfg.HubEndpoint != "" || cfg.InMemoryCredentials != nil) {
		if err := srv.initHubIntegration(); err != nil {
			slog.Warn("Failed to initialize Hub integration", "error", err)
		}
	}

	srv.registerRoutes()

	return srv
}

// RuntimeName returns the name of the currently active container runtime.
func (s *Server) RuntimeName() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.runtime.Name()
}

// SwapRuntime replaces the broker's container runtime and agent manager.
// This is called when the co-located hub detects a runtime configuration
// change (e.g. during onboarding) so the broker picks up the new engine
// without requiring a full server restart. Running heartbeat services are
// updated to use the new manager so they don't continue shelling out to
// the old (possibly missing) runtime binary.
func (s *Server) SwapRuntime(rt scionrt.Runtime) {
	s.mu.Lock()
	old := s.runtime.Name()
	s.runtime = rt
	newMgr := agent.NewManager(rt)
	s.manager = newMgr
	s.mu.Unlock()

	// Propagate the new manager to all running heartbeat services so they
	// use the detected runtime binary instead of the stale one.
	s.hubMu.RLock()
	for _, conn := range s.hubConnections {
		conn.mu.RLock()
		hb := conn.Heartbeat
		conn.mu.RUnlock()
		if hb != nil {
			hb.SwapManager(newMgr)
			hb.SetDefaultRuntime(rt)
		}
	}
	s.hubMu.RUnlock()

	slog.Info("Runtime broker swapped container runtime",
		"old", old,
		"new", rt.Name(),
	)
}

// initHubIntegration initializes the shared template cache and hub connections.
func (s *Server) initHubIntegration() error {
	// 1. Initialize shared template cache
	cacheDir := s.config.TemplateCacheDir
	if cacheDir == "" {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("failed to get home directory: %w", err)
		}
		cacheDir = filepath.Join(homeDir, ".scion", "cache", "templates")
	}

	maxSize := s.config.TemplateCacheMaxSize
	if maxSize <= 0 {
		maxSize = templatecache.DefaultMaxSize
	}

	cache, err := templatecache.New(cacheDir, maxSize)
	if err != nil {
		return fmt.Errorf("failed to initialize template cache: %w", err)
	}
	s.cache = cache

	// 1b. Initialize the harness-config cache alongside the template cache,
	// under a sibling directory so the two content-addressed stores stay
	// independent.
	hcCacheDir := filepath.Join(filepath.Dir(cacheDir), "harness-configs")
	hcCache, err := templatecache.New(hcCacheDir, maxSize)
	if err != nil {
		return fmt.Errorf("failed to initialize harness-config cache: %w", err)
	}
	s.hcCache = hcCache

	// 1c. Initialize the skill cache for broker-side caching of resolved
	// skill content, keyed by content hash.
	skCacheDir := filepath.Join(filepath.Dir(cacheDir), "skills")
	skCacheMaxSize := int64(500 * 1024 * 1024) // 500MB default
	skCache, err := templatecache.New(skCacheDir, skCacheMaxSize)
	if err != nil {
		return fmt.Errorf("failed to initialize skill cache: %w", err)
	}
	s.skCache = skCache

	// 1d. Initialize the GitHub resolution cache for broker-side caching of
	// GitHub skill resolution metadata (not file content).
	ghResDir, err := agent.GitHubResolutionCacheDir()
	if err != nil {
		slog.Warn("github resolution cache: cannot determine cache dir", "error", err)
	} else {
		ghCache, err := agent.NewGitHubResolutionCache(ghResDir, agent.DefaultResolutionCacheTTL)
		if err != nil {
			slog.Warn("github resolution cache: init failed (running uncached)", "error", err)
			// nil cache is safe — resolver falls through to API call
		} else {
			s.ghResolutionCache = ghCache
			slog.Info("GitHub resolution cache initialized", "dir", ghResDir, "ttl", agent.DefaultResolutionCacheTTL)
		}
	}

	// 2. Initialize hub connections map (already done in New)

	// 3. Handle InMemoryCredentials -> "local" connection (co-located mode)
	if s.config.InMemoryCredentials != nil {
		creds := s.config.InMemoryCredentials
		if creds.Name == "" {
			creds.Name = "local"
		}
		conn, err := s.createHubConnection(creds.Name, creds)
		if err != nil {
			slog.Warn("Failed to create local hub connection", "error", err)
		} else {
			// Mark as co-located so heartbeat is handled by the internal DB loop
			// instead of the HTTP heartbeat service.
			conn.IsColocated = true

			// When the co-located Hub's storage backend is the local filesystem,
			// resolve resources by reading directly from disk — the backend IS
			// the source of truth, so no signed-URL/HTTP download or cache is
			// needed. A non-local co-located backend (e.g. GCS) hydrates through
			// the cache like any other broker.
			if stor := s.config.ColocatedStorage; stor != nil && stor.Provider() == storage.ProviderLocal {
				conn.LocalStorage = stor
			}

			s.hubMu.Lock()
			s.hubConnections[creds.Name] = conn
			s.hubMu.Unlock()
			slog.Info("Created local hub connection (co-located mode)", "name", creds.Name, "brokerID", creds.BrokerID)
		}
	}

	// 4. Load MultiStore credentials
	s.multiCredStore = brokercredentials.NewMultiStore("")
	multiCreds, err := s.multiCredStore.List()
	if err != nil {
		slog.Warn("Failed to list multi-store credentials", "error", err)
	}

	for i := range multiCreds {
		c := &multiCreds[i]
		// Skip if already handled by InMemoryCredentials
		if _, exists := s.hubConnections[c.Name]; exists {
			continue
		}
		conn, err := s.createHubConnection(c.Name, c)
		if err != nil {
			slog.Warn("Failed to create hub connection", "name", c.Name, "error", err)
			continue
		}
		s.hubMu.Lock()
		s.hubConnections[c.Name] = conn
		s.hubMu.Unlock()
		slog.Info("Created hub connection from multi-store", "name", c.Name, "brokerID", c.BrokerID)
	}

	// 5. Legacy fallback: if no connections yet (except possibly "local"),
	// try loading from the legacy single-file Store
	if len(s.hubConnections) == 0 || (len(s.hubConnections) == 1 && s.config.InMemoryCredentials != nil) {
		s.tryLegacyCredentials()
	}

	// If we still have no connections, try creating one from config (bearer/dev-auth)
	if len(s.hubConnections) == 0 && s.config.HubEndpoint != "" {
		conn, err := s.createHubConnectionFromConfig()
		if err != nil {
			slog.Warn("Failed to create hub connection from config", "error", err)
		} else {
			name := brokercredentials.DeriveHubName(s.config.HubEndpoint)
			if name == "" {
				name = "default"
			}
			s.hubMu.Lock()
			s.hubConnections[name] = conn
			s.hubMu.Unlock()
		}
	}

	// 6. Build multi-key auth middleware from all connections' secret keys
	s.buildAuthMiddleware()

	// Update BrokerID from first connection if not already set
	if s.config.BrokerID == "" {
		s.hubMu.RLock()
		for _, conn := range s.hubConnections {
			if conn.BrokerID != "" {
				s.config.BrokerID = conn.BrokerID
				break
			}
		}
		s.hubMu.RUnlock()
	}

	slog.Info("Hub integration initialized",
		"connections", len(s.hubConnections),
		"cache", cacheDir,
		"max_size_mb", maxSize/(1024*1024),
	)

	return nil
}

// createHubConnection creates a HubConnection from credentials.
func (s *Server) createHubConnection(name string, creds *brokercredentials.BrokerCredentials) (*HubConnection, error) {
	// Decode secret key
	var secretKey []byte
	if creds.SecretKey != "" {
		var err error
		secretKey, err = base64.StdEncoding.DecodeString(creds.SecretKey)
		if err != nil {
			return nil, fmt.Errorf("failed to decode secret key: %w", err)
		}
	}

	// Determine hub endpoint
	hubEndpoint := creds.HubEndpoint
	if hubEndpoint == "" {
		hubEndpoint = s.config.HubEndpoint
	}

	// Build hub client options
	opts := buildHubClientOpts(creds, secretKey)

	// Resolve transport auth once and share between REST client and control channel
	var transportSrc transportauth.TokenSource
	var transportMode transportauth.HeaderMode
	src, mode, err := transportauth.ResolveBrokerTransport(creds.TransportMode, creds.TransportAudience, adcsource.New)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve transport auth: %w", err)
	}
	if src != nil {
		transportSrc = src
		transportMode = mode
		opts = append(opts, hubclient.WithTransportAuth(src, mode))
		slog.Info("Hub connection using transport auth", "name", name, "mode", creds.TransportMode)
	}

	client, err := hubclient.New(hubEndpoint, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create Hub client: %w", err)
	}

	// Create hydrator + harness-config resolver using shared caches
	var hydrator *templatecache.Hydrator
	if s.cache != nil {
		hydrator = templatecache.NewHydrator(s.cache, client)
	}
	var hcResolver *templatecache.Resolver
	if s.hcCache != nil {
		hcResolver = templatecache.NewHarnessConfigResolver(s.hcCache, client)
	}

	conn := &HubConnection{
		Name:            name,
		HubEndpoint:     hubEndpoint,
		BrokerID:        creds.BrokerID,
		AuthMode:        creds.AuthMode,
		Credentials:     creds,
		SecretKey:       secretKey,
		TransportSource: transportSrc,
		TransportMode:   transportMode,
		HubClient:       client,
		Hydrator:        hydrator,
		HCResolver:      hcResolver,
		Status:          ConnectionStatusDisconnected,
	}

	return conn, nil
}

// createHubConnectionFromConfig creates a HubConnection from server config
// (bearer token or dev-auth), without file-based credentials.
func (s *Server) createHubConnectionFromConfig() (*HubConnection, error) {
	var opts []hubclient.Option

	if s.config.HubToken != "" {
		opts = append(opts, hubclient.WithBearerToken(s.config.HubToken))
		slog.Info("Hub client using bearer token authentication")
	} else {
		opts = append(opts, hubclient.WithAutoDevAuth())
		slog.Info("Hub client using auto dev authentication")
	}

	// Resolve transport auth once from env and share between REST client and control channel
	var transportSrc transportauth.TokenSource
	var transportMode transportauth.HeaderMode
	if src, err := transportauth.FromEnv(); src != nil && err == nil {
		transportSrc = src
		transportMode = transportauth.ModeFromEnv()
		opts = append(opts, hubclient.WithTransportAuth(src, transportMode))
	}

	client, err := hubclient.New(s.config.HubEndpoint, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create Hub client: %w", err)
	}

	var hydrator *templatecache.Hydrator
	if s.cache != nil {
		hydrator = templatecache.NewHydrator(s.cache, client)
	}
	var hcResolver *templatecache.Resolver
	if s.hcCache != nil {
		hcResolver = templatecache.NewHarnessConfigResolver(s.hcCache, client)
	}

	conn := &HubConnection{
		Name:            "default",
		HubEndpoint:     s.config.HubEndpoint,
		BrokerID:        s.config.BrokerID,
		TransportSource: transportSrc,
		TransportMode:   transportMode,
		HubClient:       client,
		Hydrator:        hydrator,
		HCResolver:      hcResolver,
		Status:          ConnectionStatusDisconnected,
	}

	return conn, nil
}

// tryLegacyCredentials attempts to load from legacy single-file Store
// and migrate to the MultiStore.
func (s *Server) tryLegacyCredentials() {
	credPath := s.config.BrokerCredentialsPath
	if credPath == "" {
		credPath = brokercredentials.DefaultPath()
	}

	legacyStore := brokercredentials.NewStore(credPath)
	if !legacyStore.Exists() {
		return
	}

	slog.Info("Found legacy credentials file, migrating to multi-store", "path", credPath)

	// Migrate
	if err := s.multiCredStore.MigrateFromLegacy(credPath); err != nil {
		slog.Warn("Failed to migrate legacy credentials", "error", err)

		// Still try to load directly
		creds, err := legacyStore.Load()
		if err != nil {
			slog.Warn("Failed to load legacy credentials", "error", err)
			return
		}

		name := brokercredentials.DeriveHubName(creds.HubEndpoint)
		if name == "" {
			name = "default"
		}
		creds.Name = name

		conn, err := s.createHubConnection(name, creds)
		if err != nil {
			slog.Warn("Failed to create hub connection from legacy credentials", "error", err)
			return
		}
		s.hubMu.Lock()
		s.hubConnections[name] = conn
		s.hubMu.Unlock()
		return
	}

	// Reload from multi-store after migration
	multiCreds, err := s.multiCredStore.List()
	if err != nil {
		slog.Warn("Failed to list credentials after migration", "error", err)
		return
	}

	for i := range multiCreds {
		c := &multiCreds[i]
		if _, exists := s.hubConnections[c.Name]; exists {
			continue
		}
		conn, err := s.createHubConnection(c.Name, c)
		if err != nil {
			slog.Warn("Failed to create hub connection after migration", "name", c.Name, "error", err)
			continue
		}
		s.hubMu.Lock()
		s.hubConnections[c.Name] = conn
		s.hubMu.Unlock()
		slog.Info("Created hub connection from migrated credentials", "name", c.Name, "brokerID", c.BrokerID)
	}
}

// buildAuthMiddleware creates or rebuilds the multi-key auth middleware
// from all hub connections' secret keys.
func (s *Server) buildAuthMiddleware() {
	s.hubMu.RLock()
	var keys []secretKeyEntry
	for _, conn := range s.hubConnections {
		if len(conn.SecretKey) > 0 {
			keys = append(keys, secretKeyEntry{
				hubName:   conn.Name,
				secretKey: conn.SecretKey,
			})
		}
	}
	s.hubMu.RUnlock()

	if !s.config.BrokerAuthEnabled || len(keys) == 0 {
		s.brokerAuthMiddleware = nil
		return
	}

	if s.brokerAuthMiddleware == nil {
		s.brokerAuthMiddleware = NewMultiKeyBrokerAuthMiddleware(
			true,
			5*time.Minute,
			!s.config.BrokerAuthStrictMode,
		)
		if s.config.BrokerAuthStrictMode {
			slog.Info("Broker auth middleware enabled (strict mode)", "keys", len(keys))
		} else {
			slog.Info("Broker auth middleware enabled (permissive mode)", "keys", len(keys))
		}
	}

	s.brokerAuthMiddleware.UpdateKeys(keys)
}

func isLoopbackHost(host string) bool {
	h := strings.TrimSpace(host)
	if h == "" || strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) authKeyCount() int {
	s.hubMu.RLock()
	defer s.hubMu.RUnlock()
	count := 0
	for _, conn := range s.hubConnections {
		if len(conn.SecretKey) > 0 {
			count++
		}
	}
	return count
}

func (s *Server) validateBrokerAuthStartup() error {
	strictAuthConfigured := s.config.BrokerAuthEnabled && s.config.BrokerAuthStrictMode
	hasKeys := s.authKeyCount() > 0
	loopbackOnly := isLoopbackHost(s.config.Host)

	// Hub-connected brokers without keys are in a "pending registration" state.
	// They must be allowed to start so that `scion broker register` can reach
	// the local health endpoint and complete the HMAC key exchange.
	// The credential watcher will pick up keys once registration finishes.
	if s.config.HubEnabled && !hasKeys {
		if !loopbackOnly {
			return fmt.Errorf("runtime broker API bound to %q in hub mode requires HMAC auth keys; register first on loopback or provide credentials", s.config.Host)
		}
		slog.Warn("Runtime Broker starting in hub mode without HMAC keys — pending registration",
			"host", s.config.Host,
			"hint", "run 'scion runtime-broker register' to complete setup",
		)
		return nil
	}

	// Non-loopback listeners must not run without strict broker auth and keys.
	if !loopbackOnly && (!strictAuthConfigured || !hasKeys) {
		return fmt.Errorf("runtime broker API bound to %q requires strict broker auth with valid HMAC keys", s.config.Host)
	}

	// Loopback-only listeners may be temporarily permissive, but emit a warning.
	if loopbackOnly && (!strictAuthConfigured || !hasKeys) {
		slog.Warn("Runtime Broker API is loopback-only and running without strict broker auth",
			"host", s.config.Host,
			"brokerAuthEnabled", s.config.BrokerAuthEnabled,
			"brokerAuthStrictMode", s.config.BrokerAuthStrictMode,
			"authKeys", s.authKeyCount(),
		)
	}

	return nil
}

// SetRequestLogger sets the dedicated request logger.
func (s *Server) SetRequestLogger(l *slog.Logger) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requestLogger = l
}

// SetMessageLogger sets the dedicated message audit logger.
func (s *Server) SetMessageLogger(l *slog.Logger) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dedicatedMessageLog = l
}

// SetHubClient sets the Hub client for template hydration.
// This is useful for testing or when the client is configured externally.
func (s *Server) SetHubClient(client hubclient.Client) {
	s.hubMu.Lock()
	defer s.hubMu.Unlock()

	// Update or create the "default" connection
	conn, ok := s.hubConnections["default"]
	if !ok {
		conn = &HubConnection{
			Name:   "default",
			Status: ConnectionStatusDisconnected,
		}
		s.hubConnections["default"] = conn
	}
	conn.HubClient = client
	if s.cache != nil {
		conn.Hydrator = templatecache.NewHydrator(s.cache, client)
	}
	if s.hcCache != nil {
		conn.HCResolver = templatecache.NewHarnessConfigResolver(s.hcCache, client)
	}
}

// SetTemplateCache sets the template cache.
// This is useful for testing or when the cache is configured externally.
func (s *Server) SetTemplateCache(cache *templatecache.Cache) {
	s.cache = cache
	s.hubMu.Lock()
	defer s.hubMu.Unlock()
	for _, conn := range s.hubConnections {
		if conn.HubClient != nil {
			conn.Hydrator = templatecache.NewHydrator(cache, conn.HubClient)
			if s.hcCache != nil {
				conn.HCResolver = templatecache.NewHarnessConfigResolver(s.hcCache, conn.HubClient)
			}
		}
	}
}

// GetHydrator returns the template hydrator from the first available connection.
func (s *Server) GetHydrator() *templatecache.Hydrator {
	s.hubMu.RLock()
	defer s.hubMu.RUnlock()
	for _, conn := range s.hubConnections {
		if conn.Hydrator != nil {
			return conn.Hydrator
		}
	}
	return nil
}

// Start starts the HTTP server.
func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	s.startTime = time.Now()
	if err := s.validateBrokerAuthStartup(); err != nil {
		s.mu.Unlock()
		return err
	}

	handler := s.applyMiddleware(s.mux)

	s.httpServer = &http.Server{
		Addr:         fmt.Sprintf("%s:%d", s.config.Host, s.config.Port),
		Handler:      handler,
		ReadTimeout:  s.config.ReadTimeout,
		WriteTimeout: s.config.WriteTimeout,
	}
	s.mu.Unlock()

	slog.Info("Runtime Broker API server starting",
		"host", s.config.Host,
		"port", s.config.Port,
	)
	if s.config.Debug {
		slog.Debug("Broker details",
			"brokerID", s.config.BrokerID,
			"brokerName", s.config.BrokerName,
			"hub_endpoint", s.config.HubEndpoint,
			"hub_connections", len(s.hubConnections),
		)
	}

	// Runtime broker boot hook: warn about legacy environment variables
	// scion no longer reads, before scanning projects/. This is the
	// broker-boot half of the legacy-migration hook points;
	// the CLI equivalent is Execute()/rootCmd.PersistentPreRunE in
	// cmd/root.go (skipped for "start" under the "server"/"runtime-broker"
	// subtree — this hook reports instead), and the hub equivalent is
	// cmd/server_foreground.go:runServerStart. Both this hook and the hub's
	// share config's process-wide sync.Once (WarnRemovedLegacyEnvOnce), so
	// a combined `--enable-hub --enable-runtime-broker` process reports
	// once, not twice. On-disk layout migration is expected to run at this
	// same hook point.
	config.WarnRemovedLegacyEnvOnce(os.Getenv, config.NewSlogReporter())
	// Per-project migration (config.ReadProjectID, as projects load) reports
	// through slog here too. This is already the default, but set it
	// explicitly so all three boot hooks (CLI, hub, broker) are visible at
	// their call sites.
	config.SetProjectMigrationReporter(config.NewSlogReporter())
	// Migrate the global ~/.scion layout before scanning projects/ below.
	if globalDir, err := config.GetGlobalDir(); err == nil {
		config.MigrateLegacyGlobalLayoutOnce(globalDir, config.NewSlogReporter())
	} else {
		slog.Warn("skipping legacy layout migration: could not resolve global directory", "error", err)
	}

	// Discover auxiliary runtimes (e.g. Kubernetes) from project settings
	// so that agents running on non-default runtimes can be found after
	// a broker restart.
	s.discoverAuxiliaryRuntimes()

	// Check (and, with nfs.auto_mount, mount) the configured NFS shares.
	// This runs in the background: an NFS mount or mountpoint check against
	// an unreachable server can block for minutes, and a failed mount must
	// not delay or stop the broker from serving projects that do not use
	// NFS. Until the first pass finishes, /healthz reports the shares as not
	// reconciled.
	s.startNFSReconcileLoop(ctx)

	// Start all hub connections' services
	s.hubMu.RLock()
	for name, conn := range s.hubConnections {
		if err := conn.Start(ctx, s); err != nil {
			slog.Error("Failed to start hub connection", "name", name, "error", err)
		}
	}
	s.hubMu.RUnlock()

	// Log a summary of all hub connections
	s.logHubConnections()

	// Start credential watcher for dynamic reload
	if s.config.HubEnabled && s.multiCredStore != nil {
		s.startCredentialWatcher(ctx)
	}

	errCh := make(chan error, 1)
	go func() {
		if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		return s.Shutdown(context.Background())
	}
}

// startNFSReconcileLoop starts the NFS reconciler's background loop (see
// NFSMountReconciler.Run). It is a no-op when NFS is not configured, and
// starts at most one loop per Server. The loop stops when ctx is cancelled
// or Shutdown is called.
func (s *Server) startNFSReconcileLoop(ctx context.Context) {
	if s.nfsMountReconciler == nil {
		return
	}
	s.nfsReconcileOnce.Do(func() {
		loopCtx, cancel := context.WithCancel(ctx)
		s.mu.Lock()
		s.nfsReconcileCancel = cancel
		s.mu.Unlock()
		go func() {
			defer close(s.nfsReconcileStopped)
			s.nfsMountReconciler.Run(loopCtx, s.nfsReconcileInterval, func() {
				s.logNFSStartupResult()
				close(s.nfsStartupReconcileDone)
			})
		}()
	})
}

// logNFSStartupResult logs the outcome of the first reconcile pass. Failures
// are recorded per share (surfaced in /healthz and by scion doctor) and
// never stop the broker.
func (s *Server) logNFSStartupResult() {
	r := s.nfsMountReconciler
	if r.IsHealthy() {
		slog.Info("NFS mounts checked at startup",
			"status", r.HealthCheckString(), "autoMount", r.AutoMount())
		return
	}
	slog.Error("NFS mounts unhealthy at startup; the broker keeps serving",
		"detail", r.HealthCheckString(), "autoMount", r.AutoMount())
}

// Shutdown gracefully shuts down the server.
func (s *Server) Shutdown(ctx context.Context) error {
	// Write any resolution cache entries still waiting for their delayed
	// write. Deferred so it runs on every return path, and after the HTTP
	// server has drained, when in-flight requests have finished adding to it.
	defer func() {
		if s.ghResolutionCache != nil {
			s.ghResolutionCache.Flush()
		}
	}()

	// Stop credential watcher
	s.mu.RLock()
	srv := s.httpServer
	credWatcherStop := s.credWatcherStop
	s.mu.RUnlock()

	if credWatcherStop != nil {
		slog.Info("Stopping credential watcher...")
		close(credWatcherStop)
	}

	// Stop the NFS reconcile loop. Cancelling its context kills a mount
	// command in progress (its whole process group), so the loop ends
	// promptly; it is not waited for.
	s.mu.RLock()
	nfsCancel := s.nfsReconcileCancel
	s.mu.RUnlock()
	if nfsCancel != nil {
		nfsCancel()
	}

	// Cancel every start still running on a start, restart or create
	// handler and wait for Run's deferred cleanup, before the hub
	// connections and the HTTP server drain, so no start outlives the
	// starts this process last reported in flight.
	// One deadline covers both this wait and the HTTP drain below.
	ctx, cancel := context.WithTimeout(ctx, shutdownDeadline)
	defer cancel()
	if !s.startsInFlight.cancelAllAndWait(ctx) {
		slog.Warn("Shutdown proceeding before every cancelled start finished its cleanup")
	}

	// Stop all hub connections
	s.hubMu.RLock()
	for _, conn := range s.hubConnections {
		conn.Stop()
	}
	s.hubMu.RUnlock()

	if srv == nil {
		return nil
	}

	slog.Info("Runtime Broker API server shutting down...")

	return srv.Shutdown(ctx)
}

// Handler returns the HTTP handler for the server.
// This is useful for testing without starting a listener.
func (s *Server) Handler() http.Handler {
	return s.applyMiddleware(s.mux)
}

// getAuxiliaryManagers returns the managers for all registered auxiliary runtimes.
func (s *Server) getAuxiliaryManagers() []agent.Manager {
	entries := s.sortedAuxiliaryRuntimes()
	managers := make([]agent.Manager, 0, len(entries))
	for _, aux := range entries {
		managers = append(managers, aux.Manager)
	}
	return managers
}

// namedAuxiliaryRuntime pairs an auxiliaryRuntime with the identity key it is
// stored under, for callers (logging, debugging) that want both.
type namedAuxiliaryRuntime struct {
	identity string
	auxiliaryRuntime
}

// sortedAuxiliaryRuntimes returns a snapshot of the currently registered
// auxiliary runtimes ordered by identity key. More than one auxiliary
// runtime of the same type can be registered (see
// auxiliaryRuntimeIdentity), so first-match-wins consumers (LookupAgent,
// resolveAgentRuntimeTarget, and LookupContainerID via auxListAgentsSorted,
// which sorts the same keys) need a deterministic iteration order — the
// same pattern resolveDeleteTarget already used for its manager list —
// rather than depending on Go's randomized map order.
func (s *Server) sortedAuxiliaryRuntimes() []namedAuxiliaryRuntime {
	s.auxiliaryRuntimesMu.RLock()
	defer s.auxiliaryRuntimesMu.RUnlock()

	keys := make([]string, 0, len(s.auxiliaryRuntimes))
	for k := range s.auxiliaryRuntimes {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	entries := make([]namedAuxiliaryRuntime, 0, len(keys))
	for _, k := range keys {
		entries = append(entries, namedAuxiliaryRuntime{identity: k, auxiliaryRuntime: s.auxiliaryRuntimes[k]})
	}
	return entries
}

// discoverAuxiliaryRuntimes scans project settings for runtime profiles that
// resolve to a runtime different from the broker's default. Any discovered
// non-default runtimes are registered as auxiliary runtimes so that agents
// running on them (e.g. Kubernetes pods) can be found after a broker restart.
func (s *Server) discoverAuxiliaryRuntimes() {
	// Collect project paths to scan
	var projectPaths []string

	// Hub-managed projects: ~/.scion/projects/<slug>/.scion/
	globalDir, err := config.GetGlobalDir()
	if err == nil {
		projectsDir := filepath.Join(globalDir, "projects")
		entries, err := os.ReadDir(projectsDir)
		if err == nil {
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				scionDir := filepath.Join(projectsDir, e.Name(), ".scion")
				if _, err := os.Stat(scionDir); err == nil {
					projectPaths = append(projectPaths, scionDir)
				}
			}
		}
	}

	// Current project dir
	if pd, _ := config.GetResolvedProjectDir(""); pd != "" {
		projectPaths = append(projectPaths, pd)
	}

	s.discoverAuxiliaryRuntimesForProjects(projectPaths)
}

// discoverAuxiliaryRuntimesForProjects scans the given project settings
// directories for runtime profiles that resolve to a runtime different from
// the broker's default, and registers each distinct resolved runtime once.
//
// De-duplication is keyed by the resolved runtime's IDENTITY (see
// auxiliaryRuntimeIdentity), not by its TYPE: two profiles that resolve to
// different Kubernetes clusters/contexts/namespaces are distinct runtimes and
// must both be registered, while two profiles that resolve to the same
// underlying runtime must be registered once. Keying by type alone would
// register only one of several distinct same-type runtimes, chosen by map
// iteration order. See ptone/scion#2260.
//
// Iteration over profile names is sorted so registration does not depend on
// map order.
//
// Split out from discoverAuxiliaryRuntimes so tests can exercise resolution,
// de-duplication and registration directly, without depending on the
// broker's on-disk project discovery.
func (s *Server) discoverAuxiliaryRuntimesForProjects(projectPaths []string) {
	// Compared against every resolved profile below so that a profile
	// pointing at the broker's own runtime instance — not merely the same
	// TYPE as the broker's default — is recognized and skipped. A broker
	// whose default is Kubernetes must still register a profile aimed at a
	// different cluster/context/namespace as an auxiliary runtime; comparing
	// types alone would wrongly treat it as "the default" and drop it.
	defaultIdentity := auxiliaryRuntimeIdentity(s.runtime)

	discoveredIdentities := make(map[string]bool)

	for _, gp := range projectPaths {
		vs, _, _ := config.LoadEffectiveSettings(gp)
		if vs == nil {
			continue
		}

		profileNames := make([]string, 0, len(vs.Profiles))
		for profileName := range vs.Profiles {
			profileNames = append(profileNames, profileName)
		}
		sort.Strings(profileNames)

		for _, profileName := range profileNames {
			// Only used here to skip profiles whose runtime entry doesn't
			// exist at all; the actual default/de-dup comparisons below are
			// by resolved IDENTITY, not by this settings-level type string,
			// so settings spellings that alias one runtime type (e.g. "k8s"
			// and "kubernetes") don't register twice.
			if _, _, err := vs.ResolveRuntime(profileName); err != nil {
				continue
			}

			resolved := s.resolveAuxiliaryRuntime(gp, "", profileName)
			if resolved.Name() == "error" {
				slog.Warn("Failed to resolve auxiliary runtime", "profile", profileName)
				continue
			}

			identity := auxiliaryRuntimeIdentity(resolved)
			if identity == defaultIdentity || discoveredIdentities[identity] {
				continue
			}
			discoveredIdentities[identity] = true

			mgr := agent.NewManager(resolved)
			s.auxiliaryRuntimesMu.Lock()
			s.auxiliaryRuntimes[identity] = auxiliaryRuntime{Runtime: resolved, Manager: mgr}
			s.auxiliaryRuntimesMu.Unlock()

			slog.Info("Discovered auxiliary runtime from project settings",
				"runtime", resolved.Name(), "identity", identity, "profile", profileName)
		}
	}
}

// auxiliaryRuntimeIdentity returns a key that identifies a specific resolved
// runtime INSTANCE, not merely its type. It is used to de-duplicate
// discovered auxiliary runtimes, as the key under which they are stored in
// Server.auxiliaryRuntimes, and to compare a resolved profile against the
// broker's own default runtime — so that two distinct instances of the same
// runtime type (e.g. Kubernetes pointed at different clusters/contexts/
// namespaces) are tracked and addressed separately, while two profiles that
// resolve to the same instance collapse to one entry.
//
// The type component always comes from rt.Name() — the runtime's own
// canonical name — never from a settings-level type string: two settings
// spellings that alias one runtime type (e.g. "k8s" and "kubernetes", both
// accepted by runtime.GetRuntime) must produce the same identity.
//
// Extra fields are appended only for a runtime type with per-instance config
// that both (a) varies between two profiles of the same type on one broker
// process and (b) is actually consumed by the runtime, so it genuinely
// distinguishes one reachable target from another: Kubernetes (context,
// namespace). Every other type keeps the bare type-name identity today:
//
//   - Docker/Podman: Host is assigned onto DockerRuntime/PodmanRuntime from
//     settings, but neither runtime reads it back (no -H/--host flag, no
//     DOCKER_HOST/CONTAINER_HOST wiring) — every instance talks to the same
//     local daemon regardless of Host, so Host cannot distinguish one from
//     another. It also isn't normalized (an empty host, "unix:///var/run/
//     docker.sock" and "/var/run/docker.sock" would otherwise be three
//     identities for one daemon). If per-host Docker/Podman is wanted later,
//     it belongs with the change that wires Host into the command actually
//     run, together with socket-spelling normalization.
//   - Cloud Run: ProjectID/Location are resolved lazily from GCE metadata
//     when auto-detected, so an unresolved instance and the same instance
//     after its first API call would otherwise produce two identities for
//     one target, and reading them here would be unsynchronized with
//     CloudRunRuntime's own resolution: that path holds a mutex while it
//     writes ProjectID/Location, and this identity function does not take
//     that lock. Project/region disambiguation for Cloud Run is a follow-up,
//     not made here.
//   - Apple container, Cloud Run Sandbox: no per-instance config exists at
//     all today.
func auxiliaryRuntimeIdentity(rt scionrt.Runtime) string {
	switch r := rt.(type) {
	case *scionrt.KubernetesRuntime:
		ctx := ""
		if r.Client != nil {
			ctx = r.Client.CurrentContext
		}
		return fmt.Sprintf("%s|context=%s|namespace=%s", rt.Name(), ctx, r.DefaultNamespace)
	default:
		return rt.Name()
	}
}

// canonicalRuntimeTypeName normalizes "k8s" to "kubernetes" for comparing a
// profile's declared type against a resolved runtime's Name(): "k8s" and
// "kubernetes" both resolve to the same KubernetesRuntime (see
// runtime.GetRuntime), but Name() always returns "kubernetes". Other
// settings-level aliases of "kubernetes" (e.g. "remote") are not normalized
// here, so a profile declared with one of those falls through to full
// resolution instead of being matched by this cheap comparison.
func canonicalRuntimeTypeName(runtimeType string) string {
	if runtimeType == "k8s" {
		return "kubernetes"
	}
	return runtimeType
}

// defaultRuntimeMatchesProfile reports whether a profile's settings-declared
// runtime type (and, for Kubernetes, target cluster/namespace) is the same as
// the broker's own default runtime — WITHOUT fully resolving the profile
// (runtime.GetRuntime, which for Kubernetes also calls Client.Verify(), a
// live API round trip). This lets resolveManagerForOpts return the shared
// manager for the common case (a start on the broker's own default) at the
// cost of a settings-string comparison, instead of paying a network call —
// and risking a start failing on a transient Verify error — for every single
// agent start.
//
// A false result does not mean "this is definitely a different runtime"; it
// means the cheap check could not prove a match, so the caller should fall
// back to fully resolving the profile for a conclusive answer.
func (s *Server) defaultRuntimeMatchesProfile(runtimeType string, rtConfig config.V1RuntimeConfig) bool {
	if canonicalRuntimeTypeName(runtimeType) != s.runtime.Name() {
		return false
	}

	defaultK8s, isDefaultK8s := s.runtime.(*scionrt.KubernetesRuntime)
	if !isDefaultK8s {
		// Every non-Kubernetes type uses the bare type-name identity
		// (see auxiliaryRuntimeIdentity): a type match is an identity match,
		// so there is nothing further to resolve.
		return true
	}
	if defaultK8s.Client == nil {
		return false
	}

	context := rtConfig.Context
	if context == "" {
		// An unset context targets the kubeconfig's current context. Resolve
		// it the same way runtime.GetRuntime does (k8s.NewClientWithContext),
		// but without the Verify() call that follows it there: building the
		// client only reads local kubeconfig/in-cluster files, it never
		// dials the cluster. A failure here is not conclusive — e.g. no
		// kubeconfig reachable from this process — so fall back to the full
		// resolution path rather than guessing.
		client, err := k8s.NewClientWithContext(os.Getenv("KUBECONFIG"), "")
		if err != nil {
			return false
		}
		context = client.CurrentContext
	}
	if context != defaultK8s.Client.CurrentContext {
		return false
	}

	namespace := rtConfig.Namespace
	if namespace == "" {
		namespace = scionrt.DefaultKubernetesNamespace()
	}
	return namespace == defaultK8s.DefaultNamespace
}

// ErrAgentListUnavailable wraps a runtime.List failure encountered while
// resolving an agent slug (e.g. LookupAgent, LookupContainerID). It is
// distinct from "no such agent": the container runtime itself failed to
// respond (e.g. an intermittent `docker ps` error), so the caller should
// treat this as retryable rather than reporting the agent as missing.
var ErrAgentListUnavailable = errors.New("agent runtime listing temporarily unavailable")

// ErrAgentNotFound marks a lookup result from LookupContainerID that must be
// treated the same as a genuine "no such agent": either the runtime listing
// succeeded but no agent matched the requested slug/project, or a matching
// agent record was found but carries no resolvable container id at all (no
// "scion.container.id" label, no ContainerID, no ID) — e.g. a malformed or
// partial runtime entry that carries no container id — nothing addressable
// to stop. In both cases there is nothing present to act on, so callers use
// errors.Is(err, ErrAgentNotFound) to fold this into the idempotent "not
// found" path (skip stop, proceed to start on restart) rather than
// aborting. This is distinct from any other lookup failure (a runtime
// listing error, an ambiguous match), which reflects a real problem
// resolving an agent that may well exist and must still be surfaced as an
// error rather than treated as "not found".
var ErrAgentNotFound = errors.New("agent not found")

// agentNotFoundError implements the existing "agent '<slug>' not found"
// message while allowing errors.Is(err, ErrAgentNotFound) to match it.
type agentNotFoundError struct{ slug string }

func (e *agentNotFoundError) Error() string {
	return fmt.Sprintf("agent '%s' not found", e.slug)
}

func (e *agentNotFoundError) Is(target error) bool {
	return target == ErrAgentNotFound
}

// LookupContainerID implements AgentLookup interface.
// It looks up an agent by slug and returns its container ID.
// projectID scopes the lookup to prevent cross-project collision.
// See lookupAgentTarget for the resolution rules and error types.
func (s *Server) LookupContainerID(ctx context.Context, slug, projectID string) (string, error) {
	containerID, _, _, err := s.lookupAgentTarget(ctx, slug, projectID)
	return containerID, err
}

// lookupAgentTarget resolves an agent slug to its container identifier
// (container ID for docker/podman, pod name for k8s, or whatever a runtime
// reports) and also returns the agent.Manager and scionrt.Runtime pair
// whose List call produced the match, so a caller that goes on to act on
// the target (e.g. Stop, Exec) sends the operation to the same runtime the
// identifier came from rather than to a manager or runtime re-derived by a
// second, independent lookup. The manager and runtime are paired at the
// stage that produced the match — the default runtime's own manager and
// runtime together when the default stage matches, or the single
// auxiliaryRuntime entry auxListAgentsSorted matched against — so there is
// no point at which one is resolved independently of the other.
//
// Resolution runs in two stages, each consulting the default runtime first
// and then every auxiliary runtime in sorted identity order (auxListAgentsSorted):
//
//  1. a project-scoped search (scion.name plus the project label);
//  2. only when projectID is set and stage 1 found nothing, a slug-only
//     search that accepts just containers carrying no project label at all
//     (pre-label or solo/CLI containers — see agentsWithoutProjectLabel).
//
// Errors:
//   - a default-runtime List failure, in either stage, wraps
//     ErrAgentListUnavailable;
//   - an auxiliary-runtime List failure is decisive only when no runtime
//     produced a match in that stage: a match found on another runtime is
//     authoritative over an error seen along the way, but "no match after a
//     failed List" wraps ErrAgentListUnavailable rather than claiming the
//     agent is absent, and ends the lookup without running the next stage;
//   - more than one matching entry returns uniqueAgentEntry's (unwrapped)
//     ambiguity error;
//   - no match at all, or a matched record carrying no container identifier
//     (nothing addressable to act on), satisfies errors.Is(err,
//     ErrAgentNotFound).
//
// Every error errs toward an explicit failure, never toward a false
// not-found or a wrong target.
func (s *Server) lookupAgentTarget(ctx context.Context, slug, projectID string) (string, agent.Manager, scionrt.Runtime, error) {
	m, err := s.lookupAgentMatch(ctx, slug, projectID)
	if err != nil {
		return "", nil, nil, err
	}
	return m.containerID, m.manager, m.runtime, nil
}

// agentMatch is the result of lookupAgentMatch: the container identifier,
// the manager and runtime that listed it, and the listed entry itself.
//
// matched reports that exactly one entry matched. It is also set alongside
// the ErrAgentNotFound returned for a matched entry that carries no
// container identifier: there is nothing to act on, but the entry's listed
// name and project path are still the agent's.
type agentMatch struct {
	containerID string
	manager     agent.Manager
	runtime     scionrt.Runtime
	entry       api.AgentInfo
	matched     bool
}

// lookupAgentMatch is lookupAgentTarget's search, returning the matched
// entry as well, so a caller that needs the agent's listed name or project
// path reads them from the same entry it acts on. Resolution order and
// errors are exactly lookupAgentTarget's.
func (s *Server) lookupAgentMatch(ctx context.Context, slug, projectID string) (agentMatch, error) {
	if s.manager == nil {
		return agentMatch{}, fmt.Errorf("agent manager not available")
	}

	slug = strings.ToLower(slug)

	filter := scopedNameFilter(slug, projectID)
	// A recorded runtime type (ptone/scion#2748) can exclude the default
	// runtime; auxListAgentsSorted applies the same restriction.
	useDefault := s.defaultRuntimeAllowed(ctx)
	var agents []api.AgentInfo
	var err error
	if useDefault {
		agents, err = s.manager.List(ctx, filter)
		if err != nil {
			return agentMatch{}, fmt.Errorf("%w: failed to list agents: %w", ErrAgentListUnavailable, err)
		}
		agents = agentsForProject(agents, projectID)
	}
	matchManager := s.manager
	matchRuntime := s.runtime

	// Fall back to auxiliary runtimes (e.g. kubernetes when default is docker)
	if len(agents) == 0 {
		auxAgents, auxManager, auxRuntime, auxErr := s.auxListAgentsSorted(ctx, slug, false, filter, func(a []api.AgentInfo) []api.AgentInfo { return agentsForProject(a, projectID) })
		if auxErr != nil {
			return agentMatch{}, auxErr
		}
		agents = auxAgents
		matchManager = auxManager
		matchRuntime = auxRuntime
	}

	// Backward compatibility: retry without project filter, but only accept
	// containers that lack a project label (pre-existing agents or solo/CLI
	// mode). A container labeled for a different project must not match a
	// project-scoped request, or same-slug agents across projects would collide.
	if len(agents) == 0 && projectID != "" {
		fallbackFilter := map[string]string{"scion.name": slug}
		if useDefault {
			agents, err = s.manager.List(ctx, fallbackFilter)
			if err != nil {
				return agentMatch{}, fmt.Errorf("%w: failed to list agents: %w", ErrAgentListUnavailable, err)
			}
			agents = agentsWithoutProjectLabel(agents)
		}
		matchManager = s.manager
		matchRuntime = s.runtime
		if len(agents) == 0 {
			auxAgents, auxManager, auxRuntime, auxErr := s.auxListAgentsSorted(ctx, slug, true, fallbackFilter, agentsWithoutProjectLabel)
			if auxErr != nil {
				return agentMatch{}, auxErr
			}
			agents = auxAgents
			matchManager = auxManager
			matchRuntime = auxRuntime
		}
	}

	if len(agents) == 0 {
		return agentMatch{}, &agentNotFoundError{slug: slug}
	}

	entry, err := uniqueAgentEntry(slug, agents)
	if err != nil {
		return agentMatch{}, err
	}

	// Get container ID - prefer label, then ContainerID from runtime, then ID
	containerID := entry.Labels["scion.container.id"]
	if containerID == "" {
		containerID = entry.ContainerID
	}
	if containerID == "" {
		containerID = entry.ID
	}
	if containerID == "" {
		return agentMatch{manager: matchManager, runtime: matchRuntime, entry: entry, matched: true},
			fmt.Errorf("agent '%s' has no container ID: %w", slug, ErrAgentNotFound)
	}

	return agentMatch{containerID: containerID, manager: matchManager, runtime: matchRuntime, entry: entry, matched: true}, nil
}

// auxListAgentsSorted queries every currently registered auxiliary runtime
// with filter, in sorted identity order (the same order allManagers() uses),
// and returns the first non-empty match after filterAgents narrows it, together
// with the agent.Manager whose List call produced that match.
//
// A match is authoritative: once one auxiliary runtime's List call succeeds
// and filterAgents leaves at least one entry, the remaining auxiliary
// runtimes are not consulted at all, regardless of whether an earlier one
// in the sorted order failed to list. Only when no auxiliary runtime
// produces a match does a List error along the way turn into an
// ErrAgentListUnavailable-wrapped error — an error from a runtime that was
// never going to match must not block a genuine match found elsewhere, but
// a failed List on the one runtime that may hold the agent must not be
// misread as "not found" either. The fixed iteration order keeps the
// outcome the same from one call to the next instead of depending on Go's
// randomized map order.
//
// slug and fallback are used only for the debug line logged on a match.
//
// The returned agent.Manager and scionrt.Runtime are always the pair
// registered together for the same auxiliary runtime identity (auxiliaryRuntime
// stores them together), so a caller never has to re-derive one from the
// other: they are paired at the moment the match is found, not looked up
// again afterward.
func (s *Server) auxListAgentsSorted(ctx context.Context, slug string, fallback bool, filter map[string]string, filterAgents func([]api.AgentInfo) []api.AgentInfo) ([]api.AgentInfo, agent.Manager, scionrt.Runtime, error) {
	var listErr error
	for _, aux := range s.sortedAuxiliaryRuntimesFor(ctx) {
		rtName := aux.identity
		auxAgents, auxErr := aux.Manager.List(ctx, filter)
		if auxErr != nil {
			if listErr == nil {
				listErr = fmt.Errorf("%w %q: %v", errAuxiliaryRuntimeList, rtName, auxErr)
			}
			continue
		}
		if matched := filterAgents(auxAgents); len(matched) > 0 {
			msg := "Agent found via auxiliary runtime"
			if fallback {
				msg += " (fallback)"
			}
			slog.Debug(msg, "slug", slug, "runtime", rtName)
			return matched, aux.Manager, aux.Runtime, nil
		}
	}
	if listErr != nil {
		return nil, nil, nil, fmt.Errorf("%w: %w", ErrAgentListUnavailable, listErr)
	}
	return nil, nil, nil, nil
}

// errAuxiliaryRuntimeList marks an ErrAgentListUnavailable error that came
// from an auxiliary runtime's List call (the default runtime listed
// successfully), so a caller can treat it differently from a default-runtime
// failure.
var errAuxiliaryRuntimeList = errors.New("auxiliary runtime")

// LookupAgent implements AgentLookup interface.
// It looks up an agent by slug and returns detailed info including the runtime.
// projectID scopes the lookup to prevent cross-project collision.
func (s *Server) LookupAgent(ctx context.Context, slug, projectID string) (*AgentLookupResult, error) {
	if s.manager == nil {
		return nil, fmt.Errorf("agent manager not available")
	}

	slug = strings.ToLower(slug)
	filter := scopedNameFilter(slug, projectID)

	// Try default manager first
	agents, err := s.manager.List(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("%w: failed to list agents: %w", ErrAgentListUnavailable, err)
	}
	agents = agentsForProject(agents, projectID)

	runtimeName := s.runtime.Name()
	var matchedRuntime scionrt.Runtime

	// listUnavailable tracks whether any consulted runtime's List call itself
	// failed (as opposed to succeeding with zero matches). A failure here must
	// not be indistinguishable from "no such agent": the caller (the PTY
	// stream classifier) needs to tell a genuinely missing agent (4404,
	// terminal) from a runtime that briefly could not answer (4503, retry).
	var listUnavailable bool

	// Fall back to auxiliary runtimes, in a deterministic (identity-sorted)
	// order: more than one auxiliary runtime can share a type, and in
	// pathological overlap cases (e.g. two Kubernetes namespace-scoped
	// entries plus one with ListAllNamespaces) more than one could plausibly
	// answer for the same slug.
	if len(agents) == 0 {
		for _, aux := range s.sortedAuxiliaryRuntimes() {
			auxAgents, auxErr := aux.Manager.List(ctx, filter)
			if auxErr != nil {
				listUnavailable = true
				continue
			}
			auxAgents = agentsForProject(auxAgents, projectID)
			if len(auxAgents) > 0 {
				agents = auxAgents
				// runtimeName must stay a canonical runtime type (e.g.
				// "kubernetes"), not the aux map key: that key is a
				// per-instance identity (see auxiliaryRuntimeIdentity) so
				// distinct same-type runtimes can be tracked separately.
				runtimeName = aux.Runtime.Name()
				matchedRuntime = aux.Runtime
				slog.Debug("Agent found via auxiliary runtime", "slug", slug, "runtime", aux.identity)
				break
			}
		}
	}

	// Backward compatibility: retry without project filter, but only accept
	// containers that lack a project label (pre-existing agents or solo/CLI
	// mode). A container labeled for a different project must not match a
	// project-scoped request, or same-slug agents across projects would collide.
	if len(agents) == 0 && projectID != "" {
		fallbackFilter := map[string]string{"scion.name": slug}
		agents, err = s.manager.List(ctx, fallbackFilter)
		if err != nil {
			return nil, fmt.Errorf("%w: failed to list agents: %w", ErrAgentListUnavailable, err)
		}
		agents = agentsWithoutProjectLabel(agents)
		if len(agents) == 0 {
			for _, aux := range s.sortedAuxiliaryRuntimes() {
				auxAgents, auxErr := aux.Manager.List(ctx, fallbackFilter)
				if auxErr != nil {
					listUnavailable = true
					continue
				}
				auxAgents = agentsWithoutProjectLabel(auxAgents)
				if len(auxAgents) > 0 {
					agents = auxAgents
					// See the identity note above: use the runtime's own
					// type, not the per-instance aux map key.
					runtimeName = aux.Runtime.Name()
					matchedRuntime = aux.Runtime
					slog.Debug("Agent found via auxiliary runtime (fallback)", "slug", slug, "runtime", aux.identity)
					break
				}
			}
		}
	}

	if len(agents) == 0 {
		if listUnavailable {
			return nil, fmt.Errorf("%w: an auxiliary runtime failed to list agents while resolving '%s'", ErrAgentListUnavailable, slug)
		}
		return nil, fmt.Errorf("agent '%s' not found", slug)
	}

	ag, err := uniqueAgentEntry(slug, agents)
	if err != nil {
		return nil, err
	}

	containerID := ag.Labels["scion.container.id"]
	if containerID == "" {
		containerID = ag.ContainerID
	}
	if containerID == "" {
		containerID = ag.ID
	}

	// Determine the exec user from the runtime that owns this agent.
	// resolvedRuntime is the same one that produced ag: nil matchedRuntime
	// means the primary s.manager/s.runtime pair matched.
	resolvedRuntime := matchedRuntime
	if resolvedRuntime == nil {
		resolvedRuntime = s.runtime
	}
	execUser := "scion"
	if resolvedRuntime != nil {
		execUser = resolvedRuntime.ExecUser()
	}

	// resolvedRuntime (computed above) is also the live instance that
	// actually produced this match: matchedRuntime is set on every
	// auxiliary-runtime match path above (including the no-project-label
	// fallback stage), and stays nil only when the match came from the
	// primary manager, in which case it's s.runtime. Callers use this to
	// ask capability questions (e.g. scionrt.HasAttachSupport) about the
	// runtime that actually owns the agent, not assume it is the broker's
	// default.
	result := &AgentLookupResult{
		ContainerID: containerID,
		RuntimeName: runtimeName,
		ExecUser:    execUser,
		// Phase is deliberately re-read from the runtime directly
		// (rawRuntimePhase), not taken from ag.Phase above: ag came from
		// s.manager.List / aux.Manager.List, which is agent.Manager's merged
		// view (runtime status overlaid with agent-info.json). That overlay
		// can keep reporting a stale "stopped"/"error" phase for a
		// container that has since restarted or is still starting up (see
		// pkg/agent/list.go's own reconciliation comment). Callers that
		// need to know the container's actual state right now — like
		// classifyAttachEnd's stopped check — need the runtime's own
		// unmerged phase instead.
		Phase:   rawRuntimePhase(ctx, resolvedRuntime, slug, containerID),
		Runtime: resolvedRuntime,
	}

	// Include K8s metadata if available
	if ag.Kubernetes != nil {
		result.Namespace = ag.Kubernetes.Namespace
	}

	// For kubernetes agents, include the Go K8s client for direct API access
	// (avoids needing kubectl in PATH and reuses the broker's auth)
	if runtimeName == "kubernetes" || runtimeName == "k8s" {
		if k8sRT, ok := resolvedRuntime.(*scionrt.KubernetesRuntime); ok && k8sRT.Client != nil {
			result.K8sConfig = k8sRT.Client.Config
			result.K8sClientset = k8sRT.Client.Clientset
		}
	}

	return result, nil
}

// rawRuntimePhase best-effort re-resolves containerID's lifecycle phase
// directly from rt.List, bypassing agent.Manager's agent-info.json overlay
// entirely (unlike the ag.Phase LookupAgent's caller already has, which came
// from that merged view). It never errors: a failed list call, a nil
// runtime, or a containerID that no longer appears in the listing all return
// "", which runningResultFromPhase (pty_classifier.go) already treats as
// unknown rather than guessing running or stopped. The name-only filter
// (matching LookupContainerID/LookupAgent's own backward-compat fallback
// filter) is intentionally broader than scopedNameFilter's project-scoped
// one — this runs after LookupAgent has already uniquely resolved
// containerID by whichever filter matched (the primary, auxiliary, or
// backward-compat fallback path), so matching back by containerID rather
// than re-deriving which filter won is both simpler and exact.
func rawRuntimePhase(ctx context.Context, rt scionrt.Runtime, slug, containerID string) string {
	if rt == nil || containerID == "" {
		return ""
	}
	agents, err := rt.List(ctx, map[string]string{"scion.name": slug})
	if err != nil {
		return ""
	}
	for _, a := range agents {
		id := a.Labels["scion.container.id"]
		if id == "" {
			id = a.ContainerID
		}
		if id == "" {
			id = a.ID
		}
		if id == containerID {
			return a.Phase
		}
	}
	return ""
}

// scopedNameFilter builds the Runtime.List label filter for a slug lookup,
// including the project scope label when a project is known so that runtimes
// can narrow the listing themselves (ptone/scion#1819).
func scopedNameFilter(slug, projectID string) map[string]string {
	filter := map[string]string{"scion.name": slug}
	if projectID != "" {
		filter[projectkeys.LabelProjectID] = projectID
	}
	return filter
}

// uniqueAgentEntry returns the single runtime entry in agents, failing closed
// when more than one distinct container matches rather than acting on
// whichever entry the runtime happened to list first.
func uniqueAgentEntry(slug string, agents []api.AgentInfo) (api.AgentInfo, error) {
	distinct := dedupeAgentEntries(agents)
	if len(distinct) > 1 {
		return api.AgentInfo{}, fmt.Errorf("agent '%s' is ambiguous: %d containers match", slug, len(distinct))
	}
	return distinct[0], nil
}

// dedupeAgentEntries collapses entries that refer to the same backing
// container (the same container can be reported more than once, e.g. by a
// runtime that is registered both as default and auxiliary).
func dedupeAgentEntries(agents []api.AgentInfo) []api.AgentInfo {
	if len(agents) < 2 {
		return agents
	}
	seen := make(map[string]bool, len(agents))
	out := make([]api.AgentInfo, 0, len(agents))
	for _, a := range agents {
		key := a.ContainerID
		if key == "" {
			key = a.ID
		}
		if key == "" {
			key = "path:" + a.ProjectPath + "|" + a.Name
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, a)
	}
	return out
}

func agentsForProject(agents []api.AgentInfo, projectID string) []api.AgentInfo {
	if projectID == "" {
		return agents
	}
	filtered := make([]api.AgentInfo, 0, len(agents))
	for _, agent := range agents {
		if projectkeys.ProjectIDFromLabels(agent.Labels) == projectID {
			filtered = append(filtered, agent)
		}
	}
	return filtered
}

// RuntimeCommand implements AgentLookup interface.
// It returns the container runtime command (e.g., "docker", "podman", "container").
// The result reflects the currently detected runtime, which may change at any
// time via SwapRuntime, so callers should not cache the return value.
func (s *Server) RuntimeCommand() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.runtime == nil {
		slog.Warn("RuntimeCommand called before runtime detection completed, falling back to docker")
		return "docker"
	}
	return s.runtime.Name()
}

// startCredentialWatcher starts a goroutine that watches for credential file changes.
// When credentials change, it reinitializes hub connections as needed.
func (s *Server) startCredentialWatcher(ctx context.Context) {
	if s.multiCredStore == nil {
		slog.Warn("No multi-credential store configured, skipping watcher")
		return
	}

	s.credWatcherStop = make(chan struct{})
	go s.credentialWatchLoop(ctx)
	slog.Info("Credential watcher started", "interval", "10s")
}

// credentialWatchLoop is the main credential watching loop.
func (s *Server) credentialWatchLoop(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.credWatcherStop:
			return
		case <-ticker.C:
			if err := s.checkAndReloadCredentials(ctx); err != nil {
				slog.Error("Error checking credentials", "error", err)
			}
		}
	}
}

// checkAndReloadCredentials checks if multi-store credentials have changed and reloads if necessary.
func (s *Server) checkAndReloadCredentials(ctx context.Context) error {
	if s.multiCredStore == nil {
		return nil
	}

	creds, scanTime, changed, err := s.multiCredStore.LoadAllIfChanged(s.credLastScan)
	if err != nil {
		return fmt.Errorf("failed to check credentials: %w", err)
	}
	if !changed {
		return nil
	}
	s.credLastScan = scanTime

	slog.Info("Credentials changed, reloading", "count", len(creds))

	// Build name -> creds map from new scan
	newCreds := make(map[string]*brokercredentials.BrokerCredentials)
	for i := range creds {
		newCreds[creds[i].Name] = &creds[i]
	}

	s.hubMu.Lock()

	// Detect removals: connections that exist but are not in newCreds
	// (skip "local" connection which comes from InMemoryCredentials)
	for name, conn := range s.hubConnections {
		if name == "local" && s.config.InMemoryCredentials != nil {
			continue
		}
		if _, exists := newCreds[name]; !exists {
			slog.Info("Removing hub connection", "name", name)
			conn.Stop()
			delete(s.hubConnections, name)
		}
	}

	// Detect additions and modifications
	for name, c := range newCreds {
		existingConn, exists := s.hubConnections[name]
		if !exists {
			// New connection
			conn, err := s.createHubConnection(name, c)
			if err != nil {
				slog.Warn("Failed to create new hub connection", "name", name, "error", err)
				continue
			}
			s.hubConnections[name] = conn
			slog.Info("Added new hub connection", "name", name, "brokerID", c.BrokerID)

			// Start services for the new connection
			go func(conn *HubConnection) {
				if err := conn.Start(ctx, s); err != nil {
					slog.Error("Failed to start new hub connection", "name", conn.Name, "error", err)
				}
			}(conn)
		} else {
			// Check if credentials changed
			if existingConn.Credentials == nil ||
				existingConn.Credentials.BrokerID != c.BrokerID ||
				existingConn.Credentials.SecretKey != c.SecretKey ||
				existingConn.Credentials.HubEndpoint != c.HubEndpoint {

				slog.Info("Reinitializing hub connection", "name", name)
				go func(conn *HubConnection, creds *brokercredentials.BrokerCredentials) {
					if err := conn.Reinitialize(ctx, s, creds); err != nil {
						slog.Error("Failed to reinitialize hub connection", "name", conn.Name, "error", err)
					}
				}(existingConn, c)
			}
		}
	}

	s.hubMu.Unlock()

	// Rebuild auth middleware with updated keys
	s.buildAuthMiddleware()

	return nil
}

// buildProjectFilterForHub builds a project filter function for a specific hub endpoint.
// In multi-hub mode, each heartbeat should only report projects that belong to its hub.
// In single-hub mode or when only one connection exists, no filtering is applied.
func (s *Server) buildProjectFilterForHub(hubEndpoint string) func(string) bool {
	s.hubMu.RLock()
	connCount := len(s.hubConnections)
	s.hubMu.RUnlock()

	// Single-hub mode: no filtering needed
	if connCount <= 1 {
		return nil
	}

	// Multi-hub mode: build a filter from project settings
	// Scan projects and check which ones have their hub.endpoint matching this connection
	return func(projectID string) bool {
		// For now, try to find the project's settings to determine its hub endpoint.
		// This requires the agent manager to provide project paths.
		// As a simple implementation, we scan agents and check their project settings.
		if s.manager == nil {
			return true // Can't filter without a manager
		}

		agents, err := s.manager.List(context.Background(), nil)
		if err != nil {
			return true // Allow on error
		}

		for _, ag := range agents {
			agProjectID := ag.ProjectID
			if agProjectID == "" {
				agProjectID = ag.Project
			}
			if agProjectID != projectID {
				continue
			}

			// Found an agent in this project, check its project path settings
			if ag.ProjectPath == "" {
				continue
			}

			projectSettings, err := config.LoadSettingsFromDir(ag.ProjectPath)
			if err != nil {
				continue
			}

			ep := projectSettings.GetHubEndpoint()
			if ep != "" {
				return ep == hubEndpoint
			}
		}

		// If we can't determine the project's hub, include it (safe default)
		return true
	}
}

// isMultiHubMode returns true if the broker is connected to more than one hub.
func (s *Server) isMultiHubMode() bool {
	s.hubMu.RLock()
	defer s.hubMu.RUnlock()
	return len(s.hubConnections) > 1
}

// isGlobalProject returns true if this is the global project.
// A request with a specific (non-empty, non-"global") ProjectID is never the
// global project, even when projectPath is empty (e.g. git-based projects where the
// broker resolves the workspace from a git remote rather than a local path).
func (s *Server) isGlobalProject(projectID, projectPath string) bool {
	return projectID == "global" || (projectID == "" && projectPath == "")
}

// resolveHubConnection resolves the hub connection for a request, routing to
// the correct connection based on the X-Scion-Hub-Connection header.
func (s *Server) resolveHubConnection(r *http.Request) *HubConnection {
	connName := r.Header.Get("X-Scion-Hub-Connection")
	if connName != "" {
		s.hubMu.RLock()
		conn, ok := s.hubConnections[connName]
		s.hubMu.RUnlock()
		if ok && conn.Hydrator != nil {
			return conn
		}
	}

	// Fallback: return first available connection with a hydrator
	s.hubMu.RLock()
	defer s.hubMu.RUnlock()
	for _, conn := range s.hubConnections {
		if conn.Hydrator != nil {
			return conn
		}
	}
	return nil
}

// resolveHubNameForLaunch picks the hub connection name a launch's reports
// should target, following design t1-async-create-v11.md §3.8.5's routing
// order, stopping at the first that resolves to a connection with a
// HubClient: (1) the X-Scion-Hub-Connection header (set by the control
// channel, controlchannel.go); (2) the hub whose key authenticated the
// request (brokerauth.go's authenticatingHubConnFromContext); (3) the only
// connection. Returns "" when none of these resolve (e.g. more than one
// connection and neither 1 nor 2 identified one) — the sender then fans out
// to every connection with a HubClient (routing rule 4), exactly as
// reportMessageFailure does.
func (s *Server) resolveHubNameForLaunch(r *http.Request) string {
	s.hubMu.RLock()
	defer s.hubMu.RUnlock()

	if connName := r.Header.Get("X-Scion-Hub-Connection"); connName != "" {
		if conn, ok := s.hubConnections[connName]; ok && conn.HubClient != nil {
			return connName
		}
	}
	if connName := authenticatingHubConnFromContext(r.Context()); connName != "" {
		if conn, ok := s.hubConnections[connName]; ok && conn.HubClient != nil {
			return connName
		}
	}
	var only string
	count := 0
	for name, conn := range s.hubConnections {
		if conn.HubClient == nil {
			continue
		}
		only = name
		count++
		if count > 1 {
			return ""
		}
	}
	if count == 1 {
		return only
	}
	return ""
}

// resolveHubEndpointFromRequest returns the hub endpoint for the hub connection
// identified by the X-Scion-Hub-Connection header. This allows the broker to
// use the correct hub endpoint when dispatched by a remote hub, rather than
// falling back to its own config.HubEndpoint (which may point to a different hub).
func (s *Server) resolveHubEndpointFromRequest(r *http.Request) string {
	connName := r.Header.Get("X-Scion-Hub-Connection")
	if connName == "" {
		return ""
	}
	s.hubMu.RLock()
	conn, ok := s.hubConnections[connName]
	s.hubMu.RUnlock()
	if ok {
		return conn.HubEndpoint
	}
	return ""
}

// logHubConnections logs a summary of all active hub connections.
func (s *Server) logHubConnections() {
	s.hubMu.RLock()
	defer s.hubMu.RUnlock()

	count := len(s.hubConnections)
	if count == 0 {
		slog.Info("No hub connections configured")
		return
	}

	for _, conn := range s.hubConnections {
		attrs := []slog.Attr{
			slog.String("name", conn.Name),
			slog.String("endpoint", conn.HubEndpoint),
			slog.String("status", string(conn.GetStatus())),
		}

		if conn.AuthMode != "" {
			attrs = append(attrs, slog.String("auth", string(conn.AuthMode)))
		}

		if conn.IsColocated {
			attrs = append(attrs, slog.Bool("colocated", true))
		}

		hasHeartbeat := conn.Heartbeat != nil
		hasControlChannel := conn.ControlChannel != nil
		attrs = append(attrs,
			slog.Bool("heartbeat", hasHeartbeat),
			slog.Bool("control_channel", hasControlChannel),
		)

		slog.LogAttrs(context.Background(), slog.LevelInfo, "Hub connection active", attrs...)
	}

	mode := "single-hub"
	if count > 1 {
		mode = "multi-hub"
	}
	slog.Info("Hub connections summary", "total", count, "mode", mode)
}

// registerRoutes sets up all API routes.
func (s *Server) registerRoutes() {
	// Health endpoints
	s.mux.HandleFunc("/healthz", s.handleHealthz)
	s.mux.HandleFunc("/readyz", s.handleReadyz)

	// API v1 routes
	s.mux.HandleFunc("/api/v1/info", s.handleInfo)
	s.mux.HandleFunc("/api/v1/hub-connections", s.handleHubConnections)

	// Agent routes
	s.mux.HandleFunc("/api/v1/agents", s.handleAgents)
	s.mux.HandleFunc("/api/v1/agents/", s.handleAgentByID)

	// Project routes
	s.mux.HandleFunc("/api/v1/projects/", s.handleProjectBySlug)

	// Image state endpoints
	s.mux.HandleFunc("/api/v1/images/status", s.handleImageStatus)
	s.mux.HandleFunc("/api/v1/images/pull", s.handleImagePull)
	s.mux.HandleFunc("/api/v1/images/local", s.handleImageDeleteLocal)

	// Workspace sync routes (for Hub-initiated sync via control channel)
	s.mux.HandleFunc("/api/v1/workspace/upload", s.handleWorkspaceUpload)
	s.mux.HandleFunc("/api/v1/workspace/apply", s.handleWorkspaceApply)
	s.mux.HandleFunc("/api/v1/workspace/project-upload", s.handleProjectWorkspaceUpload)
}

// applyMiddleware wraps the handler with middleware.
func (s *Server) applyMiddleware(h http.Handler) http.Handler {
	// Apply middleware in reverse order (last applied runs first)
	h = s.recoveryMiddleware(h)
	if s.requestLogger != nil {
		h = logging.RequestLogMiddleware(s.requestLogger, "broker", logging.BrokerPathPatterns(), s.config.SlowRequestThreshold)(h)
	} else {
		h = s.loggingMiddleware(h)
	}
	if s.config.CORSEnabled {
		h = s.corsMiddleware(h)
	}
	// Apply broker auth middleware if configured
	if s.brokerAuthMiddleware != nil {
		h = s.brokerAuthMiddleware.Middleware(h)
	}

	// OTel HTTP tracing (outermost - wraps all middleware for full request lifecycle)
	h = otelhttp.NewHandler(h, "broker")

	return h
}

// corsMiddleware adds CORS headers.
func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			next.ServeHTTP(w, r)
			return
		}

		// Check if origin is allowed
		allowed := false
		for _, o := range s.config.CORSAllowedOrigins {
			if o == "*" || o == origin {
				allowed = true
				break
			}
		}

		if allowed {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", strings.Join(s.config.CORSAllowedMethods, ", "))
			w.Header().Set("Access-Control-Allow-Headers", strings.Join(s.config.CORSAllowedHeaders, ", "))
			w.Header().Set("Access-Control-Max-Age", fmt.Sprintf("%d", s.config.CORSMaxAge))
		}

		// Handle preflight
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// loggingMiddleware logs requests.
func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		wrapped := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		// Extract contextual metadata for logging.
		traceID := logging.ExtractTraceIDFromHeaders(r)

		attrs := []slog.Attr{
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.String("remote_addr", r.RemoteAddr),
		}
		if traceID != "" {
			attrs = append(attrs, slog.String(logging.AttrTraceID, traceID))
		}

		if s.config.Debug {
			slog.Debug("Incoming request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("remote_addr", r.RemoteAddr),
				slog.String("query", r.URL.RawQuery),
			)
		}

		next.ServeHTTP(wrapped, r)

		duration := time.Since(start)
		level := slog.LevelInfo
		if wrapped.statusCode >= 500 {
			level = slog.LevelError
		} else if wrapped.statusCode >= 400 {
			level = slog.LevelWarn
		}

		slog.LogAttrs(r.Context(), level, "Request completed",
			append(attrs,
				slog.Int("status", wrapped.statusCode),
				slog.Duration("duration", duration),
			)...,
		)
	})
}

// recoveryMiddleware recovers from panics.
func (s *Server) recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				slog.Error("Panic recovered",
					slog.Any("error", err),
					slog.String("path", r.URL.Path),
				)
				InternalError(w)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// responseWriter wraps http.ResponseWriter to capture status code.
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

// Helper functions

// writeJSON writes a JSON response.
func writeJSON(w http.ResponseWriter, statusCode int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(data)
}

// readJSON reads JSON from request body.
func readJSON(r *http.Request, v interface{}) error {
	if r.Body == nil {
		return fmt.Errorf("empty request body")
	}
	return json.NewDecoder(r.Body).Decode(v)
}

// extractID extracts the ID from a URL path like "/api/v1/agents/{id}".
func extractID(r *http.Request, prefix string) string {
	path := strings.TrimPrefix(r.URL.Path, prefix)
	path = strings.TrimPrefix(path, "/")
	// Remove any trailing path segments
	if idx := strings.Index(path, "/"); idx != -1 {
		path = path[:idx]
	}
	return path
}

// extractAction extracts the action from a URL path like "/api/v1/agents/{id}/start".
func extractAction(r *http.Request, prefix string) (id, action string) {
	path := strings.TrimPrefix(r.URL.Path, prefix)
	path = strings.TrimPrefix(path, "/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) == 0 {
		return "", ""
	}
	id = parts[0]
	if len(parts) > 1 {
		action = parts[1]
	}
	return
}

// IsControlChannelConnected reports whether the broker has at least one live
// control-channel WebSocket. Returns true when no control channel is configured
// (e.g. Cloud Run stateless brokers) so callers can treat "no channel" as healthy.
func (s *Server) IsControlChannelConnected() bool {
	s.hubMu.RLock()
	defer s.hubMu.RUnlock()

	if len(s.hubConnections) == 0 {
		return !s.config.ControlChannelEnabled
	}

	for _, conn := range s.hubConnections {
		conn.mu.RLock()
		cc := conn.ControlChannel
		conn.mu.RUnlock()
		if cc != nil && cc.IsConnected() {
			return true
		}
	}
	return !s.config.ControlChannelEnabled
}
