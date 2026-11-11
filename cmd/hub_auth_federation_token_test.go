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
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The token value below is a synthetic placeholder, not a credential.
const fakeCLIFederationToken = "synthetic.cli.federation.fixture"

func TestHubAuthFederationToken_PrintsBareToken(t *testing.T) {
	m := &mockHubAuthService{federationTokenResp: &hubclient.FederationTokenResponse{
		Token: fakeCLIFederationToken, ExpiresAt: time.Date(2026, 10, 12, 17, 0, 0, 0, time.UTC),
	}}
	var out bytes.Buffer
	require.NoError(t, runHubAuthFederationToken(context.Background(), m, "  https://peer.example.com  ", false, &out))
	assert.Equal(t, fakeCLIFederationToken+"\n", out.String())
	require.Len(t, m.federationTokenReqs, 1)
	assert.Equal(t, "https://peer.example.com", m.federationTokenReqs[0].Audience)
}

func TestHubAuthFederationToken_JSONOutput(t *testing.T) {
	expires := time.Date(2026, 10, 12, 17, 0, 0, 0, time.UTC)
	m := &mockHubAuthService{federationTokenResp: &hubclient.FederationTokenResponse{Token: fakeCLIFederationToken, ExpiresAt: expires}}
	var out bytes.Buffer
	require.NoError(t, runHubAuthFederationToken(context.Background(), m, "https://peer.example.com", true, &out))
	var got hubclient.FederationTokenResponse
	require.NoError(t, json.Unmarshal(out.Bytes(), &got))
	assert.Equal(t, fakeCLIFederationToken, got.Token)
	assert.True(t, expires.Equal(got.ExpiresAt))
}

func TestHubAuthFederationToken_EmptyAudienceIsUsageError(t *testing.T) {
	m := &mockHubAuthService{}
	var out bytes.Buffer
	err := runHubAuthFederationToken(context.Background(), m, "   ", false, &out)
	require.Error(t, err)
	assert.True(t, isUsageError(err))
	assert.Empty(t, m.federationTokenReqs, "no request is sent without an audience")
	assert.Empty(t, out.String())
}

func TestHubAuthFederationToken_HubErrorReturned(t *testing.T) {
	m := &mockHubAuthService{federationTokenErr: errors.New("audience is not allowed")}
	var out bytes.Buffer
	err := runHubAuthFederationToken(context.Background(), m, "https://peer.example.com", false, &out)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "audience is not allowed")
	assert.Empty(t, out.String(), "nothing is printed on failure")
}

// TestHubAuthFederationTokenCommand_WiringAndMode pins the command's place
// in the real tree, its required --audience flag, and that it is human-mode
// only: hub.auth.federation-token is not in agentAllowed and agent mode
// removes it.
func TestHubAuthFederationTokenCommand_WiringAndMode(t *testing.T) {
	c := resolveCommandPath(rootCmd, "hub.auth.federation-token")
	require.NotNil(t, c, "hub.auth.federation-token must exist in the real command tree in human mode")
	flag := c.Flags().Lookup("audience")
	require.NotNil(t, flag)
	assert.Equal(t, []string{"true"}, flag.Annotations[cobra.BashCompOneRequiredFlag], "--audience is required")
	require.NotNil(t, c.Flags().Lookup("json"))

	assert.False(t, agentAllowed["hub.auth.federation-token"], "agentAllowed must NOT contain hub.auth.federation-token")
	assert.False(t, agentAllowed["hub.auth"], "agentAllowed must NOT contain hub.auth")

	root := &cobra.Command{Use: "scion"}
	real := resolveCommandPath(rootCmd, "hub")
	require.NotNil(t, real)
	root.AddCommand(cloneCommandShape(real))
	t.Setenv("SCION_CLI_MODE", "agent")
	applyModeRestrictions(root, resolveMode())
	assert.Nil(t, resolveCommandPath(root, "hub.auth.federation-token"))
}
