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
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	sconduit "github.com/GoogleCloudPlatform/scion/pkg/sciontool/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/conduit/sshd"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
	"github.com/google/uuid"
)

// Hub-level tests of active authorization on tunnel streams (criterion
// 16b for tcp and ssh tunnels) and of one user session holding tunnels to
// many agents (criterion 25 (a) and (b)). The ssh tunnels reach the
// embedded SSH server of a real sciontool target; a real SSH client runs
// over the tunnel stream and forwards a direct-tcpip channel to an echo.
// The re-check and the authorization deadline run on the relay's fake
// clock.

// tunnelAuthzStreamMax is the user stream authorization interval of the
// deadline tests: short enough that reaching it fires no keepalive on the
// relay's clock. The tests read the interval back from the re-check's
// configuration.
const tunnelAuthzStreamMax = time.Second

// tunnelAuthzKinds are the tunnel kinds of the 16b tests.
var tunnelAuthzKinds = []string{grant.StreamKindTCP, grant.StreamKindSSH}

// tunnelAuthzLog is a goroutine-safe log sink.
type tunnelAuthzLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *tunnelAuthzLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *tunnelAuthzLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// tunnelAuthzFixture is a tunnel fixture whose agent also serves ssh
// streams, with a user who holds attach and port access to the agent's
// project through separate role bindings.
type tunnelAuthzFixture struct {
	*tunnelFixture
	authz   *conduitStreamAuthz
	metrics *metricWaiter
	logs    *tunnelAuthzLog
	hostKey ssh.Signer

	attachRoleID, portRoleID string
	// memberID holds the role bindings below; memberToken is its CLI token.
	memberID, memberEmail, memberToken string
	attachBinding, portBinding         string
}

// newTunnelAuthzFixture builds the fixture. A non-zero streamAuthzMax sets
// the authorization interval of the user streams opened from then on.
func newTunnelAuthzFixture(t *testing.T, streamAuthzMax time.Duration) *tunnelAuthzFixture {
	t.Helper()
	// The relay and the re-check take their logger at start.
	logs := &tunnelAuthzLog{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	f := &tunnelAuthzFixture{tunnelFixture: newTunnelFixture(t), metrics: newMetricWaiter(), logs: logs}
	f.authz = f.srv.conduitAuthz.Load()
	require.NotNil(t, f.authz, "the re-check runs with the relay")
	require.Zero(t, f.authz.Len())
	if streamAuthzMax != 0 {
		f.authz.cfg.UserStreamAuthzMax = streamAuthzMax
	}
	var m conduitStreamAuthzMetrics = f.metrics
	f.srv.conduitAuthzMetrics.Store(&m)

	ctx := context.Background()
	attachRD, err := f.store.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name: "tunnel-attach", ScopeType: store.RoleScopeProject, Permissions: []string{"agent.attach"}})
	require.NoError(t, err)
	portRD, err := f.store.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name: "tunnel-port", ScopeType: store.RoleScopeProject, Permissions: []string{"agent.port_access"}})
	require.NoError(t, err)
	f.attachRoleID, f.portRoleID = attachRD.ID, portRD.ID

	f.memberID, f.memberEmail = tid("tunnel-authz-user"), "tunnel-user@conduit.test"
	createTestUserWithProjectRole(t, f.store, f.memberID, f.memberEmail, f.launched.ProjectID, store.ProjectRoleMember)
	ensureHubMembership(ctx, f.store, f.memberID)
	f.attachBinding = f.bindRole(t, f.attachRoleID, f.launched.ProjectID)
	f.portBinding = f.bindRole(t, f.portRoleID, f.launched.ProjectID)
	f.memberToken = f.userJWT(t, f.memberID, f.memberEmail)

	f.hostKey, err = sshd.NewHostKey()
	require.NoError(t, err)
	f.startSSHAgent(t, f.launched)
	return f
}

// bindRole binds the fixture user to a role in a project and returns the
// binding id.
func (f *tunnelAuthzFixture) bindRole(t *testing.T, roleID, projectID string) string {
	t.Helper()
	b, err := f.store.CreateRoleBinding(context.Background(), &store.RoleBinding{
		RoleDefinitionID: roleID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: f.memberID,
		ScopeType: store.RoleScopeProject, ScopeID: projectID, CreatedBy: "test",
	})
	require.NoError(t, err)
	return b.ID
}

