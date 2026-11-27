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
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// startupLayer1Values holds the startup (bootstrap) values of the Layer-1
// settings whose saved value can be cleared. ApplySnapshot returns to these
// values when a snapshot leaves the setting unset, so clearing a saved key
// reverts the running hub to what it would use after a restart rather than
// keeping the last saved value (ptone/scion#3904).
//
// New records them from ServerConfig before any snapshot is applied. A
// Server built without New (tests) has zero startup values.
type startupLayer1Values struct {
	SoftDeleteRetention    time.Duration
	SoftDeleteRetainFiles  bool
	TelemetryDefault       *bool
	AutoExposePortsDefault *bool
	TelemetryConfig        *api.TelemetryConfig
	// TelemetryProjectID is the startup metrics dashboard GCP project ID
	// (from settings.yaml or the scion-telemetry-gcp-credentials secret).
	TelemetryProjectID string
	// GitHubApp holds the non-secret GitHub App fields only. The private
	// key and webhook secret are never part of a snapshot.
	GitHubApp githubAppPublicConfig
	// AuthorizedDomains is the sign-in domain list from settings.yaml and
	// SCION_SERVER_* (ptone/scion#6000).
	AuthorizedDomains []string
	// Branding is the Hub-wide UI branding from startup config.
	Branding *config.BrandingConfig
	// Uploads holds the web upload limits from startup config.
	Uploads config.HubUploadsConfig
}

// githubAppPublicConfig is the part of GitHubAppServerConfig that the
// github_app settings section owns.
type githubAppPublicConfig struct {
	AppID                 int64
	APIBaseURL            string
	WebhooksEnabled       bool
	InstallationURL       string
	PrivateKeyPath        string
	AppSlug               string
	CommandMentionAliases []string
}

func (a githubAppPublicConfig) equal(b githubAppPublicConfig) bool {
	return a.AppID == b.AppID &&
		a.APIBaseURL == b.APIBaseURL &&
		a.WebhooksEnabled == b.WebhooksEnabled &&
		a.InstallationURL == b.InstallationURL &&
		a.PrivateKeyPath == b.PrivateKeyPath &&
		a.AppSlug == b.AppSlug &&
		slices.Equal(a.CommandMentionAliases, b.CommandMentionAliases)
}

func githubAppPublicFrom(c GitHubAppServerConfig) githubAppPublicConfig {
	return githubAppPublicConfig{
		AppID:                 c.AppID,
		APIBaseURL:            c.APIBaseURL,
		WebhooksEnabled:       c.WebhooksEnabled,
		InstallationURL:       c.InstallationURL,
		PrivateKeyPath:        c.PrivateKeyPath,
		AppSlug:               c.AppSlug,
		CommandMentionAliases: slices.Clone(c.CommandMentionAliases),
	}
}

// setGitHubAppPublicLocked writes the non-secret GitHub App fields into
// s.config. It reports whether anything changed. Callers hold s.mu.
func (s *Server) setGitHubAppPublicLocked(g githubAppPublicConfig) bool {
	prev := githubAppPublicFrom(s.config.GitHubAppConfig)
	if prev.equal(g) {
		return false
	}
	s.config.GitHubAppConfig.AppID = g.AppID
	s.config.GitHubAppConfig.APIBaseURL = g.APIBaseURL
	s.config.GitHubAppConfig.WebhooksEnabled = g.WebhooksEnabled
	s.config.GitHubAppConfig.InstallationURL = g.InstallationURL
	s.config.GitHubAppConfig.PrivateKeyPath = g.PrivateKeyPath
	s.config.GitHubAppConfig.AppSlug = g.AppSlug
	s.config.GitHubAppConfig.CommandMentionAliases = slices.Clone(g.CommandMentionAliases)
	if prev.AppID != g.AppID || prev.APIBaseURL != g.APIBaseURL || prev.PrivateKeyPath != g.PrivateKeyPath {
		s.githubAppIdentity.reset()
	}
	return true
}

