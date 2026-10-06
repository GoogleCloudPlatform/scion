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

//go:build !no_sqlite

package cmd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const joinTestDevToken = "scion_dev_join_token_cli_test_0123456789abcdef"

// isolateJoinEnv clears the environment the join and mint commands read, so
// values exported by the surrounding container do not leak in.
func isolateJoinEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		envBrokerJoinToken, envBrokerID, "SCION_HUB_ENDPOINT", "SCION_HUB_URL",
		"SCION_AUTH_TOKEN", "SCION_HUB_TOKEN", "SCION_DEV_TOKEN", "SCION_DEV_TOKEN_FILE",
		"SCION_TRANSPORT_MODE", "SCION_TRANSPORT_AUDIENCE",
	} {
		t.Setenv(k, "")
	}
	savedProjectPath, savedGlobal, savedHubEndpoint := projectPath, globalMode, hubEndpoint
	savedBrokerID, savedHubName := brokerJoinBrokerID, brokerHubName
	savedTTL, savedJSON := hubBrokersJoinTokenTTL, hubBrokersJoinTokenJSON
	t.Cleanup(func() {
		projectPath, globalMode, hubEndpoint = savedProjectPath, savedGlobal, savedHubEndpoint
		brokerJoinBrokerID, brokerHubName = savedBrokerID, savedHubName
		hubBrokersJoinTokenTTL, hubBrokersJoinTokenJSON = savedTTL, savedJSON
	})
	globalMode, hubEndpoint = false, ""
	brokerJoinBrokerID, brokerHubName = "", ""
	hubBrokersJoinTokenTTL, hubBrokersJoinTokenJSON = 0, false
}

// recordingHub is a fake hub that records each request and answers
// POST /api/v1/brokers and POST /api/v1/brokers/join.
type recordingHub struct {
	*httptest.Server
	mu       sync.Mutex
	requests []recordedRequest
}

type recordedRequest struct {
	Path          string
	Authorization string
	Body          map[string]any
}

func newRecordingHub(t *testing.T) *recordingHub {
	t.Helper()
	h := &recordingHub{}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		h.mu.Lock()
		h.requests = append(h.requests, recordedRequest{Path: r.URL.Path, Authorization: r.Header.Get("Authorization"), Body: body})
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/brokers":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"brokerId":  "11111111-2222-3333-4444-555555555555",
				"joinToken": "scion_join_minted",
				"expiresAt": "2026-10-06T16:00:00Z",
				"reissued":  true,
			})
		case "/api/v1/brokers/join":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"brokerId":    body["brokerId"],
				"secretKey":   base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")),
				"hubEndpoint": "http://" + r.Host,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(h.Close)
	return h
}

func (h *recordingHub) recorded() []recordedRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]recordedRequest(nil), h.requests...)
}

// ---------------------------------------------------------------------------
// runtime-broker join
// ---------------------------------------------------------------------------

func TestBrokerJoin_NoTokenFlag(t *testing.T) {
	assert.Nil(t, brokerJoinCmd.Flags().Lookup("token"), "the token must not be accepted on the command line")
}

func TestBrokerJoin_TokenFromEnv(t *testing.T) {
	isolateJoinEnv(t)
	home, globalDir := brokerTestHome(t)
	fake := newRecordingHub(t)
	projectPath = setupSecretProject(t, home, fake.URL)
	t.Setenv("SCION_HUB_ENDPOINT", fake.URL)
	setBrokerFlagForTest(t, brokerJoinCmd, "port", strconv.Itoa(unusedPort(t)))

	const brokerID = "11111111-2222-3333-4444-555555555555"
	t.Setenv(envBrokerJoinToken, "  scion_join_from_env\n")
	brokerJoinBrokerID = brokerID

	stdout, stderr := captureStdoutStderr(t, func() {
		require.NoError(t, runBrokerJoin(brokerJoinCmd, nil))
	})
	assert.Contains(t, stderr, "broker server not running", "a missing local broker is only a warning")
	assert.Contains(t, stdout, "Broker "+brokerID+" joined hub "+fake.URL)
	assert.NotContains(t, stdout+stderr, "scion_join_from_env", "the token is never printed")

	reqs := fake.recorded()
	require.Len(t, reqs, 1)
	assert.Equal(t, "/api/v1/brokers/join", reqs[0].Path)
	assert.Empty(t, reqs[0].Authorization, "join sends no user credential")
	assert.Equal(t, "scion_join_from_env", reqs[0].Body["joinToken"], "surrounding whitespace is trimmed")
	assert.Equal(t, brokerID, reqs[0].Body["brokerId"])

	creds, err := brokercredentials.NewMultiStore("").Load(brokercredentials.DeriveHubName(fake.URL))
	require.NoError(t, err)
	assert.Equal(t, brokerID, creds.BrokerID)
	assert.Equal(t, fake.URL, creds.HubEndpoint)
	assert.NotEmpty(t, creds.SecretKey)

	gs, err := config.LoadSettings(globalDir)
	require.NoError(t, err)
	require.NotNil(t, gs.Hub)
	assert.Equal(t, brokerID, gs.Hub.BrokerID)
}

