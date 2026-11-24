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

package hub

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/maintenance"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

type autoUpdateJobProbe struct {
	now          time.Time
	running      string
	checks       int
	gotOpts      maintenance.ReleaseCheckOptions
	checkResult  *maintenance.ReleaseUpdateCheckResult
	checkErr     error
	resolved     int
	executor     MaintenanceExecutor
	resolveError error
}

func (p *autoUpdateJobProbe) deps() autoUpdateJobDeps {
	return autoUpdateJobDeps{
		now:     func() time.Time { return p.now },
		running: p.running,
		check: func(_ context.Context, o maintenance.ReleaseCheckOptions) (*maintenance.ReleaseUpdateCheckResult, error) {
			p.checks++
			p.gotOpts = o
			return p.checkResult, p.checkErr
		},
		updateExecutor: func() (MaintenanceExecutor, error) {
			p.resolved++
			return p.executor, p.resolveError
		},
	}
}

func newAutoUpdateJobTestServer(t *testing.T, mc MaintenanceConfig) (*Server, store.Store) {
	t.Helper()
	srv, s := newTestServerWithStore(t)
	srv.config.MaintenanceConfig = mc
	srv.config.HubID = "" // deterministic 0-minute offset unless test overrides
	return srv, s
}

func enableAutoUpdateSetting(t *testing.T, srv *Server, hour int) {
	t.Helper()
	err := srv.writeAutoUpdateSetting(context.Background(), AutoUpdateSetting{
		Enabled:        true,
		WindowStartUTC: hour,
		SetBy:          "admin@example.com",
		SetAt:          "2026-11-24T00:00:00Z",
	}, "admin-id", 0)
	require.NoError(t, err)
}

