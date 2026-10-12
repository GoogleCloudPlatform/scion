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

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestAuthorizeConduitTCPTarget_AllowedPortsSemantics pins the meaning of
// ConduitTCPAllowedPorts (server.hub.conduit.tcp_allowed_ports): extra
// ports besides the agent's exposed ports. Unset means exposed ports only,
// and the reserved ports are refused even when listed.
func TestAuthorizeConduitTCPTarget_AllowedPortsSemantics(t *testing.T) {
	f := newConduitFixture(t) // the agent exposes 3000
	tests := []struct {
		name    string
		allowed []int
		port    int
		ok      bool
	}{
		{name: "unset: exposed port", port: 3000, ok: true},
		{name: "unset: any other port refused", port: 8080},
		{name: "unset: high port refused", port: 65000},
		{name: "listed: exposed port still allowed", allowed: []int{8080}, port: 3000, ok: true},
		{name: "listed: extra port allowed", allowed: []int{8080}, port: 8080, ok: true},
		{name: "listed: unlisted port refused", allowed: []int{8080}, port: 4000},
		{name: "listed: reserved hub port refused", allowed: []int{9810}, port: 9810},
		{name: "listed: reserved metadata port refused", allowed: []int{18380}, port: 18380},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f.srv.config.ConduitTCPAllowedPorts = tt.allowed
			err := f.srv.authorizeConduitTCPTarget(f.agent, tt.port)
			if tt.ok {
				assert.NoError(t, err)
			} else {
				assert.ErrorIs(t, err, errConduitForbidden)
			}
		})
	}
}
