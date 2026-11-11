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
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/brokerconvert"
	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/brokerhost"
	"github.com/GoogleCloudPlatform/scion/pkg/brokerregistration"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/daemon"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// colocatedStoppedSetup prepares a co-located host whose server (the Hub)
// is down and NO Hub client exists: the convert flags for one profile,
// auto-confirm, and co-located seams with a drained local store. It
// returns the env, settings, a command and the local plan's digest.
func colocatedStoppedSetup(t *testing.T) (*brokerconvert.Env, *config.Settings, *cobra.Command, string) {
	t.Helper()
	convertTestHome(t, convertTestSettings)
	setConvertFlags(t, false, false, "", "build")
	brokerConvertApply = true
	oldConfirm, oldHook := autoConfirm, convertColocatedEnvHook
	t.Cleanup(func() { autoConfirm, convertColocatedEnvHook = oldConfirm, oldHook })
	autoConfirm = true
	convertColocatedEnvHook = func(ce *brokerconvert.ColocatedEnv) {
		ce.ServerUp = func(context.Context) bool { return false }
		ce.Q7 = func(context.Context, *brokerconvert.Journal) brokerconvert.Q7Input {
			return brokerconvert.Q7Input{
				Topology: brokerconvert.TopologySingleSQLite,
				LegacyStore: func() (brokerconvert.LegacyStoreFacts, error) {
					return brokerconvert.LegacyStoreFacts{RowPresent: true}, nil
				},
			}
		}
	}
	// No Hub is reachable: the Hub is this stopped server. No client is
	// opened, so any Hub call would fail.
	setConvertHubClient(t, func(*config.Settings) (hubclient.Client, error) { return nil, nil })
	env, settings, err := newConvertEnv()
	require.NoError(t, err)
	require.Nil(t, env.Client)
	c := &cobra.Command{}
	c.SetErr(io.Discard)
	c.SetContext(context.Background())
	in, _, err := loadConvertInput(c)
	require.NoError(t, err)
	plan, err := brokerconvert.BuildLocal(in)
	require.NoError(t, err)
	digest, err := brokerconvert.PlanDigest(plan, in.Choices)
	require.NoError(t, err)
	return env, settings, c, digest
}

// The stopped phase of a co-located apply runs from the CLI with no Hub
// reachable: with the journal awaiting the stop, a second --apply plans
// locally (no Hub collection), passes Q7 and writes
// server.broker.instances, reaching applied.
func TestRunConvertApply_ColocatedStoppedPhaseNeedsNoHub(t *testing.T) {
	env, settings, c, digest := colocatedStoppedSetup(t)
	now := time.Now().UTC()
	j, err := brokerconvert.NewJournal(digest, convertTestBrokerID, now)
	require.NoError(t, err)
	j.State = brokerconvert.JournalAwaitingStop
	j.StopRecheckAt = &now
	require.NoError(t, brokerconvert.SaveJournal(env.GlobalDir, j, now))

	rep, err := runConvertApply(context.Background(), c, env, settings, colocatedHost{Colocated: true}, brokerconvert.HostModeColocated, brokerconvert.HostModeDetected)
	require.NoError(t, err)
	assert.Equal(t, brokerconvert.JournalApplied, rep.State)
	assert.Contains(t, rep.Notes, brokerconvert.NoteColocatedStart)
	insts, err := env.LoadInstances()
	require.NoError(t, err)
	assert.Len(t, insts, 1, "server.broker.instances is written")
	got, err := brokerconvert.LoadJournal(env.GlobalDir)
	require.NoError(t, err)
	assert.Equal(t, brokerconvert.JournalApplied, got.State)
}

// With the settings written and the server still down, a further --apply
// gives the start-pending note and no error, again with no Hub reachable.
func TestRunConvertApply_ColocatedStartPendingNeedsNoHub(t *testing.T) {
	env, settings, c, digest := colocatedStoppedSetup(t)
	now := time.Now().UTC()
	j, err := brokerconvert.NewJournal(digest, convertTestBrokerID, now)
	require.NoError(t, err)
	j.State = brokerconvert.JournalApplied
	j.Settings = brokerconvert.ItemStatus{State: brokerconvert.StepDone}
	j.Switch = brokerconvert.ItemStatus{State: brokerconvert.StepDone}
	require.NoError(t, brokerconvert.SaveJournal(env.GlobalDir, j, now))

	rep, err := runConvertApply(context.Background(), c, env, settings, colocatedHost{Colocated: true}, brokerconvert.HostModeColocated, brokerconvert.HostModeDetected)
	require.NoError(t, err)
	require.NotNil(t, rep)
	assert.Contains(t, rep.Notes, brokerconvert.NoteColocatedStart)
	assert.Nil(t, rep.Refusal)
}

