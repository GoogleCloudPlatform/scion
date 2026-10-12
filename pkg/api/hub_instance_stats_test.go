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

package api

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeHubInstanceStats_DBCountersClampedAtZero(t *testing.T) {
	got := NormalizeHubInstanceStats(HubInstanceStats{
		DB: &HubInstanceDBStats{InUse: -1, Idle: 3, MaxOpen: -5, WaitCount: -2},
	})
	want := &HubInstanceDBStats{InUse: 0, Idle: 3, MaxOpen: 0, WaitCount: 0}
	if !reflect.DeepEqual(got.DB, want) {
		t.Fatalf("db = %+v, want %+v", got.DB, want)
	}
	if got := NormalizeHubInstanceStats(HubInstanceStats{}); got.DB != nil {
		t.Fatalf("db = %+v, want nil when the store has no pool", got.DB)
	}
}

func TestNormalizeHubInstanceStats_IntegrationsAllowListed(t *testing.T) {
	got := NormalizeHubInstanceStats(HubInstanceStats{Integrations: []HubInstanceIntegration{
		{Name: "telegram", Health: "degraded: slow", Connected: true, Version: "1.2.3"},
		{Name: "chat-app_2", Health: "available", Version: strings.Repeat("v", 33)},
		{Name: "Upper", Health: "healthy"},
		{Name: "dot.name", Health: "healthy"},
		{Name: strings.Repeat("a", 65), Health: "healthy"},
		{Name: "", Health: "healthy"},
		{Name: "slack", Health: "HEALTHY", Version: "1.0\n"},
		{Name: "telegram", Health: "unhealthy"},
	}})
	want := []HubInstanceIntegration{
		// available is a check value, not an integration health: unknown.
		{Name: "chat-app_2", Health: "unknown", Version: ""},
		{Name: "slack", Health: "healthy", Version: ""},
		// A repeated name keeps its least healthy entry.
		{Name: "telegram", Health: "unhealthy"},
	}
	if !reflect.DeepEqual(got.Integrations, want) {
		t.Fatalf("integrations = %+v, want %+v", got.Integrations, want)
	}
}

// A repeated integration name keeps the same entry whatever the input
// order: the least healthy, then not connected, then the lowest version.
func TestNormalizeHubInstanceStats_RepeatedNameIndependentOfOrder(t *testing.T) {
	entries := []HubInstanceIntegration{
		{Name: "chat", Health: "healthy", Connected: true, Version: "2.0"},
		{Name: "chat", Health: "degraded", Connected: true, Version: "1.9"},
		{Name: "chat", Health: "degraded", Connected: false, Version: "1.8"},
		{Name: "chat", Health: "degraded", Connected: false, Version: "1.7"},
		{Name: "chat", Health: "unknown"},
		{Name: "alpha", Health: "healthy", Version: "b"},
		{Name: "alpha", Health: "healthy", Version: "a"},
	}
	want := []HubInstanceIntegration{
		{Name: "alpha", Health: "healthy", Version: "a"},
		{Name: "chat", Health: "degraded", Connected: false, Version: "1.7"},
	}
	// Every rotation and the reverse of every rotation.
	for shift := 0; shift < len(entries); shift++ {
		rotated := append(append([]HubInstanceIntegration{}, entries[shift:]...), entries[:shift]...)
		reversed := make([]HubInstanceIntegration, len(rotated))
		for i := range rotated {
			reversed[len(rotated)-1-i] = rotated[i]
		}
		for _, in := range [][]HubInstanceIntegration{rotated, reversed} {
			got := NormalizeHubInstanceStats(HubInstanceStats{Integrations: in})
			if !reflect.DeepEqual(got.Integrations, want) {
				t.Fatalf("input %+v: integrations = %+v, want %+v", in, got.Integrations, want)
			}
		}
	}
	// unhealthy is less healthy than every other value.
	got := NormalizeHubInstanceStats(HubInstanceStats{Integrations: []HubInstanceIntegration{
		{Name: "x", Health: "unknown"}, {Name: "x", Health: "unhealthy", Connected: true}, {Name: "x", Health: "degraded"},
	}})
	if len(got.Integrations) != 1 || got.Integrations[0].Health != "unhealthy" {
		t.Fatalf("integrations = %+v, want the unhealthy entry", got.Integrations)
	}
}

