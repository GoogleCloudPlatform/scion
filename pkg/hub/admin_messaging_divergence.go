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
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
)

// divergenceBoardCaveats contains the machine-readable caveats that accompany
// every divergence board response. They are constant — the board's structural
// limitations do not change at runtime.
type divergenceBoardCaveats struct {
	Scope                     string `json:"scope"`
	ScopeDetail               string `json:"scope_detail"`
	MismatchComposition       string `json:"mismatch_composition"`
	ConsistencyCheckFailsOpen string `json:"consistency_check_fails_open"`
	NotGoNoGo                 string `json:"not_go_no_go"`
	CounterSnapshot           string `json:"counter_snapshot"`
}

// divergenceBoardResponse is the JSON shape returned by
// GET /api/v1/admin/messaging/divergence.
type divergenceBoardResponse struct {
	HubID            string                 `json:"hub_id"`
	ProcessStartTime string                 `json:"process_start_time"`
	ProcessUptime    string                 `json:"process_uptime"`
	Matches          int64                  `json:"matches"`
	Mismatches       int64                  `json:"mismatches"`
	Comparisons      int64                  `json:"comparisons"`
	Fallbacks        int64                  `json:"fallbacks"`
	Caveats          divergenceBoardCaveats `json:"caveats"`
}

// caveats is the singleton caveat block. These are structural properties of
// the counter, not runtime values — they never change.
var divergenceCaveats = divergenceBoardCaveats{
	Scope:       "per_replica_since_boot",
	ScopeDetail: "These counters live in process memory and reset when the replica restarts. They reflect only this replica's traffic, identified by hub_id.",
	MismatchComposition: "The mismatches count conflates two unrelated signals: " +
		"routing-key disagreement (ComputeDivergenceMatch) and prior-message " +
		"conversation_id inconsistency (CheckConversationConsistency). " +
		"This board cannot separate them.",
	ConsistencyCheckFailsOpen: "CheckConversationConsistency returns true " +
		"(no mismatch increment) on ListMessages query errors, on empty " +
		"resolved conversation IDs, and when insufficient data prevents " +
		"lookup. A low mismatch count does not imply agreement — it is " +
		"equally consistent with agreement, query errors, or insufficient " +
		"lookup data.",
	NotGoNoGo: "This board is NOT the Tranche G go/no-go input. " +
		"The offline recomputation report is the artifact that answers " +
		"the go/no-go question.",
	CounterSnapshot: "All counters are read independently via separate atomic loads " +
		"and may not correspond to a single instant. comparisons is computed as " +
		"matches + mismatches from those independent reads, so the triple is " +
		"arithmetically consistent but not a true snapshot. Ratios derived from " +
		"these values (e.g. mismatch rate, fallback percentage) are approximate.",
}

// handleAdminMessagingDivergence handles GET /api/v1/admin/messaging/divergence.
// It returns a read-only snapshot of the in-memory divergence counters with
// structural caveats about what the numbers mean and do not mean.
//
// Authorization: enforced by routeGuard via hub.diagnostics.read permission.
func (s *Server) handleAdminMessagingDivergence(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	// Load matches and mismatches once so that comparisons = matches + mismatches
	// is arithmetically consistent in the response. Each is an atomic load from
	// DivergenceMetrics; loading them separately means a concurrent Inc could
	// land between the two reads, but the triple is internally consistent.
	m := messaging.DivergenceMetrics
	matches := m.Matches()
	mismatches := m.Mismatches()
	fallbacks := m.Fallbacks()

	writeJSON(w, http.StatusOK, divergenceBoardResponse{
		HubID:            s.HubID(),
		ProcessStartTime: s.startTime.UTC().Format(time.RFC3339),
		ProcessUptime:    time.Since(s.startTime).Round(time.Second).String(),
		Matches:          matches,
		Mismatches:       mismatches,
		Comparisons:      matches + mismatches,
		Fallbacks:        fallbacks,
		Caveats:          divergenceCaveats,
	})
}