// recordStartupLayer1Values captures the startup values ApplySnapshot
// reverts to. Called once by New, before any snapshot is applied.
func (s *Server) recordStartupLayer1Values() {
	s.startupLayer1 = startupLayer1Values{
		SoftDeleteRetention:    s.config.SoftDeleteRetention,
		SoftDeleteRetainFiles:  s.config.SoftDeleteRetainFiles,
		TelemetryDefault:       copyBoolPtr(s.config.TelemetryDefault),
		AutoExposePortsDefault: copyBoolPtr(s.config.AutoExposePortsDefault),
		TelemetryConfig:        s.config.TelemetryConfig,
		TelemetryProjectID:     s.config.TelemetryProjectID,
		GitHubApp:              githubAppPublicFrom(s.config.GitHubAppConfig),
		AuthorizedDomains:      slices.Clone(s.config.AuthorizedDomains),
		Branding:               s.config.Branding.Clone(),
		Uploads:                s.config.Uploads,
	}
}

// ResolvedUploads returns the current effective web upload limits (from live
// Layer-1 operational settings section "uploads", falling back to startup
// config). Safe for concurrent request handlers.
func (s *Server) ResolvedUploads() config.HubUploadsConfig {
	s.mu.RLock()
	u := s.config.Uploads
	s.mu.RUnlock()
	return u
}

// telemetryGCPProjectFromAPI returns the explicit telemetry.cloud.gcp_project_id
// value from cfg, trimmed of surrounding whitespace, or "" if unset.
func telemetryGCPProjectFromAPI(cfg *api.TelemetryConfig) string {
	if cfg == nil || cfg.Cloud == nil || cfg.Cloud.GCPProjectID == nil {
		return ""
	}
	return strings.TrimSpace(*cfg.Cloud.GCPProjectID)
}

// effectiveMetricsProjectLocked returns the GCP project ID the metrics
// dashboard should query for telemetryConfig, following the priority chain:
//  1. Explicit telemetry.cloud.gcp_project_id in telemetryConfig
//  2. Startup TelemetryProjectID (settings.yaml or SA credentials secret)
//  3. Hub GCPProjectID fallback
//
// Callers hold s.mu (or run during single-goroutine Server init).
func (s *Server) effectiveMetricsProjectLocked(telemetryConfig *api.TelemetryConfig) string {
	if pid := telemetryGCPProjectFromAPI(telemetryConfig); pid != "" {
		return pid
	}
	if pid := strings.TrimSpace(s.startupLayer1.TelemetryProjectID); pid != "" {
		return pid
	}
	return strings.TrimSpace(s.config.GCPProjectID)
}

// ResolvedBranding returns the running Hub's validated UI branding snapshot.
// Never returns nil; an uninitialized Server returns DefaultResolvedBranding().
func (s *Server) ResolvedBranding() *config.ResolvedBranding {
	if s != nil {
		if r := s.resolvedBranding.Load(); r != nil {
			return r
		}
	}
	return config.DefaultResolvedBranding()
}

// applyBrandingLocked resolves next and updates s.config.Branding and
// s.resolvedBranding, returning true when the effective branding config
// changed. Malformed configs log a warning and fall back to stock Scion
// defaults. Callers hold s.mu (or run during single-goroutine Server init).
func (s *Server) applyBrandingLocked(next *config.BrandingConfig) bool {
	cloned := next.Clone()
	changed := !reflect.DeepEqual(s.config.Branding, cloned)
	s.config.Branding = cloned
	resolved, errs := config.ResolveBranding(cloned)
	if len(errs) > 0 {
		logging.Subsystem("hub.settings").Warn("invalid server.hub.branding configuration; falling back to default Scion branding",
			"errors", errs)
	}
	s.resolvedBranding.Store(resolved)
	return changed
}

// softDeleteSettings returns the running soft-delete retention and the
// retain-files switch. Both are Layer-1 settings that ApplySnapshot
// rewrites while the hub runs, so every consumer reads them here, under
// s.mu, rather than from a value captured at startup.
func (s *Server) softDeleteSettings() (retention time.Duration, retainFiles bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.config.SoftDeleteRetention, s.config.SoftDeleteRetainFiles
}

// notificationChannelConfigs converts the settings form of the
// notification channels to the registry form.
func notificationChannelConfigs(channels []config.V1NotificationChannelConfig) []ChannelConfig {
	out := make([]ChannelConfig, len(channels))
	for i, c := range channels {
		out[i] = ChannelConfig{
			Type:             c.Type,
			Params:           c.Params,
			FilterTypes:      c.FilterTypes,
			FilterUrgentOnly: c.FilterUrgentOnly,
		}
	}
	return out
}

