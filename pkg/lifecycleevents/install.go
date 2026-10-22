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

package lifecycleevents

import (
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/telemetryemit"
)

// Where the hub's ID came from (design 3.3, B4).
const (
	// HubIDSourcePinned: configured explicitly (server.hub.hub_id or
	// SCION_SERVER_HUB_HUBID), shared by every replica.
	HubIDSourcePinned = "pinned"
	// HubIDSourcePersistedFile: read from the persisted ~/.scion/hub-id.
	HubIDSourcePersistedFile = "persisted_file"
	// HubIDSourceHostname: derived from the host name and not persisted.
	HubIDSourceHostname = "hostname"
	// HubIDSourceKService: derived from Cloud Run's K_SERVICE.
	HubIDSourceKService = "k_service"
)

// ClassifyHubIDSource reports where resolvedID came from, given the hub
// ID resolution inputs: whether the ID was pinned, the K_SERVICE value,
// and the content of the persisted hub-id file ("" when absent or
// unreadable). It mirrors config.ResolveHubIDFromEnv's precedence.
func ClassifyHubIDSource(pinned bool, kService, persistedID, resolvedID string) string {
	switch {
	case pinned:
		return HubIDSourcePinned
	case kService != "":
		return HubIDSourceKService
	case resolvedID != "" && strings.TrimSpace(persistedID) == resolvedID:
		return HubIDSourcePersistedFile
	default:
		return HubIDSourceHostname
	}
}

// StableHubID reports whether a hub ID from source is a stable logical-hub
// identity for producer_namespace: a pinned ID, or the persisted file on a
// single-process SQLite hub (SQLite implies one process, so the file is
// stable per installation). Anything else could change across replicas or
// restarts and would re-key replayed events.
func StableHubID(source string, sqlite bool) bool {
	return source == HubIDSourcePinned || (sqlite && source == HubIDSourcePersistedFile)
}

// ObserverState is the observer state a replica reports
// (telemetryemit.ObserverState*): enabled only when the feature is on and
// the hub ID is stable; otherwise the reason it is off. The state is
// exported as a gauge so a refusal never reads as zero lifecycle activity.
func ObserverState(enabled bool, source string, sqlite bool) string {
	switch {
	case !enabled:
		return telemetryemit.ObserverStateDisabledExperimentOff
	case !StableHubID(source, sqlite):
		return telemetryemit.ObserverStateDisabledUnpinnedHubID
	default:
		return telemetryemit.ObserverStateEnabled
	}
}
