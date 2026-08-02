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

package autoexpose

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"

	scionhub "github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
)

// mockHubClient is a test double for HubPortClient.
type mockHubClient struct {
	mu         sync.Mutex
	ports      map[int]*scionhub.ExposedPort
	registerCb func(req scionhub.RegisterPortRequest) error // optional callback
}

func newMockHubClient() *mockHubClient {
	return &mockHubClient{
		ports: make(map[int]*scionhub.ExposedPort),
	}
}

func (m *mockHubClient) RegisterPort(_ context.Context, req scionhub.RegisterPortRequest) (*scionhub.ExposedPort, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.registerCb != nil {
		if err := m.registerCb(req); err != nil {
			return nil, err
		}
	}

	if _, exists := m.ports[req.Port]; exists {
		return nil, fmt.Errorf("hub returned error 409: port already registered")
	}

	ep := &scionhub.ExposedPort{
		Port:      req.Port,
		Label:     req.Label,
		Host:      req.Host,
		ExposedBy: req.Label, // use label as exposedBy for testing
	}
	m.ports[req.Port] = ep
	return ep, nil
}

func (m *mockHubClient) DeletePort(_ context.Context, port int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.ports, port)
	return nil
}

func (m *mockHubClient) ListPorts(_ context.Context) ([]scionhub.ExposedPort, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []scionhub.ExposedPort
	for _, ep := range m.ports {
		result = append(result, *ep)
	}
	return result, nil
}

func (m *mockHubClient) registeredPorts() []int {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ports []int
	for p := range m.ports {
		ports = append(ports, p)
	}
	sort.Ints(ports)
	return ports
}

func newTestReconciler(client *mockHubClient, cfg Config, sockets []ListenSocket) *Reconciler {
	r := NewReconciler(client, cfg)
	r.scan = func() ([]ListenSocket, error) {
		return sockets, nil
	}
	return r
}

func defaultTestConfig() Config {
	return Config{
		Enabled:     true,
		Interval:    DefaultInterval,
		FilterMode:  FilterModeDenylist,
		FilterPorts: nil,
		DeniedPorts: DefaultDeniedPorts,
		MaxPorts:    DefaultMaxPorts,
		MinPort:     DefaultMinPort,
	}
}

func TestReconciler_NewPortsExposed(t *testing.T) {
	client := newMockHubClient()
	sockets := []ListenSocket{
		{Port: 3000, BindAddr: "0.0.0.0"},
		{Port: 5000, BindAddr: "127.0.0.1"},
	}

	r := newTestReconciler(client, defaultTestConfig(), sockets)
	r.reconcileOnce(context.Background())

	ports := client.registeredPorts()
	if len(ports) != 2 {
		t.Fatalf("expected 2 ports registered, got %d: %v", len(ports), ports)
	}
	if ports[0] != 3000 || ports[1] != 5000 {
		t.Errorf("registered ports = %v, want [3000, 5000]", ports)
	}

	// Check auto-exposed tracking.
	if !r.autoExposed[3000] || !r.autoExposed[5000] {
		t.Errorf("autoExposed = %v, want {3000: true, 5000: true}", r.autoExposed)
	}
}

func TestReconciler_StalePortsUnexposed(t *testing.T) {
	client := newMockHubClient()

	// First tick: port 3000 and 5000 are listening.
	sockets := []ListenSocket{
		{Port: 3000, BindAddr: "0.0.0.0"},
		{Port: 5000, BindAddr: "0.0.0.0"},
	}
	r := newTestReconciler(client, defaultTestConfig(), sockets)
	r.reconcileOnce(context.Background())

	if len(client.registeredPorts()) != 2 {
		t.Fatalf("expected 2 ports after first tick, got %v", client.registeredPorts())
	}

	// Second tick: port 5000 stopped listening.
	r.scan = func() ([]ListenSocket, error) {
		return []ListenSocket{{Port: 3000, BindAddr: "0.0.0.0"}}, nil
	}
	r.reconcileOnce(context.Background())

	ports := client.registeredPorts()
	if len(ports) != 1 || ports[0] != 3000 {
		t.Errorf("expected [3000] after unexpose, got %v", ports)
	}
	if r.autoExposed[5000] {
		t.Error("port 5000 should be removed from autoExposed tracking")
	}
}

