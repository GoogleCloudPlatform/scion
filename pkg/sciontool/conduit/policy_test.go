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

package conduit

import (
	"context"
	"net"
	"slices"
	"testing"

	core "github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/conduit/localpolicy"
)

func mustPolicy(t *testing.T, raw string) localpolicy.Policy {
	t.Helper()
	p, err := localpolicy.ParsePolicy(raw)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestAgentTCPPolicyDeniedPort: refused with 4403 before any dial.
func TestAgentTCPPolicyDeniedPort(t *testing.T) {
	key := newTestKey(t, "k1")
	h := newFakeHub(t, key.public)
	startAgent(t, h, func(o *Options) {
		o.Policy = mustPolicy(t, `{"deny_ports":[9999]}`)
		o.DialLocal = func(_ context.Context, _, addr string) (net.Conn, error) {
			t.Errorf("DialLocal(%s) called for a denied port", addr)
			return nil, net.ErrClosed
		}
	})
	s := h.nextSession(t)
	_, err := s.OpenStream(context.Background(), tcpOpen(t, key, s.Info(), "launch-1", 9999))
	wantRefusal(t, err, core.CloseForbidden, reasonForbidden)
}

func TestAgentSSHDisabledByPolicy(t *testing.T) {
	key := newTestKey(t, "k1")
	h := newFakeHub(t, key.public)
	startAgent(t, h, func(o *Options) {
		o.NoPTY = true
		o.SSH = testSSHConfig(t)
		o.Policy = mustPolicy(t, `{"disable_ssh":true}`)
	})
	if got := h.nextHello(t).GetCapabilities().GetStreamKinds(); slices.Contains(got, grant.StreamKindSSH) {
		t.Fatalf("stream kinds = %v, want no ssh", got)
	}
	s := h.nextSession(t)
	_, err := s.OpenStream(context.Background(), sshOpen(t, key, s.Info(), grant.StreamKindSSH))
	wantRefusal(t, err, core.CloseProtocolError, reasonUnsupportedKind)
}

func TestAgentSSHGetsPolicy(t *testing.T) {
	ssh := testSSHConfig(t)
	_, err := New(Options{
		HubURL: "http://hub.test", AgentID: "a", ProjectID: "p", NoPTY: true,
		Token:  func() string { return "t" },
		SSH:    ssh,
		Policy: mustPolicy(t, `{"deny_ports":[9999]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ssh.Policy.PortDenied(9999) {
		t.Fatal("SSH server did not get the agent's policy")
	}
}
