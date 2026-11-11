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

// ValidateTelemetryTLS applies the telemetry.cloud.tls rules of the
// settings file loader (config.ValidateV1TelemetryTLS: plaintext conflicts,
// cert_file and key_file set together) to a telemetry section document.
//
// It is deliberately not part of Validate. The rule is a property of the
// whole tls object, while Validate also judges stored keys one at a time
// when the hub prunes carried keys, and it runs on every write. The hub
// settings handlers call this only when a save sends cloud.tls, so stored
// state a save does not touch is never refused or trimmed because of it.
// With no cloud or no tls object the document is valid. Readability of
// the files is not checked: they are paths inside the agent container.
func ValidateTelemetryTLS(doc json.RawMessage) []config.ValidationError {
	var ts TelemetrySettings
	if err := json.Unmarshal(doc, &ts); err != nil {
		return []config.ValidationError{{Message: "telemetry: " + err.Error()}}
	}
	if ts.Cloud == nil || ts.Cloud.TLS == nil {
		return nil
	}
	if err := config.ValidateV1TelemetryTLS(ts.Cloud.TLS); err != nil {
		return []config.ValidationError{{Path: "cloud/tls", Message: err.Error()}}
	}
	return nil
}