func TestReconciler_AllowlistFiltering(t *testing.T) {
	client := newMockHubClient()
	cfg := defaultTestConfig()
	cfg.FilterMode = FilterModeAllowlist
	cfg.FilterPorts = []int{3000}

	sockets := []ListenSocket{
		{Port: 3000, BindAddr: "0.0.0.0"},
		{Port: 5000, BindAddr: "0.0.0.0"},
		{Port: 8000, BindAddr: "0.0.0.0"},
	}

	r := newTestReconciler(client, cfg, sockets)
	r.reconcileOnce(context.Background())

	ports := client.registeredPorts()
	if len(ports) != 1 || ports[0] != 3000 {
		t.Errorf("expected [3000] with allowlist, got %v", ports)
	}
}

func TestReconciler_AllowlistEmpty(t *testing.T) {
	client := newMockHubClient()
	cfg := defaultTestConfig()
	cfg.FilterMode = FilterModeAllowlist
	cfg.FilterPorts = nil // empty allowlist

	sockets := []ListenSocket{
		{Port: 3000, BindAddr: "0.0.0.0"},
		{Port: 5000, BindAddr: "0.0.0.0"},
	}

	r := newTestReconciler(client, cfg, sockets)
	r.reconcileOnce(context.Background())

	ports := client.registeredPorts()
	if len(ports) != 0 {
		t.Errorf("expected no ports with empty allowlist, got %v", ports)
	}
}

func TestReconciler_DenylistFiltering(t *testing.T) {
	client := newMockHubClient()
	cfg := defaultTestConfig()
	cfg.FilterMode = FilterModeDenylist
	cfg.FilterPorts = []int{5000}

	sockets := []ListenSocket{
		{Port: 3000, BindAddr: "0.0.0.0"},
		{Port: 5000, BindAddr: "0.0.0.0"},
		{Port: 8000, BindAddr: "0.0.0.0"},
	}

	r := newTestReconciler(client, cfg, sockets)
	r.reconcileOnce(context.Background())

	ports := client.registeredPorts()
	if len(ports) != 2 {
		t.Fatalf("expected 2 ports with denylist, got %v", ports)
	}
	// 5000 should be excluded.
	for _, p := range ports {
		if p == 5000 {
			t.Error("port 5000 should be denied by denylist")
		}
	}
}

func TestReconciler_MinPortFiltering(t *testing.T) {
	client := newMockHubClient()
	cfg := defaultTestConfig()
	cfg.MinPort = 2000

	sockets := []ListenSocket{
		{Port: 80, BindAddr: "0.0.0.0"},
		{Port: 443, BindAddr: "0.0.0.0"},
		{Port: 1024, BindAddr: "0.0.0.0"},
		{Port: 3000, BindAddr: "0.0.0.0"},
	}

	r := newTestReconciler(client, cfg, sockets)
	r.reconcileOnce(context.Background())

	ports := client.registeredPorts()
	if len(ports) != 1 || ports[0] != 3000 {
		t.Errorf("expected [3000] with minPort=2000, got %v", ports)
	}
}

func TestReconciler_DeniedPortsFiltering(t *testing.T) {
	client := newMockHubClient()
	cfg := defaultTestConfig()

	sockets := []ListenSocket{
		{Port: 3000, BindAddr: "0.0.0.0"},
		{Port: 8080, BindAddr: "0.0.0.0"},  // NOT denied — reverse tunnel makes it safe
		{Port: 9810, BindAddr: "0.0.0.0"},  // denied (hub API)
		{Port: 18380, BindAddr: "0.0.0.0"}, // denied (metadata server)
	}

	r := newTestReconciler(client, cfg, sockets)
	r.reconcileOnce(context.Background())

	ports := client.registeredPorts()
	// 8080 is no longer denied, so both 3000 and 8080 should be registered.
	if len(ports) != 2 || ports[0] != 3000 || ports[1] != 8080 {
		t.Errorf("expected [3000, 8080] (only infra ports denied), got %v", ports)
	}
}

