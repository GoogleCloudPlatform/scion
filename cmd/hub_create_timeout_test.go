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

package cmd

import (
	"testing"
	"time"
)

// TestHubCreateRequestTimeout_OutlastsSyncCreate pins the CLI's create and
// env-submit timeout above the hub's synchronous create path: one dispatch
// of up to the hub-to-broker request limit (180s, ptone/scion#6829) plus the
// hub's 30s response write slack. A shorter client timeout would give up
// before the hub answers.
func TestHubCreateRequestTimeout_OutlastsSyncCreate(t *testing.T) {
	const hubSyncCreate = 180*time.Second + 30*time.Second
	if hubCreateRequestTimeout <= hubSyncCreate {
		t.Fatalf("hubCreateRequestTimeout = %v, must exceed the hub's synchronous create path (%v)", hubCreateRequestTimeout, hubSyncCreate)
	}
	if hubCreateRequestTimeout != 4*time.Minute {
		t.Errorf("hubCreateRequestTimeout = %v, want 4m", hubCreateRequestTimeout)
	}
}
