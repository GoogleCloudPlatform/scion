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

// Package relayserver runs a separate conduit relay process (scion server
// --role relay; design §3.9 "Separate relay"). It serves conduit sessions
// on its public listener and the relay internal API on its internal
// listener, and calls the hub internal API for everything that needs
// policy (relayrole). It takes no session secret: its only credential is
// its relay-peer authenticator. Its database access is what its caller
// injects: the conduit registry store, the hub.conduit experiment reader
// and, optionally, the hub's revocation events.
package relayserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relayrole"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// RevocationSubjects are the hub event subjects a separate relay re-checks
// its sessions and streams on: stream authorization changes, agent
// deletion and port changes, project deletion, and broker credential
// changes. A hub test keeps this list equal to the hub's own.
var RevocationSubjects = []string{"conduit.authz.changed", "agent.*.deleted", "agent.*.ports", "project.*.deleted", "conduit.broker.changed"}

// SettingsUpdatedSubject is the hub event published after a settings
// change. The relay re-reads hub.conduit on it, besides its periodic read.
const SettingsUpdatedSubject = "admin.settings.updated"

// Defaults.
const (
	DefaultExperimentPollPeriod = 15 * time.Second
	DefaultStartupDeadline      = 60 * time.Second
	DefaultStartupRetry         = 2 * time.Second
)

// ErrExperimentOff refuses to start while hub.conduit is off.
var ErrExperimentOff = errors.New("--role relay requires the hub.conduit experiment; turn it on in the hub's settings")

// ExperimentSource reads whether hub.conduit is on, resolved as the hub
// resolves it.
type ExperimentSource interface {
	ConduitEnabled(ctx context.Context) (bool, error)
}

