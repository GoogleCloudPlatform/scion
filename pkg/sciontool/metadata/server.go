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
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
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
	// NetworkMode is the container network mode (e.g. "host").
	// When "host", iptables interception is skipped to avoid leaking
	// redirect rules into the host's network namespace.
	NetworkMode string

	// FetchGCPToken, if set, is called to obtain a GCP access token from the
	// Hub instead of making a direct HTTP call. This allows the metadata
	// server to use the hub client's OIDC transport and correct auth headers.
	// If nil, the server falls back to direct HTTP requests.
	FetchGCPToken func(ctx context.Context, scopes []string) (*GCPAccessTokenResponse, error)

	// FetchGCPIdentityToken, if set, is called to obtain a GCP identity
	// token from the Hub. Same motivation as FetchGCPToken.
	FetchGCPIdentityToken func(ctx context.Context, audience string) (string, error)
}

const (
	modeBlock  = "block"
	modeAssign = "assign"

	maxRestarts         = 3
	healthCheckInterval = 30 * time.Second
	healthCheckTimeout  = 2 * time.Second
	healthFailThreshold = 3
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
		_, _ = fmt.Sscanf(p, "%d", &port)
	}

	hubURL := os.Getenv("SCION_HUB_ENDPOINT")
	if hubURL == "" {
		hubURL = os.Getenv("SCION_HUB_URL")
	}

	return &Config{
		Mode:        mode,
		Port:        port,
		SAEmail:     os.Getenv("SCION_METADATA_SA_EMAIL"),
		ProjectID:   os.Getenv("SCION_METADATA_PROJECT_ID"),
		HubURL:      hubURL,
		AuthToken:   os.Getenv("SCION_AUTH_TOKEN"),
		NetworkMode: os.Getenv("SCION_NETWORK_MODE"),
	}
}

// Server is the metadata HTTP server.
type Server struct {
	config Config
	srv    *http.Server
	client *http.Client

	// Token cache
	mu          sync.RWMutex
	cachedToken *GCPAccessTokenResponse
	// Identity token cache (keyed by audience)
	idTokenMu      sync.RWMutex
	cachedIDTokens map[string]*cachedIDToken

	// Singleflight for access token fetches
	fetchMu       sync.Mutex
	fetchInFlight bool
	fetchDone     chan struct{}
	fetchErr      error

	// Singleflight for identity token fetches
	idFetchMu       sync.Mutex
	idFetchInFlight bool
	idFetchDone     chan struct{}
	idFetchErr      error
	idFetchAudience string

	cancel             context.CancelFunc
	iptablesConfigured bool        // whether iptables redirect was successfully set up
	metadataBlocked    blockMethod // which blocking method was applied (block mode only)

	healthMu     sync.Mutex
	restartCount int
	abandoned    bool

	shutdownToken     string
	shutdownTokenPath string
}

// authToken returns the current auth token, preferring the dynamic TokenFunc
// over the static AuthToken field.
func (s *Server) authToken() string {
	if s.config.TokenFunc != nil {
		return s.config.TokenFunc()
	}
	return s.config.AuthToken
}

// GCPAccessTokenResponse is the response from a GCP access token fetch.
type GCPAccessTokenResponse struct {
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

// activeServer tracks the most recently started Server in this process so that
// a new Start() call can forcefully close a stale listener without relying on
// an HTTP endpoint (which may not exist on older binaries).
var (
	activeServerMu sync.Mutex
	activeServer   *Server
)

// New creates a new metadata server.
func New(cfg Config) *Server {
	return &Server{
		config:         cfg,
		client:         &http.Client{Timeout: 10 * time.Second},
		cachedIDTokens: make(map[string]*cachedIDToken),
	}
}

func (s *Server) buildMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/computeMetadata/v1/", s.handleMetadata)
	mux.HandleFunc("/_scion/shutdown", s.handleShutdown)
	return s.requireMetadataFlavor(mux)
}

