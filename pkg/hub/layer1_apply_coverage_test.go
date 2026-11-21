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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
)

// Every Layer-1 key is either applied to the running hub when it is saved,
// or flagged restart_required in the opsettings registry (ptone/scion#3904).
// layer1LiveProbes holds one probe per live key: the test saves the probe's
// document through OperationalSettings, applies the snapshot as the
// propagation path does, and checks that the value the hub runs with
// changed. A new Layer-1 key with neither a probe nor the flag fails here.

// layer1LiveProbe proves that saving one Layer-1 key changes what the
// running hub uses.
type layer1LiveProbe struct {
	section string
	// base, when set, is saved and applied first; the probe then measures
	// the change from base to doc. Used when a key takes effect only next
	// to another one (a GitHub App field needs an app_id).
	base string
	doc  string
	// prepare runs before doc is applied (for example to approve a GCP
	// permission-check transition, as the save handler does).
	prepare func(t *testing.T, s *Server, snap Layer1Snapshot)
	// observe returns the value the running hub uses for the key.
	observe func(s *Server, o *OperationalSettings) any
}

// layer1NotWritableKeys are Layer-1 koanf paths that no settings save
// writes, so there is nothing to apply or report. Each must stay in
// dbUnwrittenLayer1Paths; when one becomes writable it needs a probe or the
// restart_required flag.
var layer1NotWritableKeys = map[string]string{
	"default_max_agent_role": "accepted only as an unchanged echo of GET (dbUnwrittenLayer1Paths)",
	"default_agent_role":     "accepted only as an unchanged echo of GET (dbUnwrittenLayer1Paths)",
}

func observeLocked[T any](f func(s *Server) T) func(s *Server, _ *OperationalSettings) any {
	return func(s *Server, _ *OperationalSettings) any {
		s.mu.RLock()
		defer s.mu.RUnlock()
		return f(s)
	}
}

func observeTelemetry(s *Server, _ *OperationalSettings) any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, _ := json.Marshal(struct {
		Default any
		Config  any
	}{s.config.TelemetryDefault, s.config.TelemetryConfig})
	return string(b)
}

func observeBranding(s *Server, _ *OperationalSettings) any {
	s.mu.RLock()
	cfg := s.config.Branding.Clone()
	s.mu.RUnlock()
	b, _ := json.Marshal(struct {
		Config *config.BrandingConfig
		Public *config.PublicBranding
	}{cfg, s.ResolvedBranding().Public()})
	return string(b)
}

func observeAgentDefaults(s *Server, _ *OperationalSettings) any {
	b, _ := json.Marshal(s.hubAgentDefaults())
	return string(b)
}

func observeGitHubApp(s *Server, _ *OperationalSettings) any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return githubAppPublicFrom(s.config.GitHubAppConfig)
}

func observeStartClaim(s *Server, _ *OperationalSettings) any {
	return s.startClaimSettings()
}

func observeOverlay(s *Server, _ *OperationalSettings) any {
	vs := &config.VersionedSettings{}
	config.GetGlobalSettingsOverlay().Apply(vs)
	b, _ := json.Marshal(struct {
		R any
		P any
		H any
	}{vs.Runtimes, vs.Profiles, vs.HarnessConfigs})
	return string(b)
}

func observeFederation(s *Server, _ *OperationalSettings) any {
	a := s.federationAuth.Load()
	if a == nil {
		return "none"
	}
	type issuerView struct {
		Raw      config.TrustedIssuerConfig
		Refresh  time.Duration
		Debounce time.Duration
	}
	issuers := map[string]issuerView{}
	for url, e := range a.issuers {
		v := issuerView{Raw: e.rawConfig}
		if e.cache != nil {
			v.Refresh = e.cache.refreshInterval
			v.Debounce = e.cache.debounceInterval
		}
		issuers[url] = v
	}
	b, _ := json.Marshal(struct {
		Algorithms any
		Issuers    any
	}{a.algorithms, issuers})
	return string(b)
}

