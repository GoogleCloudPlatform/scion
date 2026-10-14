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
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	sciontoolhub "github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	secretGetTestAgentID = "agent-secret-get-id"
	secretGetTestToken   = "fake-agent-token"
	// secretGetTestValue is a fake value with no trailing newline, an inner
	// newline and a non-ASCII byte, so any decoration or re-encoding of the
	// output shows up as a mismatch.
	secretGetTestValue = "fake-value\nline-2\xff"
)

// secretGetTestHub is a fake Hub agent-secret endpoint for one agent.
type secretGetTestHub struct {
	status   int // status for GET; 0 means 200
	requests int
}

func (h *secretGetTestHub) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.requests++
		// The value is read exactly as sciontool read it: a GET on the
		// agent-scoped endpoint, authenticated as the calling agent.
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/api/v1/agents/"+secretGetTestAgentID+"/secrets/MY_KEY", r.URL.Path)
		assert.Equal(t, secretGetTestToken, r.Header.Get("X-Scion-Agent-Token"),
			"secret get must authenticate as the agent")
		if h.status != 0 && h.status != http.StatusOK {
			w.WriteHeader(h.status)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"secret not found"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"key":    "MY_KEY",
			"value":  base64.StdEncoding.EncodeToString([]byte(secretGetTestValue)),
			"type":   "environment",
			"target": "MY_KEY",
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// useSecretGetHub points the value client factory at srv and resets the
// get flags and output format for the test.
func useSecretGetHub(t *testing.T, srv *httptest.Server) {
	t.Helper()
	t.Cleanup(sciontoolhub.SetHubTestSandboxed())
	orig := newSecretValueHubClient
	newSecretValueHubClient = func() *sciontoolhub.Client {
		return sciontoolhub.NewClientWithConfig(srv.URL, secretGetTestToken, secretGetTestAgentID)
	}
	t.Cleanup(func() { newSecretValueHubClient = orig })
	resetSecretGetFlags(t)
}

func resetSecretGetFlags(t *testing.T) {
	t.Helper()
	oldMeta, oldValue, oldFormat := agentSecretGetMetadata, agentSecretGetValue, outputFormat
	agentSecretGetMetadata, agentSecretGetValue, outputFormat = false, false, ""
	t.Cleanup(func() {
		agentSecretGetMetadata, agentSecretGetValue, outputFormat = oldMeta, oldValue, oldFormat
	})
}

func runSecretGetTest(t *testing.T, args ...string) (string, error) {
	t.Helper()
	c := &cobra.Command{}
	c.SetContext(context.Background())
	var out bytes.Buffer
	c.SetOut(&out)
	err := runAgentSecretGet(c, args)
	return out.String(), err
}

// TestSecretGet_DefaultPrintsValue checks that plain `scion secret get KEY`
// writes exactly the decoded value, with nothing added (ptone/scion#5013).
func TestSecretGet_DefaultPrintsValue(t *testing.T) {
	h := &secretGetTestHub{}
	useSecretGetHub(t, h.start(t))

	out, err := runSecretGetTest(t, "MY_KEY")
	require.NoError(t, err)
	assert.Equal(t, secretGetTestValue, out)
	assert.Equal(t, 1, h.requests)
}

// TestSecretGet_ValueFlagPrintsValue checks that --value prints exactly the
// value whatever the default is.
func TestSecretGet_ValueFlagPrintsValue(t *testing.T) {
	h := &secretGetTestHub{}
	useSecretGetHub(t, h.start(t))
	agentSecretGetValue = true

	out, err := runSecretGetTest(t, "MY_KEY")
	require.NoError(t, err)
	assert.Equal(t, secretGetTestValue, out)
}

// TestSecretGet_MetadataFlagSkipsValue checks that --metadata never reads
// the value, whatever the default is.
func TestSecretGet_MetadataFlagSkipsValue(t *testing.T) {
	resetSecretGetFlags(t)
	agentSecretGetMetadata = true
	wantValue, err := secretGetWantsValue(agentSecretGetMetadata, agentSecretGetValue, isJSONOutput())
	require.NoError(t, err)
	assert.False(t, wantValue)
}

// TestSecretGetWantsValue covers how the flags, --json and the default
// select the value or the metadata view. Only the default row depends on
// secretGetPrintsValueByDefault.
func TestSecretGetWantsValue(t *testing.T) {
	tests := []struct {
		name                  string
		metadata, value, json bool
		want                  bool
		wantUsageErr          bool
	}{
		{name: "default prints the value", want: true},
		{name: "--metadata", metadata: true, want: false},
		{name: "--value", value: true, want: true},
		{name: "--json prints metadata", json: true, want: false},
		{name: "--json --metadata", json: true, metadata: true, want: false},
		{name: "--json --value is refused", json: true, value: true, wantUsageErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := secretGetWantsValue(tt.metadata, tt.value, tt.json)
			if tt.wantUsageErr {
				require.Error(t, err)
				var ue *usageError
				assert.True(t, errors.As(err, &ue), "want a usage error, got %v", err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestSecretGet_ValueOutsideAgent checks that, without an agent's hub
// identity, a value read fails with an error that names --metadata.
func TestSecretGet_ValueOutsideAgent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		factory func() *sciontoolhub.Client
	}{
		{name: "no client", factory: func() *sciontoolhub.Client { return nil }},
		{name: "unconfigured client", factory: func() *sciontoolhub.Client {
			return sciontoolhub.NewClientWithConfig("", "", "")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetSecretGetFlags(t)
			orig := newSecretValueHubClient
			newSecretValueHubClient = tc.factory
			t.Cleanup(func() { newSecretValueHubClient = orig })

			out, err := runSecretGetTest(t, "MY_KEY")
			require.Error(t, err)
			assert.True(t, errors.Is(err, errSecretValueNeedsAgent))
			assert.Contains(t, err.Error(), "scion secret get KEY --metadata")
			assert.Contains(t, err.Error(), "agent container")
			assert.Empty(t, out)
		})
	}
}

// TestSecretGet_HubErrorReturned checks that a Hub error is returned and
// nothing is written to stdout.
func TestSecretGet_HubErrorReturned(t *testing.T) {
	h := &secretGetTestHub{status: http.StatusNotFound}
	useSecretGetHub(t, h.start(t))

	out, err := runSecretGetTest(t, "MY_KEY")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to get secret")
	assert.Contains(t, err.Error(), "404")
	assert.Empty(t, out)
}

// TestSecretGet_Flags checks that --metadata and --value are registered and
// marked mutually exclusive.
func TestSecretGet_Flags(t *testing.T) {
	for _, name := range []string{"metadata", "value"} {
		f := agentSecretGetCmd.Flags().Lookup(name)
		require.NotNil(t, f, "flag --%s", name)
		assert.Equal(t, "false", f.DefValue)
		groups := f.Annotations["cobra_annotation_mutually_exclusive"]
		assert.Contains(t, groups, "metadata value", "--%s must be exclusive with the other view flag", name)
	}
}

// TestPrintSecretMetadata pins the text metadata view printed by
// `scion secret get KEY --metadata` (the output plain get printed before
// ptone/scion#5013).
func TestPrintSecretMetadata(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	var buf bytes.Buffer
	printSecretMetadata(&buf, &hubclient.Secret{
		Key:         "MY_CERT",
		Scope:       "project",
		SecretType:  "file",
		Target:      "~/certs/cert.pem",
		Version:     3,
		Created:     ts,
		Updated:     ts,
		Description: "fake cert",
	})
	out := buf.String()
	for _, want := range []string{
		"Secret: MY_CERT\n",
		"  Scope:   project\n",
		"  Type:    file\n",
		"  Target:  ~/certs/cert.pem\n",
		"  Version: 3\n",
		"  Created: ",
		"  Updated: ",
		"  Description: fake cert\n",
	} {
		assert.Contains(t, out, want)
	}
	assert.NotContains(t, out, "Ref:")

	// An empty type is shown as environment, and a target equal to the key
	// is not repeated.
	buf.Reset()
	printSecretMetadata(&buf, &hubclient.Secret{Key: "K", Scope: "project", Target: "K", Created: ts, Updated: ts})
	assert.Contains(t, buf.String(), "  Type:    environment\n")
	assert.NotContains(t, buf.String(), "Target:")
}

// TestSecretCmd_AgentMode checks that agent mode keeps secret get and list
// and removes secret set, against the real command tree (ptone/scion#5013).
func TestSecretCmd_AgentMode(t *testing.T) {
	real := resolveCommandPath(rootCmd, "secret")
	require.NotNil(t, real, "scion secret must exist in the real command tree")
	for _, path := range []string{"secret.get", "secret.list", "secret.set"} {
		require.NotNil(t, resolveCommandPath(rootCmd, path), "real tree should contain %s", path)
	}

	t.Setenv("SCION_CLI_MODE", "agent")
	root := &cobra.Command{Use: "scion"}
	root.AddCommand(cloneCommandShape(real))
	applyModeRestrictions(root, resolveMode())
	assert.NotNil(t, resolveCommandPath(root, "secret.get"), "secret get must be available in agent mode")
	assert.NotNil(t, resolveCommandPath(root, "secret.list"), "secret list must be available in agent mode")
	assert.Nil(t, resolveCommandPath(root, "secret.set"), "secret set must stay out of agent mode")
}
