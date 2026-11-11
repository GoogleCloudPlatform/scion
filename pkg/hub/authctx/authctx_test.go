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

package authctx

import (
	"context"
	"math"
	"testing"
)

// Fails if SessionGeneration reports ok=true for a context that was never
// given a value (for example if it returned the zero value with ok=true).
func TestSessionGeneration_UnsetIsNotOK(t *testing.T) {
	gen, ok := SessionGeneration(context.Background())
	if ok || gen != 0 {
		t.Fatalf("SessionGeneration(unset) = (%d, %v), want (0, false)", gen, ok)
	}
	//nolint:staticcheck // SA1012: a nil context must be handled without panicking.
	if gen, ok := SessionGeneration(nil); ok || gen != 0 {
		t.Fatalf("SessionGeneration(nil) = (%d, %v), want (0, false)", gen, ok)
	}
	// A value stored under a different key type must not be read back.
	type otherKey struct{}
	ctx := context.WithValue(context.Background(), otherKey{}, int64(7))
	if gen, ok := SessionGeneration(ctx); ok || gen != 0 {
		t.Fatalf("SessionGeneration(foreign key) = (%d, %v), want (0, false)", gen, ok)
	}
}

// Fails if the setter or getter narrows, widens or otherwise changes the
// int64 value, or if the getter reads a different key than the setter writes.
func TestSessionGeneration_RoundTrip(t *testing.T) {
	for _, want := range []int64{1, 2, 41, math.MaxInt64, -1, math.MinInt64} {
		ctx := WithSessionGeneration(context.Background(), want)
		got, ok := SessionGeneration(ctx)
		if !ok || got != want {
			t.Fatalf("round trip of %d = (%d, %v), want (%d, true)", want, got, ok, want)
		}
	}
	// The innermost setter wins.
	ctx := WithSessionGeneration(WithSessionGeneration(context.Background(), 3), 4)
	if got, ok := SessionGeneration(ctx); !ok || got != 4 {
		t.Fatalf("nested setter = (%d, %v), want (4, true)", got, ok)
	}
}

// Fails if a stored generation of 0 is reported as absent (ok=false): 0 is
// the generation of a never-revoked user and must stay distinguishable
// from "not authenticated by the hub-JWT arm".
func TestSessionGeneration_ZeroIsOK(t *testing.T) {
	ctx := WithSessionGeneration(context.Background(), 0)
	gen, ok := SessionGeneration(ctx)
	if !ok || gen != 0 {
		t.Fatalf("SessionGeneration(0) = (%d, %v), want (0, true)", gen, ok)
	}
}