// Start starts the metadata server in the background. Returns immediately.
// If the port is already in use (e.g. a stale metadata server from a previous
// init cycle), Start attempts to gracefully shut it down and retry.
func (s *Server) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel

	addr := fmt.Sprintf("127.0.0.1:%d", s.config.Port)

	ln, err := net.Listen("tcp", addr)
	if err != nil && errors.Is(err, syscall.EADDRINUSE) {
		log.Info("Metadata server port %d already in use, attempting to reclaim", s.config.Port)

		// Primary: forcefully close a stale server in this process via the
		// package-level reference. This is reliable regardless of which
		// binary version started the old server.
		activeServerMu.Lock()
		prev := activeServer
		activeServerMu.Unlock()
		if prev != nil && prev.srv != nil {
			log.Info("Forcefully closing previous metadata server instance")
			_ = prev.srv.Close()
		} else {
			// Fallback: try the HTTP shutdown endpoint (cross-process or
			// the package-level reference was lost).
			s.shutdownExisting()
		}

		for attempt := 1; attempt <= 3; attempt++ {
			time.Sleep(time.Duration(attempt) * 250 * time.Millisecond)
			ln, err = net.Listen("tcp", addr)
			if err == nil {
				log.Info("Reclaimed metadata server port %d after %d retries", s.config.Port, attempt)
				break
			}
			if !errors.Is(err, syscall.EADDRINUSE) {
				break
			}
		}
	}
	if err != nil {
		cancel()
		return fmt.Errorf("metadata server listen: %w", err)
	}

	if err := s.ensureShutdownToken(); err != nil {
		cancel()
		_ = ln.Close()
		return fmt.Errorf("metadata server shutdown token: %w", err)
	}
	s.srv = &http.Server{
		Addr:    addr,
		Handler: s.buildMux(),
	}

	// Track this server so a future Start() can forcefully close it.
	activeServerMu.Lock()
	activeServer = s
	activeServerMu.Unlock()

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

	go s.healthCheckLoop(ctx)

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
		_ = s.srv.Shutdown(shutdownCtx)
	}()

	return nil
}

func shouldAttemptMetadataInterception(uid int, networkMode string) bool {
	if networkMode == "host" {
		return false
	}
	return uid == 0
}

