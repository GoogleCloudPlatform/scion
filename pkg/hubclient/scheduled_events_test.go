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

package hubclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestScheduledEvent_JSON(t *testing.T) {
	t.Run("unmarshal legacy groveId field is not honored", func(t *testing.T) {
		jsonData := `{"id": "e1", "groveId": "p1"}`
		var e ScheduledEvent
		if err := json.Unmarshal([]byte(jsonData), &e); err != nil {
			t.Fatalf("Unmarshal failed: %v", err)
		}
		if e.ProjectID != "" {
			t.Errorf("ProjectID = %q, want empty (legacy groveId must not be honored)", e.ProjectID)
		}
	})

	t.Run("marshal emits only canonical fields", func(t *testing.T) {
		e := ScheduledEvent{ID: "e1", ProjectID: "p1"}
		data, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("Marshal failed: %v", err)
		}

		var m map[string]interface{}
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("Unmarshal back failed: %v", err)
		}

		if m["projectId"] != "p1" {
			t.Errorf("projectId = %v, want %q", m["projectId"], "p1")
		}
		if _, ok := m["groveId"]; ok {
			t.Errorf("legacy 'groveId' key present in marshal output, want absent: %v", m["groveId"])
		}
		if _, ok := m["execSummary"]; ok {
			t.Errorf("execSummary present when nil, want omitted: %v", m["execSummary"])
		}
	})

	t.Run("unmarshal execSummary preserves zero exitCode and nullable pointers", func(t *testing.T) {
		raw := `{
			"id": "e-exec-1",
			"projectId": "p1",
			"eventType": "exec",
			"fireAt": "2026-11-15T12:00:00Z",
			"payload": "{\"agentId\":\"a1\",\"agentName\":\"worker\",\"timeoutSeconds\":60,\"commandRedacted\":true,\"argc\":3,\"commandSha256\":\"deadbeef\"}",
			"status": "fired",
			"createdAt": "2026-11-15T11:55:00Z",
			"createdBy": "user-1",
			"execSummary": {
				"outcome": "ok",
				"exitCode": 0,
				"durationMs": 145,
				"outputBytes": 0,
				"startedAt": "2026-11-15T12:00:01Z",
				"finishedAt": "2026-11-15T12:00:02Z"
			}
		}`
		var e ScheduledEvent
		if err := json.Unmarshal([]byte(raw), &e); err != nil {
			t.Fatalf("Unmarshal failed: %v", err)
		}
		if e.ExecSummary == nil {
			t.Fatal("expected non-nil ExecSummary")
		}
		if e.ExecSummary.Outcome != "ok" {
			t.Errorf("Outcome = %q, want ok", e.ExecSummary.Outcome)
		}
		if e.ExecSummary.ExitCode == nil || *e.ExecSummary.ExitCode != 0 {
			t.Errorf("ExitCode = %v, want non-nil 0", e.ExecSummary.ExitCode)
		}
		if e.ExecSummary.DurationMS == nil || *e.ExecSummary.DurationMS != 145 {
			t.Errorf("DurationMS = %v, want 145", e.ExecSummary.DurationMS)
		}
		if e.ExecSummary.OutputBytes == nil || *e.ExecSummary.OutputBytes != 0 {
			t.Errorf("OutputBytes = %v, want non-nil 0", e.ExecSummary.OutputBytes)
		}
		if e.ExecSummary.FinishedAt == nil {
			t.Error("expected non-nil FinishedAt")
		}

		// Re-marshal and verify zero exitCode and outputBytes stay present.
		remarshaled, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("Marshal failed: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(remarshaled, &m); err != nil {
			t.Fatalf("Unmarshal map failed: %v", err)
		}
		sum, ok := m["execSummary"].(map[string]any)
		if !ok {
			t.Fatalf("execSummary missing or wrong type: %T", m["execSummary"])
		}
		if sum["exitCode"] != float64(0) {
			t.Errorf("exitCode = %v, want 0", sum["exitCode"])
		}
		if sum["outputBytes"] != float64(0) {
			t.Errorf("outputBytes = %v, want 0", sum["outputBytes"])
		}
	})
}

