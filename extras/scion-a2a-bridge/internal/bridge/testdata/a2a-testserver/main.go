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

// Command a2a-testserver runs a minimal A2A SDK HTTP server backed by
// PostgresTaskStore for cross-process integration tests.
//
// Usage:
//
//	a2a-testserver -port=PORT -database-url=URL [-project=PROJ] [-agent=AGENT] [-caller-id=ID] [-mode=MODE]
//
// Modes:
//   - test (default): Uses testExecutor that completes tasks synchronously
//     without requiring a real Hub.
//   - production: Uses real ScionExecutor with a mock Hub client. The mock
//     captures Hub sends and exposes them via /internal/hub-sends. The
//     broker ingress is available at /internal/broker-publish. This mode
//     exercises the full production path: barrier, lease, Hub send, event
//     poll, durable correlation.
//
// The server prints "READY <pid> <port>" to stdout once listening, then serves
// A2A JSON-RPC requests until stdin is closed or SIGTERM is received.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/bridge"
	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/state"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

var (
	port        = flag.Int("port", 0, "port to listen on (0 for random)")
	databaseURL = flag.String("database-url", "", "Postgres connection URL")
	project     = flag.String("project", "test-proj", "project slug for route context")
	agent       = flag.String("agent", "test-agent", "agent slug for route context")
	callerID    = flag.String("caller-id", "", "optional caller identity (UserID) for ownership isolation")
	mode        = flag.String("mode", "test", "execution mode: test or production")
)

// testExecutor is a minimal AgentExecutor that creates tasks and transitions
// them to completed without requiring a real Scion Hub.
type testExecutor struct{}

func (e *testExecutor) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		// Emit submitted task.
		if execCtx.StoredTask == nil {
			task := a2a.NewSubmittedTask(execCtx, execCtx.Message)
			if !yield(task, nil) {
				return
			}
		}

		// Emit working status.
		if !yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateWorking, nil), nil) {
			return
		}

		// Emit completed status with the echoed message.
		parts := execCtx.Message.Parts
		var sdkParts []*a2a.Part
		for _, p := range parts {
			sdkParts = append(sdkParts, p)
		}
		if len(sdkParts) == 0 {
			sdkParts = append(sdkParts, a2a.NewTextPart("echo"))
		}
		replyMsg := a2a.NewMessageForTask(a2a.MessageRoleAgent, execCtx, sdkParts...)
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCompleted, replyMsg), nil)
	}
}

func (e *testExecutor) Cancel(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(execCtx, a2a.TaskStateCanceled, nil), nil)
	}
}

// hubSendCapture records a captured Hub send for test inspection.
type hubSendCapture struct {
	AgentID    string                      `json:"agentID"`
	TaskID     string                      `json:"taskID"`
	Message    *messages.StructuredMessage `json:"message"`
	CapturedAt time.Time                   `json:"capturedAt"`
}

// mockProductionHub is a thread-safe mock Hub client for production mode.
type mockProductionHub struct {
	mu       sync.Mutex
	captures []hubSendCapture
	agents   *mockProdAgentService
}

func newMockProductionHub(projectSlug, agentSlug string) *mockProductionHub {
	h := &mockProductionHub{}
	h.agents = &mockProdAgentService{
		hub:         h,
		projectSlug: projectSlug,
		agentSlug:   agentSlug,
	}
	return h
}

func (h *mockProductionHub) getCaptures() []hubSendCapture {
	h.mu.Lock()
	defer h.mu.Unlock()
	result := make([]hubSendCapture, len(h.captures))
	copy(result, h.captures)
	return result
}

func (h *mockProductionHub) recordSend(agentID string, msg *messages.StructuredMessage) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.captures = append(h.captures, hubSendCapture{
		AgentID:    agentID,
		TaskID:     msg.Metadata["a2aTaskId"],
		Message:    msg,
		CapturedAt: time.Now(),
	})
}

// mockProdAgentService implements hubclient.AgentService for production testing.
type mockProdAgentService struct {
	hub         *mockProductionHub
	projectSlug string
	agentSlug   string
}

func (m *mockProdAgentService) SendStructuredMessage(ctx context.Context, agentID string, msg *messages.StructuredMessage, interrupt, notify, wake bool) (*hubclient.MessageResponse, error) {
	m.hub.recordSend(agentID, msg)
	return nil, nil
}

