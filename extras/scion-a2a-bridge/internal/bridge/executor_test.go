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

package bridge

import (
	"encoding/json"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/state"
)

func TestTaskEventToSDKEvent_Artifact(t *testing.T) {
	payload, err := json.Marshal(TaskArtifactUpdate{
		Artifact: Artifact{
			Parts: []Part{
				{Text: "Three kinase targets are EGFR, BRAF, and BCR-ABL."},
				{URL: "https://example.com/report.pdf"},
			},
		},
	})
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	ev := &state.TaskEvent{
		Kind:    "artifact",
		Payload: payload,
	}

	execCtx := &a2asrv.ExecutorContext{
		TaskID:    "task-123",
		ContextID: "ctx-456",
	}

	sdkEv, err := taskEventToSDKEvent(execCtx, ev)
	if err != nil {
		t.Fatalf("taskEventToSDKEvent failed: %v", err)
	}

	statusEv, ok := sdkEv.(*a2a.TaskStatusUpdateEvent)
	if !ok {
		t.Fatalf("expected *a2a.TaskStatusUpdateEvent, got %T", sdkEv)
	}
	if statusEv.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("expected state %v, got %v", a2a.TaskStateCompleted, statusEv.Status.State)
	}
	if statusEv.Status.Message == nil {
		t.Fatalf("expected non-nil status.message for artifact event")
	}
	if len(statusEv.Status.Message.Parts) != 2 {
		t.Fatalf("expected 2 parts (text + URL), got %d", len(statusEv.Status.Message.Parts))
	}
}
