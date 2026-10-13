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
	"slices"
	"strconv"
	"testing"

	core "github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/conduit/localpolicy"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/conduit/sshd"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// testSSHConfig is an SSH server configuration with a fresh host key.
func testSSHConfig(t *testing.T) *sshd.Config {
	t.Helper()
	key, err := sshd.NewHostKey()
	if err != nil {
		t.Fatal(err)
	}
	return &sshd.Config{HostKey: key, Shell: "/bin/sh"}
}

// sshOpen builds an SSH StreamOpen with a grant of grantKind.
func sshOpen(t *testing.T, key testKey, info core.SessionInfo, grantKind string) *conduitv1.StreamOpen {
	t.Helper()
	return mintOpen(t, key, info, conduitv1.StreamKind_STREAM_KIND_SSH, nil, grantKind, nil)
}

// TestAgentAdvertisesSSHOnlyWhenConfigured: ssh is in the Hello's
// stream_kinds exactly when an SSH server is configured.
func TestAgentAdvertisesSSHOnlyWhenConfigured(t *testing.T) {
	tests := []struct {
		name string
		ssh  *sshd.Config
		want []string
	}{
		{"server configured", testSSHConfig(t), []string{grant.StreamKindTCP, grant.StreamKindSSH}},
		{"no server", nil, []string{grant.StreamKindTCP}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newFakeHub(t)
			startAgent(t, h, func(o *Options) { o.NoPTY = true; o.SSH = tt.ssh })
			got := h.nextHello(t).GetCapabilities().GetStreamKinds()
			if !slices.Equal(got, tt.want) {
				t.Fatalf("stream kinds = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestAgentSSHUnsupportedKindWithoutServer: without an SSH server an ssh
// stream with a valid ssh grant is refused with 4400 unsupported_kind.
func TestAgentSSHUnsupportedKindWithoutServer(t *testing.T) {
	key := newTestKey(t, "k1")
	h := newFakeHub(t, key.public)
	startAgent(t, h, func(o *Options) { o.NoPTY = true })
	s := h.nextSession(t)
	_, err := s.OpenStream(context.Background(), sshOpen(t, key, s.Info(), grant.StreamKindSSH))
	wantRefusal(t, err, core.CloseProtocolError, reasonUnsupportedKind)
}

// TestAgentSSHRefusesGrantWithoutSSHCapability: an ssh stream whose grant
// is not a valid ssh grant for this session is refused with 4403
// grant_invalid.
func TestAgentSSHRefusesGrantWithoutSSHCapability(t *testing.T) {
	key := newTestKey(t, "k1")
	other := newTestKey(t, "k2")
	ssh := conduitv1.StreamKind_STREAM_KIND_SSH
	tests := []struct {
		name string
		open func(t *testing.T, info core.SessionInfo) *conduitv1.StreamOpen
	}{
		{"port-only grant", func(t *testing.T, info core.SessionInfo) *conduitv1.StreamOpen {
			return mintOpen(t, key, info, ssh, nil, grant.StreamKindTCP, nil)
		}},
		{"pty grant", func(t *testing.T, info core.SessionInfo) *conduitv1.StreamOpen {
			return mintOpen(t, key, info, ssh, nil, grant.StreamKindPTY, nil)
		}},
		{"unknown key", func(t *testing.T, info core.SessionInfo) *conduitv1.StreamOpen {
			return sshOpen(t, other, info, grant.StreamKindSSH)
		}},
		{"no grant", func(t *testing.T, info core.SessionInfo) *conduitv1.StreamOpen {
			o := sshOpen(t, key, info, grant.StreamKindSSH)
			o.Grant = nil
			return o
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newFakeHub(t, key.public)
			startAgent(t, h, func(o *Options) { o.NoPTY = true; o.SSH = testSSHConfig(t) })
			s := h.nextSession(t)
			_, err := s.OpenStream(context.Background(), tt.open(t, s.Info()))
			wantRefusal(t, err, core.CloseForbidden, reasonGrantInvalid)
		})
	}
}

// TestAgentSSHRefusesParams: an ssh stream takes no params.
func TestAgentSSHRefusesParams(t *testing.T) {
	key := newTestKey(t, "k1")
	h := newFakeHub(t, key.public)
	startAgent(t, h, func(o *Options) { o.NoPTY = true; o.SSH = testSSHConfig(t) })
	s := h.nextSession(t)
	params := map[string]string{"user": "root"}
	open := mintOpen(t, key, s.Info(), conduitv1.StreamKind_STREAM_KIND_SSH, params, grant.StreamKindSSH, params)
	_, err := s.OpenStream(context.Background(), open)
	wantRefusal(t, err, core.CloseProtocolError, reasonBadFrame)
}

// TestSharedDenyListTCPAndSSH: the TCP stream kind and SSH direct-tcpip
// refuse exactly the same loopback ports, from the one shared list.
func TestSharedDenyListTCPAndSSH(t *testing.T) {
	if !slices.Equal(ReservedPorts, localpolicy.ReservedPorts) {
		t.Fatalf("TCP ReservedPorts %v differ from localpolicy %v", ReservedPorts, localpolicy.ReservedPorts)
	}
	for port := 1; port <= 65535; port++ {
		_, tcpErr := TCPTarget(map[string]string{grant.ParamHost: loopbackHost, grant.ParamPort: strconv.Itoa(port)})
		_, sshErr := sshd.TCPDestination(loopbackHost, uint32(port))
		if (tcpErr == nil) != (sshErr == nil) {
			t.Fatalf("port %d: tcp kind err=%v, ssh direct-tcpip err=%v; they must agree", port, tcpErr, sshErr)
		}
		if localpolicy.IsReservedPort(port) && tcpErr == nil {
			t.Fatalf("reserved port %d accepted", port)
		}
	}
}
