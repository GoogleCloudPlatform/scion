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
	"context"
	"testing"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	"github.com/GoogleCloudPlatform/scion/pkg/observability/hubmetrics"
	"github.com/GoogleCloudPlatform/scion/pkg/observability/hubtracing"
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

	mres, err := hubmetrics.NewResource(context.Background(), id.metricsOptions()...)
	if err != nil {
		t.Fatalf("metrics resource: %v", err)
	}
	tres, err := hubtracing.NewResource(context.Background(), id.tracingOptions()...)
	if err != nil {
		t.Fatalf("tracing resource: %v", err)
	}
	for signal, res := range map[string]*resource.Resource{"metrics": mres, "traces": tres} {
		if got := resourceAttr(res, semconv.ServiceInstanceIDKey); got != "pod-0-1234" {
			t.Errorf("%s service.instance.id = %q, want the hub instance ID pod-0-1234", signal, got)
		}
		if got := resourceAttr(res, "scion.hub.id"); got != "hub-a" {
			t.Errorf("%s scion.hub.id = %q, want hub-a", signal, got)
		}
		if got := resourceAttr(res, "scion.hub.name"); got != "Hub A" {
			t.Errorf("%s scion.hub.name = %q, want Hub A", signal, got)
		}
	}
}

func resourceAttr(res *resource.Resource, key attribute.Key) string {
	v, _ := res.Set().Value(key)
	return v.AsString()
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
	mres, err := hubmetrics.NewResource(context.Background(), id.metricsOptions()...)
	if err != nil {
		t.Fatalf("metrics resource: %v", err)
	}
	tres, err := hubtracing.NewResource(context.Background(), id.tracingOptions()...)
	if err != nil {
		t.Fatalf("tracing resource: %v", err)
	}
	m, tr := resourceAttr(mres, semconv.ServiceInstanceIDKey), resourceAttr(tres, semconv.ServiceInstanceIDKey)
	if m != id.instanceID || tr != id.instanceID {
		t.Fatalf("service.instance.id metrics=%q traces=%q, want both %q", m, tr, id.instanceID)
	}
}
