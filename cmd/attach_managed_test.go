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
	"errors"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/wsclient"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// managedRuntimePreflightBody is the Hub's 503 body for an agent on a
// managed runtime with no session that serves a PTY (pkg/hub
// managedPTYPath).
const managedRuntimePreflightBody = `{"error":{"code":"runtime_attach_unsupported","message":"Attach is not supported for agents on a managed runtime","details":{"reason":"managed_runtime","path":"none"}}}`

// mockManagedRuntime is the runtime the mock Hub reports for a managed agent.
const mockManagedRuntime = "managed:test"

// assertManagedRuntimeRefusal checks the CLI's message for the Hub's
// managed_runtime preflight refusal: the CLI reached the preflight (the
// detail comes from the Hub's answer) and kept the message/look hint.
func assertManagedRuntimeRefusal(t *testing.T, err error, agentName string) {
	t.Helper()
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "cannot attach to agent '"+agentName+"'")
	assert.Contains(t, msg, "attach is not supported for agents on a managed runtime")
	assert.Contains(t, msg, "status 503, runtime_attach_unsupported, reason managed_runtime")
	assert.Contains(t, msg, "Use scion message and scion look instead")
	assert.Contains(t, msg, "scion message "+agentName)
	assert.Contains(t, msg, "scion look "+agentName)
	assert.NotContains(t, msg, "try again", "the refusal is not presented as retryable")
	assert.NotContains(t, msg, "scion list", "the generic no-path hint is not used")
	var pe *wsclient.PTYPreflightError
	require.True(t, errors.As(err, &pe), "the preflight error stays reachable")
	assert.Equal(t, wsprotocol.PTYReasonManagedRuntime, pe.Reason)
}

// TestAttachViaHub_ManagedAgent_HubPreflightDecides: scion attach on a
// managed agent no longer refuses on the client; it asks the Hub preflight
// and shows the Hub's managed_runtime refusal with the message/look hint.
func TestAttachViaHub_ManagedAgent_HubPreflightDecides(t *testing.T) {
	clearAppTokenSources(t)
	t.Setenv("SCION_HUB_TOKEN", "test-token")
	stubPlainTransport(t)

	const (
		projectID = "proj-managed-attach"
		agentName = "managed-agent"
		agentID   = "agent-uuid-managed"
	)
	srv := newAttachMockHubServer(t, projectID, agentName, agentID, mockManagedRuntime)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	err = attachViaHub(&HubContext{Client: client, Endpoint: srv.URL, ProjectID: projectID}, agentName)
	assertManagedRuntimeRefusal(t, err, agentName)
}

// TestStartAgentViaHub_ManagedAgent_HubPreflightDecides: scion start -a and
// scion resume -a on a managed agent reach the Hub preflight and show its
// managed_runtime refusal with the message/look hint.
func TestStartAgentViaHub_ManagedAgent_HubPreflightDecides(t *testing.T) {
	for _, tc := range []struct {
		name   string
		resume bool
	}{
		{"start -a", false},
		{"resume -a", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearAppTokenSources(t)
			t.Setenv("SCION_HUB_TOKEN", "test-token")
			stubPlainTransport(t)

			restore := saveAttachTestState()
			defer restore()
			attach = true
			templateName = ""
			labelFlags = nil
			runtimeBrokerID = ""
			harnessConfigFlag = ""
			harnessAuthFlag = ""

			const (
				projectID = "proj-start-managed"
				agentName = "start-managed-agent"
				agentID   = "start-managed-uuid"
			)
			srv := newStartAgentMockHubServer(t, projectID, agentName, agentID, mockManagedRuntime)
			client, err := hubclient.New(srv.URL)
			require.NoError(t, err)

			hubCtx := &HubContext{Client: client, Endpoint: srv.URL, ProjectID: projectID}
			err = startAgentViaHub(nil, hubCtx, agentName, "", tc.resume, nil)
			assertManagedRuntimeRefusal(t, err, agentName)
		})
	}
}
