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

// Package metadata implements a GCE compute metadata server emulator.
// It runs as an in-process HTTP server within sciontool, providing GCP
// identity to agents via the standard metadata endpoint format.
package metadata

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

// Config holds configuration for the metadata server.
type Config struct {
	// Mode is "assign" or "block".
	Mode string
	// Port is the local port to listen on (default: 18380).
	Port int
	// SAEmail is the service account email (required for assign mode).
	SAEmail string
	// ProjectID is the GCP project ID.
	ProjectID string
	// HubURL is the Hub endpoint for token brokering.
	HubURL string
	// AuthToken is the agent's SCION_AUTH_TOKEN for Hub authentication.
	// Deprecated: Use TokenFunc for dynamic token retrieval.
	AuthToken string
	// TokenFunc, if set, is called to get the current auth token.
	// This allows the metadata server to pick up refreshed tokens.
	// If nil, the static AuthToken field is used.
	TokenFunc func() string
}

const (
	modeBlock  = "block"
	modeAssign = "assign"
)

// ConfigFromEnv reads metadata server configuration from environment variables.
// Returns nil if SCION_METADATA_MODE is not set.
func ConfigFromEnv() *Config {
	mode := os.Getenv("SCION_METADATA_MODE")
	if mode == "" {
		return nil
	}

	port := 18380
	if p := os.Getenv("SCION_METADATA_PORT"); p != "" {
		fmt.Sscanf(p, "%d", &port)
	}

	hubURL := os.Getenv("SCION_HUB_ENDPOINT")
	if hubURL == "" {
		hubURL = os.Getenv("SCION_HUB_URL")
	}

	return &Config{
		Mode:      mode,
		Port:      port,
		SAEmail:   os.Getenv("SCION_METADATA_SA_EMAIL"),
		ProjectID: os.Getenv("SCION_METADATA_PROJECT_ID"),
		HubURL:    hubURL,
		AuthToken: os.Getenv("SCION_AUTH_TOKEN"),
	}
}

// Server is the metadata HTTP server.
type Server struct {
	config Config
	srv    *http.Server
	client *http.Client

	// Token cache
	mu          sync.RWMutex
	cachedToken *cachedAccessToken
	// Identity token cache (keyed by audience)
	idTokenMu      sync.RWMutex
	cachedIDTokens map[string]*cachedIDToken

	// Singleflight for token fetches
	fetchMu       sync.Mutex
	fetchInFlight bool
	fetchDone     chan struct{}

	cancel             context.CancelFunc
	iptablesConfigured bool        // whether iptables redirect was successfully set up
	metadataBlocked    blockMethod // which blocking method was applied (block mode only)
}

// authToken returns the current auth token, preferring the dynamic TokenFunc
// over the static AuthToken field.
func (s *Server) authToken() string {
	if s.config.TokenFunc != nil {
		return s.config.TokenFunc()
	}
	return s.config.AuthToken
}

type cachedAccessToken struct {
	AccessToken string    `json:"access_token"`
	ExpiresIn   int       `json:"expires_in"`
	TokenType   string    `json:"token_type"`
	FetchedAt   time.Time `json:"-"`
}

type cachedIDToken struct {
	Token     string
	FetchedAt time.Time
	ExpiresAt time.Time
}

// New creates a new metadata server.
func New(cfg Config) *Server {
	return &Server{
		config:         cfg,
		client:         &http.Client{Timeout: 30 * time.Second},
		cachedIDTokens: make(map[string]*cachedIDToken),
	}
}

// Start starts the metadata server in the background. Returns immediately.
func (s *Server) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/computeMetadata/v1/", s.handleMetadata)

	addr := fmt.Sprintf("127.0.0.1:%d", s.config.Port)
	s.srv = &http.Server{
		Addr:    addr,
		Handler: s.requireMetadataFlavor(mux),
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		cancel()
		return fmt.Errorf("metadata server listen: %w", err)
	}

	go func() {
		log.Info("Metadata server started on %s (mode=%s)", addr, s.config.Mode)
		if err := s.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Error("Metadata server error: %v", err)
		}
	}()

	// Set up network-level interception for the GCE metadata server IP.
	//
	// For block mode: we apply BOTH a REDIRECT (so GCP SDKs hitting the IP
	// get a clean HTTP 403 from the sidecar) AND a filter-level REJECT as
	// defense-in-depth.
	//
	// For assign mode: only the REDIRECT is needed.
	//
	// In non-root containers (notably hosted Kubernetes agents), iptables
	// interception is not available. In that case the metadata env vars are the
	// primary mechanism and we skip the interception setup entirely to avoid
	// misleading warnings.
	s.configureMetadataInterception(os.Getuid())

	// Start proactive refresh if in assign mode
	if s.config.Mode == modeAssign {
		go s.proactiveRefreshLoop(ctx)
	}

	go func() {
		<-ctx.Done()
		if s.metadataBlocked != blockNone {
			cleanupMetadataBlock(s.metadataBlocked)
		}
		if s.iptablesConfigured {
			cleanupIPTablesRedirect(s.config.Port)
		}
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		s.srv.Shutdown(shutdownCtx)
	}()

	return nil
}

