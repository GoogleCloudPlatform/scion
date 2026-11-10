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

package agent

import (
	"context"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Delete's graceful pre-stop uses the engines' 10s default rather than
// the runtime's longer default stop window, so deleting a running agent does
// not wait out its full shutdown (ptone/scion#3608).
func TestDeleteTarget_PreStopUsesShortTimeout(t *testing.T) {
	var stopRef, deleteRef runtime.RunRef
	rt := &runtime.MockRuntime{
		StopFunc:   func(_ context.Context, ref runtime.RunRef) error { stopRef = ref; return nil },
		DeleteFunc: func(_ context.Context, ref runtime.RunRef) error { deleteRef = ref; return nil },
	}
	ref := runtime.RunRef{ID: "cid", RunID: "r1"}
	if _, err := NewManager(rt).DeleteTarget(context.Background(), "dev", "", ref, false, "", false); err != nil {
		t.Fatalf("DeleteTarget: %v", err)
	}

	want := runtime.RunRef{ID: "cid", RunID: "r1", StopTimeout: 10 * time.Second}
	if stopRef != want {
		t.Errorf("Stop ref = %+v, want %+v", stopRef, want)
	}
	if deleteRef != ref {
		t.Errorf("Delete ref = %+v, want %+v", deleteRef, ref)
	}
}

// The delete pre-stop stays below the hub's deferred-delete wait
// (dispatchDeleteTimeout in pkg/hub, 15s), with room for the force-remove
// after it. pkg/agent cannot import pkg/hub, so the 15s is restated here;
// change both together.
func TestDeletePreStopTimeout_BelowHubDeleteWait(t *testing.T) {
	const hubDispatchDeleteTimeout = 15 * time.Second
	if deletePreStopTimeout >= hubDispatchDeleteTimeout {
		t.Errorf("deletePreStopTimeout = %s, want below the hub's %s delete wait", deletePreStopTimeout, hubDispatchDeleteTimeout)
	}
}
