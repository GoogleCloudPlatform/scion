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
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// TestGetHealthInfo_ProjectStoragePrecedence: both conditions that report on
// /healthz "project_storage" (invalid project storage settings, and a project
// storage share that is not visible on a shared Layout) share ONE key. Invalid
// settings win the value ("unavailable"); otherwise the share check's value
// is reported; the status degrades when either condition holds; the
// heartbeat carries the same normalised value.
func TestGetHealthInfo_ProjectStoragePrecedence(t *testing.T) {
	if healthCheckProjectStorage != shareHealthCheck {
		t.Fatalf("the two conditions must report on one key: %q vs %q", healthCheckProjectStorage, shareHealthCheck)
	}
	ctx := context.Background()
	f := newShareVisibleFixture(t, "kubernetes", "10.0.0.2:/scion-workspaces/")
	projects := filepath.Join(f.hostBase, "projects")
	setShare := func(visible bool) {
		t.Helper()
		if visible {
			if err := os.MkdirAll(projects, 0o755); err != nil {
				t.Fatal(err)
			}
		} else if err := os.RemoveAll(projects); err != nil {
			t.Fatal(err)
		}
		f.srv.checkShareHealth(ctx)
	}
	setInvalid := func(invalid bool) {
		if invalid {
			f.srv.config.StorageError = invalidStorageErr()
		} else {
			f.srv.config.StorageError = nil
		}
	}

	for _, tc := range []struct {
		name            string
		shareVisible    bool
		invalid         bool
		wantValue       string // exact value, or "" for the share-not-visible text
		wantStatus      string
		wantHeartbeatPS string
	}{
		{"neither", true, false, "healthy", "healthy", "healthy"},
		{"share not visible only", false, false, "", "degraded", "unhealthy"},
		{"invalid settings only", true, true, api.BrokerHealthUnavailable, "degraded", "unavailable"},
		{"both: invalid settings win the value", false, true, api.BrokerHealthUnavailable, "degraded", "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setShare(tc.shareVisible)
			setInvalid(tc.invalid)
			info := f.srv.GetHealthInfo(ctx)
			got, ok := info.Checks[healthCheckProjectStorage]
			if !ok {
				t.Fatalf("no %s check: %v", healthCheckProjectStorage, info.Checks)
			}
			if tc.wantValue == "" {
				if !strings.HasPrefix(got, shareHealthNotVisible+" (") {
					t.Errorf("%s = %q, want the share-not-visible value", healthCheckProjectStorage, got)
				}
				assertNoPathValues(t, f, got)
			} else if got != tc.wantValue {
				t.Errorf("%s = %q, want %q", healthCheckProjectStorage, got, tc.wantValue)
			}
			if info.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q (checks %v)", info.Status, tc.wantStatus, info.Checks)
			}
			hb := sendHealthHeartbeat(t, f.srv)
			if hb.Health == nil || hb.Health.Checks[healthCheckProjectStorage] != tc.wantHeartbeatPS || hb.Health.Status != tc.wantStatus {
				t.Errorf("heartbeat health = %+v, want %s=%s and status %s", hb.Health, healthCheckProjectStorage, tc.wantHeartbeatPS, tc.wantStatus)
			}
		})
	}
}