func shouldAttemptMetadataInterception(uid int) bool {
	return uid == 0
}

func (s *Server) configureMetadataInterception(uid int) {
	if !shouldAttemptMetadataInterception(uid) {
		log.Debug("Skipping metadata IP interception: process is not running as root")
		return
	}

	if err := setupIPTablesRedirect(s.config.Port); err != nil {
		// Non-fatal: iptables may not be available (no NET_ADMIN cap, non-Docker runtime).
		// The GCE_METADATA_HOST / GCE_METADATA_ROOT env vars are the primary mechanism.
		log.Debug("iptables redirect not available: %v", err)
	} else {
		s.iptablesConfigured = true
	}

	if s.config.Mode != modeBlock {
		return
	}

	// Defense-in-depth: block traffic to the metadata IP at the filter level
	// so that even if the nat REDIRECT fails or is bypassed, direct access to
	// the real metadata server is denied.
	method, err := setupMetadataBlock()
	if err != nil {
		log.Error("metadata block: failed to block metadata IP — direct access to %s may still be possible: %v", metadataIP, err)
		return
	}

	s.metadataBlocked = method
}

// Stop gracefully shuts down the server.
func (s *Server) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
}

func (s *Server) requireMetadataFlavor(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Health check at root doesn't require the header
		if r.URL.Path == "/" {
			next.ServeHTTP(w, r)
			return
		}

		if r.Header.Get("Metadata-Flavor") != "Google" {
			http.Error(w, "Missing Metadata-Flavor:Google header.", http.StatusForbidden)
			return
		}
		w.Header().Set("Metadata-Flavor", "Google")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		w.Header().Set("Metadata-Flavor", "Google")
		fmt.Fprint(w, "OK")
		return
	}
	http.NotFound(w, r)
}

func (s *Server) handleMetadata(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/computeMetadata/v1/")

	switch {
	case path == "" || path == "/":
		fmt.Fprint(w, "project/\ninstance/\n")

	case path == "project/project-id":
		fmt.Fprint(w, s.config.ProjectID)

	case path == "project/numeric-project-id":
		fmt.Fprint(w, "0")

	case path == "instance/service-accounts/" || path == "instance/service-accounts":
		s.handleServiceAccountList(w, r)

	case strings.HasPrefix(path, "instance/service-accounts/"):
		s.handleServiceAccount(w, r, strings.TrimPrefix(path, "instance/service-accounts/"))

	default:
		http.NotFound(w, r)
	}
}

// isRecursive returns true if the request has ?recursive=true (case-insensitive value).
func isRecursive(r *http.Request) bool {
	return strings.EqualFold(r.URL.Query().Get("recursive"), "true")
}

