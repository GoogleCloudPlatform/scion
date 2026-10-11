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

package tunnelclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
)

// Credential kinds. A credential's kind and the subject the hub reports
// for it form its identity.
const (
	// CredentialOAuth is a stored interactive login.
	CredentialOAuth = "oauth"
	// CredentialToken is a bearer token from the environment (a user
	// access token or a bootstrap token).
	CredentialToken = "token"
	// CredentialDev is the development token.
	CredentialDev = "dev"
)

// Scope values reported for a credential.
const (
	// ScopeHub is an interactive user login: the user's own permissions
	// on the hub.
	ScopeHub = "hub"
	// ScopeUnknown is a credential whose scope the CLI holds no
	// non-secret record of. The hub decides per tunnel.
	ScopeUnknown = "unknown"
)

// Credential is a resolved hub credential. Token is the secret: it is sent
// only as the Bearer header and is never logged, stored or printed.
type Credential struct {
	Token string
	Kind  string
}

// Identity describes a credential without its secret.
type Identity struct {
	Kind    string `json:"kind"`
	Subject string `json:"subject"`
	Scope   string `json:"scope"`
}

// User is what the hub reports for a credential.
type User struct {
	ID    string
	Email string
}

// Target is a resolved tunnel target.
type Target struct {
	AgentID   string
	ProjectID string
}

// Hub is the REST side of the hub that a tunnel needs before it dials:
// who the credential is, and the agent's id. Implementations return an
// *HTTPError for a hub answer and any other error for a transport failure.
// Neither call prompts, registers or syncs.
type Hub interface {
	Whoami(ctx context.Context, cred Credential) (User, error)
	ResolveAgent(ctx context.Context, cred Credential, project, agent string) (Target, error)
}

// HTTPError is an error answer from the hub's REST API.
type HTTPError struct {
	Status  int
	Code    string
	Message string
}

func (e *HTTPError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("HTTP %d", e.Status)
}

// ScopeReporter reports a credential's scope from non-secret metadata.
type ScopeReporter interface {
	Scope(cred Credential) string
}

// DefaultScope reports ScopeHub for an interactive login (OAuth or the
// development token) and ScopeUnknown otherwise. It never reads the
// secret.
type DefaultScope struct{}

// Scope implements ScopeReporter.
func (DefaultScope) Scope(cred Credential) string {
	switch cred.Kind {
	case CredentialOAuth, CredentialDev:
		return ScopeHub
	}
	return ScopeUnknown
}

// Config is what a tunnel needs from its caller.
type Config struct {
	// Endpoint is the hub endpoint, exactly as resolved by the CLI.
	Endpoint string
	// ResolveCredential returns the current credential for Endpoint
	// without prompting. An empty Token means there is none. It is
	// called once at start and once more on a refused tunnel request.
	ResolveCredential func() Credential
	// Hub answers the REST calls made before the dial.
	Hub Hub
	// ApplyTransport adds the transport (IAP) headers to a dial request,
	// after the Bearer header is set. Nil adds none.
	ApplyTransport func(h http.Header) error
	// Scope reports the credential's scope (default DefaultScope).
	Scope ScopeReporter
	// State publishes this process's tunnels for scion tunnel status.
	// Nil publishes nothing. A failing store never fails a tunnel.
	State StateStore
	// ClientVersion is sent in Hello.
	ClientVersion string
	// Session is the base session configuration (tests set the clock);
	// its stream and RPC handlers are replaced.
	Session conduit.Config
	// Jitter returns the delay before the one retry of a transient
	// fault (default 1s plus up to 1s).
	Jitter func() time.Duration
	// Sleep waits d or until ctx ends (default a timer).
	Sleep func(ctx context.Context, d time.Duration) error
	// Now is the clock for opened_at (default time.Now).
	Now func() time.Time
	// StreamWait bounds the wait for the hub-opened stream after a
	// successful tunnel request (default 15s).
	StreamWait time.Duration
	// DialTimeout bounds one session dial (default 30s).
	DialTimeout time.Duration
	// FlushTimeout bounds the wait, after a tunnel ends cleanly, for its
	// queued data and close to be written (default 10s).
	FlushTimeout time.Duration
	// Stderr receives the listener's informational lines (default
	// os.Stderr).
	Stderr io.Writer
}

func (c Config) jitter() time.Duration {
	if c.Jitter != nil {
		return c.Jitter()
	}
	return time.Second + rand.N(time.Second)
}

