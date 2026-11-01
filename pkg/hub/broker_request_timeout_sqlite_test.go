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

package hub

import "testing"

// TestBrokerRequestTimeout_ServerControlChannel pins that the control channel
// a hub server builds in New applies the hub-to-broker request limit
// (ptone/scion#6829), not a separate literal.
func TestBrokerRequestTimeout_ServerControlChannel(t *testing.T) {
	srv, _ := testServer(t)
	if srv.controlChannel == nil {
		t.Fatal("the server has no control channel")
	}
	if got := srv.controlChannel.config.RequestTimeout; got != brokerRequestTimeout {
		t.Errorf("server control channel RequestTimeout = %v, want %v", got, brokerRequestTimeout)
	}
}