func (s *Server) handleServiceAccountList(w http.ResponseWriter, r *http.Request) {
	if s.config.Mode == modeBlock {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	if isRecursive(r) {
		w.Header().Set("Content-Type", "application/json")
		result := map[string]interface{}{
			"default": s.serviceAccountInfo("default"),
		}
		if s.config.SAEmail != "" {
			result[s.config.SAEmail] = s.serviceAccountInfo(s.config.SAEmail)
		}
		json.NewEncoder(w).Encode(result)
		return
	}

	fmt.Fprintf(w, "default/\n%s/\n", s.config.SAEmail)
}

// serviceAccountInfo returns the recursive JSON representation of a service account.
func (s *Server) serviceAccountInfo(account string) map[string]interface{} {
	aliases := []string{}
	if account == "default" {
		aliases = []string{"default"}
	}
	return map[string]interface{}{
		"email":   s.config.SAEmail,
		"scopes":  []string{"https://www.googleapis.com/auth/cloud-platform"},
		"aliases": aliases,
	}
}

func (s *Server) handleServiceAccount(w http.ResponseWriter, r *http.Request, path string) {
	// Parse: {account}/{action}
	parts := strings.SplitN(path, "/", 2)
	account := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	// Validate account
	if account != "default" && account != s.config.SAEmail {
		http.NotFound(w, r)
		return
	}

	if s.config.Mode == modeBlock {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}

	switch action {
	case "email":
		fmt.Fprint(w, s.config.SAEmail)

	case "scopes":
		scopes := "https://www.googleapis.com/auth/cloud-platform"
		fmt.Fprint(w, scopes)

	case "token":
		s.handleToken(w, r)

	case "identity":
		s.handleIdentityToken(w, r)

	case "":
		if isRecursive(r) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(s.serviceAccountInfo(account))
			return
		}
		// List endpoints for this account
		fmt.Fprint(w, "email\nscopes\ntoken\nidentity\n")

	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	// Check cache
	s.mu.RLock()
	cached := s.cachedToken
	s.mu.RUnlock()

	if cached != nil {
		elapsed := time.Since(cached.FetchedAt)
		remaining := time.Duration(cached.ExpiresIn)*time.Second - elapsed
		if remaining > 60*time.Second {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"access_token": cached.AccessToken,
				"expires_in":   int(remaining.Seconds()),
				"token_type":   cached.TokenType,
			})
			return
		}
	}

	// Fetch from Hub
	token, err := s.fetchAccessToken(r.Context())
	if err != nil {
		log.Error("Failed to fetch GCP access token from Hub: %v", err)
		http.Error(w, "token generation failed", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(token)
}

func (s *Server) handleIdentityToken(w http.ResponseWriter, r *http.Request) {
	audience := r.URL.Query().Get("audience")
	if audience == "" {
		http.Error(w, "audience parameter is required", http.StatusBadRequest)
		return
	}

	// Check cache
	s.idTokenMu.RLock()
	cached := s.cachedIDTokens[audience]
	s.idTokenMu.RUnlock()

	if cached != nil && time.Now().Before(cached.ExpiresAt.Add(-60*time.Second)) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, cached.Token)
		return
	}

	token, err := s.fetchIdentityToken(r.Context(), audience)
	if err != nil {
		log.Error("Failed to fetch GCP identity token from Hub: %v", err)
		http.Error(w, "identity token generation failed", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprint(w, token.Token)
}

func (s *Server) fetchAccessToken(ctx context.Context) (*cachedAccessToken, error) {
	endpoint := fmt.Sprintf("%s/api/v1/agent/gcp-token", strings.TrimSuffix(s.config.HubURL, "/"))

	body, _ := json.Marshal(map[string][]string{
		"scopes": {"https://www.googleapis.com/auth/cloud-platform"},
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.authToken())

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hub request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hub returned %d: %s", resp.StatusCode, string(respBody))
	}

	var token cachedAccessToken
	if err := json.Unmarshal(respBody, &token); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	token.FetchedAt = time.Now()

	// Cache
	s.mu.Lock()
	s.cachedToken = &token
	s.mu.Unlock()

	return &token, nil
}

type hubIDTokenResponse struct {
	Token string `json:"token"`
}

func (s *Server) fetchIdentityToken(ctx context.Context, audience string) (*cachedIDToken, error) {
	endpoint := fmt.Sprintf("%s/api/v1/agent/gcp-identity-token", strings.TrimSuffix(s.config.HubURL, "/"))

	body, _ := json.Marshal(map[string]string{"audience": audience})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.authToken())

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hub request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hub returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result hubIDTokenResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	cached := &cachedIDToken{
		Token:     result.Token,
		FetchedAt: time.Now(),
		ExpiresAt: time.Now().Add(55 * time.Minute), // Conservative: ID tokens are ~1hr
	}

	s.idTokenMu.Lock()
	s.cachedIDTokens[audience] = cached
	s.idTokenMu.Unlock()

	return cached, nil
}

func (s *Server) proactiveRefreshLoop(ctx context.Context) {
	// Wait for first request to populate the cache, then refresh proactively
	ticker := time.NewTicker(4 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.mu.RLock()
			cached := s.cachedToken
			s.mu.RUnlock()

			if cached == nil {
				continue
			}

			elapsed := time.Since(cached.FetchedAt)
			remaining := time.Duration(cached.ExpiresIn)*time.Second - elapsed
			if remaining < 300*time.Second {
				log.Debug("Proactively refreshing GCP access token (remaining: %v)", remaining)
				refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				if _, err := s.fetchAccessToken(refreshCtx); err != nil {
					log.Error("Proactive token refresh failed: %v", err)
				}
				cancel()
			}
		}
	}
}
