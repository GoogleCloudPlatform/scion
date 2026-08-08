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

package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	smpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"gopkg.in/yaml.v3"

	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/bridge"
	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/identity"
	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/state"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/integration/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin/grpcbroker"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	brokerv1 "github.com/GoogleCloudPlatform/scion/proto/broker/v1"
)

func main() {
	standalone := flag.Bool("standalone", false, "Run in standalone mode with Postgres store and gRPC broker")
	configPath := flag.String("config", "scion-a2a-bridge.yaml", "Path to configuration file")
	flag.Parse()

	// Check environment variable as alternative to --standalone flag.
	if os.Getenv("A2A_STANDALONE") == "true" {
		*standalone = true
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	// Initialize structured logging before validation so config errors are logged.
	useGCP := os.Getenv("SCION_LOG_GCP") == "true" || os.Getenv("K_SERVICE") != ""
	debug := cfg.Logging.Level == "debug"
	logging.Setup("scion-a2a-bridge", debug, useGCP)
	log := slog.Default()

	if *standalone {
		serveStandalone(cfg, log)
		return
	}

	// Validate auth configuration at startup (fail closed).
	if err := bridge.ValidateConfig(cfg); err != nil {
		log.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	log.Info("scion-a2a-bridge starting")

	// Initialize SQLite state database.
	dbPath := cfg.State.Database
	if dbPath == "" {
		dbPath = "scion-a2a-bridge.db"
	}
	store, err := state.NewSQLite(dbPath)
	if err != nil {
		log.Error("failed to initialize state database", "error", err)
		os.Exit(1)
	}
	defer store.Close()
	log.Info("state database initialized", "path", dbPath)

	// Determine state directory for admin overlay persistence.
	stateDir := filepath.Dir(dbPath)

	// Load and apply any persisted admin overlay (from a previous Configure push).
	// This ensures the bridge boots with the last-known admin config, not just
	// the base YAML, bridging the gap until the Hub's first Configure() push.
	baseCfg := *cfg // snapshot of the base YAML config (immutable reference)
	overlay, overlayErr := bridge.LoadPersistedOverlay(stateDir)
	if overlayErr != nil {
		log.Warn("failed to load persisted admin overlay, proceeding with base YAML config", "error", overlayErr)
	}
	if overlay != nil {
		effective := bridge.ApplyOverlay(baseCfg, overlay)
		cfg = &effective
		log.Info("applied persisted admin overlay",
			"auth_scheme", cfg.Auth.Scheme,
			"external_url", cfg.Bridge.ExternalURL,
			"projects", len(cfg.Projects),
		)
	}

	// Build the initial config snapshot (base YAML + persisted overlay).
	snapshot := bridge.NewSnapshotHolder(bridge.BuildSnapshot(*cfg))

	// Load hub signing key.
	signingKeyB64, err := loadSigningKey(cfg.Hub)
	if err != nil {
		log.Error("failed to load signing key", "error", err)
		os.Exit(1)
	}
	signingKey, err := base64.StdEncoding.DecodeString(signingKeyB64)
	if err != nil {
		log.Error("failed to decode hub signing key (expected base64)", "error", err)
		os.Exit(1)
	}

	keyHash := sha256.Sum256(signingKey)
	log.Info("signing key loaded",
		"key_len", len(signingKey),
		"key_sha256", hex.EncodeToString(keyHash[:8]),
	)

	minter, err := identity.NewTokenMinter(signingKey)
	if err != nil {
		log.Error("failed to create token minter", "error", err)
		os.Exit(1)
	}

	hubUserID := cfg.Hub.UserID
	if hubUserID == "" {
		hubUserID = cfg.Hub.User
	}
	adminAuth := identity.NewMintingAuth(minter, hubUserID, cfg.Hub.User, "admin", 15*time.Minute)

	adminClient, err := hubclient.New(cfg.Hub.Endpoint, hubclient.WithAuthenticator(adminAuth))
	if err != nil {
		log.Error("failed to create hub client", "error", err)
		os.Exit(1)
	}
	log.Info("hub client initialized", "endpoint", cfg.Hub.Endpoint, "admin_user", cfg.Hub.User)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	metrics := bridge.NewMetrics(prometheus.DefaultRegisterer)

	// Create core bridge.
	b := bridge.New(store, adminClient, minter, cfg, metrics, log.With("component", "bridge"))

	// Create broker server and wire the bridge as handler.
	broker := bridge.NewBrokerServer(b.HandleBrokerMessage, log.With("component", "broker"), ctx)

	// Start broker plugin RPC server.
	pluginAddr := cfg.Plugin.ListenAddress
	if pluginAddr == "" {
		pluginAddr = "localhost:9090"
	}
	pluginServer, err := broker.Serve(pluginAddr, cfg.Plugin.AllowRemote)
	if err != nil {
		log.Error("failed to start broker plugin server", "error", err)
		os.Exit(1)
	}
	defer pluginServer.Close()
	log.Info("broker plugin RPC server started", "address", pluginServer.Addr())

	// Wire broker into the bridge for subscription management.
	b.SetBroker(broker)

	// Wire admin config management: snapshot + base config + state dir.
	b.SetSnapshot(snapshot)
	broker.SetAdminConfig(&baseCfg, snapshot, stateDir)

	// Create SDK executor and request handler.
	// Use a route-key authenticator so the in-memory task store associates tasks
	// with the correct project/agent pair, and a ScopedTaskStore wrapper that
	// enforces ownership on Get/Update to prevent cross-tenant access.
	executor := bridge.NewScionExecutor(b, log.With("component", "executor"))
	routeAuthenticator := bridge.RouteKeyAuthenticator()
	innerTaskStore := taskstore.NewInMemory(&taskstore.InMemoryStoreConfig{
		Authenticator: routeAuthenticator,
	})
	scopedTaskStore := bridge.NewScopedTaskStore(innerTaskStore)
	sdkRequestHandler := a2asrv.NewHandler(
		executor,
		a2asrv.WithLogger(log.With("component", "a2a-sdk")),
		a2asrv.WithCapabilityChecks(&a2a.AgentCapabilities{
			Streaming:         true,
			PushNotifications: false,
		}),
		a2asrv.WithAgentInactivityTimeout(cfg.Timeouts.SendMessage),
		a2asrv.WithTaskStore(scopedTaskStore),
	)
	b.SetSDKRequestHandler(sdkRequestHandler)

	// Create SDK JSON-RPC transport handler.
	sdkJSONRPCHandler := a2asrv.NewJSONRPCHandler(
		sdkRequestHandler,
		a2asrv.WithTransportKeepAlive(cfg.Timeouts.SSEKeepalive),
	)

	// Start A2A HTTP server.
	listenAddr := cfg.Bridge.ListenAddress
	if listenAddr == "" {
		listenAddr = ":8443"
	}

	srv := bridge.NewServer(b, cfg, metrics, log.With("component", "a2a-server"), sdkJSONRPCHandler)
	srv.SetSnapshot(snapshot)
	if signingKey != nil {
		srv.SetJWTValidator(bridge.NewJWTValidator(signingKey))
	}
	srv.WarnOnOpenAuth()

	httpServer := &http.Server{
		Addr:           listenAddr,
		Handler:        srv.Handler(),
		ReadTimeout:    30 * time.Second,
		WriteTimeout:   30 * time.Second,
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Warn("A2A server starting WITHOUT TLS — ensure TLS is terminated at a reverse proxy (e.g. Caddy, nginx, cloud LB)", "address", listenAddr)
		log.Info("A2A protocol server starting", "address", listenAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- fmt.Errorf("a2a server: %w", err)
		}
	}()

	log.Info("scion-a2a-bridge ready",
		"transport", "JSON-RPC",
		"sdk", "a2a-go/v2",
	)

	// Wait for shutdown signal.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	select {
	case sig := <-sigCh:
		log.Info("received signal, shutting down", "signal", sig)
	case err := <-errCh:
		log.Error("server error", "error", err)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(ctx, 30*time.Second)
	defer shutdownCancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Error("failed to stop A2A server", "error", err)
	}

	// Drain background goroutines before closing the store.
	b.Shutdown()

	log.Info("scion-a2a-bridge stopped")
}

// serveStandalone runs the A2A bridge in standalone mode using a Postgres
// store and the integration runtime for config management. This mode is fully
// leaderless — no advisory locks, no lock loops, no leader election.
func serveStandalone(cfg *bridge.Config, log *slog.Logger) {
	log.Info("scion-a2a-bridge starting in standalone mode")

	// Detect Cloud Run or explicit port-muxing mode (single-port h2c).
	muxPorts := os.Getenv("MUX_PORTS") == "true" || os.Getenv("K_SERVICE") != ""

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// 1. Require DATABASE_URL for Postgres store.
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Error("DATABASE_URL is required in standalone mode")
		os.Exit(1)
	}

	store, err := state.NewPostgres(databaseURL)
	if err != nil {
		log.Error("failed to initialize Postgres store", "error", err)
		os.Exit(1)
	}
	defer store.Close()
	log.Info("Postgres store initialized")

	// 2. Set up gRPC broker server + health service early so health probes
	// work during the runtime's DB-connect retry window.
	brokerServer := bridge.NewBrokerServer(nil, log.With("component", "broker"), ctx)
	grpcBrokerServer := grpcbroker.NewServer(brokerServer)

	grpcServer := grpc.NewServer()
	brokerv1.RegisterBrokerServiceServer(grpcServer, grpcBrokerServer)
	healthServer := health.NewServer()
	healthpb.RegisterHealthServer(grpcServer, healthServer)
	healthServer.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)

	// When port-muxing is active, gRPC is served through the HTTP handler via
	// h2c — no dedicated gRPC listener is needed.
	var grpcPort int
	if !muxPorts {
		grpcPort = 50051
		if p := os.Getenv("GRPC_PORT"); p != "" {
			if parsed, err := strconv.Atoi(p); err == nil {
				grpcPort = parsed
			}
		}

		lis, err := net.Listen("tcp", fmt.Sprintf(":%d", grpcPort))
		if err != nil {
			log.Error("failed to listen for gRPC", "error", err, "port", grpcPort)
			os.Exit(1)
		}

		go func() {
			log.Info("gRPC server listening", "port", grpcPort)
			if err := grpcServer.Serve(lis); err != nil {
				log.Error("gRPC server error", "error", err)
			}
		}()
	}

	// 3. Start the integration runtime for config management.
	rt := runtime.New(runtime.Options{
		Integration: "a2a-bridge",
		DatabaseURL: databaseURL,
		EnvPrefix:   "A2A",
		EnvKeys: []string{
			"external_url", "auth_scheme", "uat_cache_ttl",
			"rate_limit_enabled", "rate_limit_rps", "rate_limit_burst",
			"send_message_timeout", "sse_keepalive", "push_retry_max",
			"provider_org", "provider_url",
		},
		UpdateHook: os.Getenv("UPDATE_HOOK"),
		Log:        log,
	})

	rctx, err := rt.Start(ctx)
	if err != nil {
		log.Error("failed to start integration runtime", "error", err)
		os.Exit(1)
	}
	defer rt.Stop()

	// 4. Apply runtime config onto the base YAML config.
	rtConfig := rt.Config()
	applyRuntimeConfig(cfg, rtConfig)

	// 5. Read A2A_API_KEY from environment (secret, never through runtime config path).
	if apiKey := os.Getenv("A2A_API_KEY"); apiKey != "" {
		cfg.Auth.APIKey = apiKey
	}

	// Validate configuration after applying runtime overrides.
	if err := bridge.ValidateConfig(cfg); err != nil {
		log.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	// 6. Load hub signing key and create hub client.
	signingKeyB64, err := loadSigningKey(cfg.Hub)
	if err != nil {
		log.Error("failed to load signing key", "error", err)
		os.Exit(1)
	}
	signingKey, err := base64.StdEncoding.DecodeString(signingKeyB64)
	if err != nil {
		log.Error("failed to decode hub signing key (expected base64)", "error", err)
		os.Exit(1)
	}

	keyHash := sha256.Sum256(signingKey)
	log.Info("signing key loaded",
		"key_len", len(signingKey),
		"key_sha256", hex.EncodeToString(keyHash[:8]),
	)

	minter, err := identity.NewTokenMinter(signingKey)
	if err != nil {
		log.Error("failed to create token minter", "error", err)
		os.Exit(1)
	}

	hubUserID := cfg.Hub.UserID
	if hubUserID == "" {
		hubUserID = cfg.Hub.User
	}
	adminAuth := identity.NewMintingAuth(minter, hubUserID, cfg.Hub.User, "admin", 15*time.Minute)

	hubClient, err := hubclient.New(cfg.Hub.Endpoint, hubclient.WithAuthenticator(adminAuth))
	if err != nil {
		log.Error("failed to create hub client", "error", err)
		os.Exit(1)
	}
	log.Info("hub client initialized", "endpoint", cfg.Hub.Endpoint, "admin_user", cfg.Hub.User)

	metrics := bridge.NewMetrics(prometheus.DefaultRegisterer)

	// 7. Build config snapshot (base YAML + runtime overrides).
	baseCfg := *cfg
	snapshot := bridge.NewSnapshotHolder(bridge.BuildSnapshot(*cfg))

	// 8. Create core bridge.
	b := bridge.New(store, hubClient, minter, cfg, metrics, log.With("component", "bridge"))

	// Create and wire the NOTIFY accelerator for reduced latency on
	// blocking SendMessage and SSE streaming. Purely additive — polling
	// remains the correctness floor (design §5.2, D7).
	notifier := bridge.NewNotifier(databaseURL, log.With("component", "notifier"))
	b.SetNotifier(notifier)

	// Wire broker handler and admin config into the broker server.
	brokerServer.SetHandler(b.HandleBrokerMessage)
	b.SetBroker(brokerServer)
	b.SetSnapshot(snapshot)
	brokerServer.SetAdminConfig(&baseCfg, snapshot, "")

	// 9. Set up reconfigure callback for runtime config changes.
	rt.SetReconfigure(func(newCfg map[string]string) error {
		applyRuntimeConfig(cfg, newCfg)
		// Re-read A2A_API_KEY on reconfigure (may have been rotated).
		if apiKey := os.Getenv("A2A_API_KEY"); apiKey != "" {
			cfg.Auth.APIKey = apiKey
		}
		snap := bridge.BuildSnapshot(*cfg)
		snapshot.Store(snap)
		return nil
	})

	// 10. Create SDK executor and request handler.
	executor := bridge.NewScionExecutor(b, log.With("component", "executor"))
	routeAuthenticator := bridge.RouteKeyAuthenticator()
	innerTaskStore := taskstore.NewInMemory(&taskstore.InMemoryStoreConfig{
		Authenticator: routeAuthenticator,
	})
	scopedTaskStore := bridge.NewScopedTaskStore(innerTaskStore)
	sdkRequestHandler := a2asrv.NewHandler(
		executor,
		a2asrv.WithLogger(log.With("component", "a2a-sdk")),
		a2asrv.WithCapabilityChecks(&a2a.AgentCapabilities{
			Streaming:         true,
			PushNotifications: false,
		}),
		a2asrv.WithAgentInactivityTimeout(cfg.Timeouts.SendMessage),
		a2asrv.WithTaskStore(scopedTaskStore),
	)
	b.SetSDKRequestHandler(sdkRequestHandler)

	sdkJSONRPCHandler := a2asrv.NewJSONRPCHandler(
		sdkRequestHandler,
		a2asrv.WithTransportKeepAlive(cfg.Timeouts.SSEKeepalive),
	)

	// 11. Start A2A HTTP server.
	listenAddr := cfg.Bridge.ListenAddress
	if listenAddr == "" {
		listenAddr = ":8443"
	}

	srv := bridge.NewServer(b, cfg, metrics, log.With("component", "a2a-server"), sdkJSONRPCHandler)
	srv.SetSnapshot(snapshot)
	if signingKey != nil {
		srv.SetJWTValidator(bridge.NewJWTValidator(signingKey))
	}
	srv.WarnOnOpenAuth()

	httpHandler := srv.Handler()
	if muxPorts {
		// Override listen address with PORT env var (Cloud Run convention).
		if port := os.Getenv("PORT"); port != "" {
			listenAddr = ":" + port
		}
		// Wrap with h2c to multiplex HTTP/1.1 and gRPC (HTTP/2) on the same port.
		a2aHandler := httpHandler
		httpHandler = h2c.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
				grpcServer.ServeHTTP(w, r)
			} else {
				a2aHandler.ServeHTTP(w, r)
			}
		}), &http2.Server{})
	}

	httpServer := &http.Server{
		Addr:           listenAddr,
		Handler:        httpHandler,
		ReadTimeout:    30 * time.Second,
		WriteTimeout:   30 * time.Second,
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("A2A protocol server starting", "address", listenAddr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- fmt.Errorf("a2a server: %w", err)
		}
	}()

	// Mark healthy now that all services are up.
	healthServer.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)

	log.Info("scion-a2a-bridge ready (standalone)",
		"transport", "JSON-RPC",
		"sdk", "a2a-go/v2",
		"grpc_port", grpcPort,
		"http_addr", listenAddr,
		"mux_ports", muxPorts,
	)

	// 12. Block until signal, server error, or update-triggered shutdown.
	select {
	case <-rctx.Done():
	case updateID := <-rt.ShutdownRequested():
		log.Info("update-triggered shutdown", "update_id", updateID)
		stop()
	case err := <-errCh:
		log.Error("server error", "error", err)
		stop()
	}

	// 13. Graceful shutdown.
	healthServer.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Error("failed to stop A2A server", "error", err)
	}

	if !muxPorts {
		grpcDone := make(chan struct{})
		go func() {
			grpcServer.GracefulStop()
			close(grpcDone)
		}()
		select {
		case <-grpcDone:
		case <-time.After(5 * time.Second):
			grpcServer.Stop()
		}
	}

	notifier.Stop()
	b.Shutdown()
	log.Info("scion-a2a-bridge stopped (standalone)")
}

