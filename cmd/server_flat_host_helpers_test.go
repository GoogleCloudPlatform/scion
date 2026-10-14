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
	"context"
	"errors"
	"path/filepath"
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/brokerhost"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// flatInstanceStartup is one co-located flat instance activated by the host.
type flatInstanceStartup struct {
	identity *brokeridentity.Identity
	instance config.V1RuntimeBrokerInstanceConfig
	row      *store.RuntimeBroker
}

// recordingActivator wraps the production co-located activator and keeps
// each instance's refusal.
type recordingActivator struct {
	*colocatedFlatActivator
	mu      sync.Mutex
	refused map[string]error
}

func (a *recordingActivator) Refused(inst config.V1RuntimeBrokerInstanceConfig, err error) {
	a.mu.Lock()
	if a.refused == nil {
		a.refused = map[string]error{}
	}
	a.refused[inst.Key] = err
	a.mu.Unlock()
	a.colocatedFlatActivator.Refused(inst, err)
}

// prepareFlatInstance activates one co-located flat instance through the
// production host (brokerhost.Host with the co-located activator and the
// production runtime factory), with probe standing in for the Docker scope
// probe. It returns the activated identity and stored row, or the refusal.
func prepareFlatInstance(ctx context.Context, hubSrv *hub.Server, _ *runtime.DockerRuntime, inst config.V1RuntimeBrokerInstanceConfig, legacyIDs []string, globalDir string, opts hub.EmbeddedFlatRegistrationOptions, probe func(context.Context, string) (brokeridentity.ExecutionScope, error)) (*flatInstanceStartup, error) {
	act := &recordingActivator{colocatedFlatActivator: &colocatedFlatActivator{hubSrv: hubSrv, endpoint: opts.Endpoint, autoProvide: opts.AutoProvide}}
	h, err := brokerhost.New(brokerhost.Config{
		GlobalDir:  globalDir,
		Instances:  []config.V1RuntimeBrokerInstanceConfig{inst},
		Mode:       brokerhost.ModeColocated,
		LegacyIDs:  legacyIDs,
		NewRuntime: newFlatInstanceRuntime,
		ProbeScope: func(ctx context.Context, _ config.V1RuntimeBrokerInstanceConfig, rt runtime.Runtime) (brokeridentity.ExecutionScope, error) {
			return probe(ctx, rt.(*runtime.DockerRuntime).Command)
		},
		Activator: act,
		BuildServer: func(ic brokerhost.InstanceContext) (*runtimebroker.Server, error) {
			cfg := runtimebroker.DefaultServerConfig()
			cfg.BrokerID = ic.Identity.RuntimeBrokerID
			cfg.StateDir = filepath.Join(globalDir, "state", ic.Identity.RuntimeBrokerID)
			cfg.FlatInstance = &runtimebroker.FlatInstanceConfig{Identity: ic.Identity, Instance: ic.Instance, HubInProcess: true}
			return runtimebroker.New(cfg, ic.Manager, ic.Runtime), nil
		},
	})
	if err != nil {
		return nil, err
	}
	if err := h.Prepare(ctx); err != nil {
		return nil, err
	}
	if err := act.refused[inst.Key]; err != nil {
		return nil, err
	}
	active := h.Active()
	if len(active) != 1 {
		return nil, errors.New("flat instance not activated")
	}
	id := active[0].Context.Identity
	row, err := hubSrv.GetStore().GetRuntimeBroker(ctx, id.RuntimeBrokerID)
	if err != nil {
		return nil, err
	}
	return &flatInstanceStartup{identity: id, instance: inst, row: row}, nil
}
