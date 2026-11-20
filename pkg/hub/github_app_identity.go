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

package hub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/githubapp"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	githubAppIdentityFetchTimeout        = 5 * time.Second
	githubAppIdentityFailureRetryBackoff = 5 * time.Minute
	githubAppSlugWarnInterval            = time.Hour
)

// githubTriggersRequiredEvents lists the GitHub App webhook events Stage 3
// requires for trigger readiness (§10.1). Sorted for deterministic output.
// Note: push is optional (D-S3-9 / G2b) and is not required for readiness.
var githubTriggersRequiredEvents = []string{
	"issue_comment",
	"issues",
	"pull_request",
	"pull_request_review",
	"pull_request_review_comment",
}

type githubRequiredPermission struct {
	name  string
	level string
}

// githubTriggersRequiredPermissions lists the GitHub App permissions Stage 3
// requires for trigger readiness (§10.1). Sorted by name for deterministic output.
var githubTriggersRequiredPermissions = []githubRequiredPermission{
	{name: "contents", level: "write"},
	{name: "issues", level: "write"},
	{name: "metadata", level: "read"},
	{name: "pull_requests", level: "write"},
}

// GitHubTriggersReadiness reports whether the configured GitHub App has the
// webhook settings, subscribed events, and permissions required for GitHub
// triggers (§10.1, §10.2).
type GitHubTriggersReadiness struct {
	Ready              bool       `json:"ready"`
	WebhooksEnabled    bool       `json:"webhooks_enabled"`
	HasWebhookSecret   bool       `json:"has_webhook_secret"`
	MissingEvents      []string   `json:"missing_events"`
	MissingPermissions []string   `json:"missing_permissions"`
	AppSlug            string     `json:"app_slug"`
	CheckedAt          *time.Time `json:"checked_at,omitempty"`
	Error              string     `json:"error,omitempty"`
}

// GitHubTriggersProjectReadiness is the project-scoped readiness view (§10.2):
// project members see overall readiness and missing App items without hub-level
// webhook configuration fields.
type GitHubTriggersProjectReadiness struct {
	Ready              bool       `json:"ready"`
	MissingEvents      []string   `json:"missing_events"`
	MissingPermissions []string   `json:"missing_permissions"`
	AppSlug            string     `json:"app_slug,omitempty"`
	CheckedAt          *time.Time `json:"checked_at,omitempty"`
	Error              string     `json:"error,omitempty"`
}

// githubTriggersEnabled reports whether GitHub App triggers are enabled.
// Both hub.triggers and hub.github_triggers must be enabled server-side (§4.1 step 0, §8.2).
func (s *Server) githubTriggersEnabled() bool {
	return s.experimentEnabled(experiments.Triggers) && s.experimentEnabled(experiments.GitHubTriggers)
}

// requireGitHubTriggers wraps next with the server-side experiment gate for
// GitHub App triggers. Registration still validates experiments.GitHubTriggers
// via requireExperiment so a misconfigured registry fails at startup.
func (s *Server) requireGitHubTriggers(next http.HandlerFunc) http.HandlerFunc {
	gated := s.requireExperiment(experiments.GitHubTriggers, next)
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.githubTriggersEnabled() {
			NotFound(w, "route")
			return
		}
		gated(w, r)
	}
}

// githubAppIdentityState holds the cached GET /app metadata on Server.
type githubAppIdentityState struct {
	refreshMu sync.Mutex

	mu           sync.RWMutex
	gen          uint64
	appID        int64
	identity     *githubapp.AppIdentity
	checkedAt    time.Time
	lastErr      string
	lastSlugWarn time.Time
	nowFn        func() time.Time

	startupMu      sync.Mutex
	startupCancel  context.CancelFunc
	startupStopped bool
	startupWG      sync.WaitGroup
}

func (c *githubAppIdentityState) reset() {
	c.mu.Lock()
	c.gen++
	c.appID = 0
	c.identity = nil
	c.checkedAt = time.Time{}
	c.lastErr = ""
	c.mu.Unlock()
}

func (c *githubAppIdentityState) generation() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.gen
}

func (c *githubAppIdentityState) now() time.Time {
	c.mu.RLock()
	fn := c.nowFn
	c.mu.RUnlock()
	if fn != nil {
		return fn()
	}
	return timeNow()
}

