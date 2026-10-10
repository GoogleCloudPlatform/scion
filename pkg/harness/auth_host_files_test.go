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

package harness

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

func writeHomeFile(t *testing.T, home, suffix string) string {
	t.Helper()
	p := filepath.Join(home, suffix)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestHostCredentialFiles(t *testing.T) {
	home := t.TempDir()
	oauth := writeHomeFile(t, home, "/.claude/.credentials.json")
	adc := writeHomeFile(t, home, "/.config/gcloud/application_default_credentials.json")

	meta := &config.HarnessAuthMetadata{
		Types: map[string]config.HarnessAuthTypeMetadata{
			"oauth": {RequiredFiles: []config.HarnessAuthFileRequirement{
				{Name: "claude-auth", Field: "ClaudeAuthFile", TargetSuffix: "/.claude/.credentials.json"},
				// Missing on disk: skipped.
				{Name: "missing", Field: "OtherFile", TargetSuffix: "/.other/creds.json"},
				// No field or suffix: skipped.
				{Name: "nofield", TargetSuffix: "/.claude/.credentials.json"},
				{Name: "nosuffix", Field: "NoSuffix"},
			}},
		},
	}
	got := HostCredentialFiles(meta, home)
	want := []HostCredentialFile{{Name: "claude-auth", Field: "ClaudeAuthFile", TargetSuffix: "/.claude/.credentials.json", Path: oauth}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("HostCredentialFiles = %+v, want %+v", got, want)
	}

	// Duplicate field across auth types: reported once.
	meta.Types["vertex"] = config.HarnessAuthTypeMetadata{RequiredFiles: []config.HarnessAuthFileRequirement{
		{Name: "gcloud-adc", Field: "GoogleAppCredentials", TargetSuffix: "/.config/gcloud/application_default_credentials.json"},
	}}
	meta.Types["vertex2"] = config.HarnessAuthTypeMetadata{RequiredFiles: []config.HarnessAuthFileRequirement{
		{Name: "gcloud-adc-dup", Field: "GoogleAppCredentials", TargetSuffix: "/.config/gcloud/application_default_credentials.json"},
	}}
	got = HostCredentialFiles(meta, home)
	// Types are visited in sorted order: oauth, vertex, vertex2.
	if len(got) != 2 || got[1].Name != "gcloud-adc" || got[1].Path != adc {
		t.Fatalf("expected 2 files (dedup by field, sorted type order), got %+v", got)
	}
	if m := gatherConfigFiles(meta, home); m["ClaudeAuthFile"] != oauth || m["GoogleAppCredentials"] != adc || len(m) != 2 {
		t.Fatalf("gatherConfigFiles = %v", m)
	}

	if HostCredentialFiles(nil, home) != nil || HostCredentialFiles(meta, "") != nil {
		t.Fatal("expected nil for nil meta or empty home")
	}
	if gatherConfigFiles(&config.HarnessAuthMetadata{}, home) != nil {
		t.Fatal("expected nil map when nothing is found")
	}
}
