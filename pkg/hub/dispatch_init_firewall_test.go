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
	"context"
	"log/slog"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/firewallpolicy"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHubNew_RejectsInvalidInitFirewall(t *testing.T) {
	_, err := New(ServerConfig{
		InitFirewall: firewallpolicy.Policy{
			AllowDomains: []string{"bad;name"},
		},
	}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "server.hub.init_firewall")
	assert.Contains(t, err.Error(), "bad;name")
}

// Each send wrapper unconditionally overwrites initFirewall from the hub's
// operator-authoritative policy: any value pre-populated on the request or
// StartExtras (such as evil.example) is replaced by the hub's policy, or by
// nil when the hub has no policy configured.
func TestSendWrappersOverwriteInitFirewall(t *testing.T) {
	ctx := context.Background()
	agent := &store.Agent{
		ID:              tid("agent-fw-seal"),
		RuntimeBrokerID: tid("broker-fw-seal"),
		AppliedConfig:   &store.AgentAppliedConfig{},
	}

	t.Run("overwrites pre-populated value with hub policy", func(t *testing.T) {
		m := &mockRuntimeBrokerClient{}
		d := NewHTTPAgentDispatcherWithClient(nil, m, false, slog.Default())
		hubPolicy, err := firewallpolicy.Normalize([]string{"artifactory.corp.example", "pypi.org"}, true)
		require.NoError(t, err)
		d.SetInitFirewall(func() firewallpolicy.Policy { return hubPolicy })

		want := &RemoteInitFirewall{
			AllowDomains: []string{"artifactory.corp.example", "pypi.org"},
			AllowSSH:     true,
		}
		evil := func() *RemoteInitFirewall {
			return &RemoteInitFirewall{AllowDomains: []string{"evil.example"}, AllowSSH: false}
		}

		req := &RemoteCreateAgentRequest{InitFirewall: evil()}
		_, _ = d.sendCreate(ctx, agent, "http://broker", req)
		require.NotNil(t, m.lastCreateReq)
		assert.Equal(t, want, m.lastCreateReq.InitFirewall)

		// Mutating the sent slice must not mutate the hub's policy.
		m.lastCreateReq.InitFirewall.AllowDomains[0] = "mutated.example"

		reqGather := &RemoteCreateAgentRequest{InitFirewall: evil()}
		_, _, _ = d.sendCreateWithGather(ctx, agent, "http://broker", reqGather)
		assert.Equal(t, want, reqGather.InitFirewall)

		_, _ = d.sendStart(ctx, agent, "http://broker", "a", "p", "", "", "", "", "", "", map[string]string{}, nil, nil, nil, false, false, StartExtras{InitFirewall: evil()})
		assert.Equal(t, want, m.lastStartExtras.InitFirewall)

		_, _ = d.sendRestart(ctx, agent, "http://broker", "a", "p", map[string]string{}, StartExtras{InitFirewall: evil()})
		assert.Equal(t, want, m.lastRestartExtras.InitFirewall)
	})

	t.Run("clears pre-populated value when hub has no policy", func(t *testing.T) {
		m := &mockRuntimeBrokerClient{}
		d := NewHTTPAgentDispatcherWithClient(nil, m, false, slog.Default())
		evil := func() *RemoteInitFirewall {
			return &RemoteInitFirewall{AllowDomains: []string{"evil.example"}, AllowSSH: true}
		}

		req := &RemoteCreateAgentRequest{InitFirewall: evil()}
		_, _ = d.sendCreate(ctx, agent, "http://broker", req)
		require.NotNil(t, m.lastCreateReq)
		assert.Nil(t, m.lastCreateReq.InitFirewall)

		reqGather := &RemoteCreateAgentRequest{InitFirewall: evil()}
		_, _, _ = d.sendCreateWithGather(ctx, agent, "http://broker", reqGather)
		assert.Nil(t, reqGather.InitFirewall)

		_, _ = d.sendStart(ctx, agent, "http://broker", "a", "p", "", "", "", "", "", "", map[string]string{}, nil, nil, nil, false, false, StartExtras{InitFirewall: evil()})
		assert.Nil(t, m.lastStartExtras.InitFirewall)

		_, _ = d.sendRestart(ctx, agent, "http://broker", "a", "p", map[string]string{}, StartExtras{InitFirewall: evil()})
		assert.Nil(t, m.lastRestartExtras.InitFirewall)
	})
}

func TestApplyStartExtras_InitFirewall(t *testing.T) {
	empty := map[string]interface{}{}
	applyStartExtras(empty, StartExtras{})
	_, present := empty["initFirewall"]
	assert.False(t, present, "nil InitFirewall must not be emitted")

	fw := &RemoteInitFirewall{
		AllowDomains: []string{"artifactory.corp.example"},
		AllowSSH:     true,
	}
	payload := map[string]interface{}{}
	applyStartExtras(payload, StartExtras{InitFirewall: fw})
	assert.Equal(t, fw, payload["initFirewall"])
}
