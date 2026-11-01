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

package lifecycleevents

import (
	"context"
	"testing"
)

// TestWithoutResolution: both resolution marks are masked; other context
// values survive.
func TestWithoutResolution(t *testing.T) {
	ctx := WithDispatchResult(WithConfigResolution(context.Background()), DispatchResult{RunID: "r1", BrokerEvidence: true})
	ctx = WithCreateContext(ctx, CreateContext{CreatorType: "user", CreatorID: "u1"})
	if !ConfigResolutionFrom(ctx) {
		t.Fatal("precondition: config resolution set")
	}
	if _, ok := DispatchResultFrom(ctx); !ok {
		t.Fatal("precondition: dispatch result set")
	}
	got := WithoutResolution(ctx)
	if ConfigResolutionFrom(got) {
		t.Error("config resolution survived WithoutResolution")
	}
	if r, ok := DispatchResultFrom(got); ok {
		t.Errorf("dispatch result survived WithoutResolution: %+v", r)
	}
	if c, ok := CreateContextFrom(got); !ok || c.CreatorID != "u1" {
		t.Errorf("unrelated context values must survive: %+v %v", c, ok)
	}
	if ConfigResolutionFrom(WithoutResolution(context.Background())) {
		t.Error("an unmarked context must stay unmarked")
	}
}
