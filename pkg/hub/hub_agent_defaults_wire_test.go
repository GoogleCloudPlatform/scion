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
	"log/slog"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// hubDefaultsDispatchAgent returns a minimal agent record for the dispatch
// tests below. AppliedConfig must be non-nil: buildCreateRequest only builds
// req.Config when the agent has one.
func hubDefaultsDispatchAgent() *store.Agent {
	return &store.Agent{
		ID:              tid("agent-1"),
		Name:            "test-agent",
		Slug:            "test-agent",
		OwnerID:         tid("user-1"),
		RuntimeBrokerID: tid("host-1"),
		AppliedConfig:   &store.AgentAppliedConfig{},
	}
}

func hubDefaultsDispatcher(t *testing.T, srv *Server) *HTTPAgentDispatcher {
	t.Helper()
	memStore := createTestStore(t)
	d := NewHTTPAgentDispatcherWithClient(memStore, &mockRuntimeBrokerClient{}, false, slog.Default())
	if srv != nil {
		d.SetHubAgentDefaultsProvider(srv.hubAgentDefaults)
	}
	return d
}

// TestDispatch_HubAgentDefaults_OnWire is the transport half of Gap 2c: the four
// limit/resource operational agent_defaults must reach the broker in their own
// typed slot in Postgres mode, and must be absent in file mode.
//
// The nil half is the file-mode-parity guard (acceptance criterion 12, rejected
// alternative A7), not an optional extra: in file mode a co-located broker reads
// the same settings.yaml and applies these values at the BOTTOM of its own
// chain, so sending them from the hub would promote them above broker profile
// resources and template limits in installs that never behaved that way.
func TestDispatch_HubAgentDefaults_OnWire(t *testing.T) {
	ctx := context.Background()

	t.Run("postgres mode populates the wire field", func(t *testing.T) {
		srv := &Server{maintenance: NewMaintenanceState(false, "")}
		// Postgres mode: Snapshot() fills Layer1Snapshot.Default* from the koanf
		// merge and ApplySnapshot writes them into ServerConfig (Phase 6).
		ApplySnapshot(srv, Layer1Snapshot{
			DefaultTemplate:      "team-default",
			DefaultHarnessConfig: "claude-vertex",
			DefaultMaxTurns:      50,
			DefaultMaxModelCalls: 200,
			DefaultMaxDuration:   "2h",
			DefaultResources:     &api.ResourceSpec{Limits: api.ResourceList{CPU: "2"}, Disk: "10Gi"},
		})

		req, err := hubDefaultsDispatcher(t, srv).buildCreateRequest(ctx, hubDefaultsDispatchAgent(), "test")
		if err != nil {
			t.Fatalf("buildCreateRequest: %v", err)
		}
		if req.Config == nil {
			t.Fatal("req.Config is nil")
		}
		hd := req.Config.HubAgentDefaults
		if hd == nil {
			t.Fatal("HubAgentDefaults is nil in Postgres mode; hub agent_defaults never reach the broker")
		}
		if hd.MaxTurns != 50 {
			t.Errorf("MaxTurns: want 50, got %d", hd.MaxTurns)
		}
		if hd.MaxModelCalls != 200 {
			t.Errorf("MaxModelCalls: want 200, got %d", hd.MaxModelCalls)
		}
		if hd.MaxDuration != "2h" {
			t.Errorf("MaxDuration: want 2h, got %q", hd.MaxDuration)
		}
		if hd.Resources == nil || hd.Resources.Limits.CPU != "2" || hd.Resources.Disk != "10Gi" {
			t.Errorf("Resources: want cpu=2 disk=10Gi, got %+v", hd.Resources)
		}

		// The two hub-resolved fields are Phase 7's ladder rungs and must NOT
		// ride this channel: they need Hub-side ID/hash stamping.
		blob, err := json.Marshal(hd)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		for _, forbidden := range []string{"team-default", "claude-vertex"} {
			if strings.Contains(string(blob), forbidden) {
				t.Errorf("hubAgentDefaults must not carry %q (Phase 7 ladder rungs, not this channel): %s", forbidden, blob)
			}
		}

		// The values must not have been folded into InlineConfig, which lands in
		// the OVERRIDE position broker-side and would beat a template's explicit
		// max_turns — rejected alternative A5, the inversion this phase exists
		// to avoid.
		if req.InlineConfig != nil && (req.InlineConfig.MaxTurns != 0 || req.InlineConfig.MaxModelCalls != 0 ||
			req.InlineConfig.MaxDuration != "" || req.InlineConfig.Resources != nil) {
			t.Errorf("hub defaults leaked into InlineConfig (alternative A5): %+v", req.InlineConfig)
		}
	})

	t.Run("file mode leaves the wire field nil", func(t *testing.T) {
		srv := &Server{maintenance: NewMaintenanceState(false, "")}
		// File mode: BuildLayer1SnapshotFromFile deliberately leaves the
		// agent-defaults fields zero (design §3.2.4), so the provider returns the
		// zero value and the wire field is omitted.
		ApplySnapshot(srv, BuildLayer1SnapshotFromFile(&config.GlobalConfig{
			Hub:  config.HubServerConfig{AdminEmails: []string{"admin@file.com"}},
			Auth: config.DevAuthConfig{UserAccessMode: "open"},
		}))

		req, err := hubDefaultsDispatcher(t, srv).buildCreateRequest(ctx, hubDefaultsDispatchAgent(), "test")
		if err != nil {
			t.Fatalf("buildCreateRequest: %v", err)
		}
		if req.Config == nil {
			t.Fatal("req.Config is nil")
		}
		if req.Config.HubAgentDefaults != nil {
			t.Fatalf("file mode must not send hub agent defaults (criterion 12 / alternative A7), got %+v",
				req.Config.HubAgentDefaults)
		}
		blob, err := json.Marshal(req.Config)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if strings.Contains(string(blob), "hubAgentDefaults") {
			t.Errorf("file-mode request JSON must not carry the key at all, got %s", blob)
		}
	})

	t.Run("no provider wired leaves the wire field nil", func(t *testing.T) {
		// Local/test dispatchers have no Server behind them. The omitempty tag
		// plus the nil provider means such a dispatch is byte-identical to today.
		req, err := hubDefaultsDispatcher(t, nil).buildCreateRequest(ctx, hubDefaultsDispatchAgent(), "test")
		if err != nil {
			t.Fatalf("buildCreateRequest: %v", err)
		}
		if req.Config.HubAgentDefaults != nil {
			t.Errorf("want nil with no provider, got %+v", req.Config.HubAgentDefaults)
		}
	})
}

