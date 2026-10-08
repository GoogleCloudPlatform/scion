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

package runtimebroker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// ControlChannelConfig holds configuration for the control channel client.
type ControlChannelConfig struct {
	// HubEndpoint is the base URL of the Hub API.
	HubEndpoint string
	// HostID is the unique identifier for this runtime broker.
	BrokerID string
	// SecretKey is the HMAC secret key for authentication.
	SecretKey []byte
	// Version is the runtime broker version string.
	Version string
	// Projects is a list of project IDs this broker serves.
	Projects []string

	// ReconnectBackoff configuration
	ReconnectInitial    time.Duration
	ReconnectMax        time.Duration
	ReconnectMultiplier float64

	// Connection timeouts
	PingInterval time.Duration
	PongWait     time.Duration
	WriteWait    time.Duration

	// Debug enables verbose logging.
	Debug bool

	// TransportSource provides OIDC tokens for transport-layer auth (IAP).
	TransportSource transportauth.TokenSource
	// TransportMode controls which HTTP header carries the transport token.
	TransportMode transportauth.HeaderMode

	// OnConnectionStateChange is called when the control channel connects
	// or disconnects. connected=true after a successful handshake,
	// connected=false when the WebSocket drops.
	OnConnectionStateChange func(connected bool)
}

// DefaultControlChannelConfig returns the default configuration.
func DefaultControlChannelConfig() ControlChannelConfig {
	return ControlChannelConfig{
		ReconnectInitial:    1 * time.Second,
		ReconnectMax:        60 * time.Second,
		ReconnectMultiplier: 2.0,
		PingInterval:        30 * time.Second,
		PongWait:            60 * time.Second,
		WriteWait:           10 * time.Second,
		Debug:               false,
	}
}

// AgentLookupResult holds the result of looking up an agent for PTY attachment.
type AgentLookupResult struct {
	ContainerID string // Container/pod ID
	RuntimeName string // Runtime that owns the agent (e.g., "docker", "kubernetes")
	ExecUser    string // Container user for exec/attach (e.g., "scion" or "root" for rootless Podman)
	Namespace   string // Kubernetes namespace (empty for non-k8s runtimes)

	// Phase is the container runtime's OWN lifecycle phase at lookup time
	// (e.g. "running", "stopped", "created" — see api.AgentInfo.Phase),
	// read directly from the runtime's listing (Server.rawRuntimePhase).
	// This is deliberately NOT agent.Manager's merged phase (which overlays
	// agent-info.json and can lag the runtime's actual state — see
	// pkg/agent/list.go): callers like classifyAttachEnd that need to know
	// whether a container is definitively running right now would get a
	// false answer from that overlay. Empty when the runtime's listing
	// doesn't include this container at lookup time, or the re-list itself
	// failed — callers must treat that as unknown, not stopped.
	Phase string

	// Runtime is the live instance that actually produced this match — the
	// default runtime, or the matched auxiliary runtime — so a caller can
	// ask it capability questions (e.g. scionrt.HasAttachSupport) instead of
	// branching on RuntimeName's type-name string. Set on every match path
	// in Server.LookupAgent.
	Runtime scionrt.Runtime

	// K8sConfig and K8sClientset are set for kubernetes agents so that
	// PTY handlers can use the Go client (remotecommand) instead of
	// shelling out to kubectl (which may not be in PATH or may lack auth).
	K8sConfig    *rest.Config
	K8sClientset kubernetes.Interface
}

// AgentLookup provides agent information for control channel operations.
type AgentLookup interface {
	// LookupContainerID returns the container ID for an agent by its slug/name.
	// projectID scopes the lookup to a specific project to prevent cross-project
	// collision when multiple agents share the same slug. Pass empty string
	// to fall back to name-only lookup (backward compat).
	LookupContainerID(ctx context.Context, slug, projectID string) (containerID string, err error)
	// LookupAgent returns detailed lookup info including the runtime that owns the agent.
	// projectID scopes the lookup to a specific project (same semantics as LookupContainerID).
	LookupAgent(ctx context.Context, slug, projectID string) (*AgentLookupResult, error)
	// RuntimeCommand returns the container runtime command (e.g., "docker", "container").
	RuntimeCommand() string
}

const defaultMaxConcurrentDispatches = 20

