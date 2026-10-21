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
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/templatecache"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth/adcsource"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// ConnectionStatus represents the state of a hub connection.
type ConnectionStatus string

const (
	// ConnectionStatusConnected indicates the connection is active and healthy.
	ConnectionStatusConnected ConnectionStatus = "connected"
	// ConnectionStatusDisconnected indicates the connection is not active.
	ConnectionStatusDisconnected ConnectionStatus = "disconnected"
	// ConnectionStatusReconnecting indicates the control channel dropped and is attempting to reconnect.
	ConnectionStatusReconnecting ConnectionStatus = "reconnecting"
	// ConnectionStatusError indicates the connection encountered an error.
	ConnectionStatusError ConnectionStatus = "error"
)

// HubConnection encapsulates all per-hub state for a single hub connection.
type HubConnection struct {
	Name        string // "local", "prod", "hub-scion-dev"
	HubEndpoint string
	BrokerID    string
	AuthMode    brokercredentials.AuthMode

	Credentials *brokercredentials.BrokerCredentials
	SecretKey   []byte // decoded from Credentials.SecretKey

	// TransportSource and TransportMode are resolved per-connection for
	// the control channel WebSocket. The REST hubclient handles its own
	// transport auth via WithTransportAuth(), but the control channel
	// dials directly and needs these to build auth headers.
	TransportSource transportauth.TokenSource
	TransportMode   transportauth.HeaderMode

	HubClient      hubclient.Client
	Hydrator       *templatecache.Hydrator
	HCResolver     *templatecache.Resolver // harness-config hydrator
	Heartbeat      *HeartbeatService
	ControlChannel *ControlChannelClient

	// IsColocated indicates this connection is to a co-located hub running in
	// the same process.
	IsColocated bool

	// LocalStorage is the co-located Hub's storage backend when it is backed by
	// the local filesystem. When set, the broker resolves resources by reading
	// directly from the backend's on-disk location, bypassing the signed-URL/
	// HTTP download path and the hydration cache entirely. It is nil for remote
	// connections and for co-located hubs using a non-local backend (e.g. GCS),
	// which hydrate through the cache like any other remote broker.
	LocalStorage storage.Storage

	Status ConnectionStatus
	mu     sync.RWMutex

	// lifecycleMu makes Start, Stop, close and Reinitialize on this connection run
	// one at a time. It is held for the whole operation, including the
	// blocking waits in Stop. Lock order: lifecycleMu, then hc.mu (a leaf
	// lock). Nothing that holds lifecycleMu takes it again: the exported
	// methods take it and call the unexported start/stop/reinitialize, which
	// never take it. Callers must not hold s.hubMu while calling the
	// exported methods, because Start takes s.hubMu (via the heartbeat's
	// project filter).
	lifecycleMu sync.Mutex

	// reinitCreds is the latest credential set requested through
	// requestReinitialize, reinitGen counts those requests, and
	// reinitAppliedGen is the request most recently taken up by
	// applyRequestedReinitialize. All three are guarded by hc.mu.
	reinitCreds      *brokercredentials.BrokerCredentials
	reinitGen        uint64
	reinitAppliedGen uint64
	// reinitFailed is set when the reinitialize for the request most
	// recently taken up failed, so retryFailedReinitialize can request
	// the same set again. Guarded by hc.mu.
	reinitFailed bool

	// Retry backoff for a credential set whose reinitialize keeps failing
	// (see retryFailedReinitialize). All guarded by hc.mu.
	//
	// reinitFailures counts consecutive failed reinitializes of the
	// latest requested set, and reinitNextRetry is the earliest time
	// retryFailedReinitialize requests another retry. Both are reset when
	// a reinitialize succeeds and when requestReinitialize records a set.
	// reinitLastErr is the error text of the latest failure.
	reinitFailures  int
	reinitNextRetry time.Time
	reinitLastErr   string

	// now, for tests only, replaces time.Now as the clock for the retry
	// backoff. nil means time.Now. Set before the connection is used.
	now func() time.Time

	// closed is set by close, the final stop used when the connection is
	// removed or the server shuts down. Once set, start and
	// applyRequestedReinitialize do nothing, so a Start or reinitialize
	// that was queued on lifecycleMu behind close cannot restart services
	// that nothing would stop again. Guarded by hc.mu; written only with
	// lifecycleMu held.
	closed bool

	// ccWg tracks the control-channel Connect goroutine spawned in Start so
	// that Stop / Reinitialize can wait for it to exit before replacing or
	// clearing ControlChannel. Without this, Reinitialize can race with the
	// previous Connect goroutine and leak goroutines across reconnects.
	ccWg sync.WaitGroup
	// ccCancel cancels the context the control channel's Connect runs
	// under. Stop calls it before Close: Close alone does nothing when the
	// Connect goroutine has not started yet, which would leave it
	// retrying and Stop waiting on ccWg. Guarded by hc.mu.
	ccCancel context.CancelFunc

	// conduitCancel stops the conduit dialer started in Start, and
	// conduitWg tracks its goroutine. The conduit session runs alongside
	// the control channel with its own lifecycle (conduit_dial.go).
	conduitCancel context.CancelFunc
	conduitWg     sync.WaitGroup
	// conduitExitHook, when set, runs on the dialer goroutine after the
	// dialer returned, before the goroutine ends (tests only).
	conduitExitHook func()
}

