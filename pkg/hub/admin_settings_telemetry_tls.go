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
	"bytes"
	"encoding/json"
	"log/slog"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
)

// telemetryTLSTouched reports whether a server-config PUT body sets or
// changes telemetry.cloud.tls: it sends a tls member inside
// telemetry.cloud (any value, null and {} included), or it sends
// telemetry.cloud as null, which clears tls along with the rest of cloud.
// Members are matched the way the typed request decode matches them
// (telemetryPresence, sentFold), so a case variant such as "TLS" counts.
func telemetryTLSTouched(rawBody []byte) bool {
	cloud, ok := telemetryPresence(rawBody).sentFold("cloud")
	if !ok {
		return false
	}
	cloud = bytes.TrimSpace(cloud)
	if string(cloud) == "null" {
		return true
	}
	if !isJSONObject(cloud) {
		return false
	}
	cp, err := parseFieldPresence(cloud)
	if err != nil {
		return false
	}
	_, ok = cp.sentFold("tls")
	return ok
}

// telemetryTLSSaveErrors applies the telemetry.cloud.tls unit rule
// (opsettings.ValidateTelemetryTLS) to the merged telemetry section
// document of a save. A save that touches tls (telemetryTLSTouched) gets
// the errors, so it is refused and nothing is written. A save that does not
// touch tls never fails on it: an invalid tls already stored is kept as
// stored, byte for byte, and logged on logger, so pre-existing state never
// blocks an unrelated save and is never silently trimmed.
func telemetryTLSSaveErrors(doc json.RawMessage, touched bool, logger *slog.Logger) []config.ValidationError {
	errs := opsettings.ValidateTelemetryTLS(doc)
	if len(errs) == 0 || touched {
		return errs
	}
	logger.Warn("admin settings save: stored telemetry.cloud.tls is invalid; kept unchanged",
		"rule", errs[0].Message)
	return nil
}
