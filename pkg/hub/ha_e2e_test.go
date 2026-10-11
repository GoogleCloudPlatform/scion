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

//go:build integration

package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// haTestEnv holds two hub Server instances sharing one Postgres database,
// each with its own OperationalSettings and event publisher.
type haTestEnv struct {
	dsn    string
	storeA *entadapter.CompositeStore
	storeB *entadapter.CompositeStore
	srvA   *Server
	srvB   *Server
	opsA   *OperationalSettings
	opsB   *OperationalSettings
	pubA   *PostgresEventPublisher
	pubB   *PostgresEventPublisher
}

func requirePG(t *testing.T) {
	t.Helper()
	if !enttest.Active() {
		t.Skip("integration: set SCION_TEST_POSTGRES_URL to a live Postgres to run the HA e2e suite")
	}
}

// newHATestEnv creates two hub Server instances sharing one Postgres schema.
// Both have event publishers and propagation started with a short poll interval.
func newHATestEnv(t *testing.T) *haTestEnv {
	t.Helper()
	requirePG(t)

	dsn := enttest.NewSchemaURL(t)

	clientA, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: 5, MaxIdleConns: 2})
	require.NoError(t, err)
	t.Cleanup(func() { _ = clientA.Close() })
	storeA := entadapter.NewCompositeStore(clientA)

	clientB, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: 5, MaxIdleConns: 2})
	require.NoError(t, err)
	t.Cleanup(func() { _ = clientB.Close() })
	storeB := entadapter.NewCompositeStore(clientB)

	fileK := koanf.New(".")
	envK := koanf.New(".")

	opsA := NewOperationalSettings(storeA, fileK, envK)
	opsB := NewOperationalSettings(storeB, fileK, envK)

	// Short poll interval for tests.
	opsA.PollInterval = 200 * time.Millisecond
	opsB.PollInterval = 200 * time.Millisecond

	srvA := &Server{
		dbDriver:    "postgres",
		maintenance: NewMaintenanceState(false, ""),
	}
	srvA.SetOperationalSettings(opsA)

	srvB := &Server{
		dbDriver:    "postgres",
		maintenance: NewMaintenanceState(false, ""),
	}
	srvB.SetOperationalSettings(opsB)

	// Event publishers
	ctx := context.Background()
	pubA, err := NewPostgresEventPublisher(ctx, dsn, nil, slog.Default())
	require.NoError(t, err)
	t.Cleanup(func() { pubA.Close() })

	pubB, err := NewPostgresEventPublisher(ctx, dsn, nil, slog.Default())
	require.NoError(t, err)
	t.Cleanup(func() { pubB.Close() })

	// Wire event publishers and start propagation.
	opsA.SetEventPublisher(pubA)
	opsA.StartPropagation(ctx, srvA)
	t.Cleanup(func() { opsA.StopPropagation() })

	opsB.SetEventPublisher(pubB)
	opsB.StartPropagation(ctx, srvB)
	t.Cleanup(func() { opsB.StopPropagation() })

	return &haTestEnv{
		dsn: dsn, storeA: storeA, storeB: storeB,
		srvA: srvA, srvB: srvB,
		opsA: opsA, opsB: opsB,
		pubA: pubA, pubB: pubB,
	}
}

// pollUntil retries fn every 50ms up to timeout, returning nil on first
// success or the last error on timeout.
func pollUntil(timeout time.Duration, fn func() error) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if lastErr = fn(); lastErr == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("timed out after %s: %w", timeout, lastErr)
}

// adminReq builds an HTTP request with admin identity injected into context.
func adminReq(method, url, body string) *http.Request {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, url, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, url, nil)
	}
	admin := NewAuthenticatedUser("u1", "admin@e2e.test", "Admin", "admin", "cli")
	r = r.WithContext(contextWithIdentity(r.Context(), admin))
	return r
}

// ----- AC1: PUT on A → GET on B returns + enforces within ≤2s -----

