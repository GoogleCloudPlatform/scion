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
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/plugin"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
)

// noopChannel is a minimal NotificationChannel that does nothing.
// It exists to populate a ChannelRegistry for race testing.
type noopChannel struct{}

func (noopChannel) Name() string                                                   { return "noop" }
func (noopChannel) Deliver(_ context.Context, _ *messages.StructuredMessage) error { return nil }
func (noopChannel) Validate() error                                                { return nil }

// TestChannelRegistryRace verifies that concurrent SetChannelRegistry(nil)
// calls do not race with the handler read path in handleAgentOutboundMessage.
//
// The pre-fix code read s.channelRegistry three times without holding s.mu:
//
//	if s.channelRegistry != nil && s.channelRegistry.Len() > 0 {
//	    s.channelRegistry.Dispatch(ctx, structuredMsg)
//	}
//
// A concurrent SetChannelRegistry(nil) landing between the nil check and
// Dispatch is a nil-pointer dereference. With the fix, a single snapshot
// under RLock ensures all three accesses see the same value.
//
// This test has two detection modes:
//
//  1. Without -race (go test -count=1 ./pkg/hub/): goroutines recover from
//     nil-deref panics and fail the test. This is probabilistic — the timing
//     window is narrow and may not be hit on every run. A green result without
//     -race is NOT proof of correctness.
//
//  2. With -race (go test -race ./pkg/hub/): the race detector instruments
//     every memory access and will reliably detect the unsynchronized read
//     even if the nil-deref timing is never hit.
//
// CI does not currently pass -race, so this test does not gate merges.
func TestChannelRegistryRace(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   api.NewUUID(),
		Name: "cr-race",
		Slug: "cr-race",
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	agent := &store.Agent{
		ID:        api.NewUUID(),
		Name:      "cr-agent",
		Slug:      "cr-agent",
		ProjectID: project.ID,
		Phase:     "running",
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	if err := s.CreateUser(ctx, &store.User{
		ID:    api.NewUUID(),
		Email: "cr-human@example.com",
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	// Build a ChannelRegistry with one noop channel so Len() > 0.
	registry := &ChannelRegistry{
		channels: []NotificationChannel{noopChannel{}},
		configs:  []ChannelConfig{{Type: "noop"}},
		log:      slog.Default(),
	}

	// Disable the per-sender rate limiter so high concurrency does not 429.
	srv.chatSendLimiter = newChatSendLimiterWithRates(
		map[chatSenderClass]float64{
			chatSenderHuman:       1e9,
			chatSenderAgent:       1e9,
			chatSenderAgentMirror: 1e9,
		}, time.Now)

	const goroutines = 8
	const iterations = 200

	var panicked atomic.Int32
	var wg sync.WaitGroup

	// Writer goroutines: toggle channelRegistry between a live registry and nil.
	for i := 0; i < goroutines/2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				if j%2 == 0 {
					srv.SetChannelRegistry(registry)
				} else {
					srv.SetChannelRegistry(nil)
				}
			}
		}()
	}

	// Reader goroutines: call handleAgentOutboundMessage which reads
	// s.channelRegistry on the non-broker path.
	for i := 0; i < goroutines/2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				func() {
					defer func() {
						if r := recover(); r != nil {
							panicked.Add(1)
						}
					}()
					body, _ := json.Marshal(OutboundMessageRequest{
						Recipient: "user:cr-human@example.com",
						Msg:       "race probe",
					})
					req := httptest.NewRequest(http.MethodPost,
						"/api/v1/agents/"+agent.ID+"/outbound-message",
						bytes.NewReader(body))
					req.Header.Set("Content-Type", "application/json")
					req = req.WithContext(contextWithIdentity(req.Context(),
						&agentIdentityWrapper{&AgentTokenClaims{
							Claims:    jwt.Claims{Subject: agent.ID},
							ProjectID: project.ID,
						}}))
					w := httptest.NewRecorder()
					srv.handleAgentOutboundMessage(w, req, agent.ID)
				}()
			}
		}()
	}

	wg.Wait()

	if n := panicked.Load(); n > 0 {
		t.Fatalf("detected %d nil-pointer panics from unguarded channelRegistry reads", n)
	}
}

