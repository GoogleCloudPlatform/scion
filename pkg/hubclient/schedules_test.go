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

func TestSchedule_JSON(t *testing.T) {
	t.Run("unmarshal legacy groveId field is not honored", func(t *testing.T) {
		jsonData := `{"id": "sc1", "groveId": "p1"}`
		var s Schedule
		if err := json.Unmarshal([]byte(jsonData), &s); err != nil {
			t.Fatalf("Unmarshal failed: %v", err)
		}
		if s.ProjectID != "" {
			t.Errorf("ProjectID = %q, want empty (legacy groveId must not be honored)", s.ProjectID)
		}
	})

	t.Run("marshal emits only canonical fields", func(t *testing.T) {
		s := Schedule{ID: "sc1", ProjectID: "p1"}
		data, err := json.Marshal(s)
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
	})
}

func TestScheduleService_ExecRoundTrip(t *testing.T) {
	exitZero := 0
	dur := int64(310)
	outBytes := int64(128)
	startedAt := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	finishedAt := time.Date(2026, 11, 15, 9, 0, 1, 0, time.UTC)

	var gotCreateBody map[string]any
	var gotUpdateBody map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/proj-1/schedules":
			if err := json.NewDecoder(r.Body).Decode(&gotCreateBody); err != nil {
				t.Fatalf("decode create body: %v", err)
			}
			_ = json.NewEncoder(w).Encode(Schedule{
				ID:        "sch-1",
				ProjectID: "proj-1",
				Name:      "nightly-gc",
				CronExpr:  "0 3 * * *",
				EventType: "exec",
				Status:    "active",
				Payload:   `{"agentId":"a-1","agentName":"worker","command":["/usr/local/bin/gc","--dry-run"],"timeoutSeconds":300}`,
			})
		case r.Method == http.MethodPatch && r.URL.Path == "/api/v1/projects/proj-1/schedules/sch-1":
			if err := json.NewDecoder(r.Body).Decode(&gotUpdateBody); err != nil {
				t.Fatalf("decode update body: %v", err)
			}
			_ = json.NewEncoder(w).Encode(Schedule{
				ID:        "sch-1",
				ProjectID: "proj-1",
				Name:      "nightly-gc",
				CronExpr:  "0 3 * * *",
				EventType: "exec",
				Status:    "active",
				Payload:   `{"agentId":"a-1","agentName":"worker","command":["/usr/local/bin/gc"],"timeoutSeconds":600}`,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/schedules/sch-1/history":
			_ = json.NewEncoder(w).Encode(ListScheduledEventsResponse{
				Events: []ScheduledEvent{
					{
						ID:         "evt-h1",
						ProjectID:  "proj-1",
						ScheduleID: "sch-1",
						EventType:  "exec",
						Status:     "fired",
						ExecSummary: &ExecSummary{
							Outcome:     "ok",
							ExitCode:    &exitZero,
							DurationMS:  &dur,
							OutputBytes: &outBytes,
							StartedAt:   startedAt,
							FinishedAt:  &finishedAt,
						},
					},
				},
				TotalCount: 1,
				ServerTime: finishedAt,
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
	svc := c.Schedules("proj-1")

	created, err := svc.Create(context.Background(), &CreateScheduleRequest{
		Name:           "nightly-gc",
		CronExpr:       "0 3 * * *",
		EventType:      "exec",
		AgentName:      "worker",
		Command:        []string{"/usr/local/bin/gc", "--dry-run"},
		TimeoutSeconds: 300,
	})
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if created.ID != "sch-1" || created.EventType != "exec" {
		t.Errorf("unexpected created schedule: %+v", created)
	}
	wantCreateCmd := []any{"/usr/local/bin/gc", "--dry-run"}
	if !reflect.DeepEqual(gotCreateBody["command"], wantCreateCmd) {
		t.Errorf("create command = %v, want %v", gotCreateBody["command"], wantCreateCmd)
	}
	if gotCreateBody["timeoutSeconds"] != float64(300) {
		t.Errorf("create timeoutSeconds = %v, want 300", gotCreateBody["timeoutSeconds"])
	}

	updated, err := svc.Update(context.Background(), "sch-1", &UpdateScheduleRequest{
		Command:        []string{"/usr/local/bin/gc"},
		TimeoutSeconds: 600,
	})
	if err != nil {
		t.Fatalf("Update failed: %v", err)
	}
	if updated.ID != "sch-1" {
		t.Errorf("unexpected updated schedule: %+v", updated)
	}
	wantUpdateCmd := []any{"/usr/local/bin/gc"}
	if !reflect.DeepEqual(gotUpdateBody["command"], wantUpdateCmd) {
		t.Errorf("update command = %v, want %v", gotUpdateBody["command"], wantUpdateCmd)
	}
	if gotUpdateBody["timeoutSeconds"] != float64(600) {
		t.Errorf("update timeoutSeconds = %v, want 600", gotUpdateBody["timeoutSeconds"])
	}

	hist, err := svc.History(context.Background(), "sch-1", nil)
	if err != nil {
		t.Fatalf("History failed: %v", err)
	}
	if len(hist.Events) != 1 || hist.Events[0].ExecSummary == nil {
		t.Fatalf("expected 1 history event with ExecSummary, got %+v", hist.Events)
	}
	if hist.Events[0].ExecSummary.Outcome != "ok" || *hist.Events[0].ExecSummary.ExitCode != 0 {
		t.Errorf("unexpected history ExecSummary: %+v", hist.Events[0].ExecSummary)
	}
}