func TestBrokerJoin_BrokerIDFromEnv(t *testing.T) {
	isolateJoinEnv(t)
	home, _ := brokerTestHome(t)
	fake := newRecordingHub(t)
	projectPath = setupSecretProject(t, home, fake.URL)
	t.Setenv("SCION_HUB_ENDPOINT", fake.URL)
	setBrokerFlagForTest(t, brokerJoinCmd, "port", strconv.Itoa(unusedPort(t)))

	t.Setenv(envBrokerJoinToken, "scion_join_x")
	t.Setenv(envBrokerID, "22222222-2222-3333-4444-555555555555")
	captureStdoutStderr(t, func() {
		require.NoError(t, runBrokerJoin(brokerJoinCmd, nil))
	})
	reqs := fake.recorded()
	require.Len(t, reqs, 1)
	assert.Equal(t, "22222222-2222-3333-4444-555555555555", reqs[0].Body["brokerId"])
}

func TestBrokerJoin_RejectsBeforeAnyRequest(t *testing.T) {
	cases := []struct {
		name     string
		token    string
		brokerID string
		wantErr  string
	}{
		{"missing token", "", "11111111-2222-3333-4444-555555555555", "no join token"},
		{"whitespace token", "  \n", "11111111-2222-3333-4444-555555555555", "no join token"},
		{"wrong prefix", "scion_dev_abc", "11111111-2222-3333-4444-555555555555", "not a join token"},
		{"missing broker ID", "scion_join_abc", "", "broker ID is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateJoinEnv(t)
			home, _ := brokerTestHome(t)
			fake := newRecordingHub(t)
			projectPath = setupSecretProject(t, home, fake.URL)
			t.Setenv("SCION_HUB_ENDPOINT", fake.URL)
			t.Setenv(envBrokerJoinToken, tc.token)
			brokerJoinBrokerID = tc.brokerID

			err := runBrokerJoin(brokerJoinCmd, nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			if strings.TrimSpace(tc.token) != "" {
				assert.NotContains(t, err.Error(), strings.TrimSpace(tc.token), "the rejected value is not echoed")
			}
			assert.Empty(t, fake.recorded(), "nothing is sent to the hub")
		})
	}
}

// ---------------------------------------------------------------------------
// hub brokers join-token create
// ---------------------------------------------------------------------------

func runJoinTokenCreateForTest(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	hubBrokersJoinTokenCreateCmd.SetOut(&out)
	hubBrokersJoinTokenCreateCmd.SetErr(&errOut)
	t.Cleanup(func() {
		hubBrokersJoinTokenCreateCmd.SetOut(nil)
		hubBrokersJoinTokenCreateCmd.SetErr(nil)
	})
	err = runHubBrokersJoinTokenCreate(hubBrokersJoinTokenCreateCmd, args)
	return out.String(), errOut.String(), err
}

