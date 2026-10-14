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
	"fmt"
	"net"
	"time"

	core "github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// requireSSHGrant is the SSH capability check at the target: it accepts
// only claims whose signed stream kind is ssh.
func requireSSHGrant(claims *grant.Claims) error {
	if claims == nil || claims.Stream.Kind != grant.StreamKindSSH {
		return fmt.Errorf("%w: grant is not for an ssh stream", grant.ErrStream)
	}
	return nil
}

// handleSSH serves an SSH stream with the embedded SSH server (design
// §3.8). The order is fixed: an ssh stream takes no params (checked
// before the grant, so a refusal does not consume its jti), verify the
// grant and require the ssh capability, accept, then run one SSH
// connection over the stream until it ends.
func (a *Agent) handleSSH(ctx context.Context, open *conduitv1.StreamOpen, ps core.PendingStream) error {
	if len(open.GetParams()) != 0 {
		log.Warn("Conduit: refused SSH stream %d: unexpected params", ps.ID())
		return ps.Reject(core.CloseProtocolError, reasonBadFrame)
	}
	claims, reason, err := a.verifyGrant(ctx, open)
	if err == nil {
		if err = requireSSHGrant(claims); err != nil {
			reason = reasonGrantInvalid
		}
	}
	if err != nil {
		log.Warn("Conduit: refused SSH stream %d: grant: %v", ps.ID(), err)
		return ps.Reject(core.CloseForbidden, reason)
	}
	st, err := ps.Accept()
	if err != nil {
		return nil // the stream already left the opening state
	}
	defer a.trackDeadline(ctx, st, claims)()
	if err := a.opts.SSH.Serve(ctx, streamConn{st}); err != nil {
		log.Debug("Conduit: SSH stream %d: %v", ps.ID(), err)
	}
	return nil
}

// streamConn presents a conduit stream as the net.Conn the SSH server
// reads from. Deadlines are not supported; the stream's context bounds
// its life.
type streamConn struct{ core.Stream }

func (streamConn) LocalAddr() net.Addr              { return streamAddr{} }
func (streamConn) RemoteAddr() net.Addr             { return streamAddr{} }
func (streamConn) SetDeadline(time.Time) error      { return nil }
func (streamConn) SetReadDeadline(time.Time) error  { return nil }
func (streamConn) SetWriteDeadline(time.Time) error { return nil }

type streamAddr struct{}

func (streamAddr) Network() string { return "conduit" }
func (streamAddr) String() string  { return "conduit-ssh-stream" }