// applyRuntimeConfig merges runtime config values into the bridge config.
// Only non-empty values override existing config.
func applyRuntimeConfig(cfg *bridge.Config, rtCfg map[string]string) {
	if v := rtCfg["external_url"]; v != "" {
		cfg.Bridge.ExternalURL = v
	}
	if v := rtCfg["auth_scheme"]; v != "" {
		cfg.Auth.Scheme = v
	}
	if v := rtCfg["uat_cache_ttl"]; v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Auth.UATCacheTTL = d
		}
	}
	if v := rtCfg["rate_limit_enabled"]; v != "" {
		cfg.RateLimit.Enabled = v == "true" || v == "1"
	}
	if v := rtCfg["rate_limit_rps"]; v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.RateLimit.RequestsPerSec = f
		}
	}
	if v := rtCfg["rate_limit_burst"]; v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.RateLimit.Burst = n
		}
	}
	if v := rtCfg["send_message_timeout"]; v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Timeouts.SendMessage = d
		}
	}
	if v := rtCfg["sse_keepalive"]; v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			cfg.Timeouts.SSEKeepalive = d
		}
	}
	if v := rtCfg["push_retry_max"]; v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Timeouts.PushRetryMax = n
		}
	}
	if v := rtCfg["provider_org"]; v != "" {
		cfg.Bridge.Provider.Organization = v
	}
	if v := rtCfg["provider_url"]; v != "" {
		cfg.Bridge.Provider.URL = v
	}
}

