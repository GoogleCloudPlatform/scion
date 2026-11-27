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
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"
)

func TestUploads_SectionAndLiveApply(t *testing.T) {
	for _, key := range []string{
		config.HubUploadsMaxAttachmentSizeKey,
		config.HubUploadsMaxWorkspaceFileSizeKey,
	} {
		if got := OwningSection(key); got != UploadsSectionName {
			t.Errorf("OwningSection(%q) = %q, want %q", key, got, UploadsSectionName)
		}
	}

	info, ok := SchemaInfo()[UploadsSectionName]
	if !ok {
		t.Fatalf("SchemaInfo has no %q section", UploadsSectionName)
	}
	if len(info.RestartRequired) != 0 {
		t.Errorf("uploads RestartRequired = %v, want empty (live apply)", info.RestartRequired)
	}
}

func TestUploads_Validate(t *testing.T) {
	valid := []string{
		`{}`,
		`{"max_attachment_size":0}`,
		`{"max_attachment_size":20971520,"max_workspace_file_size":83886080}`,
		`{"max_attachment_size":104857600,"max_workspace_file_size":104857600}`,
		`{"max_attachment_size":20971520,"chunk_bytes":4194304,"max_sessions_per_user":8}`,
	}
	for _, doc := range valid {
		if errs := Validate(UploadsSectionName, json.RawMessage(doc)); len(errs) > 0 {
			t.Errorf("Validate(%s) unexpected errors: %v", doc, errs)
		}
		if _, err := ParseUploadsDoc(json.RawMessage(doc)); err != nil {
			t.Errorf("ParseUploadsDoc(%s) unexpected error: %v", doc, err)
		}
	}

	invalid := []string{
		`{"max_attachment_size":-1}`,
		`{"max_workspace_file_size":-1}`,
		`{"max_attachment_size":104857601}`,
		`{"max_workspace_file_size":1073741824}`,
		`{"unknown_key":123}`,
		`{"max_attachment_size":"big"}`,
	}
	for _, doc := range invalid {
		if errs := Validate(UploadsSectionName, json.RawMessage(doc)); len(errs) == 0 {
			t.Errorf("Validate(%s) expected error, got none", doc)
		}
	}
}

func TestUploads_SeedRoundTrip(t *testing.T) {
	k := koanf.New(".")
	if err := k.Load(confmap.Provider(map[string]interface{}{
		config.HubUploadsMaxAttachmentSizeKey:    int64(20 << 20),
		config.HubUploadsMaxWorkspaceFileSizeKey: int64(80 << 20),
	}, "."), nil); err != nil {
		t.Fatalf("load koanf: %v", err)
	}

	doc, err := ExtractSectionFromKoanf(k, UploadsSectionName)
	if err != nil {
		t.Fatalf("ExtractSectionFromKoanf: %v", err)
	}
	if errs := Validate(UploadsSectionName, doc); len(errs) > 0 {
		t.Fatalf("Validate extracted doc: %v", errs)
	}

	back, err := LoadSectionsIntoKoanf(map[string]json.RawMessage{UploadsSectionName: doc})
	if err != nil {
		t.Fatalf("LoadSectionsIntoKoanf: %v", err)
	}
	if got := back.Int64(config.HubUploadsMaxAttachmentSizeKey); got != 20<<20 {
		t.Errorf("max_attachment_size = %d, want %d", got, 20<<20)
	}
	if got := back.Int64(config.HubUploadsMaxWorkspaceFileSizeKey); got != 80<<20 {
		t.Errorf("max_workspace_file_size = %d, want %d", got, 80<<20)
	}
}

