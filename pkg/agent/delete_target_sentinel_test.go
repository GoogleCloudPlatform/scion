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
	"errors"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// ErrAgentFilesDelete marks only a file-cleanup failure after a successful
// runtime delete, never a failure of the runtime Delete call itself.
func TestDeleteTarget_ErrAgentFilesDeleteScope(t *testing.T) {
	projectDir := t.TempDir()

	deleted := false
	ok := &runtime.MockRuntime{DeleteFunc: func(context.Context, runtime.RunRef) error { deleted = true; return nil }}
	// "../escape" fails DeleteAgentFiles' containment check after the
	// runtime delete has succeeded.
	_, err := NewManager(ok).DeleteTarget(context.Background(), "../escape", runtime.RunRef{ID: "cid", RunID: "r1"}, true, projectDir, false)
	if !deleted {
		t.Fatal("premise: runtime delete not called")
	}
	if err == nil {
		t.Fatal("premise: expected a file-cleanup error")
	}
	// The file-cleanup failure is the positive signal that the runtime
	// entry is gone (ptone/scion#4766).
	if !errors.Is(err, ErrAgentFilesDelete) {
		t.Errorf("file-cleanup failure not wrapped as ErrAgentFilesDelete: %v", err)
	}

	cause := errors.New("apiserver unavailable")
	bad := &runtime.MockRuntime{DeleteFunc: func(context.Context, runtime.RunRef) error { return cause }}
	_, err = NewManager(bad).DeleteTarget(context.Background(), "dev", runtime.RunRef{ID: "cid", RunID: "r1"}, true, projectDir, false)
	if !errors.Is(err, cause) {
		t.Errorf("runtime failure does not wrap its cause: %v", err)
	}
	if errors.Is(err, ErrAgentFilesDelete) {
		t.Errorf("runtime failure wrapped as ErrAgentFilesDelete: %v", err)
	}
}
