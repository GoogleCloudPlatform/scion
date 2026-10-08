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

// healthIntegrationQueryTimeout bounds how long one health summary waits
// for all managed plugins' health, in total. A variable so tests can lower
// it.
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
// page uses, all at once, waiting at most healthIntegrationQueryTimeout in
// total: a plugin that does not answer in time is listed with health
// "unknown" (not reported) and a fixed reason; concurrent summaries share
// one query per plugin. pluginRecordNames are the plugin names of the plugin
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

// integrationHealthFlight is one running health query for a plugin. row
// is set before done is closed.
type integrationHealthFlight struct {
	done chan struct{}
	row  HealthSummaryIntegration
}

// integrationHealthQuery returns the running health query for the named
// plugin, starting one if none is running. The query runs in one goroutine
// that ends when the plugin answers; it then drops itself from
// healthIntegrationFlights, so the next caller starts a fresh query.
// Callers only wait on done, so a caller that gives up leaves nothing
// behind.
func (s *Server) integrationHealthQuery(mgr IntegrationManager, name string) *integrationHealthFlight {
	s.healthIntegrationMu.Lock()
	defer s.healthIntegrationMu.Unlock()
	if f, ok := s.healthIntegrationFlights[name]; ok {
		return f
	}
	if s.healthIntegrationFlights == nil {
		s.healthIntegrationFlights = map[string]*integrationHealthFlight{}
	}
	f := &integrationHealthFlight{done: make(chan struct{})}
	s.healthIntegrationFlights[name] = f
	go func() {
		defer func() {
			// A panicking plugin call must not take the hub down or
			// leave the flight open: report the plugin as unknown.
			if r := recover(); r != nil {
				slog.Error("health summary: integration health query panicked", "integration", name, "panic", r)
				f.row = healthSummaryIntegrationFromStatus(name, nil)
			}
			s.healthIntegrationMu.Lock()
			delete(s.healthIntegrationFlights, name)
			s.healthIntegrationMu.Unlock()
			close(f.done)
		}()
		f.row = healthSummaryIntegrationFromStatus(name, getIntegrationStatus(mgr, name))
	}()
	return f
}

// queryHealthSummaryIntegrations reads the health of the named plugins,
// waiting at most healthIntegrationQueryTimeout (or until ctx ends) in
// total. Queries are shared per plugin (integrationHealthQuery): a caller
// that arrives while a query for the same plugin is running waits for that
// query's result, with its own deadline, instead of starting another. The
// plugin manager calls take no context, so a hung plugin keeps its one
// query running; every caller meanwhile reports it as not reported when
// its own deadline passes. The caller starts no goroutine of its own. The
// result has one row per name, in name order.
func (s *Server) queryHealthSummaryIntegrations(ctx context.Context, mgr IntegrationManager, names []string) []HealthSummaryIntegration {
	rows := make([]HealthSummaryIntegration, len(names))
	flights := make([]*integrationHealthFlight, len(names))
	for i, name := range names {
		rows[i] = HealthSummaryIntegration{
			Name:     name,
			Platform: resolvePlatform(name),
			Health:   healthIntegrationUnknown,
			Reason:   healthIntegrationTimedOutReason,
		}
		flights[i] = s.integrationHealthQuery(mgr, name)
	}
	if len(names) == 0 {
		return rows
	}
	timer := time.NewTimer(healthIntegrationQueryTimeout)
	defer timer.Stop()
	expired := false
	for i, f := range flights {
		if !expired {
			select {
			case <-f.done:
				rows[i] = f.row
				continue
			case <-timer.C:
				expired = true
			case <-ctx.Done():
				expired = true
			}
		}
		// Past the deadline: take only results that are already in.
		select {
		case <-f.done:
			rows[i] = f.row
		default:
			slog.Warn("health summary: integration health not reported in time", "integration", names[i])
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
// whether the route guard would allow the caller on the Integrations admin
// route. It uses the guard's own decision (routePermissionDecision) with
// that route's metadata, so the two cannot disagree, and fails closed when
// the route is missing.
func (s *Server) healthSummaryCanReadIntegrations(r *http.Request) bool {
	meta, ok := routeMetadataTable[healthSummaryIntegrationsRoute]
	if !ok || meta.Classification != RouteHubAdmin || meta.Permission == "" {
		return false
	}
	return s.routePermissionDecision(r, meta).outcome == routePermissionAllowed
}
