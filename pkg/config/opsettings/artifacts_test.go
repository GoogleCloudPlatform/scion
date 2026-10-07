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
	"testing"
)

func TestArtifactsResolve_AbsentFieldsTakeDefaults(t *testing.T) {
	got, err := ArtifactsSettings{}.Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := ArtifactsConfig{
		Enabled:              true,
		MaxFileBytes:         32 << 20,
		MaxBundleBytes:       256 << 20,
		MaxFiles:             200,
		DefaultRetentionDays: 0,
		LinkDefaultTTLHours:  168,
		LinkMaxTTLHours:      720,

		RemoteImagesEnabled:      true,
		RemoteImageMaxCount:      32,
		RemoteImageMaxBytes:      5 << 20,
		RemoteImageFetchTimeoutS: 10,
		RemoteImageTotalBudgetS:  30,
	}
	if got != want {
		t.Fatalf("Resolve() = %+v, want %+v", got, want)
	}
	if DefaultArtifactsConfig() != want {
		t.Fatalf("DefaultArtifactsConfig() = %+v, want %+v", DefaultArtifactsConfig(), want)
	}
}

func TestParseArtifactsDoc(t *testing.T) {
	malformed := MalformedArtifactsConfig()
	if malformed.Enabled || malformed.RemoteImagesEnabled || !malformed.Malformed {
		t.Fatalf("MalformedArtifactsConfig() = %+v, want disabled (remote images too) and malformed", malformed)
	}

	tests := []struct {
		name    string
		raw     string
		want    ArtifactsConfig
		wantErr bool
	}{
		{name: "empty object", raw: `{}`, want: DefaultArtifactsConfig()},
		{
			name: "every field set",
			raw:  `{"enabled":false,"max_file_bytes":1024,"max_bundle_bytes":4096,"max_files":3,"default_retention_days":30,"link_default_ttl_hours":24,"link_max_ttl_hours":48,"remote_images_enabled":false,"remote_image_max_count":3,"remote_image_max_bytes":512,"remote_image_fetch_timeout_s":3,"remote_image_total_budget_s":9}`,
			want: ArtifactsConfig{Enabled: false, MaxFileBytes: 1024, MaxBundleBytes: 4096, MaxFiles: 3, DefaultRetentionDays: 30, LinkDefaultTTLHours: 24, LinkMaxTTLHours: 48,
				RemoteImagesEnabled: false, RemoteImageMaxCount: 3, RemoteImageMaxBytes: 512, RemoteImageFetchTimeoutS: 3, RemoteImageTotalBudgetS: 9},
		},
		{
			name: "file limit equal to bundle limit",
			raw:  `{"max_file_bytes":4096,"max_bundle_bytes":4096}`,
			want: func() ArtifactsConfig {
				c := DefaultArtifactsConfig()
				c.MaxFileBytes, c.MaxBundleBytes, c.RemoteImageMaxBytes = 4096, 4096, 4096
				return c
			}(),
		},
		{
			// A document written before remote images existed stays valid:
			// the omitted remote caps follow the lower file limits.
			name: "lower file count clamps the remote image count",
			raw:  `{"max_files":10}`,
			want: func() ArtifactsConfig {
				c := DefaultArtifactsConfig()
				c.MaxFiles, c.RemoteImageMaxCount = 10, 10
				return c
			}(),
		},
		{
			name: "lower file size clamps the remote image size",
			raw:  `{"max_file_bytes":1048576}`,
			want: func() ArtifactsConfig {
				c := DefaultArtifactsConfig()
				c.MaxFileBytes, c.RemoteImageMaxBytes = 1048576, 1048576
				return c
			}(),
		},
		{name: "invalid JSON", raw: `{"enabled":`, want: malformed, wantErr: true},
		{name: "not an object", raw: `[1,2]`, want: malformed, wantErr: true},
		{name: "wrong type", raw: `{"max_files":"many"}`, want: malformed, wantErr: true},
		{name: "enabled wrong type", raw: `{"enabled":"yes"}`, want: malformed, wantErr: true},
		{name: "zero file limit", raw: `{"max_file_bytes":0}`, want: malformed, wantErr: true},
		{name: "negative bundle limit", raw: `{"max_bundle_bytes":-1}`, want: malformed, wantErr: true},
		{name: "zero max files", raw: `{"max_files":0}`, want: malformed, wantErr: true},
		{name: "negative retention", raw: `{"default_retention_days":-1}`, want: malformed, wantErr: true},
		{name: "zero default link TTL", raw: `{"link_default_ttl_hours":0}`, want: malformed, wantErr: true},
		{name: "zero max link TTL", raw: `{"link_max_ttl_hours":0}`, want: malformed, wantErr: true},
		{name: "file limit above bundle limit", raw: `{"max_file_bytes":2048,"max_bundle_bytes":1024}`, want: malformed, wantErr: true},
		{name: "default TTL above max TTL", raw: `{"link_default_ttl_hours":800}`, want: malformed, wantErr: true},
		{name: "remote images enabled wrong type", raw: `{"remote_images_enabled":"yes"}`, want: malformed, wantErr: true},
		{name: "malformed doc with enabled true stays disabled", raw: `{"enabled":true,"max_files":-5}`, want: malformed, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseArtifactsDoc(json.RawMessage(tt.raw))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestArtifactsSchema(t *testing.T) {
	valid := []string{
		`{}`,
		`{"enabled":true,"max_file_bytes":1,"max_bundle_bytes":1,"max_files":1,"default_retention_days":0,"link_default_ttl_hours":1,"link_max_ttl_hours":1}`,
	}
	for _, doc := range valid {
		if errs := Validate("artifacts", json.RawMessage(doc)); len(errs) != 0 {
			t.Errorf("Validate(%s) = %v, want no errors", doc, errs)
		}
	}
	invalid := []string{
		`{"max_file_bytes":0}`,
		`{"max_files":-1}`,
		`{"default_retention_days":-1}`,
		`{"link_max_ttl_hours":0}`,
		`{"enabled":"yes"}`,
		`{"remote_image_max_count":0}`,
		`{"remote_image_max_bytes":0}`,
		`{"remote_images_enabled":1}`,
		`{"remote_image_total_budget_s":61}`,
		`{"remote_image_fetch_timeout_s":61}`,
		`{"max_files":1.5}`,
		`{"unknown":1}`,
	}
	for _, doc := range invalid {
		if errs := Validate("artifacts", json.RawMessage(doc)); len(errs) == 0 {
			t.Errorf("Validate(%s) = no errors, want a rejection", doc)
		}
	}
}

// TestParseArtifactsDocRemoteImageValues: an invalid remote image value
// turns remote images off and names the problem; the service stays on.
func TestParseArtifactsDocRemoteImageValues(t *testing.T) {
	for _, tc := range []struct{ name, raw string }{
		{"zero remote image count", `{"remote_image_max_count":0}`},
		{"zero remote image bytes", `{"remote_image_max_bytes":0}`},
		{"zero remote fetch timeout", `{"remote_image_fetch_timeout_s":0}`},
		{"negative remote budget", `{"remote_image_total_budget_s":-1}`},
		{"remote image larger than a file", `{"max_file_bytes":1024,"max_bundle_bytes":4096,"remote_image_max_bytes":2048}`},
		{"more remote images than files", `{"max_files":10,"remote_image_max_count":11}`},
		{"remote budget below one fetch timeout", `{"remote_image_fetch_timeout_s":20,"remote_image_total_budget_s":10}`},
		{"remote budget above the cap", `{"remote_image_total_budget_s":61}`},
	} {
		got, err := ParseArtifactsDoc(json.RawMessage(tc.raw))
		if err != nil || got.Malformed || !got.Enabled {
			t.Errorf("%s: service disabled: %+v, %v", tc.name, got, err)
		}
		if got.RemoteImagesEnabled || got.RemoteImagesInvalid == "" {
			t.Errorf("%s: remote images still on: %+v", tc.name, got)
		}
	}
	// Explicit values within the limits keep remote images on.
	got, err := ParseArtifactsDoc(json.RawMessage(`{"max_files":10,"remote_image_max_count":10}`))
	if err != nil || !got.RemoteImagesEnabled || got.RemoteImagesInvalid != "" {
		t.Errorf("valid explicit values: %+v, %v", got, err)
	}
}
