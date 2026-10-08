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
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Integration health values used by the health summary in addition to the
// values a plugin reports itself (healthy, degraded, unhealthy).
const healthIntegrationUnknown = "unknown"

// healthIntegrationNotManagedReason is the fixed reason given for a plugin
// that has a hub record but is not run by the serving hub instance.
const healthIntegrationNotManagedReason = "not managed by this hub instance"

// HealthSummaryIntegration is one chat or messaging plugin in the health
// summary. Its fields are an explicit allow-list: the plugin's own health
// message and details are never copied, because hub.health.read is a
// narrower permission than the integrations admin surface.
type HealthSummaryIntegration struct {
	Name     string `json:"name"`
	Platform string `json:"platform"`
	// Health is the plugin's reported health (healthy, degraded,
	// unhealthy) or "unknown" when it could not be queried.
	Health    string `json:"health"`
	Connected bool   `json:"connected"`
	Version   string `json:"version"`
	// Reason is a fixed, server-composed explanation, set only when the
	// health could not be read for a known cause.
	Reason string `json:"reason,omitempty"`
}

// healthIntegrationQueryTimeout bounds how long the health summary waits
// for one plugin's health. A variable so tests can lower it.
var healthIntegrationQueryTimeout = 2 * time.Second

// healthIntegrationTimedOutReason is the fixed reason given for a managed
// plugin whose health did not arrive within healthIntegrationQueryTimeout.
const healthIntegrationTimedOutReason = "health not reported in time"

// HealthSummaryIntegrationCounts is the non-identifying aggregate of the
// integrations section. It is returned to every caller of the summary,
// including callers that may not see integration names.
type HealthSummaryIntegrationCounts struct {
	Total     int `json:"total"`
	Healthy   int `json:"healthy"`
	Degraded  int `json:"degraded"`
	Unhealthy int `json:"unhealthy"`
	// Unknown counts integrations whose health was not reported.
	Unknown int `json:"unknown"`
}

