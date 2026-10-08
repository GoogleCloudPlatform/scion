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
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// fakePTY is a PTYProcess that echoes what is written to it.
type fakePTY struct {
	req     PTYRequest
	outR    *io.PipeReader
	outW    *io.PipeWriter
	resizes chan core.WindowSize
	closed  chan struct{}
	once    sync.Once
}

func newFakePTY(req PTYRequest) *fakePTY {
	r, w := io.Pipe()
	return &fakePTY{req: req, outR: r, outW: w, resizes: make(chan core.WindowSize, 8), closed: make(chan struct{})}
}

func (f *fakePTY) Read(b []byte) (int, error)  { return f.outR.Read(b) }
func (f *fakePTY) Write(b []byte) (int, error) { return f.outW.Write(b) }

func (f *fakePTY) Resize(cols, rows uint16) error {
	select {
	case <-f.closed:
		return errPTYClosed
	default:
	}
	f.resizes <- core.WindowSize{Cols: cols, Rows: rows}
	return nil
}

func (f *fakePTY) Close() error {
	f.once.Do(func() {
		_ = f.outW.Close()
		_ = f.outR.Close()
		close(f.closed)
	})
	return nil
}

// exit simulates the tmux client exiting: its output ends.
func (f *fakePTY) exit() { _ = f.outW.Close() }

func (f *fakePTY) waitClosed(t *testing.T) {
	t.Helper()
	select {
	case <-f.closed:
	case <-time.After(waitTimeout):
		t.Fatal("pty was not torn down")
	}
}

// fakeSpawner records every spawned fakePTY.
type fakeSpawner struct {
	calls atomic.Int64
	procs chan *fakePTY
	err   error
	// during, when set, runs inside the spawn (before it returns).
	during func(ctx context.Context)
}

func newFakeSpawner() *fakeSpawner { return &fakeSpawner{procs: make(chan *fakePTY, 8)} }

func (s *fakeSpawner) spawn(ctx context.Context, req PTYRequest) (PTYProcess, error) {
	s.calls.Add(1)
	if s.err != nil {
		return nil, s.err
	}
	p := newFakePTY(req)
	if s.during != nil {
		s.during(ctx)
	}
	s.procs <- p
	return p, nil
}

func (s *fakeSpawner) next(t *testing.T) *fakePTY {
	t.Helper()
	select {
	case p := <-s.procs:
		return p
	case <-time.After(waitTimeout):
		t.Fatal("no pty spawned")
		return nil
	}
}

