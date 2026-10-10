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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Integration health values used by the health summary in addition to the
// values a plugin reports itself (healthy, degraded, unhealthy).
const healthIntegrationUnknown = "unknown"

// healthIntegrationNotRunReason is the fixed reason given for a plugin that
// has a hub record but that no live hub instance reports running.
const healthIntegrationNotRunReason = "not run by any running hub instance"

// healthIntegrationRegistryUnavailableReason is the fixed reason given for a
// plugin record when the hub-instance registry could not be read, so it is
// not known which hub instances run the plugin.
const healthIntegrationRegistryUnavailableReason = "hub instance data not available"

// HealthSummaryIntegration is one chat or messaging plugin in the health
// summary, merged across the live hub instances that run it (see
// mergeHealthSummaryIntegrations). Its fields are an explicit allow-list:
// the plugin's own health message and details are never copied, because
// hub.health.read is a narrower permission than the integrations admin
// surface.
type HealthSummaryIntegration struct {
	Name     string `json:"name"`
	Platform string `json:"platform"`
	// Health is the worst health (healthy, degraded, unhealthy) reported
	// by the live hub instances that run the plugin, or "unknown" when
	// none of them reported a known value.
	Health string `json:"health"`
	// Connected is true only when every reporting instance reports the
	// plugin connected.
	Connected bool `json:"connected"`
	// Version is the version in the most recent report.
	Version string `json:"version"`
	// Reason is a fixed, server-composed explanation, set only when the
	// health could not be read for a known cause.
	Reason string `json:"reason,omitempty"`
	// ManagedBy lists the IDs of the live hub instances that report the
	// plugin, ordered by instance label, then ID. Empty when none does.
	ManagedBy []string `json:"managed_by,omitempty"`
	// ReportedAt is the oldest last write (store clock) among the reports
	// used, so the merged health is at least this fresh. Null when no
	// live instance reports the plugin.
	ReportedAt *time.Time `json:"reported_at,omitempty"`
}

// healthIntegrationQueryTimeout bounds how long one registry tick waits for
// all managed plugins' health, in total. A variable so tests can lower it.
var healthIntegrationQueryTimeout = 2 * time.Second

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

// hubInstanceIntegrations returns the health of the plugins this hub
// instance's plugin manager runs, for its registry row. It is called from
// the registry tick only; the health summary makes no plugin calls. The
// plugins are queried with getIntegrationStatus, the same check the
// Integrations admin page uses, all at once, waiting at most
// healthIntegrationQueryTimeout in total: a plugin that does not answer in
// time is reported with health "unknown". Only the allow-listed fields are
// kept; the registry writer normalises and caps the list.
func (s *Server) hubInstanceIntegrations(ctx context.Context) []api.HubInstanceIntegration {
	s.mu.RLock()
	mgr := s.pluginManager
	s.mu.RUnlock()
	if mgr == nil {
		return nil
	}
	seen := map[string]bool{}
	var names []string
	for _, key := range mgr.ListPlugins() {
		name := pluginNameFromKey(key)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)
	return s.queryIntegrationHealth(ctx, mgr, names)
}

// integrationHealthFlight is one running health query for a plugin. row
// is set before done is closed.
type integrationHealthFlight struct {
	done chan struct{}
	row  api.HubInstanceIntegration
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
				slog.Error("hub instance registry: integration health query panicked", "integration", name, "panic", r)
				f.row = hubInstanceIntegrationFromStatus(name, nil)
			}
			s.healthIntegrationMu.Lock()
			// Remove only this flight: never another query for the
			// same plugin that replaced it.
			if s.healthIntegrationFlights[name] == f {
				delete(s.healthIntegrationFlights, name)
			}
			s.healthIntegrationMu.Unlock()
			close(f.done)
		}()
		f.row = hubInstanceIntegrationFromStatus(name, getIntegrationStatus(mgr, name))
	}()
	return f
}

// queryIntegrationHealth reads the health of the named plugins, waiting at
// most healthIntegrationQueryTimeout (or until ctx ends) in total. Queries
// are shared per plugin (integrationHealthQuery): a caller that arrives
// while a query for the same plugin is running waits for that query's
// result, with its own deadline, instead of starting another. The plugin
// manager calls take no context, so a hung plugin keeps its one query
// running; every caller meanwhile reports it as unknown when its own
// deadline passes. The caller starts no goroutine of its own. The result
// has one entry per name, in the order of names.
func (s *Server) queryIntegrationHealth(ctx context.Context, mgr IntegrationManager, names []string) []api.HubInstanceIntegration {
	rows := make([]api.HubInstanceIntegration, len(names))
	flights := make([]*integrationHealthFlight, len(names))
	for i, name := range names {
		rows[i] = hubInstanceIntegrationFromStatus(name, nil)
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
			slog.Warn("hub instance registry: integration health not reported in time", "integration", names[i])
		}
	}
	return rows
}

