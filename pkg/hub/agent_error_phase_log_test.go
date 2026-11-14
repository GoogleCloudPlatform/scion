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
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// provisionFailureMessage is the phase error message sciontool init writes
// for a failed harness provisioner (cmd/sciontool/commands/init.go): the
// hook error with the provisioner's recorded, scrubbed error appended.
// It carries a credential-shaped KEY=VALUE pair that SanitizeText masks.
const provisionFailureMessage = "pre-start hook failed: hook errors: script hooks: " +
	"script /home/scion/.scion/hooks/pre-start.d/20-harness-provision: execution failed: exit status 1: " +
	"harness provisioner failed: exit status 1: auth: ANTHROPIC_API_KEY=sk-ant-not-a-real-key-0123456789 rejected"

func TestAgentErrorStage(t *testing.T) {
	cases := []struct {
		name, message, stage, hook, exit string
	}{
		{"harness provisioner", provisionFailureMessage, failureStageHarnessProvision, "20-harness-provision", "1"},
		{"other pre-start hook",
			"pre-start hook failed: hook errors: script hooks: script /etc/scion/hooks/pre-start.d/10-project: execution failed: exit status 3",
			failureStagePreStartHook, "10-project", "3"},
		{"pre-start handler, no script", "pre-start hook failed: hook errors: handler: boom", failureStagePreStartHook, "", ""},
		{"git clone", "git clone failed: exit status 128", failureStageGitClone, "", ""},
		{"crash", "Agent crashed with exit code 2", failureStageOther, "", ""},
		{"empty", "", failureStageOther, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stage, hook, exit := agentErrorStage(tc.message)
			assert.Equal(t, tc.stage, stage)
			assert.Equal(t, tc.hook, hook)
			assert.Equal(t, tc.exit, exit)
		})
	}
}

// errorPhaseLogRecords decodes the JSON lifecycle log in buf and returns
// its agent_entered_error_phase records.
func errorPhaseLogRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &rec), line)
		if rec["msg"] == logAgentEnteredErrorPhase {
			out = append(out, rec)
		}
	}
	return out
}

func TestLogAgentErrorPhaseEntry(t *testing.T) {
	var buf bytes.Buffer
	s := &Server{agentLifecycleLog: slog.New(slog.NewJSONHandler(&buf, nil))}
	exit := 1
	before := &store.Agent{ID: "a1", Phase: "starting"}
	after := &store.Agent{
		ID: "a1", Slug: "dev", ProjectID: "p1", RuntimeBrokerID: "b1", RunID: "r1",
		Phase: "error", Message: provisionFailureMessage, ExitCode: &exit, ContainerStatus: "exited (1)",
	}

	s.logAgentErrorPhaseEntry(before, after, errorPhaseSourceHeartbeat)
	recs := errorPhaseLogRecords(t, &buf)
	require.Len(t, recs, 1)
	rec := recs[0]
	assert.Equal(t, "WARN", rec["level"])
	assert.Equal(t, "a1", rec["agent_id"])
	assert.Equal(t, "dev", rec["agent"])
	assert.Equal(t, "p1", rec["project_id"])
	assert.Equal(t, "b1", rec["broker_id"])
	assert.Equal(t, "r1", rec["run_id"])
	assert.Equal(t, errorPhaseSourceHeartbeat, rec["source"])
	assert.Equal(t, "starting", rec["previous_phase"])
	assert.Equal(t, failureStageHarnessProvision, rec["failure_stage"])
	assert.Equal(t, "20-harness-provision", rec["hook"])
	assert.Equal(t, "1", rec["hook_exit_status"])
	assert.EqualValues(t, 1, rec["exit_code"])
	assert.Equal(t, "exited (1)", rec["container_status"])
	msg, _ := rec["message"].(string)
	assert.Contains(t, msg, "harness provisioner failed")
	assert.NotContains(t, msg, "sk-ant-not-a-real-key", "the message is logged sanitized")
	assert.NotContains(t, buf.String(), "sk-ant-not-a-real-key")

	// No transition, no record: an error phase confirmed again, a phase
	// other than error, or a missing read.
	buf.Reset()
	s.logAgentErrorPhaseEntry(after, after, errorPhaseSourceHeartbeat)
	s.logAgentErrorPhaseEntry(before, &store.Agent{ID: "a1", Phase: "running"}, errorPhaseSourceHeartbeat)
	s.logAgentErrorPhaseEntry(nil, after, errorPhaseSourceAgentStatus)
	assert.Empty(t, errorPhaseLogRecords(t, &buf))
}

func TestLogAgentErrorPhaseEntry_BoundsMessage(t *testing.T) {
	var buf bytes.Buffer
	s := &Server{agentLifecycleLog: slog.New(slog.NewJSONHandler(&buf, nil))}
	long := "pre-start hook failed: " + strings.Repeat("word ", 2000)
	s.logAgentErrorPhaseEntry(&store.Agent{Phase: "running"}, &store.Agent{ID: "a1", Phase: "error", Message: long}, errorPhaseSourceAgentStatus)
	recs := errorPhaseLogRecords(t, &buf)
	require.Len(t, recs, 1)
	msg, _ := recs[0]["message"].(string)
	assert.Less(t, len(msg), len(long))
	assert.LessOrEqual(t, len(msg), 2*1024, "SanitizeText bounds the logged message")
}