func (m *mockProdAgentService) List(ctx context.Context, opts *hubclient.ListAgentsOptions) (*hubclient.ListAgentsResponse, error) {
	return &hubclient.ListAgentsResponse{
		Agents: []hubclient.Agent{
			{
				ID:        "mock-agent-id",
				Name:      m.agentSlug,
				Slug:      m.agentSlug,
				ProjectID: m.projectSlug,
			},
		},
	}, nil
}

// Stub implementations for the remaining AgentService methods.
func (m *mockProdAgentService) Get(ctx context.Context, agentID string) (*hubclient.Agent, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *mockProdAgentService) Create(ctx context.Context, req *hubclient.CreateAgentRequest) (*hubclient.CreateAgentResponse, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *mockProdAgentService) Update(ctx context.Context, agentID string, req *hubclient.UpdateAgentRequest) (*hubclient.Agent, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *mockProdAgentService) ResetAuth(ctx context.Context, agentID string) error {
	return fmt.Errorf("not implemented")
}
func (m *mockProdAgentService) Delete(ctx context.Context, agentID string, opts *hubclient.DeleteAgentOptions) error {
	return fmt.Errorf("not implemented")
}
func (m *mockProdAgentService) Start(ctx context.Context, agentID string) error {
	return fmt.Errorf("not implemented")
}
func (m *mockProdAgentService) Stop(ctx context.Context, agentID string) error {
	return fmt.Errorf("not implemented")
}
func (m *mockProdAgentService) Suspend(ctx context.Context, agentID string) error {
	return fmt.Errorf("not implemented")
}
func (m *mockProdAgentService) Restart(ctx context.Context, agentID string) error {
	return fmt.Errorf("not implemented")
}
func (m *mockProdAgentService) StopAll(ctx context.Context) (*hubclient.StopAllResponse, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *mockProdAgentService) SendMessage(ctx context.Context, agentID string, message string, interrupt bool) error {
	return fmt.Errorf("not implemented")
}
func (m *mockProdAgentService) BroadcastMessage(ctx context.Context, msg *messages.StructuredMessage, interrupt bool) (*hubclient.BroadcastResponse, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *mockProdAgentService) SubmitEnv(ctx context.Context, agentID string, req *hubclient.SubmitEnvRequest) (*hubclient.CreateAgentResponse, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *mockProdAgentService) Restore(ctx context.Context, agentID string) (*hubclient.Agent, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *mockProdAgentService) Exec(ctx context.Context, agentID string, command []string, timeout int) (*hubclient.ExecResponse, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *mockProdAgentService) GetLogs(ctx context.Context, agentID string, opts *hubclient.GetLogsOptions) (string, error) {
	return "", fmt.Errorf("not implemented")
}
func (m *mockProdAgentService) SendOutboundMessage(ctx context.Context, agentID string, msg *hubclient.OutboundMessageRequest) error {
	return fmt.Errorf("not implemented")
}
func (m *mockProdAgentService) GetCloudLogs(ctx context.Context, agentID string, opts *hubclient.GetCloudLogsOptions) (*hubclient.CloudLogsResponse, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *mockProdAgentService) StreamCloudLogs(ctx context.Context, agentID string, opts *hubclient.GetCloudLogsOptions, handler func(hubclient.CloudLogEntry)) error {
	return fmt.Errorf("not implemented")
}
func (m *mockProdAgentService) SetMessageMode(ctx context.Context, agentID string, req *hubclient.SetMessageModeRequest, opts *hubclient.SetMessageModeOptions) (*hubclient.SetMessageModeResponse, error) {
	return nil, fmt.Errorf("not implemented")
}

// mockProdHubClient implements hubclient.Client.
type mockProdHubClient struct {
	agents *mockProdAgentService
}

