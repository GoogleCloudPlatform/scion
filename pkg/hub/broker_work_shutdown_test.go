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
	"database/sql"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// errShutdownStoreClosed is what shutdownOrderStore returns for a call made
// after markClosed, standing in for the real store's closed-database error.
var errShutdownStoreClosed = errors.New("sql: database is closed")

// shutdownGateKey names one store method call for one broker.
type shutdownGateKey struct {
	method   string
	brokerID string
}

// shutdownGate holds one store call open: entered is closed when the call
// arrives, and the call proceeds once release is closed.
type shutdownGate struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

// shutdownOrderStore wraps a real store for the hub shutdown test. It holds
// selected broker-work store calls open on gates, and after markClosed
// (the point where cmd closes the store) it records every broker-work call
// as late and fails it, as a closed store would. The gates are fixed at
// construction, before the Server exists, so no goroutine reads the map
// while it is written.
type shutdownOrderStore struct {
	store.Store
	gates map[shutdownGateKey]*shutdownGate

	mu     sync.Mutex
	closed bool
	late   []string
}

func newShutdownOrderStore(inner store.Store, gated ...shutdownGateKey) *shutdownOrderStore {
	gates := make(map[shutdownGateKey]*shutdownGate, len(gated))
	for _, k := range gated {
		gates[k] = &shutdownGate{entered: make(chan struct{}), release: make(chan struct{})}
	}
	return &shutdownOrderStore{Store: inner, gates: gates}
}

// DB forwards to the wrapped store's raw *sql.DB, so New()'s membership
// index migration runs against the real store.
func (s *shutdownOrderStore) DB() *sql.DB {
	if p, ok := s.Store.(interface{ DB() *sql.DB }); ok {
		return p.DB()
	}
	return nil
}

func (s *shutdownOrderStore) gate(method, brokerID string) *shutdownGate {
	return s.gates[shutdownGateKey{method: method, brokerID: brokerID}]
}

// enter records the call, fails it when the store is closed, and waits on
// its gate if it has one.
func (s *shutdownOrderStore) enter(method, brokerID string) error {
	s.mu.Lock()
	closed := s.closed
	if closed {
		s.late = append(s.late, method+" "+brokerID)
	}
	s.mu.Unlock()
	if closed {
		return errShutdownStoreClosed
	}
	if g := s.gate(method, brokerID); g != nil {
		g.once.Do(func() { close(g.entered) })
		<-g.release
	}
	return nil
}

func (s *shutdownOrderStore) markClosed() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
}

func (s *shutdownOrderStore) releaseAll() {
	for _, g := range s.gates {
		close(g.release)
	}
}

func (s *shutdownOrderStore) lateCalls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.late...)
}

func (s *shutdownOrderStore) ClaimRuntimeBrokerConnection(ctx context.Context, brokerID, hubInstanceID, sessionID string) error {
	if err := s.enter("ClaimRuntimeBrokerConnection", brokerID); err != nil {
		return err
	}
	return s.Store.ClaimRuntimeBrokerConnection(ctx, brokerID, hubInstanceID, sessionID)
}

func (s *shutdownOrderStore) ReleaseAndMarkBrokerOffline(ctx context.Context, brokerID, hubInstanceID, sessionID string) (bool, error) {
	if err := s.enter("ReleaseAndMarkBrokerOffline", brokerID); err != nil {
		return false, err
	}
	return s.Store.ReleaseAndMarkBrokerOffline(ctx, brokerID, hubInstanceID, sessionID)
}

func (s *shutdownOrderStore) GetBrokerProjects(ctx context.Context, brokerID string) ([]store.ProjectProvider, error) {
	if err := s.enter("GetBrokerProjects", brokerID); err != nil {
		return nil, err
	}
	return s.Store.GetBrokerProjects(ctx, brokerID)
}

func (s *shutdownOrderStore) UpdateProviderStatus(ctx context.Context, projectID, brokerID, status string) error {
	if err := s.enter("UpdateProviderStatus", brokerID); err != nil {
		return err
	}
	return s.Store.UpdateProviderStatus(ctx, projectID, brokerID, status)
}

