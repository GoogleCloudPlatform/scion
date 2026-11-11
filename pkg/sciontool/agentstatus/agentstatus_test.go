/*
Copyright 2025 The Scion Authors.
*/

package agentstatus

import (
	"testing"

	state "github.com/GoogleCloudPlatform/scion/pkg/agent/state"
)

func TestStatusDefinitionHubUpdate(t *testing.T) {
	tests := []struct {
		name            string
		statusType      string
		wantActivity    state.Activity
		wantStatus      string
		wantMessage     string
		wantTaskSummary string
	}{
		{
			name:         "ask user",
			statusType:   "ask_user",
			wantActivity: state.ActivityWaitingForInput,
			wantStatus:   "waiting_for_input",
			wantMessage:  "details",
		},
		{
			name:         "blocked",
			statusType:   "blocked",
			wantActivity: state.ActivityBlocked,
			wantStatus:   "blocked",
			wantMessage:  "details",
		},
		{
			name:            "task completed",
			statusType:      "task_completed",
			wantActivity:    state.ActivityCompleted,
			wantStatus:      "completed",
			wantTaskSummary: "details",
		},
		{
			name:         "limits exceeded",
			statusType:   "limits_exceeded",
			wantActivity: state.ActivityLimitsExceeded,
			wantStatus:   "limits_exceeded",
			wantMessage:  "details",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			definition, ok := Lookup(tt.statusType)
			if !ok {
				t.Fatalf("status definition %q not found", tt.statusType)
			}

			update := definition.HubUpdate("details")
			if update.Activity != tt.wantActivity {
				t.Errorf("activity = %q, want %q", update.Activity, tt.wantActivity)
			}
			if update.Status != tt.wantStatus {
				t.Errorf("status = %q, want %q", update.Status, tt.wantStatus)
			}
			if update.Message != tt.wantMessage {
				t.Errorf("message = %q, want %q", update.Message, tt.wantMessage)
			}
			if update.TaskSummary != tt.wantTaskSummary {
				t.Errorf("task summary = %q, want %q", update.TaskSummary, tt.wantTaskSummary)
			}
		})
	}
}
