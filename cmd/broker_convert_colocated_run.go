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
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/brokerconvert"
	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/brokerhost"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/daemon"
)

// errConvertSingleHubFlag refuses --single-hub-replica outside --apply.
var errConvertSingleHubFlag = errors.New("--single-hub-replica is accepted only with --apply on a co-located host, in its stopped phase")

// convertDetectHost decides whether this host is co-located. A variable so
// tests can choose the path.
var convertDetectHost = detectColocatedHost

// convertColocatedEnvHook lets tests replace the co-located host seams.
var convertColocatedEnvHook func(ce *brokerconvert.ColocatedEnv)

// brokerConvertHostMode is --host-mode: the operator's assertion of the
// host mode when detection cannot decide.
var brokerConvertHostMode string

// errConvertHostModeFlag and errConvertHostModeValue refuse a misplaced or
// unknown --host-mode.
var (
	errConvertHostModeFlag  = errors.New("--host-mode is accepted only with --apply or --rollback")
	errConvertHostModeValue = errors.New("--host-mode takes colocated or remote")
)

// convertHost resolves this invocation's host mode once (design E4):
// detection from the saved daemon launch, the --host-mode assertion and the
// journal must agree (ResolveHostMode). For a co-located mode it then checks
// the host's evidence (ColocatedHostCheck): a saved legacy broker
// credentials file, or a named Hub endpoint that is not local, refuses. Both
// run before any apply or rollback step, so a refusal writes nothing and
// leaves no journaled mode. It returns the host, the mode and its source.
func convertHost(globalDir string) (colocatedHost, string, string, error) {
	host, derr := convertDetectHost(globalDir)
	detected := ""
	if derr == nil {
		detected = brokerconvert.HostModeRemote
		if host.Colocated {
			detected = brokerconvert.HostModeColocated
		}
	}
	j, err := brokerconvert.LoadJournal(globalDir)
	if err != nil {
		return colocatedHost{}, "", "", brokerconvertRefusal(brokerconvert.RefuseLocalWrite)
	}
	mode, source, ref := brokerconvert.ResolveHostMode(detected, brokerConvertHostMode, j)
	if ref != nil {
		return colocatedHost{}, "", "", ref
	}
	launched := derr == nil && host.ServerLaunch
	if !launched {
		// No saved server launch backs the mode: the settings describe the host.
		cfg, err := config.LoadGlobalConfig("")
		if err != nil || cfg == nil {
			return colocatedHost{}, "", "", brokerconvertRefusal(brokerconvert.RefuseColocatedUnknown)
		}
		host = colocatedHost{Config: cfg}
		if mode == brokerconvert.HostModeColocated {
			host.Colocated = true
			host.Listeners = serverListenersFromArgs(nil, cfg, config.LoadServerMode(), func() error { return nil })
		}
	}
	if mode == brokerconvert.HostModeColocated {
		ev := hostEvidence(globalDir, host, source, launched)
		if j != nil && j.State != brokerconvert.JournalRolledBack && j.HostMode != "" {
			ev.Source = brokerconvert.HostModeJournal
		}
		if ref := brokerconvert.ColocatedHostCheck(mode, ev); ref != nil {
			return colocatedHost{}, "", "", ref
		}
	}
	return host, mode, source, nil
}

// hostEvidence gathers what this host shows about where its Hub is: a saved
// legacy broker credentials file (broker-credentials.json, or any entry in
// hub-credentials/; the co-located legacy path keeps its credentials in
// memory and never saves one), the settings' Hub endpoint, the server
// settings' explicit server.hub.endpoint, and, only when a saved server
// launch backs the mode, that launch's Hub and web listener URLs.
func hostEvidence(globalDir string, host colocatedHost, source string, launched bool) brokerconvert.HostEvidence {
	ev := brokerconvert.HostEvidence{Source: source, Named: []string{invocationHubEndpoint()}}
	ev.Configured = serverConfiguredHubEndpoint(host.Config)
	legacy := filepath.Join(globalDir, brokercredentials.DefaultFileName)
	if _, err := os.Lstat(legacy); !errors.Is(err, fs.ErrNotExist) {
		ev.LegacyCredentials = legacy
	} else if entries, err := os.ReadDir(filepath.Join(globalDir, brokercredentials.DefaultMultiDirName)); err == nil && len(entries) > 0 {
		ev.LegacyCredentials = filepath.Join(globalDir, brokercredentials.DefaultMultiDirName)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		ev.LegacyCredentials = filepath.Join(globalDir, brokercredentials.DefaultMultiDirName)
	}
	ev.LaunchFile = filepath.Join(globalDir, daemon.ArgsFileName(serverDaemonComponent))
	if launched && host.Colocated {
		base := func(h string, port int) string { return strings.TrimSuffix(healthzURL(h, port), "/healthz") }
		ev.Listeners = []string{base(host.Listeners.HubHost, host.Listeners.WebPort), base(host.Listeners.HubHost, host.Listeners.HubPort)}
	}
	return ev
}