// GetStatus returns the current connection status.
func (hc *HubConnection) GetStatus() ConnectionStatus {
	hc.mu.RLock()
	defer hc.mu.RUnlock()
	return hc.Status
}

// hubConnectionSnapshot is a point-in-time copy of the HubConnection fields
// that are read from goroutines other than the one running Start, Stop or
// Reinitialize. Credentials and SecretKey are replaced wholesale on
// Reinitialize, never mutated in place, so sharing them is safe.
type hubConnectionSnapshot struct {
	HubEndpoint       string
	BrokerID          string
	AuthMode          brokercredentials.AuthMode
	Credentials       *brokercredentials.BrokerCredentials
	SecretKey         []byte
	Heartbeat         *HeartbeatService
	HasControlChannel bool
}

// snapshot returns a copy of the concurrently read fields, taken under hc.mu.
func (hc *HubConnection) snapshot() hubConnectionSnapshot {
	hc.mu.RLock()
	defer hc.mu.RUnlock()
	return hubConnectionSnapshot{
		HubEndpoint:       hc.HubEndpoint,
		BrokerID:          hc.BrokerID,
		AuthMode:          hc.AuthMode,
		Credentials:       hc.Credentials,
		SecretKey:         hc.SecretKey,
		Heartbeat:         hc.Heartbeat,
		HasControlChannel: hc.ControlChannel != nil,
	}
}

// setStatus updates the connection status.
func (hc *HubConnection) setStatus(status ConnectionStatus) {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	hc.Status = status
}

// Start starts the heartbeat and control channel services for this connection.
// If services from an earlier Start are still running, they are stopped first.
func (hc *HubConnection) Start(ctx context.Context, server *Server) error {
	hc.lifecycleMu.Lock()
	defer hc.lifecycleMu.Unlock()
	return hc.start(ctx, server)
}