func (s *shutdownOrderStore) ListPendingDispatch(ctx context.Context, brokerID string) ([]store.BrokerDispatch, error) {
	if err := s.enter("ListPendingDispatch", brokerID); err != nil {
		return nil, err
	}
	return s.Store.ListPendingDispatch(ctx, brokerID)
}

func (s *shutdownOrderStore) GetUndispatchedAgentNotifications(ctx context.Context, brokerID string) ([]store.Notification, error) {
	if err := s.enter("GetUndispatchedAgentNotifications", brokerID); err != nil {
		return nil, err
	}
	return s.Store.GetUndispatchedAgentNotifications(ctx, brokerID)
}

// TestHubShutdown_JoinsBrokerWorkBeforeStoreClose covers ptone/scion#4869.
// It shuts a hub down while four kinds of broker work are in flight, each
// held inside a store call:
//
//   - the connect path (markBrokerOnline), in GetBrokerProjects
//   - the dispatch reconcile markBrokerOnline starts, in ListPendingDispatch
//   - the disconnect callback (handleBrokerDisconnect), in
//     ReleaseAndMarkBrokerOffline
//   - the heartbeat-triggered stop drain of an HTTP-only broker
//     (startHTTPStopDrain), in ListPendingDispatch
//
// Background teardown must not return until all of them have exited, so
// that nothing runs when the store closes right after it (as cmd does),
// and the shutdown must log no ERROR record. A broker that reconnects
// after shutdown has started must do nothing.
func TestHubShutdown_JoinsBrokerWorkBeforeStoreClose(t *testing.T) {
	// The capture goes in before New(): the hub's subsystem loggers are
	// derived from slog.Default when the Server is built.
	capture := captureDefaultCapturingHandler(t)

	base, err := newTestStore(t, ":memory:")
	require.NoError(t, err)

	connectID := tid("broker-selfheal-shutdown-connect")
	reconcileID := tid("broker-selfheal-shutdown-reconcile")
	disconnectID := tid("broker-selfheal-shutdown-disconnect")
	httpDrainID := tid("broker-selfheal-shutdown-httpdrain")
	wrapped := newShutdownOrderStore(base,
		shutdownGateKey{method: "GetBrokerProjects", brokerID: connectID},
		shutdownGateKey{method: "ListPendingDispatch", brokerID: reconcileID},
		shutdownGateKey{method: "ReleaseAndMarkBrokerOffline", brokerID: disconnectID},
		shutdownGateKey{method: "ListPendingDispatch", brokerID: httpDrainID},
	)
	srv, _ := testServerOnMigratedStore(t, wrapped)

	for _, suffix := range []string{"shutdown-connect", "shutdown-reconcile", "shutdown-disconnect", "shutdown-httpdrain"} {
		broker, _ := newProviderSelfHealFixture(t, base, suffix)
		require.Contains(t, []string{connectID, reconcileID, disconnectID, httpDrainID}, broker.ID)
	}

	start := len(capture.all())

	// Connect work in flight.
	connectDone := make(chan struct{})
	go func() {
		defer close(connectDone)
		srv.markBrokerOnline(connectID, "sess-shutdown-connect")
	}()
	<-wrapped.gate("GetBrokerProjects", connectID).entered

	// Reconcile in flight: the connect itself completes, the reconcile it
	// starts is held.
	srv.markBrokerOnline(reconcileID, "sess-shutdown-reconcile")
	<-wrapped.gate("ListPendingDispatch", reconcileID).entered

	// Disconnect callback in flight, fired the way a dropped socket fires
	// it.
	srv.controlChannel.mu.Lock()
	srv.controlChannel.connections[disconnectID] = &BrokerConnection{brokerID: disconnectID, sessionID: "sess-shutdown-disconnect"}
	srv.controlChannel.mu.Unlock()
	srv.controlChannel.removeConnection(disconnectID, "sess-shutdown-disconnect")
	<-wrapped.gate("ReleaseAndMarkBrokerOffline", disconnectID).entered

	// Heartbeat-triggered stop drain of an HTTP-only broker in flight.
	srv.startHTTPStopDrain(httpDrainID)
	<-wrapped.gate("ListPendingDispatch", httpDrainID).entered

	runningAtClose := -1
	cleanupDone := make(chan struct{})
	go func() {
		defer close(cleanupDone)
		_ = srv.CleanupBackgroundResources(context.Background())
		// What cmd does next: close the store.
		runningAtClose = srv.brokerWork.runningCount()
		wrapped.markClosed()
	}()

	// If teardown never closes admission, this fails by assertion when
	// teardown returns rather than hanging.
	waitStopping(t, &srv.brokerWork, cleanupDone, "background teardown")
	select {
	case <-cleanupDone:
		t.Fatal("background teardown returned while broker work was still running")
	default:
	}

	wrapped.releaseAll()
	<-cleanupDone
	<-connectDone

	assert.Equal(t, 0, runningAtClose, "every unit of broker work must exit before the store closes")
	assert.Empty(t, wrapped.lateCalls(), "no broker work may reach the store after it closes")

	// A broker that reconnects after shutdown has started does nothing.
	srv.markBrokerOnline(reconcileID, "sess-shutdown-late")
	assert.Empty(t, wrapped.lateCalls(), "a connect after shutdown must not touch the store")
	requireRecordLive(t, capture, "Hub is shutting down; not marking broker online")

	// A heartbeat after shutdown has started does not start a stop drain,
	// and leaves no in-flight marker behind.
	srv.startHTTPStopDrain(httpDrainID)
	assert.Empty(t, wrapped.lateCalls(), "a heartbeat drain after shutdown must not touch the store")
	_, marked := srv.httpDrains.Load(httpDrainID)
	assert.False(t, marked, "a refused heartbeat drain must clear its in-flight marker")

	for _, r := range capture.all()[start:] {
		assert.Less(t, r.Level, slog.LevelError, "shutdown logged an ERROR: %q", r.Message)
	}
}