func TestReconciler_NeverUnexposeManualPorts(t *testing.T) {
	client := newMockHubClient()

	// Pre-register a port manually (not via auto-expose).
	client.ports[5000] = &scionhub.ExposedPort{
		Port:      5000,
		ExposedBy: "agent",
	}

	sockets := []ListenSocket{
		{Port: 3000, BindAddr: "0.0.0.0"},
		{Port: 5000, BindAddr: "0.0.0.0"},
	}

	r := newTestReconciler(client, defaultTestConfig(), sockets)
	r.reconcileOnce(context.Background())

	// Now stop listening on port 5000.
	r.scan = func() ([]ListenSocket, error) {
		return []ListenSocket{{Port: 3000, BindAddr: "0.0.0.0"}}, nil
	}
	r.reconcileOnce(context.Background())

	// Port 5000 should still be registered (manual port not touched).
	ports := client.registeredPorts()
	found5000 := false
	for _, p := range ports {
		if p == 5000 {
			found5000 = true
		}
	}
	if !found5000 {
		t.Errorf("manual port 5000 should not be unexposed, got %v", ports)
	}

	// Port 3000 should also still be there.
	found3000 := false
	for _, p := range ports {
		if p == 3000 {
			found3000 = true
		}
	}
	if !found3000 {
		t.Errorf("auto port 3000 should be registered, got %v", ports)
	}
}

func TestReconciler_MaxPortsLimit(t *testing.T) {
	client := newMockHubClient()
	cfg := defaultTestConfig()
	cfg.MaxPorts = 3

	sockets := []ListenSocket{
		{Port: 3000, BindAddr: "0.0.0.0"},
		{Port: 3001, BindAddr: "0.0.0.0"},
		{Port: 3002, BindAddr: "0.0.0.0"},
		{Port: 3003, BindAddr: "0.0.0.0"},
		{Port: 3004, BindAddr: "0.0.0.0"},
	}

	r := newTestReconciler(client, cfg, sockets)
	r.reconcileOnce(context.Background())

	ports := client.registeredPorts()
	if len(ports) > 3 {
		t.Errorf("expected at most 3 ports (maxPorts=3), got %d: %v", len(ports), ports)
	}
}

func TestReconciler_ConflictHandledGracefully(t *testing.T) {
	client := newMockHubClient()

	// Simulate a race condition: RegisterPort returns 409 for port 3000
	// (registered by another process between scan and register), but
	// ListPorts doesn't show it yet (cache staleness).
	client.registerCb = func(req scionhub.RegisterPortRequest) error {
		if req.Port == 3000 {
			return fmt.Errorf("hub returned error 409: port already registered")
		}
		return nil
	}

	sockets := []ListenSocket{
		{Port: 3000, BindAddr: "0.0.0.0"},
		{Port: 5000, BindAddr: "0.0.0.0"},
	}

	r := newTestReconciler(client, defaultTestConfig(), sockets)
	r.reconcileOnce(context.Background())

	// Port 3000 should NOT be tracked as auto-exposed after 409 —
	// it may have been manually registered.
	if r.autoExposed[3000] {
		t.Error("port 3000 should NOT be tracked as auto-exposed after 409 conflict")
	}

	// Port 5000 should be registered normally.
	if !r.autoExposed[5000] {
		t.Error("port 5000 should be tracked as auto-exposed")
	}
}

