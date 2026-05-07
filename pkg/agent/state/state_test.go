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

package state

import (
	"encoding/json"
	"testing"
)

func TestPhaseIsValid(t *testing.T) {
	tests := []struct {
		phase Phase
		want  bool
	}{
		// All 9 valid phases.
		{PhaseCreated, true},
		{PhaseProvisioning, true},
		{PhaseCloning, true},
		{PhaseStarting, true},
		{PhaseRunning, true},
		{PhaseSuspended, true},
		{PhaseStopping, true},
		{PhaseStopped, true},
		{PhaseError, true},
		// Legacy values that should NOT be valid.
		{"pending", false},
		{"deleted", false},
		{"restored", false},
		// Case sensitivity.
		{"RUNNING", false},
		{"Running", false},
		{"CREATED", false},
		// Empty and garbage.
		{"", false},
		{"bogus", false},
	}

	for _, tt := range tests {
		t.Run(string(tt.phase), func(t *testing.T) {
			if got := tt.phase.IsValid(); got != tt.want {
				t.Errorf("Phase(%q).IsValid() = %v, want %v", tt.phase, got, tt.want)
			}
		})
	}
}

func TestPhaseValidate(t *testing.T) {
	if err := PhaseRunning.Validate(); err != nil {
		t.Errorf("PhaseRunning.Validate() returned unexpected error: %v", err)
	}
	if err := Phase("bogus").Validate(); err == nil {
		t.Error("Phase(\"bogus\").Validate() returned nil, want error")
	}
}

func TestPhaseString(t *testing.T) {
	if s := PhaseRunning.String(); s != "running" {
		t.Errorf("PhaseRunning.String() = %q, want %q", s, "running")
	}
}

func TestActivityIsValid(t *testing.T) {
	tests := []struct {
		activity Activity
		want     bool
	}{
		// All 10 valid activities.
		{ActivityIdle, true},
		{ActivityThinking, true},
		{ActivityExecuting, true},
		{ActivityWaitingForInput, true},
		{ActivityBlocked, true},
		{ActivityCompleted, true},
		{ActivityLimitsExceeded, true},
		{ActivityStalled, true},
		{ActivityOffline, true},
		{ActivityCrashed, true},
		// Empty is valid (omitempty / non-running phase).
		{"", true},
		// Legacy values that should NOT be valid.
		{"busy", false},
		{"IDLE", false},
		{"THINKING", false},
		{"EXECUTING", false},
		{"WAITING_FOR_INPUT", false},
		// Garbage.
		{"bogus", false},
	}

	for _, tt := range tests {
		name := string(tt.activity)
		if name == "" {
			name = "(empty)"
		}
		t.Run(name, func(t *testing.T) {
			if got := tt.activity.IsValid(); got != tt.want {
				t.Errorf("Activity(%q).IsValid() = %v, want %v", tt.activity, got, tt.want)
			}
		})
	}
}

func TestActivityValidate(t *testing.T) {
	if err := ActivityIdle.Validate(); err != nil {
		t.Errorf("ActivityIdle.Validate() returned unexpected error: %v", err)
	}
	if err := Activity("").Validate(); err != nil {
		t.Errorf("Activity(\"\").Validate() returned unexpected error: %v", err)
	}
	if err := Activity("busy").Validate(); err == nil {
		t.Error("Activity(\"busy\").Validate() returned nil, want error")
	}
}

func TestActivityString(t *testing.T) {
	if s := ActivityWaitingForInput.String(); s != "waiting_for_input" {
		t.Errorf("ActivityWaitingForInput.String() = %q, want %q", s, "waiting_for_input")
	}
}

func TestActivityIsSticky(t *testing.T) {
	tests := []struct {
		activity Activity
		want     bool
	}{
		{ActivityWaitingForInput, true},
		{ActivityBlocked, true},
		{ActivityCompleted, true},
		{ActivityLimitsExceeded, true},
		{ActivityCrashed, true},
		{ActivityIdle, false},
		{ActivityThinking, false},
		{ActivityExecuting, false},
		{ActivityStalled, false},
		{ActivityOffline, false},
		{"", false},
	}

	for _, tt := range tests {
		name := string(tt.activity)
		if name == "" {
			name = "(empty)"
		}
		t.Run(name, func(t *testing.T) {
			if got := tt.activity.IsSticky(); got != tt.want {
				t.Errorf("Activity(%q).IsSticky() = %v, want %v", tt.activity, got, tt.want)
			}
		})
	}
}