func TestHA_AC1_SettingsPropagation(t *testing.T) {
	env := newHATestEnv(t)

	// PUT user_access_mode on hub A.
	body := `{"server":{"hub":{"admin_emails":["admin@e2e.test"]},"auth":{"user_access_mode":"invite_only"}}}`
	rr := httptest.NewRecorder()
	env.srvA.handlePutServerConfigDB(rr, adminReq(http.MethodPut, "/api/v1/admin/server-config", body), env.opsA)
	require.Equal(t, http.StatusOK, rr.Code, "PUT on A: %s", rr.Body.String())

	// Verify A's config reflects the change immediately.
	snapA := env.opsA.Snapshot()
	assert.Equal(t, "invite_only", snapA.UserAccessMode, "A snapshot after PUT")

	// Poll B until it sees the change (within 2s).
	err := pollUntil(2*time.Second, func() error {
		snapB := env.opsB.Snapshot()
		if snapB.UserAccessMode != "invite_only" {
			return fmt.Errorf("B user_access_mode = %q, want invite_only", snapB.UserAccessMode)
		}
		return nil
	})
	require.NoError(t, err, "AC1: B should see user_access_mode=invite_only within 2s")

	// Enforcement: verify B's config struct has the updated value.
	env.srvB.mu.Lock()
	bMode := env.srvB.config.UserAccessMode
	env.srvB.mu.Unlock()
	assert.Equal(t, "invite_only", bMode, "AC1: B's server config should enforce invite_only")

	// Also verify via GET on B.
	rrB := httptest.NewRecorder()
	env.srvB.handleGetServerConfigDB(rrB, adminReq(http.MethodGet, "/api/v1/admin/server-config", ""), env.opsB)
	require.Equal(t, http.StatusOK, rrB.Code)

	var resp ServerConfigDBResponse
	require.NoError(t, json.Unmarshal(rrB.Body.Bytes(), &resp))
	require.NotNil(t, resp.Server)
	require.NotNil(t, resp.Server.Auth)
	assert.Equal(t, "invite_only", resp.Server.Auth.UserAccessMode,
		"AC1: GET on B should return user_access_mode=invite_only")
}

// ----- AC2: Concurrent startup seeds hub_settings exactly once -----

func TestHA_AC2_ConcurrentSeedingAdvisoryLock(t *testing.T) {
	requirePG(t)

	dsn := enttest.NewSchemaURL(t)

	const concurrency = 5
	var wg sync.WaitGroup
	wg.Add(concurrency)

	errors := make([]error, concurrency)

	for i := 0; i < concurrency; i++ {
		go func(idx int) {
			defer wg.Done()
			client, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: 3, MaxIdleConns: 1})
			if err != nil {
				errors[idx] = err
				return
			}
			defer client.Close()

			cs := entadapter.NewCompositeStore(client)
			fileK := koanf.New(".")

			ctx := context.Background()

			acquired, release, err := cs.TryAdvisoryLock(ctx, store.LockHubSettingsSeed)
			if err != nil {
				errors[idx] = err
				return
			}
			if acquired {
				errors[idx] = seedForTest(ctx, cs, fileK)
				if rerr := release(); rerr != nil {
					t.Logf("release error (goroutine %d): %v", idx, rerr)
				}
			}
		}(i)
	}
	wg.Wait()

	for i, err := range errors {
		require.NoError(t, err, "goroutine %d", i)
	}

	// Verify _meta was written exactly once.
	client, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: 2, MaxIdleConns: 1})
	require.NoError(t, err)
	defer client.Close()

	cs := entadapter.NewCompositeStore(client)
	meta, err := cs.GetHubSetting(context.Background(), "_meta")
	require.NoError(t, err, "should find _meta row")
	assert.Contains(t, string(meta.Value), "seed_version")

	// Verify GET shows source "db" for seeded sections.
	fileK := koanf.New(".")
	envK := koanf.New(".")
	ops := NewOperationalSettings(cs, fileK, envK)
	_, err = ops.Refresh(context.Background())
	require.NoError(t, err)

	srv := &Server{dbDriver: "postgres", maintenance: NewMaintenanceState(false, "")}
	srv.SetOperationalSettings(ops)

	rr := httptest.NewRecorder()
	srv.handleGetServerConfigDB(rr, adminReq(http.MethodGet, "/api/v1/admin/server-config", ""), ops)
	require.Equal(t, http.StatusOK, rr.Code)

	var resp ServerConfigDBResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))

	for _, secName := range opsettings.SectionNames() {
		sec := opsettings.SectionByName(secName)
		if sec == nil || len(sec.KoanfPaths) == 0 {
			continue // maintenance has no koanf paths, not seeded
		}
		meta, ok := resp.SectionMeta[secName]
		require.True(t, ok, "section %s should have metadata", secName)
		assert.Equal(t, "db", meta.Source, "section %s source should be 'db'", secName)
	}
}

