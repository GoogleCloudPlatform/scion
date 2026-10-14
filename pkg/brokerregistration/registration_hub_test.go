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

package brokerregistration

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/knadh/koanf/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

const realHubDevToken = "scion_dev_brokerregistration_test_0123456789abcdef"

// realHubSeq makes each real Hub's in-memory database unique per run, so
// repeated runs of one test (-count) never share a database.
var realHubSeq atomic.Int64

// newRealHub runs the in-process Hub (SQLite) with hub.flat_runtime_brokers
// on and a dev user that may create Runtime Brokers.
func newRealHub(t *testing.T) (*httptest.Server, store.Store) {
	t.Helper()
	ctx := context.Background()
	dbName := fmt.Sprintf("%s_%d", strings.NewReplacer("/", "_", " ", "_").Replace(t.Name()), realHubSeq.Add(1))
	client, err := entc.OpenSQLite("file:"+dbName+"?mode=memory&cache=shared", entc.PoolConfig{})
	require.NoError(t, err)
	require.NoError(t, entc.AutoMigrate(ctx, client))
	s := entadapter.NewCompositeStore(client)
	t.Cleanup(func() { _ = s.Close() })

	_, err = s.UpsertHubSetting(ctx, "experiments",
		json.RawMessage(fmt.Sprintf(`{"overrides":{%q:true}}`, experiments.FlatRuntimeBrokers)), "test", -1, "managed")
	require.NoError(t, err)

	cfg := hub.DefaultServerConfig()
	cfg.DevAuthToken = realHubDevToken
	cfg.DisableCloudLogQuery = true
	cfg.DevUserConfig = hub.DevUserConfig{Username: "dev", DisplayName: "Development User", Email: "dev@localhost"}
	srv, err := hub.New(cfg, s)
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	ops := hub.NewOperationalSettings(s, koanf.New("."), koanf.New("."))
	_, err = ops.Refresh(ctx)
	require.NoError(t, err)
	srv.SetOperationalSettings(ops)

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, s
}

func userClient(t *testing.T, url string) hubclient.Client {
	t.Helper()
	c, err := hubclient.New(url, hubclient.WithDevToken(realHubDevToken))
	require.NoError(t, err)
	return c
}

// TestRegisterInstance_RealHubRegistersAndActivates registers through the
// real Hub's user-credential endpoint and join, then validates activation
// with the saved HMAC credentials through carrier (a).
func TestRegisterInstance_RealHubRegistersAndActivates(t *testing.T) {
	ts, s := newRealHub(t)
	ti := newTestInstance(t)

	creds, err := RegisterInstance(context.Background(), userClient(t, ts.URL), ti.inst, ti.id, testHubName, ti.credDir)
	require.NoError(t, err)
	assert.Equal(t, ti.id.RuntimeBrokerID, creds.BrokerID)

	row, err := s.GetRuntimeBroker(context.Background(), ti.id.RuntimeBrokerID)
	require.NoError(t, err)
	require.NotNil(t, row.RuntimeTarget, "the Hub stored a flat row")
	assert.Equal(t, ti.id.RuntimeTarget.ID, row.RuntimeTarget.ID)
	assert.Empty(t, row.Profiles)

	list, err := LoadInstanceCredentials(ti.globalDir, ti.id)
	require.NoError(t, err)
	require.Len(t, list, 1)
	// A nil client builds the HMAC client from the saved credentials; the
	// endpoint saved is the Hub's advertised one, so point it at the test
	// server.
	saved := list[0]
	saved.HubEndpoint = ts.URL
	require.NoError(t, ValidateActivation(context.Background(), nil, ti.id, &saved))
}

func TestRegisterInstance_UserCredentialRequired(t *testing.T) {
	ts, _ := newRealHub(t)
	ti := newTestInstance(t)

	// A first, user-authorized registration gives this instance a valid
	// Runtime Broker HMAC identity.
	creds, err := RegisterInstance(context.Background(), userClient(t, ts.URL), ti.inst, ti.id, testHubName, ti.credDir)
	require.NoError(t, err)
	secret, err := base64.StdEncoding.DecodeString(creds.SecretKey)
	require.NoError(t, err)
	hmacClient, err := hubclient.New(ts.URL, hubclient.WithHMACAuth(creds.BrokerID, secret))
	require.NoError(t, err)

	// The Runtime Broker HMAC identity alone is not admitted.
	otherCredDir := filepath.Join(t.TempDir(), HubCredentialsDirName)
	got, err := RegisterInstance(context.Background(), hmacClient, ti.inst, ti.id, testHubName, otherCredDir)
	require.Error(t, err)
	assert.Nil(t, got)
	assert.Contains(t, err.Error(), "flat Runtime Broker registration failed")
	var apiErr *apiclient.APIError
	if assert.True(t, errors.As(err, &apiErr), "the Hub's answer: %v", err) {
		assert.Equal(t, 401, apiErr.StatusCode, "the registration endpoint requires a user credential")
	}
	_, statErr := os.Stat(otherCredDir)
	assert.True(t, os.IsNotExist(statErr), "nothing saved for a refused registration")

	// The user-authorized credentials saved earlier still validate: the
	// refused call rotated nothing.
	saved, err := LoadInstanceCredentials(ti.globalDir, ti.id)
	require.NoError(t, err)
	saved[0].HubEndpoint = ts.URL
	require.NoError(t, ValidateActivation(context.Background(), nil, ti.id, &saved[0]))
}

// TestRegisterInstance_CapabilitiesBeforeFirstHeartbeat: the static
// capabilities sent at registration are stored by the join, so the row
// carries what the heartbeat will report before any heartbeat arrives
// (ptone/scion#2918).
func TestRegisterInstance_CapabilitiesBeforeFirstHeartbeat(t *testing.T) {
	ts, s := newRealHub(t)
	ti := newTestInstance(t)
	rt := scionrt.NewDockerRuntime()

	_, err := RegisterInstance(context.Background(), userClient(t, ts.URL), ti.inst, ti.id, testHubName, ti.credDir,
		WithOptions(Options{Capabilities: runtimebroker.StaticCapabilityNames(rt), Version: "test"}))
	require.NoError(t, err)

	row, err := s.GetRuntimeBroker(context.Background(), ti.id.RuntimeBrokerID)
	require.NoError(t, err)
	require.NotNil(t, row.Capabilities)
	want := runtimebroker.StaticCapabilities(rt)
	assert.Equal(t, want.Sync, row.Capabilities.Sync)
	assert.Equal(t, want.Attach, row.Capabilities.Attach)
	assert.Equal(t, want.Reprovision, row.Capabilities.Reprovision)
	assert.Equal(t, want.AsyncLaunch, row.Capabilities.AsyncLaunch)
	assert.Equal(t, want.EmptyPerAgentWorkspace, row.Capabilities.EmptyPerAgentWorkspace)
	assert.Equal(t, want.AgentMove, row.Capabilities.AgentMove)
	assert.Equal(t, want.ReprovisionEmptyPerAgent, row.Capabilities.ReprovisionEmptyPerAgent)
	assert.True(t, row.Capabilities.AsyncLaunch, "asyncLaunch is known before the first heartbeat")
	assert.True(t, row.Capabilities.EmptyPerAgentWorkspace, "a Docker instance supports empty-per-agent workspaces")
}
