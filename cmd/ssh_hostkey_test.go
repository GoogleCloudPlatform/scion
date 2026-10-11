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
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/tunnelclient"
)

// Host-key rotation, tunnel side: after the agent is recreated, the check
// prints the recreated hint and lets ssh report the mismatch.
func TestCheckSSHHostKeyPin_MismatchPrintsRecreatedHint(t *testing.T) {
	home := t.TempDir()
	hub := twoProjectHub(t, "tok")
	env, _, errb := newSSHTestEnv(t, home, hub)
	require.Equal(t, tunnelclient.ExitOK, runSSHConfig(context.Background(), env, nil, true), errb.String())

	var stderr bytes.Buffer
	require.NoError(t, checkSSHHostKeyPin(context.Background(), &stderr, home, hub.srv.URL, "tok", "a-b", "c"))
	assert.Empty(t, stderr.String(), "matching key: no output")

	hub.rotateKey(t, "ag-c")
	require.NoError(t, checkSSHHostKeyPin(context.Background(), &stderr, home, hub.srv.URL, "tok", "a-b", "c"))
	assert.Equal(t, sshRecreatedHint+"\n", stderr.String())
}

// No pin (ssh-config never written for this hub) is not a mismatch.
func TestCheckSSHHostKeyPin_NoPinNoOutput(t *testing.T) {
	hub := twoProjectHub(t, "tok")
	stubSSHTransport(t)
	var stderr bytes.Buffer
	require.NoError(t, checkSSHHostKeyPin(context.Background(), &stderr, t.TempDir(), hub.srv.URL, "tok", "a-b", "c"))
	assert.Empty(t, stderr.String())
}

// The ProxyCommand path for a deleted agent exits 4
// promptly (well within 5s) with one line.
func TestCheckSSHHostKeyPin_DeletedAgentExitsAgentNotFoundPromptly(t *testing.T) {
	home := t.TempDir()
	hub := twoProjectHub(t, "tok")
	env, _, errb := newSSHTestEnv(t, home, hub)
	require.Equal(t, tunnelclient.ExitOK, runSSHConfig(context.Background(), env, nil, true), errb.String())
	hub.deleteAgent("ag-c")

	var stderr bytes.Buffer
	start := time.Now()
	err := checkSSHHostKeyPin(context.Background(), &stderr, home, hub.srv.URL, "tok", "a-b", "c")
	assert.Less(t, time.Since(start), 5*time.Second)
	require.Error(t, err)
	assert.Equal(t, tunnelclient.ExitAgentNotFound, tunnelclient.CodeOf(err))
	assert.NotContains(t, err.Error(), "\n")
}

// Credential rows on the host-key accessor: a refused token is 3.
func TestAgentSSHHostKey_RefusedCredential(t *testing.T) {
	hub := twoProjectHub(t, "tok")
	stubSSHTransport(t)
	conn, err := newSSHHubConn(hub.srv.URL, "wrong")
	require.NoError(t, err)
	_, err = conn.agentSSHHostKey(context.Background(), "", "ag-c")
	assert.Equal(t, tunnelclient.ExitCredential, tunnelclient.CodeOf(err))
}

// The accessor sends the user bearer token and reads sshHostKey.
func TestAgentSSHHostKey_ReadsField(t *testing.T) {
	hub := twoProjectHub(t, "tok")
	stubSSHTransport(t)
	conn, err := newSSHHubConn(hub.srv.URL, "tok")
	require.NoError(t, err)
	key, err := conn.agentSSHHostKey(context.Background(), "", "ag-c")
	require.NoError(t, err)
	assert.Equal(t, hub.agents[0].HostKey, key)
	key, err = conn.agentSSHHostKey(context.Background(), "p-ab", "c")
	require.NoError(t, err)
	assert.Equal(t, hub.agents[0].HostKey, key)
}

func TestClassifySSHHubError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"401", &apiclient.APIError{StatusCode: http.StatusUnauthorized}, tunnelclient.ExitCredential},
		{"403", &apiclient.APIError{StatusCode: http.StatusForbidden}, tunnelclient.ExitForbidden},
		{"502", &apiclient.APIError{StatusCode: http.StatusBadGateway}, tunnelclient.ExitHubUnavailable},
		{"503", &apiclient.APIError{StatusCode: http.StatusServiceUnavailable}, tunnelclient.ExitHubUnavailable},
		{"504", &apiclient.APIError{StatusCode: http.StatusGatewayTimeout}, tunnelclient.ExitHubUnavailable},
		{"500", &apiclient.APIError{StatusCode: http.StatusInternalServerError}, tunnelclient.ExitError},
		{"dial", fmt.Errorf("request failed: %w", &url.Error{Op: "Get", URL: "x", Err: errors.New("refused")}), tunnelclient.ExitHubUnavailable},
		{"deadline", context.DeadlineExceeded, tunnelclient.ExitHubUnavailable},
		{"marked", tunnelclient.Errorf(tunnelclient.ExitAgentNotFound, "gone"), tunnelclient.ExitAgentNotFound},
		{"other", errors.New("boom"), tunnelclient.ExitError},
	}
	for _, c := range cases {
		got := classifySSHHubError("https://hub.example", c.err)
		assert.Equal(t, c.want, got.Code, c.name)
		assert.NotContains(t, got.Msg, "\n", c.name)
	}
}