// The wiring of the Register seam: PR1's env registers with the remote
// precheck; on a co-located host the apply installs the co-located one (and
// re-applies the test hook); any other host keeps the remote Register.
func TestUseColocatedRegister_InstallsColocatedPrecheck(t *testing.T) {
	convertTestHome(t, convertTestSettings)
	setConvertFlags(t, false, false, "", "build")
	setConvertHubClient(t, func(*config.Settings) (hubclient.Client, error) { return nil, nil })
	var modes []brokerhost.Mode
	oldFor, oldHook := convertRegisterFor, convertEnvHook
	t.Cleanup(func() { convertRegisterFor, convertEnvHook = oldFor, oldHook })
	convertRegisterFor = func(settings *config.Settings, globalDir string, hc flatPrecheckHost) func(context.Context, config.V1RuntimeBrokerInstanceConfig) (string, string, error) {
		modes = append(modes, hc.mode)
		return convertRegister(settings, globalDir, hc)
	}
	hooked := 0
	convertEnvHook = func(*brokerconvert.Env) { hooked++ }

	env, settings, err := newConvertEnv()
	require.NoError(t, err)
	require.Equal(t, []brokerhost.Mode{brokerhost.ModeRemote}, modes, "PR1's env: the remote precheck")
	require.Equal(t, 1, hooked)

	useColocatedRegister(env, settings, colocatedHost{})
	assert.Equal(t, []brokerhost.Mode{brokerhost.ModeRemote}, modes, "a remote host keeps the remote Register")
	assert.Equal(t, 1, hooked)

	useColocatedRegister(env, settings, colocatedHost{Colocated: true})
	assert.Equal(t, []brokerhost.Mode{brokerhost.ModeRemote, brokerhost.ModeColocated}, modes, "a co-located host installs the co-located precheck")
	assert.Equal(t, 2, hooked, "the test hook is re-applied")
	assert.NotNil(t, env.Register)
}

// An undecidable host refuses (fail closed) instead of falling back to the
// remote path.
func TestConvertHost_UndeterminedRefuses(t *testing.T) {
	old := convertDetectHost
	t.Cleanup(func() { convertDetectHost = old })
	convertDetectHost = func(string) (colocatedHost, error) { return colocatedHost{}, errColocatedUndetermined }

	_, _, _, err := convertHost(t.TempDir())
	var ref *brokerconvert.Refusal
	require.True(t, errors.As(err, &ref))
	assert.Equal(t, brokerconvert.RefuseColocatedUnknown, ref.Code)
}

// --single-hub-replica on a remote host refuses before PR1's apply runs.
func TestConvertApply_SingleHubReplicaOnRemoteHostRefuses(t *testing.T) {
	old := brokerConvertSingleHubReplica
	t.Cleanup(func() { brokerConvertSingleHubReplica = old })
	brokerConvertSingleHubReplica = true

	rep, err := convertApply(context.Background(), &brokerconvert.Env{GlobalDir: t.TempDir()}, colocatedHost{}, brokerconvert.ApplyInput{})
	var ref *brokerconvert.Refusal
	require.True(t, errors.As(err, &ref))
	assert.Equal(t, brokerconvert.RefuseSingleHubMisplaced, ref.Code)
	require.NotNil(t, rep)
	assert.Equal(t, brokerconvert.ModeApply, rep.Mode)
}

// The online read needs the Hub client; without one it is an error, which
// the cleanup reports as a Hub request refusal rather than a removal.
func TestNewColocatedEnv_InstanceOnlineNeedsClient(t *testing.T) {
	ce := newColocatedEnv(&brokerconvert.Env{GlobalDir: t.TempDir()}, colocatedHost{Colocated: true})
	_, err := ce.InstanceOnline(context.Background(), "b-1")
	assert.ErrorIs(t, err, errHubNotConfigured)
}