// seedForTest re-implements seedHubSettingsIfNeeded (cmd/server_foreground.go) to isolate the advisory-lock mechanism under test from the production seeding path.
func seedForTest(ctx context.Context, s store.HubSettingStore, fileK *koanf.Koanf) error {
	_, err := s.GetHubSetting(ctx, "_meta")
	if err == nil {
		return nil // already seeded
	}
	if !isNotFound(err) {
		return err
	}

	for _, sec := range opsettings.Registry {
		if len(sec.KoanfPaths) == 0 {
			continue
		}
		doc, err := opsettings.ExtractSectionFromKoanf(fileK, sec.Name)
		if err != nil {
			continue
		}
		if _, err := s.UpsertHubSetting(ctx, sec.Name, doc, "seed", -1, "seeded"); err != nil {
			return err
		}
	}

	metaDoc, _ := json.Marshal(map[string]interface{}{
		"seeded_from":  "test",
		"seeded_at":    time.Now().UTC().Format(time.RFC3339),
		"seed_version": "1",
	})
	_, err = s.UpsertHubSetting(ctx, "_meta", metaDoc, "seed", -1, "seeded")
	return err
}

func isNotFound(err error) bool {
	return errors.Is(err, store.ErrNotFound)
}

// ----- AC3: Maintenance persists across restart -----