func TestActivityIsPlatformSet(t *testing.T) {
	tests := []struct {
		activity Activity
		want     bool
	}{
		{ActivityStalled, true},
		{ActivityOffline, true},
		{ActivityIdle, false},
		{ActivityThinking, false},
		{ActivityExecuting, false},
		{ActivityWaitingForInput, false},
		{ActivityBlocked, false},
		{ActivityCompleted, false},
		{ActivityLimitsExceeded, false},
		{ActivityCrashed, false},
		{"", false},
	}

	for _, tt := range tests {
		name := string(tt.activity)
		if name == "" {
			name = "(empty)"
		}
		t.Run(name, func(t *testing.T) {
			if got := tt.activity.IsPlatformSet(); got != tt.want {
				t.Errorf("Activity(%q).IsPlatformSet() = %v, want %v", tt.activity, got, tt.want)
			}
		})
	}
}

func TestActivityIsTerminal(t *testing.T) {
	tests := []struct {
		activity Activity
		want     bool
	}{
		{ActivityCrashed, true},
		{ActivityLimitsExceeded, true},
		{ActivityIdle, false},
		{ActivityThinking, false},
		{ActivityExecuting, false},
		{ActivityWaitingForInput, false},
		{ActivityBlocked, false},
		{ActivityCompleted, false},
		{ActivityStalled, false},
		{ActivityOffline, false},
		{"", false},
	}

	for _, tt := range tests {
		name := string(tt.activity)
		if name == "" {
			name = "(empty)"
		}
		t.Run(name, func(t *testing.T) {
			if got := tt.activity.IsTerminal(); got != tt.want {
				t.Errorf("Activity(%q).IsTerminal() = %v, want %v", tt.activity, got, tt.want)
			}
		})
	}
}

