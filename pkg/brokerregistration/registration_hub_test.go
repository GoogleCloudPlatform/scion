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
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
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
	// MaxOpenConns must be 1, as in the production SQLite pool and the
	// pkg/hub test store: with more than one connection to a shared-cache
	// database, the Hub's asynchronous user last-seen write can hold a table
	// lock that fails a concurrent transaction with SQLITE_LOCKED
	// (ptone/scion#5291).
	client, err := entc.OpenSQLite("file:"+dbName+"?mode=memory&cache=shared", entc.PoolConfig{MaxOpenConns: 1})
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
	want := runtimebroker.StaticCapabilities(rt, "")
	assert.Equal(t, want.Sync, row.Capabilities.Sync)
	assert.Equal(t, want.Attach, row.Capabilities.Attach)
	assert.Equal(t, want.Reprovision, row.Capabilities.Reprovision)
	assert.Equal(t, want.AsyncLaunch, row.Capabilities.AsyncLaunch)
	assert.Equal(t, want.EmptyPerAgentWorkspace, row.Capabilities.EmptyPerAgentWorkspace)
	assert.Equal(t, want.AgentMove, row.Capabilities.AgentMove)
	assert.Equal(t, want.ReprovisionEmptyPerAgent, row.Capabilities.ReprovisionEmptyPerAgent)
	assert.Equal(t, want.ChangedFields, row.Capabilities.ChangedFields)
	assert.True(t, row.Capabilities.ChangedFields, "changedFields is known before the first heartbeat")
	assert.Equal(t, want.ClearedFields, row.Capabilities.ClearedFields)
	assert.True(t, row.Capabilities.ClearedFields, "clearedFields is known before the first heartbeat")
	assert.True(t, row.Capabilities.AsyncLaunch, "asyncLaunch is known before the first heartbeat")
	assert.True(t, row.Capabilities.EmptyPerAgentWorkspace, "a Docker instance supports empty-per-agent workspaces")
}

type stubGCPTokenGen struct{ email string }

func (g *stubGCPTokenGen) GenerateAccessToken(context.Context, string, []string) (*hub.GCPAccessToken, error) {
	return nil, errors.New("unused")
}
func (g *stubGCPTokenGen) GenerateIDToken(context.Context, string, string) (*hub.GCPIDToken, error) {
	return nil, errors.New("unused")
}
func (g *stubGCPTokenGen) VerifyImpersonation(context.Context, string) error { return nil }
func (g *stubGCPTokenGen) ServiceAccountEmail() string                       { return g.email }

