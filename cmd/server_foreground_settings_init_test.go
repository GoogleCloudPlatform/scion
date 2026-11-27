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
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	scionplugin "github.com/GoogleCloudPlatform/scion/pkg/plugin"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// --- test doubles ---

// backfillFailStore wraps a real store.Store and overrides BackfillOrigin to
// return an error, simulating a persistent DB failure during settings init.
// All other Store/HubSettingStore methods delegate to the underlying store.
type backfillFailStore struct {
	store.Store
	calls atomic.Int32
}

func (s *backfillFailStore) BackfillOrigin(_ context.Context) error {
	s.calls.Add(1)
	return errors.New("simulated DB connection failure")
}

// backfillTransientFailStore wraps a real store.Store and fails BackfillOrigin
// for the first N calls, then delegates to the underlying store.
type backfillTransientFailStore struct {
	store.Store
	calls     atomic.Int32
	failCount int32
}

func (s *backfillTransientFailStore) BackfillOrigin(ctx context.Context) error {
	n := s.calls.Add(1)
	if n <= s.failCount {
		return errors.New("transient DB error")
	}
	return s.Store.(store.HubSettingStore).BackfillOrigin(ctx)
}

// --- tests ---

// TestInitOperationalSettings_FailClosed verifies that when the settings
// store is persistently broken, initOperationalSettingsWithRetry returns a
// non-nil error. initHubServer returns it and runServerStart returns before
// the hub starts serving (fail-closed via the error-return chain), so this
// error prevents the hub from accepting traffic — the fail-closed
// invariant for DB-persisted maintenance mode (admin_mode=true).
func TestInitOperationalSettings_FailClosed(t *testing.T) {
	ctx := context.Background()
	cfg := &config.GlobalConfig{}
	cfg.Database.Driver = "postgres"

	inner := newTestStore(t)

	// Use the real store for hub.New (it needs full store capabilities),
	// but pass the failing wrapper to initOperationalSettingsWithRetry
	// (which only uses HubSettingStore methods after the type assertion).
	srv, err := hub.New(hub.ServerConfig{}, inner)
	if err != nil {
		t.Fatalf("hub.New: %v", err)
	}

	fakeStore := &backfillFailStore{Store: inner}
	err = initOperationalSettingsWithRetry(ctx, cfg, srv, fakeStore, t.TempDir())
	if err == nil {
		t.Fatal("expected initOperationalSettingsWithRetry to return an error when settings load fails, but got nil — server would accept traffic without authoritative settings (maintenance-mode bypass)")
	}

	// Verify retries actually happened.
	calls := fakeStore.calls.Load()
	expectedMinCalls := int32(settingsInitMaxRetries + 1) // initial + retries
	if calls < expectedMinCalls {
		t.Errorf("BackfillOrigin called %d times, want at least %d (1 initial + %d retries)",
			calls, expectedMinCalls, settingsInitMaxRetries)
	}

	// Verify operational settings were NOT set on the server.
	if srv.GetOperationalSettings() != nil {
		t.Error("OperationalSettings should be nil when init fails — server must not have settings from a partial init")
	}

	t.Logf("initOperationalSettingsWithRetry correctly returned error after %d attempts: %v", calls, err)
}

// TestInitOperationalSettings_SuccessOnRetry verifies that a transient
// failure followed by recovery allows settings init to succeed.
func TestInitOperationalSettings_SuccessOnRetry(t *testing.T) {
	ctx := context.Background()
	cfg := &config.GlobalConfig{}
	cfg.Database.Driver = "sqlite3"

	inner := newTestStore(t)

	srv, err := hub.New(hub.ServerConfig{}, inner)
	if err != nil {
		t.Fatalf("hub.New: %v", err)
	}

	fakeStore := &backfillTransientFailStore{
		Store:     inner,
		failCount: 2, // fail first 2 calls, succeed on 3rd
	}

	err = initOperationalSettingsWithRetry(ctx, cfg, srv, fakeStore, t.TempDir())
	if err != nil {
		t.Fatalf("expected success after transient failures, got: %v", err)
	}

	// Verify the hub now has operational settings set.
	if srv.GetOperationalSettings() == nil {
		t.Error("OperationalSettings is nil after successful init — ApplySnapshot/SetOperationalSettings was not called")
	}
}