// TestBrokerDispatchDrain_StopsClaimingOnceShutdownBegins covers the
// in-flight half of ptone/scion#4869. A drain that is already running when
// shutdown begins must claim no more rows: the broker's tunnel is about to
// close, so a claimed row would only be failed. The rows it has not
// claimed must stay pending, so they drain on the next connect.
func TestBrokerDispatchDrain_StopsClaimingOnceShutdownBegins(t *testing.T) {
	capture := captureDefaultCapturingHandler(t)
	ctx := context.Background()

	base, err := newTestStore(t, ":memory:")
	require.NoError(t, err)
	brokerID := tid("broker-selfheal-drain-shutdown")
	wrapped := newShutdownOrderStore(base, shutdownGateKey{method: "ListPendingDispatch", brokerID: brokerID})
	srv, _ := testServerOnMigratedStore(t, wrapped)

	broker, project := newProviderSelfHealFixture(t, base, "drain-shutdown")
	require.Equal(t, brokerID, broker.ID)
	ids := []string{tid("dispatch-drain-shutdown-1"), tid("dispatch-drain-shutdown-2")}
	for _, id := range ids {
		require.NoError(t, base.InsertBrokerDispatch(ctx, &store.BrokerDispatch{
			ID: id, BrokerID: brokerID, ProjectID: project.ID, Op: "stop",
		}))
	}

	// The drain waits on the gate inside its ListPendingDispatch call,
	// before it reads its pending rows; it reads them after release.
	require.True(t, srv.brokerWork.goTracked(func(workCtx context.Context) {
		srv.reconcileBroker(workCtx, brokerID)
	}))
	<-wrapped.gate("ListPendingDispatch", brokerID).entered

	// Shutdown begins while the drain holds its row list.
	srv.brokerWork.closeAdmission()
	wrapped.releaseAll()
	require.Equal(t, 0, srv.brokerWork.wait(ctx, time.Hour), "the drain must finish")

	for _, id := range ids {
		d, err := base.GetBrokerDispatch(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, store.DispatchStatePending, d.State, "dispatch %s must stay pending for the next connect", id)
		assert.Empty(t, d.ClaimedBy, "dispatch %s must not be claimed", id)
	}
	requireRecordLive(t, capture, "reconcile: hub is shutting down; leaving the remaining dispatches pending")
}
