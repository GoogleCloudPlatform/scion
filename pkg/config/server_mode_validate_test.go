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
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestValidateServerMode(t *testing.T) {
	for _, m := range []string{"", "workstation", "hosted", "production"} {
		if err := ValidateServerMode(m); err != nil {
			t.Errorf("ValidateServerMode(%q) = %v, want nil", m, err)
		}
	}
	for _, m := range []string{"Hosted", "prod", "standalone", "hosted "} {
		err := ValidateServerMode(m)
		if err == nil {
			t.Errorf("ValidateServerMode(%q) = nil, want an error", m)
			continue
		}
		for _, want := range []string{`"workstation"`, `"hosted"`, `"production"`, "--hosted"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q should name %s", err, want)
			}
		}
	}
}

// validServerModes must match the settings schema's server.mode enum.
func TestValidServerModes_MatchSchemaEnum(t *testing.T) {
	data, err := os.ReadFile("schemas/settings-v1.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties struct {
			Server struct {
				Properties struct {
					Mode struct {
						Enum []string `json:"enum"`
					} `json:"mode"`
				} `json:"properties"`
			} `json:"server"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	if got := schema.Properties.Server.Properties.Mode.Enum; !reflect.DeepEqual(got, validServerModes) {
		t.Errorf("schema server.mode enum = %v, validServerModes = %v", got, validServerModes)
	}
}
