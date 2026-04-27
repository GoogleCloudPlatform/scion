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

package runtimebroker

import (
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

func TestEvaluateHarnessConfigPolicy(t *testing.T) {
	cases := []struct {
		name        string
		allow       bool
		entry       config.HarnessConfigEntry
		wantOK      bool
		wantStatus  int
		wantInError string
	}{
		{
			name:   "no provisioner is allowed",
			allow:  false,
			entry:  config.HarnessConfigEntry{Harness: "claude"},
			wantOK: true,
		},
		{
			name:  "builtin provisioner is allowed",
			allow: false,
			entry: config.HarnessConfigEntry{
				Harness:     "claude",
				Provisioner: &config.HarnessProvisionerConfig{Type: "builtin"},
			},
			wantOK: true,
		},
		{
			name:  "container-script blocked when allow=false",
			allow: false,
			entry: config.HarnessConfigEntry{
				Harness:     "claude",
				Provisioner: &config.HarnessProvisionerConfig{Type: "container-script"},
			},
			wantOK:      false,
			wantStatus:  403,
			wantInError: "allow_container_script_harnesses",
		},
		{
			name:  "container-script allowed when allow=true",
			allow: true,
			entry: config.HarnessConfigEntry{
				Harness:     "claude",
				Provisioner: &config.HarnessProvisionerConfig{Type: "container-script"},
			},
			wantOK: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{
				config: ServerConfig{AllowContainerScriptHarnesses: tc.allow},
			}
			d := s.evaluateHarnessConfigPolicy("test-harness", tc.entry)
			if d.OK != tc.wantOK {
				t.Errorf("OK=%v, want %v", d.OK, tc.wantOK)
			}
			if !tc.wantOK {
				if d.HTTPStatus != tc.wantStatus {
					t.Errorf("HTTPStatus=%d, want %d", d.HTTPStatus, tc.wantStatus)
				}
				if !strings.Contains(d.Message, tc.wantInError) {
					t.Errorf("Message=%q, want it to contain %q", d.Message, tc.wantInError)
				}
				if !strings.Contains(d.Message, "test-harness") {
					t.Errorf("Message=%q should mention the harness name", d.Message)
				}
			}
		})
	}
}