func (c Config) sleep(ctx context.Context, d time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c Config) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c Config) streamWait() time.Duration {
	if c.StreamWait > 0 {
		return c.StreamWait
	}
	return 15 * time.Second
}

func (c Config) dialTimeout() time.Duration {
	if c.DialTimeout > 0 {
		return c.DialTimeout
	}
	return 30 * time.Second
}

func (c Config) flushTimeout() time.Duration {
	if c.FlushTimeout > 0 {
		return c.FlushTimeout
	}
	return 10 * time.Second
}

func (c Config) stderr() io.Writer {
	if c.Stderr != nil {
		return c.Stderr
	}
	return os.Stderr
}

func (c Config) scope() ScopeReporter {
	if c.Scope != nil {
		return c.Scope
	}
	return DefaultScope{}
}

// Request is one scion tunnel invocation.
type Request struct {
	// Agent is the agent slug or id; Project the project slug or id.
	Agent   string
	Project string
	// Port is the agent-local TCP port (not with SSH).
	Port int
	// LocalPort is the local listener port (0 picks one; not with Stdio).
	LocalPort int
	// SSH asks for an ssh stream instead of tcp.
	SSH bool
	// Stdio copies bytes on Stdin/Stdout instead of listening.
	Stdio  bool
	Stdin  io.Reader
	Stdout io.Writer
	// Ready, if set, is called with the listener's address once it
	// listens.
	Ready func(addr net.Addr)
}

// Kind returns the stream kind the request asks for.
func (r Request) Kind() string {
	if r.SSH {
		return string(conduit.StreamSSH)
	}
	return string(conduit.StreamTCP)
}

// Validate checks the request locally. Every failure is ExitUsage.
func (r Request) Validate() error {
	switch {
	case r.Agent == "":
		return Errorf(ExitUsage, "an agent is required")
	case !validAgentRef(r.Agent):
		return Errorf(ExitUsage, "invalid agent %q: use the agent's slug or id", r.Agent)
	case r.Project == "":
		return Errorf(ExitUsage, "no project: pass --project")
	case !validProjectRef(r.Project):
		return Errorf(ExitUsage, "invalid project %q: use the project's slug or id", r.Project)
	case r.SSH && r.Port != 0:
		return Errorf(ExitUsage, "--ssh and --port cannot be used together")
	case !r.SSH && r.Port == 0:
		return Errorf(ExitUsage, "--port is required (or --ssh)")
	case r.Port < 0 || r.Port > 65535:
		return Errorf(ExitUsage, "invalid --port %d: must be 1-65535", r.Port)
	case r.Stdio && r.LocalPort != 0:
		return Errorf(ExitUsage, "--local-port cannot be used with --stdio")
	case r.LocalPort < 0 || r.LocalPort > 65535:
		return Errorf(ExitUsage, "invalid --local-port %d: must be 1-65535", r.LocalPort)
	}
	return nil
}

// params returns the tunnel request params.
func (r Request) params() map[string]string {
	if r.SSH {
		return nil
	}
	return map[string]string{"port": strconv.Itoa(r.Port)}
}

// loginHint names the command that fixes a credential failure.
func loginHint(endpoint string) string {
	return "log in with: scion hub auth login --hub " + endpoint
}

// Run runs one tunnel invocation until it ends. The returned error carries
// the exit code (CodeOf); a cancelled ctx returns ctx's error, and the
// caller maps the signal (SignalExit).
//
// With Stdio, Run returns when the tunnel ends, which can be while a read
// from Stdin is still blocked: that read is left behind (a CLI process
// exits). In-process callers that need it to end should close Stdin, or
// pass a reader that ends when Run returns. On the remote side's FIN,
// Stdout is closed if it is an io.Closer.
func Run(ctx context.Context, cfg Config, req Request) error {
	if err := req.Validate(); err != nil {
		return err
	}
	c := &client{cfg: cfg, req: req, tunnels: map[*tunnelEntry]struct{}{}, lost: make(chan error, 1)}
	defer c.shutdown()
	if err := c.start(ctx); err != nil {
		return err
	}
	if req.Stdio {
		return c.runStdio(ctx)
	}
	return c.runListener(ctx)
}