func TestScheduledEventService_ExecRoundTrip(t *testing.T) {
	exitCode := 2
	durationMS := int64(820)
	outputBytes := int64(42)
	startedAt := time.Date(2026, 11, 15, 12, 0, 1, 0, time.UTC)
	finishedAt := time.Date(2026, 11, 15, 12, 0, 2, 0, time.UTC)

	var gotCreateBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/proj-1/scheduled-events":
			if err := json.NewDecoder(r.Body).Decode(&gotCreateBody); err != nil {
				t.Fatalf("decode create body: %v", err)
			}
			_ = json.NewEncoder(w).Encode(ScheduledEvent{
				ID:        "evt-1",
				ProjectID: "proj-1",
				EventType: "exec",
				Status:    "pending",
				Payload:   `{"agentId":"a-1","agentName":"worker","command":["/bin/echo","hi"],"timeoutSeconds":120}`,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/scheduled-events/evt-1":
			_ = json.NewEncoder(w).Encode(ScheduledEvent{
				ID:        "evt-1",
				ProjectID: "proj-1",
				EventType: "exec",
				Status:    "failed",
				Error:     "exit status 2",
				Payload:   `{"agentId":"a-1","agentName":"worker","timeoutSeconds":120,"commandRedacted":true,"argc":2,"commandSha256":"abc123"}`,
				ExecSummary: &ExecSummary{
					Outcome:      "nonzero",
					ExitCode:     &exitCode,
					DurationMS:   &durationMS,
					OutputBytes:  &outputBytes,
					CaptureMode:  "json",
					OutputStatus: "captured",
					StartedAt:    startedAt,
					FinishedAt:   &finishedAt,
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/scheduled-events/evt-1/output":
			_ = json.NewEncoder(w).Encode(ExecOutput{
				CaptureMode:  "json",
				OutputStatus: "captured",
				Output:       json.RawMessage(`{"a":1}`),
			})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	c, err := New(server.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	svc := c.ScheduledEvents("proj-1")

	created, err := svc.Create(context.Background(), &CreateScheduledEventRequest{
		EventType:      "exec",
		FireIn:         "10m",
		AgentName:      "worker",
		Command:        []string{"/bin/echo", "hi"},
		TimeoutSeconds: 120,
	})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if created.ID != "evt-1" || created.EventType != "exec" {
		t.Errorf("unexpected created event: %+v", created)
	}
	wantCmd := []any{"/bin/echo", "hi"}
	if !reflect.DeepEqual(gotCreateBody["command"], wantCmd) {
		t.Errorf("request command = %v, want %v", gotCreateBody["command"], wantCmd)
	}
	if gotCreateBody["timeoutSeconds"] != float64(120) {
		t.Errorf("request timeoutSeconds = %v, want 120", gotCreateBody["timeoutSeconds"])
	}
	if _, ok := gotCreateBody["payload"]; ok {
		t.Errorf("expected payload omitted when empty, got %v", gotCreateBody["payload"])
	}

	// Verify raw Payload serialization on CreateScheduledEventRequest as well.
	rawReq := CreateScheduledEventRequest{
		EventType: "exec",
		FireIn:    "5m",
		Payload:   `{"agentId":"a-1","command":["true"]}`,
	}
	rawBytes, err := json.Marshal(rawReq)
	if err != nil {
		t.Fatalf("Marshal rawReq: %v", err)
	}
	var rawMap map[string]any
	if err := json.Unmarshal(rawBytes, &rawMap); err != nil {
		t.Fatalf("Unmarshal rawMap: %v", err)
	}
	if rawMap["payload"] != `{"agentId":"a-1","command":["true"]}` {
		t.Errorf("payload = %v", rawMap["payload"])
	}
	if _, ok := rawMap["command"]; ok {
		t.Errorf("expected command omitted when nil, got %v", rawMap["command"])
	}
	if _, ok := rawMap["timeoutSeconds"]; ok {
		t.Errorf("expected timeoutSeconds omitted when 0, got %v", rawMap["timeoutSeconds"])
	}

	got, err := svc.Get(context.Background(), "evt-1")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if got.ExecSummary == nil {
		t.Fatal("expected ExecSummary on Get")
	}
	if got.ExecSummary.Outcome != "nonzero" || got.ExecSummary.ExitCode == nil || *got.ExecSummary.ExitCode != 2 {
		t.Errorf("unexpected ExecSummary: %+v", got.ExecSummary)
	}
	if got.ExecSummary.DurationMS == nil || *got.ExecSummary.DurationMS != 820 {
		t.Errorf("unexpected DurationMS: %v", got.ExecSummary.DurationMS)
	}
	if got.ExecSummary.OutputBytes == nil || *got.ExecSummary.OutputBytes != 42 {
		t.Errorf("unexpected OutputBytes: %v", got.ExecSummary.OutputBytes)
	}
	if got.ExecSummary.CaptureMode != "json" || got.ExecSummary.OutputStatus != "captured" {
		t.Errorf("unexpected CaptureMode/OutputStatus: %+v", got.ExecSummary)
	}

	out, err := svc.GetOutput(context.Background(), "evt-1")
	if err != nil {
		t.Fatalf("GetOutput failed: %v", err)
	}
	if out.CaptureMode != "json" || out.OutputStatus != "captured" || string(out.Output) != `{"a":1}` {
		t.Errorf("unexpected ExecOutput: %+v", out)
	}
}
