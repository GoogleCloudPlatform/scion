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

	// lifecycleMu makes Start, Stop and Reinitialize on this connection run
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
			slog.Warn("Skipping heartbeat for connection: no valid credentials", "name", hc.Name)
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
	creds := hc.reinitCreds
	pending := creds != nil && hc.reinitGen != hc.reinitAppliedGen
	hc.reinitAppliedGen = hc.reinitGen
	hc.mu.Unlock()
	if !pending {
		return false, nil
	}
	return true, hc.reinitialize(ctx, server, creds)
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

// newHeartbeatService builds the heartbeat service for one hub connection.
// A flat instance's service is in flat mode (HeartbeatService.flat).
func (s *Server) newHeartbeatService(client hubclient.RuntimeBrokerService, brokerID, hubEndpoint string, interval time.Duration) *HeartbeatService {
	hb := NewHeartbeatService(
		client,
		brokerID,
		interval,
		s.manager,
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
	hb.SetVersion(s.version)
	hb.SetDefaultRuntime(s.runtime)
	return hb
}