// ControlChannelClient manages the WebSocket connection to the Hub.
type ControlChannelClient struct {
	config         ControlChannelConfig
	conn           *wsprotocol.Connection
	handlers       http.Handler // Reuse existing HTTP handlers
	agentLookup    AgentLookup  // For looking up agent container IDs
	connectionName string       // identifies which HubConnection this belongs to
	log            *slog.Logger
	streams        map[string]*StreamHandler
	streamMu       sync.RWMutex

	// dispatchSem limits concurrent async request dispatches to prevent
	// unbounded goroutine growth under load.
	dispatchSem chan struct{}

	// cancels tracks the CancelFunc for each in-flight dispatched request,
	// keyed by RequestID, so a "cancel" message from the Hub (sent when it
	// gives up waiting past its own dispatch timeout, or the original
	// caller's request was itself cancelled) can abort the request's
	// context instead of letting it run to completion uncancellably.
	// Entries are added before the request waits for a dispatch slot and
	// removed when it finishes (see trackRequest).
	cancels  map[string]*requestCancel
	cancelMu sync.Mutex

	// writePing sends one keepalive ping on conn. It is nil in production
	// (conn.WritePing is used); tests set it to simulate a failed write.
	writePing func(conn *wsprotocol.Connection) error

	// Connection state
	connected   bool
	sessionID   string
	connectedAt time.Time
	mu          sync.RWMutex

	// Lifecycle
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// StreamHandler handles a multiplexed stream.
type StreamHandler struct {
	streamID   string
	streamType string
	slug       string
	projectID  string
	dataCh     chan []byte
	resizeCh   chan [2]int // [cols, rows]
	closeCh    chan struct{}
	closed     bool
	closeMu    sync.Mutex

	// input holds client input not yet taken from dataCh. It is set by
	// handleStreamOpen; handlers built directly (tests) feed dataCh instead.
	input *streamInputQueue
}

// StreamInputLimit is the most client input, in bytes, the broker holds for
// one stream before the PTY consumes it. Input is queued only while the
// write into the agent's terminal is blocked (the program in the session is
// not reading stdin fast enough), so the limit is sized well above any
// realistic paste: 4 MiB is about 200 times the 20 KB paste that already
// worked, more than a large source file, and four times the 1 MiB
// control-channel message cap. Exceeding it closes the stream with
// closeCodeInputOverflow; input is never dropped silently.
const StreamInputLimit = 4 << 20

// closeCodeInputOverflow (1009, "message too big") is the close code for a
// stream whose input exceeded StreamInputLimit. The Hub passes it through
// unchanged and clients classify it as terminal, so the client reports it
// instead of reconnecting and pasting again.
const closeCodeInputOverflow = 1009

// closeReasonInputOverflow is the close reason sent with
// closeCodeInputOverflow.
const closeReasonInputOverflow = "input_overflow"

// streamInputQueue is a byte-bounded FIFO of input frames for one stream.
// The control-channel read loop pushes without blocking; run moves frames
// into the stream's dataCh in order. A frame counts against the limit until
// the consumer has taken it from dataCh.
type streamInputQueue struct {
	mu     sync.Mutex
	frames [][]byte
	queued int
	limit  int
	notify chan struct{} // capacity 1
}

func newStreamInputQueue(limit int) *streamInputQueue {
	return &streamInputQueue{limit: limit, notify: make(chan struct{}, 1)}
}

// push queues data. It reports false, queueing nothing, if data would take
// the queue over its limit.
func (q *streamInputQueue) push(data []byte) bool {
	if len(data) == 0 {
		return true
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.queued+len(data) > q.limit {
		return false
	}
	q.frames = append(q.frames, data)
	q.queued += len(data)
	select {
	case q.notify <- struct{}{}:
	default:
	}
	return true
}

// run delivers queued frames to out, in order, until closeCh is closed.
func (q *streamInputQueue) run(out chan<- []byte, closeCh <-chan struct{}) {
	for {
		q.mu.Lock()
		var data []byte
		if len(q.frames) > 0 {
			data = q.frames[0]
		}
		q.mu.Unlock()

		if data == nil {
			select {
			case <-q.notify:
				continue
			case <-closeCh:
				q.discard()
				return
			}
		}

		select {
		case out <- data:
			q.mu.Lock()
			q.frames[0] = nil
			q.frames = q.frames[1:]
			q.queued -= len(data)
			q.mu.Unlock()
		case <-closeCh:
			q.discard()
			return
		}
	}
}

// discard drops any queued frames once the stream has closed.
func (q *streamInputQueue) discard() {
	q.mu.Lock()
	q.frames = nil
	q.queued = 0
	q.mu.Unlock()
}

// NewControlChannelClient creates a new control channel client.
// The connectionName identifies which HubConnection this control channel belongs to,
// enabling request routing to the correct hydrator in multi-hub mode.
func NewControlChannelClient(config ControlChannelConfig, handlers http.Handler, agentLookup AgentLookup, connectionName string, log *slog.Logger) *ControlChannelClient {
	return &ControlChannelClient{
		config:         config,
		handlers:       handlers,
		agentLookup:    agentLookup,
		connectionName: connectionName,
		log:            log,
		streams:        make(map[string]*StreamHandler),
		dispatchSem:    make(chan struct{}, defaultMaxConcurrentDispatches),
		cancels:        make(map[string]*requestCancel),
	}
}

// Connect establishes the WebSocket connection to the Hub.
func (c *ControlChannelClient) Connect(ctx context.Context) error {
	c.mu.Lock()
	if c.connected {
		c.mu.Unlock()
		return nil
	}
	c.ctx, c.cancel = context.WithCancel(ctx)
	c.mu.Unlock()

	return c.connectWithBackoff()
}

// connectWithBackoff attempts to connect with exponential backoff.
func (c *ControlChannelClient) connectWithBackoff() error {
	backoff := c.config.ReconnectInitial
	if backoff == 0 {
		backoff = 1 * time.Second
	}

	for {
		select {
		case <-c.ctx.Done():
			return c.ctx.Err()
		default:
		}

		if err := c.doConnect(); err != nil {
			c.log.Error("Control channel connection failed", "error", err, "retry_in", backoff)

			select {
			case <-c.ctx.Done():
				return c.ctx.Err()
			case <-time.After(backoff):
			}

			// Increase backoff
			backoff = time.Duration(float64(backoff) * c.config.ReconnectMultiplier)
			if c.config.ReconnectMax > 0 && backoff > c.config.ReconnectMax {
				backoff = c.config.ReconnectMax
			}
			continue
		}

		// Successfully connected, run the message loop
		c.runMessageLoop()

		// Connection lost, try to reconnect
		if c.ctx.Err() == nil {
			c.log.Info("Control channel connection lost, reconnecting...")
			backoff = c.config.ReconnectInitial
			if backoff == 0 {
				backoff = 1 * time.Second
			}
		}
	}
}

// doConnect performs the actual WebSocket connection.
func (c *ControlChannelClient) doConnect() error {
	// Build WebSocket URL
	wsURL, err := c.buildWebSocketURL()
	if err != nil {
		return fmt.Errorf("invalid hub endpoint: %w", err)
	}

	// Build signed headers for authentication
	headers, err := c.buildAuthHeaders()
	if err != nil {
		return fmt.Errorf("failed to build auth headers: %w", err)
	}

	// Connect. gorilla/websocket's Dialer returns a non-nil *http.Response on
	// handshake failure (e.g. 401/403), and the application is responsible
	// for closing resp.Body in that case — otherwise the transport holds the
	// connection open and leaks the file descriptor. Drain-and-close before
	// returning the error.
	//
	// Use DialWithConfig so the broker's read limit matches the hub's write
	// limit. The default 64KB was too small for RemoteCreateAgentRequest
	// payloads that include InlineConfig, resolved env/secrets, and a JWT
	// (see issue #165). 1MB aligns with the hub-side MaxMessageSize.
	connCfg := wsprotocol.DefaultConnectionConfig()
	connCfg.MaxMessageSize = 1024 * 1024 // 1MB
	conn, resp, err := wsprotocol.DialWithConfig(c.ctx, wsURL, headers, connCfg)
	if err != nil {
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			return fmt.Errorf("websocket dial failed (status %d): %w", resp.StatusCode, err)
		}
		return fmt.Errorf("websocket dial failed: %w", err)
	}

	c.conn = conn

	// Send connect message
	connectMsg := wsprotocol.NewConnectMessage(c.config.BrokerID, c.config.Version, c.config.Projects)
	if err := conn.WriteJSON(connectMsg); err != nil {
		_ = c.conn.Close()
		return fmt.Errorf("failed to send connect message: %w", err)
	}

	// Wait for connected response
	if err := c.waitForConnected(); err != nil {
		_ = c.conn.Close()
		return fmt.Errorf("connection handshake failed: %w", err)
	}

	// Mark connected only after the full handshake succeeds, so that
	// IsConnected() and the state-change callback reflect reality.
	c.mu.Lock()
	c.connected = true
	c.connectedAt = time.Now()
	cb := c.config.OnConnectionStateChange
	c.mu.Unlock()
	if cb != nil {
		cb(true)
	}

	c.log.Info("Connected to Hub control channel", "sessionID", c.sessionID)
	return nil
}

// buildWebSocketURL constructs the WebSocket URL from the Hub endpoint.
func (c *ControlChannelClient) buildWebSocketURL() (string, error) {
	u, err := url.Parse(c.config.HubEndpoint)
	if err != nil {
		return "", err
	}

	// Convert http(s) to ws(s)
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	case "wss", "ws":
		// Already WebSocket scheme
	default:
		u.Scheme = "ws"
	}

	u.Path = "/api/v1/runtime-brokers/connect"
	return u.String(), nil
}