// TestDispatch_FileMode_RequestJSONUnchanged is acceptance criterion 12's
// "diff a dispatch request against one captured on the pre-change tree",
// executable and pinned.
//
// The golden is the WHOLE serialized RemoteCreateAgentRequest for a fixed
// input, not just the new field, so an incidental file-mode change to any part
// of the dispatch payload trips it too. It was captured by running this exact
// marshal on the pre-change tree — base 5b0fb1c5 with every Phase 8 edit
// removed — and the byte-for-byte match was verified against the post-change
// tree before the golden was written down here.
//
// requestId is dropped before comparison: it is a fresh uuid.NewString() per
// call and is the only non-deterministic member. Everything else, including the
// two derived UUIDs, is stable across runs.
func TestDispatch_FileMode_RequestJSONUnchanged(t *testing.T) {
	const preChangeGolden = `{"config":{},"id":"8de2cea5-95b0-5ee2-a75a-c4d168aff6a7",` +
		`"name":"test-agent","projectId":"","slug":"test-agent",` +
		`"userId":"b88f5d8f-14e6-5a0a-8f6c-6b720a0f672c"}`

	ctx := context.Background()
	srv := &Server{maintenance: NewMaintenanceState(false, "")}
	ApplySnapshot(srv, BuildLayer1SnapshotFromFile(&config.GlobalConfig{
		Hub:  config.HubServerConfig{AdminEmails: []string{"admin@file.com"}},
		Auth: config.DevAuthConfig{UserAccessMode: "open"},
	}))

	req, err := hubDefaultsDispatcher(t, srv).buildCreateRequest(ctx, hubDefaultsDispatchAgent(), "test")
	if err != nil {
		t.Fatalf("buildCreateRequest: %v", err)
	}
	blob, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(blob, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	delete(m, "requestId")
	// json.Marshal of a map sorts keys, so the normalised form is stable
	// regardless of struct field order.
	norm, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}

	if string(norm) != preChangeGolden {
		t.Errorf("file-mode dispatch payload changed.\n pre-change: %s\npost-change: %s", preChangeGolden, norm)
	}
}

