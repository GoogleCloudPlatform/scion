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
	"strconv"
	"sync"

	core "github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// DefaultPTYSession is the tmux session a PTY stream attaches to; it is
// the only session the target serves.
const DefaultPTYSession = "scion"

// Default terminal size when a PTY grant names none.
const (
	defaultPTYCols = 80
	defaultPTYRows = 24
)

// PTYRequest is a validated PTY stream target.
type PTYRequest struct {
	Cols, Rows uint16
	Session    string
}

// PTYProcess is a running tmux client on a local pty. Read and Write go
// to the pty master. Close tears it down: it hangs up the pty, ends and
// reaps the client, and is safe to call more than once and concurrently
// with Read, Write and Resize.
type PTYProcess interface {
	io.ReadWriteCloser
	Resize(cols, rows uint16) error
}

// PTYSpawner starts a tmux client attached to req.Session on a new pty
// of req's size. ctx ends when the stream is cancelled; the handler
// closes whatever the spawner returns in that case, so a spawner need
// not watch ctx itself.
type PTYSpawner func(ctx context.Context, req PTYRequest) (PTYProcess, error)

// PTYUser is who a PTY stream's tmux client runs as: the agent user that
// owns the tmux server. A zero UID or GID means this process's identity.
type PTYUser struct {
	UID, GID int
	// Username sets HOME (/home/<name>), USER and LOGNAME for the client.
	Username string
	// RequirePrivilegeDrop refuses to start a client as this process's
	// identity (UID or GID not positive).
	RequirePrivilegeDrop bool
}

// streamKinds is the Hello's stream_kinds: tcp always, pty only when
// this agent can spawn a tmux client.
func (a *Agent) streamKinds() []string {
	kinds := []string{grant.StreamKindTCP}
	if a.opts.SpawnPTY != nil {
		kinds = append(kinds, grant.StreamKindPTY)
	}
	return kinds
}

// requirePTYGrant is the PTY capability check at the target: it accepts
// only claims whose signed stream kind is pty. VerifyStreamOpen has
// already matched the grant's stream header to the StreamOpen; this
// keeps the rule local to the code that spawns the client, so a grant
// for any other capability (a port-only tcp grant, say) never reaches
// the spawner.
func requirePTYGrant(claims *grant.Claims) error {
	if claims == nil || claims.Stream.Kind != grant.StreamKindPTY {
		return fmt.Errorf("%w: grant is not for a pty stream", grant.ErrStream)
	}
	return nil
}

// handlePTY serves a PTY stream. The order is fixed: verify the grant
// (consuming its jti) and require the pty capability, check the params
// against the local policy, spawn the tmux client, and only then accept.
// If the stream left the opening state meanwhile (cancel, open timeout,
// session loss), the spawned client and its pty are torn down before
// returning.
func (a *Agent) handlePTY(ctx context.Context, open *conduitv1.StreamOpen, ps core.PendingStream) error {
	claims, reason, err := a.verifyGrant(ctx, open)
	if err == nil {
		if err = requirePTYGrant(claims); err != nil {
			reason = reasonGrantInvalid
		}
	}
	if err != nil {
		log.Warn("Conduit: refused PTY stream %d: grant: %v", ps.ID(), err)
		return ps.Reject(core.CloseForbidden, reason)
	}
	req, err := PTYTarget(open.GetParams())
	if err != nil {
		log.Warn("Conduit: refused PTY stream %d: %v", ps.ID(), err)
		return ps.Reject(core.CloseForbidden, reasonForbidden)
	}
	p, err := a.opts.SpawnPTY(ctx, req)
	if err != nil {
		// e.g. the tmux session does not exist yet: transient (4504).
		log.Warn("Conduit: PTY stream %d: %v", ps.ID(), err)
		return ps.Reject(core.CloseRelayTimeout, reasonUpstreamUnreachable)
	}
	if a.opts.ptyBeforeAccept != nil {
		a.opts.ptyBeforeAccept(ctx)
	}
	st, err := ps.Accept()
	if err != nil {
		_ = p.Close()
		return nil // the stream already left the opening state
	}
	servePTY(ctx, st, p)
	return nil
}

// PTYTarget validates the (already grant-verified) PTY params: only
// cols, rows and session are allowed; cols and rows, when present, are
// canonical integers in 1..65535 (default 80x24); session, when present,
// must be DefaultPTYSession.
func PTYTarget(params map[string]string) (PTYRequest, error) {
	req := PTYRequest{Cols: defaultPTYCols, Rows: defaultPTYRows, Session: DefaultPTYSession}
	for k, v := range params {
		switch k {
		case grant.ParamCols, grant.ParamRows:
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 0xFFFF || strconv.Itoa(n) != v {
				return PTYRequest{}, fmt.Errorf("invalid pty %s %q", k, v)
			}
			if k == grant.ParamCols {
				req.Cols = uint16(n)
			} else {
				req.Rows = uint16(n)
			}
		case grant.ParamSession:
			if v != DefaultPTYSession {
				return PTYRequest{}, fmt.Errorf("pty session %q is not served", v)
			}
		default:
			return PTYRequest{}, fmt.Errorf("unexpected pty param %q", k)
		}
	}
	return req, nil
}

// servePTY copies between the stream and the pty and applies resizes,
// until either side ends or ctx ends (the stream or its session ended).
// Whichever happens first tears down both: the tmux client and its pty
// are closed and reaped, and the stream is closed. It returns only after
// every goroutine it started has finished.
func servePTY(ctx context.Context, st core.Stream, p PTYProcess) {
	var once sync.Once
	teardown := func() {
		once.Do(func() {
			_ = p.Close()
			_ = st.Close()
		})
	}
	stop := context.AfterFunc(ctx, teardown)
	defer stop()

	var wg sync.WaitGroup
	if rs, ok := st.(core.Resizable); ok {
		// Resizes is closed when the stream ends or is closed locally.
		wg.Add(1)
		go func() {
			defer wg.Done()
			for sz := range rs.Resizes() {
				if err := p.Resize(sz.Cols, sz.Rows); err != nil && !errors.Is(err, errPTYClosed) {
					log.Debug("Conduit: PTY resize: %v", err)
				}
			}
		}()
	}
	done := make(chan struct{}, 2)
	pipe := func(dst io.Writer, src io.Reader) {
		_, _ = io.Copy(dst, src)
		done <- struct{}{}
	}
	go pipe(st, p) // client output; ends when the client exits or p closes
	go pipe(p, st) // terminal input; ends when the peer closes or finishes
	<-done
	teardown()
	<-done
	wg.Wait()
}

// errPTYClosed is returned by a closed PTYProcess's Resize.
var errPTYClosed = errors.New("conduit: pty closed")