func loadConfig(path string) (*bridge.Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	// NOTE: os.Expand has no escape mechanism — a literal "$" in config values
	// will be interpreted as the start of an environment variable reference.
	var missing []string
	expanded := os.Expand(string(data), func(name string) string {
		v, ok := os.LookupEnv(name)
		if !ok && name == "SCION_PROJECT_ID" {
			v, ok = os.LookupEnv("SCION_GROVE_ID")
		}
		if !ok {
			missing = append(missing, name)
		}
		return v
	})
	if len(missing) > 0 {
		return nil, fmt.Errorf("config references unset environment variables: %v", missing)
	}

	var cfg bridge.Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	// Backward compatibility: merge legacy 'groves' into 'projects' if 'projects' is empty.
	if len(cfg.Projects) == 0 && len(cfg.Groves) > 0 {
		cfg.Projects = cfg.Groves
	}

	if cfg.Timeouts.SendMessage == 0 {
		cfg.Timeouts.SendMessage = 120 * time.Second
	}
	if cfg.Timeouts.SSEKeepalive == 0 {
		cfg.Timeouts.SSEKeepalive = 30 * time.Second
	}
	if cfg.Timeouts.PushRetryMax == 0 {
		cfg.Timeouts.PushRetryMax = 3
	}

	return &cfg, nil
}

