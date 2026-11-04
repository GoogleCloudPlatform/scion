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

package lifecycleevents

import "github.com/GoogleCloudPlatform/scion/pkg/telemetryemit"

// SinkEnvironment is what the server knows about where it runs, for the
// lifecycle sink selection (design section 22 item 4).
type SinkEnvironment struct {
	// CloudLogging: server Cloud Logging is enabled (the server has a
	// ResilientCloudHandler, through SCION_CLOUD_LOGGING or the settings
	// switch).
	CloudLogging bool
	// LoopbackOnly: every listener the server binds is a loopback address.
	LoopbackOnly bool
	// SQLite: the hub store is SQLite.
	SQLite bool
	// DevAuth: the server runs with dev auth.
	DevAuth bool
	// CloudRunService: cloudrunenv.IsService() is true.
	CloudRunService bool
	// Kubernetes: KUBERNETES_SERVICE_HOST is set.
	Kubernetes bool
}

// SelectSinkKind decides the lifecycle sink (vm-deploy 07:54/55, conditions
// (a) and (b)):
//   - Cloud Logging (scion-analytics) whenever server Cloud Logging is
//     enabled;
//   - stdout only on a local workstation server, which requires ALL of:
//     loopback-only listeners, a SQLite store, dev auth, not a Cloud Run
//     service and not on Kubernetes (fail closed: any doubt means no
//     stdout sink, so a hosted hub never marks events delivered to a
//     stdout that does not reach scion-analytics);
//   - otherwise none, and the observer then refuses with
//     disabled_no_sink.
func SelectSinkKind(env SinkEnvironment) string {
	switch {
	case env.CloudLogging:
		return telemetryemit.SinkKindCloudLogging
	case env.LoopbackOnly && env.SQLite && env.DevAuth && !env.CloudRunService && !env.Kubernetes:
		return telemetryemit.SinkKindStdout
	default:
		return telemetryemit.SinkKindNone
	}
}