func approveGCPIAMFromSnapshot(t *testing.T, s *Server, snap Layer1Snapshot) {
	t.Helper()
	s.mu.RLock()
	cur := s.gcpIAMSettingsLocked()
	base := s.gcpIAMBaseLocked()
	s.mu.RUnlock()
	cand, warns := resolveGCPIAMSettings(snap.GCPIAMCheckMode, snap.GCPIAMDenyUnknownPolicy, base)
	if len(warns) > 0 {
		t.Fatalf("gcp_iam probe document is not usable: %v", warns)
	}
	s.approveGCPIAMTransition(gcpIAMTransition{From: cur, To: cand})
}

// telemetryProbeDocs maps each telemetry koanf path to a section document
// that sets it.
var telemetryProbeDocs = map[string]string{
	"telemetry.enabled":                        `{"enabled":true}`,
	"telemetry.cloud":                          `{"cloud":{"endpoint":"otel.example.com:4317"}}`,
	"telemetry.cloud.enabled":                  `{"cloud":{"enabled":true}}`,
	"telemetry.cloud.endpoint":                 `{"cloud":{"endpoint":"otel.example.com:4317"}}`,
	"telemetry.cloud.protocol":                 `{"cloud":{"protocol":"http"}}`,
	"telemetry.cloud.headers":                  `{"cloud":{"headers":{"x-probe":"v"}}}`,
	"telemetry.cloud.provider":                 `{"cloud":{"provider":"gcp"}}`,
	"telemetry.cloud.gcp_project_id":           `{"cloud":{"gcp_project_id":"probe-project"}}`,
	"telemetry.cloud.tls":                      `{"cloud":{"tls":{"enabled":true}}}`,
	"telemetry.cloud.tls.enabled":              `{"cloud":{"tls":{"enabled":true}}}`,
	"telemetry.cloud.tls.insecure_skip_verify": `{"cloud":{"tls":{"insecure_skip_verify":true}}}`,
	"telemetry.cloud.tls.ca_file":              `{"cloud":{"tls":{"ca_file":"/etc/ca.pem"}}}`,
	"telemetry.cloud.tls.cert_file":            `{"cloud":{"tls":{"cert_file":"/etc/client.pem"}}}`,
	"telemetry.cloud.tls.key_file":             `{"cloud":{"tls":{"key_file":"/etc/client-key.pem"}}}`,
	"telemetry.cloud.batch":                    `{"cloud":{"batch":{"max_size":10}}}`,
	"telemetry.cloud.batch.max_size":           `{"cloud":{"batch":{"max_size":10}}}`,
	"telemetry.cloud.batch.timeout":            `{"cloud":{"batch":{"timeout":"7s"}}}`,
	"telemetry.hub":                            `{"hub":{"enabled":true}}`,
	"telemetry.hub.enabled":                    `{"hub":{"enabled":true}}`,
	"telemetry.hub.report_interval":            `{"hub":{"report_interval":"45s"}}`,
	"telemetry.local":                          `{"local":{"enabled":true}}`,
	"telemetry.local.enabled":                  `{"local":{"enabled":true}}`,
	"telemetry.local.file":                     `{"local":{"file":"/tmp/otel.jsonl"}}`,
	"telemetry.local.console":                  `{"local":{"console":true}}`,
	"telemetry.filter":                         `{"filter":{"enabled":true}}`,
	"telemetry.filter.enabled":                 `{"filter":{"enabled":true}}`,
	"telemetry.filter.respect_debug_mode":      `{"filter":{"respect_debug_mode":true}}`,
	"telemetry.filter.events":                  `{"filter":{"events":{"include":["agent.start"]}}}`,
	"telemetry.filter.events.include":          `{"filter":{"events":{"include":["agent.start"]}}}`,
	"telemetry.filter.events.exclude":          `{"filter":{"events":{"exclude":["agent.start"]}}}`,
	"telemetry.filter.attributes":              `{"filter":{"attributes":{"redact":["prompt"]}}}`,
	"telemetry.filter.attributes.redact":       `{"filter":{"attributes":{"redact":["prompt"]}}}`,
	"telemetry.filter.attributes.hash":         `{"filter":{"attributes":{"hash":["user"]}}}`,
	"telemetry.filter.sampling":                `{"filter":{"sampling":{"default":0.5}}}`,
	"telemetry.filter.sampling.default":        `{"filter":{"sampling":{"default":0.5}}}`,
	"telemetry.filter.sampling.rates":          `{"filter":{"sampling":{"rates":{"agent.tool.call":0.25}}}}`,
	"telemetry.resource":                       `{"resource":{"service.namespace":"probe"}}`,
}

