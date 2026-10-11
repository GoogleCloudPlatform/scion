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
	cfg    Config
	req    Request
	cred   Credential
	id     Identity
	user   User
	target Target

	mu       sync.Mutex
	cur      *session
	sessions []*session
	tunnels  map[*tunnelEntry]struct{}

	// lost receives the error when the current session ends unplanned.
	lost chan error
}

// start resolves the credential, its identity and the target, and dials
// the first session. Nothing reads stdin or prompts.
func (c *client) start(ctx context.Context) error {
	c.cred = c.cfg.ResolveCredential()
	if c.cred.Token == "" {
		return Errorf(ExitCredential, "no usable credential for %s; %s", c.cfg.Endpoint, loginHint(c.cfg.Endpoint))
	}
	user, err := c.cfg.Hub.Whoami(ctx, c.cred)
	if err != nil {
		return c.restError(ctx, err, "checking the credential")
	}
	c.user = user
	c.id = Identity{Kind: c.cred.Kind, Subject: user.ID, Scope: c.cfg.scope().Scope(c.cred)}
	tgt, err := c.cfg.Hub.ResolveAgent(ctx, c.cred, c.req.Project, c.req.Agent)
	if err != nil {
		var he *HTTPError
		if errors.As(err, &he) && he.Status == http.StatusNotFound {
			return Errorf(ExitAgentNotFound, "agent %q not found in project %q", c.req.Agent, c.req.Project)
		}
		return c.restError(ctx, err, "looking up the agent")
	}
	c.target = tgt
	s, err := c.dial(ctx, c.cred)
	if err != nil {
		return err
	}
	c.setCurrent(s)
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
func (c *client) setCurrent(s *session) {
	c.mu.Lock()
	c.cur = s
	c.sessions = append(c.sessions, s)
	c.mu.Unlock()
	go c.watch(s)
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
}

// shutdown closes every session.
func (c *client) shutdown() {
	c.mu.Lock()
	sessions := append([]*session(nil), c.sessions...)
	c.mu.Unlock()
	for _, s := range sessions {
		_ = s.ls.Close()
	}
}

// tunnelEntry is one open tunnel of this process.
type tunnelEntry struct {
	sessionID string
	openedAt  time.Time
	deadline  *time.Time
}

func (c *client) addTunnel(t *tunnelEntry) {
	c.mu.Lock()
	c.tunnels[t] = struct{}{}
	c.mu.Unlock()
}

func (c *client) removeTunnel(t *tunnelEntry) {
	c.mu.Lock()
	delete(c.tunnels, t)
	c.mu.Unlock()
}

// runStdio opens one tunnel and copies it to stdin/stdout until it ends.
func (c *client) runStdio(ctx context.Context) error {
	st, entry, err := c.openTunnel(ctx)
	if err != nil {
		return err
	}
	c.addTunnel(entry)
	defer c.removeTunnel(entry)
	done := make(chan error, 1)
	go func() { done <- splice(c.req.Stdin, c.req.Stdout, nil, st) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = st.CloseWithCode(conduit.CloseCancelled, "cancelled")
		return ctx.Err()
	}
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
	defer func() {
		cancelConns()
		_ = ln.Close()
		conns.Wait()
	}()
	go func() {
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
	if err := splice(conn, conn, conn, st); err != nil && ctx.Err() == nil {
		_, _ = fmt.Fprintf(c.cfg.stderr(), "tunnel closed: %v\n", err)
	}
	return nil
}

// splice copies between a local side (r, w) and the stream until the
// stream ends, and returns how it ended (nil for a clean close). closer,
// if set, is closed when the stream ends so a blocked local read returns.
func splice(r io.Reader, w io.Writer, closer io.Closer, st conduit.Stream) error {
	go func() {
		_, err := io.Copy(st, r)
		if err == nil {
			if cw, ok := st.(interface{ CloseWrite() error }); ok {
				_ = cw.CloseWrite()
				return
			}
		}
		_ = st.Close()
	}()
	err := copyFromStream(w, st)
	if closer != nil {
		_ = closer.Close()
	}
	end := streamEnd(err)
	if end != nil {
		return end
	}
	_ = st.Close()
	return nil
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