// buildAuthHeaders creates the HMAC-signed headers for authentication.
func (c *ControlChannelClient) buildAuthHeaders() (http.Header, error) {
	headers := http.Header{}

	if len(c.config.SecretKey) == 0 {
		headers.Set("X-Scion-Broker-ID", c.config.BrokerID)
		// Still apply transport auth even without HMAC (proxy-auth mode)
		if c.config.TransportSource != nil {
			if err := transportauth.ApplyHeaders(headers, c.config.TransportSource, c.config.TransportMode); err != nil {
				return nil, fmt.Errorf("failed to apply transport auth headers: %w", err)
			}
		}
		return headers, nil
	}

	// Build a dummy request for signing
	u, err := url.Parse(c.config.HubEndpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid hub endpoint: %w", err)
	}
	u.Path = "/api/v1/runtime-brokers/connect"

	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return nil, err
	}

	// Apply HMAC auth using the HMACAuth type
	hmacAuth := &apiclient.HMACAuth{
		BrokerID:  c.config.BrokerID,
		SecretKey: c.config.SecretKey,
	}
	if err := hmacAuth.ApplyAuth(req); err != nil {
		return nil, fmt.Errorf("failed to apply HMAC auth: %w", err)
	}

	// Copy the signed headers
	for key := range req.Header {
		headers.Set(key, req.Header.Get(key))
	}

	// Transport-layer OIDC for IAP-protected hubs
	if c.config.TransportSource != nil {
		if err := transportauth.ApplyHeaders(headers, c.config.TransportSource, c.config.TransportMode); err != nil {
			return nil, fmt.Errorf("failed to apply transport auth headers: %w", err)
		}
	}

	return headers, nil
}

// waitForConnected waits for the connected response from the Hub.
func (c *ControlChannelClient) waitForConnected() error {
	// Set read deadline for handshake
	if err := c.conn.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return err
	}

	_, data, err := c.conn.ReadMessage()
	if err != nil {
		return fmt.Errorf("failed to read connected message: %w", err)
	}

	env, err := wsprotocol.ParseEnvelope(data)
	if err != nil {
		return fmt.Errorf("failed to parse message: %w", err)
	}

	if env.Type != wsprotocol.TypeConnected {
		return fmt.Errorf("expected connected message, got %s", env.Type)
	}

	var connected wsprotocol.ConnectedMessage
	if err := json.Unmarshal(data, &connected); err != nil {
		return fmt.Errorf("failed to parse connected message: %w", err)
	}

	c.sessionID = connected.SessionID

	// Update ping interval if specified by Hub
	if connected.PingIntervalMs > 0 {
		c.config.PingInterval = time.Duration(connected.PingIntervalMs) * time.Millisecond
	}

	// Clear read deadline
	return c.conn.SetReadDeadline(time.Time{})
}

