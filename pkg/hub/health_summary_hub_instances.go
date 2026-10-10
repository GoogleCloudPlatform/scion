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
	"sort"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// The health summary's hub_instances section (health dashboard F3 design
// §5.6). It is read from the hub_instances table only: the serving replica
// runs no live probe for it, and its own entry is its DB row like every
// other replica's, marked serving.
//
// One clock: the display cut, every state and AsOf all come from the store
// clock that ListHubInstances reads, the same clock that wrote started_at
// and last_seen. The serving hub's local clock is never mixed in, so a hub
// whose clock is off from the database's still shows correct states and
// ages. The dashboard computes uptime and "last seen" from AsOf.

const (
	// hubInstanceStaleAfter is how long a row may go without a write before
	// it is shown as stale: 3 registry ticks.
	hubInstanceStaleAfter = 3 * hubInstanceTickInterval
	// hubInstanceDisplayWindow lists rows whose last write (or stop) is
	// within this window.
	hubInstanceDisplayWindow = time.Hour
)

// healthSummaryHubInstanceLimit caps the hub_instances items. A variable so
// tests can lower it.
var healthSummaryHubInstanceLimit = 50

// Hub instance states, computed when the summary is read.
const (
	HubInstanceStateLive    = "live"
	HubInstanceStateStale   = "stale"
	HubInstanceStateStopped = "stopped"
)

// HealthSummaryHubInstances is the hub_instances section of the health
// summary: every hub instance that wrote its registry row within the last
// hour.
type HealthSummaryHubInstances struct {
	// AsOf is the store clock the section was computed at (UTC). State,
	// the display window and the dashboard's uptime and age all use it.
	AsOf time.Time `json:"as_of"`
	// Items lists live instances first (the serving one first), then
	// stale, then stopped; each group ordered by label, then ID. Capped at
	// healthSummaryHubInstanceLimit.
	Items []HealthHubInstance `json:"items"`
	// Live is the number of live instances, including any cut from Items.
	Live int `json:"live"`
	// Total is the number of listed instances, including any cut from
	// Items.
	Total int `json:"total"`
	// Truncated is true when Total exceeds len(Items).
	Truncated bool `json:"truncated"`
}

// HealthHubInstance is one hub instance (process) in the summary.
type HealthHubInstance struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Version string `json:"version"`
	// State is live, stale (no write for hubInstanceStaleAfter) or stopped.
	State string `json:"state"`
	// Serving is true for the instance that built this response.
	Serving bool `json:"serving"`
	// StartedAt is the store clock at the instance's first write; the
	// dashboard computes uptime from it and the response's generated_at.
	StartedAt time.Time `json:"started_at"`
	// LastSeen is the store clock of the instance's last write.
	LastSeen time.Time `json:"last_seen"`
	// StoppedAt is null unless the instance stopped cleanly.
	StoppedAt *time.Time `json:"stopped_at"`
	// Status is the instance's last reported status: healthy, degraded or
	// unhealthy. For a stale or stopped instance it is out of date.
	Status string `json:"status"`
	// Checks is the instance's last reported check map; fixed values only
	// (see api.NormalizeHubInstanceChecks).
	Checks map[string]string `json:"checks"`
	// Database is the instance's last reported database connection pool,
	// from its own sql.DB.Stats(). Null when the instance reported no pool
	// (its store exposes no *sql.DB) or its stats could not be read.
	Database *HealthHubInstanceDB `json:"database"`
}

// HealthHubInstanceDB is one hub instance's database connection pool.
type HealthHubInstanceDB struct {
	// PoolActive is the number of connections in use.
	PoolActive int `json:"pool_active"`
	// PoolIdle is the number of idle connections.
	PoolIdle int `json:"pool_idle"`
	// PoolMax is the pool limit; 0 means no limit.
	PoolMax int `json:"pool_max"`
	// PoolWaitCountTotal is the cumulative number of waits for a
	// connection since the instance started.
	PoolWaitCountTotal int64 `json:"pool_wait_count_total"`
}

