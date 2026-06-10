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
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	smpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/api/iterator"
	"gopkg.in/yaml.v3"

	"github.com/GoogleCloudPlatform/scion/extras/scion-chat-app/internal/chatapp"
	"github.com/GoogleCloudPlatform/scion/extras/scion-chat-app/internal/googlechat"
	"github.com/GoogleCloudPlatform/scion/extras/scion-chat-app/internal/identity"
	"github.com/GoogleCloudPlatform/scion/extras/scion-chat-app/internal/state"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

func main() {
	configPath := flag.String("config", "scion-chat-app.yaml", "Path to configuration file")
	flag.Parse()

	// Load configuration.
	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	// Initialize structured logging.
	log := initLogger(cfg.Logging)
	log.Info("scion-chat-app starting")

	// Initialize SQLite state database.
	dbPath := cfg.State.Database
	if dbPath == "" {
		dbPath = "scion-chat-app.db"
	}
	store, err := state.New(dbPath)
	if err != nil {
		log.Error("failed to initialize state database", "error", err)
		os.Exit(1)
	}
	defer store.Close()
	log.Info("state database initialized", "path", dbPath)

	// Load hub signing key: local file → explicit SM secret → auto-discover from SM.
	var signingKeyB64 string
	switch {
	case cfg.Hub.SigningKey != "":
		data, err := os.ReadFile(cfg.Hub.SigningKey)
		if err != nil {
			log.Error("failed to read hub signing key", "path", cfg.Hub.SigningKey, "error", err)
			os.Exit(1)
		}
		signingKeyB64 = strings.TrimSpace(string(data))
	case cfg.Hub.SigningKeySecret != "":
		smCtx, smCancel := context.WithTimeout(context.Background(), 30*time.Second)
		val, err := accessSecret(smCtx, cfg.Hub.SigningKeySecret)
		smCancel()
		if err != nil {
			log.Error("failed to fetch signing key from Secret Manager", "secret", cfg.Hub.SigningKeySecret, "error", err)
			os.Exit(1)
		}
		signingKeyB64 = strings.TrimSpace(val)
		log.Info("loaded signing key from Secret Manager", "secret", cfg.Hub.SigningKeySecret)
	default:
		// Auto-discover the signing key from GCP Secret Manager by label.
		// Prefer the hub's explicit project, then the GCE metadata project
		// (which matches the VM where IAM permissions are granted), then
		// fall back to the Google Chat project.
		projectID := cfg.Hub.Project
		if projectID == "" {
			projectID = gceProjectID()
		}
		if projectID == "" {
			projectID = cfg.Platforms.GoogleChat.ProjectID
		}
		if projectID == "" {
			log.Error("cannot auto-discover signing key: set hub.project, hub.signing_key, or hub.signing_key_secret in the config")
			os.Exit(1)
		}
		log.Info("no signing key configured, searching Secret Manager by label", "project_id", projectID)
		smCtx, smCancel := context.WithTimeout(context.Background(), 30*time.Second)
		val, secretName, err := discoverSigningKey(smCtx, projectID)
		smCancel()
		if err != nil {
			log.Error("failed to auto-discover signing key from Secret Manager",
				"project_id", projectID,
				"error", err,
				"hint", "set hub.signing_key (local file) or hub.signing_key_secret (explicit SM resource) to bypass auto-discovery, or grant the VM service account roles/secretmanager.secretAccessor on the project",
			)
			os.Exit(1)
		}
		signingKeyB64 = strings.TrimSpace(val)
		log.Info("auto-discovered signing key from Secret Manager", "secret", secretName)
	}
	signingKey, err := base64.StdEncoding.DecodeString(signingKeyB64)
	if err != nil {
		log.Error("failed to decode hub signing key (expected base64)", "error", err)
		os.Exit(1)
	}

	// Log key fingerprint for debugging key mismatch issues.
	keyHash := sha256.Sum256(signingKey)
	log.Info("signing key loaded",
		"key_len", len(signingKey),
		"key_sha256", hex.EncodeToString(keyHash[:8]),
		"b64_len", len(signingKeyB64),
	)

	minter, err := identity.NewTokenMinter(signingKey)
	if err != nil {
		log.Error("failed to create token minter", "error", err)
		os.Exit(1)
	}

	// Create auto-refreshing admin auth for the configured hub user.
	if cfg.Hub.User == "" {
		log.Error("hub user is required")
		os.Exit(1)
	}
	adminAuth := identity.NewMintingAuth(minter, cfg.Hub.User, cfg.Hub.User, "admin", 15*time.Minute)

	adminClient, err := hubclient.New(cfg.Hub.Endpoint, hubclient.WithAuthenticator(adminAuth))
	if err != nil {
		log.Error("failed to create hub client", "error", err)
		os.Exit(1)
	}
	log.Info("hub client initialized", "endpoint", cfg.Hub.Endpoint, "admin_user", cfg.Hub.User)

	// Verify hub connectivity by minting a token and listing projects.
	verifyHubConnectivity(context.Background(), log, minter, signingKey, cfg.Hub.User, cfg.Hub.Endpoint, adminClient)

	// Create identity mapper.
	idMapper := identity.NewMapper(store, adminClient, cfg.Hub.Endpoint, minter, log.With("component", "identity"))

	// Create broker server with a nil handler; wired to the notification relay below.
	broker := chatapp.NewBrokerServer(nil, log.With("component", "broker"))

	// Start broker plugin RPC server.
	pluginAddr := cfg.Plugin.ListenAddress
	if pluginAddr == "" {
		pluginAddr = "localhost:9090"
	}
	pluginServer, err := broker.Serve(pluginAddr)
	if err != nil {
		log.Error("failed to start broker plugin server", "error", err)
		os.Exit(1)
	}
	defer pluginServer.Close()
	log.Info("broker plugin RPC server started", "address", pluginServer.Addr())

	// Create a root context for the application lifetime.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create command router with a nil messenger; set after adapter creation.
	cmdRouter := chatapp.NewCommandRouter(
		adminClient,
		cfg.Hub.Endpoint,
		store,
		idMapper,
		nil, // messenger wired below
		broker,
		log.With("component", "commands"),
	)

	// Initialize the platform messenger (Google Chat adapter for now).
	var messenger chatapp.Messenger

	if cfg.Platforms.GoogleChat.Enabled {
		// Preflight: verify Chat API credentials before starting.
		gcLog := log.With("component", "googlechat")
		if err := googlechat.PreflightAuth(ctx, cfg.Platforms.GoogleChat.Credentials, cfg.Platforms.GoogleChat.ProjectID, gcLog); err != nil {
			log.Error("google chat credential preflight failed", "error", err)
			os.Exit(1)
		}

		// Create an authenticated HTTP client (SA key file or ADC).
		chatClient, err := googlechat.NewAuthenticatedClient(ctx, cfg.Platforms.GoogleChat.Credentials, gcLog)
		if err != nil {
			log.Error("failed to create authenticated Chat API client", "error", err)
			os.Exit(1)
		}

		gcAdapter := googlechat.NewAdapter(
			googlechat.Config{
				ProjectID:           cfg.Platforms.GoogleChat.ProjectID,
				ExternalURL:         cfg.Platforms.GoogleChat.ExternalURL,
				ServiceAccountEmail: cfg.Platforms.GoogleChat.ServiceAccountEmail,
				CommandIDMap:        cfg.Platforms.GoogleChat.CommandIDMap,
				ListenAddress:       cfg.Platforms.GoogleChat.ListenAddress,
				Credentials:         cfg.Platforms.GoogleChat.Credentials,
			},
			cmdRouter.HandleEvent,
			chatClient,
			gcLog,
		)
		messenger = gcAdapter
		log.Info("google chat adapter initialized",
			"project_id", cfg.Platforms.GoogleChat.ProjectID,
			"external_url", cfg.Platforms.GoogleChat.ExternalURL,
		)
	}

	// Wire the messenger into the command router now that it exists.
	cmdRouter.SetMessenger(messenger)

	// Create notification relay and wire it as the broker's message handler.
	relay := chatapp.NewNotificationRelay(store, messenger, log.With("component", "notifications"))
	broker.SetHandler(relay.HandleBrokerMessage)

	// Load existing space-project links and request broker subscriptions.
	links, err := store.ListSpaceLinks()
	if err != nil {
		log.Error("failed to load space links", "error", err)
	} else {
		for _, link := range links {
			// Subscribe only to user-targeted messages so that agent-to-agent
			// traffic and broadcasts do not leak into chat.
			pattern := fmt.Sprintf("scion.grove.%s.user.>", link.ProjectID)
			if err := broker.RequestSubscription(pattern); err != nil {
				log.Warn("failed to request subscription for project",
					"project_id", link.ProjectID,
					"error", err,
				)
			}
		}
		log.Info("loaded existing space-project links", "count", len(links))
	}

	// Start platform servers.
	errCh := make(chan error, 1)

	if cfg.Platforms.GoogleChat.Enabled && messenger != nil {
		gcAdapter := messenger.(*googlechat.Adapter)
		listenAddr := cfg.Platforms.GoogleChat.ListenAddress
		if listenAddr == "" {
			listenAddr = ":8443"
		}
		go func() {
			if err := gcAdapter.Start(listenAddr); err != nil {
				errCh <- fmt.Errorf("google chat server: %w", err)
			}
		}()
		log.Info("google chat webhook server starting", "address", listenAddr)
	}

	log.Info("scion-chat-app ready")

	// Wait for shutdown signal.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	select {
	case sig := <-sigCh:
		log.Info("received signal, shutting down", "signal", sig)
	case err := <-errCh:
		log.Error("server error", "error", err)
	}

	// Graceful shutdown with timeout.
	shutdownCtx, shutdownCancel := context.WithTimeout(ctx, 30*time.Second)
	defer shutdownCancel()

	if cfg.Platforms.GoogleChat.Enabled && messenger != nil {
		gcAdapter := messenger.(*googlechat.Adapter)
		if err := gcAdapter.Stop(shutdownCtx); err != nil {
			log.Error("failed to stop google chat adapter", "error", err)
		}
	}

	log.Info("scion-chat-app stopped")
}

