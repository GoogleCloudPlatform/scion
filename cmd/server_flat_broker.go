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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Co-located flat Runtime Broker startup (.design/flat-runtime-brokers-contract.md
// sections 2, 4, 5 and R10). A process with server.broker.instances hosts
// only the flat instance: it never loads legacy Runtime Broker credentials,
// never runs the legacy embedded registration or orphan reassignment, and
// never resolves Runtime Broker Profiles for the instance.

// loadServerRuntimeBrokerInstances strictly loads server.broker.instances and
// refuses startup when the strict result differs from what the lenient server
// config loader produced (that difference means a silent fallback was taken).
func loadServerRuntimeBrokerInstances(cfg *config.GlobalConfig, configPath string) ([]config.V1RuntimeBrokerInstanceConfig, error) {
	instances, err := config.LoadRuntimeBrokerInstances(configPath)
	if err != nil {
		return nil, fmt.Errorf("server.broker.instances: %w", err)
	}
	strict := config.RuntimeBrokerInstancesToGlobal(instances)
	var loaded []config.RuntimeBrokerInstanceConfig
	if cfg != nil {
		loaded = cfg.RuntimeBroker.Instances
	}
	if len(strict) == 0 && len(loaded) == 0 {
		return nil, nil
	}
	if !reflect.DeepEqual(strict, loaded) {
		return nil, errors.New("server.broker.instances: the strictly loaded instances differ from the server configuration; check settings.yaml for a server section that failed to load")
	}
	return instances, nil
}

// probeDockerExecutionScope probes the Docker execution scope through the
// same CLI command the DockerRuntime uses, so the active docker context,
// DOCKER_CONTEXT and DOCKER_HOST are honoured exactly as the runtime honours
// them. The daemon ID is the identity; the endpoint is informational.
func probeDockerExecutionScope(ctx context.Context, command string) (brokeridentity.ExecutionScope, error) {
	if command == "" {
		command = "docker"
	}
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(probeCtx, command, "info", "--format", "{{.ID}}").Output()
	daemonID := strings.TrimSpace(string(out))
	if err != nil || daemonID == "" {
		return brokeridentity.ExecutionScope{}, fmt.Errorf("%w: %s info returned no daemon ID: %v", brokeridentity.ErrExecutionScopeUnidentified, command, err)
	}
	endpoint := ""
	if epOut, epErr := exec.CommandContext(probeCtx, command, "context", "inspect", "--format", "{{.Endpoints.docker.Host}}").Output(); epErr == nil {
		endpoint = brokeridentity.NormalizeDockerEndpoint(strings.TrimSpace(string(epOut)))
	}
	return brokeridentity.ExecutionScope{
		Type:   brokeridentity.TargetTypeDocker,
		Docker: &brokeridentity.DockerScope{DaemonID: daemonID, Endpoint: endpoint},
	}, nil
}

// legacyRuntimeBrokerIDs lists every legacy Runtime Broker identity source
// visible to this process, read-only, so a flat identity can never collide
// with one. Credential files are read here only for their broker IDs; a flat
// process never hosts them.
func legacyRuntimeBrokerIDs(cfg *config.GlobalConfig, settings *config.Settings, vsBroker *config.V1BrokerConfig, globalDir string) []string {
	var ids []string
	add := func(id string) {
		if id != "" {
			ids = append(ids, id)
		}
	}
	if vsBroker != nil {
		add(vsBroker.BrokerID)
	}
	if settings != nil && settings.Hub != nil {
		add(settings.Hub.BrokerID)
	}
	if cfg != nil {
		add(cfg.RuntimeBroker.BrokerID)
	}
	if globalDir != "" {
		if creds, err := brokercredentials.NewStore(filepath.Join(globalDir, "broker-credentials.json")).Load(); err == nil && creds != nil {
			add(creds.BrokerID)
		}
		if list, err := brokercredentials.NewMultiStore(filepath.Join(globalDir, "hub-credentials")).List(); err == nil {
			for _, c := range list {
				add(c.BrokerID)
			}
		}
	}
	return ids
}

// warnUnhostedFlatIdentities is the startup safety net for a process with no
// instance configured: it warns, naming the flat Runtime Broker IDs whose
// identities exist on disk but are not hosted. It does not refuse, so a
// deliberate rollback to legacy hosting stays possible.
func warnUnhostedFlatIdentities(globalDir string) {
	if globalDir == "" {
		return
	}
	matches, _ := filepath.Glob(filepath.Join(globalDir, brokeridentity.RuntimeBrokersDirName, "*", brokeridentity.IdentityFileName))
	var unhosted []string
	for _, m := range matches {
		key := filepath.Base(filepath.Dir(m))
		var id brokeridentity.Identity
		data, err := os.ReadFile(m)
		if err == nil {
			err = json.Unmarshal(data, &id)
		}
		if err != nil || id.RuntimeBrokerID == "" {
			unhosted = append(unhosted, key+" (unreadable identity)")
			continue
		}
		unhosted = append(unhosted, fmt.Sprintf("%s (Runtime Broker %s)", key, id.RuntimeBrokerID))
	}
	if len(unhosted) > 0 {
		slog.Warn("Flat Runtime Broker identities exist but server.broker.instances is not configured; they are NOT hosted by this process and their agents cannot start until the instance is configured again",
			"instances", strings.Join(unhosted, ", "))
	}
}

// flatInstanceStartup is a flat instance that passed every startup check and
// the embedded registration's bound-result validation.
type flatInstanceStartup struct {
	runtime  *runtime.DockerRuntime
	identity *brokeridentity.Identity
	instance config.V1RuntimeBrokerInstanceConfig
	row      *store.RuntimeBroker
}

// prepareFlatInstance takes the instance's Docker runtime (constructed from
// its explicit configuration), probes and verifies its execution scope, loads or
// creates its identity, and registers it through the Hub's embedded flat
// registration. Any refusal is reported through
// EmbeddedBrokerRegistrationFailed and returned; the caller then does not
// activate the instance (no Runtime Broker server, control channel,
// heartbeat or dispatch), and nothing falls back to the legacy identity.
func prepareFlatInstance(ctx context.Context, hubSrv *hub.Server, dr *runtime.DockerRuntime, inst config.V1RuntimeBrokerInstanceConfig, legacyIDs []string, globalDir string, opts hub.EmbeddedFlatRegistrationOptions, probe func(context.Context, string) (brokeridentity.ExecutionScope, error)) (*flatInstanceStartup, error) {
	fail := func(err error) (*flatInstanceStartup, error) {
		hubSrv.EmbeddedBrokerRegistrationFailed(err)
		return nil, err
	}
	if inst.RuntimeTarget == nil || inst.RuntimeTarget.Type != brokeridentity.TargetTypeDocker {
		return fail(fmt.Errorf("flat Runtime Broker instance %q: only runtime_target.type docker is supported in this release", inst.Key))
	}
	if probe == nil {
		probe = probeDockerExecutionScope
	}
	scope, err := probe(ctx, dr.Command)
	if err != nil {
		return fail(fmt.Errorf("flat Runtime Broker instance %q: %w", inst.Key, err))
	}
	id, err := brokeridentity.LoadOrCreate(brokeridentity.InstanceDir(globalDir, inst.Key), inst.Key, inst.RuntimeTarget.Type, scope, legacyIDs)
	if err != nil {
		return fail(fmt.Errorf("flat Runtime Broker instance %q: %w", inst.Key, err))
	}
	// RegisterEmbeddedFlatRuntimeBroker reports its own refusals.
	row, err := hubSrv.RegisterEmbeddedFlatRuntimeBroker(ctx, id, inst, opts)
	if err != nil {
		return nil, err
	}
	return &flatInstanceStartup{runtime: dr, identity: id, instance: inst, row: row}, nil
}

// flatInstanceCapabilities are the embedded flat instance's capabilities on
// its Docker runtime (the same set the legacy embedded registration
// reports for its runtime).
func flatInstanceCapabilities(rt runtime.Runtime) *store.BrokerCapabilities {
	return &store.BrokerCapabilities{
		WebPTY:                 false,
		Sync:                   true,
		Attach:                 runtime.HasAttachSupport(rt),
		Reprovision:            true,
		AsyncLaunch:            true,
		EmptyPerAgentWorkspace: runtime.HasEmptyPerAgentSupport(rt),
		AgentMove:              false,
	}
}
