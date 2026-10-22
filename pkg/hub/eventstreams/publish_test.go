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

package eventstreams

import (
	"context"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/GoogleCloudPlatform/scion/pkg/observability/ssemetrics"
)

func TestTrigger(t *testing.T) {
	ctx := context.Background()
	if got := Trigger(ctx); got != ssemetrics.TriggerChange {
		t.Errorf("Trigger(plain) = %q, want %q", got, ssemetrics.TriggerChange)
	}
	if got := Trigger(WithHeartbeatTrigger(ctx)); got != ssemetrics.TriggerHeartbeat {
		t.Errorf("Trigger(heartbeat) = %q, want %q", got, ssemetrics.TriggerHeartbeat)
	}
	type other struct{}
	derived := context.WithValue(WithHeartbeatTrigger(ctx), other{}, 1)
	if got := Trigger(derived); got != ssemetrics.TriggerHeartbeat {
		t.Errorf("Trigger(derived) = %q, want %q", got, ssemetrics.TriggerHeartbeat)
	}
	//nolint:staticcheck // a nil context must not panic
	if got := Trigger(nil); got != ssemetrics.TriggerChange {
		t.Errorf("Trigger(nil) = %q, want %q", got, ssemetrics.TriggerChange)
	}
}

func TestSubjectClass(t *testing.T) {
	for subject, want := range map[string]string{
		"agent.a1.status":                "agent.status",
		"agent.a1.message":               "agent.message",
		"project.p1.agent.status":        "project.agent.status",
		"project.p1.chat.message.edited": "project.chat.message.edited",
		"project.p1.created":             "project.created",
		"user.u1.chat.read-state":        "user.chat.read-state",
		"broker.b1.status":               "broker.status",
		"broker.dispatch.d1.done":        "broker.dispatch.done",
		"system.images.job-1":            "system.images",
		"admin.allowlist.changed":        "admin.allowlist.changed",
		"admin.settings.updated":         "admin.settings.updated",
		"notification.created":           "notification.created",
		"conduit.authz.changed":          "conduit.authz.changed",
		"agent":                          "agent",
		"":                               "other",
		"mystery.x.y":                    "other",
		"system.other.thing":             "other",
	} {
		if got := SubjectClass(subject); got != want {
			t.Errorf("SubjectClass(%q) = %q, want %q", subject, got, want)
		}
	}
}

func TestPublishCounter(t *testing.T) {
	var zero PublishCounter
	zero.Count("agent.a.status", ssemetrics.TriggerChange) // no recorder: no panic

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	rec, err := ssemetrics.New(mp)
	if err != nil {
		t.Fatalf("ssemetrics.New: %v", err)
	}
	var c PublishCounter
	c.SetMetrics(rec)
	c.Count("agent.a1.status", ssemetrics.TriggerHeartbeat)
	c.Count("agent.a2.status", ssemetrics.TriggerHeartbeat)
	c.Count("project.p1.agent.status", ssemetrics.TriggerChange)

	got := int64Points(t, reader, ssemetrics.MetricEventsPublished)
	want := map[string]int64{
		"subject_class=agent.status,trigger=heartbeat":      2,
		"subject_class=project.agent.status,trigger=change": 1,
	}
	if len(got) != len(want) {
		t.Fatalf("published = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("published[%s] = %d, want %d", k, got[k], v)
		}
	}

	c.SetMetrics(nil)
	c.Count("agent.a1.status", ssemetrics.TriggerHeartbeat)
	if got := int64Points(t, reader, ssemetrics.MetricEventsPublished); got["subject_class=agent.status,trigger=heartbeat"] != 2 {
		t.Errorf("a disabled counter still counted: %v", got)
	}
}