// client is one invocation's state: one user session at a time (a
// replaced session lives on while its tunnels drain).
type client struct {
	cfg Config
	req Request
	// id, user and target are set once by start, before any concurrent
	// use. Each fresh session re-resolves its own credential, so no
	// credential is shared between concurrent tunnel requests.
	id     Identity
	user   User
	target Target

	mu       sync.Mutex
	cur      *session
	sessions []*session
	tunnels  map[*tunnelEntry]struct{}
	started  time.Time
	record   RecordWriter

	// lost receives the error when the current session ends unplanned.
	lost chan error
}

// start resolves the credential, its identity and the target, and dials
// the first session. Nothing reads stdin or prompts.
func (c *client) start(ctx context.Context) error {
	c.started = c.cfg.now()
	cred := c.cfg.ResolveCredential()
	if cred.Token == "" {
		return Errorf(ExitCredential, "no usable credential for %s; %s", c.cfg.Endpoint, loginHint(c.cfg.Endpoint))
	}
	user, err := c.cfg.Hub.Whoami(ctx, cred)
	if err != nil {
		return c.restError(ctx, err, "checking the credential")
	}
	c.user = user
	c.id = Identity{Kind: cred.Kind, Subject: user.ID, Scope: c.cfg.scope().Scope(cred)}
	tgt, err := c.cfg.Hub.ResolveAgent(ctx, cred, c.req.Project, c.req.Agent)
	if err != nil {
		var he *HTTPError
		if errors.As(err, &he) && he.Status == http.StatusNotFound {
			return Errorf(ExitAgentNotFound, "agent %q not found in project %q", c.req.Agent, c.req.Project)
		}
		return c.restError(ctx, err, "looking up the agent")
	}
	c.target = tgt
	s, err := c.dial(ctx, cred)
	if err != nil {
		return err
	}
	c.setCurrent(s)
	if c.cfg.State != nil {
		if w, err := c.cfg.State.Open(); err == nil {
			c.record = w
		}
	}
	c.publish()
	return nil
}

// restError maps a failed REST call to its exit code.
func (c *client) restError(ctx context.Context, err error, doing string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var he *HTTPError
	if !errors.As(err, &he) {
		return Errorf(ExitHubUnavailable, "cannot reach the hub at %s while %s: %v", c.cfg.Endpoint, doing, err)
	}
	switch {
	case he.Status == http.StatusUnauthorized:
		return Errorf(ExitCredential, "the hub at %s refused the credential; %s", c.cfg.Endpoint, loginHint(c.cfg.Endpoint))
	case he.Status == http.StatusForbidden:
		return Errorf(ExitForbidden, "not permitted while %s: %s", doing, he.Message)
	case he.Status == http.StatusNotFound:
		return Errorf(ExitAgentNotFound, "not found while %s: %s", doing, he.Message)
	case he.Status >= 500:
		return Errorf(ExitHubUnavailable, "the hub at %s is unavailable while %s (HTTP %d)", c.cfg.Endpoint, doing, he.Status)
	}
	return Errorf(ExitError, "the hub refused while %s: %v", doing, he)
}

// current returns the session new tunnels go to.
func (c *client) current() *session {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cur
}

// setCurrent makes s the session for new tunnels and watches it.
// A replaced session that nothing uses is closed at once; otherwise it is
// closed when its last tunnel or request ends.
func (c *client) setCurrent(s *session) {
	c.mu.Lock()
	old := c.cur
	c.cur = s
	c.sessions = append(c.sessions, s)
	idle := old != nil && old.users == 0
	c.mu.Unlock()
	go c.watch(s)
	if idle {
		_ = old.ls.Close()
	}
}

// watch forgets s when it ends, and reports an unplanned end of the
// current session: that ends the invocation's local connections.
func (c *client) watch(s *session) {
	<-s.ls.Done()
	// A planned drain (GoAway with relay_restart) is not a loss: new
	// tunnels go to a fresh session. A GoAway that closes at once with
	// another code is.
	planned := goAwayReceived(s) && conduit.CodeOf(s.ls.Err(), conduit.CloseRelayRestart) == conduit.CloseRelayRestart
	c.mu.Lock()
	for i, x := range c.sessions {
		if x == s {
			c.sessions = append(c.sessions[:i], c.sessions[i+1:]...)
			break
		}
	}
	isCurrent := c.cur == s
	c.mu.Unlock()
	if isCurrent && !planned {
		select {
		case c.lost <- Errorf(ExitHubUnavailable, "the session to the hub at %s was lost", c.cfg.Endpoint):
		default:
		}
	}
	c.publish()
}