// NewChannelRegistryFromSettings builds a notification channel registry from
// the settings form of the channels. It returns nil when there are none.
func NewChannelRegistryFromSettings(channels []config.V1NotificationChannelConfig) *ChannelRegistry {
	r, _ := newChannelRegistryFromSettings(channels)
	return r
}

// newChannelRegistryFromSettings is NewChannelRegistryFromSettings that also
// returns the indexes of the channels it skipped (ptone/scion#4466).
func newChannelRegistryFromSettings(channels []config.V1NotificationChannelConfig) (*ChannelRegistry, []int) {
	if len(channels) == 0 {
		return nil, nil
	}
	return newChannelRegistry(notificationChannelConfigs(channels), logging.Subsystem("hub.notification-channels"))
}

// currentChannelRegistry returns the notification channel registry in use,
// or nil. The registry is immutable once built; ApplySnapshot replaces it
// as a whole (SetChannelRegistry), so a caller may dispatch on the value it
// got while a new registry is swapped in.
func (s *Server) currentChannelRegistry() *ChannelRegistry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.channelRegistry
}

// notificationChannelsApplyState records the notification channels the
// running registry was last built from, and the indexes of those it
// skipped because they failed validation.
type notificationChannelsApplyState struct {
	mu       sync.Mutex
	applied  bool
	channels []config.V1NotificationChannelConfig
	skipped  []int
}

// applyNotificationChannels rebuilds the notification channel registry from
// channels and swaps it in, unless the registry was already built from the
// same list. The new registry is complete before the swap, and the old one
// is never changed, so a dispatch in progress is unaffected. An empty list
// removes the registry. It reports whether the registry was replaced.
func (s *Server) applyNotificationChannels(channels []config.V1NotificationChannelConfig) bool {
	if len(channels) == 0 {
		channels = nil
	}
	st := &s.notificationApply
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.applied && reflect.DeepEqual(st.channels, channels) {
		return false
	}
	registry, skipped := newChannelRegistryFromSettings(channels)
	s.SetChannelRegistry(registry)
	st.applied = true
	st.channels = append([]config.V1NotificationChannelConfig(nil), channels...)
	st.skipped = skipped
	return true
}

// Reasons a federation configuration was not installed by ApplySnapshot.
// They name no configured value.
const (
	federationSkipInvalid       = "The federation settings failed validation and are not in use."
	federationSkipBuildFailed   = "The federation settings could not be applied and are not in use."
	federationSkipTestAdminTier = "Federation is not in use while the test admin tier is on."
)

// federationApplyState records why the federation configuration of the
// last ApplySnapshot was not installed; skipReason is empty when it was
// installed or federation is off.
type federationApplyState struct {
	mu         sync.Mutex
	skipReason string
}

func (s *Server) setFederationSkipReason(reason string) {
	s.federationApply.mu.Lock()
	s.federationApply.skipReason = reason
	s.federationApply.mu.Unlock()
}

// layer1ApplySkips is what the last ApplySnapshot did not apply
// (ptone/scion#4466).
type layer1ApplySkips struct {
	// ChannelCount is the number of notification channels last applied,
	// and SkippedChannels the indexes of those that were skipped.
	ChannelCount    int
	SkippedChannels []int
	// FederationReason is set when the federation configuration was not
	// installed.
	FederationReason string
	// AuthorizedDomainsPinned is set when a SCION_SERVER_* variable pins
	// authorized_domains on this node, so a saved list is not applied
	// (ptone/scion#6000).
	AuthorizedDomainsPinned bool
}

// layer1ApplySkips returns the items the last ApplySnapshot skipped.
func (s *Server) layer1ApplySkips() layer1ApplySkips {
	var out layer1ApplySkips
	st := &s.notificationApply
	st.mu.Lock()
	if st.applied {
		out.ChannelCount = len(st.channels)
		out.SkippedChannels = append([]int(nil), st.skipped...)
	}
	st.mu.Unlock()
	s.federationApply.mu.Lock()
	out.FederationReason = s.federationApply.skipReason
	s.federationApply.mu.Unlock()
	if ops := s.GetOperationalSettings(); ops != nil {
		out.AuthorizedDomainsPinned = slices.Contains(ops.EnvOverriddenKeys(), authorizedDomainsKey)
	}
	return out
}