// revokeKind removes, in the store only, the binding that grants kind's
// action, so no revocation event is published.
func (f *tunnelAuthzFixture) revokeKind(t *testing.T, kind string) {
	t.Helper()
	id := f.portBinding
	if kind == grant.StreamKindSSH {
		id = f.attachBinding
	}
	require.NoError(t, f.store.DeleteRoleBinding(context.Background(), id))
}

// startSSHAgent starts a sciontool conduit agent for a that serves tcp
// streams (echo) and ssh streams (the embedded server, whose direct-tcpip
// channels reach an echo).
func (f *tunnelAuthzFixture) startSSHAgent(t *testing.T, a *store.Agent) {
	t.Helper()
	guardSciontoolLog()
	tok := f.agentToken(t, a)
	admitted := make(chan *conduitv1.Welcome, 4)
	echoConn := func() net.Conn {
		c1, c2 := net.Pipe()
		go func() { _, _ = io.Copy(c2, c2) }()
		return c1
	}
	ag, err := sconduit.New(sconduit.Options{
		HubURL:    f.public.URL,
		AgentID:   a.ID,
		ProjectID: a.ProjectID,
		LaunchID:  a.RunID,
		Token:     func() string { return tok },
		OnSession: func(w *conduitv1.Welcome) { admitted <- w },
		Backoff:   &conduit.Backoff{Rand: func(int64) int64 { return 0 }},
		Clock:     fixtureClock{Fake: clock.NewFake(f.clock.Now()), now: f.clock.Now},
		DialLocal: func(context.Context, string, string) (net.Conn, error) { return echoConn(), nil },
		NoPTY:     true,
		SSH: &sshd.Config{
			HostKey: f.hostKey,
			Shell:   "/bin/sh",
			DialTCP: func(context.Context, string, string) (net.Conn, error) { return echoConn(), nil },
		},
	})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		_ = ag.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			t.Error("conduit agent did not stop")
		}
	})
	select {
	case <-admitted:
	case <-time.After(10 * time.Second):
		t.Fatal("conduit session not admitted")
	}
}

// userClient dials a user session as the fixture user.
func (f *tunnelAuthzFixture) userClient(t *testing.T) *tunnelClient {
	t.Helper()
	c, err := f.dialUser(t, f.memberToken, f.memberID)
	require.NoError(t, err)
	return c
}

// tracked returns the tracked user stream of c's tunnel id.
func (f *tunnelAuthzFixture) tracked(t *testing.T, c *tunnelClient, id uint32) *conduitUserStream {
	t.Helper()
	sid := c.ls.Info().SessionID
	f.authz.mu.Lock()
	defer f.authz.mu.Unlock()
	for st := range f.authz.streams {
		if st.SessionID == sid && st.StreamID == id {
			return st
		}
	}
	t.Fatalf("tunnel %d of session %s is not tracked", id, sid)
	return nil
}

// tunnelSSHConn carries an SSH client connection over a tunnel stream and
// keeps the first read error, which holds the stream's close code.
type tunnelSSHConn struct {
	conduit.Stream
	mu      sync.Mutex
	readErr error
}

func (c *tunnelSSHConn) Read(p []byte) (int, error) {
	n, err := c.Stream.Read(p)
	if err != nil {
		c.mu.Lock()
		if c.readErr == nil {
			c.readErr = err
		}
		c.mu.Unlock()
	}
	return n, err
}

func (c *tunnelSSHConn) firstReadErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readErr
}

func (*tunnelSSHConn) LocalAddr() net.Addr              { return tunnelSSHAddr{} }
func (*tunnelSSHConn) RemoteAddr() net.Addr             { return tunnelSSHAddr{} }
func (*tunnelSSHConn) SetDeadline(time.Time) error      { return nil }
func (*tunnelSSHConn) SetReadDeadline(time.Time) error  { return nil }
func (*tunnelSSHConn) SetWriteDeadline(time.Time) error { return nil }

type tunnelSSHAddr struct{}

func (tunnelSSHAddr) Network() string { return "conduit" }
func (tunnelSSHAddr) String() string  { return "conduit-tunnel" }

// authzTunnel is an open tcp or ssh tunnel. An ssh tunnel carries an SSH
// connection with one direct-tcpip channel to an echo.
type authzTunnel struct {
	kind   string
	st     conduit.Stream
	conn   *tunnelSSHConn
	client *ssh.Client
	fwd    net.Conn
}