// runMessageLoop processes incoming messages on the current connection
// until it fails. On return the connection is closed and its ping loop has
// exited, so a reconnect never leaves the previous connection or its ping
// loop behind.
func (c *ControlChannelClient) runMessageLoop() {
	conn := c.conn

	// Start the ping loop for this connection only. loopDone tells it to
	// exit once this read loop is over; pingDone reports that it has.
	loopDone := make(chan struct{})
	pingDone := make(chan struct{})
	c.wg.Add(1)
	go func() {
		defer close(pingDone)
		c.pingLoop(conn, loopDone)
	}()
	defer func() {
		close(loopDone)
		// Close is idempotent, so this is safe when the ping loop already
		// closed the connection after a failed write.
		_ = conn.Close()
		<-pingDone
	}()

	// Set pong handler
	conn.SetPongHandler(func(appData string) error {
		return conn.SetReadDeadline(time.Now().Add(c.config.PongWait))
	})

	// Set initial read deadline
	if err := conn.SetReadDeadline(time.Now().Add(c.config.PongWait)); err != nil {
		c.log.Error("Failed to set read deadline", "error", err)
		c.markDisconnected()
		return
	}

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		_, data, err := conn.ReadMessage()
		if err != nil {
			if wsprotocol.IsUnexpectedCloseError(err, wsprotocol.CloseGoingAway, wsprotocol.CloseNormalClosure) {
				c.log.Error("Control channel read error", "error", err)
			}
			c.markDisconnected()
			return
		}

		if err := c.handleMessage(data); err != nil {
			c.log.Error("Control channel message handling error", "error", err)
		}
	}
}

// pingLoop sends periodic pings on conn to keep it alive, until done is
// closed or the client is closed. When a ping write fails, the connection is
// treated as lost: pingLoop closes it, which makes the read loop return at
// once and starts the reconnect, instead of leaving the channel half open
// until a read deadline expires.
func (c *ControlChannelClient) pingLoop(conn *wsprotocol.Connection, done <-chan struct{}) {
	defer c.wg.Done()

	ticker := time.NewTicker(c.config.PingInterval)
	defer ticker.Stop()

	writePing := c.writePing
	if writePing == nil {
		writePing = (*wsprotocol.Connection).WritePing
	}

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-done:
			return
		case <-ticker.C:
			if err := writePing(conn); err != nil {
				c.log.Error("Failed to ping Hub; closing control channel to reconnect", "error", err)
				_ = conn.Close()
				return
			}
		}
	}
}

// handleMessage processes a single incoming message.
func (c *ControlChannelClient) handleMessage(data []byte) error {
	env, err := wsprotocol.ParseEnvelope(data)
	if err != nil {
		return fmt.Errorf("failed to parse envelope: %w", err)
	}

	switch env.Type {
	case wsprotocol.TypeRequest:
		return c.handleRequest(data)
	case wsprotocol.TypeCancel:
		return c.handleCancel(data)
	case wsprotocol.TypeStreamOpen:
		return c.handleStreamOpen(data)
	case wsprotocol.TypeStream:
		return c.handleStreamData(data)
	case wsprotocol.TypeStreamClose:
		return c.handleStreamClose(data)
	case wsprotocol.TypeStreamResize:
		return c.handleStreamResize(data)
	case wsprotocol.TypePing:
		return c.conn.WriteJSON(wsprotocol.NewPongMessage())
	default:
		if c.config.Debug {
			c.log.Debug("Unknown control channel message type", "type", env.Type)
		}
		return nil
	}
}

// handleRequest parses the request envelope and dispatches it asynchronously
// so the message loop is never blocked by slow HTTP handlers (e.g. container
// creation that can take 60-90s). Without this, the single-threaded message
// loop cannot call ReadMessage while a handler is running, pong frames are
// never consumed, and the hub's PongWait expires — dropping the WebSocket.
func (c *ControlChannelClient) handleRequest(data []byte) error {
	var req wsprotocol.RequestEnvelope
	if err := json.Unmarshal(data, &req); err != nil {
		return fmt.Errorf("failed to parse request: %w", err)
	}

	if c.config.Debug {
		c.log.Debug("Control channel request", "method", req.Method, "path", req.Path)
	}

	conn := c.conn
	// Register the request's cancel synchronously, on the read loop, before
	// the dispatch goroutine starts. The read loop handles frames in order,
	// so a "cancel" frame the Hub sends for this RequestID always finds it,
	// including while the request is still queued for a dispatch slot
	// (ptone/scion#2877).
	ctx, done := c.trackRequest(req.RequestID)
	c.wg.Add(1)
	go c.runRequest(ctx, done, conn, req)
	return nil
}

// dispatchRequest registers req's cancel and runs it. It is used only by
// tests, which call it directly without going through the read loop; when
// run as a goroutine its registration is therefore asynchronous, unlike
// handleRequest's, which registers on the read loop before starting
// runRequest. The caller must have called c.wg.Add(1).
func (c *ControlChannelClient) dispatchRequest(conn *wsprotocol.Connection, req wsprotocol.RequestEnvelope) {
	ctx, done := c.trackRequest(req.RequestID)
	c.runRequest(ctx, done, conn, req)
}

