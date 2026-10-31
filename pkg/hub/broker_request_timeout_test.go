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
	if reincarnationRerenderTimeout < 2*brokerRequestTimeout+time.Minute {
		t.Errorf("reincarnationRerenderTimeout = %v, want at least two broker round trips plus a minute (%v)", reincarnationRerenderTimeout, 2*brokerRequestTimeout+time.Minute)
	}
}
