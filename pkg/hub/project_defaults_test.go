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
	"context"
	"encoding/json"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// --- ProjectDefaultScratchpad() unit tests ---

func TestProjectDefaultScratchpad_AbsentSection(t *testing.T) {
	// No project_defaults section in the cache → compiled default (true).
	fakeStore := newFakeHubSettingStore()
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	_, _ = ops.Refresh(context.Background())

	if got := ops.ProjectDefaultScratchpad(); !got {
		t.Error("expected true (compiled default) when section is absent, got false")
	}
}

func TestProjectDefaultScratchpad_ExplicitTrue(t *testing.T) {
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("project_defaults", json.RawMessage(`{"default_scratchpad":true}`))
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	_, _ = ops.Refresh(context.Background())

	if got := ops.ProjectDefaultScratchpad(); !got {
		t.Error("expected true when explicitly set to true, got false")
	}
}

func TestProjectDefaultScratchpad_ExplicitFalse(t *testing.T) {
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("project_defaults", json.RawMessage(`{"default_scratchpad":false}`))
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	_, _ = ops.Refresh(context.Background())

	if got := ops.ProjectDefaultScratchpad(); got {
		t.Error("expected false when explicitly set to false, got true")
	}
}

func TestProjectDefaultScratchpad_EmptyDoc(t *testing.T) {
	// Section exists in DB but field is omitted → compiled default (true).
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("project_defaults", json.RawMessage(`{}`))
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	_, _ = ops.Refresh(context.Background())

	if got := ops.ProjectDefaultScratchpad(); !got {
		t.Error("expected true (compiled default) when field is omitted, got false")
	}
}

func TestProjectDefaultScratchpad_InvalidJSON(t *testing.T) {
	// Section exists but contains unparseable JSON → compiled default (true).
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("project_defaults", json.RawMessage(`{invalid`))
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	_, _ = ops.Refresh(context.Background())

	if got := ops.ProjectDefaultScratchpad(); !got {
		t.Error("expected true (compiled default) on parse error, got false")
	}
}

// --- defaultProjectSharedDirs() unit tests ---

func TestDefaultProjectSharedDirs_NoOps(t *testing.T) {
	// No OperationalSettings (file/SQLite mode) → compiled default (scratchpad ON).
	srv := &Server{}
	dirs := srv.defaultProjectSharedDirs()
	if len(dirs) != 1 {
		t.Fatalf("expected 1 shared dir, got %d", len(dirs))
	}
	if dirs[0].Name != "scratchpad" {
		t.Errorf("expected name=scratchpad, got %q", dirs[0].Name)
	}
	if dirs[0].ReadOnly {
		t.Error("expected ReadOnly=false")
	}
	if dirs[0].InWorkspace {
		t.Error("expected InWorkspace=false")
	}
}

func TestDefaultProjectSharedDirs_Enabled(t *testing.T) {
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("project_defaults", json.RawMessage(`{"default_scratchpad":true}`))
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	_, _ = ops.Refresh(context.Background())

	srv := &Server{}
	srv.operationalSettings.Store(ops)

	dirs := srv.defaultProjectSharedDirs()
	if len(dirs) != 1 || dirs[0].Name != "scratchpad" {
		t.Errorf("expected [scratchpad], got %v", dirs)
	}
}

func TestDefaultProjectSharedDirs_Disabled(t *testing.T) {
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("project_defaults", json.RawMessage(`{"default_scratchpad":false}`))
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	_, _ = ops.Refresh(context.Background())

	srv := &Server{}
	srv.operationalSettings.Store(ops)

	dirs := srv.defaultProjectSharedDirs()
	if dirs != nil {
		t.Errorf("expected nil when disabled, got %v", dirs)
	}
}

func TestDefaultProjectSharedDirs_SharedDirSpec(t *testing.T) {
	// Verify the returned SharedDir matches the design spec exactly.
	srv := &Server{}
	dirs := srv.defaultProjectSharedDirs()
	expected := api.SharedDir{Name: "scratchpad", ReadOnly: false, InWorkspace: false}
	if len(dirs) != 1 || dirs[0] != expected {
		t.Errorf("expected %+v, got %+v", expected, dirs)
	}
}
