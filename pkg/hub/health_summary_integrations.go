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
	"sort"
	"strings"

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

// healthSummaryIntegrations builds the integrations section of the health
// summary. Plugins run by this hub instance's plugin manager are queried
// with getIntegrationStatus, the same live check the Integrations admin
// page uses. Plugin records in the store that this instance does not run
// are listed with health "unknown" and a fixed reason. The list is sorted
// by name and is never nil.
func (s *Server) healthSummaryIntegrations(ctx context.Context) []HealthSummaryIntegration {
	s.mu.RLock()
	mgr := s.pluginManager
	s.mu.RUnlock()

	out := []HealthSummaryIntegration{}
	managed := map[string]bool{}
	if mgr != nil {
		for _, key := range mgr.ListPlugins() {
			name := pluginNameFromKey(key)
			if name == "" || managed[name] {
				continue
			}
			managed[name] = true
			out = append(out, healthSummaryIntegrationFromStatus(name, getIntegrationStatus(mgr, name)))
		}
	}

	for _, name := range s.healthSummaryPluginRecordNames(ctx) {
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

// healthSummaryPluginRecordNames returns the plugin names of the plugin
// records (the plugin label) in the runtime broker table. On a store
// error it logs and returns what it has, so managed plugins still show.
func (s *Server) healthSummaryPluginRecordNames(ctx context.Context) []string {
	var names []string
	opts := store.ListOptions{Limit: healthSummaryBrokerPageSize}
	for {
		page, err := s.store.ListRuntimeBrokers(ctx, store.RuntimeBrokerFilter{}, opts)
		if err != nil {
			slog.Warn("health summary: failed to list plugin records", "error", err)
			return names
		}
		for i := range page.Items {
			b := &page.Items[i]
			if !isPluginBroker(b) {
				continue
			}
			name := b.Labels[pluginBrokerLabel]
			if name == "" {
				name = strings.TrimPrefix(b.Name, "plugin-")
			}
			if name != "" {
				names = append(names, name)
			}
		}
		if page.NextCursor == "" || page.NextCursor == opts.Cursor {
			break
		}
		opts.Cursor = page.NextCursor
	}
	return names
}