// openTunnel opens a tunnel of kind to the agent on c.
func (f *tunnelAuthzFixture) openTunnel(t *testing.T, c *tunnelClient, agentID, kind string) *authzTunnel {
	t.Helper()
	if kind == grant.StreamKindTCP {
		return &authzTunnel{kind: kind, st: c.open(t, agentID, kind, tcpParams("3000"))}
	}
	tu := &authzTunnel{kind: kind, st: c.open(t, agentID, kind, nil)}
	tu.conn = &tunnelSSHConn{Stream: tu.st}
	cc, chans, reqs, err := ssh.NewClientConn(tu.conn, "agent", &ssh.ClientConfig{
		User:            "someone",
		HostKeyCallback: ssh.FixedHostKey(f.hostKey.PublicKey()),
		Timeout:         10 * time.Second,
	})
	require.NoError(t, err, "ssh handshake over the tunnel")
	tu.client = ssh.NewClient(cc, chans, reqs)
	t.Cleanup(func() { _ = tu.client.Close() })
	tu.fwd, err = tu.client.Dial("tcp", "localhost:3000")
	require.NoError(t, err, "direct-tcpip to localhost")
	return tu
}

// tunnelEcho checks one round trip through the tunnel.
func (tu *authzTunnel) tunnelEcho(t *testing.T, msg string) {
	t.Helper()
	if tu.kind == grant.StreamKindTCP {
		roundTrip(t, tu.st, msg)
		return
	}
	got := make([]byte, len(msg))
	done := make(chan error, 1)
	go func() { _, err := io.ReadFull(tu.fwd, got); done <- err }()
	_, err := tu.fwd.Write([]byte(msg))
	require.NoError(t, err)
	select {
	case err := <-done:
		require.NoError(t, err)
		assert.Equal(t, msg, string(got))
	case <-time.After(recheckWait):
		t.Fatalf("no echo of %q over ssh", msg)
	}
}

// end waits for the tunnel's stream to end and returns its read error
// (for an ssh tunnel, the SSH connection ends with it).
func (tu *authzTunnel) end(t *testing.T) error {
	t.Helper()
	if tu.kind == grant.StreamKindTCP {
		return streamEnd(t, tu.st)
	}
	done := make(chan struct{})
	go func() {
		_ = tu.client.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the ssh connection did not end")
	}
	return tu.conn.firstReadErr()
}

// requireAuthzExpired asserts a 4401 authz_expired stream close.
func requireAuthzExpired(t *testing.T, err error) {
	t.Helper()
	var ce *conduit.CloseError
	require.True(t, errors.As(err, &ce), "want a 4401 close, got %v", err)
	assert.Equal(t, conduit.CloseUnauthenticated, ce.Code)
	assert.Equal(t, conduitReasonAuthzExpired, ce.Reason)
}

// TestConduitTunnelAuthz_RevokedClosed4401WithinRecheckInterval (C16b
// revocation, tcp and ssh tunnels): the user loses the permission of the
// tunnel's kind with no revocation event (the notification is missed).
// Within one recheck interval the tunnel is closed with 4401 authz_expired
// by the sweep. The user's session stays up, and another user's tunnel to
// the same agent stays open.
func TestConduitTunnelAuthz_RevokedClosed4401WithinRecheckInterval(t *testing.T) {
	for _, kind := range tunnelAuthzKinds {
		t.Run(kind, func(t *testing.T) {
			f := newTunnelAuthzFixture(t, 0)
			recheck := f.authz.cfg.RecheckInterval
			require.Positive(t, recheck, "the sweep runs")
			c := f.userClient(t)
			revoked := f.openTunnel(t, c, f.launched.ID, kind)
			other := f.openTunnel(t, f.ownerClient(t), f.launched.ID, kind)
			revoked.tunnelEcho(t, "before")
			other.tunnelEcho(t, "before")
			require.Equal(t, 2, f.authz.Len())
			deadline := f.tracked(t, c, revoked.st.ID()).Deadline()
			require.True(t, deadline.After(f.relayClock.Now().Add(recheck)), "only the sweep can catch the revocation")

			f.revokeKind(t, kind)
			f.relayClock.Advance(recheck)

			requireAuthzExpired(t, revoked.end(t))
			f.metrics.wait(t, conduitAuthzTriggerSweep+"/"+conduitAuthzOutcomeClosed+"/"+kind, 1)
			other.tunnelEcho(t, "after")
			assert.Nil(t, c.ls.Err(), "the session stays up")
			require.Eventually(t, func() bool { return f.authz.Len() == 1 }, recheckWait, 20*time.Millisecond)
		})
	}
}