func waitForRunTerminal(t *testing.T, s store.Store, runID string) *store.MaintenanceOperationRun {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		run, err := s.GetMaintenanceRun(ctx, runID)
		require.NoError(t, err)
		if run.CompletedAt != nil {
			return run
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("maintenance run %s did not reach terminal state before deadline", runID)
	return nil
}

func TestAutoUpdateWindowOffsetAndWindowFor(t *testing.T) {
	t.Run("empty hubID yields zero offset", func(t *testing.T) {
		assert.Equal(t, time.Duration(0), autoUpdateWindowOffset(""))
	})

	t.Run("non-empty hubID yields stable offset in [0, 60m)", func(t *testing.T) {
		off1 := autoUpdateWindowOffset("hub-alpha-123")
		off2 := autoUpdateWindowOffset("hub-alpha-123")
		assert.Equal(t, off1, off2)
		assert.GreaterOrEqual(t, off1, time.Duration(0))
		assert.Less(t, off1, 60*time.Minute)
	})

	t.Run("window boundaries with offset", func(t *testing.T) {
		offset := 17 * time.Minute
		in, day := autoUpdateWindowFor(time.Date(2026, 11, 24, 4, 16, 59, 0, time.UTC), 4, offset)
		assert.False(t, in)
		assert.Empty(t, day)

		in, day = autoUpdateWindowFor(time.Date(2026, 11, 24, 4, 17, 0, 0, time.UTC), 4, offset)
		assert.True(t, in)
		assert.Equal(t, "2026-11-24", day)

		in, day = autoUpdateWindowFor(time.Date(2026, 11, 24, 5, 59, 59, 0, time.UTC), 4, offset)
		assert.True(t, in)
		assert.Equal(t, "2026-11-24", day)

		in, day = autoUpdateWindowFor(time.Date(2026, 11, 24, 6, 0, 0, 0, time.UTC), 4, offset)
		assert.False(t, in)
		assert.Empty(t, day)
	})

	t.Run("window crossing midnight uses window start date", func(t *testing.T) {
		in, day := autoUpdateWindowFor(time.Date(2026, 11, 24, 23, 30, 0, 0, time.UTC), 23, 15*time.Minute)
		assert.True(t, in)
		assert.Equal(t, "2026-11-24", day)

		in, day = autoUpdateWindowFor(time.Date(2026, 11, 25, 0, 45, 0, 0, time.UTC), 23, 15*time.Minute)
		assert.True(t, in)
		assert.Equal(t, "2026-11-24", day)

		in, day = autoUpdateWindowFor(time.Date(2026, 11, 25, 1, 0, 0, 0, time.UTC), 23, 15*time.Minute)
		assert.False(t, in)
		assert.Empty(t, day)
	})
}

func TestRunReleaseAutoUpdate_DisabledAndOutsideWindow(t *testing.T) {
	ctx := context.Background()
	baseMC := MaintenanceConfig{
		DeploymentTier: "binary",
		ReleaseChannel: "nightly",
		UpdatePolicy:   "notify",
		Installer:      newTestInstaller(maintenance.StrategySymlink),
	}

	t.Run("not enabled is a no-op", func(t *testing.T) {
		srv, s := newAutoUpdateJobTestServer(t, baseMC)
		p := &autoUpdateJobProbe{
			now:         time.Date(2026, 11, 24, 4, 30, 0, 0, time.UTC),
			running:     "nightly-20261120",
			checkResult: releaseCheckResult(true),
			executor:    newParamRecordingExecutor(),
		}
		srv.runReleaseAutoUpdate(ctx, p.deps())
		assert.Equal(t, 0, p.checks)
		assert.Equal(t, 0, p.resolved)
		_, err := s.GetHubSetting(ctx, HubSettingSectionAutoUpdateState)
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("outside window is a no-op", func(t *testing.T) {
		srv, s := newAutoUpdateJobTestServer(t, baseMC)
		enableAutoUpdateSetting(t, srv, 4)

		for _, ts := range []time.Time{
			time.Date(2026, 11, 24, 3, 59, 0, 0, time.UTC),
			time.Date(2026, 11, 24, 6, 0, 0, 0, time.UTC),
			time.Date(2026, 11, 24, 15, 0, 0, 0, time.UTC),
		} {
			p := &autoUpdateJobProbe{
				now:         ts,
				running:     "nightly-20261120",
				checkResult: releaseCheckResult(true),
				executor:    newParamRecordingExecutor(),
			}
			srv.runReleaseAutoUpdate(ctx, p.deps())
			assert.Equal(t, 0, p.checks, "time=%s", ts)
		}
		_, err := s.GetHubSetting(ctx, HubSettingSectionAutoUpdateState)
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("per-hub offset delays start into window", func(t *testing.T) {
		srv, _ := newAutoUpdateJobTestServer(t, baseMC)
		srv.config.HubID = "hub-with-offset"
		offset := autoUpdateWindowOffset(srv.config.HubID)
		if offset == 0 {
			srv.config.HubID = "hub-with-nonzero-offset-2"
			offset = autoUpdateWindowOffset(srv.config.HubID)
		}
		require.Greater(t, offset, time.Duration(0))
		enableAutoUpdateSetting(t, srv, 4)

		// One minute before hub offset: no-op.
		p := &autoUpdateJobProbe{
			now:         time.Date(2026, 11, 24, 4, 0, 0, 0, time.UTC).Add(offset - time.Minute),
			running:     "nightly-20261120",
			checkResult: releaseCheckResult(false),
		}
		srv.runReleaseAutoUpdate(ctx, p.deps())
		assert.Equal(t, 0, p.checks)

		// At hub offset: runs.
		p.now = time.Date(2026, 11, 24, 4, 0, 0, 0, time.UTC).Add(offset)
		srv.runReleaseAutoUpdate(ctx, p.deps())
		assert.Equal(t, 1, p.checks)
	})
}

func TestRunReleaseAutoUpdate_OncePerDayAndRestart(t *testing.T) {
	ctx := context.Background()
	mc := MaintenanceConfig{
		DeploymentTier: "binary",
		ReleaseChannel: "nightly",
		UpdatePolicy:   "notify",
		Installer:      newTestInstaller(maintenance.StrategySymlink),
	}
	srv, s := newAutoUpdateJobTestServer(t, mc)
	enableAutoUpdateSetting(t, srv, 4)

	exec := newParamRecordingExecutor()
	p := &autoUpdateJobProbe{
		now:     time.Date(2026, 11, 24, 4, 10, 0, 0, time.UTC),
		running: "nightly-20261120",
		checkResult: &maintenance.ReleaseUpdateCheckResult{
			Tier:            "binary",
			UpdateAvailable: true,
			CurrentVersion:  "nightly-20261120",
			LatestVersion:   "nightly-20261124",
			Channel:         "nightly",
			Class:           "upgrade",
		},
		executor: exec,
	}

	// First tick in window: checks with Refresh=true and starts explicit-tag run.
	srv.runReleaseAutoUpdate(ctx, p.deps())
	require.Equal(t, 1, p.checks)
	assert.True(t, p.gotOpts.Refresh, "auto-update job must pass Refresh: true")
	assert.Equal(t, "nightly", p.gotOpts.Channel)

	select {
	case <-exec.ran:
	case <-time.After(5 * time.Second):
		t.Fatal("auto-update executor never ran")
	}
	exec.mu.Lock()
	params := exec.params
	exec.mu.Unlock()
	assert.Equal(t, "nightly-20261124", params[maintenance.ParamTargetVersion], "must pass explicit tag, not latest:<channel>")
	assert.Equal(t, "nightly", params[maintenance.ParamChannel])
	assert.Empty(t, params[maintenance.ParamConfirmDowngrade], "confirm_downgrade must never be passed")

	runs, err := s.ListMaintenanceRuns(ctx, "update-binary", 5)
	require.NoError(t, err)
	require.Len(t, runs, 1)
	assert.Equal(t, autoUpdateStartedBy, runs[0].StartedBy)

	state, err := srv.readAutoUpdateState(ctx)
	require.NoError(t, err)
	assert.Equal(t, "2026-11-24", state.LastAttemptDay)
	assert.Equal(t, "2026-11-24T04:10:00Z", state.LastCheckAt)
	assert.Equal(t, runs[0].ID, state.LastRunID)
	assert.Equal(t, "nightly-20261124", state.LastTarget)
	assert.Equal(t, AutoUpdateOutcomeStarted, state.LastOutcome)

	// Second tick same day (still in window): no-op.
	p.now = time.Date(2026, 11, 24, 4, 20, 0, 0, time.UTC)
	srv.runReleaseAutoUpdate(ctx, p.deps())
	assert.Equal(t, 1, p.checks)

	// Simulated restart: fresh Server over the same store on the same day,
	// after the run completed and hub booted on the new version.
	run := waitForRunTerminal(t, s, runs[0].ID)
	run.Status = store.MaintenanceStatusCompleted
	require.NoError(t, s.UpdateMaintenanceRun(ctx, run))

	restarted := &Server{
		store:          s,
		maintenanceLog: logging.Subsystem("hub.maintenance"),
		config: ServerConfig{
			MaintenanceConfig: mc,
		},
	}
	p2 := &autoUpdateJobProbe{
		now:     time.Date(2026, 11, 24, 4, 30, 0, 0, time.UTC),
		running: "nightly-20261124",
		checkResult: &maintenance.ReleaseUpdateCheckResult{
			Tier:            "binary",
			UpdateAvailable: true,
			CurrentVersion:  "nightly-20261124",
			LatestVersion:   "nightly-20261125",
			Channel:         "nightly",
			Class:           "upgrade",
		},
		executor: newParamRecordingExecutor(),
	}
	restarted.runReleaseAutoUpdate(ctx, p2.deps())
	assert.Equal(t, 0, p2.checks, "restarted server on same day must not check or run again")

	// Reconciliation still ran and recorded verified.
	state, err = restarted.readAutoUpdateState(ctx)
	require.NoError(t, err)
	assert.Equal(t, AutoUpdateOutcomeVerified, state.LastOutcome)
	assert.Empty(t, state.SuppressedTag)
}

func TestRunReleaseAutoUpdate_IneligibleSkips(t *testing.T) {
	ctx := context.Background()
	inWindow := time.Date(2026, 11, 24, 4, 15, 0, 0, time.UTC)

	t.Run("pinned", func(t *testing.T) {
		srv, s := newAutoUpdateJobTestServer(t, MaintenanceConfig{
			DeploymentTier: "binary", ReleaseChannel: "nightly", UpdatePolicy: "notify",
			Installer: newTestInstaller(maintenance.StrategySymlink),
		})
		enableAutoUpdateSetting(t, srv, 4)
		putReleaseSelection(t, s, ReleaseSelection{Mode: UpdateModeRelease, Channel: "nightly", Pin: "nightly-20261120"})
		p := &autoUpdateJobProbe{now: inWindow, running: "nightly-20261120", checkResult: releaseCheckResult(true), executor: newParamRecordingExecutor()}
		srv.runReleaseAutoUpdate(ctx, p.deps())
		assert.Equal(t, 0, p.checks)
	})

	t.Run("git mode", func(t *testing.T) {
		srv, _ := newAutoUpdateJobTestServer(t, MaintenanceConfig{
			DeploymentTier: "source", ReleaseChannel: "nightly", UpdatePolicy: "notify",
			Installer: newTestInstaller(maintenance.StrategySymlink),
		})
		enableAutoUpdateSetting(t, srv, 4)
		p := &autoUpdateJobProbe{now: inWindow, running: "nightly-20261120", checkResult: releaseCheckResult(true), executor: newParamRecordingExecutor()}
		srv.runReleaseAutoUpdate(ctx, p.deps())
		assert.Equal(t, 0, p.checks)
	})

	t.Run("unsupported installer", func(t *testing.T) {
		srv, _ := newAutoUpdateJobTestServer(t, MaintenanceConfig{
			DeploymentTier: "binary", ReleaseChannel: "nightly", UpdatePolicy: "notify",
			Installer: newTestInstaller(maintenance.StrategyUnsupported),
		})
		enableAutoUpdateSetting(t, srv, 4)
		p := &autoUpdateJobProbe{now: inWindow, running: "nightly-20261120", checkResult: releaseCheckResult(true), executor: newParamRecordingExecutor()}
		srv.runReleaseAutoUpdate(ctx, p.deps())
		assert.Equal(t, 0, p.checks)
	})

	t.Run("no channel", func(t *testing.T) {
		srv, _ := newAutoUpdateJobTestServer(t, MaintenanceConfig{
			DeploymentTier: "binary", UpdatePolicy: "notify",
			Installer: newTestInstaller(maintenance.StrategySymlink),
		})
		enableAutoUpdateSetting(t, srv, 4)
		p := &autoUpdateJobProbe{now: inWindow, running: "", checkResult: releaseCheckResult(true), executor: newParamRecordingExecutor()}
		srv.runReleaseAutoUpdate(ctx, p.deps())
		assert.Equal(t, 0, p.checks)
	})

	t.Run("config locked", func(t *testing.T) {
		srv, _ := newAutoUpdateJobTestServer(t, MaintenanceConfig{
			DeploymentTier: "binary", ReleaseChannel: "nightly",
			UpdatePolicy: "disabled", UpdatePolicyExplicit: true,
			Installer: newTestInstaller(maintenance.StrategySymlink),
		})
		enableAutoUpdateSetting(t, srv, 4)
		p := &autoUpdateJobProbe{now: inWindow, running: "nightly-20261120", checkResult: releaseCheckResult(true), executor: newParamRecordingExecutor()}
		srv.runReleaseAutoUpdate(ctx, p.deps())
		assert.Equal(t, 0, p.checks)
	})

	t.Run("maintenance mode skips and records skipped:maintenance_mode", func(t *testing.T) {
		srv, _ := newAutoUpdateJobTestServer(t, MaintenanceConfig{
			DeploymentTier: "binary", ReleaseChannel: "nightly", UpdatePolicy: "notify",
			Installer: newTestInstaller(maintenance.StrategySymlink),
		})
		srv.maintenance = NewMaintenanceState(true, "planned work")
		enableAutoUpdateSetting(t, srv, 4)
		p := &autoUpdateJobProbe{now: inWindow, running: "nightly-20261120", checkResult: releaseCheckResult(true), executor: newParamRecordingExecutor()}
		srv.runReleaseAutoUpdate(ctx, p.deps())
		assert.Equal(t, 0, p.checks)

		state, err := srv.readAutoUpdateState(ctx)
		require.NoError(t, err)
		assert.Equal(t, AutoUpdateOutcomeSkippedMaintenanceMode, state.LastOutcome)
		assert.Equal(t, "2026-11-24T04:15:00Z", state.LastCheckAt)
		assert.Empty(t, state.LastAttemptDay)
	})
}

func TestRunReleaseAutoUpdate_CheckErrorNoRetrySameDay(t *testing.T) {
	ctx := context.Background()
	srv, _ := newAutoUpdateJobTestServer(t, MaintenanceConfig{
		DeploymentTier: "binary", ReleaseChannel: "nightly", UpdatePolicy: "auto",
		Installer: newTestInstaller(maintenance.StrategySymlink),
	})

	p := &autoUpdateJobProbe{
		now:      time.Date(2026, 11, 24, 4, 10, 0, 0, time.UTC),
		running:  "nightly-20261120",
		checkErr: errors.New("github 503"),
		executor: newParamRecordingExecutor(),
	}
	srv.runReleaseAutoUpdate(ctx, p.deps())
	assert.Equal(t, 1, p.checks)
	state, err := srv.readAutoUpdateState(ctx)
	require.NoError(t, err)
	assert.Equal(t, "2026-11-24", state.LastAttemptDay)
	assert.Equal(t, AutoUpdateOutcomeSkippedCheckFailed, state.LastOutcome)

	// Later tick same day inside window: does NOT retry.
	p.now = time.Date(2026, 11, 24, 4, 40, 0, 0, time.UTC)
	srv.runReleaseAutoUpdate(ctx, p.deps())
	assert.Equal(t, 1, p.checks)

	// Next day inside window: retries and records no_update.
	p.now = time.Date(2026, 11, 25, 4, 10, 0, 0, time.UTC)
	p.checkErr = nil
	p.checkResult = releaseCheckResult(false)
	srv.runReleaseAutoUpdate(ctx, p.deps())
	assert.Equal(t, 2, p.checks)
	state, err = srv.readAutoUpdateState(ctx)
	require.NoError(t, err)
	assert.Equal(t, "2026-11-25", state.LastAttemptDay)
	assert.Equal(t, AutoUpdateOutcomeNoUpdate, state.LastOutcome)
}

func TestRunReleaseAutoUpdate_SuppressionOnFailureRollbackAndClearOnNewer(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name        string
		rolledBack  bool
		wantOutcome string
	}{
		{name: "failed run suppresses tag", rolledBack: false, wantOutcome: AutoUpdateOutcomeFailed},
		{name: "rolled back run suppresses tag", rolledBack: true, wantOutcome: AutoUpdateOutcomeRolledBack},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, s := newAutoUpdateJobTestServer(t, MaintenanceConfig{
				DeploymentTier: "binary", ReleaseChannel: "nightly", UpdatePolicy: "notify",
				Installer: newTestInstaller(maintenance.StrategySymlink),
			})
			enableAutoUpdateSetting(t, srv, 4)

			// Day 1: auto-update starts nightly-20261124.
			exec1 := newParamRecordingExecutor()
			p := &autoUpdateJobProbe{
				now:     time.Date(2026, 11, 24, 4, 10, 0, 0, time.UTC),
				running: "nightly-20261120",
				checkResult: &maintenance.ReleaseUpdateCheckResult{
					Tier: "binary", UpdateAvailable: true,
					CurrentVersion: "nightly-20261120", LatestVersion: "nightly-20261124",
					Channel: "nightly", Class: "upgrade",
				},
				executor: exec1,
			}
			srv.runReleaseAutoUpdate(ctx, p.deps())
			select {
			case <-exec1.ran:
			case <-time.After(5 * time.Second):
				t.Fatal("day 1 executor never ran")
			}

			state, err := srv.readAutoUpdateState(ctx)
			require.NoError(t, err)
			require.NotEmpty(t, state.LastRunID)

			// Wait for the runner goroutine to finish writing its terminal
			// status before overwriting the run to failed or rolled_back.
			run := waitForRunTerminal(t, s, state.LastRunID)
			run.Status = store.MaintenanceStatusFailed
			if tc.rolledBack {
				resJSON, _ := json.Marshal(map[string]any{"verification": LastUpdateRolledBack})
				run.Result = string(resJSON)
				require.NoError(t, srv.writeHubSettingJSON(ctx, HubSettingSectionLastUpdate, LastUpdate{
					RunID:  run.ID,
					From:   "nightly-20261120",
					To:     "nightly-20261124",
					Status: LastUpdateRolledBack,
					At:     "2026-11-24T04:12:00Z",
				}))
			}
			require.NoError(t, s.UpdateMaintenanceRun(ctx, run))

			// Tick outside window on Day 1: reconciles exact outcome and sets SuppressedTag.
			p.now = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
			srv.runReleaseAutoUpdate(ctx, p.deps())
			state, err = srv.readAutoUpdateState(ctx)
			require.NoError(t, err)
			assert.Equal(t, tc.wantOutcome, state.LastOutcome)
			assert.Equal(t, "nightly-20261124", state.SuppressedTag)

			// Day 2: same tag nightly-20261124 is suppressed and skipped.
			exec2 := newParamRecordingExecutor()
			p.now = time.Date(2026, 11, 25, 4, 10, 0, 0, time.UTC)
			p.executor = exec2
			srv.runReleaseAutoUpdate(ctx, p.deps())

			state, err = srv.readAutoUpdateState(ctx)
			require.NoError(t, err)
			assert.Equal(t, "nightly-20261124", state.SuppressedTag)
			assert.Equal(t, AutoUpdateOutcomeSkippedSuppressed, state.LastOutcome)
			assert.Equal(t, "2026-11-25", state.LastAttemptDay)
			runs, err := s.ListMaintenanceRuns(ctx, "update-binary", 10)
			require.NoError(t, err)
			assert.Len(t, runs, 1, "suppressed tag must not start a new run")

			// Day 3: newer tag nightly-20261126 clears suppression and applies.
			exec3 := newParamRecordingExecutor()
			p.now = time.Date(2026, 11, 26, 4, 10, 0, 0, time.UTC)
			p.checkResult = &maintenance.ReleaseUpdateCheckResult{
				Tier: "binary", UpdateAvailable: true,
				CurrentVersion: "nightly-20261120", LatestVersion: "nightly-20261126",
				Channel: "nightly", Class: "upgrade",
			}
			p.executor = exec3
			srv.runReleaseAutoUpdate(ctx, p.deps())

			select {
			case <-exec3.ran:
			case <-time.After(5 * time.Second):
				t.Fatal("day 3 executor never ran for newer tag")
			}
			exec3.mu.Lock()
			gotTarget := exec3.params[maintenance.ParamTargetVersion]
			exec3.mu.Unlock()
			assert.Equal(t, "nightly-20261126", gotTarget)

			state, err = srv.readAutoUpdateState(ctx)
			require.NoError(t, err)
			assert.Empty(t, state.SuppressedTag, "newer tag must clear suppression")
			assert.Equal(t, "nightly-20261126", state.LastTarget)
			assert.Equal(t, AutoUpdateOutcomeStarted, state.LastOutcome)
			waitForRunTerminal(t, s, state.LastRunID)
		})
	}

	t.Run("reconcile records exact outcome outside window", func(t *testing.T) {
		for _, tc := range []struct {
			name        string
			runStatus   string
			lastStatus  string
			wantOutcome string
			wantSupp    string
		}{
			{"verified", store.MaintenanceStatusCompleted, LastUpdateVerified, AutoUpdateOutcomeVerified, ""},
			{"failed", store.MaintenanceStatusFailed, LastUpdateFailed, AutoUpdateOutcomeFailed, "nightly-20261124"},
			{"rolled_back", store.MaintenanceStatusFailed, LastUpdateRolledBack, AutoUpdateOutcomeRolledBack, "nightly-20261124"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				srv, s := newAutoUpdateJobTestServer(t, MaintenanceConfig{
					DeploymentTier: "binary", ReleaseChannel: "nightly", UpdatePolicy: "auto",
					Installer: newTestInstaller(maintenance.StrategySymlink),
				})
				run := &store.MaintenanceOperationRun{
					ID:           api.NewUUID(),
					OperationKey: "update-binary",
					Status:       tc.runStatus,
					StartedAt:    time.Now(),
					StartedBy:    autoUpdateStartedBy,
				}
				require.NoError(t, s.CreateMaintenanceRun(ctx, run))
				require.NoError(t, srv.writeHubSettingJSON(ctx, HubSettingSectionLastUpdate, LastUpdate{
					RunID: run.ID, From: "nightly-20261120", To: "nightly-20261124", Status: tc.lastStatus,
				}))
				require.NoError(t, srv.writeAutoUpdateState(ctx, AutoUpdateState{
					LastAttemptDay: "2026-11-24",
					LastRunID:      run.ID,
					LastTarget:     "nightly-20261124",
					LastOutcome:    AutoUpdateOutcomeStarted,
				}))

				p := &autoUpdateJobProbe{
					now:     time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC), // outside window
					running: "nightly-20261120",
				}
				srv.runReleaseAutoUpdate(ctx, p.deps())
				assert.Equal(t, 0, p.checks)

				state, err := srv.readAutoUpdateState(ctx)
				require.NoError(t, err)
				assert.Equal(t, tc.wantOutcome, state.LastOutcome)
				assert.Equal(t, tc.wantSupp, state.SuppressedTag)
			})
		}
	})
}