func (c *githubAppIdentityState) snapshot(appID int64) (*githubapp.AppIdentity, time.Time, string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if appID == 0 || c.appID != appID {
		return nil, time.Time{}, ""
	}
	var cp *githubapp.AppIdentity
	if c.identity != nil {
		cloned := c.identity.Clone()
		cp = &cloned
	}
	return cp, c.checkedAt, c.lastErr
}

func (c *githubAppIdentityState) shouldWarnSlugUnavailable(now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.lastSlugWarn.IsZero() && now.Sub(c.lastSlugWarn) < githubAppSlugWarnInterval {
		return false
	}
	c.lastSlugWarn = now
	return true
}

// Private key source identifiers reported by the GitHub App admin API.
const (
	GitHubAppPrivateKeySourceConfig = "config"
	GitHubAppPrivateKeySourceFile   = "file"
	GitHubAppPrivateKeySourceSecret = "secret"
)

// githubAppKeyStatus describes which private key source is active for the
// GitHub App and whether a configured private key or private_key_path overrides
// a stored GITHUB_APP_PRIVATE_KEY hub secret.
type githubAppKeyStatus struct {
	hasPrivateKey   bool
	source          string
	overridesSecret bool
}

// hasStoredGitHubAppSecret reports whether a hub-scoped GitHub App secret
// exists in the secrets backend (or store fallback) without decrypting it.
func (s *Server) hasStoredGitHubAppSecret(ctx context.Context, name string) bool {
	if s.secretBackend != nil {
		meta, err := s.secretBackend.GetMeta(ctx, name, store.ScopeHub, s.hubID)
		return err == nil && meta != nil
	}
	if s.store != nil {
		sec, err := s.store.GetSecret(ctx, name, store.ScopeHub, s.hubID)
		return err == nil && sec != nil
	}
	return false
}

// githubAppPrivateKeyStatus resolves the active GitHub App private key source
// using the same precedence as getGitHubAppClient (in-memory config, then
// private_key_path, then hub secret) and reports whether a configured private
// key or private_key_path is overriding a stored GITHUB_APP_PRIVATE_KEY hub
// secret.
func (s *Server) githubAppPrivateKeyStatus(ctx context.Context) (GitHubAppServerConfig, githubAppKeyStatus) {
	s.mu.RLock()
	cfg := s.config.GitHubAppConfig
	fromSecret := s.githubAppPrivateKeyFromSecret
	s.mu.RUnlock()

	if cfg.PrivateKey != "" {
		if fromSecret {
			return cfg, githubAppKeyStatus{
				hasPrivateKey: true,
				source:        GitHubAppPrivateKeySourceSecret,
			}
		}
		return cfg, githubAppKeyStatus{
			hasPrivateKey:   true,
			source:          GitHubAppPrivateKeySourceConfig,
			overridesSecret: s.hasStoredGitHubAppSecret(ctx, GitHubAppSecretPrivateKey),
		}
	}
	if cfg.PrivateKeyPath != "" {
		return cfg, githubAppKeyStatus{
			hasPrivateKey:   true,
			source:          GitHubAppPrivateKeySourceFile,
			overridesSecret: s.hasStoredGitHubAppSecret(ctx, GitHubAppSecretPrivateKey),
		}
	}
	if s.hasStoredGitHubAppSecret(ctx, GitHubAppSecretPrivateKey) {
		return cfg, githubAppKeyStatus{
			hasPrivateKey: true,
			source:        GitHubAppPrivateKeySourceSecret,
		}
	}
	return cfg, githubAppKeyStatus{}
}

// warnGitHubAppKeyOverride logs a warning once per process if a configured
// private key or private_key_path is overriding a stored GITHUB_APP_PRIVATE_KEY
// hub secret. It checks an atomic flag before doing any secret-metadata lookup
// so hot paths like getGitHubAppClient incur no extra DB reads after the first
// warning. It never logs key material.
func (s *Server) warnGitHubAppKeyOverride(ctx context.Context) {
	if s.githubAppKeyOverrideWarned.Load() {
		return
	}
	_, st := s.githubAppPrivateKeyStatus(ctx)
	if !st.overridesSecret {
		return
	}
	if s.githubAppKeyOverrideWarned.CompareAndSwap(false, true) {
		slog.Warn("GitHub App configured private key or private_key_path is overriding the stored hub secret",
			"active_source", st.source,
			"overridden_secret", GitHubAppSecretPrivateKey,
		)
	}
}

