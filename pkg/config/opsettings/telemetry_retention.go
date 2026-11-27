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

package opsettings

import (
	"encoding/json"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// ValidateTelemetryRetention applies config.ValidateV1TelemetryRetention to a
// telemetry section document. With no retention object the document is valid.
func ValidateTelemetryRetention(doc json.RawMessage) []config.ValidationError {
	var ts TelemetrySettings
	if err := json.Unmarshal(doc, &ts); err != nil {
		return []config.ValidationError{{Message: "telemetry: " + err.Error()}}
	}
	if ts.Retention == nil {
		return nil
	}
	if err := config.ValidateV1TelemetryRetention(ts.Retention); err != nil {
		return []config.ValidationError{{Path: "retention", Message: err.Error()}}
	}
	return nil
}
