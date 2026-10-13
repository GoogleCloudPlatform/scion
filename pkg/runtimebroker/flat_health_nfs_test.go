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

package runtimebroker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// TestFlatInstanceHealth_NFSCodesOnly: a flat instance's health reports an
// unhealthy share by code, never with the mount message (which can name
// local paths); a legacy Runtime Broker keeps the message.
func TestFlatInstanceHealth_NFSCodesOnly(t *testing.T) {
	const detail = "mount.nfs: access denied for /srv/private/export"
	for _, flat := range []bool{false, true} {
		mc := newSyncMountChecker()
		mc.mountErr = errors.New(detail)
		cfg := ServerConfig{Host: "127.0.0.1", Port: 0, NFSConfig: nfsCfg(true), NFSMountChecker: mc, StateDir: t.TempDir()}
		if flat {
			cfg.BrokerID = "rb-flat"
			cfg.FlatInstance = &FlatInstanceConfig{Identity: &brokeridentity.Identity{RuntimeBrokerID: "rb-flat"}, HubInProcess: true}
		}
		var mgr agent.Manager
		if flat {
			mgr = &mockManager{} // a flat instance needs an owner-scoped manager
		}
		srv := New(cfg, mgr, &runtime.MockRuntime{NameFunc: func() string { return "mock" }})
		startTestBroker(t, srv)
		waitClosed(t, srv.nfsStartupReconcileDone, "first reconcile pass")

		got := srv.GetHealthInfo(context.Background()).Checks["nfs_mounts"]
		if flat {
			if strings.Contains(got, "access denied") || strings.Contains(got, "/srv/private") {
				t.Errorf("flat instance health carries the mount message: %q", got)
			}
			if !strings.HasPrefix(got, "unhealthy: ") || !strings.Contains(got, ": unhealthy") {
				t.Errorf("flat instance nfs_mounts = %q, want per-share codes", got)
			}
		} else if !strings.Contains(got, "access denied") {
			t.Errorf("legacy nfs_mounts = %q, want the mount message unchanged", got)
		}
	}
}