// brandingProbeDocs maps each branding koanf path to a valid section document
// that sets it and changes the running branding snapshot.
var brandingProbeDocs = map[string]string{
	"server.hub.branding":                              `{"name":"Probe Brand"}`,
	"server.hub.branding.name":                         `{"name":"Probe Brand"}`,
	"server.hub.branding.short_name":                   `{"short_name":"Probe"}`,
	"server.hub.branding.subtitle":                     `{"subtitle":"Probe Subtitle"}`,
	"server.hub.branding.attribution":                  `{"attribution":"hidden"}`,
	"server.hub.branding.corners":                      `{"corners":"square"}`,
	"server.hub.branding.logo":                         `{"logo":{"light":"https://cdn.example.com/logo.svg"}}`,
	"server.hub.branding.logo.light":                   `{"logo":{"light":"https://cdn.example.com/logo-light.svg"}}`,
	"server.hub.branding.logo.dark":                    `{"logo":{"dark":"https://cdn.example.com/logo-dark.svg"}}`,
	"server.hub.branding.icons":                        `{"icons":{"favicon":"https://cdn.example.com/favicon.svg"}}`,
	"server.hub.branding.icons.favicon":                `{"icons":{"favicon":"https://cdn.example.com/favicon.svg"}}`,
	"server.hub.branding.icons.apple_touch_icon":       `{"icons":{"apple_touch_icon":"https://cdn.example.com/apple.png"}}`,
	"server.hub.branding.icons.icon_192":               `{"icons":{"icon_192":"https://cdn.example.com/192.png"}}`,
	"server.hub.branding.icons.icon_512":               `{"icons":{"icon_512":"https://cdn.example.com/512.png"}}`,
	"server.hub.branding.icons.icon_maskable_512":      `{"icons":{"icon_maskable_512":"https://cdn.example.com/maskable.png"}}`,
	"server.hub.branding.accent_color":                 `{"accent_color":"#0d9488"}`,
	"server.hub.branding.palette":                      `{"palette":{"primary":{"600":"#0f766e"}}}`,
	"server.hub.branding.palette.primary":              `{"palette":{"primary":{"600":"#0f766e"}}}`,
	"server.hub.branding.palette.neutral":              `{"palette":{"neutral":{"900":"#111827"}}}`,
	"server.hub.branding.palette.success":              `{"palette":{"success":{"600":"#15803d"}}}`,
	"server.hub.branding.palette.warning":              `{"palette":{"warning":{"600":"#b45309"}}}`,
	"server.hub.branding.palette.danger":               `{"palette":{"danger":{"600":"#b91c1c"}}}`,
	"server.hub.branding.palette.light":                `{"palette":{"light":{"primary":"#0f766e"}}}`,
	"server.hub.branding.palette.light.bg":             `{"palette":{"light":{"bg":"#fafaf9"}}}`,
	"server.hub.branding.palette.light.bg_subtle":      `{"palette":{"light":{"bg_subtle":"#ffffff"}}}`,
	"server.hub.branding.palette.light.surface":        `{"palette":{"light":{"surface":"#fafaf9"}}}`,
	"server.hub.branding.palette.light.surface_raised": `{"palette":{"light":{"surface_raised":"#fafaf9"}}}`,
	"server.hub.branding.palette.light.text":           `{"palette":{"light":{"text":"#111827"}}}`,
	"server.hub.branding.palette.light.text_secondary": `{"palette":{"light":{"text_secondary":"#334155"}}}`,
	"server.hub.branding.palette.light.text_muted":     `{"palette":{"light":{"text_muted":"#475569"}}}`,
	"server.hub.branding.palette.light.border":         `{"palette":{"light":{"border":"#cbd5e1"}}}`,
	"server.hub.branding.palette.light.primary":        `{"palette":{"light":{"primary":"#0f766e"}}}`,
	"server.hub.branding.palette.light.primary_hover":  `{"palette":{"light":{"primary_hover":"#115e59"}}}`,
	"server.hub.branding.palette.light.primary_active": `{"palette":{"light":{"primary_active":"#134e4a"}}}`,
	"server.hub.branding.palette.light.primary_text":   `{"palette":{"light":{"primary_text":"#f8fafc"}}}`,
	"server.hub.branding.palette.dark":                 `{"palette":{"dark":{"primary":"#2dd4bf"}}}`,
	"server.hub.branding.palette.dark.bg":              `{"palette":{"dark":{"bg":"#020617"}}}`,
	"server.hub.branding.palette.dark.bg_subtle":       `{"palette":{"dark":{"bg_subtle":"#0f172a"}}}`,
	"server.hub.branding.palette.dark.surface":         `{"palette":{"dark":{"surface":"#0f172a"}}}`,
	"server.hub.branding.palette.dark.surface_raised":  `{"palette":{"dark":{"surface_raised":"#1e293b"}}}`,
	"server.hub.branding.palette.dark.text":            `{"palette":{"dark":{"text":"#ffffff"}}}`,
	"server.hub.branding.palette.dark.text_secondary":  `{"palette":{"dark":{"text_secondary":"#e2e8f0"}}}`,
	"server.hub.branding.palette.dark.text_muted":      `{"palette":{"dark":{"text_muted":"#cbd5e1"}}}`,
	"server.hub.branding.palette.dark.border":          `{"palette":{"dark":{"border":"#475569"}}}`,
	"server.hub.branding.palette.dark.primary":         `{"palette":{"dark":{"primary":"#2dd4bf"}}}`,
	"server.hub.branding.palette.dark.primary_hover":   `{"palette":{"dark":{"primary_hover":"#5eead4"}}}`,
	"server.hub.branding.palette.dark.primary_active":  `{"palette":{"dark":{"primary_active":"#99f6e4"}}}`,
	"server.hub.branding.palette.dark.primary_text":    `{"palette":{"dark":{"primary_text":"#020617"}}}`,
}