// hubInstanceDatabase decodes a registry row's stats column and returns its
// pool block, or nil when the row has none or its stats do not decode (the
// row is still listed).
func hubInstanceDatabase(raw json.RawMessage) *HealthHubInstanceDB {
	if len(raw) == 0 {
		return nil
	}
	var stats api.HubInstanceStats
	if err := json.Unmarshal(raw, &stats); err != nil || stats.DB == nil {
		return nil
	}
	return &HealthHubInstanceDB{
		PoolActive:         stats.DB.InUse,
		PoolIdle:           stats.DB.Idle,
		PoolMax:            stats.DB.MaxOpen,
		PoolWaitCountTotal: stats.DB.WaitCount,
	}
}

// hubInstanceState applies the state rule: stopped when stopped_at is set,
// stale when now - last_seen exceeds hubInstanceStaleAfter, live otherwise.
// now must be the store clock returned with the rows.
func hubInstanceState(row store.HubInstance, now time.Time) string {
	switch {
	case row.StoppedAt != nil:
		return HubInstanceStateStopped
	case now.Sub(row.LastSeen) > hubInstanceStaleAfter:
		return HubInstanceStateStale
	default:
		return HubInstanceStateLive
	}
}

// hubInstanceStateRank orders the item groups: live, stale, stopped.
func hubInstanceStateRank(state string) int {
	switch state {
	case HubInstanceStateLive:
		return 0
	case HubInstanceStateStale:
		return 1
	default:
		return 2
	}
}

// healthSummaryHubInstances reads the hub_instances section. The store cuts
// the list at its own clock minus hubInstanceDisplayWindow. It returns nil
// and the error when the registry cannot be read; the summary then reports
// the section as null ("not reported").
func (s *Server) healthSummaryHubInstances(ctx context.Context) (*HealthSummaryHubInstances, error) {
	rows, storeNow, err := s.store.ListHubInstances(ctx, hubInstanceDisplayWindow)
	if err != nil {
		return nil, err
	}
	return buildHealthSummaryHubInstances(rows, storeNow, s.InstanceID()), nil
}

// buildHealthSummaryHubInstances computes each row's state against the
// store clock now, marks the serving instance, orders the items and applies
// the cap. A pure function of its inputs.
func buildHealthSummaryHubInstances(rows []store.HubInstance, now time.Time, servingID string) *HealthSummaryHubInstances {
	out := &HealthSummaryHubInstances{
		AsOf:  now.UTC(),
		Items: make([]HealthHubInstance, 0, len(rows)),
		Total: len(rows),
	}
	for _, r := range rows {
		item := HealthHubInstance{
			ID:        r.ID,
			Label:     r.Label,
			Version:   r.Version,
			State:     hubInstanceState(r, now),
			Serving:   r.ID == servingID,
			StartedAt: r.StartedAt,
			LastSeen:  r.LastSeen,
			StoppedAt: r.StoppedAt,
			Status:    r.Status,
			Checks:    r.Checks,
			Database:  hubInstanceDatabase(r.Stats),
		}
		if item.Checks == nil {
			item.Checks = map[string]string{}
		}
		if item.State == HubInstanceStateLive {
			out.Live++
		}
		out.Items = append(out.Items, item)
	}
	sort.SliceStable(out.Items, func(i, j int) bool {
		a, b := out.Items[i], out.Items[j]
		if ra, rb := hubInstanceStateRank(a.State), hubInstanceStateRank(b.State); ra != rb {
			return ra < rb
		}
		if a.Serving != b.Serving {
			return a.Serving
		}
		if a.Label != b.Label {
			return a.Label < b.Label
		}
		return a.ID < b.ID
	})
	if len(out.Items) > healthSummaryHubInstanceLimit {
		out.Items = out.Items[:healthSummaryHubInstanceLimit]
		out.Truncated = true
	}
	return out
}
