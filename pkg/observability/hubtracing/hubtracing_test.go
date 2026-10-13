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

package hubtracing

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

func instanceIDOf(t *testing.T, res *resource.Resource) string {
	t.Helper()
	v, ok := res.Set().Value(semconv.ServiceInstanceIDKey)
	if !ok {
		t.Fatalf("resource has no %s: %v", semconv.ServiceInstanceIDKey, res.Attributes())
	}
	return v.AsString()
}

// TestResourceInstanceID checks hub spans carry the replica's instance ID as
// service.instance.id (ptone/scion#3644).
func TestResourceInstanceID(t *testing.T) {
	t.Setenv("SCION_HUB_ID", "")
	o := &options{serviceName: "scion-server", hubID: "hub-a"}
	WithInstanceID("hub-0-1234")(o)
	res, err := newResource(context.Background(), o)
	if err != nil {
		t.Fatalf("newResource: %v", err)
	}
	if got := instanceIDOf(t, res); got != "hub-0-1234" {
		t.Errorf("service.instance.id = %q, want %q", got, "hub-0-1234")
	}
	if v, _ := res.Set().Value("scion.hub.id"); v.AsString() != "hub-a" {
		t.Errorf("scion.hub.id = %q, want hub-a", v.AsString())
	}
}

// TestResourceInstanceIDFallback checks a missing instance ID still yields a
// distinct per-process service.instance.id.
func TestResourceInstanceIDFallback(t *testing.T) {
	t.Setenv("SCION_HUB_ID", "")
	a, err := newResource(context.Background(), &options{serviceName: "scion-server"})
	if err != nil {
		t.Fatalf("newResource: %v", err)
	}
	b, err := newResource(context.Background(), &options{serviceName: "scion-server"})
	if err != nil {
		t.Fatalf("newResource: %v", err)
	}
	idA, idB := instanceIDOf(t, a), instanceIDOf(t, b)
	if idA == "" || idA == idB {
		t.Errorf("generated instance IDs must be non-empty and distinct: %q, %q", idA, idB)
	}
}
