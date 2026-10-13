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
	"errors"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// TestRuntimeBrokerInstanceHosting_SimulatedRemoteRefused: hubInProcess is
// exactly colocatedBrokerRegisters (.design/flat-runtime-brokers-contract.md
// R10), so --simulate-remote-broker (Hub in process, no embedded
// registration) refuses flat instances like a remote host does.
func TestRuntimeBrokerInstanceHosting_SimulatedRemoteRefused(t *testing.T) {
	origHub, origSim := enableHub, simulateRemoteBroker
	t.Cleanup(func() { enableHub, simulateRemoteBroker = origHub, origSim })
	s := newTestStore(t)
	cfg := &config.GlobalConfig{RuntimeBroker: config.RuntimeBrokerConfig{Enabled: true}}
	inst := []config.V1RuntimeBrokerInstanceConfig{{Key: "local-docker", Name: "example-docker",
		RuntimeTarget: &config.V1RuntimeTargetConfig{Type: "docker"}}}

	enableHub, simulateRemoteBroker = true, true
	err := config.CheckRuntimeBrokerInstanceHosting(inst, colocatedBrokerRegisters(cfg, s))
	var he *config.RuntimeBrokerHostingError
	if !errors.As(err, &he) || he.Code() != api.ErrCodeFlatRuntimeBrokerRemoteUnsupported {
		t.Fatalf("simulated remote must be refused, got %v", err)
	}

	enableHub, simulateRemoteBroker = true, false
	if err := config.CheckRuntimeBrokerInstanceHosting(inst, colocatedBrokerRegisters(cfg, s)); err != nil {
		t.Fatalf("co-located hosting must be allowed: %v", err)
	}

	enableHub = false
	if err := config.CheckRuntimeBrokerInstanceHosting(inst, colocatedBrokerRegisters(cfg, s)); err == nil {
		t.Fatal("a Runtime Broker without the Hub in process must be refused")
	}
	if err := config.CheckRuntimeBrokerInstanceHosting(nil, colocatedBrokerRegisters(cfg, s)); err != nil {
		t.Fatalf("legacy remote hosting is unchanged: %v", err)
	}
}