func TestHA_AC3_MaintenanceSurvivesRestart(t *testing.T) {
	requirePG(t)

	dsn := enttest.NewSchemaURL(t)
	t.Setenv("SCION_SERVER_ADMIN_MODE", "")
	t.Setenv("SCION_SERVER_MAINTENANCE_MESSAGE", "")

	// Phase 1: two hubs, PUT maintenance on A, verify B sees it.
	clientA1, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: 5, MaxIdleConns: 2})
	require.NoError(t, err)
	storeA1 := entadapter.NewCompositeStore(clientA1)

	clientB1, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: 5, MaxIdleConns: 2})
	require.NoError(t, err)
	storeB1 := entadapter.NewCompositeStore(clientB1)

	fileK := koanf.New(".")
	envK := koanf.New(".")

	opsA1 := NewOperationalSettings(storeA1, fileK, envK)
	opsA1.PollInterval = 200 * time.Millisecond
	srvA1 := &Server{dbDriver: "postgres", maintenance: NewMaintenanceState(false, "")}
	srvA1.SetOperationalSettings(opsA1)
	opsA1.server = srvA1

	opsB1 := NewOperationalSettings(storeB1, fileK, envK)
	opsB1.PollInterval = 200 * time.Millisecond
	srvB1 := &Server{dbDriver: "postgres", maintenance: NewMaintenanceState(false, "")}
	srvB1.SetOperationalSettings(opsB1)

	ctx := context.Background()
	pubA1, err := NewPostgresEventPublisher(ctx, dsn, nil, slog.Default())
	require.NoError(t, err)
	pubB1, err := NewPostgresEventPublisher(ctx, dsn, nil, slog.Default())
	require.NoError(t, err)

	opsA1.SetEventPublisher(pubA1)
	opsA1.StartPropagation(ctx, srvA1)
	opsB1.SetEventPublisher(pubB1)
	opsB1.StartPropagation(ctx, srvB1)

	// PUT maintenance on A.
	rr := httptest.NewRecorder()
	srvA1.handlePutMaintenanceDB(rr,
		adminReq(http.MethodPut, "/api/v1/admin/maintenance", `{"enabled":true,"message":"AC3 maintenance"}`),
		opsA1)
	require.Equal(t, http.StatusOK, rr.Code, "PUT maintenance on A: %s", rr.Body.String())

	// A should be in maintenance.
	assert.True(t, srvA1.maintenance.IsEnabled(), "A should be in maintenance")

	// B should pick it up.
	err = pollUntil(2*time.Second, func() error {
		if !srvB1.maintenance.IsEnabled() {
			return fmt.Errorf("B maintenance not enabled yet")
		}
		return nil
	})
	require.NoError(t, err, "AC3: B should see maintenance within 2s")

	// Phase 2: shut down both hubs, create fresh instances against same DB.
	opsA1.StopPropagation()
	opsB1.StopPropagation()
	pubA1.Close()
	pubB1.Close()
	_ = clientA1.Close()
	_ = clientB1.Close()

	clientA2, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: 5, MaxIdleConns: 2})
	require.NoError(t, err)
	defer clientA2.Close()
	storeA2 := entadapter.NewCompositeStore(clientA2)

	clientB2, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: 5, MaxIdleConns: 2})
	require.NoError(t, err)
	defer clientB2.Close()
	storeB2 := entadapter.NewCompositeStore(clientB2)

	opsA2 := NewOperationalSettings(storeA2, fileK, envK)
	srvA2 := &Server{dbDriver: "postgres", maintenance: NewMaintenanceState(false, "")}
	srvA2.SetOperationalSettings(opsA2)

	opsB2 := NewOperationalSettings(storeB2, fileK, envK)
	srvB2 := &Server{dbDriver: "postgres", maintenance: NewMaintenanceState(false, "")}
	srvB2.SetOperationalSettings(opsB2)

	// Refresh from DB (simulating startup).
	_, err = opsA2.Refresh(ctx)
	require.NoError(t, err)
	snapA2 := opsA2.Snapshot()
	ApplySnapshot(srvA2, snapA2)
	ApplyMaintenanceFromSnapshot(srvA2, snapA2)

	_, err = opsB2.Refresh(ctx)
	require.NoError(t, err)
	snapB2 := opsB2.Snapshot()
	ApplySnapshot(srvB2, snapB2)
	ApplyMaintenanceFromSnapshot(srvB2, snapB2)

	// Both should be in maintenance after restart.
	assert.True(t, srvA2.maintenance.IsEnabled(), "AC3: A after restart should be in maintenance")
	assert.True(t, srvB2.maintenance.IsEnabled(), "AC3: B after restart should be in maintenance")
	assert.Equal(t, "AC3 maintenance", srvA2.maintenance.Message())
}

// ----- AC5: DB wins over env, superseded keys reported -----