// shutdown closes every session and removes the status record.
func (c *client) shutdown() {
	c.mu.Lock()
	sessions := append([]*session(nil), c.sessions...)
	rec := c.record
	c.record = nil
	c.mu.Unlock()
	for _, s := range sessions {
		_ = s.ls.Close()
	}
	if rec != nil {
		_ = rec.Close()
	}
}

// tunnelEntry is one open tunnel of this process.
type tunnelEntry struct {
	sess *session
	// ended is closed by conduit when the stream has ended; for a stream
	// we close, once its StreamClose has been written.
	ended    <-chan struct{}
	openedAt time.Time
	deadline *time.Time
}

// acquire counts a tunnel request on s; the count passes to the tunnel
// when the request succeeds and is released otherwise.
func (c *client) acquire(s *session) {
	c.mu.Lock()
	s.users++
	c.mu.Unlock()
}

// release drops a count taken by acquire. A session that is no longer
// current is closed when nothing uses it.
func (c *client) release(s *session) {
	c.mu.Lock()
	s.users--
	idle := s != c.cur && s.users == 0
	c.mu.Unlock()
	if idle {
		_ = s.ls.Close()
	}
}

func (c *client) addTunnel(t *tunnelEntry) {
	c.mu.Lock()
	c.tunnels[t] = struct{}{}
	c.mu.Unlock()
	c.publish()
}

// removeTunnel forgets an ended tunnel and releases its session.
func (c *client) removeTunnel(t *tunnelEntry) {
	c.mu.Lock()
	delete(c.tunnels, t)
	c.mu.Unlock()
	c.publish()
	c.release(t.sess)
}

// publish rewrites this process's status record.
func (c *client) publish() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.record == nil {
		return
	}
	rec := Record{
		Schema:     RecordSchema,
		PID:        os.Getpid(),
		StartedAt:  c.started.UTC(),
		Endpoint:   c.cfg.Endpoint,
		Credential: c.id,
		Tunnels:    []TunnelRecord{},
	}
	if c.cur != nil {
		rec.SessionID = c.cur.id
	}
	port := c.req.Port
	for t := range c.tunnels {
		tr := TunnelRecord{
			Agent:    c.req.Agent,
			Project:  c.req.Project,
			Kind:     c.req.Kind(),
			Port:     port,
			OpenedAt: t.openedAt.UTC(),
		}
		if t.deadline != nil {
			d := t.deadline.UTC()
			tr.AuthzDeadline = &d
		}
		rec.Tunnels = append(rec.Tunnels, tr)
	}
	sortTunnels(rec.Tunnels)
	_ = c.record.Write(rec)
}

// runStdio opens one tunnel and copies it to stdin/stdout until it ends.
func (c *client) runStdio(ctx context.Context) error {
	st, entry, err := c.openTunnel(ctx)
	if err != nil {
		return err
	}
	c.addTunnel(entry)
	defer c.removeTunnel(entry)
	// On the stream's FIN, stdout is closed when it can be.
	var halfClose func()
	if cl, ok := c.req.Stdout.(io.Closer); ok {
		halfClose = func() { _ = cl.Close() }
	}
	type result struct {
		err    error
		closed bool
	}
	done := make(chan result, 1)
	go func() {
		closed, err := splice(c.req.Stdin, c.req.Stdout, halfClose, st)
		done <- result{err, closed}
	}()
	select {
	case r := <-done:
		if r.err != nil || !r.closed {
			return r.err
		}
		return c.awaitFlush(ctx, st, entry)
	case <-ctx.Done():
		_ = st.CloseWithCode(conduit.CloseCancelled, "cancelled")
		return ctx.Err()
	}
}

// awaitFlush waits, after a clean end where we closed the stream, until
// the stream's queued data and close have been written, so closing the
// session afterwards cannot drop them. It returns nil at once when the
// stream is already closed (the peer closed first) and nil when our close
// was written; it returns ctx's error on a signal, and an exit-6 error when
// the stream failed, the session ended first or the flush timeout passed.
func (c *client) awaitFlush(ctx context.Context, st conduit.Stream, t *tunnelEntry) error {
	switch streamState(st) {
	case conduit.StateClosed:
		return nil
	case conduit.StateFailed:
		// Failed (for example, the session dropped): queued data may
		// have been lost.
		return c.flushFailed()
	}
	timer := time.NewTimer(c.cfg.flushTimeout())
	defer timer.Stop()
	select {
	case <-t.ended:
		if streamState(st) == conduit.StateClosed {
			return nil
		}
	case <-t.sess.ls.Done():
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
	}
	return c.flushFailed()
}

