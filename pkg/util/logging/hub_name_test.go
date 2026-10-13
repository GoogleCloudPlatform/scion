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

package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
)

// gcpHubLabel logs one record through a GCPHandler built with startup and
// returns its "hub" label ("" when absent).
func gcpHubLabel(t *testing.T, startup string) string {
	t.Helper()
	var buf bytes.Buffer
	h := NewGCPHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}, "test-component", startup)
	// Derived handlers must follow the applied name too.
	slog.New(h).With("k", "v").WithGroup("g").Info("m")
	var data map[string]any
	if err := json.Unmarshal(buf.Bytes(), &data); err != nil {
		t.Fatalf("decode record: %v", err)
	}
	labels, _ := data[GCPKeyLabels].(map[string]any)
	got, _ := labels["hub"].(string)
	return got
}

// TestSetHubName_LabelsCarryAppliedName checks records carry the hub name
// applied from operational settings, not the startup name the handler was
// built with (ptone/scion#4903).
func TestSetHubName_LabelsCarryAppliedName(t *testing.T) {
	t.Cleanup(func() { SetHubName("") })

	if got := gcpHubLabel(t, "startup-hub"); got != "startup-hub" {
		t.Fatalf("before SetHubName: hub label = %q, want startup-hub", got)
	}

	SetHubName("applied-hub")
	if got := gcpHubLabel(t, "startup-hub"); got != "applied-hub" {
		t.Errorf("stored name differs: hub label = %q, want applied-hub", got)
	}
	if got := gcpHubLabel(t, ""); got != "applied-hub" {
		t.Errorf("handler built without a name: hub label = %q, want applied-hub", got)
	}

	SetHubName("startup-hub")
	if got := gcpHubLabel(t, "startup-hub"); got != "startup-hub" {
		t.Errorf("names match: hub label = %q, want startup-hub", got)
	}

	SetHubName("")
	if got := gcpHubLabel(t, "startup-hub"); got != "startup-hub" {
		t.Errorf("after clearing: hub label = %q, want startup-hub", got)
	}
	if got := gcpHubLabel(t, ""); got != "" {
		t.Errorf("after clearing, no name: hub label = %q, want none", got)
	}
}

// TestAddHubLabel checks the helper the Cloud Logging handlers (request,
// message and server logs) use to build the same label.
func TestAddHubLabel(t *testing.T) {
	t.Cleanup(func() { SetHubName("") })

	for _, tc := range []struct {
		name, applied, startup, want string
	}{
		{name: "unset keeps startup", startup: "startup-hub", want: "startup-hub"},
		{name: "unset and empty sets none"},
		{name: "applied differs", applied: "applied-hub", startup: "startup-hub", want: "applied-hub"},
		{name: "applied matches", applied: "startup-hub", startup: "startup-hub", want: "startup-hub"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			SetHubName(tc.applied)
			labels := map[string]string{}
			addHubLabel(labels, tc.startup)
			got, ok := labels["hub"]
			if tc.want == "" {
				if ok {
					t.Fatalf("hub label = %q, want none", got)
				}
				return
			}
			if got != tc.want {
				t.Fatalf("hub label = %q, want %q", got, tc.want)
			}
		})
	}
}
