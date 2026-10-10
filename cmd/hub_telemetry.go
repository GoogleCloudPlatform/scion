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
	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/observability/hubmetrics"
	"github.com/GoogleCloudPlatform/scion/pkg/observability/hubtracing"
)

// hubTelemetrySource is the part of the hub server that identifies it in
// exported telemetry.
type hubTelemetrySource interface {
	HubID() string
	InstanceID() string
}

// hubTelemetryIdentity is the identity the hub exports on both metrics and
// traces. It is resolved once, so the two signals always carry the same
// service.instance.id and can be joined per replica.
type hubTelemetryIdentity struct {
	hubID      string
	hubName    string
	instanceID string
}

// newHubTelemetryIdentity reads the hub and instance IDs from src. The hub
// always assigns an instance ID; if it ever comes back empty, one UUID is
// generated here and shared by both signals, rather than letting metrics and
// tracing each fall back to a different one.
func newHubTelemetryIdentity(src hubTelemetrySource, hubName string) hubTelemetryIdentity {
	id := hubTelemetryIdentity{hubID: src.HubID(), hubName: hubName, instanceID: src.InstanceID()}
	if id.instanceID == "" {
		id.instanceID = uuid.NewString()
	}
	return id
}

func (id hubTelemetryIdentity) metricsOptions() []hubmetrics.Option {
	return []hubmetrics.Option{
		hubmetrics.WithHubID(id.hubID),
		hubmetrics.WithHubName(id.hubName),
		hubmetrics.WithInstanceID(id.instanceID),
	}
}

func (id hubTelemetryIdentity) tracingOptions() []hubtracing.Option {
	return []hubtracing.Option{
		hubtracing.WithHubID(id.hubID),
		hubtracing.WithHubName(id.hubName),
		hubtracing.WithInstanceID(id.instanceID),
	}
}
