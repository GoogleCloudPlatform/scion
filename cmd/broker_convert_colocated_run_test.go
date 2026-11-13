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

// setConvertProcessEndpointFlags puts the process-global 'server start' flags
// in the state they have inside 'scion runtime-broker convert' (unset, with
// webPort's flag default), and restores them when the test ends.
func setConvertProcessEndpointFlags(t *testing.T) {
	t.Helper()
	savedHub, savedBroker, savedWeb, savedHosted := enableHub, enableRuntimeBroker, enableWeb, hostedMode
	savedWebPort, savedHubPort, savedBase, savedDebug := webPort, hubPort, webBaseURL, enableDebug
	t.Cleanup(func() {
		enableHub, enableRuntimeBroker, enableWeb, hostedMode = savedHub, savedBroker, savedWeb, savedHosted
		webPort, hubPort, webBaseURL, enableDebug = savedWebPort, savedHubPort, savedBase, savedDebug
	})
	enableHub, enableRuntimeBroker, enableWeb, hostedMode = false, false, false, false
	webPort, hubPort, webBaseURL, enableDebug = 8080, 9810, "", false
}

// serverResolveForSavedLaunch runs the server's own resolveHubEndpointWithSource
// with the global flags and port override reconciled as 'scion server start'
// reconciles them for a co-located launch (ptone/scion#7714).
func serverResolveForSavedLaunch(t *testing.T, configPath string, settings *config.Settings, hubOn, webOn, hosted bool, hubPortOverride, webPortVal int, baseURL string) (string, hubEndpointSource) {
	t.Helper()
	savedHub, savedBroker, savedWeb, savedHosted := enableHub, enableRuntimeBroker, enableWeb, hostedMode
	savedWebPort, savedBase, savedDebug := webPort, webBaseURL, enableDebug
	defer func() {
		enableHub, enableRuntimeBroker, enableWeb, hostedMode = savedHub, savedBroker, savedWeb, savedHosted
		webPort, webBaseURL, enableDebug = savedWebPort, savedBase, savedDebug
	}()
	cfg, err := config.LoadGlobalConfig(configPath)
	require.NoError(t, err)
	if hubPortOverride != 0 {
		cfg.Hub.Port = hubPortOverride
	}
	enableHub, enableRuntimeBroker, enableWeb, hostedMode = hubOn, true, webOn, hosted
	webPort, webBaseURL, enableDebug = webPortVal, baseURL, false
	return resolveHubEndpointWithSource(cfg, settings)
}