// TestConduitTunnelAuthz_IntervalRenewsThenCloses4401 (C16b interval, tcp
// and ssh tunnels): at each authorization deadline a tunnel whose user
// still holds the kind's permission is renewed, its deadline moving by
// exactly one interval, and it keeps carrying data. Once the permission
// is gone (no revocation event), the tunnel is closed with 4401
// authz_expired at its next deadline, and the session stays up.
func TestConduitTunnelAuthz_IntervalRenewsThenCloses4401(t *testing.T) {
	for _, kind := range tunnelAuthzKinds {
		t.Run(kind, func(t *testing.T) {
			f := newTunnelAuthzFixture(t, tunnelAuthzStreamMax)
			interval := f.authz.cfg.UserStreamAuthzMax
			require.Less(t, 3*interval, f.authz.cfg.RecheckInterval, "no sweep runs in this test")
			c := f.userClient(t)
			tu := f.openTunnel(t, c, f.launched.ID, kind)
			tu.tunnelEcho(t, "before")
			st := f.tracked(t, c, tu.st.ID())
			d0 := st.Deadline()
			require.False(t, d0.IsZero(), "the tunnel has an authorization deadline")

			renewed := conduitAuthzTriggerInterval + "/" + conduitAuthzOutcomeRenewed + "/" + kind
			for i := 1; i <= 2; i++ {
				f.relayClock.Advance(interval)
				f.metrics.wait(t, renewed, i)
				assert.Equal(t, d0.Add(time.Duration(i)*interval), st.Deadline(), "renewal %d moves the deadline by one interval", i)
				tu.tunnelEcho(t, "after renewal "+strconv.Itoa(i))
			}

			f.revokeKind(t, kind)
			f.relayClock.Advance(interval)
			requireAuthzExpired(t, tu.end(t))
			f.metrics.wait(t, conduitAuthzTriggerInterval+"/"+conduitAuthzOutcomeClosed+"/"+kind, 1)
			assert.Nil(t, c.ls.Err(), "the session stays up")
			require.Eventually(t, func() bool { return f.authz.Len() == 0 }, recheckWait, 20*time.Millisecond)
		})
	}
}

// TestConduitTunnelAuthz_IntervalRenewalNoticeDelivered (tcp and ssh
// tunnels): the agent's session is held by this node, so the tunnel is
// tracked with the shared renewal sender (conduitStreamRenewal). At its
// deadline the hub sends AuthRefresh{stream_id} on the agent's session
// without error, the renewal is logged with notice=local (never
// not_delivered), and the tunnel keeps carrying data.
func TestConduitTunnelAuthz_IntervalRenewalNoticeDelivered(t *testing.T) {
	for _, kind := range tunnelAuthzKinds {
		t.Run(kind, func(t *testing.T) {
			f := newTunnelAuthzFixture(t, tunnelAuthzStreamMax)
			interval := f.authz.cfg.UserStreamAuthzMax
			require.Less(t, interval, f.authz.cfg.RecheckInterval, "no sweep runs in this test")
			c := f.userClient(t)
			tu := f.openTunnel(t, c, f.launched.ID, kind)
			tu.tunnelEcho(t, "before")
			st := f.tracked(t, c, tu.st.ID())
			require.NotNil(t, st.Renew, "the agent's session is local: a notice sender is set")
			calls := watchStream(st)

			f.relayClock.Advance(interval)
			f.metrics.wait(t, conduitAuthzTriggerInterval+"/"+conduitAuthzOutcomeRenewed+"/"+kind, 1)
			closes, renews := calls.snapshot()
			assert.Empty(t, closes)
			require.Len(t, renews, 1, "one renewal notice")
			assert.NoError(t, renews[0], "the notice was sent on the agent's session")

			streamAttr := "stream_id=" + strconv.FormatUint(uint64(tu.st.ID()), 10) + " "
			var line string
			require.Eventually(t, func() bool {
				for _, l := range strings.Split(f.logs.String(), "\n") {
					if strings.Contains(l, "msg=conduit_stream_authz") && strings.Contains(l, "outcome=renewed") && strings.Contains(l, streamAttr) {
						line = l
						return true
					}
				}
				return false
			}, recheckWait, 20*time.Millisecond, "the renewal is logged")
			assert.Contains(t, line, "notice=local")
			assert.NotContains(t, line, "notice=not_delivered")

			// The notice is queued as a control frame ahead of this data.
			tu.tunnelEcho(t, "after renewal")
			assert.Nil(t, c.ls.Err(), "the session stays up")
		})
	}
}