func TestNormalizeHubInstanceStats_CapsIntegrationsSorted(t *testing.T) {
	var in []HubInstanceIntegration
	for i := 39; i >= 0; i-- {
		in = append(in, HubInstanceIntegration{Name: fmt.Sprintf("plugin-%02d", i), Health: "healthy"})
	}
	got := NormalizeHubInstanceStats(HubInstanceStats{Integrations: in})
	if len(got.Integrations) != HubInstanceMaxIntegrations {
		t.Fatalf("len = %d, want %d", len(got.Integrations), HubInstanceMaxIntegrations)
	}
	for i, it := range got.Integrations {
		if want := fmt.Sprintf("plugin-%02d", i); it.Name != want {
			t.Fatalf("integrations[%d] = %q, want %q (first names in sorted order)", i, it.Name, want)
		}
	}
}

func TestCapHubInstanceStats_WithinBudgetKeepsEverything(t *testing.T) {
	in := HubInstanceStats{DB: &HubInstanceDBStats{InUse: 1, Idle: 2, MaxOpen: 25, WaitCount: 4}}
	got, raw := CapHubInstanceStats(in, HubInstanceRowMaxBytes)
	if got.IntegrationsTruncated {
		t.Fatal("truncated set without a cut")
	}
	want := `{"db":{"in_use":1,"idle":2,"max_open":25,"wait_count":4}}`
	if string(raw) != want {
		t.Fatalf("raw = %s, want %s", raw, want)
	}
}

func TestCapHubInstanceStats_CutsIntegrationsToBudget(t *testing.T) {
	var in []HubInstanceIntegration
	for i := 0; i < HubInstanceMaxIntegrations; i++ {
		in = append(in, HubInstanceIntegration{
			Name:    fmt.Sprintf("%02d", i) + strings.Repeat("n", HubInstanceMaxIntegrationNameChars-2),
			Health:  "unhealthy",
			Version: strings.Repeat("v", HubInstanceMaxIntegrationVersionBytes),
		})
	}
	got, raw := CapHubInstanceStats(HubInstanceStats{
		DB:           &HubInstanceDBStats{InUse: 1 << 30, Idle: 1 << 30, MaxOpen: 1 << 30, WaitCount: 1 << 62},
		Integrations: in,
	}, HubInstanceRowMaxBytes)
	if len(raw) > HubInstanceRowMaxBytes {
		t.Fatalf("len(raw) = %d, want <= %d", len(raw), HubInstanceRowMaxBytes)
	}
	if !got.IntegrationsTruncated {
		t.Fatal("integrations_truncated not set after a cut")
	}
	if len(got.Integrations) == 0 || len(got.Integrations) >= HubInstanceMaxIntegrations {
		t.Fatalf("kept %d integrations, want a partial cut", len(got.Integrations))
	}
	// The cut drops from the end of the sorted list.
	if !strings.HasPrefix(got.Integrations[0].Name, "00") {
		t.Fatalf("first kept = %q, want the first in sorted order", got.Integrations[0].Name)
	}
	var decoded HubInstanceStats
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("raw does not decode: %v", err)
	}
	if !reflect.DeepEqual(decoded, got) {
		t.Fatalf("raw and returned stats differ:\n%+v\n%+v", decoded, got)
	}
}

func TestCapHubInstanceStats_TinyBudgetDropsAllIntegrationsKeepsDB(t *testing.T) {
	got, raw := CapHubInstanceStats(HubInstanceStats{
		DB:           &HubInstanceDBStats{MaxOpen: 4},
		Integrations: []HubInstanceIntegration{{Name: "chat", Health: "healthy"}},
	}, 10)
	if got.Integrations != nil || !got.IntegrationsTruncated || got.DB == nil {
		t.Fatalf("got %+v, want no integrations, truncated, db kept", got)
	}
	want := `{"db":{"in_use":0,"idle":0,"max_open":4,"wait_count":0},"integrations_truncated":true}`
	if string(raw) != want {
		t.Fatalf("raw = %s, want %s", raw, want)
	}
}