// start is Start without lifecycleMu; the caller holds it.
func (hc *HubConnection) start(ctx context.Context, server *Server) error {
	if hc.isClosed() {
		slog.Debug("Not starting closed hub connection", "name", hc.Name)
		return nil
	}

	// Stop anything a previous start left running, so that its control
	// channel, conduit and heartbeat are not overwritten and orphaned.
	hc.mu.RLock()
	running := hc.ControlChannel != nil || hc.ccCancel != nil || hc.Heartbeat != nil || hc.conduitCancel != nil
	hc.mu.RUnlock()
	if running {
		hc.stop()
	}

	// A flat instance's heartbeat and control channel run only under its
	// own persisted Runtime Broker ID.
	if fi := server.flatInstance(); fi != nil && hc.BrokerID != fi.Identity.RuntimeBrokerID {
		return fmt.Errorf("flat Runtime Broker instance: hub connection %q has Runtime Broker %s, not the instance's %s; no heartbeat or control channel is started",
			hc.Name, hc.BrokerID, fi.Identity.RuntimeBrokerID)
	}
	hasValidCredentials := hc.Credentials != nil && hc.Credentials.SecretKey != ""

	// Start heartbeat service if enabled.
	if server.config.HeartbeatEnabled && hc.HubClient != nil && hc.BrokerID != "" {
		if !hasValidCredentials {
			slog.Warn("Not registered with the Hub; heartbeats are off for this connection. Run scion broker register", "name", hc.Name)
		} else {
			interval := server.config.HeartbeatInterval
			if interval <= 0 {
				interval = DefaultHeartbeatInterval
			}

			hb := server.newHeartbeatService(hc.HubClient.RuntimeBrokers(), hc.BrokerID, hc.HubEndpoint, interval)
			hc.mu.Lock()
			hc.Heartbeat = hb
			hc.mu.Unlock()
			hb.Start(ctx)
			slog.Info("Heartbeat started for hub connection", "name", hc.Name, "interval", interval)
		}
	}

	// Start control channel if enabled
	if server.config.ControlChannelEnabled && hc.HubEndpoint != "" && hc.BrokerID != "" {
		if !hasValidCredentials {
			slog.Warn("Skipping control channel for connection: no valid credentials", "name", hc.Name)
		} else {
			ccConfig := ControlChannelConfig{
				HubEndpoint:         hc.HubEndpoint,
				BrokerID:            hc.BrokerID,
				SecretKey:           hc.SecretKey,
				Version:             server.version,
				ReconnectInitial:    1 * time.Second,
				ReconnectMax:        60 * time.Second,
				ReconnectMultiplier: 2.0,
				PingInterval:        30 * time.Second,
				PongWait:            60 * time.Second,
				WriteWait:           10 * time.Second,
				Debug:               server.config.Debug,
				TransportSource:     hc.TransportSource,
				TransportMode:       hc.TransportMode,
				OnConnectionStateChange: func(connected bool) {
					if connected {
						hc.setStatus(ConnectionStatusConnected)
					} else {
						hc.setStatus(ConnectionStatusReconnecting)
					}
				},
			}

			cc := NewControlChannelClient(ccConfig, server.Handler(), server, hc.Name, logging.Subsystem("broker.control-channel"))
			ccCtx, ccCancel := context.WithCancel(ctx)
			hc.mu.Lock()
			hc.ControlChannel = cc
			hc.ccCancel = ccCancel
			hc.mu.Unlock()
			// Capture cc locally so the goroutine doesn't race with Stop()
			// nil-ing hc.ControlChannel out from under it.
			hc.ccWg.Add(1)
			go func() {
				defer hc.ccWg.Done()
				if err := cc.Connect(ccCtx); err != nil {
					if ccCtx.Err() != nil {
						slog.Info("Control channel stopped", "name", hc.Name)
					} else {
						slog.Error("Control channel error", "name", hc.Name, "error", err)
					}
				}
			}()
			slog.Info("Connecting to Hub control channel", "name", hc.Name, "endpoint", hc.HubEndpoint)
			hc.startConduit(ctx, server)
		}
	}

	hc.setStatus(ConnectionStatusConnected)
	return nil
}

// Stop stops the heartbeat and control channel services for this connection.
// Stop waits for any in-flight control-channel Connect goroutine to return
// before clearing ControlChannel so that a subsequent Start / Reinitialize
// cannot race with the previous incarnation.
func (hc *HubConnection) Stop() {
	hc.lifecycleMu.Lock()
	defer hc.lifecycleMu.Unlock()
	hc.stop()
}

// close stops the connection's services for good: it is the final stop,
// used when the connection is removed or the server shuts down. A Start
// or reinitialize that runs after close does nothing.
func (hc *HubConnection) close() {
	hc.lifecycleMu.Lock()
	defer hc.lifecycleMu.Unlock()
	hc.mu.Lock()
	hc.closed = true
	hc.mu.Unlock()
	hc.stop()
}

// isClosed reports whether close has run.
func (hc *HubConnection) isClosed() bool {
	hc.mu.RLock()
	defer hc.mu.RUnlock()
	return hc.closed
}

// stop is Stop without lifecycleMu; the caller holds it.
func (hc *HubConnection) stop() {
	hc.mu.Lock()
	cc := hc.ControlChannel
	hc.ControlChannel = nil
	ccCancel := hc.ccCancel
	hc.ccCancel = nil
	hb := hc.Heartbeat
	hc.Heartbeat = nil
	hc.mu.Unlock()

	if ccCancel != nil {
		ccCancel()
	}
	if cc != nil {
		slog.Info("Stopping control channel for connection", "name", hc.Name)
		_ = cc.Close()
	}
	// Wait for the Connect goroutine launched in Start to observe the close
	// and exit. Safe to call even when no goroutine is outstanding.
	hc.ccWg.Wait()
	hc.stopConduit()

	if hb != nil {
		slog.Info("Stopping heartbeat for connection", "name", hc.Name)
		hb.Stop()
	}
	hc.setStatus(ConnectionStatusDisconnected)
}