// githubAppCredentialPresence checks whether the GitHub App private key and
// webhook secret are present in in-memory config, a configured key path, or
// the hub secret backend.
func (s *Server) githubAppCredentialPresence(ctx context.Context) (cfg GitHubAppServerConfig, hasPrivateKey, hasWebhookSecret bool) {
	cfg, keyStatus := s.githubAppPrivateKeyStatus(ctx)
	hasPrivateKey = keyStatus.hasPrivateKey
	hasWebhookSecret = cfg.WebhookSecret != ""
	if !hasWebhookSecret {
		hasWebhookSecret = s.hasStoredGitHubAppSecret(ctx, GitHubAppSecretWebhookSecret)
	}
	return cfg, hasPrivateKey, hasWebhookSecret
}

// githubAppIdentityErrorMessage maps a GET /app or client-construction error to
// fixed, content-free text suitable for API responses (ptone/scion#7498).
func githubAppIdentityErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	var mintErr *githubapp.TokenMintError
	if errors.As(err, &mintErr) && mintErr.Message != "" {
		return mintErr.Message
	}
	return "Failed to fetch GitHub App metadata from GitHub."
}

// refreshGitHubAppIdentity performs a single GET /app call, updates the
// Server's cached AppIdentity (and rate-limit snapshot), and returns the
// parsed identity. Concurrent callers serialize on refreshMu; callers that only
// want a lazy initial load use ensureGitHubAppIdentity instead.
func (s *Server) refreshGitHubAppIdentity(ctx context.Context) (*githubapp.AppIdentity, error) {
	s.githubAppIdentity.refreshMu.Lock()
	defer s.githubAppIdentity.refreshMu.Unlock()
	return s.refreshGitHubAppIdentityLocked(ctx)
}

func (s *Server) refreshGitHubAppIdentityLocked(ctx context.Context) (*githubapp.AppIdentity, error) {
	s.mu.RLock()
	appID := s.config.GitHubAppConfig.AppID
	startGen := s.githubAppIdentity.generation()
	s.mu.RUnlock()

	now := s.githubAppIdentity.now().UTC()
	client, err := s.getGitHubAppClient()
	if err != nil {
		if appID != 0 {
			s.githubAppIdentity.mu.Lock()
			if s.githubAppIdentity.gen == startGen {
				s.githubAppIdentity.appID = appID
				s.githubAppIdentity.identity = nil
				s.githubAppIdentity.checkedAt = now
				s.githubAppIdentity.lastErr = "GitHub App credentials are not valid or not configured."
			}
			s.githubAppIdentity.mu.Unlock()
		}
		return nil, fmt.Errorf("GitHub App not configured: %w", err)
	}

	fetchCtx, cancel := context.WithTimeout(ctx, githubAppIdentityFetchTimeout)
	defer cancel()

	raw, err := client.GetApp(fetchCtx)
	if rl := client.GetRateLimit(); rl != nil {
		s.mu.Lock()
		s.githubAppRateLimit = rl
		s.mu.Unlock()
	}
	now = s.githubAppIdentity.now().UTC()
	if err != nil {
		// Do not poison the shared cache when the caller's own context was
		// canceled or expired (e.g. an aborted HTTP request or server shutdown),
		// or when a concurrent config reset invalidated startGen.
		if ctx.Err() == nil {
			msg := githubAppIdentityErrorMessage(err)
			s.githubAppIdentity.mu.Lock()
			if s.githubAppIdentity.gen == startGen {
				s.githubAppIdentity.appID = appID
				s.githubAppIdentity.identity = nil
				s.githubAppIdentity.checkedAt = now
				s.githubAppIdentity.lastErr = msg
			}
			s.githubAppIdentity.mu.Unlock()
		}
		return nil, fmt.Errorf("failed to fetch app info from GitHub: %w", err)
	}

	parsed := githubapp.ParseAppIdentity(raw, now)
	if parsed.AppID == 0 {
		parsed.AppID = appID
	}
	s.githubAppIdentity.mu.Lock()
	if s.githubAppIdentity.gen == startGen {
		s.githubAppIdentity.appID = appID
		s.githubAppIdentity.identity = &parsed
		s.githubAppIdentity.checkedAt = now
		s.githubAppIdentity.lastErr = ""
	}
	s.githubAppIdentity.mu.Unlock()

	cloned := parsed.Clone()
	return &cloned, nil
}