// flushFailed is the exit for a tunnel whose queued data may not have
// been written.
func (c *client) flushFailed() error {
	return Errorf(ExitHubUnavailable, "the end of the tunnel's data may not have reached the hub at %s", c.cfg.Endpoint)
}

// streamState returns the stream's lifecycle state (StateActive for a
// stream that does not report one).
func streamState(st conduit.Stream) conduit.StreamState {
	if s, ok := st.(interface{ State() conduit.StreamState }); ok {
		return s.State()
	}
	return conduit.StateActive
}

// runListener listens on 127.0.0.1 and opens one tunnel per accepted
// connection, until ctx ends, the session is lost, or a request fails in a
// way no later connection can fix.
func (c *client) runListener(ctx context.Context) error {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(c.req.LocalPort)))
	if err != nil {
		return Errorf(ExitError, "cannot listen on 127.0.0.1:%d: %v", c.req.LocalPort, err)
	}
	defer func() { _ = ln.Close() }()
	if c.req.SSH {
		_, _ = fmt.Fprintf(c.cfg.stderr(), "Forwarding %s to the ssh service of agent %s\n", ln.Addr(), c.req.Agent)
	} else {
		_, _ = fmt.Fprintf(c.cfg.stderr(), "Forwarding %s to port %d of agent %s\n", ln.Addr(), c.req.Port, c.req.Agent)
	}
	if c.req.Ready != nil {
		c.req.Ready(ln.Addr())
	}

	fatal := make(chan error, 1)
	var conns sync.WaitGroup
	connCtx, cancelConns := context.WithCancel(ctx)
	acceptDone := make(chan struct{})
	defer func() {
		cancelConns()
		_ = ln.Close()
		// No conns.Add can run once the accept loop has exited.
		<-acceptDone
		conns.Wait()
	}()
	go func() {
		defer close(acceptDone)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conns.Add(1)
			go func() {
				defer conns.Done()
				if err := c.serveConn(connCtx, conn); err != nil {
					select {
					case fatal <- err:
					default:
					}
				}
			}()
		}
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-c.lost:
		return err
	case err := <-fatal:
		return err
	}
}

// listenerFatal reports whether a failed tunnel request ends the listener:
// failures that no later connection can fix.
func listenerFatal(code int) bool {
	switch code {
	case ExitCredential, ExitAgentNotFound, ExitForbidden, ExitNoTunnelSupport:
		return true
	}
	return false
}

// serveConn serves one local connection. It returns an error only when
// the listener should end.
func (c *client) serveConn(ctx context.Context, conn net.Conn) error {
	defer func() { _ = conn.Close() }()
	st, entry, err := c.openTunnel(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		if listenerFatal(CodeOf(err)) {
			return err
		}
		_, _ = fmt.Fprintf(c.cfg.stderr(), "tunnel request failed: %v\n", err)
		return nil
	}
	c.addTunnel(entry)
	defer c.removeTunnel(entry)
	stop := context.AfterFunc(ctx, func() {
		_ = st.CloseWithCode(conduit.CloseCancelled, "cancelled")
		_ = conn.Close()
	})
	defer stop()
	// On the stream's FIN, the local connection is half-closed (TCP FIN).
	halfClose := func() {
		if cw, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}
	closed, err := splice(conn, conn, halfClose, st)
	if err == nil && closed {
		// Before removeTunnel can close a replaced session.
		err = c.awaitFlush(ctx, st, entry)
	}
	if err != nil && ctx.Err() == nil {
		_, _ = fmt.Fprintf(c.cfg.stderr(), "tunnel closed: %v\n", err)
	}
	return nil
}