// planHostMode returns the fixed host-mode token (`colocated`, `remote`, or
// `unknown`) for `--plan` JSON and human output (ptone/scion#7627). Only
// compile-time constants are returned, so no path, endpoint, or settings value
// can ever leak through it.
func planHostMode(globalDir string) string {
	host, err := convertDetectHost(globalDir)
	switch {
	case err != nil:
		return brokerconvert.HostModeUnknown
	case host.Colocated:
		return brokerconvert.HostModeColocated
	default:
		return brokerconvert.HostModeRemote
	}
}

// planHostModeLine is the human --plan line: the detected host mode, or
// that --host-mode is required.
func planHostModeLine(globalDir string) string {
	switch mode := planHostMode(globalDir); mode {
	case brokerconvert.HostModeColocated, brokerconvert.HostModeRemote:
		return "Host mode: " + mode
	default:
		return "Host mode: unknown: --host-mode required"
	}
}

// brokerconvertRefusal is a refusal with its fixed text.
func brokerconvertRefusal(code string) *brokerconvert.Refusal {
	return &brokerconvert.Refusal{Code: code, Text: brokerconvert.RefusalText(code)}
}

// newColocatedEnv builds the co-located host seams for one invocation.
func newColocatedEnv(env *brokerconvert.Env, host colocatedHost) *brokerconvert.ColocatedEnv {
	ce := &brokerconvert.ColocatedEnv{
		ServerUp: func(ctx context.Context) bool {
			return localServerAnswers(ctx, env.GlobalDir, host.Listeners)
		},
		Q7: func(ctx context.Context, j *brokerconvert.Journal) brokerconvert.Q7Input {
			return colocatedQ7Input(ctx, host.Config, j)
		},
		SingleHubReplica: brokerConvertSingleHubReplica,
		InstanceOnline: func(ctx context.Context, brokerID string) (bool, error) {
			if env.Client == nil {
				return false, errHubNotConfigured
			}
			row, err := env.Client.RuntimeBrokers().Get(ctx, brokerID)
			if err != nil {
				return false, err
			}
			return row != nil && strings.EqualFold(row.Status, "online"), nil
		},
		RemoveCredentials: func(key string) error {
			return removeInstanceCredentials(env.GlobalDir, key)
		},
	}
	if convertColocatedEnvHook != nil {
		convertColocatedEnvHook(ce)
	}
	return ce
}

// colocatedQ7Input builds the stopped phase's drained-check input from the
// server settings: the store topology, the read-only SQLite count for a
// local store, and the dark-endpoint probe of the Hub endpoints for an
// external one. No database credential is read for an external store.
func colocatedQ7Input(ctx context.Context, cfg *config.GlobalConfig, j *brokerconvert.Journal) brokerconvert.Q7Input {
	in := brokerconvert.Q7Input{Topology: colocatedStoreTopology(cfg, nil)}
	if cfg == nil || j == nil {
		return in
	}
	legacyID := j.LegacyBrokerID
	if in.Topology == brokerconvert.TopologySingleSQLite {
		dsn := cfg.Database.URL
		in.LegacyStore = func() (brokerconvert.LegacyStoreFacts, error) {
			return legacyStoreSQLite(ctx, dsn, legacyID)
		}
	}
	if in.Topology == brokerconvert.TopologySingleExternal {
		endpoints := []string{serverConfiguredHubEndpoint(cfg), invocationHubEndpoint()}
		in.EndpointAnswers = func() (bool, error) {
			return darkEndpointProbe(ctx, endpoints)
		}
	}
	return in
}