func (s *Server) configureMetadataInterception(uid int) {
	if !shouldAttemptMetadataInterception(uid, s.config.NetworkMode) {
		if s.config.NetworkMode == "host" {
			log.Debug("Skipping metadata IP interception: host networking mode (iptables would leak to host namespace)")
		} else {
			log.Debug("Skipping metadata IP interception: process is not running as root")
		}
		return
	}

	err := setupIPTablesRedirect(s.config.Port)
	if err != nil {
		// Non-fatal: iptables may not be available (no NET_ADMIN cap, non-Docker runtime).
		// The GCE_METADATA_HOST / GCE_METADATA_ROOT env vars are the primary mechanism.
		log.Debug("iptables redirect not available: %v", err)
	}
	if err == nil {
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

// Stop gracefully shuts down the server. It closes the listener synchronously
// so the port is released before Stop returns. The background goroutine handles
// iptables cleanup separately.
func (s *Server) Stop() {
	activeServerMu.Lock()
	if activeServer == s {
		activeServer = nil
	}
	activeServerMu.Unlock()

	// Close the listener and drain connections immediately so the port is
	// released before the caller proceeds. The context-cancellation goroutine
	// still runs for iptables cleanup; http.Server.Shutdown is safe to call
	// more than once.
	if s.srv != nil {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = s.srv.Shutdown(shutdownCtx)
		shutdownCancel()
	}
	if s.shutdownTokenPath != "" {
		if err := os.Remove(s.shutdownTokenPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Debug("Failed to remove metadata shutdown token file %s: %v", s.shutdownTokenPath, err)
		}
	}

	if s.cancel != nil {
		s.cancel()
	}
}

// shutdownExisting tries to shut down an existing metadata server on the port
// by sending a POST to its /_scion/shutdown endpoint. This handles the case
// where a stale server from a previous init cycle holds the port.
func (s *Server) shutdownExisting() {
	client := &http.Client{Timeout: 2 * time.Second}
	url := fmt.Sprintf("http://127.0.0.1:%d/_scion/shutdown", s.config.Port)
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		return
	}
	req.Header.Set("Metadata-Flavor", "Google")
	token, err := os.ReadFile(shutdownTokenPath(s.config.Port))
	if err != nil {
		log.Debug("Could not read metadata shutdown token for port %d: %v", s.config.Port, err)
		return
	}
	req.Header.Set("X-Scion-Shutdown-Token", strings.TrimSpace(string(token)))
	resp, err := client.Do(req)
	if err != nil {
		log.Debug("Could not reach existing metadata server for shutdown: %v", err)
		return
	}
	_ = resp.Body.Close()
	log.Info("Sent shutdown request to existing metadata server on port %d (status=%d)", s.config.Port, resp.StatusCode)
}

// handleShutdown handles POST /_scion/shutdown requests, allowing a new
// metadata server instance to reclaim the port from a stale server.
func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.shutdownToken == "" || r.Header.Get("X-Scion-Shutdown-Token") != s.shutdownToken {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	log.Info("Shutdown requested via /_scion/shutdown, stopping metadata server")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, "shutting down")
	go func() {
		time.Sleep(50 * time.Millisecond)
		s.Stop()
	}()
}

func shutdownTokenPath(port int) string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("scion-metadata-shutdown-%d.token", port))
}

func (s *Server) ensureShutdownToken() error {
	if s.shutdownToken != "" {
		return nil
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return err
	}
	s.shutdownToken = hex.EncodeToString(tokenBytes)
	s.shutdownTokenPath = shutdownTokenPath(s.config.Port)
	return writeShutdownToken(s.shutdownTokenPath, s.shutdownToken)
}

func writeShutdownToken(path, token string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(token + "\n"); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

func (s *Server) probeHealth() bool {
	client := &http.Client{Timeout: healthCheckTimeout}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/", s.config.Port))
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (s *Server) isAbandoned() bool {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	return s.abandoned
}

func (s *Server) healthCheckLoop(ctx context.Context) {
	select {
	case <-time.After(5 * time.Second):
	case <-ctx.Done():
		return
	}

	ticker := time.NewTicker(healthCheckInterval)
	defer ticker.Stop()

	consecutiveFailures := 0

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if s.isAbandoned() {
				return
			}
			if s.probeHealth() {
				consecutiveFailures = 0
				continue
			}
			consecutiveFailures++
			log.Error("Metadata server health check failed (%d/%d)",
				consecutiveFailures, healthFailThreshold)

			if consecutiveFailures >= healthFailThreshold {
				log.Error("Metadata server unresponsive after %d probes, attempting restart",
					consecutiveFailures)
				if err := s.restartHTTP(ctx); err != nil {
					log.Error("Metadata server restart failed: %v", err)
				} else {
					log.Info("Metadata server restarted successfully")
				}
				consecutiveFailures = 0
			}
		}
	}
}