// trackRequest derives the per-request cancellable context and registers its
// CancelFunc under requestID, so a "cancel" message from the Hub (sent when
// it gives up waiting, e.g. its own dispatch timeout elapsed or the original
// caller's request was itself cancelled) can abort the request: while it is
// queued for a dispatch slot, or while its handler runs. Without this, a
// slow create (e.g. a cold-start container/sandbox build) keeps running and
// can leak a started sandbox the Hub no longer knows about, and a queued
// request keeps its place in the queue after nobody is waiting for it.
//
// The returned done func unregisters and cancels the context; call it
// exactly once, when the request has finished.
func (c *ControlChannelClient) trackRequest(requestID string) (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	entry := c.registerCancel(requestID, cancel)
	return ctx, func() {
		c.unregisterCancel(requestID, entry)
		cancel()
	}
}

// runRequest runs the HTTP handler for a tunneled request and sends the
// response back over the WebSocket. It acquires the dispatch semaphore to
// bound concurrency and releases it when done. ctx is the request's own
// context from trackRequest; done is called when runRequest returns.
//
// A request whose ctx is cancelled before it obtains a dispatch slot leaves
// the queue at once: its handler never runs, so nothing it would have done
// (for keys, terminal injection) can happen, and no response is sent — the
// Hub has already stopped waiting for this RequestID. This never reports
// non-execution to the Hub; the Hub decides its own outcome when it gives up
// (for keys, keys_outcome_unknown unless non-execution is proven otherwise).
func (c *ControlChannelClient) runRequest(ctx context.Context, done func(), conn *wsprotocol.Connection, req wsprotocol.RequestEnvelope) {
	defer c.wg.Done()
	defer done()

	// Acquire dispatch semaphore to limit concurrent goroutines.
	select {
	case c.dispatchSem <- struct{}{}:
		defer func() { <-c.dispatchSem }()
	case <-ctx.Done():
		c.logQueuedCancel(req)
		return
	case <-c.ctx.Done():
		return
	}
	// select picks at random when a slot and the cancel are both ready:
	// recheck so a request cancelled while queued never runs its handler.
	if ctx.Err() != nil {
		c.logQueuedCancel(req)
		return
	}

	// Extract trace context from request envelope headers for cross-component propagation.
	ctx = otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier(req.Headers))
	ctx, span := tracer.Start(ctx, "broker.controlchannel.dispatch")
	defer span.End()
	span.SetAttributes(
		attribute.String("scion.request.method", req.Method),
		attribute.String("scion.request.path", req.Path),
	)

	// Recover from panics (e.g. httptest.NewRequest on malformed URLs) to
	// prevent crashing the broker process. Send a 400 error back instead.
	defer func() {
		if r := recover(); r != nil {
			span.SetStatus(codes.Error, fmt.Sprintf("panic: %v", r))
			c.log.Error("Panic in control channel request handler", "panic", r, "method", req.Method, "path", req.Path)
			resp := wsprotocol.NewResponseEnvelope(req.RequestID, http.StatusBadRequest, nil, []byte(fmt.Sprintf(`{"error":"request caused panic: %v"}`, r)))
			if writeErr := conn.WriteJSON(resp); writeErr != nil {
				c.log.Error("Failed to send panic error response", "error", writeErr)
			}
		}
	}()

	// Build HTTP request
	path := req.Path
	if req.Query != "" {
		path = path + "?" + req.Query
	}

	var body io.Reader
	if len(req.Body) > 0 {
		body = bytes.NewReader(req.Body)
	}

	httpReq := httptest.NewRequest(req.Method, path, body)
	httpReq = httpReq.WithContext(ctx)
	for key, value := range req.Headers {
		httpReq.Header.Set(key, value)
	}

	// Inject connection name header so the server can route to the correct hydrator
	if c.connectionName != "" {
		httpReq.Header.Set("X-Scion-Hub-Connection", c.connectionName)
	}

	// Execute through existing handlers
	w := httptest.NewRecorder()
	c.handlers.ServeHTTP(w, httpReq)

	// Build response envelope
	result := w.Result()
	respBody, _ := io.ReadAll(result.Body)
	_ = result.Body.Close()

	headers := make(map[string]string)
	for key := range result.Header {
		headers[key] = result.Header.Get(key)
	}

	resp := wsprotocol.NewResponseEnvelope(req.RequestID, result.StatusCode, headers, respBody)

	if err := conn.WriteJSON(resp); err != nil {
		span.SetStatus(codes.Error, "failed to send response: "+err.Error())
		c.log.Error("Failed to send response", "error", err, "requestID", req.RequestID)
	}
}

// logQueuedCancel records, at debug level, that req was cancelled by the Hub
// before it obtained a dispatch slot and so never ran.
func (c *ControlChannelClient) logQueuedCancel(req wsprotocol.RequestEnvelope) {
	if c.config.Debug {
		c.log.Debug("Control channel request cancelled while queued; not dispatched",
			"requestID", req.RequestID, "method", req.Method, "path", req.Path)
	}
}

// requestCancel is one registration in ControlChannelClient.cancels. Its
// pointer identity lets unregisterCancel remove only its own entry.
type requestCancel struct {
	cancel context.CancelFunc
}

