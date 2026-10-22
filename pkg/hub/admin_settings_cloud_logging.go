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
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
)

// telemetryCloudLoggingKey is the settings key of the hub's direct Cloud
// Logging switch. The hub reads it once at startup, before the settings
// database is opened (SCION_CLOUD_LOGGING, else settings.yaml), so a value
// stored in the database is never used.
const telemetryCloudLoggingKey = "telemetry.cloud.cloud_logging"

// startupOnlyKeyRejectedCode is the error of a database-backed server-config
// write that changes a key the hub reads only from settings.yaml or the
// environment at startup.
const startupOnlyKeyRejectedCode = "startup_only_key_rejected"

// cloudLoggingDBWriteRefusal holds the result of checking a database-backed
// server-config body for telemetry.cloud.cloud_logging.
type cloudLoggingDBWriteRefusal struct {
	// body is the request body without any cloud_logging member that only
	// echoes the GET view, so an echo writes nothing.
	body []byte
	// refused is true when the body changes the value.
	refused bool
}

// checkCloudLoggingDBWrite refuses telemetry.cloud.cloud_logging in a
// database-backed server-config write (ptone/scion#5664, interim): a stored
// value would take effect neither live nor after a restart. An unchanged
// echo of the GET view is accepted and dropped from the body, so nothing is
// written for it; any other value, an explicit null included, is refused.
// Member names are matched the way settingsBodyKeyPaths matches them
// (case-folded, dotted names split). The body has already passed
// rejectRepeatedJSONMembers; one that does not parse is returned as it is
// (the typed decode reports it).
func (s *Server) checkCloudLoggingDBWrite(ctx context.Context, ops *OperationalSettings, rawBody []byte) (cloudLoggingDBWriteRefusal, error) {
	keep := cloudLoggingDBWriteRefusal{body: rawBody}
	if len(matchedSettingsKeys(settingsBodyKeyPaths(rawBody), []string{telemetryCloudLoggingKey})) == 0 {
		return keep, nil
	}
	dec := json.NewDecoder(bytes.NewReader(rawBody))
	dec.UseNumber()
	var root any
	if err := dec.Decode(&root); err != nil {
		return keep, nil
	}
	obj, ok := root.(map[string]any)
	if !ok {
		return keep, nil
	}
	view, err := s.serverConfigDBView(ctx, ops)
	if err != nil {
		return keep, err
	}
	var want []string
	for _, seg := range strings.Split(telemetryCloudLoggingKey, ".") {
		want = append(want, foldJSONMemberName(seg))
	}
	viewPath := strings.Split(telemetryCloudLoggingKey, ".")
	refused := false
	var walk func(prefix []string, obj map[string]any)
	walk = func(prefix []string, obj map[string]any) {
		for name, child := range obj {
			path := append([]string(nil), prefix...)
			for _, seg := range strings.Split(name, ".") {
				path = append(path, foldJSONMemberName(seg))
			}
			if slices.Equal(path, want) {
				raw, err := json.Marshal(child)
				if err != nil || !isEchoOfView(rawPath{path: viewPath, value: raw}, view) {
					refused = true
					continue
				}
				delete(obj, name)
				continue
			}
			if len(path) < len(want) && slices.Equal(path, want[:len(path)]) {
				if sub, ok := child.(map[string]any); ok && len(sub) > 0 {
					walk(path, sub)
					if len(sub) == 0 {
						delete(obj, name)
					}
				}
			}
		}
	}
	walk(nil, obj)
	if refused {
		return cloudLoggingDBWriteRefusal{body: rawBody, refused: true}, nil
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return keep, nil
	}
	return cloudLoggingDBWriteRefusal{body: out}, nil
}

// writeCloudLoggingDBWriteRefused answers a refused
// telemetry.cloud.cloud_logging write with a structured 422.
func writeCloudLoggingDBWriteRefused(w http.ResponseWriter) {
	writeJSON(w, http.StatusUnprocessableEntity, map[string]interface{}{
		"error": startupOnlyKeyRejectedCode,
		"message": "telemetry.cloud.cloud_logging is read at startup from SCION_CLOUD_LOGGING or settings.yaml; " +
			"a database-backed hub does not use a saved value. Set it in settings.yaml or the environment and restart.",
		"keys": []string{telemetryCloudLoggingKey},
	})
}
