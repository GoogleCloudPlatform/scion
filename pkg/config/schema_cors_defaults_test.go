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

package config

import (
	"encoding/json"
	"testing"
)

// The settings schema must not publish a default that turns CORS on or
// allows every origin: schema-aware editors insert published defaults on
// completion. corsConfig is shared by server.hub.cors and
// server.broker.cors.
func TestSettingsSchema_CORSConfigPublishesNoWildcardDefault(t *testing.T) {
	raw, err := schemasFS.ReadFile("schemas/settings-v1.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Defs map[string]struct {
			Properties map[string]map[string]json.RawMessage `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	cors, ok := schema.Defs["corsConfig"]
	if !ok {
		t.Fatal("settings schema has no $defs.corsConfig")
	}
	for _, field := range []string{"enabled", "allowed_origins"} {
		prop, ok := cors.Properties[field]
		if !ok {
			t.Fatalf("corsConfig has no %q property", field)
		}
		if d, ok := prop["default"]; ok {
			t.Errorf("corsConfig.%s publishes default %s, want none", field, d)
		}
	}
}
