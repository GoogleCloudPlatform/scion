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
	"testing"
)

// TestApplyLiveSignInPolicy_NilUser_FailsClosed is a direct regression on
// applyLiveSignInPolicy's own nil guard: every caller finds its user record
// through a different store call, so the guard lives once in the shared
// helper rather than being re-implemented per caller. A nil user record
// must be rejected with a clean error, not a nil-pointer fault.
func TestApplyLiveSignInPolicy_NilUser_FailsClosed(t *testing.T) {
	got, err := applyLiveSignInPolicy(context.Background(), signInPolicyDeps{}, nil, "", "", false, signInPolicyPersistOpts{})
	if err == nil {
		t.Fatalf("expected an error for a nil user record, got user=%+v", got)
	}
	if got != nil {
		t.Fatalf("expected a nil user on error, got %+v", got)
	}
}