func TestDisplayStatus(t *testing.T) {
	tests := []struct {
		name  string
		state AgentState
		want  string
	}{
		{
			name:  "running with activity returns activity",
			state: AgentState{Phase: PhaseRunning, Activity: ActivityThinking},
			want:  "thinking",
		},
		{
			name:  "running with executing returns executing",
			state: AgentState{Phase: PhaseRunning, Activity: ActivityExecuting},
			want:  "executing",
		},
		{
			name:  "running with waiting_for_input returns waiting_for_input",
			state: AgentState{Phase: PhaseRunning, Activity: ActivityWaitingForInput},
			want:  "waiting_for_input",
		},
		{
			name:  "non-running phase returns phase",
			state: AgentState{Phase: PhaseProvisioning},
			want:  "provisioning",
		},
		{
			name:  "suspended returns phase",
			state: AgentState{Phase: PhaseSuspended},
			want:  "suspended",
		},
		{
			name:  "stopped returns phase",
			state: AgentState{Phase: PhaseStopped},
			want:  "stopped",
		},
		{
			name:  "stopped with crashed returns crashed",
			state: AgentState{Phase: PhaseStopped, Activity: ActivityCrashed},
			want:  "crashed",
		},
		{
			name:  "stopped with limits_exceeded returns limits_exceeded",
			state: AgentState{Phase: PhaseStopped, Activity: ActivityLimitsExceeded},
			want:  "limits_exceeded",
		},
		{
			name:  "error returns phase",
			state: AgentState{Phase: PhaseError},
			want:  "error",
		},
		{
			name:  "running with empty activity returns running",
			state: AgentState{Phase: PhaseRunning},
			want:  "running",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.state.DisplayStatus(); got != tt.want {
				t.Errorf("DisplayStatus() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAgentStateValidate(t *testing.T) {
	tests := []struct {
		name    string
		state   AgentState
		wantErr bool
	}{
		{
			name:    "valid running with activity",
			state:   AgentState{Phase: PhaseRunning, Activity: ActivityIdle},
			wantErr: false,
		},
		{
			name:    "valid running with no activity",
			state:   AgentState{Phase: PhaseRunning},
			wantErr: false,
		},
		{
			name:    "valid non-running with no activity",
			state:   AgentState{Phase: PhaseStopped},
			wantErr: false,
		},
		{
			name:    "valid created with no activity",
			state:   AgentState{Phase: PhaseCreated},
			wantErr: false,
		},
		{
			name:    "valid suspended with no activity",
			state:   AgentState{Phase: PhaseSuspended},
			wantErr: false,
		},
		{
			name:    "invalid: activity with suspended phase",
			state:   AgentState{Phase: PhaseSuspended, Activity: ActivityIdle},
			wantErr: true,
		},
		{
			name:    "valid: terminal activity with stopped phase",
			state:   AgentState{Phase: PhaseStopped, Activity: ActivityCrashed},
			wantErr: false,
		},
		{
			name:    "valid: limits_exceeded with stopped phase",
			state:   AgentState{Phase: PhaseStopped, Activity: ActivityLimitsExceeded},
			wantErr: false,
		},
		{
			name:    "invalid: non-terminal activity with stopped phase",
			state:   AgentState{Phase: PhaseStopped, Activity: ActivityIdle},
			wantErr: true,
		},
		{
			name:    "invalid: activity with provisioning phase",
			state:   AgentState{Phase: PhaseProvisioning, Activity: ActivityThinking},
			wantErr: true,
		},
		{
			name:    "invalid: bad phase",
			state:   AgentState{Phase: "bogus"},
			wantErr: true,
		},
		{
			name:    "invalid: bad activity",
			state:   AgentState{Phase: PhaseRunning, Activity: "bogus"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.state.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestJSONRoundTrip(t *testing.T) {
	original := AgentState{
		Phase:    PhaseRunning,
		Activity: ActivityExecuting,
		Detail: Detail{
			ToolName:    "Bash",
			Message:     "Running tests",
			TaskSummary: "Implement auth module",
		},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded AgentState
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	if decoded.Phase != original.Phase {
		t.Errorf("Phase = %q, want %q", decoded.Phase, original.Phase)
	}
	if decoded.Activity != original.Activity {
		t.Errorf("Activity = %q, want %q", decoded.Activity, original.Activity)
	}
	if decoded.Detail.ToolName != original.Detail.ToolName {
		t.Errorf("Detail.ToolName = %q, want %q", decoded.Detail.ToolName, original.Detail.ToolName)
	}
	if decoded.Detail.Message != original.Detail.Message {
		t.Errorf("Detail.Message = %q, want %q", decoded.Detail.Message, original.Detail.Message)
	}
	if decoded.Detail.TaskSummary != original.Detail.TaskSummary {
		t.Errorf("Detail.TaskSummary = %q, want %q", decoded.Detail.TaskSummary, original.Detail.TaskSummary)
	}
}

func TestJSONOmitempty(t *testing.T) {
	t.Run("empty activity and detail omitted", func(t *testing.T) {
		s := AgentState{Phase: PhaseStopped}
		data, err := json.Marshal(s)
		if err != nil {
			t.Fatalf("Marshal failed: %v", err)
		}

		var raw map[string]interface{}
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatalf("Unmarshal failed: %v", err)
		}

		if _, ok := raw["activity"]; ok {
			t.Error("expected 'activity' to be omitted from JSON, but it was present")
		}
		if p, ok := raw["phase"]; !ok || p != "stopped" {
			t.Errorf("expected phase = 'stopped', got %v", p)
		}
	})

	t.Run("detail with values is present", func(t *testing.T) {
		s := AgentState{
			Phase:    PhaseRunning,
			Activity: ActivityExecuting,
			Detail:   Detail{ToolName: "Bash"},
		}
		data, err := json.Marshal(s)
		if err != nil {
			t.Fatalf("Marshal failed: %v", err)
		}

		var raw map[string]interface{}
		if err := json.Unmarshal(data, &raw); err != nil {
			t.Fatalf("Unmarshal failed: %v", err)
		}

		detail, ok := raw["detail"].(map[string]interface{})
		if !ok {
			t.Fatal("expected 'detail' to be present in JSON")
		}
		if detail["toolName"] != "Bash" {
			t.Errorf("expected detail.toolName = 'Bash', got %v", detail["toolName"])
		}
		// message and taskSummary should be omitted when empty.
		if _, ok := detail["message"]; ok {
			t.Error("expected 'message' to be omitted from detail, but it was present")
		}
		if _, ok := detail["taskSummary"]; ok {
			t.Error("expected 'taskSummary' to be omitted from detail, but it was present")
		}
	})
}

func TestPhasesEnumeration(t *testing.T) {
	phases := Phases()
	if len(phases) != 9 {
		t.Fatalf("Phases() returned %d items, want 9", len(phases))
	}

	// Verify all returned phases are valid.
	for _, p := range phases {
		if !p.IsValid() {
			t.Errorf("Phases() returned invalid phase: %q", p)
		}
	}

	// Defensive copy: mutating the returned slice must not affect future calls.
	phases[0] = "mutated"
	fresh := Phases()
	if fresh[0] == "mutated" {
		t.Error("Phases() did not return a defensive copy; mutation leaked")
	}
}

func TestActivitiesEnumeration(t *testing.T) {
	activities := Activities()
	if len(activities) != 10 {
		t.Fatalf("Activities() returned %d items, want 10", len(activities))
	}

	// Verify all returned activities are valid.
	for _, a := range activities {
		if !a.IsValid() {
			t.Errorf("Activities() returned invalid activity: %q", a)
		}
	}

	// Defensive copy: mutating the returned slice must not affect future calls.
	activities[0] = "mutated"
	fresh := Activities()
	if fresh[0] == "mutated" {
		t.Error("Activities() did not return a defensive copy; mutation leaked")
	}
}