// The Q7 input carries only the probe its topology needs: the SQLite count
// for a local store, the endpoint probe for an external one, none for a
// replica setup.
func TestColocatedQ7Input_ProbesByTopology(t *testing.T) {
	j := &brokerconvert.Journal{LegacyBrokerID: "b-legacy"}
	mk := func(driver, url string) *config.GlobalConfig {
		cfg := config.DefaultGlobalConfig()
		cfg.Database.Driver, cfg.Database.URL = driver, url
		cfg.Storage.Provider, cfg.Auth.Mode = "local", ""
		return &cfg
	}
	in := colocatedQ7Input(context.Background(), mk("sqlite", "/x/hub.db"), j)
	assert.Equal(t, brokerconvert.TopologySingleSQLite, in.Topology)
	assert.NotNil(t, in.LegacyStore)
	assert.Nil(t, in.EndpointAnswers)

	in = colocatedQ7Input(context.Background(), mk("postgres", "postgres://h/db"), j)
	assert.Equal(t, brokerconvert.TopologySingleExternal, in.Topology)
	assert.Nil(t, in.LegacyStore, "no database is opened for an external store")
	assert.NotNil(t, in.EndpointAnswers)

	in = colocatedQ7Input(context.Background(), nil, j)
	assert.Equal(t, brokerconvert.TopologyUndetermined, in.Topology)
}

// detectColocatedHost decides from the saved launches: a saved server launch
// (workstation defaults: Hub and Runtime Broker on) is co-located unless it
// sets --simulate-remote-broker; with no server launch, a saved broker-daemon
// launch is the remote path; with neither, the mode is refused, not guessed.
func TestDetectColocatedHost_FromSavedLaunches(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	g := filepath.Join(home, ".scion")
	require.NoError(t, os.MkdirAll(g, 0o700))

	// A credentials file from an earlier run: detection must touch nothing.
	creds := filepath.Join(g, "runtime-brokers", "docker", "hub-credentials", "hub.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(creds), 0o700))
	require.NoError(t, os.WriteFile(creds, []byte("{}"), 0o600))

	_, err := detectColocatedHost(g)
	assert.ErrorIs(t, err, errColocatedUndetermined, "no saved launch: undetermined")
	_, err = os.Lstat(creds)
	require.NoError(t, err, "a refused detection touches no file")
	fi, err := os.Lstat(filepath.Join(g, "runtime-brokers", "docker"))
	require.NoError(t, err)
	assert.True(t, fi.IsDir(), "the instance directory has not moved")

	require.NoError(t, daemon.SaveArgs(brokerDaemonComponent, g, []string{"--foreground"}))
	host, err := detectColocatedHost(g)
	require.NoError(t, err)
	assert.False(t, host.Colocated, "a broker-daemon launch is the remote path")

	// Both launches saved: the server launch wins.
	require.NoError(t, daemon.SaveArgs(serverDaemonComponent, g, []string{"--foreground"}))
	host, err = detectColocatedHost(g)
	require.NoError(t, err)
	assert.True(t, host.Colocated, "a workstation server launch runs the Hub and the Runtime Broker")

	require.NoError(t, daemon.SaveArgs(serverDaemonComponent, g, []string{"--foreground", "--simulate-remote-broker"}))
	host, err = detectColocatedHost(g)
	require.NoError(t, err)
	assert.False(t, host.Colocated, "--simulate-remote-broker takes the remote path")
}

// A saved launch file that is a link or not a regular file is refused, not
// followed.
func TestDetectColocatedHost_RefusesLinkedOrOddLaunchFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	g := filepath.Join(home, ".scion")
	require.NoError(t, os.MkdirAll(g, 0o700))
	target := filepath.Join(t.TempDir(), "args.json")
	require.NoError(t, os.WriteFile(target, []byte(`["--foreground"]`), 0o600))
	server := filepath.Join(g, daemon.ArgsFileName(serverDaemonComponent))

	require.NoError(t, os.Symlink(target, server))
	_, err := detectColocatedHost(g)
	assert.ErrorIs(t, err, errColocatedUndetermined, "a linked server launch is refused")
	require.NoError(t, os.Remove(server))

	require.NoError(t, os.Mkdir(server, 0o700))
	_, err = detectColocatedHost(g)
	assert.ErrorIs(t, err, errColocatedUndetermined, "a directory at the launch path is refused")
	require.NoError(t, os.Remove(server))

	broker := filepath.Join(g, daemon.ArgsFileName(brokerDaemonComponent))
	require.NoError(t, os.Symlink(target, broker))
	_, err = detectColocatedHost(g)
	assert.ErrorIs(t, err, errColocatedUndetermined, "a linked broker launch is refused")
	require.NoError(t, os.Remove(broker))

	require.NoError(t, os.Mkdir(broker, 0o700))
	_, err = detectColocatedHost(g)
	assert.ErrorIs(t, err, errColocatedUndetermined, "a directory at the broker launch path is refused")
}

