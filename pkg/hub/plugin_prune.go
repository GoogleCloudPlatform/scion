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
	"os"
	"path/filepath"
	"sort"

	"github.com/GoogleCloudPlatform/scion/pkg/plugin"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// PruneStalePluginRecords deletes runtime_brokers rows that represent broker
// plugins (carrying Labels["scion.io/plugin"]) when the record is stale:
//   - no live hub instance reports running the plugin,
//   - it is not loaded in this server's plugin manager,
//   - it is not present in configuredPlugins (server.plugins.broker), and
//   - it has no valid executable binary in pluginsDir/broker/scion-plugin-<name>.
//
// Plugin names that fail plugin.IsValidPluginName (for example backup files
// like "discord.bak.<timestamp>") are never considered backed by a valid
// binary, so they are pruned once unrun and unconfigured.
//
// If the hub-instance registry cannot be read, pruning is skipped (fail-safe)
// so records owned by peer HA replicas are never mistakenly removed.
// Returns the sorted plugin names whose records were pruned.
func (s *Server) PruneStalePluginRecords(ctx context.Context, configuredPlugins map[string]bool, pluginsDir string) ([]string, error) {
	if s.store == nil {
		return nil, nil
	}

	instanceRows, storeNow, err := s.store.ListHubInstances(ctx, hubInstanceDisplayWindow)
	if err != nil {
		return nil, fmt.Errorf("list hub instances: %w", err)
	}

	liveReported := make(map[string]bool)
	for _, r := range instanceRows {
		if hubInstanceState(r, storeNow) != HubInstanceStateLive {
			continue
		}
		stats, ok := decodeHubInstanceStats(r.Stats)
		if !ok {
			continue
		}
		for _, in := range stats.Integrations {
			if in.Name != "" {
				liveReported[in.Name] = true
			}
		}
	}

	localLoaded := make(map[string]bool)
	s.mu.RLock()
	mgr := s.pluginManager
	s.mu.RUnlock()
	if mgr != nil {
		for _, key := range mgr.ListPlugins() {
			if name := pluginNameFromKey(key); name != "" {
				localLoaded[name] = true
			}
		}
	}

	type pruneCandidate struct {
		id   string
		name string
	}
	var candidates []pruneCandidate
	opts := store.ListOptions{Limit: healthSummaryBrokerPageSize}
	for {
		page, listErr := s.store.ListRuntimeBrokers(ctx, store.RuntimeBrokerFilter{}, opts)
		if listErr != nil {
			return nil, fmt.Errorf("list runtime brokers: %w", listErr)
		}
		for i := range page.Items {
			b := &page.Items[i]
			if !isPluginBroker(b) {
				continue
			}
			name := pluginRecordName(b)
			if name == "" {
				continue
			}
			if liveReported[name] || localLoaded[name] || configuredPlugins[name] {
				continue
			}
			if hasValidPluginBinary(pluginsDir, name) {
				continue
			}
			candidates = append(candidates, pruneCandidate{id: b.ID, name: name})
		}
		if page.NextCursor == "" || page.NextCursor == opts.Cursor {
			break
		}
		opts.Cursor = page.NextCursor
	}

	var pruned []string
	var errs []error
	for _, c := range candidates {
		if delErr := s.store.DeleteRuntimeBroker(ctx, c.id); delErr != nil {
			if errors.Is(delErr, store.ErrNotFound) {
				continue
			}
			slog.WarnContext(ctx, "Failed to prune stale integration record",
				"plugin", c.name, "broker_id", c.id, "error", delErr)
			errs = append(errs, fmt.Errorf("delete plugin broker %s (%s): %w", c.name, c.id, delErr))
			continue
		}
		slog.InfoContext(ctx, "Pruned stale integration record",
			"plugin", c.name, "broker_id", c.id)
		pruned = append(pruned, c.name)
	}

	sort.Strings(pruned)
	return pruned, errors.Join(errs...)
}

// hasValidPluginBinary reports whether pluginsDir/broker/scion-plugin-<name>
// exists as an executable regular file for a valid plugin identifier.
func hasValidPluginBinary(pluginsDir, name string) bool {
	if !plugin.IsValidPluginName(name) || pluginsDir == "" {
		return false
	}
	candidate := filepath.Join(pluginsDir, plugin.PluginTypeBroker, plugin.PluginBinaryPrefix+name)
	info, err := os.Stat(candidate)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode()&0111 != 0
}