func (m *mockProdHubClient) Agents() hubclient.AgentService                 { return m.agents }
func (m *mockProdHubClient) ProjectAgents(string) hubclient.AgentService    { return m.agents }
func (m *mockProdHubClient) Projects() hubclient.ProjectService             { return nil }
func (m *mockProdHubClient) RuntimeBrokers() hubclient.RuntimeBrokerService { return nil }
func (m *mockProdHubClient) Templates() hubclient.TemplateService           { return nil }
func (m *mockProdHubClient) HarnessConfigs() hubclient.HarnessConfigService { return nil }
func (m *mockProdHubClient) Workspace() hubclient.WorkspaceService          { return nil }
func (m *mockProdHubClient) Users() hubclient.UserService                   { return nil }
func (m *mockProdHubClient) Env() hubclient.EnvService                      { return nil }
func (m *mockProdHubClient) Secrets() hubclient.SecretService               { return nil }
func (m *mockProdHubClient) Auth() hubclient.AuthService                    { return nil }
func (m *mockProdHubClient) Notifications() hubclient.NotificationService   { return nil }
func (m *mockProdHubClient) Tokens() hubclient.TokenService                 { return nil }
func (m *mockProdHubClient) Subscriptions() hubclient.SubscriptionService   { return nil }
func (m *mockProdHubClient) SubscriptionTemplates() hubclient.SubscriptionTemplateService {
	return nil
}
func (m *mockProdHubClient) ScheduledEvents(string) hubclient.ScheduledEventService { return nil }
func (m *mockProdHubClient) Schedules(string) hubclient.ScheduleService             { return nil }
func (m *mockProdHubClient) GCPServiceAccounts() hubclient.GCPServiceAccountService { return nil }
func (m *mockProdHubClient) Messages() hubclient.MessageService                     { return nil }
func (m *mockProdHubClient) Conversations() hubclient.ConversationService           { return nil }
func (m *mockProdHubClient) AllowList() hubclient.AllowListService                  { return nil }
func (m *mockProdHubClient) Invites() hubclient.InviteService                       { return nil }
func (m *mockProdHubClient) Skills() hubclient.SkillService                         { return nil }
func (m *mockProdHubClient) SkillRegistries() hubclient.SkillRegistryService        { return nil }
func (m *mockProdHubClient) ProjectInjectedSkills(projectID string) hubclient.InjectedSkillsService {
	return nil
}
func (m *mockProdHubClient) UserInjectedSkills() hubclient.InjectedSkillsService { return nil }
func (m *mockProdHubClient) ProjectPreStartHooks(projectID string) hubclient.ProjectPreStartHookService {
	return nil
}
func (m *mockProdHubClient) HubPreStartHooks() hubclient.HubPreStartHookService { return nil }
func (m *mockProdHubClient) Messaging() hubclient.MessagingService              { return nil }
func (m *mockProdHubClient) Health(ctx context.Context) (*hubclient.HealthResponse, error) {
	return &hubclient.HealthResponse{}, nil
}
func (m *mockProdHubClient) DiscoverSkillsDirectory(ctx context.Context, req hubclient.DiscoverSkillsDirectoryRequest) (*hubclient.DiscoverSkillsDirectoryResponse, error) {
	return nil, nil
}

func main() {
	flag.Parse()
	if *databaseURL == "" {
		fmt.Fprintln(os.Stderr, "missing -database-url")
		os.Exit(1)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	store, err := bridge.NewPostgresTaskStore(*databaseURL)
	if err != nil {
		log.Error("failed to create store", "error", err)
		os.Exit(1)
	}
	defer store.Close()

	var jsonrpcHandler http.Handler
	var mockHub *mockProductionHub

	switch *mode {
	case "production":
		jsonrpcHandler, mockHub = setupProductionMode(store, log)
	default:
		jsonrpcHandler = setupTestMode(store, log)
	}

	// Wrap with middleware that sets RouteInfo and optional CallerIdentity.
	// Supports per-request overrides via X-Test-* headers for isolation testing.
	routeHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proj := *project
		agSlug := *agent
		cid := *callerID

		// Per-request overrides for isolation testing.
		if v := r.Header.Get("X-Test-Project"); v != "" {
			proj = v
		}
		if v := r.Header.Get("X-Test-Agent"); v != "" {
			agSlug = v
		}
		if v := r.Header.Get("X-Test-Caller-ID"); v != "" {
			cid = v
		}

		ctx := bridge.WithRouteInfo(r.Context(), bridge.RouteInfo{
			ProjectSlug: proj,
			AgentSlug:   agSlug,
		})
		if cid != "" {
			ctx = bridge.WithCallerIdentity(ctx, &bridge.CallerIdentity{
				UserID:    cid,
				TokenType: "uat",
			})
		}
		jsonrpcHandler.ServeHTTP(w, r.WithContext(ctx))
	})

	mux := http.NewServeMux()
	mux.Handle("/", routeHandler)

	// Production-mode endpoints for test orchestration.
	if mockHub != nil {
		// GET /internal/hub-sends — returns captured Hub sends as JSON.
		mux.HandleFunc("/internal/hub-sends", func(w http.ResponseWriter, r *http.Request) {
			captures := mockHub.getCaptures()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(captures)
		})
	}

	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		log.Error("listen failed", "error", err)
		os.Exit(1)
	}

	actualPort := listener.Addr().(*net.TCPAddr).Port

	server := &http.Server{Handler: mux}

	// Print readiness signal with PID and port.
	fmt.Fprintf(os.Stdout, "READY %d %d\n", os.Getpid(), actualPort)
	os.Stdout.Sync()

	// Shutdown on signal or stdin close.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		select {
		case <-sigCh:
		case <-ctx.Done():
		}
		server.Shutdown(context.Background())
	}()

	// Also shutdown when stdin is closed (parent process exits).
	go func() {
		buf := make([]byte, 1)
		for {
			_, err := os.Stdin.Read(buf)
			if err != nil {
				cancel()
				return
			}
		}
	}()

	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		log.Error("server error", "error", err)
		os.Exit(1)
	}
}