// registerCancel records the CancelFunc for a dispatched request so a later
// "cancel" message from the Hub can abort it, and returns the registration
// for unregisterCancel.
//
// RequestIDs are unique in practice (the Hub generates a UUID per request).
// Handling of a reused ID is best-effort: if one is reused while an earlier
// request with the same ID is still tracked, the new entry replaces the old
// one and, while it remains, a cancel for that ID cancels both requests. If
// the later request finishes first, its unregister removes the ID, and the
// earlier request can no longer be cancelled by a cancel frame.
//
// cancelMu guards only the map: it is never held while calling a handler or
// a CancelFunc, so the read loop cannot block on a running handler here.
func (c *ControlChannelClient) registerCancel(requestID string, cancel context.CancelFunc) *requestCancel {
	c.cancelMu.Lock()
	defer c.cancelMu.Unlock()
	if c.cancels == nil {
		// Defensive lazy-init: NewControlChannelClient always sets this up,
		// but tests and other callers sometimes build a ControlChannelClient
		// via struct literal.
		c.cancels = make(map[string]*requestCancel)
	}
	entry := &requestCancel{cancel: cancel}
	if prev, ok := c.cancels[requestID]; ok {
		prevCancel := prev.cancel
		entry.cancel = func() {
			cancel()
			prevCancel()
		}
	}
	c.cancels[requestID] = entry
	return entry
}

// unregisterCancel removes entry once its request has finished
// (successfully, with an error, or via cancellation, whether it ran or was
// cancelled while queued), so handleCancel can no longer find it. It leaves
// a newer registration for the same RequestID in place. If entry is the
// newer registration, removing it also drops the earlier request's only
// route to handleCancel (see registerCancel).
func (c *ControlChannelClient) unregisterCancel(requestID string, entry *requestCancel) {
	c.cancelMu.Lock()
	if c.cancels[requestID] == entry {
		delete(c.cancels, requestID)
	}
	c.cancelMu.Unlock()
}

// handleCancel aborts the context of an in-flight dispatched request in
// response to a "cancel" message from the Hub. The Hub sends this when it
// gives up waiting for a response — its own dispatch timeout elapsed, or
// the original caller's request was itself cancelled — so this request's
// work can stop instead of continuing to run (and potentially leak a
// started sandbox) after nobody is listening for the result. If the request
// already completed or is unknown (e.g. an old Hub replaying a stale
// RequestID, or the cancel arrived after the response was sent), this is a
// no-op.
func (c *ControlChannelClient) handleCancel(data []byte) error {
	var msg wsprotocol.CancelMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return fmt.Errorf("failed to parse cancel message: %w", err)
	}

	c.cancelMu.Lock()
	entry, ok := c.cancels[msg.RequestID]
	c.cancelMu.Unlock()

	if !ok {
		if c.config.Debug {
			c.log.Debug("Cancel for unknown or already-completed request", "requestID", msg.RequestID)
		}
		return nil
	}

	if c.config.Debug {
		c.log.Debug("Cancelling in-flight request", "requestID", msg.RequestID)
	}
	entry.cancel()
	return nil
}

// handleStreamOpen processes a stream open request.
func (c *ControlChannelClient) handleStreamOpen(data []byte) error {
	var open wsprotocol.StreamOpenMessage
	if err := json.Unmarshal(data, &open); err != nil {
		return fmt.Errorf("failed to parse stream open: %w", err)
	}

	if c.config.Debug {
		c.log.Debug("Stream open requested via control channel",
			"streamID", open.StreamID,
			"type", open.StreamType,
			"slug", open.Slug,
		)
	}

	// Create stream handler
	handler := &StreamHandler{
		streamID:   open.StreamID,
		streamType: open.StreamType,
		slug:       open.Slug,
		projectID:  open.ProjectID,
		// Unbuffered: input waits in the byte-bounded input queue instead.
		dataCh:   make(chan []byte),
		resizeCh: make(chan [2]int, 8),
		closeCh:  make(chan struct{}),
		input:    newStreamInputQueue(StreamInputLimit),
	}

	c.streamMu.Lock()
	c.streams[open.StreamID] = handler
	c.streamMu.Unlock()

	go handler.input.run(handler.dataCh, handler.closeCh)

	// Start stream handler based on type
	switch open.StreamType {
	case wsprotocol.StreamTypePTY:
		go c.handlePTYStream(handler, open.Cols, open.Rows)
	default:
		c.log.Debug("Unknown stream type", "type", open.StreamType)
	}

	return nil
}

// handleStreamData processes incoming stream data.
func (c *ControlChannelClient) handleStreamData(data []byte) error {
	var frame wsprotocol.StreamFrame
	if err := json.Unmarshal(data, &frame); err != nil {
		return fmt.Errorf("failed to parse stream frame: %w", err)
	}

	c.streamMu.RLock()
	handler, ok := c.streams[frame.StreamID]
	c.streamMu.RUnlock()

	if !ok {
		if c.config.Debug {
			c.log.Debug("Data for unknown stream", "streamID", frame.StreamID)
		}
		return nil
	}

	if handler.isClosed() {
		// Already closed with a reported code (e.g. input overflow) and
		// waiting for the Hub's close; late input is not delivered.
		return nil
	}

	if handler.input == nil {
		// Only handlers built outside handleStreamOpen (tests, with a
		// buffered dataCh) lack an input queue. Hand off without blocking
		// the read loop; on a full channel, close with the overflow code
		// rather than dropping the frame.
		select {
		case handler.dataCh <- frame.Data:
		default:
			c.closeStreamAsync(handler, closeReasonInputOverflow, closeCodeInputOverflow)
		}
		return nil
	}

	if !handler.input.push(frame.Data) {
		c.log.Warn("Stream input exceeded the buffer limit; closing stream",
			"streamID", frame.StreamID, "limitBytes", StreamInputLimit)
		c.closeStreamAsync(handler, closeReasonInputOverflow, closeCodeInputOverflow)
	}

	return nil
}

// isClosed reports whether the stream has been closed.
func (h *StreamHandler) isClosed() bool {
	h.closeMu.Lock()
	defer h.closeMu.Unlock()
	return h.closed
}

