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
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/lifecycleevents"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// sinkTestGlobals saves and restores the package state the lifecycle sink
// wiring reads (lifecycleCloudHandler, lifecycleMeterProvider,
// newLifecycleCloudSink, enableRuntimeBroker). Tests that use it are not
// parallel.
func sinkTestGlobals(t *testing.T) {
	t.Helper()
	h, mp, seam, broker := lifecycleCloudHandler, lifecycleMeterProvider, newLifecycleCloudSink, enableRuntimeBroker
	t.Cleanup(func() {
		lifecycleCloudHandler, lifecycleMeterProvider, newLifecycleCloudSink, enableRuntimeBroker = h, mp, seam, broker
	})
	t.Setenv("K_SERVICE", "")
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
}

func sinkTestConfig(hubHost, brokerHost, driver string) *config.GlobalConfig {
	cfg := &config.GlobalConfig{}
	cfg.Hub.Host = hubHost
	cfg.RuntimeBroker.Host = brokerHost
	cfg.Database.Driver = driver
	return cfg
}

// TestLifecycleSinkEnvironment covers every input lifecycleSinkEnvironment
// gathers for the sink selection (design section 22 item 4).
func TestLifecycleSinkEnvironment(t *testing.T) {
	for _, tt := range []struct {
		name       string
		handler    bool
		withBroker bool
		hubHost    string
		brokerHost string
		driver     string
		devAuth    bool
		kService   string
		k8s        string
		want       lifecycleevents.SinkEnvironment
	}{
		{name: "workstation: loopback hub, sqlite, dev auth", hubHost: "127.0.0.1", driver: "sqlite", devAuth: true,
			want: lifecycleevents.SinkEnvironment{LoopbackOnly: true, SQLite: true, DevAuth: true}},
		{name: "localhost counts as loopback; driver case-insensitive", hubHost: "localhost", driver: "SQLite",
			want: lifecycleevents.SinkEnvironment{LoopbackOnly: true, SQLite: true}},
		{name: "co-located broker on loopback keeps loopback", withBroker: true, hubHost: "127.0.0.1", brokerHost: "::1", driver: "sqlite",
			want: lifecycleevents.SinkEnvironment{LoopbackOnly: true, SQLite: true}},
		{name: "co-located broker off loopback clears it", withBroker: true, hubHost: "127.0.0.1", brokerHost: "0.0.0.0", driver: "sqlite",
			want: lifecycleevents.SinkEnvironment{SQLite: true}},
		{name: "broker host ignored without a co-located broker", hubHost: "127.0.0.1", brokerHost: "0.0.0.0", driver: "sqlite",
			want: lifecycleevents.SinkEnvironment{LoopbackOnly: true, SQLite: true}},
		{name: "public hub host, postgres", hubHost: "0.0.0.0", driver: "postgres",
			want: lifecycleevents.SinkEnvironment{}},
		{name: "cloud logging handler present", handler: true, hubHost: "0.0.0.0", driver: "postgres",
			want: lifecycleevents.SinkEnvironment{CloudLogging: true}},
		{name: "cloud run service", hubHost: "0.0.0.0", driver: "postgres", kService: "scion-hub",
			want: lifecycleevents.SinkEnvironment{CloudRunService: true}},
		{name: "kubernetes", hubHost: "127.0.0.1", driver: "sqlite", devAuth: true, k8s: "10.0.0.1",
			want: lifecycleevents.SinkEnvironment{LoopbackOnly: true, SQLite: true, DevAuth: true, Kubernetes: true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sinkTestGlobals(t)
			lifecycleCloudHandler = nil
			if tt.handler {
				lifecycleCloudHandler = &logging.ResilientCloudHandler{}
			}
			enableRuntimeBroker = tt.withBroker
			t.Setenv("K_SERVICE", tt.kService)
			t.Setenv("KUBERNETES_SERVICE_HOST", tt.k8s)
			got := lifecycleSinkEnvironment(sinkTestConfig(tt.hubHost, tt.brokerHost, tt.driver), tt.devAuth)
			assert.Equal(t, tt.want, got)
		})
	}
}
