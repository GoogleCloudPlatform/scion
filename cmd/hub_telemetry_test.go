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

package cmd

import (
	"testing"

	"github.com/google/uuid"
)

type fakeTelemetrySource struct{ hubID, instanceID string }

func (f fakeTelemetrySource) HubID() string      { return f.hubID }
func (f fakeTelemetrySource) InstanceID() string { return f.instanceID }

// TestHubTelemetryIdentity checks metrics and traces are wired from the hub's
// instance ID, not its hub ID (ptone/scion#3644).
func TestHubTelemetryIdentity(t *testing.T) {
	id := newHubTelemetryIdentity(fakeTelemetrySource{hubID: "hub-a", instanceID: "pod-0-1234"}, "Hub A")
	if id.hubID != "hub-a" || id.hubName != "Hub A" || id.instanceID != "pod-0-1234" {
		t.Fatalf("identity = %+v, want hub-a / Hub A / pod-0-1234", id)
	}
	if n := len(id.metricsOptions()); n != 3 {
		t.Errorf("metrics options = %d, want hub ID, hub name and instance ID", n)
	}
	if n := len(id.tracingOptions()); n != 3 {
		t.Errorf("tracing options = %d, want hub ID, hub name and instance ID", n)
	}
}

// TestHubTelemetryIdentityFallback checks an empty instance ID is replaced
// once, by a UUID both signals share.
func TestHubTelemetryIdentityFallback(t *testing.T) {
	id := newHubTelemetryIdentity(fakeTelemetrySource{hubID: "hub-a"}, "")
	if _, err := uuid.Parse(id.instanceID); err != nil {
		t.Fatalf("fallback instance ID %q is not a UUID: %v", id.instanceID, err)
	}
	if id.instanceID == "hub-a" {
		t.Fatal("fallback must not reuse the hub ID")
	}
}
