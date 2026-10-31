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

package hub

import (
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
)

// TestBrokerRequestTimeout_OneLimit pins the hub-to-broker request limit at
// 180s (ptone/scion#6829) and that every place that applies it uses the one
// constant: the broker HTTP clients, the control channel default (also the
// conduit tunnels' default) and syncDispatchTimeout, the bound on one
// synchronous launch dispatch. A budget sized for two broker round trips
// must still hold both.
func TestBrokerRequestTimeout_OneLimit(t *testing.T) {
	if brokerRequestTimeout != 180*time.Second {
		t.Fatalf("brokerRequestTimeout = %v, want 180s", brokerRequestTimeout)
	}
	tr := newBrokerHTTPTransport(false, nil)
	if tr.client.Timeout != brokerRequestTimeout {
		t.Errorf("broker HTTP client timeout = %v, want %v", tr.client.Timeout, brokerRequestTimeout)
	}
	if tr.keysClient.Timeout != brokerRequestTimeout {
		t.Errorf("broker keys client timeout = %v, want %v", tr.keysClient.Timeout, brokerRequestTimeout)
	}
	if got := DefaultControlChannelConfig().RequestTimeout; got != brokerRequestTimeout {
		t.Errorf("control channel default RequestTimeout = %v, want %v", got, brokerRequestTimeout)
	}
	if got := newConduitBrokerTunnel(nil, 0).timeout; got != brokerRequestTimeout {
		t.Errorf("conduit tunnel default timeout = %v, want %v", got, brokerRequestTimeout)
	}
	if syncDispatchTimeout != brokerRequestTimeout {
		t.Errorf("syncDispatchTimeout = %v, want %v: the dispatch bound must equal the request limit, or it fires first", syncDispatchTimeout, brokerRequestTimeout)
	}
	if got := (ControlChannelConfig{}).requestTimeout(); got != brokerRequestTimeout {
		t.Errorf("control channel zero RequestTimeout falls back to %v, want %v", got, brokerRequestTimeout)
	}
	if got := (ControlChannelConfig{RequestTimeout: 7 * time.Second}).requestTimeout(); got != 7*time.Second {
		t.Errorf("an explicit RequestTimeout = %v, want 7s kept", got)
	}
	if reincarnationRerenderTimeout < 2*brokerRequestTimeout+time.Minute {
		t.Errorf("reincarnationRerenderTimeout = %v, want at least two broker round trips plus a minute (%v)", reincarnationRerenderTimeout, 2*brokerRequestTimeout+time.Minute)
	}
}

// TestBrokerRequestTimeout_BrokerWriteTimeoutOutlastsIt pins that the broker
// HTTP server's default write timeout leaves at least 30s above the
// hub-to-broker request limit (ptone/scion#6829): broker handlers do not
// extend their write deadline, so a lower default would cut off a direct-HTTP
// answer the hub is still waiting for. Both the broker package default and the
// settings default must hold it.
func TestBrokerRequestTimeout_BrokerWriteTimeoutOutlastsIt(t *testing.T) {
	floor := brokerRequestTimeout + 30*time.Second
	if got := runtimebroker.DefaultServerConfig().WriteTimeout; got < floor {
		t.Errorf("runtimebroker default WriteTimeout = %v, want at least %v", got, floor)
	}
	if got := config.DefaultGlobalConfig().RuntimeBroker.WriteTimeout; got < floor {
		t.Errorf("settings default server.broker.write_timeout = %v, want at least %v", got, floor)
	}
}