// healthSummaryIntegrations builds the integrations section of the health
// summary. Plugins run by this hub instance's plugin manager are queried
// with getIntegrationStatus, the same live check the Integrations admin
// page uses, all at once, each bounded by healthIntegrationQueryTimeout: a
// plugin that does not answer in time is listed with health "unknown" (not
// reported) and a fixed reason, and is not queried again until its earlier
// query returns. pluginRecordNames are the plugin names of the plugin
// records in the runtime broker table (from the handler's broker pass);
// those this instance does not run are listed with health "unknown" and a
// fixed reason. The list is sorted by name and is never nil.
func (s *Server) healthSummaryIntegrations(ctx context.Context, pluginRecordNames []string) []HealthSummaryIntegration {
	s.mu.RLock()
	mgr := s.pluginManager
	s.mu.RUnlock()

	out := []HealthSummaryIntegration{}
	managed := map[string]bool{}
	if mgr != nil {
		var names []string
		for _, key := range mgr.ListPlugins() {
			name := pluginNameFromKey(key)
			if name == "" || managed[name] {
				continue
			}
			managed[name] = true
			names = append(names, name)
		}
		out = append(out, s.queryHealthSummaryIntegrations(ctx, mgr, names)...)
	}

	for _, name := range pluginRecordNames {
		if managed[name] {
			continue
		}
		managed[name] = true
		out = append(out, HealthSummaryIntegration{
			Name:     name,
			Platform: resolvePlatform(name),
			Health:   healthIntegrationUnknown,
			Reason:   healthIntegrationNotManagedReason,
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// queryHealthSummaryIntegrations queries the named plugins concurrently and
// waits at most healthIntegrationQueryTimeout (or until ctx ends) for all
// of them. The plugin manager calls take no context, so a query that
// overruns keeps running in its goroutine; healthIntegrationInflight marks
// the plugin until it returns, and later polls report it as not reported
// without starting another query. The result has one row per name, in
// name order.
func (s *Server) queryHealthSummaryIntegrations(ctx context.Context, mgr IntegrationManager, names []string) []HealthSummaryIntegration {
	type result struct {
		i   int
		row HealthSummaryIntegration
	}
	rows := make([]HealthSummaryIntegration, len(names))
	done := make([]bool, len(names))
	results := make(chan result, len(names))
	pending := 0
	for i, name := range names {
		rows[i] = HealthSummaryIntegration{
			Name:     name,
			Platform: resolvePlatform(name),
			Health:   healthIntegrationUnknown,
			Reason:   healthIntegrationTimedOutReason,
		}
		if _, busy := s.healthIntegrationInflight.LoadOrStore(name, struct{}{}); busy {
			continue
		}
		pending++
		go func(i int, name string) {
			defer s.healthIntegrationInflight.Delete(name)
			results <- result{i: i, row: healthSummaryIntegrationFromStatus(name, getIntegrationStatus(mgr, name))}
		}(i, name)
	}
	if pending > 0 {
		timer := time.NewTimer(healthIntegrationQueryTimeout)
		defer timer.Stop()
	wait:
		for pending > 0 {
			select {
			case r := <-results:
				rows[r.i] = r.row
				done[r.i] = true
				pending--
			case <-timer.C:
				break wait
			case <-ctx.Done():
				break wait
			}
		}
	}
	for i, name := range names {
		if !done[i] {
			slog.Warn("health summary: integration health not reported in time", "integration", name)
		}
	}
	return rows
}

// healthSummaryIntegrationFromStatus copies the allow-listed fields of a
// live integration status. Message and Details are deliberately dropped.
func healthSummaryIntegrationFromStatus(name string, st *IntegrationStatus) HealthSummaryIntegration {
	row := HealthSummaryIntegration{
		Name:     name,
		Platform: resolvePlatform(name),
		Health:   healthIntegrationUnknown,
	}
	if st == nil {
		return row
	}
	if st.Health != "" {
		row.Health = st.Health
	}
	row.Connected = st.Connected
	row.Version = st.Version
	return row
}

// pluginRecordName returns the plugin name of a plugin record in the
// runtime broker table: its plugin label, else its name without the
// "plugin-" prefix.
func pluginRecordName(b *store.RuntimeBroker) string {
	if name := b.Labels[pluginBrokerLabel]; name != "" {
		return name
	}
	return strings.TrimPrefix(b.Name, "plugin-")
}

// healthSummaryIntegrationCounts aggregates the integrations by health.
func healthSummaryIntegrationCounts(list []HealthSummaryIntegration) HealthSummaryIntegrationCounts {
	c := HealthSummaryIntegrationCounts{Total: len(list)}
	for _, it := range list {
		switch it.Health {
		case HealthStatusHealthy:
			c.Healthy++
		case HealthStatusDegraded:
			c.Degraded++
		case HealthStatusUnhealthy:
			c.Unhealthy++
		default:
			c.Unknown++
		}
	}
	return c
}

// omitHealthSummaryIntegrationDetail removes all integration identity from
// a summary, for a caller without hub.integrations.read. The integrations
// list becomes empty and the integration attention items are replaced, at
// the position of the first one, by aggregate items built only from
// IntegrationCounts ("N integrations unhealthy"). What is left matches the
// response on a hub with no integrations, except for the counts and the
// status, which hub.health.read covers.
func omitHealthSummaryIntegrationDetail(resp *HealthSummaryResponse) {
	resp.Integrations = []HealthSummaryIntegration{}
	resp.IntegrationsDetail = false

	var aggregate []HealthAttentionItem
	subject := HealthAttentionSubject{Type: HealthSubjectIntegration}
	if n := resp.IntegrationCounts.Unhealthy; n > 0 {
		aggregate = append(aggregate, HealthAttentionItem{
			Severity: HealthAttentionWarning, Kind: HealthAttentionIntegration, Subject: subject,
			Message: pluralCount(n, "integration", "integrations") + " unhealthy",
		})
	}
	if n := resp.IntegrationCounts.Degraded; n > 0 {
		aggregate = append(aggregate, HealthAttentionItem{
			Severity: HealthAttentionWarning, Kind: HealthAttentionIntegration, Subject: subject,
			Message: pluralCount(n, "integration", "integrations") + " degraded",
		})
	}

	out := make([]HealthAttentionItem, 0, len(resp.Attention)+len(aggregate))
	inserted := false
	for _, it := range resp.Attention {
		if it.Kind != HealthAttentionIntegration {
			out = append(out, it)
			continue
		}
		if !inserted {
			out = append(out, aggregate...)
			inserted = true
		}
	}
	resp.Attention = out
}

// healthSummaryIntegrationsRoute is the Integrations admin route. Its
// route metadata names the permission (hub.integrations.read) a caller of
// the health summary needs to see integration identity.
const healthSummaryIntegrationsRoute = "/api/v1/admin/integrations"

// healthSummaryCanReadIntegrations reports whether the caller may see
// integration identity (names, platforms, versions) in the health summary:
// it runs the same decision the route guard runs for the Integrations admin
// route, from that route's metadata. It fails closed: no authorization
// service, no user identity, or a misconfigured route means false.
func (s *Server) healthSummaryCanReadIntegrations(r *http.Request) bool {
	meta, ok := routeMetadataTable[healthSummaryIntegrationsRoute]
	if !ok || meta.Permission == "" || meta.Resource == "" || meta.Action == "" || s.authzService == nil {
		return false
	}
	ctx := r.Context()
	user, ok := GetIdentityFromContext(ctx).(UserIdentity)
	if !ok || user == nil {
		return false
	}
	if meta.SessionOnly != "" && !sessionCredentialAllowed(ctx) {
		return false
	}
	target, evidence, ok := routeGuardTarget(meta)
	if !ok {
		return false
	}
	decision := s.authzService.Decide(ctx, AuthzRequest{
		Principal:      principalContextForIdentity(user),
		Credential:     credentialContextForIdentity(user),
		Resource:       target,
		Action:         Action(meta.Action),
		Permission:     meta.Permission,
		TargetEvidence: evidence,
	})
	return decision.Allowed
}
