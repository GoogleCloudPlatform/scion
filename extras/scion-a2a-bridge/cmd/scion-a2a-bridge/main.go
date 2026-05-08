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
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	smpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/prometheus/client_golang/prometheus"
	"gopkg.in/yaml.v3"

	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/bridge"
	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/identity"
	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/state"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

func main() {
	configPath := flag.String("config", "scion-a2a-bridge.yaml", "Path to configuration file")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	// Validate auth configuration at startup (fail closed).
	if err := bridge.ValidateConfig(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "invalid configuration: %v\n", err)
		os.Exit(1)
	}

	log := initLogger(cfg.Logging)
	log.Info("scion-a2a-bridge starting")

	// Initialize SQLite state database.
	dbPath := cfg.State.Database
	if dbPath == "" {
		dbPath = "scion-a2a-bridge.db"
	}
	store, err := state.New(dbPath)
	if err != nil {
		log.Error("failed to initialize state database", "error", err)
		os.Exit(1)
	}
	defer store.Close()
	log.Info("state database initialized", "path", dbPath)

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

	// Start A2A HTTP server.
	listenAddr := cfg.Bridge.ListenAddress
	if listenAddr == "" {
		listenAddr = ":8443"
	}

	srv := bridge.NewServer(b, cfg, metrics, log.With("component", "a2a-server"))
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

	log.Info("scion-a2a-bridge ready")

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

func initLogger(cfg bridge.LoggingConfig) *slog.Logger {
	level := slog.LevelInfo
	switch cfg.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	var handler slog.Handler
	if cfg.Format == "json" {
		handler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	} else {
		handler = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	}
	logger := slog.New(handler)
	slog.SetDefault(logger)
	return logger
}