func TestHubBrokersJoinTokenCreate_TextOutput(t *testing.T) {
	isolateJoinEnv(t)
	home, _ := brokerTestHome(t)
	fake := newRecordingHub(t)
	projectPath = setupSecretProject(t, home, fake.URL)
	t.Setenv("SCION_HUB_ENDPOINT", fake.URL)
	hubBrokersJoinTokenTTL = 30 * time.Minute

	stdout, stderr, err := runJoinTokenCreateForTest(t, "build-host-3")
	require.NoError(t, err)
	assert.Equal(t, "scion_join_minted\n", stdout, "stdout carries only the token")
	assert.Contains(t, stderr, "Join token for broker 'build-host-3' (ID 11111111-2222-3333-4444-555555555555) expires 2026-10-06T16:00:00Z")
	assert.Contains(t, stderr, "previous unused join token")
	assert.Contains(t, stderr, "scion runtime-broker join --broker-id 11111111-2222-3333-4444-555555555555")
	assert.NotContains(t, stderr, "scion_join_minted")

	reqs := fake.recorded()
	require.Len(t, reqs, 1)
	assert.Equal(t, "/api/v1/brokers", reqs[0].Path)
	assert.Equal(t, "build-host-3", reqs[0].Body["name"])
	assert.EqualValues(t, 1800, reqs[0].Body["joinTokenTtlSeconds"])
	assert.Equal(t, true, reqs[0].Body["preserveSettings"])
	assert.NotContains(t, reqs[0].Body, "brokerId", "the hub generates the broker ID")
	assert.NotContains(t, reqs[0].Body, "autoProvide")
}

func TestHubBrokersJoinTokenCreate_JSON(t *testing.T) {
	isolateJoinEnv(t)
	home, _ := brokerTestHome(t)
	fake := newRecordingHub(t)
	projectPath = setupSecretProject(t, home, fake.URL)
	t.Setenv("SCION_HUB_ENDPOINT", fake.URL)
	hubBrokersJoinTokenJSON = true

	stdout, stderr, err := runJoinTokenCreateForTest(t, "build-host-3")
	require.NoError(t, err)
	assert.Empty(t, stderr)

	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout), &got), "stdout: %s", stdout)
	assert.Equal(t, map[string]any{
		"brokerId":    "11111111-2222-3333-4444-555555555555",
		"brokerName":  "build-host-3",
		"joinToken":   "scion_join_minted",
		"expiresAt":   "2026-10-06T16:00:00Z",
		"hubEndpoint": fake.URL,
		"reissued":    true,
	}, got)

	reqs := fake.recorded()
	require.Len(t, reqs, 1)
	assert.NotContains(t, reqs[0].Body, "joinTokenTtlSeconds", "no --ttl leaves the hub default")
}

func TestHubBrokersJoinTokenCreate_TTLRange(t *testing.T) {
	for _, ttl := range []time.Duration{time.Second, 4 * time.Minute, 25 * time.Hour, -time.Minute} {
		t.Run(ttl.String(), func(t *testing.T) {
			isolateJoinEnv(t)
			home, _ := brokerTestHome(t)
			fake := newRecordingHub(t)
			projectPath = setupSecretProject(t, home, fake.URL)
			t.Setenv("SCION_HUB_ENDPOINT", fake.URL)
			hubBrokersJoinTokenTTL = ttl

			_, _, err := runJoinTokenCreateForTest(t, "build-host-3")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "--ttl must be between 5m0s and 24h0m0s")
			assert.Empty(t, fake.recorded(), "an out-of-range --ttl is rejected before any request")
		})
	}
	for ttl, want := range map[time.Duration]int{5 * time.Minute: 300, 24 * time.Hour: 86400, 0: 0} {
		got, err := brokerJoinTokenTTLSeconds(ttl)
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}
}

// ---------------------------------------------------------------------------
// End to end: mint on one machine, join on another, against a real hub.
// ---------------------------------------------------------------------------