// setupTestMode creates the test-mode handler (testExecutor, no Hub).
func setupTestMode(store *bridge.PostgresTaskStore, log *slog.Logger) http.Handler {
	handler := a2asrv.NewHandler(
		&testExecutor{},
		a2asrv.WithTaskStore(store),
		a2asrv.WithLogger(log),
	)
	return a2asrv.NewJSONRPCHandler(handler)
}

// setupProductionMode creates the production-mode handler with ScionExecutor,
// mock Hub, BarrierTaskStore, and broker ingress endpoint. Returns the
// JSON-RPC handler and the mock Hub for test introspection.
func setupProductionMode(sdkStore *bridge.PostgresTaskStore, log *slog.Logger) (http.Handler, *mockProductionHub) {
	// Bridge state store (for event log, contexts, legacy tasks).
	pgStore, err := state.NewPostgres(*databaseURL)
	if err != nil {
		log.Error("failed to create bridge state store", "error", err)
		os.Exit(1)
	}

	// Mock Hub client.
	mockHub := newMockProductionHub(*project, *agent)
	hubClient := &mockProdHubClient{agents: mockHub.agents}

	// Bridge with real stores + mock Hub.
	cfg := &bridge.Config{
		Hub:      bridge.HubConfig{User: "test-user"},
		Timeouts: bridge.TimeoutConfig{SendMessage: 15 * time.Second},
	}
	b := bridge.New(pgStore, hubClient, nil, cfg, nil, log)
	b.SetSDKTaskStore(sdkStore)

	// BarrierTaskStore wraps PostgresTaskStore for deterministic barrier.
	// No ScopedTaskStore — PostgresTaskStore is the authoritative owner enforcer.
	barrierStore := bridge.NewBarrierTaskStore(sdkStore)
	b.SetBarrierStore(barrierStore)

	// BrokerServer with production handler for broker ingress testing.
	brokerServer := bridge.NewBrokerServer(b.HandleBrokerMessage, log, context.Background())
	b.SetBroker(brokerServer)

	// ScionExecutor using the production Bridge.
	executor := bridge.NewScionExecutor(b, log)

	// SDK handler — PostgresTaskStore enforces owner_key at SQL level.
	handler := a2asrv.NewHandler(executor,
		a2asrv.WithTaskStore(barrierStore),
		a2asrv.WithLogger(log),
	)

	// DurableRequestHandler for SubscribeToTask.
	durableHandler := bridge.NewDurableRequestHandler(handler, sdkStore, pgStore, nil)
	jsonrpcHandler := a2asrv.NewJSONRPCHandler(durableHandler)

	// Wrap with broker-publish endpoint: POST /internal/broker-publish.
	// This calls broker.Publish, which is the production broker ingress.
	wrappedMux := http.NewServeMux()
	wrappedMux.Handle("/", jsonrpcHandler)
	wrappedMux.HandleFunc("/internal/broker-publish", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Topic   string                      `json:"topic"`
			Message *messages.StructuredMessage `json:"message"`
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := brokerServer.Publish(r.Context(), req.Topic, req.Message); err != nil {
			http.Error(w, "broker publish failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, `{"ok":true}`)
	})

	return wrappedMux, mockHub
}