// Reinitialize updates credentials and restarts services for this connection.
//
// Every field written here is written under hc.mu, because request handlers
// and the credential watcher read them from other goroutines (via snapshot).
// hc.mu is a leaf lock: it is never held across Stop, Start or any other
// blocking call.
//
// Reinitialize does not update the requested-credential generation
// (reinitCreds/reinitGen). Production code must go through
// requestReinitialize and applyRequestedReinitialize, as the credential
// watcher does, so the latest requested set wins; today only tests call
// Reinitialize directly.
func (hc *HubConnection) Reinitialize(ctx context.Context, server *Server, creds *brokercredentials.BrokerCredentials) error {
	hc.lifecycleMu.Lock()
	defer hc.lifecycleMu.Unlock()
	return hc.reinitialize(ctx, server, creds)
}

// requestReinitialize records creds as the latest credential set for this
// connection. A following applyRequestedReinitialize applies it.
func (hc *HubConnection) requestReinitialize(creds *brokercredentials.BrokerCredentials) {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	hc.reinitCreds = creds
	hc.reinitGen++
	// A newly requested set is applied at once, and its failures back
	// off from the start.
	hc.resetReinitBackoffLocked()
}

// resetReinitBackoffLocked clears the retry backoff. The caller holds hc.mu.
func (hc *HubConnection) resetReinitBackoffLocked() {
	hc.reinitFailures = 0
	hc.reinitNextRetry = time.Time{}
	hc.reinitLastErr = ""
}

// clock returns the current time from hc.now, or time.Now when it is nil.
func (hc *HubConnection) clock() time.Time {
	if hc.now != nil {
		return hc.now()
	}
	return time.Now()
}

// latestCredentials returns the most recently requested credential set, or
// the current Credentials when none has been requested.
func (hc *HubConnection) latestCredentials() *brokercredentials.BrokerCredentials {
	hc.mu.RLock()
	defer hc.mu.RUnlock()
	if hc.reinitCreds != nil {
		return hc.reinitCreds
	}
	return hc.Credentials
}

// applyRequestedReinitialize reinitializes the connection with the latest
// credential set recorded by requestReinitialize. Calls queue on
// lifecycleMu in no particular order, so each one applies whatever is
// latest when it gets the lock, and does nothing (applied=false) when an
// earlier call already took up that request. The latest request is
// therefore always applied, whatever order the calls run in.
func (hc *HubConnection) applyRequestedReinitialize(ctx context.Context, server *Server) (applied bool, err error) {
	hc.lifecycleMu.Lock()
	defer hc.lifecycleMu.Unlock()

	hc.mu.Lock()
	if hc.closed {
		hc.mu.Unlock()
		return false, nil
	}
	creds := hc.reinitCreds
	gen := hc.reinitGen
	pending := creds != nil && gen != hc.reinitAppliedGen
	hc.reinitAppliedGen = gen
	if pending {
		// Taken up: an earlier failure no longer applies, so a watcher
		// tick while this reinitialize runs does not queue a retry.
		hc.reinitFailed = false
	}
	hc.mu.Unlock()
	if !pending {
		return false, nil
	}
	err = hc.reinitialize(ctx, server, creds)
	hc.mu.Lock()
	hc.reinitFailed = err != nil
	// A set requested while this reinitialize ran reset the backoff and
	// is applied next, so this result does not count towards it; a
	// failure is still logged at Error.
	report := reinitFailureReport{level: slog.LevelError}
	if hc.reinitGen == gen {
		report = hc.recordReinitResultLocked(err, hc.clock())
	}
	hc.mu.Unlock()
	report.log(hc.Name, err)
	return true, err
}

// reinitFailureReport says how to log a failed reinitialize; see
// recordReinitResultLocked.
type reinitFailureReport struct {
	level    slog.Level
	failures int
	retryIn  time.Duration
	skip     bool // nothing to log
}

// recordReinitResultLocked updates the retry backoff with the result of
// a reinitialize of the latest requested set, at time now, and returns
// how to log it. The caller holds hc.mu.
//
// After the n-th consecutive failure the next retry is due
// reinitRetryDelay(n) later: the credential watcher interval, doubling
// per failure up to reinitRetryMaxDelay. The first failure of a set is
// logged at Error, as is a failure whose error differs from the one
// before. An identical repeat is logged at Debug.
func (hc *HubConnection) recordReinitResultLocked(err error, now time.Time) reinitFailureReport {
	if err == nil {
		hc.resetReinitBackoffLocked()
		return reinitFailureReport{skip: true}
	}
	hc.reinitFailures++
	delay := reinitRetryDelay(hc.reinitFailures)
	// The watcher only looks at reinitNextRetry once per tick, and ticks
	// are not aligned with failures. Making a retry due half a tick
	// early means it is taken at the nearer tick, so retries are spaced
	// delay apart rather than one tick more.
	hc.reinitNextRetry = now.Add(delay - credentialWatchInterval/2)

	report := reinitFailureReport{level: slog.LevelDebug, failures: hc.reinitFailures, retryIn: delay}
	msg := err.Error()
	if hc.reinitFailures == 1 || msg != hc.reinitLastErr {
		report.level = slog.LevelError
	}
	hc.reinitLastErr = msg
	return report
}