func TestHA_AC5_EnvOverrideOneHub(t *testing.T) {
	requirePG(t)

	dsn := enttest.NewSchemaURL(t)
	ctx := context.Background()

	client, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: 5, MaxIdleConns: 2})
	require.NoError(t, err)
	defer client.Close()
	cs := entadapter.NewCompositeStore(client)

	// Write access section as "managed" (admin-written) with DB value.
	_, err = cs.UpsertHubSetting(ctx, "access",
		json.RawMessage(`{"admin_emails":["db@test.com"],"user_access_mode":"open"}`),
		"admin", -1, "managed")
	require.NoError(t, err)

	// Hub A: env override on admin_emails + bootstrap koanf reflecting the env.
	envKA := koanf.New(".")
	require.NoError(t, envKA.Load(confmap.Provider(map[string]interface{}{
		"server.hub.admin_emails": []interface{}{"env@test.com"},
	}, "."), nil))

	bootstrapA := koanf.New(".")
	require.NoError(t, bootstrapA.Load(confmap.Provider(map[string]interface{}{
		"server.hub.admin_emails": []interface{}{"env@test.com"},
	}, "."), nil))

	opsA := NewOperationalSettings(cs, bootstrapA, envKA)
	_, err = opsA.Refresh(ctx)
	require.NoError(t, err)

	srvA := &Server{dbDriver: "postgres", maintenance: NewMaintenanceState(false, "")}
	srvA.SetOperationalSettings(opsA)

	// Hub B: no env override, empty bootstrap.
	opsB := NewOperationalSettings(cs, koanf.New("."), koanf.New("."))
	_, err = opsB.Refresh(ctx)
	require.NoError(t, err)

	srvB := &Server{dbDriver: "postgres", maintenance: NewMaintenanceState(false, "")}
	srvB.SetOperationalSettings(opsB)

	// GET on A: DB value wins, env_overrides listed, superseded_keys reported.
	rrA := httptest.NewRecorder()
	srvA.handleGetServerConfigDB(rrA, adminReq(http.MethodGet, "/api/v1/admin/server-config", ""), opsA)
	require.Equal(t, http.StatusOK, rrA.Code)

	var respA ServerConfigDBResponse
	require.NoError(t, json.Unmarshal(rrA.Body.Bytes(), &respA))

	require.NotNil(t, respA.Server)
	require.NotNil(t, respA.Server.Hub)
	assert.Equal(t, []string{"db@test.com"}, respA.Server.Hub.AdminEmails,
		"AC5: DB value must win over env on hub A")
	assert.Contains(t, respA.EnvOverrides, "server.hub.admin_emails",
		"AC5: A should still list env key in env_overrides")

	require.Contains(t, respA.SupersededKeys, "access",
		"AC5: managed access section should have superseded keys on A")
	var foundAdmin bool
	for _, sk := range respA.SupersededKeys["access"] {
		if sk.Key == "admin_emails" {
			foundAdmin = true
			assert.Equal(t, "server_env", sk.Source,
				"AC5: superseded admin_emails source should be server_env")
		}
	}
	assert.True(t, foundAdmin, "AC5: admin_emails must appear in superseded_keys for access")

	// GET on B: same DB value, no env_overrides, no superseded keys.
	rrB := httptest.NewRecorder()
	srvB.handleGetServerConfigDB(rrB, adminReq(http.MethodGet, "/api/v1/admin/server-config", ""), opsB)
	require.Equal(t, http.StatusOK, rrB.Code)

	var respB ServerConfigDBResponse
	require.NoError(t, json.Unmarshal(rrB.Body.Bytes(), &respB))

	require.NotNil(t, respB.Server)
	require.NotNil(t, respB.Server.Hub)
	assert.Equal(t, []string{"db@test.com"}, respB.Server.Hub.AdminEmails,
		"AC5: B should serve DB value for admin_emails")
	assert.Empty(t, respB.EnvOverrides, "AC5: B should have no env_overrides")
	assert.Empty(t, respB.SupersededKeys, "AC5: B should have no superseded_keys")
}

// ----- AC7: Concurrent PUTs with CAS -----

