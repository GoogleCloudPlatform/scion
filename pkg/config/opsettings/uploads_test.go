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
	}
	for _, doc := range valid {
		if errs := Validate(UploadsSectionName, json.RawMessage(doc)); len(errs) > 0 {
			t.Errorf("Validate(%s) unexpected errors: %v", doc, errs)
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