// initGitHubAppIdentity populates the GitHub App identity cache when AppID and
// a private key are already available (§10.2). When startup has no key, the
// cache stays unfetched and lazy-populates on first use once credentials are
// configured.
func (s *Server) initGitHubAppIdentity(ctx context.Context) {
	cfg, hasPrivateKey, _ := s.githubAppCredentialPresence(ctx)
	if cfg.AppID == 0 || !hasPrivateKey {
		return
	}
	if _, err := s.refreshGitHubAppIdentity(ctx); err != nil {
		slog.Warn("GitHub App startup identity fetch failed",
			"app_id", cfg.AppID, "error", upstreamText(err.Error()))
	}
}

// startGitHubAppIdentityInit runs initGitHubAppIdentity asynchronously when
// hub.github_triggers is enabled so an unreachable GitHub/GHES endpoint never
// delays listener bind on startup. When hub.github_triggers is off at startup,
// ensureGitHubAppIdentity lazily fetches on first use if the experiment is
// later enabled at runtime.
func (s *Server) startGitHubAppIdentityInit(ctx context.Context) {
	if !s.githubTriggersEnabled() {
		return
	}
	st := &s.githubAppIdentity
	runCtx, cancel := context.WithCancel(ctx)
	st.startupMu.Lock()
	if st.startupStopped || st.startupCancel != nil {
		st.startupMu.Unlock()
		cancel()
		return
	}
	st.startupCancel = cancel
	st.startupWG.Add(1)
	st.startupMu.Unlock()

	go func() {
		defer st.startupWG.Done()
		defer cancel()
		defer func() {
			if r := recover(); r != nil {
				slog.Error("GitHub App startup identity fetch: recovered from panic", "panic", fmt.Sprint(r))
			}
		}()
		s.initGitHubAppIdentity(runCtx)
	}()
}

// stopGitHubAppIdentityInit cancels any in-flight startup identity fetch and
// waits for its goroutine to exit.
func (s *Server) stopGitHubAppIdentityInit() {
	st := &s.githubAppIdentity
	st.startupMu.Lock()
	st.startupStopped = true
	cancel := st.startupCancel
	st.startupMu.Unlock()
	if cancel != nil {
		cancel()
	}
	st.startupWG.Wait()
}

func githubAppIdentityCacheUsable(id *githubapp.AppIdentity, checkedAt, now time.Time) bool {
	if checkedAt.IsZero() {
		return false
	}
	if id != nil {
		return true
	}
	return now.Sub(checkedAt) < githubAppIdentityFailureRetryBackoff
}