// claimClose closes the stream locally and reports whether this call did
// it. Exactly one path claims each stream, and only a claiming path that is
// responsible for telling the Hub (closeStreamAsync, CloseStream) sends a
// StreamClose, so the Hub sees one close code per stream.
func (h *StreamHandler) claimClose() bool {
	h.closeMu.Lock()
	defer h.closeMu.Unlock()
	if h.closed {
		return false
	}
	h.closed = true
	close(h.closeCh)
	return true
}

// closeStreamAsync closes handler's stream locally at once and, if this call
// claimed the close, reports it to the Hub from a separate goroutine so the
// read loop never waits on the write. The handler stays registered, marked
// closed, until the Hub's StreamClose or a CloseStream call removes it; a
// CloseStream that finds it already claimed sends nothing, so a PTY
// goroutine finishing at the same time cannot report a second code.
//
// It reports whether a report was started: false if another path already
// closed the stream, or if the client is closing.
func (c *ControlChannelClient) closeStreamAsync(handler *StreamHandler, reason string, code int) bool {
	if !handler.claimClose() {
		return false
	}
	conn := c.conn
	closeMsg := wsprotocol.NewStreamCloseMessage(handler.streamID, reason, code)
	return c.goTracked(func() {
		if err := conn.WriteJSON(closeMsg); err != nil {
			c.log.Warn("Failed to report stream close to Hub", "streamID", handler.streamID, "code", code, "error", err)
		}
	})
}

// goTracked runs f in a goroutine counted by c.wg, unless Close has started.
// The read loop is not itself tracked and can still be handling a frame
// after Close cancels c.ctx, so an unguarded c.wg.Add there could run
// concurrently with Close's c.wg.Wait at a zero count, which WaitGroup does
// not allow. Checking c.ctx and adding under c.mu, which Close holds while
// cancelling, orders every Add either before Close's Wait or not at all.
func (c *ControlChannelClient) goTracked(f func()) bool {
	c.mu.Lock()
	if c.ctx != nil && c.ctx.Err() != nil {
		c.mu.Unlock()
		return false
	}
	c.wg.Add(1)
	c.mu.Unlock()
	go func() {
		defer c.wg.Done()
		f()
	}()
	return true
}

// handleStreamClose processes a stream close message.
func (c *ControlChannelClient) handleStreamClose(data []byte) error {
	var closeMsg wsprotocol.StreamCloseMessage
	if err := json.Unmarshal(data, &closeMsg); err != nil {
		return fmt.Errorf("failed to parse stream close: %w", err)
	}

	c.streamMu.Lock()
	handler, ok := c.streams[closeMsg.StreamID]
	if ok {
		delete(c.streams, closeMsg.StreamID)
	}
	c.streamMu.Unlock()

	if handler != nil {
		handler.closeMu.Lock()
		if !handler.closed {
			handler.closed = true
			close(handler.closeCh)
		}
		handler.closeMu.Unlock()
	}

	if c.config.Debug {
		c.log.Debug("Control channel stream closed", "streamID", closeMsg.StreamID, "reason", closeMsg.Reason)
	}

	return nil
}

// handleStreamResize processes a stream resize message.
func (c *ControlChannelClient) handleStreamResize(data []byte) error {
	var resizeMsg wsprotocol.StreamResizeMessage
	if err := json.Unmarshal(data, &resizeMsg); err != nil {
		return fmt.Errorf("failed to parse stream resize: %w", err)
	}

	c.streamMu.RLock()
	handler, ok := c.streams[resizeMsg.StreamID]
	c.streamMu.RUnlock()

	if !ok {
		return nil // Stream not found, ignore
	}

	// Send resize to handler (non-blocking)
	select {
	case handler.resizeCh <- [2]int{resizeMsg.Cols, resizeMsg.Rows}:
	default:
		// Channel full, drop oldest resize
		select {
		case <-handler.resizeCh:
		default:
		}
		handler.resizeCh <- [2]int{resizeMsg.Cols, resizeMsg.Rows}
	}

	return nil
}