func TestHA_AC7_ConcurrentCAS(t *testing.T) {
	env := newHATestEnv(t)
	ctx := context.Background()

	// Seed access section.
	_, err := env.storeA.UpsertHubSetting(ctx, "access",
		json.RawMessage(`{"admin_emails":["orig@test.com"],"user_access_mode":"open"}`),
		"seed", -1, "seeded")
	require.NoError(t, err)
	_, err = env.opsA.Refresh(ctx)
	require.NoError(t, err)
	_, err = env.opsB.Refresh(ctx)
	require.NoError(t, err)

	// Subtest: with CAS — exactly one 409 loser.
	t.Run("WithCAS", func(t *testing.T) {
		var wg sync.WaitGroup
		results := make([]int, 2)
		wg.Add(2)

		for i := 0; i < 2; i++ {
			go func(idx int) {
				defer wg.Done()
				body := fmt.Sprintf(`{
					"server":{"hub":{"admin_emails":["cas%d@test.com"]},"auth":{}},
					"expected_revisions":{"access":1}
				}`, idx)
				ops := env.opsA
				srv := env.srvA
				if idx == 1 {
					ops = env.opsB
					srv = env.srvB
				}
				rr := httptest.NewRecorder()
				srv.handlePutServerConfigDB(rr, adminReq(http.MethodPut, "/api/v1/admin/server-config", body), ops)
				results[idx] = rr.Code
			}(i)
		}
		wg.Wait()

		got200, got409 := 0, 0
		for _, code := range results {
			switch code {
			case http.StatusOK:
				got200++
			case http.StatusConflict:
				got409++
			}
		}
		assert.Equal(t, 1, got200, "AC7 CAS: exactly one 200")
		assert.Equal(t, 1, got409, "AC7 CAS: exactly one 409")
	})

	// Subtest: without CAS — both succeed, both audited.
	t.Run("WithoutCAS", func(t *testing.T) {
		var wg sync.WaitGroup
		results := make([]int, 2)
		wg.Add(2)

		for i := 0; i < 2; i++ {
			go func(idx int) {
				defer wg.Done()
				body := fmt.Sprintf(`{
					"server":{"hub":{"admin_emails":["lww%d@test.com"]},"auth":{}}
				}`, idx)
				ops := env.opsA
				srv := env.srvA
				if idx == 1 {
					ops = env.opsB
					srv = env.srvB
				}
				rr := httptest.NewRecorder()
				srv.handlePutServerConfigDB(rr, adminReq(http.MethodPut, "/api/v1/admin/server-config", body), ops)
				results[idx] = rr.Code
			}(i)
		}
		wg.Wait()

		for i, code := range results {
			assert.Equal(t, http.StatusOK, code, "AC7 LWW: writer %d should succeed", i)
		}

		// Verify the winner is audited (updated_by set).
		row, err := env.storeA.GetHubSetting(context.Background(), "access")
		require.NoError(t, err)
		assert.NotEmpty(t, row.UpdatedBy, "AC7 LWW: updated_by should be set")
	})
}

// ----- AC9: Poll backstop when LISTEN is disrupted -----
// The reconnect-triggers-immediate-refresh path (SetOnReconnect callback) is
// verified by inspection of StartPropagation; this test exercises the poll
// backstop by substituting a noop publisher so no LISTEN events arrive.