// ensureGitHubAppIdentity returns the cached AppIdentity snapshot, performing a
// lazy GET /app fetch if the cache has never been populated for the current
// AppID (or if a previous failed fetch has passed the bounded retry backoff)
// and credentials are available.
func (s *Server) ensureGitHubAppIdentity(ctx context.Context) (*githubapp.AppIdentity, time.Time, string) {
	cfg, hasPrivateKey, _ := s.githubAppCredentialPresence(ctx)
	if cfg.AppID == 0 {
		return nil, time.Time{}, ""
	}
	now := s.githubAppIdentity.now().UTC()
	if id, checkedAt, lastErr := s.githubAppIdentity.snapshot(cfg.AppID); githubAppIdentityCacheUsable(id, checkedAt, now) {
		return id, checkedAt, lastErr
	}
	if !hasPrivateKey {
		return nil, time.Time{}, ""
	}

	s.githubAppIdentity.refreshMu.Lock()
	defer s.githubAppIdentity.refreshMu.Unlock()
	// Retry once if a concurrent App config reset (generation counter bump)
	// discarded the in-flight result, leaving checkedAt zero. Failed GET /app
	// calls record checkedAt and are cached for githubAppIdentityFailureRetryBackoff
	// before the next rate-limited retry.
	for attempt := 0; attempt < 2; attempt++ {
		cfg, hasPrivateKey, _ = s.githubAppCredentialPresence(ctx)
		if cfg.AppID == 0 || !hasPrivateKey {
			return nil, time.Time{}, ""
		}
		now = s.githubAppIdentity.now().UTC()
		if id, checkedAt, lastErr := s.githubAppIdentity.snapshot(cfg.AppID); githubAppIdentityCacheUsable(id, checkedAt, now) {
			return id, checkedAt, lastErr
		}
		if _, err := s.refreshGitHubAppIdentityLocked(ctx); err != nil {
			slog.Warn("GitHub App lazy identity fetch failed",
				"app_id", cfg.AppID, "error", upstreamText(err.Error()))
		}
		now = s.githubAppIdentity.now().UTC()
		if id, checkedAt, lastErr := s.githubAppIdentity.snapshot(cfg.AppID); githubAppIdentityCacheUsable(id, checkedAt, now) || ctx.Err() != nil {
			return id, checkedAt, lastErr
		}
	}
	cfg, _, _ = s.githubAppCredentialPresence(ctx)
	return s.githubAppIdentity.snapshot(cfg.AppID)
}

// GitHubAppIdentity returns a copy of the cached GitHub App identity, lazily
// fetching it from GET /app if never fetched and credentials exist.
func (s *Server) GitHubAppIdentity(ctx context.Context) (*githubapp.AppIdentity, bool) {
	id, _, _ := s.ensureGitHubAppIdentity(ctx)
	if id == nil {
		return nil, false
	}
	return id, true
}

// GitHubAppBotLogin returns the GitHub App's "<slug>[bot]" login for the
// own-App webhook guard (§4.1 step 6, §5.7), honoring the configured app_slug
// override when set and falling back to the cached GET /app slug.
func (s *Server) GitHubAppBotLogin(ctx context.Context) string {
	s.mu.RLock()
	override := normalizeGitHubMentionHandle(s.config.GitHubAppConfig.AppSlug)
	s.mu.RUnlock()
	if override != "" {
		return override + "[bot]"
	}
	if id, ok := s.GitHubAppIdentity(ctx); ok && id.BotLogin != "" {
		return id.BotLogin
	}
	return ""
}

// peekGitHubAppMentionIdentities reads the configured app_slug override or
// already-cached GET /app slug and mention aliases from memory without making
// any network/store call or emitting an unavailable-slug warning.
func (s *Server) peekGitHubAppMentionIdentities() (slug string, aliases []string, ok bool) {
	s.mu.RLock()
	appID := s.config.GitHubAppConfig.AppID
	override := normalizeGitHubMentionHandle(s.config.GitHubAppConfig.AppSlug)
	rawAliases := append([]string(nil), s.config.GitHubAppConfig.CommandMentionAliases...)
	s.mu.RUnlock()

	aliases = normalizeGitHubMentionAliases(rawAliases)
	if override != "" {
		return override, aliases, true
	}
	if appID > 0 {
		if id, _, _ := s.githubAppIdentity.snapshot(appID); id != nil && id.Slug != "" {
			return id.Slug, aliases, true
		}
	}
	return "", aliases, false
}

// GitHubAppMentionIdentities returns the canonical App mention slug and any
// configured command mention aliases (without leading '@') for Stage 3 command
// detection (§5.6, §10.2). When neither app_slug nor a fetched GET /app slug
// is available, ok is false and a warning is logged at most once per hour.
func (s *Server) GitHubAppMentionIdentities(ctx context.Context) (slug string, aliases []string, ok bool) {
	s.mu.RLock()
	override := normalizeGitHubMentionHandle(s.config.GitHubAppConfig.AppSlug)
	rawAliases := append([]string(nil), s.config.GitHubAppConfig.CommandMentionAliases...)
	s.mu.RUnlock()

	aliases = normalizeGitHubMentionAliases(rawAliases)
	if override != "" {
		return override, aliases, true
	}
	if id, hasID := s.GitHubAppIdentity(ctx); hasID && id.Slug != "" {
		return id.Slug, aliases, true
	}
	if s.githubAppIdentity.shouldWarnSlugUnavailable(s.githubAppIdentity.now().UTC()) {
		slog.Warn("GitHub App slug unavailable; skipping command mention detection until GET /app succeeds or server.github_app.app_slug is set")
	}
	return "", aliases, false
}

