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
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"sync"
	"time"

	core "github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/target"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// ReservedPorts are never a TCP stream target, whatever the grant says:
// the hub API (9810) and the metadata server (18380).
var ReservedPorts = []int{9810, 18380}

// loopbackHost is the only host a TCP stream may reach (design §3.10:
// TCP happens only on in-container loopback, at the sciontool end).
const loopbackHost = "127.0.0.1"

// localDialTimeout bounds the loopback dial.
const localDialTimeout = 10 * time.Second

// Stream refusal reasons.
const (
	reasonGrantInvalid        = "grant_invalid"
	reasonForbidden           = "forbidden"
	reasonUpstreamUnreachable = "upstream_unreachable"
	reasonUnsupportedKind     = "unsupported_kind"
)

// handleStream implements core.StreamHandler: it dispatches on the
// stream kind. A kind this agent does not serve is refused with
// unsupported_kind before anything else is looked at.
func (a *Agent) handleStream(ctx context.Context, open *conduitv1.StreamOpen, ps core.PendingStream) error {
	switch open.GetKind() {
	case conduitv1.StreamKind_STREAM_KIND_TCP:
		return a.handleTCP(ctx, open, ps)
	case conduitv1.StreamKind_STREAM_KIND_PTY:
		if a.opts.SpawnPTY != nil {
			return a.handlePTY(ctx, open, ps)
		}
	}
	return ps.Reject(core.CloseProtocolError, reasonUnsupportedKind)
}

// verifyGrant verifies open's grant against this session's Welcome and
// consumes its jti. It returns the grant's claims, or a refusal reason
// (the caller rejects with 4403) and the error to log.
func (a *Agent) verifyGrant(ctx context.Context, open *conduitv1.StreamOpen) (*grant.Claims, string, error) {
	w := core.WelcomeFromContext(ctx)
	if w == nil {
		return nil, reasonGrantInvalid + ": no session binding", errors.New("no session binding")
	}
	a.applyWelcomeKeys(w)
	v := &target.Verifier{
		Identity: target.IdentityFromWelcome(grant.TargetKindAgent, a.opts.AgentID, a.opts.ProjectID, w),
		Keys:     &a.keys,
		Replay:   a.replay,
		Now:      a.clk.Now,
	}
	claims, err := v.VerifyStreamOpen(ctx, target.BindingFromWelcome(w), open, target.Acting{})
	if err != nil {
		return nil, reasonGrantInvalid, err
	}
	return claims, "", nil
}

// handleTCP serves a TCP stream. The order is fixed: verify the grant
// against this session's Welcome, check the target against the local
// policy, dial loopback, and only then accept.
func (a *Agent) handleTCP(ctx context.Context, open *conduitv1.StreamOpen, ps core.PendingStream) error {
	if _, reason, err := a.verifyGrant(ctx, open); err != nil {
		log.Warn("Conduit: refused TCP stream %d: grant: %v", ps.ID(), err)
		return ps.Reject(core.CloseForbidden, reason)
	}
	port, err := TCPTarget(open.GetParams())
	if err != nil {
		log.Warn("Conduit: refused TCP stream %d: %v", ps.ID(), err)
		return ps.Reject(core.CloseForbidden, reasonForbidden)
	}
	conn, err := a.opts.DialLocal(ctx, "tcp", net.JoinHostPort(loopbackHost, strconv.Itoa(port)))
	if err != nil {
		log.Debug("Conduit: TCP stream %d: dial port %d: %v", ps.ID(), port, err)
		return ps.Reject(core.CloseRelayTimeout, reasonUpstreamUnreachable)
	}
	st, err := ps.Accept()
	if err != nil {
		_ = conn.Close()
		return nil // the stream already left the opening state
	}
	splice(ctx, st, conn)
	return nil
}

// TCPTarget validates the (already grant-verified) TCP params and returns
// the port. The params must be exactly {host: "127.0.0.1", port}, and the
// port must not be reserved.
func TCPTarget(params map[string]string) (int, error) {
	if len(params) != 2 || params[grant.ParamHost] != loopbackHost {
		return 0, fmt.Errorf("tcp target must be exactly {host:%s, port}", loopbackHost)
	}
	raw := params[grant.ParamPort]
	port, err := strconv.Atoi(raw)
	if err != nil || port < 1 || port > 65535 || strconv.Itoa(port) != raw {
		return 0, fmt.Errorf("invalid tcp port %q", raw)
	}
	if slices.Contains(ReservedPorts, port) {
		return 0, fmt.Errorf("tcp port %d is reserved", port)
	}
	return port, nil
}

// halfCloser is implemented by *net.TCPConn and by conduit streams.
type halfCloser interface{ CloseWrite() error }

// splice copies both ways between the stream and the local connection,
// propagating half-closes, until both directions finish or ctx ends (the
// stream or its session ended).
func splice(ctx context.Context, st core.Stream, conn net.Conn) {
	var wg sync.WaitGroup
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			_ = conn.Close()
			_ = st.Close()
		})
	}
	stop := context.AfterFunc(ctx, closeBoth)
	defer stop()
	pipe := func(dst io.Writer, src io.Reader, done func()) {
		defer wg.Done()
		_, err := io.Copy(dst, src)
		if err != nil && !errors.Is(err, net.ErrClosed) {
			closeBoth()
			return
		}
		done()
	}
	wg.Add(2)
	go pipe(st, conn, func() {
		if hc, ok := st.(halfCloser); ok {
			_ = hc.CloseWrite()
		}
	})
	go pipe(conn, st, func() {
		if hc, ok := conn.(halfCloser); ok {
			_ = hc.CloseWrite()
		}
	})
	wg.Wait()
	closeBoth()
}