func TestRunReleaseAutoUpdate_InFlightDowngradeAndSelectedChannel(t *testing.T) {
	ctx := context.Background()
	inWindow := time.Date(2026, 11, 24, 4, 15, 0, 0, time.UTC)

	t.Run("in-flight manual run skips auto-update", func(t *testing.T) {
		srv, s := newAutoUpdateJobTestServer(t, MaintenanceConfig{
			DeploymentTier: "binary", ReleaseChannel: "nightly", UpdatePolicy: "auto",
			Installer: newTestInstaller(maintenance.StrategySymlink),
		})
		newRestartingRun(t, s)
		p := &autoUpdateJobProbe{
			now:     inWindow,
			running: "nightly-20261120",
			checkResult: &maintenance.ReleaseUpdateCheckResult{
				Tier: "binary", UpdateAvailable: true,
				CurrentVersion: "nightly-20261120", LatestVersion: "nightly-20261124",
				Channel: "nightly", Class: "upgrade",
			},
			executor: newParamRecordingExecutor(),
		}
		srv.runReleaseAutoUpdate(ctx, p.deps())
		runs, err := s.ListMaintenanceRuns(ctx, "update-binary", 5)
		require.NoError(t, err)
		assert.Len(t, runs, 1, "must not start a second run while one is in flight")
		state, err := srv.readAutoUpdateState(ctx)
		require.NoError(t, err)
		assert.Equal(t, AutoUpdateOutcomeSkippedInFlight, state.LastOutcome)
	})

	t.Run("downgrade is never attempted", func(t *testing.T) {
		srv, s := newAutoUpdateJobTestServer(t, MaintenanceConfig{
			DeploymentTier: "binary", ReleaseChannel: "nightly", UpdatePolicy: "auto",
			Installer: newTestInstaller(maintenance.StrategySymlink),
		})
		realExec := maintenance.NewBinaryUpdateExecutor(maintenance.BinaryUpdateOptions{
			CurrentVersion: "nightly-20261124",
			Installer:      newTestInstaller(maintenance.StrategySymlink),
			DowngradeFloor: downgradeFloor,
		})
		p := &autoUpdateJobProbe{
			now:     inWindow,
			running: "nightly-20261124",
			checkResult: &maintenance.ReleaseUpdateCheckResult{
				Tier: "binary", UpdateAvailable: true,
				CurrentVersion: "nightly-20261124", LatestVersion: "nightly-20261120",
				Channel: "nightly", Class: "downgrade",
			},
			executor: realExec,
		}
		srv.runReleaseAutoUpdate(ctx, p.deps())
		runs, err := s.ListMaintenanceRuns(ctx, "update-binary", 5)
		require.NoError(t, err)
		assert.Empty(t, runs, "downgrade must be refused without confirm_downgrade")
		state, err := srv.readAutoUpdateState(ctx)
		require.NoError(t, err)
		assert.Equal(t, AutoUpdateOutcomeSkippedRefused, state.LastOutcome)
	})

	t.Run("own in-flight run ends tick and preserves started outcome", func(t *testing.T) {
		srv, s := newAutoUpdateJobTestServer(t, MaintenanceConfig{
			DeploymentTier: "binary", ReleaseChannel: "nightly", UpdatePolicy: "auto",
			Installer: newTestInstaller(maintenance.StrategySymlink),
		})
		srv.maintenance = NewMaintenanceState(true, "planned work")
		run := &store.MaintenanceOperationRun{
			ID:           api.NewUUID(),
			OperationKey: "update-binary",
			Status:       store.MaintenanceStatusRunning,
			StartedAt:    time.Now(),
			StartedBy:    autoUpdateStartedBy,
		}
		require.NoError(t, s.CreateMaintenanceRun(ctx, run))
		require.NoError(t, srv.writeAutoUpdateState(ctx, AutoUpdateState{
			LastAttemptDay: "2026-11-24",
			LastRunID:      run.ID,
			LastTarget:     "nightly-20261124",
			LastOutcome:    AutoUpdateOutcomeStarted,
		}))

		// Next day inside the window with maintenance mode enabled: must end the
		// tick early and preserve LastOutcome="started" so a later failure is
		// still reconciled and suppressed.
		p := &autoUpdateJobProbe{
			now:     time.Date(2026, 11, 25, 4, 15, 0, 0, time.UTC),
			running: "nightly-20261120",
			checkResult: &maintenance.ReleaseUpdateCheckResult{
				Tier: "binary", UpdateAvailable: true,
				CurrentVersion: "nightly-20261120", LatestVersion: "nightly-20261125",
				Channel: "nightly", Class: "upgrade",
			},
			executor: newParamRecordingExecutor(),
		}
		srv.runReleaseAutoUpdate(ctx, p.deps())
		assert.Equal(t, 0, p.checks)

		state, err := srv.readAutoUpdateState(ctx)
		require.NoError(t, err)
		assert.Equal(t, AutoUpdateOutcomeStarted, state.LastOutcome)
		assert.Equal(t, run.ID, state.LastRunID)
		assert.Equal(t, "2026-11-24", state.LastAttemptDay)
	})

	t.Run("follows selected channel", func(t *testing.T) {
		srv, s := newAutoUpdateJobTestServer(t, MaintenanceConfig{
			DeploymentTier: "binary", ReleaseChannel: "nightly", UpdatePolicy: "notify",
			Installer: newTestInstaller(maintenance.StrategySymlink),
		})
		enableAutoUpdateSetting(t, srv, 4)
		putReleaseSelection(t, s, ReleaseSelection{Mode: UpdateModeRelease, Channel: "preview", Pin: maintenance.PinLatest})
		exec := newParamRecordingExecutor()
		p := &autoUpdateJobProbe{
			now:     inWindow,
			running: "nightly-20261120",
			checkResult: &maintenance.ReleaseUpdateCheckResult{
				Tier: "binary", UpdateAvailable: true,
				CurrentVersion: "nightly-20261120", LatestVersion: "v0.9.0-rc.1",
				Channel: "preview", Class: "switch-newer",
			},
			executor: exec,
		}
		srv.runReleaseAutoUpdate(ctx, p.deps())
		select {
		case <-exec.ran:
		case <-time.After(5 * time.Second):
			t.Fatal("executor never ran for selected channel")
		}
		assert.Equal(t, "preview", p.gotOpts.Channel)
		exec.mu.Lock()
		gotTarget := exec.params[maintenance.ParamTargetVersion]
		gotChan := exec.params[maintenance.ParamChannel]
		exec.mu.Unlock()
		assert.Equal(t, "v0.9.0-rc.1", gotTarget)
		assert.Equal(t, "preview", gotChan)
		state, err := srv.readAutoUpdateState(ctx)
		require.NoError(t, err)
		waitForRunTerminal(t, s, state.LastRunID)
	})
}

func TestRegisterSchedulerHandlers_ReleaseAutoUpdateAlwaysRegistered(t *testing.T) {
	srv, _ := newAutoUpdateJobTestServer(t, MaintenanceConfig{
		DeploymentTier: "binary",
		UpdatePolicy:   "disabled",
	})
	sched := NewScheduler(srv.store, logging.Subsystem("hub.scheduler"))
	srv.scheduler = sched
	srv.registerSchedulerHandlers()

	var found *RecurringHandler
	for i := range sched.recurring {
		if sched.recurring[i].Name == "release-auto-update" {
			found = &sched.recurring[i]
			break
		}
	}
	require.NotNil(t, found, "release-auto-update must always be registered even when update_policy is disabled")
	assert.True(t, found.Singleton, "release-auto-update must be registered via RegisterRecurringSingleton")
	assert.Equal(t, releaseAutoUpdateIntervalMinutes, found.Interval)
}