// Config configures a separate relay. There is deliberately no session
// secret input.
type Config struct {
	// InstanceID is this relay's instance id.
	InstanceID string
	// PublicURL is the relay's public URL (its relay_instances
	// public_endpoint).
	PublicURL string
	// HubInternalURL is the base URL of the hub internal API.
	HubInternalURL string
	// PublicListen and InternalListen are the listen addresses
	// (host:port) of the public and internal listeners.
	PublicListen   string
	InternalListen string
	// Advertise returns the internal endpoint other nodes reach this relay
	// at, given the internal listener's bound address (nil:
	// http://<bound address>).
	Advertise func(bound net.Addr) string
	// PeerAuth signs this relay's calls to the hub and verifies relay-peer
	// calls to it. Required.
	PeerAuth relay.PeerAuth
	// HTTPClient makes the hub and relay-peer calls (nil: default).
	HTTPClient *http.Client
	// Registry is the conduit registry store. Required.
	Registry registry.Store
	// Experiment reads hub.conduit. Required.
	Experiment ExperimentSource
	// Events, when set, delivers the hub's events: RevocationSubjects
	// trigger re-checks, SettingsUpdatedSubject a re-read of hub.conduit.
	Events relayrole.Events
	// RecheckInterval, ReconnectWindow and LifetimeCap are the conduit
	// settings of the same names (0: the defaults).
	RecheckInterval time.Duration
	ReconnectWindow time.Duration
	LifetimeCap     time.Duration
	// RequireSelfCheck makes the relay probe its own internal endpoint at
	// start (HA deployments).
	RequireSelfCheck bool
	// ExperimentPollPeriod is how often hub.conduit is re-read
	// (DefaultExperimentPollPeriod).
	ExperimentPollPeriod time.Duration
	// StartupDeadline bounds how long an unreadable hub.conduit is retried
	// at startup, every StartupRetry (DefaultStartupDeadline,
	// DefaultStartupRetry).
	StartupDeadline time.Duration
	StartupRetry    time.Duration
	// Clock drives the relay's timers (nil: real time).
	Clock clock.Clock
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

func (c *Config) validate() error {
	switch {
	case c.InstanceID == "":
		return errors.New("conduit relay: no instance id")
	case c.PublicURL == "":
		return errors.New("server.relay.public_url is required on --role relay")
	case c.HubInternalURL == "":
		return errors.New("server.relay.hub_internal_url is required on --role relay")
	case c.InternalListen == "":
		return errors.New("server.hub.conduit.internal_listen is required on --role relay")
	case c.PeerAuth == nil:
		return errors.New("conduit relay: no relay-peer authenticator")
	case c.Registry == nil:
		return errors.New("conduit relay: no registry store")
	case c.Experiment == nil:
		return errors.New("conduit relay: no hub.conduit reader")
	}
	if c.ExperimentPollPeriod <= 0 {
		c.ExperimentPollPeriod = DefaultExperimentPollPeriod
	}
	if c.StartupDeadline <= 0 {
		c.StartupDeadline = DefaultStartupDeadline
	}
	if c.StartupRetry <= 0 {
		c.StartupRetry = DefaultStartupRetry
	}
	if c.Clock == nil {
		c.Clock = clock.Real()
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return nil
}

// Server is a running separate relay.
type Server struct {
	cfg  Config
	log  *slog.Logger
	errs chan error

	// Relay is the relay; Role its policy-free front.
	Relay *relay.Relay
	Role  *relayrole.Role
	// PublicAddr and InternalAddr are the bound listener addresses.
	PublicAddr   net.Addr
	InternalAddr net.Addr

	public   *http.Server
	internal *http.Server
}

// waitExperiment checks hub.conduit at startup: off refuses to start; an
// unreadable setting is retried until the deadline, then fails.
func waitExperiment(ctx context.Context, cfg *Config) error {
	ctx, cancel := context.WithTimeout(ctx, cfg.StartupDeadline)
	defer cancel()
	for {
		on, err := cfg.Experiment.ConduitEnabled(ctx)
		switch {
		case err == nil && on:
			return nil
		case err == nil:
			return ErrExperimentOff
		}
		cfg.Logger.Warn("Conduit relay: reading hub.conduit failed; retrying", "error", err)
		t := time.NewTimer(cfg.StartupRetry)
		select {
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("conduit relay: hub.conduit could not be read before the startup deadline: %w", err)
		case <-t.C:
		}
	}
}

// Start checks hub.conduit, then starts the relay, its front and both
// listeners. ctx bounds the relay's background work; Shutdown stops it.
func Start(ctx context.Context, cfg Config) (*Server, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if err := waitExperiment(ctx, &cfg); err != nil {
		return nil, err
	}
	log := cfg.Logger.With("subsystem", "relay")
	role, err := relayrole.New(relayrole.Config{
		Hub:             &relay.HubClient{BaseURL: cfg.HubInternalURL, HTTP: cfg.HTTPClient, Auth: cfg.PeerAuth},
		RecheckInterval: cfg.RecheckInterval,
		Events:          cfg.Events,
		Subjects:        RevocationSubjects,
		Logger:          log,
	})
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, log: log, errs: make(chan error, 2), Role: role}

	// The internal listener opens first (its address is the advertised
	// endpoint); it answers 503 until the relay exists.
	var current atomic.Pointer[relay.Relay]
	internalLn, err := net.Listen("tcp", cfg.InternalListen)
	if err != nil {
		return nil, fmt.Errorf("conduit internal listener %s: %w", cfg.InternalListen, err)
	}
	s.InternalAddr = internalLn.Addr()
	s.internal = &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if r := current.Load(); r != nil {
			r.InternalHandler().ServeHTTP(w, req)
			return
		}
		http.Error(w, "conduit relay not running", http.StatusServiceUnavailable)
	})}
	go s.serve(s.internal, internalLn, "relay internal listener")

	endpoint := "http://" + internalLn.Addr().String()
	if cfg.Advertise != nil {
		endpoint = cfg.Advertise(internalLn.Addr())
	}
	r, err := relay.New(relay.Config{
		InstanceID:       cfg.InstanceID,
		InternalEndpoint: endpoint,
		PublicEndpoint:   cfg.PublicURL,
		RequireSelfCheck: cfg.RequireSelfCheck,
		Registry:         registry.New(cfg.Registry, registry.Config{}),
		Store:            cfg.Registry,
		Session:          conduit.Config{Clock: clock.Real()},
		GrantKeys: func(context.Context) ([]*conduitv1.GrantKey, error) {
			return nil, errors.New("grant keys come with each admission")
		},
		Refresh:         role.Refresh,
		PeerAuth:        cfg.PeerAuth,
		HTTPClient:      cfg.HTTPClient,
		ReconnectWindow: cfg.ReconnectWindow,
		LifetimeCap:     cfg.LifetimeCap,
		Clock:           cfg.Clock,
		Logger:          log,
	})
	if err != nil {
		_ = s.internal.Close()
		return nil, fmt.Errorf("conduit relay: %w", err)
	}
	// Published before Start: the self-check probes this relay's own
	// internal handler.
	current.Store(r)
	if err := r.Start(ctx); err != nil {
		_ = s.internal.Close()
		return nil, err
	}
	s.Relay = r
	role.Bind(r)
	role.Start(ctx)

	publicLn, err := net.Listen("tcp", cfg.PublicListen)
	if err != nil {
		r.Kill()
		_ = s.internal.Close()
		return nil, fmt.Errorf("relay public listener %s: %w", cfg.PublicListen, err)
	}
	s.PublicAddr = publicLn.Addr()
	mux := http.NewServeMux()
	mux.Handle(relayrole.ConduitPath, role)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	s.public = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go s.serve(s.public, publicLn, "relay public listener")

	go s.followExperiment(ctx)
	log.Info("Conduit relay role started", "instance_id", cfg.InstanceID, "internal_endpoint", endpoint,
		"public_url", cfg.PublicURL, "hub_internal_url", cfg.HubInternalURL, "public_addr", s.PublicAddr.String())
	return s, nil
}