// handlePTYStream handles a PTY stream by looking up the agent and starting a PTY session.
func (c *ControlChannelClient) handlePTYStream(handler *StreamHandler, cols, rows int) {
	c.log.Info("PTY stream started via control channel",
		"slug", handler.slug,
		"cols", cols,
		"rows", rows,
	)

	// Look up the container ID for this agent
	if c.agentLookup == nil {
		c.log.Error("PTY stream failed: no agent lookup configured", "slug", handler.slug)
		_ = c.CloseStream(handler.streamID, wsprotocol.CloseReasonInternalError, wsprotocol.ClosePTYInternalError)
		return
	}

	result, err := c.agentLookup.LookupAgent(c.ctx, handler.slug, handler.projectID)
	if err != nil {
		if errors.Is(err, ErrAgentListUnavailable) {
			// The container runtime itself failed to answer (e.g. an
			// intermittent `docker ps` error), not "no such agent". Send a
			// retriable code so the client re-attaches instead of giving up.
			c.log.Warn("PTY stream failed: agent lookup unavailable", "slug", handler.slug, "error", err)
			_ = c.CloseStream(handler.streamID, wsprotocol.CloseReasonRuntimeUnavailable, wsprotocol.ClosePTYUpstreamUnavailable)
			return
		}
		c.log.Error("PTY stream failed: agent lookup error", "slug", handler.slug, "error", err)
		_ = c.CloseStream(handler.streamID, wsprotocol.CloseReasonAgentNotFound, wsprotocol.ClosePTYAgentNotFound)
		return
	}
	if result == nil {
		// A well-behaved AgentLookup never returns (nil, nil); don't
		// dereference it or guess the agent is gone if one does — treat it
		// the same as a list-unavailable failure and retry.
		c.log.Error("PTY stream failed: agent lookup returned no result and no error", "slug", handler.slug)
		_ = c.CloseStream(handler.streamID, wsprotocol.CloseReasonRuntimeUnavailable, wsprotocol.ClosePTYUpstreamUnavailable)
		return
	}

	if result.ContainerID == "" {
		c.log.Error("PTY stream failed: container not found", "slug", handler.slug)
		_ = c.CloseStream(handler.streamID, wsprotocol.CloseReasonAgentNotFound, wsprotocol.ClosePTYAgentNotFound)
		return
	}

	// The container is already definitively stopped: no tmux session will
	// ever come up in it, so there's no reason to start the PTY session and
	// let it exhaust the full waitForTmuxSession timeout before
	// classifyAttachEnd reaches the same conclusion post-hoc. This is the
	// same runtime-authoritative Phase signal classifyAttachEnd's
	// containerRunningState uses; an unknown or running phase falls through
	// to the normal attach path unchanged.
	if runningResultFromPhase(result.Phase) == runningNo {
		c.log.Info("PTY stream: container is stopped, ending the attach without waiting for tmux", "slug", handler.slug)
		_ = c.CloseStream(handler.streamID, wsprotocol.CloseReasonAgentStopped, wsprotocol.ClosePTYSessionGone)
		return
	}

	c.log.Debug("PTY stream found container", "slug", handler.slug, "containerID", result.ContainerID, "runtime", result.RuntimeName)

	// Get the runtime command - use agent-specific runtime if available, else default
	runtimeCmd := result.RuntimeName
	if runtimeCmd == "" {
		runtimeCmd = c.agentLookup.RuntimeCommand()
	}

	// Reject before starting the tmux exec when the matched runtime has no
	// exec/attach/TTY primitive at all (scionrt.HasAttachSupport, asked of
	// the live instance the lookup above actually matched) — the same
	// pre-upgrade rejection handleAgentAttach applies to the direct-connect
	// path, applied here before the control-channel stream does any
	// runtime-specific work. Without this, an opted-out runtime only fails
	// once StreamPTYHandler.Run() actually tries to start it, at a point
	// where the Hub has already told its own client the stream is open.
	//
	// This is a distinct, terminal close code (4501/attach_unsupported),
	// distinct from the retriable 4503/session_not_ready used for a
	// readiness failure: a runtime that will never support attach must not
	// look the same on the wire as a broker that is merely still starting up.
	if !scionrt.HasAttachSupport(result.Runtime) {
		c.log.Info("PTY stream: runtime does not support attach", "slug", handler.slug, "runtime", runtimeCmd)
		_ = c.CloseStream(handler.streamID, wsprotocol.CloseReasonAttachUnsupported, wsprotocol.ClosePTYAttachUnsupported)
		return
	}

	// Start the actual PTY session. handlePTYStreamWithAgent classifies why
	// it ended and reports the close itself (or skips reporting entirely if
	// the Hub already closed the stream).
	c.handlePTYStreamWithAgent(handler, cols, rows, result.ContainerID, runtimeCmd, result.ExecUser, result.Namespace, result.K8sConfig, result.K8sClientset)

	c.log.Info("PTY stream ended via control channel", "slug", handler.slug)
}

// SendStreamData sends data on a stream.
func (c *ControlChannelClient) SendStreamData(streamID string, data []byte) error {
	c.mu.RLock()
	connected := c.connected
	c.mu.RUnlock()

	if !connected {
		return fmt.Errorf("not connected")
	}

	frame := wsprotocol.NewStreamFrame(streamID, data)
	return c.conn.WriteJSON(frame)
}

// CloseStream closes a stream and reports code and reason to the Hub. If the
// stream is still registered but another path already closed it with a
// reported code (input overflow), CloseStream only unregisters it and sends
// nothing, so the Hub sees a single close code.
func (c *ControlChannelClient) CloseStream(streamID, reason string, code int) error {
	c.streamMu.Lock()
	handler, ok := c.streams[streamID]
	if ok {
		delete(c.streams, streamID)
	}
	c.streamMu.Unlock()

	if handler != nil && !handler.claimClose() {
		// Another path (input overflow) already closed the stream and
		// reported its code to the Hub; do not send a second one.
		return nil
	}

	closeMsg := wsprotocol.NewStreamCloseMessage(streamID, reason, code)
	return c.conn.WriteJSON(closeMsg)
}

// markDisconnected updates the connection state.
func (c *ControlChannelClient) markDisconnected() {
	c.mu.Lock()
	c.connected = false
	cb := c.config.OnConnectionStateChange
	c.mu.Unlock()
	if cb != nil {
		cb(false)
	}

	// Close all streams
	c.streamMu.Lock()
	for _, handler := range c.streams {
		handler.closeMu.Lock()
		if !handler.closed {
			handler.closed = true
			close(handler.closeCh)
		}
		handler.closeMu.Unlock()
	}
	c.streams = make(map[string]*StreamHandler)
	c.streamMu.Unlock()
}

// Close closes the control channel connection.
func (c *ControlChannelClient) Close() error {
	c.mu.Lock()
	if c.cancel != nil {
		c.cancel()
	}
	c.mu.Unlock()

	c.wg.Wait()

	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// IsConnected returns whether the control channel is connected.
func (c *ControlChannelClient) IsConnected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.connected
}

// SessionID returns the current session ID.
func (c *ControlChannelClient) SessionID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.sessionID
}