var b64Cleaner = strings.NewReplacer(" ", "", "\t", "", "\n", "", "\r", "", "\xef\xbb\xbf", "")

func cleanBase64(raw string) (string, error) {
	cleaned := b64Cleaner.Replace(raw)
	for i := 0; i < len(cleaned); i++ {
		if cleaned[i] > 127 {
			return "", fmt.Errorf("signing key contains non-ASCII byte at position %d (possible UTF-16 or BOM encoding)", i)
		}
	}
	return cleaned, nil
}

func loadSigningKey(cfg bridge.HubConfig) (string, error) {
	switch {
	case cfg.SigningKey != "":
		data, err := os.ReadFile(cfg.SigningKey)
		if err != nil {
			return "", fmt.Errorf("reading signing key file: %w", err)
		}
		return cleanBase64(string(data))
	case cfg.SigningKeySecret != "":
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return accessSecret(ctx, cfg.SigningKeySecret)
	default:
		return "", fmt.Errorf("hub.signing_key or hub.signing_key_secret is required")
	}
}

func accessSecret(ctx context.Context, resourceName string) (string, error) {
	client, err := secretmanager.NewClient(ctx)
	if err != nil {
		return "", fmt.Errorf("creating secret manager client: %w", err)
	}
	defer client.Close()

	if !strings.Contains(resourceName, "/versions/") {
		resourceName += "/versions/latest"
	}
	resp, err := client.AccessSecretVersion(ctx, &smpb.AccessSecretVersionRequest{
		Name: resourceName,
	})
	if err != nil {
		return "", fmt.Errorf("accessing secret version: %w", err)
	}
	return cleanBase64(string(resp.Payload.Data))
}