func normalizeGitHubMentionHandle(raw string) string {
	return strings.TrimPrefix(strings.TrimSpace(raw), "@")
}

func normalizeGitHubMentionAliases(raw []string) []string {
	out := make([]string, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for _, item := range raw {
		clean := normalizeGitHubMentionHandle(item)
		if clean == "" {
			continue
		}
		lower := strings.ToLower(clean)
		if _, dup := seen[lower]; dup {
			continue
		}
		seen[lower] = struct{}{}
		out = append(out, clean)
	}
	return out
}

func githubPermissionSatisfies(appLevel, requiredLevel string) bool {
	switch requiredLevel {
	case "read":
		return appLevel == "read" || appLevel == "write" || appLevel == "admin"
	case "write":
		return appLevel == "write" || appLevel == "admin"
	default:
		return appLevel == requiredLevel
	}
}

const githubAppNotConfiguredError = "GitHub App is not configured."

// computeGitHubTriggersReadiness evaluates Stage 3 trigger readiness from the
// App identity cache and webhook configuration (§10.1, §10.2).
func (s *Server) computeGitHubTriggersReadiness(ctx context.Context, cfg GitHubAppServerConfig, hasWebhookSecret bool) GitHubTriggersReadiness {
	id, checkedAt, lastErr := s.ensureGitHubAppIdentity(ctx)
	if cfg.AppID == 0 || (id == nil && lastErr == "") {
		lastErr = githubAppNotConfiguredError
	}

	appSlug := normalizeGitHubMentionHandle(cfg.AppSlug)
	if appSlug == "" && id != nil {
		appSlug = id.Slug
	}

	missingEvents := []string{}
	missingPerms := []string{}
	if id != nil {
		eventSet := make(map[string]bool, len(id.Events))
		for _, ev := range id.Events {
			eventSet[ev] = true
		}
		for _, reqEv := range githubTriggersRequiredEvents {
			if !eventSet[reqEv] {
				missingEvents = append(missingEvents, reqEv)
			}
		}
		for _, reqPerm := range githubTriggersRequiredPermissions {
			if !githubPermissionSatisfies(id.Permissions[reqPerm.name], reqPerm.level) {
				missingPerms = append(missingPerms, reqPerm.name+":"+reqPerm.level)
			}
		}
	}

	ready := cfg.WebhooksEnabled &&
		hasWebhookSecret &&
		id != nil &&
		lastErr == "" &&
		len(missingEvents) == 0 &&
		len(missingPerms) == 0

	var checkedAtPtr *time.Time
	if !checkedAt.IsZero() {
		t := checkedAt
		checkedAtPtr = &t
	}

	return GitHubTriggersReadiness{
		Ready:              ready,
		WebhooksEnabled:    cfg.WebhooksEnabled,
		HasWebhookSecret:   hasWebhookSecret,
		MissingEvents:      missingEvents,
		MissingPermissions: missingPerms,
		AppSlug:            appSlug,
		CheckedAt:          checkedAtPtr,
		Error:              lastErr,
	}
}

// GitHubTriggersProjectReadiness computes the project-member view of GitHub App
// trigger readiness (§10.2), omitting hub-level webhook config booleans.
func (s *Server) GitHubTriggersProjectReadiness(ctx context.Context) GitHubTriggersProjectReadiness {
	cfg, _, hasWebhookSecret := s.githubAppCredentialPresence(ctx)
	full := s.computeGitHubTriggersReadiness(ctx, cfg, hasWebhookSecret)
	return GitHubTriggersProjectReadiness{
		Ready:              full.Ready,
		MissingEvents:      full.MissingEvents,
		MissingPermissions: full.MissingPermissions,
		AppSlug:            full.AppSlug,
		CheckedAt:          full.CheckedAt,
		Error:              full.Error,
	}
}