// errColocatedAssertedHubEndpoint refuses a co-located precheck whose host
// mode is asserted without a saved server launch and no configured
// server.hub.endpoint.
var errColocatedAssertedHubEndpoint = errors.New("no saved server launch; set server.hub.endpoint")

// colocatedPrecheckHost is the shared activation precheck's host for a
// co-located host's convert: ModeColocated, with the project storage
// layout and the Hub endpoint resolved as the co-located server derives it
// (brokerStorageLayout, colocatedResolveHubEndpoint). Settings that do
// not load leave the layout unusable, so a cloudrun instance is refused.
func colocatedPrecheckHost(host colocatedHost, settings *config.Settings) flatPrecheckHost {
	hc := flatPrecheckHost{mode: brokerhost.ModeColocated}
	vs, _, err := config.LoadEffectiveSettings("")
	switch {
	case err != nil:
		hc.storageErr = err
	case vs == nil:
		hc.storageErr = errors.New("no effective settings")
	default:
		hc.layout, _, hc.storageErr = brokerStorageLayout(vs)
	}
	hc.hubEndpoint, hc.hubEndpointSrc, hc.hubEndpointErr = colocatedResolveHubEndpoint(host, settings)
	return hc
}

// colocatedResolveHubEndpoint derives the Hub endpoint a co-located convert's
// shared precheck validates (ptone/scion#7714). When a saved server launch
// backs the host, it resolves with the server's own resolver from that
// launch's components, ports and --base-url. When the mode is asserted with
// no saved launch, only the configured server.hub.endpoint is used, and a
// missing value fails with a "set server.hub.endpoint" message.
func colocatedResolveHubEndpoint(host colocatedHost, settings *config.Settings) (string, hubEndpointSource, error) {
	if !host.ServerLaunch {
		ep := serverConfiguredHubEndpoint(host.Config)
		if ep == "" {
			return "", hubEndpointSourceNone, errColocatedAssertedHubEndpoint
		}
		return ep, hubEndpointSourceConfig, nil
	}
	if host.Config == nil {
		return "", hubEndpointSourceNone, errColocatedUndetermined
	}
	ep, src := host.Listeners.resolveHubEndpoint(host.Config, settings)
	return ep, src, nil
}

// colocatedPlansLocally reports whether this co-located apply runs a phase
// that does not reach the Hub, so its plan is built locally without the
// Hub sections: the stopped phase and the start-pending note (this host's
// server, which is the Hub, is stopped), or a phase refusal the engine
// reports itself. Every other apply collects the Hub sections first.
func colocatedPlansLocally(ctx context.Context, env *brokerconvert.Env, host colocatedHost) bool {
	if !host.Colocated {
		return false
	}
	j, err := brokerconvert.LoadJournal(env.GlobalDir)
	if err != nil {
		return true // the engine refuses an unreadable journal
	}
	phase, ref := brokerconvert.NextColocatedApplyPhase(j, newColocatedEnv(env, host).ServerUp(ctx))
	if ref != nil {
		return true
	}
	return phase == brokerconvert.ColocatedStopped || phase == brokerconvert.ColocatedStartPending
}

// convertApply runs the apply for this host: the co-located phases on a
// co-located host, PR1's apply otherwise.
func convertApply(ctx context.Context, env *brokerconvert.Env, host colocatedHost, in brokerconvert.ApplyInput) (*brokerconvert.Report, error) {
	if host.Colocated {
		return brokerconvert.ApplyColocated(ctx, env, newColocatedEnv(env, host), in)
	}
	if brokerConvertSingleHubReplica {
		ref := brokerconvertRefusal(brokerconvert.RefuseSingleHubMisplaced)
		return &brokerconvert.Report{Mode: brokerconvert.ModeApply, Instances: []brokerconvert.InstanceLine{}, Projects: []brokerconvert.ProjectLine{}, Notes: []string{}, Refusal: ref}, ref
	}
	return brokerconvert.Apply(ctx, env, in)
}

// convertRollback runs the rollback for this host.
func convertRollback(ctx context.Context, env *brokerconvert.Env, host colocatedHost) (*brokerconvert.Report, error) {
	if host.Colocated {
		return brokerconvert.RollbackColocated(ctx, env, newColocatedEnv(env, host))
	}
	return brokerconvert.Rollback(ctx, env)
}