// TestRemoteHubAgentDefaults_WireCompatibleWithBroker pins the two halves of the
// wire together. The hub sends RemoteHubAgentDefaults and the broker decodes
// api.HubAgentDefaults; they are separate types (the hub package's Remote*
// convention, same as RemoteGCPIdentityConfig), so a renamed JSON tag on either
// side would silently drop hub defaults on the floor with no compile error.
func TestRemoteHubAgentDefaults_WireCompatibleWithBroker(t *testing.T) {
	sent := &RemoteHubAgentDefaults{
		MaxTurns:      50,
		MaxModelCalls: 200,
		MaxDuration:   "2h",
		Resources: &api.ResourceSpec{
			Requests: api.ResourceList{CPU: "1", Memory: "1Gi"},
			Limits:   api.ResourceList{CPU: "2", Memory: "4Gi"},
			Disk:     "10Gi",
		},
	}

	blob, err := json.Marshal(&RemoteAgentConfig{HubAgentDefaults: sent})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got runtimebroker.CreateAgentConfig
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatalf("broker-side unmarshal: %v", err)
	}
	if got.HubAgentDefaults == nil {
		t.Fatalf("broker decoded no hub defaults from %s", blob)
	}
	want := api.HubAgentDefaults{
		MaxTurns:      sent.MaxTurns,
		MaxModelCalls: sent.MaxModelCalls,
		MaxDuration:   sent.MaxDuration,
		Resources:     sent.Resources,
	}
	if got.HubAgentDefaults.MaxTurns != want.MaxTurns ||
		got.HubAgentDefaults.MaxModelCalls != want.MaxModelCalls ||
		got.HubAgentDefaults.MaxDuration != want.MaxDuration ||
		got.HubAgentDefaults.Resources == nil ||
		*got.HubAgentDefaults.Resources != *want.Resources {
		t.Errorf("wire round-trip mismatch:\n sent %+v\n got  %+v (resources %+v)",
			want, got.HubAgentDefaults, got.HubAgentDefaults.Resources)
	}
}

// TestRemoteHubAgentDefaults_NilWhenEmpty covers the conversion directly: an
// empty section must produce nil, not an empty struct. An empty struct would
// still marshal the key and would make the broker-side rung "fire" with zero
// values, which is how file-mode parity would silently break.
func TestRemoteHubAgentDefaults_NilWhenEmpty(t *testing.T) {
	if got := remoteHubAgentDefaults(opsettings.AgentDefaultsSettings{}); got != nil {
		t.Errorf("want nil for empty section, got %+v", got)
	}
	// The two hub-resolved fields alone must not put the field on the wire:
	// they travel on the AppliedConfig ladder (Phase 7).
	d := opsettings.AgentDefaultsSettings{}
	d.DefaultTemplate = "team-default"
	d.DefaultHarnessConfig = "claude-vertex"
	if got := remoteHubAgentDefaults(d); got != nil {
		t.Errorf("template/harness-config defaults must not travel here, got %+v", got)
	}
	// A single limit is enough to send.
	d = opsettings.AgentDefaultsSettings{}
	d.DefaultMaxTurns = 50
	if got := remoteHubAgentDefaults(d); got == nil || got.MaxTurns != 50 {
		t.Errorf("want MaxTurns 50 on the wire, got %+v", got)
	}
}