func TestReconciler_NoOpAtSteadyState(t *testing.T) {
	client := newMockHubClient()

	sockets := []ListenSocket{
		{Port: 3000, BindAddr: "0.0.0.0"},
	}

	r := newTestReconciler(client, defaultTestConfig(), sockets)

	// First tick: registers port.
	r.reconcileOnce(context.Background())
	if len(client.registeredPorts()) != 1 {
		t.Fatal("expected 1 port after first tick")
	}

	// Second tick: no changes — should be a no-op.
	registerCalled := false
	origRegister := client.registerCb
	client.registerCb = func(_ scionhub.RegisterPortRequest) error {
		registerCalled = true
		return nil
	}
	r.reconcileOnce(context.Background())
	client.registerCb = origRegister

	if registerCalled {
		t.Error("expected no RegisterPort call at steady state")
	}
}

func TestReconciler_FilterPorts_CombinedDeniedAndDenylist(t *testing.T) {
	client := newMockHubClient()
	cfg := defaultTestConfig()
	cfg.FilterMode = FilterModeDenylist
	cfg.FilterPorts = []int{4000}

	sockets := []ListenSocket{
		{Port: 3000, BindAddr: "0.0.0.0"},
		{Port: 4000, BindAddr: "0.0.0.0"}, // in denylist
		{Port: 8080, BindAddr: "0.0.0.0"}, // NOT denied — reverse tunnel makes it safe
		{Port: 9810, BindAddr: "0.0.0.0"}, // in denied ports (hub API)
	}

	r := newTestReconciler(client, cfg, sockets)
	r.reconcileOnce(context.Background())

	ports := client.registeredPorts()
	// 8080 is no longer in denied ports, so 3000 and 8080 should be registered.
	if len(ports) != 2 || ports[0] != 3000 || ports[1] != 8080 {
		t.Errorf("expected [3000, 8080] with combined deny, got %v", ports)
	}
}

func TestReconciler_LabelSetCorrectly(t *testing.T) {
	client := newMockHubClient()

	sockets := []ListenSocket{
		{Port: 3000, BindAddr: "0.0.0.0"},
	}

	r := newTestReconciler(client, defaultTestConfig(), sockets)
	r.reconcileOnce(context.Background())

	client.mu.Lock()
	defer client.mu.Unlock()
	if ep, ok := client.ports[3000]; ok {
		if ep.Label != autoExposeLabel {
			t.Errorf("port label = %q, want %q", ep.Label, autoExposeLabel)
		}
	} else {
		t.Error("port 3000 not found in mock client")
	}
}

func TestReconciler_ConflictDoesNotAutoUnexposeManualPort(t *testing.T) {
	client := newMockHubClient()

	// Simulate: user manually registered port 3000 between cache refresh and
	// the register call. RegisterPort returns 409 for port 3000.
	client.ports[3000] = &scionhub.ExposedPort{
		Port:      3000,
		ExposedBy: "agent", // manually registered
	}

	sockets := []ListenSocket{
		{Port: 3000, BindAddr: "0.0.0.0"},
	}

	r := newTestReconciler(client, defaultTestConfig(), sockets)
	// First tick: 3000 not in cache yet, tries to register, gets 409.
	r.reconcileOnce(context.Background())

	// Port 3000 must NOT be in autoExposed.
	if r.autoExposed[3000] {
		t.Fatal("port 3000 should NOT be tracked as auto-exposed after 409 conflict")
	}

	// Now the process on port 3000 stops listening.
	r.scan = func() ([]ListenSocket, error) {
		return nil, nil
	}
	r.reconcileOnce(context.Background())

	// The manually-registered port 3000 must still be in the hub.
	ports := client.registeredPorts()
	found := false
	for _, p := range ports {
		if p == 3000 {
			found = true
		}
	}
	if !found {
		t.Errorf("manual port 3000 should NOT have been auto-unexposed, got %v", ports)
	}
}

func TestIsConflictError(t *testing.T) {
	tests := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{fmt.Errorf("hub returned error 409: port already registered"), true},
		{fmt.Errorf("hub returned error 500: internal server error"), false},
		{fmt.Errorf("network error"), false},
	}

	for _, tt := range tests {
		got := isConflictError(tt.err)
		if got != tt.want {
			t.Errorf("isConflictError(%v) = %v, want %v", tt.err, got, tt.want)
		}
	}
}