func (s *Server) restartHTTP(ctx context.Context) error {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()

	if s.abandoned {
		return fmt.Errorf("metadata server abandoned after %d restart failures", maxRestarts)
	}

	s.restartCount++
	if s.restartCount > maxRestarts {
		s.abandoned = true
		log.Error("Metadata server restart limit reached (%d), abandoning", maxRestarts)
		return fmt.Errorf("restart limit reached")
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = s.srv.Shutdown(shutdownCtx)
	shutdownCancel()

	addr := fmt.Sprintf("127.0.0.1:%d", s.config.Port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("restart listen: %w", err)
	}

	s.srv = &http.Server{
		Addr:    addr,
		Handler: s.buildMux(),
	}

	go func() {
		log.Info("Metadata server restarted on %s (attempt %d/%d)",
			addr, s.restartCount, maxRestarts)
		if err := s.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Error("Metadata server error after restart: %v", err)
		}
	}()

	time.Sleep(100 * time.Millisecond)
	if !s.probeHealth() {
		return fmt.Errorf("restarted server failed immediate health check")
	}

	restartAttempt := s.restartCount
	go func() {
		select {
		case <-time.After(60 * time.Second):
			s.healthMu.Lock()
			if s.restartCount == restartAttempt {
				s.restartCount = 0
				log.Debug("Metadata server restart counter reset (stable for 60s)")
			}
			s.healthMu.Unlock()
		case <-ctx.Done():
		}
	}()

	return nil
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
		_, _ = fmt.Fprint(w, "OK")
		return
	}
	http.NotFound(w, r)
}

func (s *Server) handleMetadata(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/computeMetadata/v1/")

	switch {
	case path == "" || path == "/":
		_, _ = fmt.Fprint(w, "project/\ninstance/\n")

	case path == "project/project-id":
		_, _ = fmt.Fprint(w, s.config.ProjectID)

	case path == "project/numeric-project-id":
		_, _ = fmt.Fprint(w, "0")

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
		_ = json.NewEncoder(w).Encode(result)
		return
	}

	_, _ = fmt.Fprintf(w, "default/\n%s/\n", s.config.SAEmail)
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
		_, _ = fmt.Fprint(w, s.config.SAEmail)

	case "scopes":
		scopes := "https://www.googleapis.com/auth/cloud-platform"
		_, _ = fmt.Fprint(w, scopes)

	case "token":
		s.handleToken(w, r)

	case "identity":
		s.handleIdentityToken(w, r)

	case "":
		if isRecursive(r) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(s.serviceAccountInfo(account))
			return
		}
		// List endpoints for this account
		_, _ = fmt.Fprint(w, "email\nscopes\ntoken\nidentity\n")

	default:
		http.NotFound(w, r)
	}
}