// hubInstanceIntegrationFromStatus copies the allow-listed fields of a
// live integration status. Message and Details are deliberately dropped.
// A nil status (not reported) gives health "unknown", not connected.
func hubInstanceIntegrationFromStatus(name string, st *IntegrationStatus) api.HubInstanceIntegration {
	row := api.HubInstanceIntegration{Name: name, Health: healthIntegrationUnknown}
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

// healthIntegrationHealthRank orders known integration health for the
// worst-of merge. unknown is absent (rank 0): it never hides a known value.
var healthIntegrationHealthRank = map[string]int{
	HealthStatusHealthy:   1,
	HealthStatusDegraded:  2,
	HealthStatusUnhealthy: 3,
}

// healthIntegrationReport is one live hub instance's report of one plugin.
type healthIntegrationReport struct {
	instanceID string
	label      string
	lastSeen   time.Time
	in         api.HubInstanceIntegration
}

// mergeHealthSummaryIntegrations builds the integrations section from the
// hub-instance registry rows and the plugin record names (F3 design §5.7).
// It makes no plugin call: every hub instance, the serving one included,
// reports its plugins through its own registry row. Only rows that are live
// at the store clock now count; a stale or stopped instance's report is
// never used. For each plugin name reported by a live row or present as a
// plugin record:
//
//   - Health is the worst known value across the live reports; unknown is
//     neutral and is used only when no report has a known value.
//   - Connected is true only when every report says connected.
//   - Version is that of the report with the latest last_seen (ties go to
//     the lower instance ID).
//   - ManagedBy lists the reporting instances by label, then ID;
//     ReportedAt is the oldest last_seen among them.
//   - A plugin record with no live report is unknown, with the reason
//     healthIntegrationNotRunReason, or, when the registry could not be
//     read (registryRead false), healthIntegrationRegistryUnavailableReason.
//
// The list is sorted by name and is never nil. A pure function of its
// inputs.
func mergeHealthSummaryIntegrations(rows []store.HubInstance, now time.Time, registryRead bool, pluginRecordNames []string) []HealthSummaryIntegration {
	reports := map[string][]healthIntegrationReport{}
	for _, r := range rows {
		if hubInstanceState(r, now) != HubInstanceStateLive {
			continue
		}
		stats, ok := decodeHubInstanceStats(r.Stats)
		if !ok {
			continue
		}
		for _, in := range stats.Integrations {
			reports[in.Name] = append(reports[in.Name], healthIntegrationReport{
				instanceID: r.ID, label: r.Label, lastSeen: r.LastSeen, in: in,
			})
		}
	}

	out := make([]HealthSummaryIntegration, 0, len(reports)+len(pluginRecordNames))
	for name, rs := range reports {
		out = append(out, mergeHealthIntegrationReports(name, rs))
	}
	reason := healthIntegrationNotRunReason
	if !registryRead {
		reason = healthIntegrationRegistryUnavailableReason
	}
	listed := map[string]bool{}
	for name := range reports {
		listed[name] = true
	}
	for _, name := range pluginRecordNames {
		if listed[name] {
			continue
		}
		listed[name] = true
		out = append(out, HealthSummaryIntegration{
			Name:     name,
			Platform: resolvePlatform(name),
			Health:   healthIntegrationUnknown,
			Reason:   reason,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// mergeHealthIntegrationReports merges the live reports of one plugin; see
// mergeHealthSummaryIntegrations. rs is never empty.
func mergeHealthIntegrationReports(name string, rs []healthIntegrationReport) HealthSummaryIntegration {
	sort.Slice(rs, func(i, j int) bool {
		if rs[i].label != rs[j].label {
			return rs[i].label < rs[j].label
		}
		return rs[i].instanceID < rs[j].instanceID
	})
	it := HealthSummaryIntegration{
		Name:      name,
		Platform:  resolvePlatform(name),
		Health:    healthIntegrationUnknown,
		Connected: true,
		ManagedBy: make([]string, 0, len(rs)),
	}
	worst := 0
	freshest := rs[0]
	oldest := rs[0].lastSeen
	for _, r := range rs {
		it.ManagedBy = append(it.ManagedBy, r.instanceID)
		if rank := healthIntegrationHealthRank[r.in.Health]; rank > worst {
			worst = rank
			it.Health = r.in.Health
		}
		if !r.in.Connected {
			it.Connected = false
		}
		if r.lastSeen.After(freshest.lastSeen) || (r.lastSeen.Equal(freshest.lastSeen) && r.instanceID < freshest.instanceID) {
			freshest = r
		}
		if r.lastSeen.Before(oldest) {
			oldest = r.lastSeen
		}
	}
	it.Version = freshest.in.Version
	reportedAt := oldest.UTC()
	it.ReportedAt = &reportedAt
	return it
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
	return integrationHealthCounts(list, func(it HealthSummaryIntegration) string { return it.Health })
}

// hubInstanceIntegrationCounts aggregates one hub instance's reported
// integrations by health.
func hubInstanceIntegrationCounts(list []api.HubInstanceIntegration) HealthSummaryIntegrationCounts {
	return integrationHealthCounts(list, func(it api.HubInstanceIntegration) string { return it.Health })
}

// integrationHealthCounts counts list by the health health returns; any
// value but healthy, degraded or unhealthy counts as unknown.
func integrationHealthCounts[T any](list []T, health func(T) string) HealthSummaryIntegrationCounts {
	c := HealthSummaryIntegrationCounts{Total: len(list)}
	for _, it := range list {
		switch health(it) {
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
// list (with its managed_by and reported_at) becomes empty, each hub
// instance's integrations list is removed (its integration_counts stay),
// and the integration attention items are replaced, at
// the position of the first one, by aggregate items built only from
// IntegrationCounts ("N integrations unhealthy"). What is left matches the
// response on a hub with no integrations, except for the counts and the
// status, which hub.health.read covers.
func omitHealthSummaryIntegrationDetail(resp *HealthSummaryResponse) {
	resp.Integrations = []HealthSummaryIntegration{}
	resp.IntegrationsDetail = false
	if resp.HubInstances != nil {
		for i := range resp.HubInstances.Items {
			resp.HubInstances.Items[i].Integrations = nil
		}
	}

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
