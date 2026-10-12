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
	"encoding/json"
	"testing"
)

func TestAdminSignal_MarshalRoundTrip(t *testing.T) {
	tests := []struct {
		name   string
		signal AdminSignal
	}{
		{
			name: "config signal",
			signal: AdminSignal{
				Integration: "discord",
				Kind:        "config",
			},
		},
		{
			name: "update signal with id",
			signal: AdminSignal{
				Integration: "telegram",
				Kind:        "update",
				ID:          "550e8400-e29b-41d4-a716-446655440000",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.signal)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}

			var decoded AdminSignal
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}

			if decoded.Integration != tt.signal.Integration {
				t.Errorf("Integration: got %q, want %q", decoded.Integration, tt.signal.Integration)
			}
			if decoded.Kind != tt.signal.Kind {
				t.Errorf("Kind: got %q, want %q", decoded.Kind, tt.signal.Kind)
			}
			if decoded.ID != tt.signal.ID {
				t.Errorf("ID: got %q, want %q", decoded.ID, tt.signal.ID)
			}
		})
	}
}

func TestAdminSignalChannel(t *testing.T) {
	if adminSignalChannel != "scion_integration_admin" {
		t.Errorf("channel name: got %q, want %q", adminSignalChannel, "scion_integration_admin")
	}
}