// mintOpen builds a StreamOpen of kind with params, carrying a grant
// minted by key for grantKind and grantParams against the session info.
func mintOpen(t *testing.T, key testKey, info core.SessionInfo, kind conduitv1.StreamKind, params map[string]string, grantKind string, grantParams map[string]string) *conduitv1.StreamOpen {
	t.Helper()
	now := time.Now().Truncate(time.Second)
	tok, err := grant.Mint(&key.signer, grant.Claims{
		Issuer:    "scion-hub",
		Subject:   "user:u1",
		ProjectID: testProjectID,
		Target: grant.Target{
			Kind: grant.TargetKindAgent, ID: testAgentID, EndpointIncarnation: "launch-1",
			SessionID: info.SessionID, ConnectionEpoch: info.ConnectionEpoch,
		},
		Stream:    grant.StreamHeader{Kind: grantKind, Params: grantParams},
		NotBefore: now,
		Expiry:    now.Add(50 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &conduitv1.StreamOpen{Kind: kind, Params: params, Grant: tok}
}

func ptyParams() map[string]string {
	return map[string]string{grant.ParamCols: "100", grant.ParamRows: "30", grant.ParamSession: "scion"}
}

// ptyOpen builds a PTY StreamOpen with a matching pty grant.
func ptyOpen(t *testing.T, key testKey, info core.SessionInfo, params map[string]string) *conduitv1.StreamOpen {
	t.Helper()
	return mintOpen(t, key, info, conduitv1.StreamKind_STREAM_KIND_PTY, params, grant.StreamKindPTY, params)
}

// startPTYAgent runs an agent whose PTY spawner is sp.
func startPTYAgent(t *testing.T, key testKey, sp *fakeSpawner, mod func(*Options)) core.LocalSession {
	t.Helper()
	h := newFakeHub(t, key.public)
	startAgent(t, h, func(o *Options) {
		o.SpawnPTY = sp.spawn
		if mod != nil {
			mod(o)
		}
	})
	return h.nextSession(t)
}

// wantRefusal checks that err is a stream refusal with code and reason.
func wantRefusal(t *testing.T, err error, code uint32, reason string) {
	t.Helper()
	var ce *core.CloseError
	if !errors.As(err, &ce) || ce.Code != code || ce.Reason != reason {
		t.Fatalf("OpenStream = %v, want %d %s", err, code, reason)
	}
}

// TestAgentAdvertisesPTYOnlyWhenSupported: pty is in the Hello's
// stream_kinds exactly when the agent can spawn a tmux client.
func TestAgentAdvertisesPTYOnlyWhenSupported(t *testing.T) {
	sp := newFakeSpawner()
	tests := []struct {
		name  string
		spawn PTYSpawner
		want  []string
	}{
		{"spawner available", sp.spawn, []string{grant.StreamKindTCP, grant.StreamKindPTY}},
		{"no spawner (no tmux or no pty support)", nil, []string{grant.StreamKindTCP}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PATH", t.TempDir()) // the default spawner finds no tmux
			h := newFakeHub(t)
			startAgent(t, h, func(o *Options) { o.SpawnPTY = tt.spawn })
			got := h.nextHello(t).GetCapabilities().GetStreamKinds()
			if !slices.Equal(got, tt.want) {
				t.Fatalf("stream kinds = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestAgentPTYUnsupportedKind: an agent that cannot spawn a tmux client
// refuses a PTY stream with 4400 unsupported_kind, even with a valid
// pty grant.
func TestAgentPTYUnsupportedKind(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	key := newTestKey(t, "k1")
	h := newFakeHub(t, key.public)
	startAgent(t, h, nil)
	s := h.nextSession(t)
	_, err := s.OpenStream(context.Background(), ptyOpen(t, key, s.Info(), ptyParams()))
	wantRefusal(t, err, core.CloseProtocolError, reasonUnsupportedKind)
}

// TestAgentPTYRefusesGrantWithoutPTYCapability: the target refuses, with
// 4403 grant_invalid and without spawning anything, a PTY stream whose
// grant is not a valid pty grant for this session: a port-only (tcp)
// grant, a tampered or foreign grant, or a replayed one.
func TestAgentPTYRefusesGrantWithoutPTYCapability(t *testing.T) {
	key := newTestKey(t, "k1")
	other := newTestKey(t, "k2")
	tcpParams := map[string]string{grant.ParamHost: loopbackHost, grant.ParamPort: "8080"}
	pty := conduitv1.StreamKind_STREAM_KIND_PTY
	tests := []struct {
		name string
		open func(t *testing.T, info core.SessionInfo) *conduitv1.StreamOpen
	}{
		{"port-only grant, tcp params", func(t *testing.T, info core.SessionInfo) *conduitv1.StreamOpen {
			return mintOpen(t, key, info, pty, tcpParams, grant.StreamKindTCP, tcpParams)
		}},
		{"port-only grant, pty params", func(t *testing.T, info core.SessionInfo) *conduitv1.StreamOpen {
			return mintOpen(t, key, info, pty, ptyParams(), grant.StreamKindTCP, ptyParams())
		}},
		{"ssh grant", func(t *testing.T, info core.SessionInfo) *conduitv1.StreamOpen {
			return mintOpen(t, key, info, pty, ptyParams(), grant.StreamKindSSH, ptyParams())
		}},
		{"params altered after signing", func(t *testing.T, info core.SessionInfo) *conduitv1.StreamOpen {
			o := ptyOpen(t, key, info, ptyParams())
			o.Params[grant.ParamCols] = "101"
			return o
		}},
		{"unknown key", func(t *testing.T, info core.SessionInfo) *conduitv1.StreamOpen {
			return ptyOpen(t, other, info, ptyParams())
		}},
		{"other session", func(t *testing.T, info core.SessionInfo) *conduitv1.StreamOpen {
			info.SessionID = "sess-other"
			return ptyOpen(t, key, info, ptyParams())
		}},
		{"no grant", func(t *testing.T, info core.SessionInfo) *conduitv1.StreamOpen {
			o := ptyOpen(t, key, info, ptyParams())
			o.Grant = nil
			return o
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sp := newFakeSpawner()
			s := startPTYAgent(t, key, sp, nil)
			_, err := s.OpenStream(context.Background(), tt.open(t, s.Info()))
			wantRefusal(t, err, core.CloseForbidden, reasonGrantInvalid)
			if n := sp.calls.Load(); n != 0 {
				t.Fatalf("spawner called %d times for a refused stream", n)
			}
		})
	}
}

// TestAgentPTYRefusesReplayedGrant: a pty grant opens one stream; its
// replay is refused before the spawner runs again.
func TestAgentPTYRefusesReplayedGrant(t *testing.T) {
	key := newTestKey(t, "k1")
	sp := newFakeSpawner()
	s := startPTYAgent(t, key, sp, nil)
	open := ptyOpen(t, key, s.Info(), ptyParams())
	st, err := s.OpenStream(context.Background(), open)
	if err != nil {
		t.Fatalf("first OpenStream: %v", err)
	}
	defer func() { _ = st.Close() }()
	replay := &conduitv1.StreamOpen{Kind: open.GetKind(), Params: open.GetParams(), Grant: open.GetGrant()}
	_, err = s.OpenStream(context.Background(), replay)
	wantRefusal(t, err, core.CloseForbidden, reasonGrantInvalid)
	if n := sp.calls.Load(); n != 1 {
		t.Fatalf("spawner called %d times, want 1", n)
	}
}

// TestRequirePTYGrant pins the target's pty capability check on its own.
func TestRequirePTYGrant(t *testing.T) {
	tests := []struct {
		name   string
		claims *grant.Claims
		ok     bool
	}{
		{"pty", &grant.Claims{Stream: grant.StreamHeader{Kind: grant.StreamKindPTY}}, true},
		{"tcp", &grant.Claims{Stream: grant.StreamHeader{Kind: grant.StreamKindTCP}}, false},
		{"ssh", &grant.Claims{Stream: grant.StreamHeader{Kind: grant.StreamKindSSH}}, false},
		{"empty kind", &grant.Claims{}, false},
		{"nil claims", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := requirePTYGrant(tt.claims)
			if (err == nil) != tt.ok {
				t.Fatalf("requirePTYGrant = %v, want ok=%v", err, tt.ok)
			}
			if err != nil && !errors.Is(err, grant.ErrStream) {
				t.Fatalf("error %v does not wrap grant.ErrStream", err)
			}
		})
	}
}

// TestAgentPTYForbiddenParams: a validly granted PTY stream whose params
// break the local policy is refused with 4403 forbidden, unspawned.
func TestAgentPTYForbiddenParams(t *testing.T) {
	key := newTestKey(t, "k1")
	tests := []struct {
		name   string
		params map[string]string
	}{
		{"other tmux session", map[string]string{grant.ParamCols: "80", grant.ParamRows: "24", grant.ParamSession: "other"}},
		{"tmux target syntax", map[string]string{grant.ParamSession: "scion:0"}},
		{"zero cols", map[string]string{grant.ParamCols: "0", grant.ParamRows: "24"}},
		{"extra param", map[string]string{grant.ParamSession: "scion", grant.ParamPort: "22"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sp := newFakeSpawner()
			s := startPTYAgent(t, key, sp, nil)
			_, err := s.OpenStream(context.Background(), ptyOpen(t, key, s.Info(), tt.params))
			wantRefusal(t, err, core.CloseForbidden, reasonForbidden)
			if n := sp.calls.Load(); n != 0 {
				t.Fatalf("spawner called %d times", n)
			}
		})
	}
}

// TestAgentPTYSpawnFailure: a tmux client that cannot start (e.g. no
// tmux session yet) refuses the stream with 4504 upstream_unreachable.
func TestAgentPTYSpawnFailure(t *testing.T) {
	key := newTestKey(t, "k1")
	sp := newFakeSpawner()
	sp.err = errors.New("no server running")
	s := startPTYAgent(t, key, sp, nil)
	_, err := s.OpenStream(context.Background(), ptyOpen(t, key, s.Info(), ptyParams()))
	wantRefusal(t, err, core.CloseRelayTimeout, reasonUpstreamUnreachable)
}

// TestPTYTarget pins the local PTY param policy.
func TestPTYTarget(t *testing.T) {
	tests := []struct {
		name    string
		params  map[string]string
		want    PTYRequest
		wantErr bool
	}{
		{"full", map[string]string{"cols": "120", "rows": "40", "session": "scion"}, PTYRequest{120, 40, "scion"}, false},
		{"defaults", nil, PTYRequest{80, 24, "scion"}, false},
		{"max size", map[string]string{"cols": "65535", "rows": "65535"}, PTYRequest{65535, 65535, "scion"}, false},
		{"too large", map[string]string{"cols": "65536"}, PTYRequest{}, true},
		{"negative", map[string]string{"rows": "-1"}, PTYRequest{}, true},
		{"non-canonical", map[string]string{"cols": "080"}, PTYRequest{}, true},
		{"not a number", map[string]string{"rows": "x"}, PTYRequest{}, true},
		{"other session", map[string]string{"session": "main"}, PTYRequest{}, true},
		{"empty session", map[string]string{"session": ""}, PTYRequest{}, true},
		{"unknown param", map[string]string{"command": "sh"}, PTYRequest{}, true},
		{"agent_id", map[string]string{"agent_id": "a"}, PTYRequest{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := PTYTarget(tt.params)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("PTYTarget = %+v, %v; want %+v (err %v)", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

// TestAgentPTYStreamResize: the pty starts at the granted size and
// follows the stream's resize frames.
func TestAgentPTYStreamResize(t *testing.T) {
	key := newTestKey(t, "k1")
	sp := newFakeSpawner()
	s := startPTYAgent(t, key, sp, nil)
	st, err := s.OpenStream(context.Background(), ptyOpen(t, key, s.Info(), ptyParams()))
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	defer func() { _ = st.Close() }()
	p := sp.next(t)
	if p.req != (PTYRequest{Cols: 100, Rows: 30, Session: "scion"}) {
		t.Fatalf("spawned with %+v", p.req)
	}
	for _, want := range []core.WindowSize{{Cols: 132, Rows: 43}, {Cols: 80, Rows: 24}} {
		if err := st.Resize(want.Cols, want.Rows); err != nil {
			t.Fatal(err)
		}
		select {
		case got := <-p.resizes:
			if got != want {
				t.Fatalf("pty resized to %+v, want %+v", got, want)
			}
		case <-time.After(waitTimeout):
			t.Fatalf("pty not resized to %+v", want)
		}
	}
}

// TestAgentPTYStreamClose: bytes round-trip through the pty, and closing
// the stream tears down the tmux client and its pty.
func TestAgentPTYStreamClose(t *testing.T) {
	key := newTestKey(t, "k1")
	sp := newFakeSpawner()
	s := startPTYAgent(t, key, sp, nil)
	st, err := s.OpenStream(context.Background(), ptyOpen(t, key, s.Info(), ptyParams()))
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	p := sp.next(t)
	if _, err := st.Write([]byte("ls\r")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 3)
	if _, err := io.ReadFull(st, buf); err != nil || string(buf) != "ls\r" {
		t.Fatalf("echo = %q, %v", buf, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	p.waitClosed(t)
}

// TestAgentPTYClientExitClosesStream: when the tmux client exits (e.g.
// the user detaches), the target closes the stream and the pty.
func TestAgentPTYClientExitClosesStream(t *testing.T) {
	key := newTestKey(t, "k1")
	sp := newFakeSpawner()
	s := startPTYAgent(t, key, sp, nil)
	st, err := s.OpenStream(context.Background(), ptyOpen(t, key, s.Info(), ptyParams()))
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	defer func() { _ = st.Close() }()
	p := sp.next(t)
	p.exit()
	if _, err := io.ReadAll(st); err != nil {
		t.Fatalf("read after client exit: %v", err)
	}
	p.waitClosed(t)
}

// TestAgentPTYCancelDuringOpening: cancelling the open while the target
// is between spawning the tmux client and accepting leaves no client or
// pty behind, and no stream results. Hooks pin the window: the opener's
// cancel lands, and the target has seen it, before the spawn returns or
// before the accept.
func TestAgentPTYCancelDuringOpening(t *testing.T) {
	for _, phase := range []string{"during spawn", "after spawn, before accept"} {
		t.Run(phase, func(t *testing.T) {
			key := newTestKey(t, "k1")
			sp := newFakeSpawner()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			hook := func(hctx context.Context) {
				cancel()
				<-hctx.Done() // the target has processed the cancel
			}
			s := startPTYAgent(t, key, sp, func(o *Options) {
				if phase == "during spawn" {
					sp.during = hook
				} else {
					o.ptyBeforeAccept = hook
				}
			})
			st, err := s.OpenStream(ctx, ptyOpen(t, key, s.Info(), ptyParams()))
			if err == nil {
				_ = st.Close()
				t.Fatal("OpenStream succeeded after cancel")
			}
			sp.next(t).waitClosed(t)
		})
	}
}
