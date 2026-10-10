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
	"sort"
)

// HubInstanceStats is the stats column of a hub-instance registry row
// (health dashboard F3 design §5.1, §5.5): bounded per-instance figures, no
// free text. New fields must be bounded too, and must keep the serialised
// payload (label, version, status, checks and stats) within
// HubInstanceRowMaxBytes (see CapHubInstanceStats).
type HubInstanceStats struct {
	// DB is the instance's database connection pool, from
	// sql.DB.Stats(). Nil when the store exposes no *sql.DB.
	DB *HubInstanceDBStats `json:"db,omitempty"`
	// Integrations lists the plugins this instance runs, sorted by name,
	// at most HubInstanceMaxIntegrations.
	Integrations []HubInstanceIntegration `json:"integrations,omitempty"`
	// IntegrationsTruncated is true when CapHubInstanceStats cut
	// Integrations to keep the payload within its size cap.
	IntegrationsTruncated bool `json:"integrations_truncated,omitempty"`
}

// HubInstanceDBStats is one hub instance's database connection pool.
// InUse, Idle and WaitCount change from tick to tick (TouchHubInstance
// writes them); MaxOpen is configuration, so a change to it is a material
// change that rewrites the full row.
type HubInstanceDBStats struct {
	// InUse is the number of connections in use.
	InUse int `json:"in_use"`
	// Idle is the number of idle connections.
	Idle int `json:"idle"`
	// MaxOpen is the pool limit; 0 means no limit.
	MaxOpen int `json:"max_open"`
	// WaitCount is the cumulative number of waits for a connection since
	// the process started.
	WaitCount int64 `json:"wait_count"`
}

// HubInstanceIntegration is one plugin run by a hub instance. Only these
// allow-listed fields are kept; never a plugin's message or details.
type HubInstanceIntegration struct {
	// Name is the plugin name, [a-z0-9_-]{1,64}.
	Name string `json:"name"`
	// Health is healthy, degraded, unhealthy or unknown.
	Health string `json:"health"`
	// Connected reports whether the plugin is connected.
	Connected bool `json:"connected"`
	// Version is at most HubInstanceMaxIntegrationVersionBytes of
	// printable ASCII, otherwise empty.
	Version string `json:"version"`
}

const (
	// HubInstanceRowMaxBytes caps the serialised payload of a registry
	// row: the JSON of its label, version, status, checks and stats. It
	// keeps the row small. The instance ID and the timestamps are not
	// counted.
	HubInstanceRowMaxBytes = 4096
	// HubInstanceMaxIntegrations caps the integrations kept in stats.
	HubInstanceMaxIntegrations = 32
	// HubInstanceMaxIntegrationNameChars caps an integration name.
	HubInstanceMaxIntegrationNameChars = 64
	// HubInstanceMaxIntegrationVersionBytes caps an integration version.
	HubInstanceMaxIntegrationVersionBytes = 32
)

// hubInstanceIntegrationHealth is the vocabulary for integration health.
var hubInstanceIntegrationHealth = map[string]bool{
	BrokerHealthHealthy:   true,
	BrokerHealthDegraded:  true,
	BrokerHealthUnhealthy: true,
	BrokerHealthUnknown:   true,
}

// NormalizeHubInstanceStats returns a bounded copy of s:
//
//   - DB counters below zero become zero.
//   - Integrations whose name fails [a-z0-9_-]{1,64} are dropped, as is a
//     repeated name (the first is kept). The rest are sorted by name and
//     cut to HubInstanceMaxIntegrations. Health is reduced to its leading
//     fixed word (healthy, degraded, unhealthy or unknown; anything else
//     becomes unknown). A version that is longer than
//     HubInstanceMaxIntegrationVersionBytes or holds a byte outside
//     printable ASCII becomes empty.
//   - IntegrationsTruncated is kept as given; CapHubInstanceStats sets it.
func NormalizeHubInstanceStats(s HubInstanceStats) HubInstanceStats {
	out := HubInstanceStats{IntegrationsTruncated: s.IntegrationsTruncated}
	if s.DB != nil {
		out.DB = &HubInstanceDBStats{
			InUse:     max(s.DB.InUse, 0),
			Idle:      max(s.DB.Idle, 0),
			MaxOpen:   max(s.DB.MaxOpen, 0),
			WaitCount: max(s.DB.WaitCount, 0),
		}
	}
	seen := make(map[string]bool, len(s.Integrations))
	for _, in := range s.Integrations {
		if !validHubInstanceIntegrationName(in.Name) || seen[in.Name] {
			continue
		}
		seen[in.Name] = true
		version := in.Version
		if !printableASCII(version, HubInstanceMaxIntegrationVersionBytes) {
			version = ""
		}
		out.Integrations = append(out.Integrations, HubInstanceIntegration{
			Name:      in.Name,
			Health:    leadingHealthWord(in.Health, hubInstanceIntegrationHealth),
			Connected: in.Connected,
			Version:   version,
		})
	}
	sort.Slice(out.Integrations, func(i, j int) bool {
		return out.Integrations[i].Name < out.Integrations[j].Name
	})
	if len(out.Integrations) > HubInstanceMaxIntegrations {
		out.Integrations = out.Integrations[:HubInstanceMaxIntegrations]
	}
	return out
}

// CapHubInstanceStats normalises s and returns it with its JSON encoding,
// cut so the encoding is at most maxBytes long. When it is longer, the
// integrations are dropped from the end of the (sorted) list, one at a
// time, and IntegrationsTruncated is set. The DB block is fixed-size and
// never cut. The registry writer passes the room left in
// HubInstanceRowMaxBytes after the payload's other fields.
func CapHubInstanceStats(s HubInstanceStats, maxBytes int) (HubInstanceStats, json.RawMessage) {
	s = NormalizeHubInstanceStats(s)
	// A struct of bounded strings, ints and bools always marshals.
	b, _ := json.Marshal(s)
	for len(b) > maxBytes && len(s.Integrations) > 0 {
		s.Integrations = s.Integrations[:len(s.Integrations)-1]
		if len(s.Integrations) == 0 {
			s.Integrations = nil
		}
		s.IntegrationsTruncated = true
		b, _ = json.Marshal(s)
	}
	return s, b
}

// validHubInstanceIntegrationName reports whether name matches
// ^[a-z0-9_-]{1,64}$.
func validHubInstanceIntegrationName(name string) bool {
	if name == "" || len(name) > HubInstanceMaxIntegrationNameChars {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// printableASCII reports whether s is at most max bytes, every one of them
// printable ASCII.
func printableASCII(s string, max int) bool {
	if len(s) > max {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}