func (s *Server) serveCachedToken(w http.ResponseWriter) bool {
	s.mu.RLock()
	cached := s.cachedToken
	s.mu.RUnlock()

	if cached == nil {
		return false
	}
	elapsed := time.Since(cached.FetchedAt)
	remaining := time.Duration(cached.ExpiresIn)*time.Second - elapsed
	if remaining <= 60*time.Second {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"access_token": cached.AccessToken,
		"expires_in":   int(remaining.Seconds()),
		"token_type":   cached.TokenType,
	})
	return true
}

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	if s.serveCachedToken(w) {
		return
	}

	// Singleflight: collapse concurrent fetches into one Hub request
	s.fetchMu.Lock()
	if s.fetchInFlight {
		done := s.fetchDone
		s.fetchMu.Unlock()
		<-done
		if s.serveCachedToken(w) {
			return
		}
		http.Error(w, "token generation failed", http.StatusBadGateway)
		return
	}
	s.fetchInFlight = true
	s.fetchDone = make(chan struct{})
	s.fetchMu.Unlock()

	token, err := s.fetchAccessToken(r.Context())

	s.fetchMu.Lock()
	s.fetchInFlight = false
	s.fetchErr = err
	close(s.fetchDone)
	s.fetchMu.Unlock()

	if err != nil {
		log.Error("Failed to fetch GCP access token from Hub: %v", err)
		http.Error(w, "token generation failed", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(token)
}

func (s *Server) serveCachedIDToken(w http.ResponseWriter, audience string) bool {
	s.idTokenMu.RLock()
	cached := s.cachedIDTokens[audience]
	s.idTokenMu.RUnlock()

	if cached == nil || !time.Now().Before(cached.ExpiresAt.Add(-60*time.Second)) {
		return false
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = fmt.Fprint(w, cached.Token)
	return true
}

func (s *Server) handleIdentityToken(w http.ResponseWriter, r *http.Request) {
	audience := r.URL.Query().Get("audience")
	if audience == "" {
		http.Error(w, "audience parameter is required", http.StatusBadRequest)
		return
	}

	if s.serveCachedIDToken(w, audience) {
		return
	}

	// Singleflight: collapse concurrent fetches for the same audience
	s.idFetchMu.Lock()
	if s.idFetchInFlight && s.idFetchAudience == audience {
		done := s.idFetchDone
		s.idFetchMu.Unlock()
		<-done
		if s.serveCachedIDToken(w, audience) {
			return
		}
		http.Error(w, "identity token generation failed", http.StatusBadGateway)
		return
	}
	s.idFetchInFlight = true
	s.idFetchAudience = audience
	s.idFetchDone = make(chan struct{})
	s.idFetchMu.Unlock()

	token, err := s.fetchIdentityToken(r.Context(), audience)

	s.idFetchMu.Lock()
	s.idFetchInFlight = false
	s.idFetchErr = err
	close(s.idFetchDone)
	s.idFetchMu.Unlock()

	if err != nil {
		log.Error("Failed to fetch GCP identity token from Hub: %v", err)
		http.Error(w, "identity token generation failed", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "text/plain")
	_, _ = fmt.Fprint(w, token.Token)
}

func (s *Server) fetchAccessToken(ctx context.Context) (*GCPAccessTokenResponse, error) {
	var token *GCPAccessTokenResponse
	var err error

	if s.config.FetchGCPToken != nil {
		token, err = s.config.FetchGCPToken(ctx, []string{"https://www.googleapis.com/auth/cloud-platform"})
	} else {
		token, err = s.fetchAccessTokenDirect(ctx)
	}

	if err != nil {
		return nil, err
	}

	token.FetchedAt = time.Now()

	s.mu.Lock()
	s.cachedToken = token
	s.mu.Unlock()

	return token, nil
}

func (s *Server) fetchAccessTokenDirect(ctx context.Context) (*GCPAccessTokenResponse, error) {
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
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hub returned %d: %s", resp.StatusCode, string(respBody))
	}

	var token GCPAccessTokenResponse
	if err := json.Unmarshal(respBody, &token); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	return &token, nil
}

type hubIDTokenResponse struct {
	Token string `json:"token"`
}

func (s *Server) fetchIdentityToken(ctx context.Context, audience string) (*cachedIDToken, error) {
	var tokenStr string
	var err error

	if s.config.FetchGCPIdentityToken != nil {
		tokenStr, err = s.config.FetchGCPIdentityToken(ctx, audience)
	} else {
		tokenStr, err = s.fetchIdentityTokenDirect(ctx, audience)
	}

	if err != nil {
		return nil, err
	}

	cached := &cachedIDToken{
		Token:     tokenStr,
		FetchedAt: time.Now(),
		ExpiresAt: time.Now().Add(55 * time.Minute),
	}

	s.idTokenMu.Lock()
	s.cachedIDTokens[audience] = cached
	s.idTokenMu.Unlock()

	return cached, nil
}

func (s *Server) fetchIdentityTokenDirect(ctx context.Context, audience string) (string, error) {
	endpoint := fmt.Sprintf("%s/api/v1/agent/gcp-identity-token", strings.TrimSuffix(s.config.HubURL, "/"))

	body, _ := json.Marshal(map[string]string{"audience": audience})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.authToken())

	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("hub request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("hub returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result hubIDTokenResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}

	return result.Token, nil
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
				refreshCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				if _, err := s.fetchAccessToken(refreshCtx); err != nil {
					log.Error("Proactive token refresh failed: %v", err)
				}
				cancel()
			}
		}
	}
}
