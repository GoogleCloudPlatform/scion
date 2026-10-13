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

//go:build unix

package conduit

import (
	"context"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
)

// TestAgentSSHStreamServesSSH: an ssh stream with a valid ssh grant
// carries an SSH connection to the embedded server: the client sees the
// configured host key and an exec runs.
func TestAgentSSHStreamServesSSH(t *testing.T) {
	key := newTestKey(t, "k1")
	cfg := testSSHConfig(t)
	h := newFakeHub(t, key.public)
	startAgent(t, h, func(o *Options) { o.NoPTY = true; o.SSH = cfg })
	s := h.nextSession(t)
	st, err := s.OpenStream(context.Background(), sshOpen(t, key, s.Info(), grant.StreamKindSSH))
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	defer func() { _ = st.Close() }()
	cc, chans, reqs, err := ssh.NewClientConn(streamConn{st}, "agent", &ssh.ClientConfig{
		User:            "someone",
		HostKeyCallback: ssh.FixedHostKey(cfg.HostKey.PublicKey()),
	})
	if err != nil {
		t.Fatalf("ssh handshake: %v", err)
	}
	client := ssh.NewClient(cc, chans, reqs)
	defer func() { _ = client.Close() }()
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	out, err := sess.Output("echo conduit-ok")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if strings.TrimSpace(string(out)) != "conduit-ok" {
		t.Fatalf("exec output %q", out)
	}
}
