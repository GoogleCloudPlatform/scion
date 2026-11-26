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
	"io/fs"
	"testing"

	harnessesEmbed "github.com/GoogleCloudPlatform/scion/harnesses"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Every bundled harness has a usage.yaml that passes the strict parse (the
// runtime loaders degrade a bad one to nil/unknown silently).
func TestEmbeddedHarnessConfigs_UsageSidecarStrict(t *testing.T) {
	names := AllHarnessNames()
	if len(names) == 0 {
		t.Fatal("AllHarnessNames returned empty list")
	}
	for _, name := range names {
		data, err := fs.ReadFile(harnessesEmbed.FS, name+"/"+config.UsageSidecarFile)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		caps, err := config.ParseUsageSidecar(data)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if caps == nil {
			t.Errorf("%s: ParseUsageSidecar returned nil caps without error", name)
		}
	}
}

// newFromEmbedFS wires usage.yaml onto the embedded harness entry.
func TestNewFromEmbedFS_LoadsUsageSidecar(t *testing.T) {
	h := newFromEmbedFS("claude")
	if h == nil {
		t.Fatal("newFromEmbedFS(\"claude\") returned nil")
	}
	d, ok := h.(*DeclarativeGenericHarness)
	if !ok {
		t.Fatalf("newFromEmbedFS(\"claude\") returned %T, want *DeclarativeGenericHarness", h)
	}
	if d.entry.UsageSidecar == nil {
		t.Fatal("claude embedded entry has nil UsageSidecar")
	}
	if d.entry.UsageSidecar.Live.Source != api.UsageLiveSourceNative {
		t.Errorf("claude UsageSidecar.Live.Source = %q, want %q", d.entry.UsageSidecar.Live.Source, api.UsageLiveSourceNative)
	}
}
