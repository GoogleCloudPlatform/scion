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

import "strings"

// cloudLoggingHealthKey is the non-critical /healthz key for the direct
// Cloud Logging path. It is outside criticalHealthChecks: a degraded value
// makes the composite status degraded (serving), never unhealthy, and
// readiness is independent of it.
const cloudLoggingHealthKey = "cloud_logging"

// cloudLoggingDegradedUnknown replaces a provider value that is neither
// "healthy" nor a "degraded: ..." string, so the key can only ever degrade.
const cloudLoggingDegradedUnknown = HealthStatusDegraded + ": unknown"

// SetCloudLoggingHealth injects the provider for the cloud_logging /healthz
// key. cmd calls it only when a Cloud Logging handler is configured, so the
// key is absent otherwise; nil removes it. The provider must be cheap and
// non-blocking and return a fixed string: "healthy",
// "degraded: circuit open" or "degraded: recent write failures"
// (logging.CloudWriteStats.HealthStatus). pkg/hub does not depend on the
// Cloud Logging handler type.
func (s *Server) SetCloudLoggingHealth(provider func() string) {
	if provider == nil {
		s.cloudLoggingHealth.Store(nil)
		return
	}
	s.cloudLoggingHealth.Store(&provider)
}

// checkCloudLoggingHealth adds the cloud_logging key when a provider is set.
func (s *Server) checkCloudLoggingHealth(checks map[string]string) {
	p := s.cloudLoggingHealth.Load()
	if p == nil {
		return
	}
	v := (*p)()
	if v != HealthStatusHealthy && !strings.HasPrefix(v, HealthStatusDegraded+": ") {
		v = cloudLoggingDegradedUnknown
	}
	checks[cloudLoggingHealthKey] = v
}