// Agreement (design errata E4): when a saved server launch decides,
// convert's answer equals colocatedBrokerRegisters evaluated on that
// launch's effective inputs, as the server derives them at start: enableHub
// and simulateRemoteBroker from the flags (workstation defaults: Hub and
// Runtime Broker on), and cfg.RuntimeBroker.Enabled from the flag when given,
// else (hosted) from the settings.
func TestDetectColocatedHost_AgreesWithPredicate(t *testing.T) {
	oldHub, oldSim := enableHub, simulateRemoteBroker
	t.Cleanup(func() { enableHub, simulateRemoteBroker = oldHub, oldSim })
	nonNilStore := struct{ store.Store }{}

	cases := []struct {
		name           string
		mode           string // server.mode in settings
		settingsBroker bool   // server.broker.enabled
		args           []string
		// The server's effective inputs for this launch.
		hub, broker, sim bool
	}{
		{"workstation defaults", "", false, []string{"--foreground"}, true, true, false},
		{"workstation hub off", "", false, []string{"--foreground", "--enable-hub=false"}, false, true, false},
		{"workstation broker off", "", false, []string{"--foreground", "--enable-runtime-broker=false"}, true, false, false},
		{"workstation simulate remote", "", false, []string{"--foreground", "--simulate-remote-broker"}, true, true, true},
		{"hosted hub, settings broker on", "hosted", true, []string{"--foreground", "--enable-hub"}, true, true, false},
		{"hosted hub, settings broker off", "hosted", false, []string{"--foreground", "--enable-hub"}, true, false, false},
		{"hosted no hub", "hosted", true, []string{"--foreground"}, false, true, false},
		{"hosted flag turns broker off", "hosted", true, []string{"--foreground", "--enable-hub", "--enable-runtime-broker=false"}, true, false, false},
		{"hosted flag turns broker on", "hosted", false, []string{"--foreground", "--enable-hub", "--enable-runtime-broker"}, true, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			g := filepath.Join(home, ".scion")
			require.NoError(t, os.MkdirAll(g, 0o700))
			settings := "schema_version: \"1\"\nserver:\n"
			if tc.mode != "" {
				settings += "  mode: " + tc.mode + "\n"
			}
			if tc.settingsBroker {
				settings += "  broker:\n    enabled: true\n"
			}
			require.NoError(t, os.WriteFile(filepath.Join(g, "settings.yaml"), []byte(settings), 0o600))
			require.NoError(t, daemon.SaveArgs(serverDaemonComponent, g, tc.args))

			cfg := config.DefaultGlobalConfig()
			cfg.RuntimeBroker.Enabled = tc.broker
			enableHub, simulateRemoteBroker = tc.hub, tc.sim
			want := colocatedBrokerRegisters(&cfg, nonNilStore)

			host, err := detectColocatedHost(g)
			require.NoError(t, err)
			assert.Equal(t, want, host.Colocated)
		})
	}
}

// convertHostHome sets up an isolated HOME with its global dir and resets
// --host-mode and the detection seam after the test.
func convertHostHome(t *testing.T) string {
	t.Helper()
	oldMode, oldDetect := brokerConvertHostMode, convertDetectHost
	t.Cleanup(func() { brokerConvertHostMode, convertDetectHost = oldMode, oldDetect })
	convertDetectHost = detectColocatedHost
	home := t.TempDir()
	t.Setenv("HOME", home)
	g := filepath.Join(home, ".scion")
	require.NoError(t, os.MkdirAll(g, 0o700))
	return g
}