// TestDispatcherRace verifies that concurrent SetDispatcher(nil) calls do not
// race with reads in handleAgentResetAuth and handleAdminResetAuthAll.
//
// The pre-fix code read s.dispatcher without holding s.mu:
//
//	if s.dispatcher == nil { return error }
//	s.dispatcher.DispatchAgentResetAuth(...)
//
// The fix snapshots via GetDispatcher() (which holds RLock) and uses the local.
//
// This test has two detection modes:
//
//  1. Without -race (go test -count=1 ./pkg/hub/): goroutines recover from
//     nil-deref panics and fail the test. This is probabilistic — the timing
//     window is narrow and may not be hit on every run. A green result without
//     -race is NOT proof of correctness.
//
//  2. With -race (go test -race ./pkg/hub/): the race detector instruments
//     every memory access and will reliably detect the unsynchronized read
//     even if the nil-deref timing is never hit.
//
// CI does not currently pass -race, so this test does not gate merges.
func TestDispatcherRace(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   api.NewUUID(),
		Name: "disp-race",
		Slug: "disp-race",
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	agent := &store.Agent{
		ID:        api.NewUUID(),
		Name:      "disp-agent",
		Slug:      "disp-agent",
		ProjectID: project.ID,
		Phase:     "running",
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	const goroutines = 8
	const iterations = 500

	var panicked atomic.Int32
	var wg sync.WaitGroup

	// Writer goroutines: toggle dispatcher between a noop and nil.
	for i := 0; i < goroutines/2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			noop := noopDispatcher{}
			for j := 0; j < iterations; j++ {
				if j%2 == 0 {
					srv.SetDispatcher(noop)
				} else {
					srv.SetDispatcher(nil)
				}
			}
		}()
	}

	// Reader goroutines: call handleAgentResetAuth concurrently.
	userIdent := NewAuthenticatedUser("u-1", "race@test", "Race Tester", "admin", "test")
	for i := 0; i < goroutines/2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				func() {
					defer func() {
						if r := recover(); r != nil {
							panicked.Add(1)
						}
					}()
					req := httptest.NewRequest(http.MethodPost,
						"/api/v1/agents/"+agent.ID+"/reset-auth", nil)
					req = req.WithContext(contextWithIdentity(req.Context(), userIdent))
					w := httptest.NewRecorder()
					srv.handleAgentResetAuth(w, req, agent.ID)
				}()
			}
		}()
	}

	wg.Wait()

	if n := panicked.Load(); n > 0 {
		t.Fatalf("detected %d nil-pointer panics from unguarded dispatcher reads", n)
	}
}

// TestPluginManagerRace verifies that concurrent writes to s.pluginManager
// do not race with reads in getA2ABridgeExternalURL.
//
// The pre-fix code read s.pluginManager without holding s.mu:
//
//	if s.pluginManager == nil { return "" }
//	cfg := s.pluginManager.GetPluginConfig(...)
//
// The fix snapshots under RLock and uses the local.
//
// Note: SetPluginManager(nil) panics inside registerReconnectCallbacks, so
// this test toggles between two distinct non-nil managers and relies on the
// race detector to catch the unsynchronized field access. It cannot provoke
// a nil-deref panic under normal go test, making it strictly a -race test.
//
// WARNING: This test is a no-op without the race detector. A green run
// under `go test ./pkg/hub/` (no -race) proves nothing — the test
// exercises a concurrency window that only the race detector can observe.
// CI does not currently pass -race, so this test does not gate merges.
func TestPluginManagerRace(t *testing.T) {
	srv, _ := testServer(t)

	const goroutines = 8
	const iterations = 500

	var wg sync.WaitGroup

	mockA := &noopPluginManager{}
	mockB := &noopPluginManager{}

	// Writer goroutines: toggle pluginManager between two distinct instances.
	for i := 0; i < goroutines/2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				if j%2 == 0 {
					srv.SetPluginManager(mockA)
				} else {
					srv.SetPluginManager(mockB)
				}
			}
		}()
	}

	// Reader goroutines: call getA2ABridgeExternalURL concurrently.
	for i := 0; i < goroutines/2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				srv.getA2ABridgeExternalURL()
			}
		}()
	}

	wg.Wait()
}

// noopPluginManager satisfies IntegrationManager with zero-value returns.
type noopPluginManager struct{}

func (*noopPluginManager) ListPlugins() []string                         { return nil }
func (*noopPluginManager) HasPlugin(_, _ string) bool                    { return false }
func (*noopPluginManager) GetPluginConfig(_, _ string) map[string]string { return nil }
func (*noopPluginManager) GetPluginConfigFile(_, _ string) string        { return "" }
func (*noopPluginManager) IsSelfManaged(_, _ string) bool                { return false }
func (*noopPluginManager) GetDeploymentMode(_, _ string) plugin.DeploymentMode {
	return plugin.DeploymentModePlugin
}
func (*noopPluginManager) ConfigureBroker(_ string, _ map[string]string) error     { return nil }
func (*noopPluginManager) ReplaceBrokerConfig(_ string, _ map[string]string) error { return nil }
func (*noopPluginManager) RestartBrokerPlugin(_ string, _ map[string]string) error { return nil }
func (*noopPluginManager) Reconnect(_, _ string) error                             { return nil }
func (*noopPluginManager) BrokerHealthCheck(_ string) (string, string, map[string]string, error) {
	return "", "", nil, nil
}
func (*noopPluginManager) BrokerInfo(_ string) (string, string, []string, error) {
	return "", "", nil, nil
}
func (*noopPluginManager) UpdatePlugin(_, _ string) error                            { return nil }
func (*noopPluginManager) InstallPlugin(_, _, _, _ string) error                     { return nil }
func (*noopPluginManager) LoadOne(_, _ string, _ plugin.PluginEntry, _ string) error { return nil }
func (*noopPluginManager) GetBroker(_ string) (eventbus.EventBus, error)             { return nil, nil }
func (*noopPluginManager) GetGRPCBrokerAdapter(_ string) plugin.GRPCBrokerClient     { return nil }