// TestConduitTunnelAuthz_DialerAuthRefreshRefused4400 (C16b, tcp and ssh
// tunnels): an AuthRefresh{stream_id} sent by the user session that holds
// the tunnel is refused with 4400 bad_frame and logged, and it moves no
// deadline. Sent from another session of the same user, naming the
// tunnel's id, it closes only that session: the tunnel stays open with
// its deadline unchanged.
func TestConduitTunnelAuthz_DialerAuthRefreshRefused4400(t *testing.T) {
	for _, kind := range tunnelAuthzKinds {
		t.Run(kind+"/holding session", func(t *testing.T) {
			f := newTunnelAuthzFixture(t, 0)
			c := f.userClient(t)
			tu := f.openTunnel(t, c, f.launched.ID, kind)
			tu.tunnelEcho(t, "before")
			st := f.tracked(t, c, tu.st.ID())
			d0 := st.Deadline()
			require.False(t, d0.IsZero())

			require.NoError(t, c.ls.RefreshAuth([]byte("credential"), tu.st.ID()))
			select {
			case <-c.ls.Done():
			case <-time.After(recheckWait):
				t.Fatal("the session that sent the renewal was not closed")
			}
			var ce *conduit.CloseError
			require.True(t, errors.As(c.ls.Err(), &ce), "want 4400 bad_frame, got %v", c.ls.Err())
			assert.EqualValues(t, conduit.CloseProtocolError, ce.Code)
			assert.True(t, strings.HasPrefix(ce.Reason, "bad_frame"), ce.Reason)

			assert.Equal(t, d0, st.Deadline(), "a dialer-sent renewal moved the deadline")
			assert.NotContains(t, f.metrics.list(), conduitAuthzTriggerInterval+"/"+conduitAuthzOutcomeRenewed+"/"+kind)
			logged := f.logs.String()
			for _, want := range []string{
				"dialer sent AuthRefresh with a stream id",
				"stream_id=" + strconv.FormatUint(uint64(tu.st.ID()), 10),
				"principal_id=" + f.memberID,
			} {
				assert.Contains(t, logged, want)
			}
		})
		t.Run(kind+"/other session", func(t *testing.T) {
			f := newTunnelAuthzFixture(t, 0)
			c := f.userClient(t)
			tu := f.openTunnel(t, c, f.launched.ID, kind)
			st := f.tracked(t, c, tu.st.ID())
			d0 := st.Deadline()

			sender := f.userClient(t)
			require.NoError(t, sender.ls.RefreshAuth([]byte("credential"), tu.st.ID()))
			select {
			case <-sender.ls.Done():
			case <-time.After(recheckWait):
				t.Fatal("the session that sent the renewal was not closed")
			}
			assert.EqualValues(t, conduit.CloseProtocolError, conduit.CodeOf(sender.ls.Err(), 0), "want 4400 bad_frame, got %v", sender.ls.Err())

			assert.Equal(t, d0, st.Deadline(), "a dialer-sent renewal moved the deadline")
			tu.tunnelEcho(t, "still open")
			assert.Nil(t, c.ls.Err(), "the tunnel's session stays up")
			assert.Equal(t, 1, f.authz.Len())
		})
	}
}

// tunnelAuthzProject creates a second project in which the agent owner is
// a member.
func tunnelAuthzProject(t *testing.T, tf *tunnelFixture, slug string) *store.Project {
	t.Helper()
	p := &store.Project{ID: tid("tunnel-project-" + slug), Name: slug, Slug: slug}
	require.NoError(t, tf.store.CreateProject(context.Background(), p))
	createTestUserWithProjectRole(t, tf.store, tf.launched.OwnerID, "owner@conduit.test", p.ID, store.ProjectRoleMember)
	return p
}

// tunnelAuthzAgentIn creates a running agent of the owner in a project,
// exposing port 3000.
func tunnelAuthzAgentIn(t *testing.T, tf *tunnelFixture, slug, projectID string) *store.Agent {
	t.Helper()
	ctx := context.Background()
	a := &store.Agent{
		ID: tid(slug), Slug: slug, Name: slug,
		ProjectID: projectID, OwnerID: tf.launched.OwnerID, Ancestry: tf.launched.Ancestry,
		RuntimeBrokerID: tf.launched.RuntimeBrokerID, Phase: string(state.PhaseRunning),
	}
	require.NoError(t, tf.store.CreateAgent(ctx, a))
	_, err := tf.store.SetAgentRunID(ctx, a.ID, uuid.NewString(), nil)
	require.NoError(t, err)
	got, err := tf.store.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	return tf.exposePort(t, got, 3000)
}

