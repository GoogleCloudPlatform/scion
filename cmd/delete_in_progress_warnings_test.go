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

package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ptone/scion#3488: when a create or start loses to a delete, the hub answers
// 409 delete_in_progress and reports in details.warnings whether it removed
// a container the broker had already started. The CLI must print those
// warnings, keep the error line and exit non-zero; without warnings its
// output is unchanged.

const dipMessage = "agent was deleted while it was being created"

var dipWarnings = []interface{}{
	"failed to remove container for agent dip-agent: broker unreachable",
	"agent dip-agent may still be running on broker b1",
}

const dipWarningLines = "Warning: failed to remove container for agent dip-agent: broker unreachable\n" +
	"Warning: agent dip-agent may still be running on broker b1\n"

// dipRunner runs one hub-mode command (create or start) against stub.
type dipRunner func(t *testing.T, stub *hubStartStub, projectID, agentName string) error

func dipRunCreate(t *testing.T, stub *hubStartStub, projectID, agentName string) error {
	return createAgentViaHub(stub.hubCtx(t, projectID), agentName, "do it")
}

func dipRunStart(t *testing.T, stub *hubStartStub, projectID, agentName string) error {
	return startAgentViaHub(nil, stub.hubCtx(t, projectID), agentName, "do it", false, nil)
}

// dipRunResume covers the existing-agent POST /agents path that `scion
// start` on a stopped agent and `scion resume` use. Its stub body mirrors
// the existing-agent 409 with warnings that ptone/scion#3255 adds; until that
// lands upstream, this case is defensive.
func dipRunResume(t *testing.T, stub *hubStartStub, projectID, agentName string) error {
	return startAgentViaHub(nil, stub.hubCtx(t, projectID), agentName, "", true, nil)
}

func TestHubCommands_DeleteInProgressWarnings(t *testing.T) {
	for _, tc := range []struct {
		name          string
		existingPhase string
		run           dipRunner
		errPrefix     string
	}{
		{"create", "", dipRunCreate, "failed to create agent via Hub: "},
		{"start", "", dipRunStart, "failed to start agent via Hub: "},
		{"resume", "stopped", dipRunResume, "failed to start agent via Hub: "},
	} {
		for _, format := range []string{"", "json"} {
			t.Run(tc.name+"/format="+format, func(t *testing.T) {
				resetHubStartGlobals(t)
				origSA := serviceAccountFlag
				t.Cleanup(func() { serviceAccountFlag = origSA })
				serviceAccountFlag = ""

				const projectID, agentName = "proj-dip", "dip-agent"
				stub := newHubStartStub(t, projectID, agentName, tc.existingPhase)
				stub.createStatus = http.StatusConflict
				stub.createErrCode = apiclient.ErrCodeDeleteInProgress
				stub.createErrMsg = dipMessage

				run := func(details map[string]interface{}) (stdout, stderr string, err error) {
					outputFormat = format
					stub.createErrDetails = details
					stdout, stderr = captureStdIO(t, func() {
						err = tc.run(t, stub, projectID, agentName)
					})
					return stdout, stderr, err
				}

				// Baseline: the same 409 with no warnings, with
				// details absent and then with details present.
				baseOut, baseErrOut, baseErr := run(nil)
				noWarnOut, noWarnErrOut, noWarnErr := run(map[string]interface{}{"agentId": "agent-id"})
				warnOut, warnErrOut, warnErr := run(map[string]interface{}{
					"agentId": "agent-id", "warnings": dipWarnings,
				})

				wantErr := fmt.Sprintf("%sdelete_in_progress: %s (status: 409)", tc.errPrefix, dipMessage)
				for _, err := range []error{baseErr, noWarnErr, warnErr} {
					require.Error(t, err)
					assert.Equal(t, wantErr, err.Error(), "the error line is unchanged")
					assert.Equal(t, 1, exitCodeFor(err), "the command still exits non-zero")
					assert.True(t, isHubFailure(err))
					var apiErr *apiclient.APIError
					require.True(t, errors.As(err, &apiErr))
					assert.Equal(t, apiclient.ErrCodeDeleteInProgress, apiErr.Code)
				}

				// Without warnings, output is identical to the no-details case.
				assert.NotContains(t, baseErrOut, "Warning:")
				assert.Equal(t, baseOut, noWarnOut)
				assert.Equal(t, baseErrOut, noWarnErrOut)

				// With warnings, the only change is the warning lines on stderr.
				assert.Equal(t, baseOut, warnOut, "stdout is unchanged")
				assert.Equal(t, baseErrOut+dipWarningLines, warnErrOut,
					"each warning is printed as a Warning: line on stderr, and nothing else changes")

				if format == "json" {
					assert.Empty(t, warnOut, "a failed command writes no JSON to stdout; warnings must not land there")
				}
				assert.Equal(t, 3, stub.createCalls)
			})
		}
	}
}

func TestPrintDeleteInProgressWarnings(t *testing.T) {
	dip := func(details map[string]interface{}) error {
		return fmt.Errorf("wrapped: %w", &apiclient.APIError{
			StatusCode: http.StatusConflict, Code: apiclient.ErrCodeDeleteInProgress,
			Message: dipMessage, Details: details,
		})
	}
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"nil error", nil, ""},
		{"non-API error", errors.New("dial tcp: connection refused"), ""},
		{"other code with warnings", &apiclient.APIError{
			StatusCode: http.StatusConflict, Code: apiclient.ErrCodeConflict,
			Details: map[string]interface{}{"warnings": []interface{}{"x"}},
		}, ""},
		{"nil details", dip(nil), ""},
		{"no warnings", dip(map[string]interface{}{"agentId": "a"}), ""},
		{"warnings not a list", dip(map[string]interface{}{"warnings": "oops"}), ""},
		{"empty list", dip(map[string]interface{}{"warnings": []interface{}{}}), ""},
		{"non-string entries skipped", dip(map[string]interface{}{
			"warnings": []interface{}{"first", 42, nil, map[string]interface{}{"k": "v"}, "second"},
		}), "Warning: first\nWarning: second\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			printDeleteInProgressWarnings(&buf, tc.err)
			assert.Equal(t, tc.want, buf.String())
		})
	}
}
