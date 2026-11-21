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

//go:build !no_sqlite

package hub

import (
	"context"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// TestBuildCreateRequest_WarnsWhenGCPTelemetryBlockedWithoutCredentials pins
// the dispatch wiring in buildCreateRequest so removing the warning call site
// fails this test.
func TestBuildCreateRequest_WarnsWhenGCPTelemetryBlockedWithoutCredentials(t *testing.T) {
	boolPtr := func(v bool) *bool { return &v }
	srv := &Server{
		store:       createTestStore(t),
		maintenance: NewMaintenanceState(false, ""),
		config: ServerConfig{
			HubEndpoint: "https://hub.example.com",
			TelemetryConfig: &api.TelemetryConfig{
				Enabled: boolPtr(true),
				Cloud: &api.TelemetryCloudConfig{
					Enabled:  boolPtr(true),
					Provider: "gcp",
				},
			},
		},
	}
	d := srv.CreateAuthenticatedDispatcher()

	// Default agent has nil GCPIdentity, which resolves to block on non-Kubernetes runtimes.
	ctx, warns := withDispatchWarnings(context.Background())
	if _, err := d.buildCreateRequest(ctx, hubDefaultsDispatchAgent(), "test"); err != nil {
		t.Fatalf("buildCreateRequest: %v", err)
	}
	got := warns.Warnings()
	found := false
	for _, w := range got {
		if strings.Contains(w, telemetryGCPCredentialsSecretName) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected dispatch warning mentioning %q, got %v", telemetryGCPCredentialsSecretName, got)
	}
}