func newJoinTestHub(t *testing.T) *httptest.Server {
	t.Helper()
	cfg := hub.DefaultServerConfig()
	cfg.DevAuthToken = joinTestDevToken
	cfg.DisableCloudLogQuery = true
	cfg.DevUserConfig = hub.DevUserConfig{Username: "dev", DisplayName: "Development User", Email: "dev@localhost"}
	srv, err := hub.New(cfg, newTestStore(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestBrokerJoinToken_MintOnOneMachineJoinOnAnother(t *testing.T) {
	isolateJoinEnv(t)
	ts := newJoinTestHub(t)

	// Machine A: a user with a hub session mints the token.
	homeA, _ := brokerTestHome(t)
	projectPath = setupSecretProject(t, homeA, ts.URL)
	t.Setenv("SCION_HUB_ENDPOINT", ts.URL)
	t.Setenv("SCION_DEV_TOKEN", joinTestDevToken)
	hubBrokersJoinTokenJSON = true
	hubBrokersJoinTokenTTL = 10 * time.Minute
	stdout, _, err := runJoinTokenCreateForTest(t, "headless-build-host")
	require.NoError(t, err)
	var minted brokerJoinTokenOutput
	require.NoError(t, json.Unmarshal([]byte(stdout), &minted), "stdout: %s", stdout)
	require.NotEmpty(t, minted.BrokerID)
	require.True(t, strings.HasPrefix(minted.JoinToken, brokerJoinTokenPrefix))

	// Machine B: a fresh home with no hub user credential at all.
	t.Setenv("SCION_DEV_TOKEN", "")
	homeB, globalDirB := brokerTestHome(t)
	require.NotEqual(t, homeA, homeB)
	projectPath = setupSecretProject(t, homeB, ts.URL)
	t.Setenv("SCION_HUB_ENDPOINT", ts.URL)
	setBrokerFlagForTest(t, brokerJoinCmd, "port", strconv.Itoa(unusedPort(t)))
	t.Setenv(envBrokerJoinToken, minted.JoinToken)
	brokerJoinBrokerID = minted.BrokerID

	joinOut, _ := captureStdoutStderr(t, func() {
		require.NoError(t, runBrokerJoin(brokerJoinCmd, nil))
	})
	assert.Contains(t, joinOut, "Broker "+minted.BrokerID+" joined hub")

	creds, err := brokercredentials.NewMultiStore("").Load(brokercredentials.DeriveHubName(ts.URL))
	require.NoError(t, err)
	assert.Equal(t, minted.BrokerID, creds.BrokerID)
	assert.Equal(t, ts.URL, creds.HubEndpoint)
	gs, err := config.LoadSettings(globalDirB)
	require.NoError(t, err)
	require.NotNil(t, gs.Hub)
	assert.Equal(t, minted.BrokerID, gs.Hub.BrokerID)

	// The saved credentials authenticate the broker over HMAC.
	key, err := base64.StdEncoding.DecodeString(creds.SecretKey)
	require.NoError(t, err)
	brokerClient, err := hubclient.New(ts.URL, hubclient.WithHMACAuth(creds.BrokerID, key))
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, brokerClient.RuntimeBrokers().Heartbeat(ctx, creds.BrokerID, &hubclient.BrokerHeartbeat{Status: "online"}))

	// The token was single use.
	err = runBrokerJoin(brokerJoinCmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid join token")
}

// ---------------------------------------------------------------------------
// CLI modes
// ---------------------------------------------------------------------------

// TestBrokerJoinTokenCommands_ModeAvailability pins the mode availability
// of the two join-token verbs: available in human and assistant modes, not
// in agent mode.
func TestBrokerJoinTokenCommands_ModeAvailability(t *testing.T) {
	paths := []string{"hub.brokers.join-token.create", "runtime-broker.join"}
	for _, p := range paths {
		assert.False(t, assistantDenied[p], "%s should be available in assistant mode", p)
		assert.False(t, agentAllowed[p], "%s should not be available in agent mode", p)
	}

	find := func(root *cobra.Command, path ...string) *cobra.Command {
		c, _, err := root.Find(path)
		if err != nil || c == root || c.Name() != path[len(path)-1] {
			return nil
		}
		return c
	}
	for _, mode := range []struct {
		apply func(*cobra.Command)
		want  bool
	}{
		{applyAssistantMode, true},
		{applyAgentMode, false},
	} {
		root := &cobra.Command{Use: "scion"}
		hubC := &cobra.Command{Use: "hub"}
		brokersC := &cobra.Command{Use: "brokers"}
		jt := &cobra.Command{Use: "join-token"}
		jt.AddCommand(&cobra.Command{Use: "create", Run: func(*cobra.Command, []string) {}})
		brokersC.AddCommand(jt)
		hubC.AddCommand(brokersC)
		rb := &cobra.Command{Use: "runtime-broker"}
		rb.AddCommand(&cobra.Command{Use: "join", Run: func(*cobra.Command, []string) {}})
		root.AddCommand(hubC, rb)

		mode.apply(root)
		assert.Equal(t, mode.want, find(root, "hub", "brokers", "join-token", "create") != nil)
		assert.Equal(t, mode.want, find(root, "runtime-broker", "join") != nil)
	}
}