func layer1LiveProbes(t *testing.T) map[string]layer1LiveProbe {
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"keys":[]}`))
	}))
	t.Cleanup(jwks.Close)
	fedBase := `{"enabled":true,"trusted_issuers":[{"issuer_url":"` + jwks.URL + `","jwks_url":"` + jwks.URL + `","expected_audience":"https://hub.example.com"}]`
	ghBase := `{"app_id":1`

	p := map[string]layer1LiveProbe{
		// access
		"server.hub.admin_emails": {section: "access", doc: `{"admin_emails":["probe@example.com"]}`,
			observe: observeLocked(func(s *Server) any { return s.config.AdminEmails })},
		"server.auth.user_access_mode": {section: "access", doc: `{"user_access_mode":"invite"}`,
			observe: observeLocked(func(s *Server) any { return s.config.UserAccessMode })},
		"server.auth.default_user_role": {section: "access", doc: `{"default_user_role":"viewer"}`,
			observe: observeLocked(func(s *Server) any { return s.config.DefaultUserRole })},
		// ptone/scion#6000: a saved list was stored but not applied.
		"server.auth.authorized_domains": {section: "access", doc: `{"authorized_domains":["probe.example.com"]}`,
			observe: func(s *Server, _ *OperationalSettings) any { return s.AuthorizedDomains() }},

		// lifecycle
		"server.hub.auto_suspend_stalled": {section: "lifecycle", doc: `{"auto_suspend_stalled":true}`,
			observe: observeLocked(func(s *Server) any { return s.config.AutoSuspendStalled })},
		"server.hub.stalled_threshold": {section: "lifecycle", doc: `{"stalled_threshold":"17m"}`,
			observe: observeLocked(func(s *Server) any { return s.config.StalledThreshold })},
		"server.hub.auto_suspend_idle": {section: "lifecycle", doc: `{"auto_suspend_idle":true}`,
			observe: observeLocked(func(s *Server) any { return s.config.AutoSuspendIdle })},
		"server.hub.idle_suspend_after": {section: "lifecycle", doc: `{"idle_suspend_after":"23m"}`,
			observe: observeLocked(func(s *Server) any { return s.config.IdleSuspendAfter })},
		"server.hub.soft_delete_retention": {section: "lifecycle", doc: `{"soft_delete_retention":"3h"}`,
			observe: func(s *Server, _ *OperationalSettings) any { r, _ := s.softDeleteSettings(); return r }},
		"server.hub.soft_delete_retain_files": {section: "lifecycle", doc: `{"soft_delete_retain_files":true}`,
			observe: func(s *Server, _ *OperationalSettings) any { _, f := s.softDeleteSettings(); return f }},
		"server.hub.start_claim_lease_ttl":         {section: "lifecycle", doc: `{"start_claim_lease_ttl":"2m"}`, observe: observeStartClaim},
		"server.hub.start_max_duration":            {section: "lifecycle", doc: `{"start_max_duration":"41m"}`, observe: observeStartClaim},
		"server.hub.start_unconfirmed_hold":        {section: "lifecycle", doc: `{"start_unconfirmed_hold":"20m"}`, observe: observeStartClaim},
		"server.hub.start_create_unconfirmed_hold": {section: "lifecycle", doc: `{"start_create_unconfirmed_hold":"16m30s"}`, observe: observeStartClaim},

		// auto_expose_ports, quotas, agent_secrets, project_defaults
		"auto_expose_ports.enabled": {section: "auto_expose_ports", doc: `{"enabled":true}`,
			observe: func(s *Server, _ *OperationalSettings) any { return s.autoExposePortsDefault() }},
		"quotas.enforce_broker_quotas": {section: "quotas", doc: `{"enforce_broker_quotas":false}`,
			observe: observeLocked(func(s *Server) any { return s.config.EnforceBrokerQuotas })},
		"agent_secrets.user_scope_only": {section: "agent_secrets", doc: `{"user_scope_only":true}`,
			observe: observeLocked(func(s *Server) any { return s.config.AgentSecretsUserScopeOnly })},
		"project_defaults.default_scratchpad": {section: "project_defaults", doc: `{"default_scratchpad":false}`,
			observe: func(_ *Server, o *OperationalSettings) any { return o.ProjectDefaultScratchpad() }},

		// gcp_iam
		"server.hub.gcp_iam_check_mode": {section: "gcp_iam", doc: `{"gcp_iam_check_mode":"enforce"}`,
			prepare: approveGCPIAMFromSnapshot,
			observe: observeLocked(func(s *Server) any { return s.gcpIAMSettingsLocked() })},
		"server.hub.gcp_iam_deny_unknown_policy": {section: "gcp_iam", doc: `{"gcp_iam_deny_unknown_policy":"fail-open"}`,
			prepare: approveGCPIAMFromSnapshot,
			observe: observeLocked(func(s *Server) any { return s.gcpIAMSettingsLocked() })},

		// agent_defaults
		"default_template":                        {section: "agent_defaults", doc: `{"default_template":"probe"}`, observe: observeAgentDefaults},
		"default_harness_config":                  {section: "agent_defaults", doc: `{"default_harness_config":"probe"}`, observe: observeAgentDefaults},
		"default_max_turns":                       {section: "agent_defaults", doc: `{"default_max_turns":7}`, observe: observeAgentDefaults},
		"default_max_model_calls":                 {section: "agent_defaults", doc: `{"default_max_model_calls":7}`, observe: observeAgentDefaults},
		"default_max_duration":                    {section: "agent_defaults", doc: `{"default_max_duration":"2h"}`, observe: observeAgentDefaults},
		"default_resources":                       {section: "agent_defaults", doc: `{"default_resources":{"disk":"10Gi"}}`, observe: observeAgentDefaults},
		"default_model":                           {section: "agent_defaults", doc: `{"default_model":"probe-model"}`, observe: observeAgentDefaults},
		"default_thinking_level":                  {section: "agent_defaults", doc: `{"default_thinking_level":5}`, observe: observeAgentDefaults},
		"default_runtime_broker":                  {section: "agent_defaults", doc: `{"default_runtime_broker":"probe-broker"}`, observe: observeAgentDefaults},
		"default_timezone":                        {section: "agent_defaults", doc: `{"default_timezone":"Europe/Paris"}`, observe: observeAgentDefaults},
		"default_gcp_identity_mode":               {section: "agent_defaults", doc: `{"default_gcp_identity_mode":"block"}`, observe: observeAgentDefaults},
		"default_gcp_identity_service_account_id": {section: "agent_defaults", doc: `{"default_gcp_identity_service_account_id":"sa-probe"}`, observe: observeAgentDefaults},

		// endpoints
		"server.hub.hub_name": {section: "endpoints", doc: `{"hub_name":"probe-hub"}`,
			observe: observeLocked(func(s *Server) any { return s.config.HubName })},
		"image_registry": {section: "endpoints", doc: `{"image_registry":"probe.example.com/scion"}`,
			observe: func(s *Server, _ *OperationalSettings) any { return s.resolveImageRegistry() }},
		"server.hub.monitoring_dashboard_url": {section: "endpoints", doc: `{"monitoring_dashboard_url":"https://monitoring.example.com/d/1"}`,
			observe: observeLocked(func(s *Server) any { return s.config.MonitoringDashboardURL })},
		"server.hub.content_url": {section: "endpoints", doc: `{"content_url":"https://content.example.net"}`,
			observe: func(s *Server, _ *OperationalSettings) any { return s.contentURLLiveState().URL }},
		"server.hub.content_url_allow_same_site": {section: "endpoints", doc: `{"content_url_allow_same_site":true}`,
			observe: func(s *Server, _ *OperationalSettings) any { return s.contentURLAllowSameSiteLiveValue() }},

		// github_app
		"server.github_app":                         {section: "github_app", base: ghBase + `}`, doc: ghBase + `,"api_base_url":"https://ghe.example.com/api/v3"}`, observe: observeGitHubApp},
		"server.github_app.api_base_url":            {section: "github_app", base: ghBase + `}`, doc: ghBase + `,"api_base_url":"https://ghe.example.com/api/v3"}`, observe: observeGitHubApp},
		"server.github_app.webhooks_enabled":        {section: "github_app", base: ghBase + `}`, doc: ghBase + `,"webhooks_enabled":true}`, observe: observeGitHubApp},
		"server.github_app.installation_url":        {section: "github_app", base: ghBase + `}`, doc: ghBase + `,"installation_url":"https://github.com/apps/probe/installations/new"}`, observe: observeGitHubApp},
		"server.github_app.private_key_path":        {section: "github_app", base: ghBase + `}`, doc: ghBase + `,"private_key_path":"/etc/probe.pem"}`, observe: observeGitHubApp},
		"server.github_app.app_slug":                {section: "github_app", base: ghBase + `}`, doc: ghBase + `,"app_slug":"scion-probe"}`, observe: observeGitHubApp},
		"server.github_app.command_mention_aliases": {section: "github_app", base: ghBase + `}`, doc: ghBase + `,"command_mention_aliases":["scion","scion-probe"]}`, observe: observeGitHubApp},

		// notifications
		"server.notification_channels": {section: "notifications",
			doc: `{"notification_channels":[{"type":"webhook","params":{"url":"http://127.0.0.1:9/probe"}}]}`,
			observe: func(s *Server, _ *OperationalSettings) any {
				if r := s.currentChannelRegistry(); r != nil {
					return r.Len()
				}
				return 0
			}},

		// federation
		"server.federation.enabled":           {section: "federation", doc: fedBase + `}`, observe: observeFederation},
		"server.federation.trusted_issuers":   {section: "federation", doc: fedBase + `}`, observe: observeFederation},
		"server.federation.algorithms":        {section: "federation", base: fedBase + `}`, doc: fedBase + `,"algorithms":["ES256"]}`, observe: observeFederation},
		"server.federation.refresh_interval":  {section: "federation", base: fedBase + `}`, doc: fedBase + `,"refresh_interval":"7m"}`, observe: observeFederation},
		"server.federation.debounce_interval": {section: "federation", base: fedBase + `}`, doc: fedBase + `,"debounce_interval":"7s"}`, observe: observeFederation},

		// map-of-objects sections, applied through the settings overlay
		"runtimes":        {section: "runtimes", doc: `{"probe":{"type":"docker"}}`, observe: observeOverlay},
		"profiles":        {section: "profiles", doc: `{"probe":{"runtime":"docker"}}`, observe: observeOverlay},
		"harness_configs": {section: "harness_configs", doc: `{"probe":{"harness":"claude"}}`, observe: observeOverlay},

		// Sections with no koanf paths, read through getters.
		"maintenance": {section: "maintenance", doc: `{"admin_mode":true,"maintenance_message":"probe"}`,
			observe: func(s *Server, _ *OperationalSettings) any { on, msg := s.maintenance.State(); return []any{on, msg} }},
		"messaging": {section: "messaging", doc: `{"conversation_envelope_switch":false}`,
			observe: func(_ *Server, o *OperationalSettings) any { return o.ConversationEnvelopeSwitch() }},
		"experiments": {section: "experiments", doc: `{"overrides":{"web.gcs_links":true}}`,
			observe: func(_ *Server, o *OperationalSettings) any { return o.ExperimentsSnapshot().Overrides }},
		"artifacts": {section: "artifacts", doc: `{"enabled":false}`,
			observe: func(_ *Server, o *OperationalSettings) any { return o.Artifacts() }},
		"profiling": {section: "profiling", doc: `{"readiness_marks":true}`,
			observe: func(_ *Server, o *OperationalSettings) any { return o.ReadinessMarks() }},
		"chat_retention": {section: "chat_retention", doc: `{"enabled":true,"messages_retention_days":30}`,
			observe: func(_ *Server, o *OperationalSettings) any { return o.ChatRetention() }},
		"agent_delegation_policy": {section: "agent_delegation_policy", doc: `{"remove":{"hub":["agent.read"]}}`,
			observe: func(_ *Server, o *OperationalSettings) any {
				p, err := o.AgentDelegationPolicy()
				return []any{p.Removes("hub", "agent.read"), err == nil}
			}},
	}
	for path, doc := range telemetryProbeDocs {
		p[path] = layer1LiveProbe{section: "telemetry", doc: doc, observe: observeTelemetry}
	}
	for path, doc := range brandingProbeDocs {
		p[path] = layer1LiveProbe{section: opsettings.BrandingSectionName, doc: doc, observe: observeBranding}
	}
	return p
}

// layer1CoverageKeys returns every Layer-1 key the coverage test checks:
// each registered koanf path, and the name of each section without koanf
// paths.
func layer1CoverageKeys() []string {
	var keys []string
	for _, sec := range opsettings.Registry {
		if len(sec.KoanfPaths) == 0 {
			keys = append(keys, sec.Name)
			continue
		}
		keys = append(keys, sec.KoanfPaths...)
	}
	sort.Strings(keys)
	return keys
}

func TestLayer1Keys_AppliedLiveOrRestartRequired(t *testing.T) {
	probes := layer1LiveProbes(t)
	notWritable := map[string]bool{}
	for _, p := range dbUnwrittenLayer1Paths {
		notWritable[strings.Join(p, ".")] = true
	}

	covered := map[string]bool{}
	for _, key := range layer1CoverageKeys() {
		covered[key] = true
		_, live := probes[key]
		restart := opsettings.IsRestartRequired(key)
		_, exempt := layer1NotWritableKeys[key]
		switch {
		case live && restart:
			t.Errorf("%s has a live-apply probe and is flagged restart_required; it must be one or the other", key)
		case exempt && (live || restart):
			t.Errorf("%s is listed as not writable but also has a probe or the restart_required flag", key)
		case exempt && !notWritable[key]:
			t.Errorf("%s is listed as not writable but is no longer in dbUnwrittenLayer1Paths; add a live-apply probe or flag it restart_required", key)
		case !live && !restart && !exempt:
			t.Errorf("Layer-1 key %s is neither applied live (no probe in layer1LiveProbes) nor flagged restart_required in the opsettings registry", key)
		}
	}
	for key := range probes {
		if !covered[key] {
			t.Errorf("probe %s is not a registered Layer-1 key", key)
		}
	}
	for _, sec := range opsettings.Registry {
		for _, rr := range sec.RestartRequired {
			if !covered[rr] {
				t.Errorf("section %s flags %s restart_required, but it is not one of its koanf paths", sec.Name, rr)
			}
		}
	}
}

// TestLayer1LiveProbes_SaveChangesRunningValue runs each probe: saving its
// document changes the value the running hub uses, without a restart.
func TestLayer1LiveProbes_SaveChangesRunningValue(t *testing.T) {
	prevOverlay := config.GetGlobalSettingsOverlay()
	t.Cleanup(func() { config.SetGlobalSettingsOverlay(prevOverlay) })

	keys := make([]string, 0)
	probes := layer1LiveProbes(t)
	for k := range probes {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		probe := probes[key]
		t.Run(key, func(t *testing.T) {
			config.SetGlobalSettingsOverlay(config.NewSettingsOverlay())
			ctx := context.Background()
			st := newFakeHubSettingStore()
			ops := NewOperationalSettings(st, emptyKoanf(), emptyKoanf())
			srv := &Server{maintenance: NewMaintenanceState(false, "")}
			srv.config.OIDCConfig.IssuerURL = "https://hub.example.com"
			// The federation probes use a local http issuer, which only a
			// dev-mode hub accepts.
			srv.config.Mode = "dev"
			srv.config.Workstation = true
			srv.federationClient = &http.Client{Timeout: 5 * time.Second}
			srv.SetOperationalSettings(ops)

			apply := func() {
				snap := ops.Snapshot()
				ApplySnapshot(srv, snap)
				ApplyMaintenanceFromSnapshot(srv, snap)
			}
			save := func(doc string) {
				t.Helper()
				if _, err := ops.Update(ctx, probe.section, json.RawMessage(doc), "probe", -1, "managed"); err != nil {
					t.Fatalf("saving %s %s: %v", probe.section, doc, err)
				}
			}

			apply()
			if probe.base != "" {
				save(probe.base)
				apply()
			}
			before := probe.observe(srv, ops)

			save(probe.doc)
			if probe.prepare != nil {
				probe.prepare(t, srv, ops.Snapshot())
			}
			apply()
			after := probe.observe(srv, ops)

			if reflect.DeepEqual(before, after) {
				t.Errorf("saving %s %s did not change the running value (still %#v)", probe.section, probe.doc, after)
			}
		})
	}
}