// TestConduitTunnel_ThreeAgentsTwoProjectsOneSession (C25 (a)): one user
// session holds tunnels to three agents in two projects at once. Closing
// one leaves the others open; revoking access to one agent closes only its
// tunnel with 4401; the session stays up throughout.
func TestConduitTunnel_ThreeAgentsTwoProjectsOneSession(t *testing.T) {
	tf := newTunnelFixture(t)
	p2 := tunnelAuthzProject(t, tf, "second")
	second := tf.newAgent(t, "tunnel-second")
	third := tunnelAuthzAgentIn(t, tf, "tunnel-third", p2.ID)
	require.NotEqual(t, tf.launched.ProjectID, third.ProjectID)
	for _, a := range []*store.Agent{tf.launched, second, third} {
		tf.startAgent(t, tf.public.URL, a)
	}
	c := tf.ownerClient(t)
	a := tf.srv.conduitAuthz.Load()

	one := c.open(t, tf.launched.ID, grant.StreamKindTCP, tcpParams("3000"))
	two := c.open(t, second.ID, grant.StreamKindTCP, tcpParams("3000"))
	three := c.open(t, third.ID, grant.StreamKindTCP, tcpParams("3000"))
	ids := map[uint32]bool{one.ID(): true, two.ID(): true, three.ID(): true}
	assert.Len(t, ids, 3, "one stream per tunnel")
	for i, st := range []conduit.Stream{one, two, three} {
		roundTrip(t, st, "open "+strconv.Itoa(i))
	}
	require.Equal(t, 3, a.Len())

	// Closing one tunnel leaves the other two open.
	require.NoError(t, one.Close())
	require.Eventually(t, func() bool { return a.Len() == 2 }, recheckWait, 20*time.Millisecond)
	roundTrip(t, two, "after close")
	roundTrip(t, three, "after close")
	assert.Nil(t, c.ls.Err(), "the session stays up")

	// The third agent stops exposing the port: only its tunnel closes.
	tf.exposePort(t, third)
	a.Recheck(context.Background(), conduitAuthzTriggerNotify, conduitAuthzMatch{AgentID: third.ID})
	requireAuthzExpired(t, streamEnd(t, three))
	roundTrip(t, two, "after revoke")
	assert.Nil(t, c.ls.Err(), "the session stays up")
	assert.Equal(t, 1, a.Len())
}

// TestConduitTunnel_ProjectScopedTokenRefusedPerTunnel (C25 (b)): a session
// admitted with a project-scoped token opens tunnels to agents in its
// project; a tunnel to an agent outside it is refused with 403 forbidden
// (the user may reach that agent with an unscoped login), and the session
// and its open tunnel stay up.
func TestConduitTunnel_ProjectScopedTokenRefusedPerTunnel(t *testing.T) {
	tf := newTunnelFixture(t)
	p2 := tunnelAuthzProject(t, tf, "outside")
	outside := tunnelAuthzAgentIn(t, tf, "tunnel-outside", p2.ID)
	tf.startAgent(t, tf.public.URL, tf.launched)
	tf.startAgent(t, tf.public.URL, outside)

	// The user may reach the outside agent with an unscoped login.
	full := tf.ownerClient(t)
	st := full.open(t, outside.ID, grant.StreamKindTCP, tcpParams("3000"))
	roundTrip(t, st, "unscoped")
	require.NoError(t, st.Close())

	scoped, err := tf.dialUser(t, tf.uat(t, "project-scoped", store.UATScopeAgentPortAccess), tf.launched.OwnerID)
	require.NoError(t, err)
	in := scoped.open(t, tf.launched.ID, grant.StreamKindTCP, tcpParams("3000"))
	roundTrip(t, in, "inside")

	r := scoped.post(t, outside.ID, grant.StreamKindTCP, tcpParams("3000"))
	require.Equal(t, http.StatusForbidden, r.status, r.body)
	assert.Equal(t, ErrCodeForbidden, r.err.Code)

	roundTrip(t, in, "after the refusal")
	assert.Nil(t, scoped.ls.Err(), "the session stays up")
	again := scoped.open(t, tf.launched.ID, grant.StreamKindTCP, tcpParams("3000"))
	roundTrip(t, again, "another tunnel inside")
}