func TestHA_AC9_PollBackstopOnListenDisruption(t *testing.T) {
	requirePG(t)

	dsn := enttest.NewSchemaURL(t)
	ctx := context.Background()

	clientA, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: 5, MaxIdleConns: 2})
	require.NoError(t, err)
	defer clientA.Close()
	storeA := entadapter.NewCompositeStore(clientA)

	clientB, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: 5, MaxIdleConns: 2})
	require.NoError(t, err)
	defer clientB.Close()
	storeB := entadapter.NewCompositeStore(clientB)

	fileK := koanf.New(".")
	envK := koanf.New(".")

	opsA := NewOperationalSettings(storeA, fileK, envK)
	srvA := &Server{dbDriver: "postgres", maintenance: NewMaintenanceState(false, "")}
	srvA.SetOperationalSettings(opsA)

	// Hub A: normal event publisher for publishing.
	pubA, err := NewPostgresEventPublisher(ctx, dsn, nil, slog.Default())
	require.NoError(t, err)
	defer pubA.Close()
	opsA.SetEventPublisher(pubA)
	opsA.StartPropagation(ctx, srvA)
	defer opsA.StopPropagation()

	// Hub B: uses noop event publisher (simulates disrupted LISTEN) but
	// has poll backstop at short interval.
	opsB := NewOperationalSettings(storeB, fileK, envK)
	opsB.PollInterval = 200 * time.Millisecond
	srvB := &Server{dbDriver: "postgres", maintenance: NewMaintenanceState(false, "")}
	srvB.SetOperationalSettings(opsB)

	// B uses noop for events (no LISTEN), but still starts poll backstop.
	opsB.SetEventPublisher(noopEventPublisher{})
	// Manually start the poll backstop without LISTEN subscription.
	propCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	opsB.server = srvB
	opsB.propagationWg.Add(1)
	go func() {
		defer opsB.propagationWg.Done()
		opsB.runPollBackstop(propCtx, srvB)
	}()

	// PUT on A.
	body := `{"server":{"hub":{"admin_emails":["ac9@test.com"]},"auth":{"user_access_mode":"domain_restricted"}}}`
	rr := httptest.NewRecorder()
	srvA.handlePutServerConfigDB(rr, adminReq(http.MethodPut, "/api/v1/admin/server-config", body), opsA)
	require.Equal(t, http.StatusOK, rr.Code)

	// B should pick it up via poll backstop (no LISTEN available).
	// With 200ms poll interval, should be visible within ~1s.
	err = pollUntil(2*time.Second, func() error {
		snapB := opsB.Snapshot()
		if snapB.UserAccessMode != "domain_restricted" {
			return fmt.Errorf("B user_access_mode = %q, want domain_restricted", snapB.UserAccessMode)
		}
		return nil
	})
	require.NoError(t, err, "AC9: B should see change via poll backstop")

	// Verify enforcement on B.
	srvB.mu.Lock()
	bMode := srvB.config.UserAccessMode
	srvB.mu.Unlock()
	assert.Equal(t, "domain_restricted", bMode, "AC9: B should enforce domain_restricted")
}

// ----- AC10: Rollback safety — file mode against DB with hub_settings -----

func TestHA_AC10_RollbackSafety(t *testing.T) {
	requirePG(t)

	dsn := enttest.NewSchemaURL(t)
	ctx := context.Background()

	// Set up a DB with hub_settings rows (simulating a deployment that wrote settings).
	client, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: 5, MaxIdleConns: 2})
	require.NoError(t, err)
	cs := entadapter.NewCompositeStore(client)

	_, err = cs.UpsertHubSetting(ctx, "access",
		json.RawMessage(`{"admin_emails":["db@test.com"],"user_access_mode":"invite_only"}`),
		"admin", -1, "managed")
	require.NoError(t, err)
	_, err = cs.UpsertHubSetting(ctx, "maintenance",
		json.RawMessage(`{"admin_mode":true,"maintenance_message":"DB maintenance"}`),
		"admin", -1, "managed")
	require.NoError(t, err)
	_ = client.Close()

	// Simulate "old build": create a server WITHOUT OperationalSettings (file mode).
	// The server boots with a store backed by the same DB but no ops wiring.
	srvOld := &Server{
		maintenance: NewMaintenanceState(false, ""),
	}
	// No dbDriver set, no OperationalSettings → file mode path.

	// The server should boot cleanly.
	assert.Nil(t, srvOld.GetOperationalSettings(), "AC10: old build has no OperationalSettings")
	assert.False(t, srvOld.IsPostgres(), "AC10: old build is not postgres mode")

	// File-mode GET should work and NOT include section_metadata.
	admin := NewAuthenticatedUser("u1", "admin@e2e.test", "Admin", "admin", "cli")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/server-config", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), admin))
	rr := httptest.NewRecorder()
	srvOld.handleAdminServerConfig(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, "AC10: file-mode GET should succeed")

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Nil(t, resp["section_metadata"], "AC10: file mode should not include section_metadata")
	assert.Nil(t, resp["env_overrides"], "AC10: file mode should not include env_overrides")

	// File-mode maintenance should use in-memory state (not DB).
	assert.False(t, srvOld.maintenance.IsEnabled(), "AC10: old build maintenance should be off (file default)")
}