func TestUploadsDefaults(t *testing.T) {
	cfg, err := ParseUploadsDoc(json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("ParseUploadsDoc({}): %v", err)
	}
	if cfg.Malformed {
		t.Fatalf("expected Malformed=false")
	}
	if cfg.ChunkBytes != 8<<20 {
		t.Errorf("ChunkBytes = %d, want %d", cfg.ChunkBytes, 8<<20)
	}
	if cfg.SessionIdleTTL != time.Hour {
		t.Errorf("SessionIdleTTL = %s, want 1h", cfg.SessionIdleTTL)
	}
	if cfg.SessionMaxLifetime != 24*time.Hour {
		t.Errorf("SessionMaxLifetime = %s, want 24h", cfg.SessionMaxLifetime)
	}
	if cfg.MaxSessionsPerUser != 4 {
		t.Errorf("MaxSessionsPerUser = %d, want 4", cfg.MaxSessionsPerUser)
	}
	if cfg.MaxPendingBytesPerUser != 2<<30 {
		t.Errorf("MaxPendingBytesPerUser = %d, want %d", cfg.MaxPendingBytesPerUser, 2<<30)
	}
	if cfg.MaxPendingBytesHub != 16<<30 {
		t.Errorf("MaxPendingBytesHub = %d, want %d", cfg.MaxPendingBytesHub, 16<<30)
	}
}

func TestUploadsValidation(t *testing.T) {
	valid := []string{
		`{}`,
		`{"chunk_bytes":1048576,"session_idle_ttl":"5m","session_max_lifetime":"5m","max_sessions_per_user":1,"max_pending_bytes_per_user":1048576,"max_pending_bytes_hub":1048576}`,
		`{"chunk_bytes":16777216,"session_idle_ttl":"24h","session_max_lifetime":"7d","max_sessions_per_user":64,"max_pending_bytes_per_user":2147483648,"max_pending_bytes_hub":17179869184}`,
	}
	for _, doc := range valid {
		if errs := Validate(UploadsSectionName, json.RawMessage(doc)); len(errs) > 0 {
			t.Errorf("Validate(%s) unexpected errors: %v", doc, errs)
		}
		if _, err := ParseUploadsDoc(json.RawMessage(doc)); err != nil {
			t.Errorf("ParseUploadsDoc(%s) unexpected error: %v", doc, err)
		}
	}

	invalid := []struct {
		name string
		doc  string
		want string
	}{
		{"chunk_too_small", `{"chunk_bytes":1048575}`, ""},
		{"chunk_too_large", `{"chunk_bytes":16777217}`, ""},
		{"idle_too_short", `{"session_idle_ttl":"4m"}`, "session_idle_ttl"},
		{"idle_too_long", `{"session_idle_ttl":"25h"}`, "session_idle_ttl"},
		{"idle_bad_duration", `{"session_idle_ttl":"notaduration"}`, "session_idle_ttl"},
		{"max_lifetime_below_idle", `{"session_idle_ttl":"2h","session_max_lifetime":"1h"}`, "session_max_lifetime"},
		{"max_lifetime_too_long", `{"session_max_lifetime":"8d"}`, "session_max_lifetime"},
		{"sessions_too_low", `{"max_sessions_per_user":0}`, ""},
		{"sessions_too_high", `{"max_sessions_per_user":65}`, ""},
		{"pending_user_zero", `{"max_pending_bytes_per_user":0}`, ""},
		{"pending_hub_below_user", `{"max_pending_bytes_per_user":4294967296,"max_pending_bytes_hub":2147483648}`, "max_pending_bytes_hub"},
		{"unknown_key", `{"unknown_field":1}`, ""},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			if errs := Validate(UploadsSectionName, json.RawMessage(tc.doc)); len(errs) == 0 {
				t.Errorf("Validate(%s) expected error, got nil", tc.doc)
			}
			cfg, err := ParseUploadsDoc(json.RawMessage(tc.doc))
			if err == nil {
				t.Errorf("ParseUploadsDoc(%s) expected error, got nil", tc.doc)
			}
			if !cfg.Malformed {
				t.Errorf("expected Malformed=true on error")
			}
			if tc.want != "" && err != nil && !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestUploadsPendingBytesWarning(t *testing.T) {
	cfg := DefaultUploadsConfig()
	if w := cfg.PendingBytesWarning(1 << 30); w != "" {
		t.Errorf("unexpected warning for 1 GiB limit vs 2 GiB pending: %q", w)
	}
	if w := cfg.PendingBytesWarning(4 << 30); w == "" {
		t.Errorf("expected warning when max_pending_bytes_per_user < largestPerFileMax")
	}
}