// The DETECTED hold row (design E4): a stale saved server launch, a newer
// broker launch and a saved legacy broker credentials file (the host now
// registers with a remote Hub) refuse with host_mode_conflict. The refusal
// comes before any apply step: no journal is written and no file is touched.
// The fix names only removing the stale launch file; --host-mode remote does
// not override the saved launch.
func TestConvertHost_DetectedStaleLaunchWithRemoteCredentialsRefuses(t *testing.T) {
	g := convertHostHome(t)
	require.NoError(t, daemon.SaveArgs(serverDaemonComponent, g, []string{"--foreground"}))
	require.NoError(t, daemon.SaveArgs(brokerDaemonComponent, g, []string{"--foreground"}))
	creds := filepath.Join(g, brokercredentials.DefaultFileName)
	require.NoError(t, os.WriteFile(creds, []byte(`{"brokerId":"b-1"}`), 0o600))

	_, _, _, err := convertHost(g)
	var ref *brokerconvert.Refusal
	require.True(t, errors.As(err, &ref))
	assert.Equal(t, brokerconvert.RefuseHostModeConflict, ref.Code)
	assert.Contains(t, ref.Text, daemon.ArgsFileName(serverDaemonComponent), "names the launch file")
	assert.Contains(t, ref.Text, brokercredentials.DefaultFileName, "names the credentials file by its fixed name")
	assert.NotContains(t, ref.Text, g, "no local path in the refusal text")
	assert.NotContains(t, ref.Text, "--host-mode")
	_, err = os.Lstat(filepath.Join(brokerconvert.ConvertDir(g), brokerconvert.JournalFileName))
	assert.True(t, errors.Is(err, fs.ErrNotExist), "no journal after the refusal")
	_, err = os.Lstat(creds)
	require.NoError(t, err, "the credentials file is untouched")

	brokerConvertHostMode = brokerconvert.HostModeRemote
	_, _, _, err = convertHost(g)
	require.True(t, errors.As(err, &ref))
	assert.Equal(t, brokerconvert.RefuseHostModeConflict, ref.Code, "--host-mode remote never overrides the saved launch")

	// After the stale launch file is removed, detection falls to the broker
	// launch (remote) and the run proceeds.
	brokerConvertHostMode = ""
	require.NoError(t, daemon.RemoveArgs(serverDaemonComponent, g))
	_, mode, source, err := convertHost(g)
	require.NoError(t, err)
	assert.Equal(t, brokerconvert.HostModeRemote, mode)
	assert.Equal(t, brokerconvert.HostModeDetected, source)
}

// With no saved launch, --host-mode decides (asserted), and without it the
// mode is refused, naming both fixes.
func TestConvertHost_AssertedWhenUndetectable(t *testing.T) {
	g := convertHostHome(t)
	_, _, _, err := convertHost(g)
	var ref *brokerconvert.Refusal
	require.True(t, errors.As(err, &ref))
	assert.Equal(t, brokerconvert.RefuseColocatedUnknown, ref.Code)
	assert.Contains(t, ref.Text, "--host-mode")
	assert.Contains(t, ref.Text, "through the daemon")

	brokerConvertHostMode = brokerconvert.HostModeRemote
	host, mode, source, err := convertHost(g)
	require.NoError(t, err)
	assert.False(t, host.Colocated)
	assert.Equal(t, brokerconvert.HostModeRemote, mode)
	assert.Equal(t, brokerconvert.HostModeAsserted, source)
}

// Path separation (design E4): the conversion's own instance credentials,
// which the running phase's HTTP pre-registration saves under
// <global>/runtime-brokers/<key>/hub-credentials/, are not legacy broker
// credentials. A later phase on the same co-located host (detected from its
// saved server launch) passes the host check. An operator's Hub login, kept
// in <global>/credentials.json, is not broker evidence either.
func TestConvertHost_OwnInstanceCredentialsAndLoginDoNotRefuse(t *testing.T) {
	g := convertHostHome(t)
	require.NoError(t, daemon.SaveArgs(serverDaemonComponent, g, []string{"--foreground"}))
	inst := brokerregistration.InstanceCredentialsDir(g, "docker")
	require.NoError(t, os.MkdirAll(inst, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(inst, "hub.json"), []byte(`{"brokerId":"b-flat-1"}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(g, "credentials.json"), []byte(`{}`), 0o600))

	host, mode, source, err := convertHost(g)
	require.NoError(t, err)
	assert.True(t, host.Colocated)
	assert.Equal(t, brokerconvert.HostModeColocated, mode)
	assert.Equal(t, brokerconvert.HostModeDetected, source)
}