// log writes the report for the reinitialize of connection name that
// returned err.
func (r reinitFailureReport) log(name string, err error) {
	if r.skip || err == nil {
		return
	}
	msg := "Failed to reinitialize hub connection; will retry"
	if r.level == slog.LevelDebug {
		msg = "Reinitialize of hub connection failed again; will retry"
	}
	attrs := []any{"name", name, "error", err}
	if r.failures > 0 {
		attrs = append(attrs, "consecutiveFailures", r.failures, "retryIn", r.retryIn)
	}
	slog.Log(context.Background(), r.level, msg, attrs...)
}

// reinitRetryDelay returns the delay before retrying after the n-th
// consecutive failed reinitialize (n >= 1): the credential watcher
// interval, doubled per further failure, at most reinitRetryMaxDelay.
func reinitRetryDelay(n int) time.Duration {
	d := credentialWatchInterval
	for i := 1; i < n && d < reinitRetryMaxDelay; i++ {
		d *= 2
	}
	if d > reinitRetryMaxDelay {
		d = reinitRetryMaxDelay
	}
	return d
}

// retryFailedReinitialize requests the latest credential set again when
// its reinitialize failed, no newer set has been requested since, and
// the retry backoff has passed (see recordReinitResultLocked), so that a
// following applyRequestedReinitialize retries it. It reports whether it
// requested a retry.
func (hc *HubConnection) retryFailedReinitialize() bool {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	if hc.closed || !hc.reinitFailed || hc.reinitCreds == nil || hc.reinitGen != hc.reinitAppliedGen {
		return false
	}
	if hc.clock().Before(hc.reinitNextRetry) {
		return false
	}
	hc.reinitGen++
	return true
}

// reinitialize is Reinitialize without lifecycleMu; the caller holds it.
func (hc *HubConnection) reinitialize(ctx context.Context, server *Server, creds *brokercredentials.BrokerCredentials) error {
	// Stop existing services. stop takes hc.mu itself.
	hc.stop()

	// Update credentials
	hc.mu.Lock()
	hc.Credentials = creds
	hc.BrokerID = creds.BrokerID
	hc.HubEndpoint = creds.HubEndpoint
	hc.AuthMode = creds.AuthMode
	hc.mu.Unlock()

	// Decode secret key
	secretKey, err := base64.StdEncoding.DecodeString(creds.SecretKey)
	if err != nil {
		hc.setStatus(ConnectionStatusError)
		return fmt.Errorf("failed to decode secret key: %w", err)
	}
	hc.mu.Lock()
	hc.SecretKey = secretKey
	hc.mu.Unlock()

	// Create new Hub client, resolving transport auth once for both REST and WebSocket
	opts := buildHubClientOpts(creds, secretKey)
	src, mode, err := transportauth.ResolveBrokerTransport(creds.TransportMode, creds.TransportAudience, adcsource.New)
	if err != nil {
		hc.setStatus(ConnectionStatusError)
		return fmt.Errorf("failed to resolve transport auth: %w", err)
	}
	if src != nil {
		opts = append(opts, hubclient.WithTransportAuth(src, mode))
	} else {
		mode = 0
	}
	hc.mu.Lock()
	hc.TransportSource = src
	hc.TransportMode = mode
	hc.mu.Unlock()

	client, err := hubclient.New(creds.HubEndpoint, opts...)
	if err != nil {
		hc.setStatus(ConnectionStatusError)
		return fmt.Errorf("failed to create Hub client: %w", err)
	}

	// Rebuild hydrator using shared cache
	var hydrator *templatecache.Hydrator
	if server.cache != nil {
		hydrator = templatecache.NewHydrator(server.cache, client)
	}
	var hcResolver *templatecache.Resolver
	if server.hcCache != nil {
		hcResolver = templatecache.NewHarnessConfigResolver(server.hcCache, client)
	}
	hc.mu.Lock()
	hc.HubClient = client
	if hydrator != nil {
		hc.Hydrator = hydrator
	}
	if hcResolver != nil {
		hc.HCResolver = hcResolver
	}
	hc.mu.Unlock()

	slog.Info("Hub connection reinitialized", "name", hc.Name, "brokerID", creds.BrokerID)

	// Restart services
	return hc.start(ctx, server)
}