func (s *Server) serve(srv *http.Server, ln net.Listener, name string) {
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		select {
		case s.errs <- fmt.Errorf("%s: %w", name, err):
		default:
		}
	}
}

// ApplyExperiment re-reads hub.conduit: off stops admission and drains the
// open sessions (the process keeps running), on resumes admission, and a
// failed read changes nothing.
func (s *Server) ApplyExperiment(ctx context.Context) {
	on, err := s.cfg.Experiment.ConduitEnabled(ctx)
	if err != nil {
		s.log.Warn("Conduit relay: re-reading hub.conduit failed; admission unchanged", "error", err)
		return
	}
	s.Relay.SetAdmitting(on)
}

// followExperiment re-reads hub.conduit every ExperimentPollPeriod and on
// every settings change event.
func (s *Server) followExperiment(ctx context.Context) {
	var changed <-chan string
	if s.cfg.Events != nil {
		ch, unsubscribe := s.cfg.Events.Subscribe(SettingsUpdatedSubject)
		defer unsubscribe()
		changed = ch
	}
	t := time.NewTicker(s.cfg.ExperimentPollPeriod)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case _, ok := <-changed:
			if !ok {
				changed = nil
				continue
			}
		}
		s.ApplyExperiment(ctx)
	}
}

// Wait blocks until ctx ends (nil), a listener fails, or the relay stops
// serving.
func (s *Server) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return nil
	case err := <-s.errs:
		return err
	case err, ok := <-s.Relay.Fatal():
		if !ok || err == nil {
			err = errors.New("relay stopped serving")
		}
		return err
	}
}

// Shutdown drains the relay (capped at drainCap when capDrain) and closes
// both listeners, within ctx.
func (s *Server) Shutdown(ctx context.Context, capDrain bool, drainCap time.Duration) error {
	err := s.Relay.ShutdownWith(ctx, relay.ShutdownOptions{CapDrain: capDrain, DrainCap: drainCap})
	_ = s.public.Shutdown(ctx)
	_ = s.internal.Shutdown(ctx)
	return err
}