// TestInitOperationalSettings_ContextCancelled verifies that a cancelled
// context causes the retry loop to exit promptly.
func TestInitOperationalSettings_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	cfg := &config.GlobalConfig{}
	cfg.Database.Driver = "postgres"

	inner := newTestStore(t)
	srv, err := hub.New(hub.ServerConfig{}, inner)
	if err != nil {
		t.Fatalf("hub.New: %v", err)
	}

	fakeStore := &backfillFailStore{Store: inner}
	err = initOperationalSettingsWithRetry(ctx, cfg, srv, fakeStore, t.TempDir())
	if err == nil {
		t.Fatal("expected error when context is cancelled")
	}
}

// TestInitOperationalSettings_HubDefaultGCPIdentitySurvivesRestart is the
// boot-path regression test for the hub-default GCP identity on SQLite hubs.
// There, the admin PUT writes settings.yaml, and on each boot syncHubSettings
// seeds or re-syncs the agent_defaults hub_settings row from the bootstrap
// koanf. If opsettings.extractAgentDefaults omits a key, the row loses it and
// the Refresh -> Snapshot -> ApplySnapshot chain silently resets the hub
// default to block after a restart.
func TestInitOperationalSettings_HubDefaultGCPIdentitySurvivesRestart(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("HOME", home)
	globalDir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(globalDir, "settings.yaml")
	writeSettings := func(body string) {
		t.Helper()
		if err := os.WriteFile(settingsPath, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	cfg := &config.GlobalConfig{}
	cfg.Database.Driver = "sqlite3"
	st := newTestStore(t)

	boot := func() hub.Layer1Snapshot {
		t.Helper()
		srv, err := hub.New(hub.ServerConfig{}, st)
		if err != nil {
			t.Fatalf("hub.New: %v", err)
		}
		if err := initOperationalSettings(ctx, cfg, srv, st, globalDir); err != nil {
			t.Fatalf("initOperationalSettings: %v", err)
		}
		ops := srv.GetOperationalSettings()
		if ops == nil {
			t.Fatal("OperationalSettings is nil after init")
		}
		return ops.Snapshot()
	}

	// First boot: no hub default, so the agent_defaults row is seeded without it.
	writeSettings("schema_version: \"1\"\ndefault_timezone: UTC\n")
	if got := boot().DefaultGCPIdentityMode; got != "" {
		t.Fatalf("first boot: DefaultGCPIdentityMode = %q, want empty", got)
	}

	// Admin saves passthrough (file-mode PUT writes settings.yaml), then the
	// hub restarts. The seeded row must be re-synced with the new key.
	writeSettings("schema_version: \"1\"\ndefault_timezone: UTC\ndefault_gcp_identity_mode: passthrough\n")
	if got := boot().DefaultGCPIdentityMode; got != "passthrough" {
		t.Fatalf("after restart: DefaultGCPIdentityMode = %q, want passthrough", got)
	}

	// A second restart with unchanged settings keeps it (idempotent re-seed).
	if got := boot().DefaultGCPIdentityMode; got != "passthrough" {
		t.Fatalf("after second restart: DefaultGCPIdentityMode = %q, want passthrough", got)
	}

	// The assign pair also survives, SA ID included.
	writeSettings("schema_version: \"1\"\ndefault_gcp_identity_mode: assign\ndefault_gcp_identity_service_account_id: sa-hub-1\n")
	snap := boot()
	if snap.DefaultGCPIdentityMode != "assign" || snap.DefaultGCPIdentityServiceAccountID != "sa-hub-1" {
		t.Fatalf("after restart with assign: mode=%q sa=%q, want assign/sa-hub-1",
			snap.DefaultGCPIdentityMode, snap.DefaultGCPIdentityServiceAccountID)
	}
}

// TestInitOperationalSettings_SeedEnvReachesHubOnSQLite shows that
// SCION_SEED_* values reach the live hub through initOperationalSettings on
// a SQLite store (LoadBootstrapKoanf seed merge -> syncHubSettings' no-lock
// branch -> Refresh -> ApplySnapshot); see closed issue ptone/scion#1284.
// It calls initOperationalSettings directly, so it does not guard against a
// driver gate returning in its caller, initHubServer.
func TestInitOperationalSettings_SeedEnvReachesHubOnSQLite(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("HOME", home)
	globalDir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCION_SEED_SERVER_HUB_ADMINEMAILS", "seed-admin@example.com,seed-admin2@example.com")
	t.Setenv("SCION_SEED_SERVER_AUTH_DEFAULTUSERROLE", "viewer")

	cfg := &config.GlobalConfig{}
	st := newTestStore(t)
	srv, err := hub.New(hub.ServerConfig{}, st)
	if err != nil {
		t.Fatalf("hub.New: %v", err)
	}
	if err := initOperationalSettings(ctx, cfg, srv, st, globalDir); err != nil {
		t.Fatalf("initOperationalSettings: %v", err)
	}

	got := srv.AdminEmails()
	want := []string{"seed-admin@example.com", "seed-admin2@example.com"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("AdminEmails() = %v, want %v", got, want)
	}
	if role := srv.DefaultUserRole(); role != "viewer" {
		t.Errorf("DefaultUserRole() = %q, want viewer", role)
	}
}

// TestInitOperationalSettings_MaintenanceBreakGlassByMode: a workstation hub
// started in admin mode (SCION_SERVER_ADMIN_MODE=true / settings.yaml
// admin_mode) stays in maintenance over a DB maintenance row that says
// otherwise; a hosted hub follows the row (ptone/scion#1091 option C).
func TestInitOperationalSettings_MaintenanceBreakGlassByMode(t *testing.T) {
	for _, tc := range []struct {
		name        string
		workstation bool
		want        bool
	}{
		{"workstation", true, true},
		{"hosted", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			home := t.TempDir()
			t.Setenv("HOME", home)
			globalDir := filepath.Join(home, ".scion")
			if err := os.MkdirAll(globalDir, 0o755); err != nil {
				t.Fatal(err)
			}
			cfg := &config.GlobalConfig{}
			cfg.Database.Driver = "sqlite3"
			st := newTestStore(t)
			if _, err := st.UpsertHubSetting(ctx, "maintenance",
				[]byte(`{"admin_mode":false}`), "admin@example.com", -1, "managed"); err != nil {
				t.Fatal(err)
			}
			srv, err := hub.New(hub.ServerConfig{AdminMode: true, Workstation: tc.workstation}, st)
			if err != nil {
				t.Fatalf("hub.New: %v", err)
			}
			if err := initOperationalSettings(ctx, cfg, srv, st, globalDir); err != nil {
				t.Fatalf("initOperationalSettings: %v", err)
			}
			if got := srv.GetMaintenanceState().IsEnabled(); got != tc.want {
				t.Errorf("maintenance enabled after startup = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestColocatedBrokerRegisters pins the single condition shared by the early
// ExpectEmbeddedBroker call and co-located registration in startRuntimeBroker.
func TestColocatedBrokerRegisters(t *testing.T) {
	prevHub, prevSim := enableHub, simulateRemoteBroker
	t.Cleanup(func() { enableHub, simulateRemoteBroker = prevHub, prevSim })

	st := newTestStore(t)
	cases := []struct {
		name          string
		hub, sim, brk bool
		store         store.Store
		want          bool
	}{
		{"hub+broker co-located", true, false, true, st, true},
		{"broker disabled", true, false, false, st, false},
		{"hub disabled", false, false, true, st, false},
		{"simulated remote broker", true, true, true, st, false},
		{"no store", true, false, true, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			enableHub, simulateRemoteBroker = tc.hub, tc.sim
			cfg := &config.GlobalConfig{}
			cfg.RuntimeBroker.Enabled = tc.brk
			if got := colocatedBrokerRegisters(cfg, tc.store); got != tc.want {
				t.Errorf("colocatedBrokerRegisters = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestInitHubServer_CallsInitOperationalSettingsUnconditionally is a cheap
// structural guard for closed issue ptone/scion#1284, which was a
// postgres-only `if` around the operational-settings init in initHubServer.
// It parses server_foreground.go and fails if initHubServer no longer calls
// initOperationalSettingsWithRetry, or if that call sits inside any `if`
// body/else, `switch`/`select`, or `for`/`range` loop (the call may be the
// `if`'s own init statement, as in `if err := call(); err != nil`). It is
// not a semantic check: an early return or `goto` before the call, or
// moving the call into a helper or closure, is not seen.
func TestInitHubServer_CallsInitOperationalSettingsUnconditionally(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "server_foreground.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "initHubServer" {
			fn = fd
		}
	}
	if fn == nil {
		t.Fatal("initHubServer not found in server_foreground.go")
	}

	found := false
	var stack []ast.Node
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); !ok || id.Name != "initOperationalSettingsWithRetry" {
			return true
		}
		found = true
		for i, anc := range stack[:len(stack)-1] {
			child := stack[i+1]
			switch a := anc.(type) {
			case *ast.IfStmt:
				if child == a.Init || child == a.Cond {
					continue
				}
				var cond strings.Builder
				_ = printer.Fprint(&cond, fset, a.Cond)
				t.Errorf("initOperationalSettingsWithRetry is conditional on `if %s` (%s); settings init must run on every driver",
					cond.String(), fset.Position(a.Pos()))
			case *ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.SelectStmt:
				t.Errorf("initOperationalSettingsWithRetry is inside a switch/select (%s); settings init must run on every driver",
					fset.Position(anc.Pos()))
			case *ast.ForStmt, *ast.RangeStmt:
				t.Errorf("initOperationalSettingsWithRetry is inside a loop (%s); settings init must run once, on every driver",
					fset.Position(anc.Pos()))
			}
		}
		return true
	})
	if !found {
		t.Fatal("initHubServer no longer calls initOperationalSettingsWithRetry")
	}
}

// TestInitHubServer_InitsOperationalSettingsBeforeResourceBootstrap guards the
// startup ordering in initHubServer: operational settings (including stored
// experiment overrides such as hub.content_hash_v2) must be loaded before
// bundled/workstation resource bootstrap or storage sync runs, so disabling
// hub.content_hash_v2 and restarting converges built-ins back to v1.
func TestInitHubServer_InitsOperationalSettingsBeforeResourceBootstrap(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "server_foreground.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "initHubServer" {
			fn = fd
		}
	}
	if fn == nil {
		t.Fatal("initHubServer not found in server_foreground.go")
	}

	var opsPos token.Pos
	afterTargets := map[string]token.Pos{
		"BootstrapBundledResources":        token.NoPos,
		"bootstrapWorkstationResources":    token.NoPos,
		"SyncAllHarnessConfigsFromStorage": token.NoPos,
		"SyncAllTemplatesFromStorage":      token.NoPos,
	}

	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		var name string
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			name = fun.Name
		case *ast.SelectorExpr:
			name = fun.Sel.Name
		}
		if name == "initOperationalSettingsWithRetry" && !opsPos.IsValid() {
			opsPos = call.Pos()
		}
		if pos, want := afterTargets[name]; want && !pos.IsValid() {
			afterTargets[name] = call.Pos()
		}
		return true
	})

	if !opsPos.IsValid() {
		t.Fatal("initHubServer does not call initOperationalSettingsWithRetry")
	}
	for name, pos := range afterTargets {
		if !pos.IsValid() {
			t.Errorf("initHubServer does not call %s", name)
			continue
		}
		if opsPos >= pos {
			t.Errorf("initOperationalSettingsWithRetry (%s) must run before %s (%s)",
				fset.Position(opsPos), name, fset.Position(pos))
		}
	}
}

// TestInitOperationalSettings_TelemetryGCPProjectIDFromDB verifies that a
// DB-saved telemetry.cloud.gcp_project_id is loaded at startup by
// initOperationalSettings (which runs after hub.New and applies the snapshot
// to the hub server, updating the metrics dashboard target; ptone/scion#5693).
func TestInitOperationalSettings_TelemetryGCPProjectIDFromDB(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("HOME", home)
	globalDir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(globalDir, "settings.yaml")
	if err := os.WriteFile(settingsPath, []byte("schema_version: \"1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	st := newTestStore(t)
	hss, ok := st.(store.HubSettingStore)
	if !ok {
		t.Fatal("test store does not implement HubSettingStore")
	}
	if _, err := hss.UpsertHubSetting(ctx, "telemetry", json.RawMessage(`{"cloud":{"gcp_project_id":"proj-db-startup"}}`), "admin", -1, "managed"); err != nil {
		t.Fatalf("UpsertHubSetting: %v", err)
	}

	cfg := &config.GlobalConfig{}
	cfg.Database.Driver = "sqlite3"
	srv, err := hub.New(hub.ServerConfig{}, st)
	if err != nil {
		t.Fatalf("hub.New: %v", err)
	}
	defer func() { _ = srv.Shutdown(ctx) }()

	if err := initOperationalSettings(ctx, cfg, srv, st, globalDir); err != nil {
		t.Fatalf("initOperationalSettings: %v", err)
	}

	ops := srv.GetOperationalSettings()
	if ops == nil {
		t.Fatal("OperationalSettings is nil after init")
	}
	snap := ops.Snapshot()
	if snap.TelemetryConfig == nil || snap.TelemetryConfig.Cloud == nil || snap.TelemetryConfig.Cloud.GCPProjectID == nil {
		t.Fatalf("expected DB telemetry.cloud.gcp_project_id in snapshot, got %+v", snap.TelemetryConfig)
	}
	if got := *snap.TelemetryConfig.Cloud.GCPProjectID; got != "proj-db-startup" {
		t.Fatalf("snapshot telemetry.cloud.gcp_project_id = %q, want %q", got, "proj-db-startup")
	}
}

// TestInitOperationalSettings_AdminEmailsResolutionAndReconciliation verifies
// all three admin_emails resolution paths (SCION_SERVER_HUB_ADMINEMAILS env,
// settings.yaml server.hub.admin_emails, and SCION_SEED_SERVER_HUB_ADMINEMAILS)
// through the deferred-reconciliation hub.New -> initOperationalSettings flow
// (ptone/scion#9143).
func TestInitOperationalSettings_AdminEmailsResolutionAndReconciliation(t *testing.T) {
	tests := []struct {
		name              string
		serverEnv         string
		seedEnv           string
		seedDefaultRole   string
		settingsYAML      string
		promoteEmail      string
		wantDemotedRole   string
		wantDeprecationLn bool
	}{
		{
			name:              "SCION_SERVER_HUB_ADMINEMAILS env",
			serverEnv:         "env-admin@example.com",
			settingsYAML:      "schema_version: \"1\"\n",
			promoteEmail:      "env-admin@example.com",
			wantDemotedRole:   store.UserRoleMember,
			wantDeprecationLn: true,
		},
		{
			name:            "settings.yaml server.hub.admin_emails",
			settingsYAML:    "schema_version: \"1\"\nserver:\n  hub:\n    admin_emails:\n      - yaml-admin@example.com\n",
			promoteEmail:    "yaml-admin@example.com",
			wantDemotedRole: store.UserRoleMember,
		},
		{
			name:            "SCION_SEED_SERVER_HUB_ADMINEMAILS seed env",
			seedEnv:         "seed-admin@example.com",
			seedDefaultRole: store.UserRoleViewer,
			settingsYAML:    "schema_version: \"1\"\n",
			promoteEmail:    "seed-admin@example.com",
			wantDemotedRole: store.UserRoleViewer,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("SCION_SERVER_HUB_ADMINEMAILS", tc.serverEnv)
			t.Setenv("SCION_SEED_SERVER_HUB_ADMINEMAILS", tc.seedEnv)
			t.Setenv("SCION_SEED_SERVER_AUTH_DEFAULTUSERROLE", tc.seedDefaultRole)

			globalDir := filepath.Join(home, ".scion")
			if err := os.MkdirAll(globalDir, 0o755); err != nil {
				t.Fatal(err)
			}
			settingsPath := filepath.Join(globalDir, "settings.yaml")
			if err := os.WriteFile(settingsPath, []byte(tc.settingsYAML), 0o600); err != nil {
				t.Fatal(err)
			}

			st := newTestStore(t)
			if err := st.CreateUser(ctx, &store.User{
				ID:          uuid.NewString(),
				Email:       tc.promoteEmail,
				DisplayName: "To Promote",
				Role:        store.UserRoleMember,
				Status:      "active",
			}); err != nil {
				t.Fatalf("CreateUser(promote): %v", err)
			}
			if err := st.CreateUser(ctx, &store.User{
				ID:          uuid.NewString(),
				Email:       "former-admin@example.com",
				DisplayName: "Former Admin",
				Role:        store.UserRoleAdmin,
				Status:      "active",
			}); err != nil {
				t.Fatalf("CreateUser(demote): %v", err)
			}

			var logBuf bytes.Buffer
			origLogger := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
			t.Cleanup(func() { slog.SetDefault(origLogger) })

			srv, err := hub.New(hub.ServerConfig{DeferAdminReconciliation: true}, st)
			if err != nil {
				t.Fatalf("hub.New: %v", err)
			}
			defer func() { _ = srv.Shutdown(ctx) }()

			if srv.GetDemotionSafe().Load() {
				t.Fatal("expected demotionSafe to remain false before initOperationalSettings")
			}

			cfg := &config.GlobalConfig{}
			cfg.Database.Driver = "sqlite3"
			if err := initOperationalSettings(ctx, cfg, srv, st, globalDir); err != nil {
				t.Fatalf("initOperationalSettings: %v", err)
			}

			gotEmails := srv.AdminEmails()
			if len(gotEmails) != 1 || gotEmails[0] != tc.promoteEmail {
				t.Fatalf("AdminEmails() = %v, want [%s]", gotEmails, tc.promoteEmail)
			}
			if !srv.GetDemotionSafe().Load() {
				t.Fatal("expected demotionSafe == true after initOperationalSettings")
			}

			promoted, err := st.GetUserByEmail(ctx, tc.promoteEmail)
			if err != nil {
				t.Fatalf("GetUserByEmail(promoted): %v", err)
			}
			if promoted.Role != store.UserRoleAdmin {
				t.Errorf("promoted user role = %q, want %q", promoted.Role, store.UserRoleAdmin)
			}

			demoted, err := st.GetUserByEmail(ctx, "former-admin@example.com")
			if err != nil {
				t.Fatalf("GetUserByEmail(demoted): %v", err)
			}
			if demoted.Role != tc.wantDemotedRole {
				t.Errorf("demoted user role = %q, want %q", demoted.Role, tc.wantDemotedRole)
			}

			if tc.wantDeprecationLn && !strings.Contains(logBuf.String(), "SCION_SERVER_* is deprecated") {
				t.Errorf("expected deprecation warning in logs, got:\n%s", logBuf.String())
			}
		})
	}
}

// TestInitOperationalSettings_EmptyAdminEmailsWarningWhenManagedSupersedesSeed
// verifies that when hub_settings["access"] is managed with empty admin_emails
// and SCION_SEED_SERVER_HUB_ADMINEMAILS is set while SCION_SERVER_HUB_ADMINEMAILS
// is unset, the effective runtime list stays empty, demotionSafe stays false
// (lockout guard), and a clear warning is logged without leaking the email value.
func TestInitOperationalSettings_EmptyAdminEmailsWarningWhenManagedSupersedesSeed(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SCION_SERVER_HUB_ADMINEMAILS", "")
	t.Setenv("SCION_SEED_SERVER_HUB_ADMINEMAILS", "seed-admin@example.com")

	globalDir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte("schema_version: \"1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	st := newTestStore(t)
	hss, ok := st.(store.HubSettingStore)
	if !ok {
		t.Fatal("test store does not implement HubSettingStore")
	}
	if _, err := hss.UpsertHubSetting(ctx, "access", json.RawMessage(`{"admin_emails":[]}`), "admin@example.com", -1, "managed"); err != nil {
		t.Fatalf("UpsertHubSetting(access): %v", err)
	}
	if err := st.CreateUser(ctx, &store.User{
		ID:          uuid.NewString(),
		Email:       "existing-admin@example.com",
		DisplayName: "Existing Admin",
		Role:        store.UserRoleAdmin,
		Status:      "active",
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	var logBuf bytes.Buffer
	origLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(origLogger) })

	srv, err := hub.New(hub.ServerConfig{DeferAdminReconciliation: true}, st)
	if err != nil {
		t.Fatalf("hub.New: %v", err)
	}
	defer func() { _ = srv.Shutdown(ctx) }()

	cfg := &config.GlobalConfig{}
	cfg.Database.Driver = "sqlite3"
	if err := initOperationalSettings(ctx, cfg, srv, st, globalDir); err != nil {
		t.Fatalf("initOperationalSettings: %v", err)
	}

	if got := srv.AdminEmails(); len(got) != 0 {
		t.Fatalf("AdminEmails() = %v, want empty", got)
	}
	if srv.GetDemotionSafe().Load() {
		t.Fatal("expected demotionSafe == false when effective admin_emails is empty")
	}

	existing, err := st.GetUserByEmail(ctx, "existing-admin@example.com")
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	if existing.Role != store.UserRoleAdmin {
		t.Errorf("existing admin role = %q, want %q (lockout guard must prevent demotion)", existing.Role, store.UserRoleAdmin)
	}

	logs := logBuf.String()
	if !strings.Contains(logs, "Effective runtime admin_emails list is empty") {
		t.Errorf("expected empty runtime admin_emails warning in logs, got:\n%s", logs)
	}
	if !strings.Contains(logs, "SCION_SEED_SERVER_HUB_ADMINEMAILS") {
		t.Errorf("expected SCION_SEED_SERVER_HUB_ADMINEMAILS in warning logs, got:\n%s", logs)
	}
	if strings.Contains(logs, "seed-admin@example.com") {
		t.Errorf("warning log must not leak email address value, got:\n%s", logs)
	}
}

// TestInitOperationalSettings_DevAuthPreservesSuperAdminBindingAcrossBoots
// is a two-boot regression test for ptone/scion#9143 R1: on a dev-auth hub
// with a non-empty admin_emails matching a real user (so demotionSafe=true),
// deferred startup reconciliation via initOperationalSettings must preserve
// the dev user's system super-admin binding on both the first and second boot
// of the same store.
func TestInitOperationalSettings_DevAuthPreservesSuperAdminBindingAcrossBoots(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SCION_SERVER_HUB_ADMINEMAILS", "")
	t.Setenv("SCION_SEED_SERVER_HUB_ADMINEMAILS", "")

	globalDir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	settingsYAML := "schema_version: \"1\"\nserver:\n  hub:\n    admin_emails:\n      - real@example.com\n"
	if err := os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(settingsYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	st := newTestStore(t)
	if err := st.CreateUser(ctx, &store.User{
		ID:          uuid.NewString(),
		Email:       "real@example.com",
		DisplayName: "Real Admin",
		Role:        store.UserRoleMember,
		Status:      "active",
	}); err != nil {
		t.Fatalf("CreateUser(real): %v", err)
	}

	cfg := &config.GlobalConfig{}
	cfg.Database.Driver = "sqlite3"

	for boot := 1; boot <= 2; boot++ {
		srv, err := hub.New(hub.ServerConfig{
			DeferAdminReconciliation: true,
			DevAuthToken:             "scion_dev_testtoken",
		}, st)
		if err != nil {
			t.Fatalf("boot %d: hub.New: %v", boot, err)
		}

		if err := initOperationalSettings(ctx, cfg, srv, st, globalDir); err != nil {
			_ = srv.Shutdown(ctx)
			t.Fatalf("boot %d: initOperationalSettings: %v", boot, err)
		}

		if !srv.GetDemotionSafe().Load() {
			_ = srv.Shutdown(ctx)
			t.Fatalf("boot %d: expected demotionSafe == true", boot)
		}

		superAdminRD, err := st.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
		if err != nil {
			_ = srv.Shutdown(ctx)
			t.Fatalf("boot %d: GetRoleDefinitionByName(super-admin): %v", boot, err)
		}

		bindings, err := st.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, hub.DevUserID)
		if err != nil {
			_ = srv.Shutdown(ctx)
			t.Fatalf("boot %d: ListRoleBindingsForPrincipal(dev): %v", boot, err)
		}
		var superAdminCount int
		for _, b := range bindings {
			if b.ScopeType == store.RoleScopeSystem && b.RoleDefinitionID == superAdminRD.ID {
				superAdminCount++
			}
		}
		if superAdminCount != 1 {
			_ = srv.Shutdown(ctx)
			t.Fatalf("boot %d: dev user super-admin bindings = %d, want 1", boot, superAdminCount)
		}

		if err := srv.Shutdown(ctx); err != nil {
			t.Fatalf("boot %d: Shutdown: %v", boot, err)
		}
	}
}

// TestInitHubServer_DefersAdminReconciliationForHubSettingStore verifies that
// initHubServer sets DeferAdminReconciliation=true when the store implements
// store.HubSettingStore, so hub.New does not run a transient reconcile against
// a stale CLI/settings.yaml adminEmailList before initOperationalSettings loads
// the DB-managed access row.
func TestInitHubServer_DefersAdminReconciliationForHubSettingStore(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SCION_SERVER_HUB_ADMINEMAILS", "")
	t.Setenv("SCION_SEED_SERVER_HUB_ADMINEMAILS", "")

	globalDir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte("schema_version: \"1\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	st := newTestStore(t)
	hss, ok := st.(store.HubSettingStore)
	if !ok {
		t.Fatal("test store does not implement HubSettingStore")
	}
	if _, err := hss.UpsertHubSetting(ctx, "access", json.RawMessage(`{"admin_emails":["db-admin@example.com"]}`), "admin@example.com", -1, "managed"); err != nil {
		t.Fatalf("UpsertHubSetting(access): %v", err)
	}

	// Seed built-in role definitions once so we can attach a known super-admin
	// binding to db-admin@example.com before calling initHubServer.
	seedSrv, err := hub.New(hub.ServerConfig{DeferAdminReconciliation: true}, st)
	if err != nil {
		t.Fatalf("seed hub.New: %v", err)
	}
	if err := seedSrv.Shutdown(ctx); err != nil {
		t.Fatalf("seed Shutdown: %v", err)
	}

	dbAdmin := &store.User{
		ID:          uuid.NewString(),
		Email:       "db-admin@example.com",
		DisplayName: "DB Admin",
		Role:        store.UserRoleAdmin,
		Status:      "active",
	}
	staleAdmin := &store.User{
		ID:          uuid.NewString(),
		Email:       "stale-admin@example.com",
		DisplayName: "Stale Admin",
		Role:        store.UserRoleMember,
		Status:      "active",
	}
	if err := st.CreateUser(ctx, dbAdmin); err != nil {
		t.Fatalf("CreateUser(dbAdmin): %v", err)
	}
	if err := st.CreateUser(ctx, staleAdmin); err != nil {
		t.Fatalf("CreateUser(staleAdmin): %v", err)
	}

	superAdminRD, err := st.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	if err != nil {
		t.Fatalf("GetRoleDefinitionByName(super-admin): %v", err)
	}
	origBinding, err := st.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: superAdminRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      dbAdmin.ID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	if err != nil {
		t.Fatalf("CreateRoleBinding(dbAdmin): %v", err)
	}

	cfg := &config.GlobalConfig{}
	cfg.Database.Driver = "sqlite3"

	// Pass a stale adminEmailList matching staleAdmin. If initHubServer did not
	// set DeferAdminReconciliation=true, hub.New would transiently demote
	// dbAdmin and delete origBinding before initOperationalSettings re-created
	// a new binding with a different ID.
	srv, err := initHubServer(ctx, cfg, st, nil, hub.HubEndpoints{}, "", []string{"stale-admin@example.com"}, false, "", nil, nil, nil, globalDir, scionplugin.NewManager(slog.Default()), nil)
	if err != nil {
		t.Fatalf("initHubServer: %v", err)
	}
	defer func() { _ = srv.Shutdown(ctx) }()

	bindings, err := st.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, dbAdmin.ID)
	if err != nil {
		t.Fatalf("ListRoleBindingsForPrincipal(dbAdmin): %v", err)
	}
	foundOrig := false
	for _, b := range bindings {
		if b.ID == origBinding.ID {
			foundOrig = true
			break
		}
	}
	if !foundOrig {
		t.Fatalf("db-admin@example.com lost its original super-admin binding %s during initHubServer; DeferAdminReconciliation was not honored", origBinding.ID)
	}

	gotStale, err := st.GetUserByEmail(ctx, staleAdmin.Email)
	if err != nil {
		t.Fatalf("GetUserByEmail(staleAdmin): %v", err)
	}
	if gotStale.Role != store.UserRoleMember {
		t.Errorf("stale-admin role = %q, want %q", gotStale.Role, store.UserRoleMember)
	}
}