// verifyHubConnectivity performs a startup connectivity check against the hub.
// It mints a fresh admin JWT, logs the token details for debugging, and
// attempts to list projects. This catches signing key mismatches early, before
// any chat-event-driven flow exercises the auth path.
func verifyHubConnectivity(ctx context.Context, log *slog.Logger, minter *identity.TokenMinter, signingKey []byte, hubUser, hubEndpoint string, adminClient hubclient.Client) {
	log = log.With("component", "hub-verify")
	log.Info("=== hub connectivity check: START ===")

	// Step 1: Mint a token manually so we can inspect it.
	token, err := minter.MintToken(hubUser, hubUser, "admin", 15*time.Minute)
	if err != nil {
		log.Error("hub-verify: failed to mint token", "error", err)
		return
	}
	log.Info("hub-verify: minted admin token",
		"token_length", len(token),
		"token_prefix", token[:min(40, len(token))]+"...",
	)

	// Step 2: Decode the JWT payload (without verification) to log claims.
	parts := strings.SplitN(token, ".", 3)
	if len(parts) == 3 {
		// JWT header
		if hdrJSON, err := base64.RawURLEncoding.DecodeString(parts[0]); err == nil {
			log.Info("hub-verify: JWT header", "header", string(hdrJSON))
		}
		// JWT payload
		if payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1]); err == nil {
			log.Info("hub-verify: JWT payload (claims)", "claims", string(payloadJSON))
			// Pretty-parse to log individual fields
			var claims map[string]interface{}
			if json.Unmarshal(payloadJSON, &claims) == nil {
				for k, v := range claims {
					log.Debug("hub-verify: claim", "key", k, "value", v)
				}
			}
		}
	}

	// Step 3: Log key fingerprint used for signing.
	keyHash := sha256.Sum256(signingKey)
	log.Info("hub-verify: signing key fingerprint",
		"key_len", len(signingKey),
		"sha256_prefix", hex.EncodeToString(keyHash[:8]),
		"key_b64_sample", base64.StdEncoding.EncodeToString(signingKey)[:min(12, len(signingKey))],
	)

	// Step 4: Make an HTTP-level request to confirm connectivity & auth,
	// logging request/response details.
	verifyURL := strings.TrimRight(hubEndpoint, "/") + "/api/v1/projects"
	log.Info("hub-verify: sending manual HTTP GET", "url", verifyURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, verifyURL, nil)
	if err != nil {
		log.Error("hub-verify: failed to build HTTP request", "error", err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Error("hub-verify: HTTP request failed", "error", err)
		return
	}
	defer resp.Body.Close()

	// Read response body (limit to 4KB for logging).
	var bodyBuf [4096]byte
	n, _ := resp.Body.Read(bodyBuf[:])
	bodySnippet := string(bodyBuf[:n])

	log.Info("hub-verify: HTTP response",
		"status", resp.StatusCode,
		"status_text", resp.Status,
		"content_type", resp.Header.Get("Content-Type"),
		"body_length", n,
		"body", bodySnippet,
	)

	if resp.StatusCode != http.StatusOK {
		log.Error("hub-verify: hub returned non-200 status — signing key mismatch or auth failure",
			"status", resp.StatusCode,
			"body", bodySnippet,
		)
		log.Error("=== hub connectivity check: FAILED ===")
		return
	}

	// Step 5: Also exercise the typed client to confirm the hubclient layer works.
	log.Info("hub-verify: listing projects via hubclient...")
	listCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	projectsResp, err := adminClient.Projects().List(listCtx, nil)
	if err != nil {
		log.Error("hub-verify: hubclient.Projects().List() failed", "error", err)
		log.Error("=== hub connectivity check: FAILED ===")
		return
	}

	log.Info("hub-verify: hubclient project list succeeded",
		"project_count", len(projectsResp.Projects),
	)
	for i, g := range projectsResp.Projects {
		log.Info("hub-verify: project",
			"index", i,
			"id", g.ID,
			"name", g.Name,
		)
	}

	log.Info("=== hub connectivity check: PASSED ===")
}