// newCloudRunRealHub starts an in-process Hub with both flat experiments on
// and returns the Hub server, httptest server, and backing store so actAs claim
// enforcement can be wired and inspected.
func newCloudRunRealHub(t *testing.T, checker store.CallerPermissionChecker) (*hub.Server, *httptest.Server, store.Store) {
	t.Helper()
	ctx := context.Background()
	dbName := fmt.Sprintf("%s_%d", strings.NewReplacer("/", "_", " ", "_").Replace(t.Name()), realHubSeq.Add(1))
	client, err := entc.OpenSQLite("file:"+dbName+"?mode=memory&cache=shared", entc.PoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	require.NoError(t, entc.AutoMigrate(ctx, client))
	s := entadapter.NewCompositeStore(client)
	t.Cleanup(func() { _ = s.Close() })

	_, err = s.UpsertHubSetting(ctx, "experiments",
		json.RawMessage(fmt.Sprintf(`{"overrides":{%q:true,%q:true}}`, experiments.FlatRuntimeBrokers, experiments.FlatCloudRunTarget)), "test", -1, "managed")
	require.NoError(t, err)

	cfg := hub.DefaultServerConfig()
	cfg.DevAuthToken = realHubDevToken
	cfg.DisableCloudLogQuery = true
	cfg.DevUserConfig = hub.DevUserConfig{Username: "dev", DisplayName: "Development User", Email: "dev@localhost"}
	cfg.GCPIAMCheckMode = hub.SAAssignCheckEnforce
	srv, err := hub.New(cfg, s)
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	if checker != nil {
		srv.SetGCPTokenGenerator(&stubGCPTokenGen{email: "hub@test.iam.gserviceaccount.com"})
		srv.SetSAAssignChecker(checker)
	}
	ops := hub.NewOperationalSettings(s, koanf.New("."), koanf.New("."))
	_, err = ops.Refresh(ctx)
	require.NoError(t, err)
	srv.SetOperationalSettings(ops)

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, ts, s
}

func newCloudRunTestInstance(t *testing.T, serviceAccount string) *testInstance {
	t.Helper()
	globalDir := t.TempDir()
	inst := config.V1RuntimeBrokerInstanceConfig{
		Key:  "cr-remote",
		Name: "cr-remote",
		RuntimeTarget: &config.V1RuntimeTargetConfig{
			Type:           config.RuntimeTargetTypeCloudRun,
			DisplayName:    "Remote Cloud Run",
			ProjectID:      "cr-proj-1",
			Region:         "us-central1",
			ServiceAccount: serviceAccount,
		},
	}
	scope := brokeridentity.ExecutionScope{
		Type:     config.RuntimeTargetTypeCloudRun,
		CloudRun: &brokeridentity.CloudRunScope{ProjectNumber: "123456789012", Region: "us-central1"},
	}
	id, err := brokeridentity.LoadOrCreate(brokeridentity.InstanceDir(globalDir, inst.Key), inst.Key, config.RuntimeTargetTypeCloudRun, scope, nil)
	require.NoError(t, err)
	return &testInstance{globalDir: globalDir, inst: inst, id: id, credDir: InstanceCredentialsDir(globalDir, inst.Key)}
}

func TestRegisterInstance_CloudRunSendsAndEnforcesGCPHostPair(t *testing.T) {
	ctx := context.Background()
	const (
		saA = "agents-a@cr-proj-1.iam.gserviceaccount.com"
		saB = "agents-b@cr-proj-2.iam.gserviceaccount.com"
	)

	t.Run("denied without actAs on runtime_target.service_account", func(t *testing.T) {
		checker := store.NewFakeCallerPermissionChecker() // denies all targets
		_, ts, s := newCloudRunRealHub(t, checker)
		ti := newCloudRunTestInstance(t, saA)

		creds, err := RegisterInstance(ctx, userClient(t, ts.URL), ti.inst, ti.id, testHubName, ti.credDir)
		require.Error(t, err)
		assert.Nil(t, creds)
		var apiErr *apiclient.APIError
		require.ErrorAs(t, err, &apiErr)
		assert.Equal(t, 403, apiErr.StatusCode)
		assert.GreaterOrEqual(t, checker.CallCount(), 1)

		_, getErr := s.GetRuntimeBroker(ctx, ti.id.RuntimeBrokerID)
		assert.ErrorIs(t, getErr, store.ErrNotFound, "denied registration creates no row")
	})

	t.Run("fills unset pair with actAs and emits KeptSetting on mismatch", func(t *testing.T) {
		checker := store.NewFakeCallerPermissionChecker().AllowTarget(saA).AllowTarget(saB)
		_, ts, s := newCloudRunRealHub(t, checker)
		ti := newCloudRunTestInstance(t, saA)

		var out strings.Builder
		_, err := RegisterInstance(ctx, userClient(t, ts.URL), ti.inst, ti.id, testHubName, ti.credDir,
			WithOptions(Options{Out: &out}))
		require.NoError(t, err)
		assert.Empty(t, out.String())

		row, err := s.GetRuntimeBroker(ctx, ti.id.RuntimeBrokerID)
		require.NoError(t, err)
		assert.Equal(t, saA, row.GCPHostServiceAccountEmail)
		assert.Equal(t, "cr-proj-1", row.GCPHostProjectID)
		assert.Equal(t, store.GCPHostSourceRegistration, row.GCPHostSource)

		// Re-registering with a changed service_account keeps the stored pair
		// (fill-only-while-unset) and prints KeptSettings.
		ti.inst.RuntimeTarget.ServiceAccount = saB
		out.Reset()
		_, err = RegisterInstance(ctx, userClient(t, ts.URL), ti.inst, ti.id, testHubName, ti.credDir,
			WithOptions(Options{Out: &out}))
		require.NoError(t, err)
		assert.Contains(t, out.String(), "gcpHostServiceAccountEmail")
		assert.Contains(t, out.String(), saA)

		rowAfter, err := s.GetRuntimeBroker(ctx, ti.id.RuntimeBrokerID)
		require.NoError(t, err)
		assert.Equal(t, saA, rowAfter.GCPHostServiceAccountEmail)
		assert.Equal(t, "cr-proj-1", rowAfter.GCPHostProjectID)
	})
}