// buildHubClientOpts creates hub client options from credentials.
func buildHubClientOpts(creds *brokercredentials.BrokerCredentials, secretKey []byte) []hubclient.Option {
	var opts []hubclient.Option

	switch creds.AuthMode {
	case brokercredentials.AuthModeDevAuth:
		opts = append(opts, hubclient.WithAutoDevAuth())
		slog.Info("Hub client using auto dev authentication", "name", creds.Name)
	case brokercredentials.AuthModeBearer:
		// Bearer mode could use a token from the credentials, but currently
		// there's no token field in BrokerCredentials for bearer auth.
		// Fall through to HMAC for now.
		fallthrough
	default:
		// Default to HMAC auth
		if len(secretKey) > 0 {
			opts = append(opts, hubclient.WithHMACAuth(creds.BrokerID, secretKey))
			slog.Info("Hub client using HMAC authentication", "name", creds.Name, "brokerID", creds.BrokerID)
		} else {
			opts = append(opts, hubclient.WithAutoDevAuth())
			slog.Info("Hub client using auto dev authentication (no secret key)", "name", creds.Name)
		}
	}

	return opts
}

// HubClientForCredentials builds the Hub client a connection with creds
// uses: HMAC authentication with the credentials' secret and their transport
// auth (IAP or Cloud Run invoker), exactly as createHubConnection builds it.
// A flat instance's host uses it for the remote activation validation, so
// the validation authenticates the same way the instance's connection will.
func HubClientForCredentials(creds *brokercredentials.BrokerCredentials) (hubclient.Client, error) {
	if creds == nil || creds.SecretKey == "" {
		return nil, fmt.Errorf("hub credentials have no secret")
	}
	secretKey, err := base64.StdEncoding.DecodeString(creds.SecretKey)
	if err != nil {
		return nil, fmt.Errorf("failed to decode secret key: %w", err)
	}
	opts := []hubclient.Option{hubclient.WithHMACAuth(creds.BrokerID, secretKey)}
	src, mode, err := transportauth.ResolveBrokerTransport(creds.TransportMode, creds.TransportAudience, adcsource.New)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve transport auth: %w", err)
	}
	if src != nil {
		opts = append(opts, hubclient.WithTransportAuth(src, mode))
	}
	return hubclient.New(creds.HubEndpoint, opts...)
}

// newHeartbeatService builds the heartbeat service for one hub connection.
// A flat instance's service is in flat mode (HeartbeatService.flat).
func (s *Server) newHeartbeatService(client hubclient.RuntimeBrokerService, brokerID, hubEndpoint string, interval time.Duration) *HeartbeatService {
	mgr, rt := s.defaultPair()
	hb := NewHeartbeatService(
		client,
		brokerID,
		interval,
		mgr,
		s.buildProjectFilterForHub(hubEndpoint),
		logging.Subsystem("broker.heartbeat"),
	)
	hb.auxiliaryManagers = s.getAuxiliaryManagers
	hb.workspaceStorage = s.workspaceStorageDescriptor
	hb.health = s.heartbeatHealthReport
	hb.profileAttach = s.heartbeatProfileAttach
	hb.profileSAMappings = s.heartbeatProfileSAMappings
	hb.startsInFlight = s.startsInFlightSnapshot
	hb.defaultProfile = s.defaultProfile
	hb.flat = s.isFlat()
	if fi := s.flatInstance(); fi != nil {
		// The instance publishes its FC4-a presentation, built only by
		// config.PresentInstance, on every heartbeat
		// (.design/flat-runtime-brokers/contract-appendix-p3.md FC4-a). The
		// epoch is the server's start time, so a restarted process fences
		// out the one it replaced; the sequence is the server's, shared by
		// every heartbeat service it builds.
		p := config.PresentInstance(fi.Instance).API()
		hb.presentation = &p
		hb.presentationTargetID = fi.Identity.RuntimeTarget.ID
		s.mu.RLock()
		hb.presentationEpoch = s.startTime.UnixNano()
		s.mu.RUnlock()
		hb.nextPresentationSeq = func() int64 { return s.presentationSeq.Add(1) }
	}
	hb.SetVersion(s.version)
	hb.SetDefaultRuntime(rt)
	return hb
}