// loadConfig reads and parses the YAML configuration file.
// Environment variables in the form ${VAR} or $VAR are expanded before parsing.
func loadConfig(path string) (*chatapp.Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	// Expand environment variables in the raw YAML.
	expanded := os.ExpandEnv(string(data))

	var cfg chatapp.Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	return &cfg, nil
}

// discoverSigningKey searches GCP Secret Manager for a secret matching the
// local hub instance. It filters by scion-name=user_signing_key and
// scion-hub-hostname matching the local hostname, which uniquely identifies
// the hub in a multi-hub project.
//
// When multiple secrets match (e.g. after a hub migration that left a stale
// secret behind), the function prefers a secret with scion-type=internal
// (the type the hub currently uses for signing keys) over one with
// scion-type=environment (a legacy default).
func discoverSigningKey(ctx context.Context, projectID string) (value, resourceName string, err error) {
	client, err := secretmanager.NewClient(ctx)
	if err != nil {
		return "", "", fmt.Errorf("creating secret manager client: %w", err)
	}
	defer client.Close()

	hostname, err := os.Hostname()
	if err != nil {
		return "", "", fmt.Errorf("getting hostname for label match: %w", err)
	}
	// Labels are stored lowercase (sanitizeLabel in the hub).
	hostnameLabel := strings.ToLower(hostname)

	filter := fmt.Sprintf(
		"labels.scion-name=user_signing_key AND labels.scion-hub-hostname=%s",
		hostnameLabel,
	)

	slog.Debug("searching Secret Manager for signing key",
		"filter", filter,
		"parent", fmt.Sprintf("projects/%s", projectID),
	)

	it := client.ListSecrets(ctx, &smpb.ListSecretsRequest{
		Parent: fmt.Sprintf("projects/%s", projectID),
		Filter: filter,
	})

	// Collect all matching secrets so we can prefer the correct one when
	// stale secrets exist from prior hub migrations.
	var candidates []*smpb.Secret
	for {
		s, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return "", "", fmt.Errorf("listing secrets: %w", err)
		}
		candidates = append(candidates, s)
	}

	if len(candidates) == 0 {
		return "", "", fmt.Errorf("no secret with labels scion-name=user_signing_key, scion-hub-hostname=%s found in project %s", hostnameLabel, projectID)
	}

	// Pick the best candidate using a scoring heuristic. The hub's current
	// convention uses scion-type=internal and scion-scope-id=<hub-instance-id>
	// (not the legacy literal "hub"). When stale secrets survive from prior
	// migrations, this ranking avoids picking the wrong one.
	chosen := candidates[0]
	if len(candidates) > 1 {
		slog.Warn("multiple signing key secrets found, selecting best match",
			"count", len(candidates),
		)

		bestScore := -1
		for _, c := range candidates {
			slog.Debug("signing key candidate",
				"name", c.Name,
				"labels", c.Labels,
			)
			score := 0
			// Prefer scion-type=internal (current hub convention) over
			// environment (legacy) or empty.
			if c.Labels["scion-type"] == "internal" {
				score += 10
			}
			// Prefer a real hub-instance scope-id over the legacy literal "hub".
			// The hub now scopes keys to its actual instance ID.
			scopeID := c.Labels["scion-scope-id"]
			if scopeID != "" && scopeID != "hub" {
				score += 5
			}
			// Deprioritise scion-type=environment (known legacy default).
			if c.Labels["scion-type"] == "environment" {
				score -= 2
			}
			slog.Debug("signing key candidate score",
				"name", c.Name,
				"score", score,
				"scope_id", scopeID,
				"type", c.Labels["scion-type"],
			)
			if score > bestScore {
				bestScore = score
				chosen = c
			}
		}
	}

	slog.Debug("found signing key secret",
		"name", chosen.Name,
		"labels", chosen.Labels,
	)

	resp, err := client.AccessSecretVersion(ctx, &smpb.AccessSecretVersionRequest{
		Name: chosen.Name + "/versions/latest",
	})
	if err != nil {
		return "", "", fmt.Errorf("accessing secret %s: %w", chosen.Name, err)
	}
	return string(resp.Payload.Data), chosen.Name, nil
}

// accessSecret fetches the latest version of a GCP Secret Manager secret.
// The resourceName should be in the form "projects/{project}/secrets/{name}".
// It uses Application Default Credentials (ADC).
func accessSecret(ctx context.Context, resourceName string) (string, error) {
	client, err := secretmanager.NewClient(ctx)
	if err != nil {
		return "", fmt.Errorf("creating secret manager client: %w", err)
	}
	defer client.Close()

	resp, err := client.AccessSecretVersion(ctx, &smpb.AccessSecretVersionRequest{
		Name: resourceName + "/versions/latest",
	})
	if err != nil {
		return "", fmt.Errorf("accessing secret version: %w", err)
	}
	return string(resp.Payload.Data), nil
}

// gceProjectID returns the GCP project ID from the GCE metadata server,
// or "" if not running on GCE or the metadata is unavailable.
func gceProjectID() string {
	client := &http.Client{Timeout: 2 * time.Second}
	req, err := http.NewRequest(http.MethodGet,
		"http://metadata.google.internal/computeMetadata/v1/project/project-id", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Metadata-Flavor", "Google")
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(body))
}

// initLogger creates a structured logger from the logging configuration.
func initLogger(cfg chatapp.LoggingConfig) *slog.Logger {
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