// Agreement with the server's Hub endpoint resolver (ptone/scion#7714): when a
// co-located host is detected from a saved server launch, colocatedPrecheckHost
// (with 'server start' flags unset, as inside 'runtime-broker convert')
// resolves the exact URL and source that the co-located server derives for the
// same saved launch across --base-url, SCION_SERVER_HUB_ENDPOINT,
// SCION_SERVER_BASE_URL, server.hub.public_url, settings hub.endpoint, hosted
// IAP audience derivation and the localhost fallback (web port or Hub port).
func TestColocatedPrecheckHost_AgreesWithServerResolver(t *testing.T) {
	const (
		flagURL     = "https://flag.example.com"
		envHubURL   = "https://env-hub.example.com"
		envBaseURL  = "https://env-base.example.com"
		fileURL     = "https://file.example.com"
		settingsURL = "https://settings.example.com"
		iapAudience = "/projects/12345/locations/us-central1/services/scion-hub"
		iapURL      = "https://scion-hub-12345.us-central1.run.app"
	)
	cases := []struct {
		name           string
		settingsYAML   string
		extraCfgYAML   string // written to a separate --config file when non-empty
		envHub         string
		envBase        string
		envLegacyHub   string
		args           []string
		serverHubOn    bool
		serverWebOn    bool
		serverHosted   bool
		serverHubPort  int
		serverWebPort  int
		serverBaseFlag string
		wantURL        string
		wantSrc        hubEndpointSource
		// checkConvertHost runs convertHost(globalDir) too when the named
		// settings endpoint does not intentionally omit a local match.
		skipConvertHost bool
	}{
		{
			name:            "--base-url outranks env and file",
			settingsYAML:    "server:\n  hub:\n    public_url: " + fileURL + "\nhub:\n  endpoint: " + fileURL + "\n",
			envHub:          envHubURL,
			envBase:         envBaseURL,
			args:            []string{"server", "start", "--foreground", "--base-url=" + flagURL + "/"},
			serverHubOn:     true,
			serverWebOn:     true,
			serverWebPort:   8080,
			serverBaseFlag:  flagURL + "/",
			wantURL:         flagURL,
			wantSrc:         hubEndpointSourceBaseURLFlag,
			skipConvertHost: true, // envHubURL overrides ev.Configured while hub.endpoint names fileURL
		},
		{
			name:           "--base-url alone on workstation launch",
			args:           []string{"server", "start", "--foreground", "--base-url=" + flagURL},
			serverHubOn:    true,
			serverWebOn:    true,
			serverWebPort:  8080,
			serverBaseFlag: flagURL,
			wantURL:        flagURL,
			wantSrc:        hubEndpointSourceBaseURLFlag,
		},
		{
			name:          "SCION_SERVER_HUB_ENDPOINT outranks SCION_SERVER_BASE_URL and file",
			settingsYAML:  "server:\n  hub:\n    public_url: " + fileURL + "\n",
			envHub:        envHubURL,
			envBase:       envBaseURL,
			args:          []string{"server", "start", "--foreground"},
			serverHubOn:   true,
			serverWebOn:   true,
			serverWebPort: 8080,
			wantURL:       envHubURL,
			wantSrc:       hubEndpointSourceConfig,
		},
		{
			name:          "SCION_SERVER_BASE_URL outranks file",
			settingsYAML:  "server:\n  hub:\n    public_url: " + fileURL + "\n",
			envBase:       envBaseURL + "/",
			args:          []string{"server", "start", "--foreground"},
			serverHubOn:   true,
			serverWebOn:   true,
			serverWebPort: 8080,
			wantURL:       envBaseURL,
			wantSrc:       hubEndpointSourceBaseURLEnv,
		},
		{
			name:          "configured server.hub.public_url outranks settings hub.endpoint",
			settingsYAML:  "server:\n  hub:\n    public_url: " + fileURL + "\nhub:\n  endpoint: " + fileURL + "\n",
			args:          []string{"server", "start", "--foreground"},
			serverHubOn:   true,
			serverWebOn:   true,
			serverWebPort: 8080,
			wantURL:       fileURL,
			wantSrc:       hubEndpointSourceConfig,
		},
		{
			name:            "settings hub.endpoint outranks IAP and localhost",
			settingsYAML:    "server:\n  mode: hosted\n  broker:\n    enabled: true\n  auth:\n    proxy:\n      iap:\n        audience: " + iapAudience + "\nhub:\n  endpoint: " + settingsURL + "\n",
			args:            []string{"server", "start", "--foreground", "--enable-hub"},
			serverHubOn:     true,
			serverWebOn:     false,
			serverHosted:    true,
			serverWebPort:   8080,
			wantURL:         settingsURL,
			wantSrc:         hubEndpointSourceSettings,
			skipConvertHost: true, // settingsURL is remote without server.hub.public_url
		},
		{
			name:          "SCION_HUB_ENDPOINT matching saved listener resolves as settings",
			envLegacyHub:  "http://127.0.0.1:8443",
			args:          []string{"server", "start", "--foreground", "--web-port=8443"},
			serverHubOn:   true,
			serverWebOn:   true,
			serverWebPort: 8443,
			wantURL:       "http://127.0.0.1:8443",
			wantSrc:       hubEndpointSourceSettings,
		},
		{
			name:          "hosted --hosted flag derives IAP Cloud Run URL",
			settingsYAML:  "server:\n  auth:\n    proxy:\n      iap:\n        audience: " + iapAudience + "\n",
			args:          []string{"server", "start", "--foreground", "--hosted", "--enable-hub", "--enable-runtime-broker"},
			serverHubOn:   true,
			serverWebOn:   false,
			serverHosted:  true,
			serverWebPort: 8080,
			wantURL:       iapURL,
			wantSrc:       hubEndpointSourceIAPAudience,
		},
		{
			name:          "hosted server.mode in settings derives IAP Cloud Run URL",
			settingsYAML:  "server:\n  mode: hosted\n  broker:\n    enabled: true\n  auth:\n    proxy:\n      iap:\n        audience: " + iapAudience + "\n",
			args:          []string{"server", "start", "--foreground", "--enable-hub"},
			serverHubOn:   true,
			serverWebOn:   false,
			serverHosted:  true,
			serverWebPort: 8080,
			wantURL:       iapURL,
			wantSrc:       hubEndpointSourceIAPAudience,
		},
		{
			name:          "workstation default localhost fallback uses web port 8080",
			args:          []string{"server", "start", "--foreground"},
			serverHubOn:   true,
			serverWebOn:   true,
			serverWebPort: 8080,
			wantURL:       "http://localhost:8080",
			wantSrc:       hubEndpointSourceLocalhost,
		},
		{
			name:          "workstation --web-port localhost fallback",
			args:          []string{"server", "start", "--foreground", "--web-port=8443"},
			serverHubOn:   true,
			serverWebOn:   true,
			serverWebPort: 8443,
			wantURL:       "http://localhost:8443",
			wantSrc:       hubEndpointSourceLocalhost,
		},
		{
			name:          "workstation --enable-web=false uses Hub port and --port override",
			args:          []string{"server", "start", "--foreground", "--enable-web=false", "--port=9123"},
			serverHubOn:   true,
			serverWebOn:   false,
			serverHubPort: 9123,
			serverWebPort: 8080,
			wantURL:       "http://localhost:9123",
			wantSrc:       hubEndpointSourceLocalhost,
		},
		{
			name:          "hosted without web uses Hub port localhost fallback",
			args:          []string{"server", "start", "--foreground", "--hosted", "--enable-hub", "--enable-runtime-broker"},
			serverHubOn:   true,
			serverWebOn:   false,
			serverHosted:  true,
			serverWebPort: 8080,
			wantURL:       "http://localhost:9810",
			wantSrc:       hubEndpointSourceLocalhost,
		},
		{
			name:          "saved --config loads server config from that path",
			extraCfgYAML:  "hub:\n  endpoint: " + fileURL + "\n",
			args:          []string{"server", "start", "--foreground"},
			serverHubOn:   true,
			serverWebOn:   true,
			serverWebPort: 8080,
			wantURL:       fileURL,
			wantSrc:       hubEndpointSourceConfig,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			globalDir := isolateServerHubEndpointEnv(t)
			t.Chdir(t.TempDir())
			oldMode, oldDetect := brokerConvertHostMode, convertDetectHost
			t.Cleanup(func() { brokerConvertHostMode, convertDetectHost = oldMode, oldDetect })
			brokerConvertHostMode = ""
			convertDetectHost = detectColocatedHost

			body := "schema_version: \"1\"\n" + tc.settingsYAML
			require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(body), 0o600))

			args := append([]string(nil), tc.args...)
			var cfgPath string
			if tc.extraCfgYAML != "" {
				cfgPath = filepath.Join(t.TempDir(), "custom-server.yaml")
				require.NoError(t, os.WriteFile(cfgPath, []byte(tc.extraCfgYAML), 0o600))
				args = append(args, "--config="+cfgPath)
			}
			require.NoError(t, daemon.SaveArgs(serverDaemonComponent, globalDir, args))

			if tc.envHub != "" {
				t.Setenv("SCION_SERVER_HUB_ENDPOINT", tc.envHub)
			}
			if tc.envBase != "" {
				t.Setenv("SCION_SERVER_BASE_URL", tc.envBase)
			}
			if tc.envLegacyHub != "" {
				t.Setenv("SCION_HUB_ENDPOINT", tc.envLegacyHub)
			}

			settings, err := config.LoadSettings(globalDir)
			require.NoError(t, err)

			wantServerURL, wantServerSrc := serverResolveForSavedLaunch(t, cfgPath, settings,
				tc.serverHubOn, tc.serverWebOn, tc.serverHosted, tc.serverHubPort, tc.serverWebPort, tc.serverBaseFlag)
			require.Equal(t, tc.wantURL, wantServerURL, "test setup: server URL")
			require.Equal(t, tc.wantSrc, wantServerSrc, "test setup: server source")

			// Inside 'scion runtime-broker convert', the 'server start' flags are unset.
			setConvertProcessEndpointFlags(t)

			detectedHost, err := detectColocatedHost(globalDir)
			require.NoError(t, err)
			require.True(t, detectedHost.Colocated)
			hc := colocatedPrecheckHost(detectedHost, settings)
			assert.Equal(t, wantServerURL, hc.hubEndpoint, "convert endpoint must match server resolver")
			assert.Equal(t, wantServerSrc, hc.hubEndpointSrc, "convert endpoint source must match server resolver")

			if !tc.skipConvertHost {
				convHost, mode, src, err := convertHost(globalDir)
				require.NoError(t, err)
				assert.Equal(t, brokerconvert.HostModeColocated, mode)
				assert.Equal(t, brokerconvert.HostModeDetected, src)
				convHC := colocatedPrecheckHost(convHost, settings)
				assert.Equal(t, wantServerURL, convHC.hubEndpoint)
				assert.Equal(t, wantServerSrc, convHC.hubEndpointSrc)
			}
		})
	}
}

