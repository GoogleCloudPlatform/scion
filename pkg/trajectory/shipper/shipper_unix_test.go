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

//go:build unix

package shipper_test

import (
	"context"
	"os"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/trajectory/shipper"
)

func TestStateAndLockPrivatePermissionsUnix(t *testing.T) {
	home := t.TempDir()
	s, err := shipper.New(shipper.Config{
		HomeDir: home,
		Now:     fixedClock(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.Baseline(context.Background()); err != nil {
		t.Fatalf("Baseline: %v", err)
	}

	dirInfo, err := os.Stat(shipper.StateDir(home))
	if err != nil {
		t.Fatalf("stat state dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("state dir mode = %04o, want no group/other access", perm)
	}

	stateInfo, err := os.Stat(shipper.StatePath(home))
	if err != nil {
		t.Fatalf("stat state file: %v", err)
	}
	if perm := stateInfo.Mode().Perm(); perm != 0o600 {
		t.Fatalf("state.json mode = %04o, want 0600", perm)
	}

	lockInfo, err := os.Stat(shipper.LockPath(home))
	if err != nil {
		t.Fatalf("stat lock file: %v", err)
	}
	if perm := lockInfo.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("state.lock mode = %04o, want no group/other access", perm)
	}
}