// splice copies between a local side and the stream, with a separate FIN
// in each direction. Bytes from r go to the stream; on EOF from r the
// stream is half-closed. Bytes from the stream go to w; on the stream's
// FIN, halfClose (if set) half-closes the local side, and the upload keeps
// running. splice returns when both directions have finished, when the
// stream is closed (a StreamClose, with or without a code), or when the
// session ends, and reports how the stream ended (nil for a clean close).
// closed reports that splice ended cleanly with both directions done and
// closed the stream itself; the caller then waits for that close to be
// written (awaitFlush). A read from r that is still blocked when splice
// returns is left behind.
func splice(r io.Reader, w io.Writer, halfClose func(), st conduit.Stream) (closed bool, err error) {
	up := make(chan struct{})
	go func() {
		defer close(up)
		// io.Copy writes each chunk it reads before reading the next.
		if _, err := io.Copy(st, r); err == nil || !isStreamWriteError(err) {
			if cw, ok := st.(interface{ CloseWrite() error }); ok {
				_ = cw.CloseWrite()
			}
		}
	}()
	down := make(chan error, 1)
	go func() { down <- copyFromStream(w, st) }()
	// ended is closed when the stream ends; conduit streams close their
	// Resizes channel then.
	var ended <-chan conduit.WindowSize
	if rz, ok := st.(conduit.Resizable); ok {
		ended = rz.Resizes()
	}
	// streamEnded records that the stream was closed (by either side, or
	// with the session). Once the download has also finished, in either
	// order, the tunnel is over even if the upload is still open.
	downDone, upDone, streamEnded := false, false, false
	for {
		select {
		case err := <-down:
			down = nil
			downDone = true
			if end := streamEnd(err); end != nil {
				return false, end
			}
			if halfClose != nil {
				halfClose()
			}
			if streamEnded {
				return false, streamEnd(finalReadError(st))
			}
			if upDone {
				_ = st.Close()
				return true, nil
			}
		case <-up:
			up = nil
			upDone = true
			if downDone {
				_ = st.Close()
				return true, nil
			}
		case _, ok := <-ended:
			if ok {
				continue // a resize: not used by tunnels
			}
			ended = nil
			streamEnded = true
			if downDone {
				// Closed after its FIN: report how.
				return false, streamEnd(finalReadError(st))
			}
			// The download sees the close on its next read.
		}
	}
}

// isStreamWriteError reports whether an upload copy failed on the stream
// side (the stream or session is gone) rather than on the local read.
func isStreamWriteError(err error) bool {
	var ce *conduit.CloseError
	return errors.As(err, &ce) || errors.Is(err, conduit.ErrStreamClosed) || errors.Is(err, conduit.ErrSessionClosed)
}

// finalReadError returns the error a read of an ended stream reports: the
// close code or session failure, or io.EOF for a normal close.
func finalReadError(st conduit.Stream) error {
	var b [1]byte
	_, err := st.Read(b[:])
	return err
}

// copyFromStream copies the stream to w until the stream ends, and returns
// the stream's read error (nil or io.EOF for a clean end). A failed local
// write means the local side went away: it ends the copy cleanly.
func copyFromStream(w io.Writer, st conduit.Stream) error {
	buf := make([]byte, 32*1024)
	for {
		n, rerr := st.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return nil
			}
		}
		if rerr != nil {
			return rerr
		}
	}
}

// streamEnd maps how a tunnel stream ended to its exit code.
func streamEnd(err error) error {
	if err == nil || errors.Is(err, io.EOF) {
		return nil
	}
	var ce *conduit.CloseError
	if errors.As(err, &ce) {
		switch ce.Code {
		case conduit.CloseNormal:
			return nil
		case conduit.CloseUnauthenticated:
			return Errorf(ExitPolicyClosed, "tunnel closed by policy (%s)", reasonOr(ce.Reason, "authz_expired"))
		case conduit.CloseForbidden:
			return Errorf(ExitForbidden, "tunnel closed: not permitted (%s)", reasonOr(ce.Reason, "forbidden"))
		case closeTargetNotFound:
			return Errorf(ExitAgentNotFound, "tunnel closed: agent not found or deleted")
		case conduit.CloseRelayRestart:
			return Errorf(ExitHubUnavailable, "tunnel closed: the hub is restarting (%s)", reasonOr(ce.Reason, "relay_restart"))
		case conduit.CloseRelayTimeout:
			return Errorf(ExitTargetUnavailable, "tunnel closed: the agent is unreachable (%s)", reasonOr(ce.Reason, "relay_timeout"))
		}
		return Errorf(ExitError, "tunnel closed with code %d (%s)", ce.Code, ce.Reason)
	}
	if errors.Is(err, conduit.ErrStreamClosed) {
		return nil
	}
	return Errorf(ExitHubUnavailable, "the session to the hub was lost: %v", err)
}

// closeTargetNotFound is the close code for a target that does not exist
// (4404).
const closeTargetNotFound uint32 = 4404

func reasonOr(reason, def string) string {
	if reason == "" {
		return def
	}
	return reason
}