// Asserted co-located mode with no saved launch (ptone/scion#7714): convert
// uses ONLY the configured server.hub.endpoint, ignoring SCION_SERVER_BASE_URL,
// settings hub.endpoint, IAP derivation and the localhost fallback, and
// refuses a Cloud Run precheck with a "set server.hub.endpoint" message when
// server.hub.endpoint is missing (while non-Cloud Run targets remain allowed).
func TestColocatedPrecheckHost_AssertedUsesOnlyConfiguredEndpoint(t *testing.T) {
	const nfsStorageYAML = "  project_storage:\n    backend: nfs\n    homes: nfs\n    nfs:\n      mount_root: /mnt/nfs\n      shares:\n        - id: ws1\n          server: 10.0.0.2\n          export: /x\n"

	crTargetInst := config.V1RuntimeBrokerInstanceConfig{
		Key:           "cr-1",
		RuntimeTarget: &config.V1RuntimeTargetConfig{Type: config.RuntimeTargetTypeCloudRun},
	}
	dockerTargetInst := config.V1RuntimeBrokerInstanceConfig{
		Key:           "docker-1",
		RuntimeTarget: &config.V1RuntimeTargetConfig{Type: config.RuntimeTargetTypeDocker},
	}

	t.Run("configured server.hub.endpoint is used and ignores SCION_SERVER_BASE_URL", func(t *testing.T) {
		globalDir := isolateServerHubEndpointEnv(t)
		t.Chdir(t.TempDir())
		oldMode, oldDetect := brokerConvertHostMode, convertDetectHost
		t.Cleanup(func() { brokerConvertHostMode, convertDetectHost = oldMode, oldDetect })
		convertDetectHost = detectColocatedHost
		brokerConvertHostMode = brokerconvert.HostModeColocated

		settingsBody := "schema_version: \"1\"\nserver:\n  hub:\n    public_url: https://configured.example.com\n" + nfsStorageYAML
		require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(settingsBody), 0o600))
		t.Setenv("SCION_SERVER_BASE_URL", "https://ignored-base.example.com")
		setConvertProcessEndpointFlags(t)

		host, mode, source, err := convertHost(globalDir)
		require.NoError(t, err)
		require.Equal(t, brokerconvert.HostModeColocated, mode)
		require.Equal(t, brokerconvert.HostModeAsserted, source)

		settings, err := config.LoadSettings(globalDir)
		require.NoError(t, err)
		hc := colocatedPrecheckHost(host, settings)
		assert.Equal(t, "https://configured.example.com", hc.hubEndpoint)
		assert.Equal(t, hubEndpointSourceConfig, hc.hubEndpointSrc)
	})

	t.Run("missing server.hub.endpoint refuses cloudrun with set server.hub.endpoint even if env or settings or IAP exist", func(t *testing.T) {
		globalDir := isolateServerHubEndpointEnv(t)
		t.Chdir(t.TempDir())
		oldMode, oldDetect := brokerConvertHostMode, convertDetectHost
		t.Cleanup(func() { brokerConvertHostMode, convertDetectHost = oldMode, oldDetect })
		convertDetectHost = detectColocatedHost
		brokerConvertHostMode = brokerconvert.HostModeColocated

		settingsBody := "schema_version: \"1\"\nserver:\n  mode: hosted\n  auth:\n    proxy:\n      iap:\n        audience: /projects/12345/locations/us-central1/services/scion-hub\n" + nfsStorageYAML
		require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(settingsBody), 0o600))
		t.Setenv("SCION_SERVER_BASE_URL", "https://ignored-base.example.com")
		setConvertProcessEndpointFlags(t)

		host, mode, source, err := convertHost(globalDir)
		require.NoError(t, err)
		require.Equal(t, brokerconvert.HostModeColocated, mode)
		require.Equal(t, brokerconvert.HostModeAsserted, source)

		// Even if settings carries hub.endpoint, asserted mode without a saved
		// launch must NOT fall back to settings.GetHubEndpoint().
		settings := &config.Settings{Hub: &config.HubClientConfig{Endpoint: "https://ignored-settings.example.com"}}
		hc := colocatedPrecheckHost(host, settings)
		assert.Empty(t, hc.hubEndpoint)
		assert.Equal(t, hubEndpointSourceNone, hc.hubEndpointSrc)

		_, err = flatInstancePrecheck(context.Background(), crTargetInst, hc)
		require.Error(t, err)
		assert.Equal(t, reasonCloudRunHubEndpoint, brokerhost.ReasonCode(err))
		assert.Contains(t, err.Error(), "set server.hub.endpoint")

		// A Docker instance on the same asserted co-located host is unaffected.
		_, err = flatInstancePrecheck(context.Background(), dockerTargetInst, hc)
		assert.NoError(t, err)
	})
}
