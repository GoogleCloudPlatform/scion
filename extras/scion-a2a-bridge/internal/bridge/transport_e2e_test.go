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

package bridge

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	a2agrpc "github.com/a2aproject/a2a-go/v2/a2agrpc/v0"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"

	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/state"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/projectcompat"
)

const (
	e2eProject   = "proj-1"
	e2eAgentSlug = "agent-a"
	e2eAgentID   = "agent-id-1"
	e2eAPIKey    = "e2e-secret"
	e2eHubUser   = "admin@test"
)

// e2eStack is a fully wired bridge: real state store, real ScionExecutor, real
// a2asrv handler, with the Scion Hub replaced by an in-process fake that echoes
// each message back through the broker path (Bridge.HandleBrokerMessage), which
// is exactly how a real Scion agent's reply reaches the bridge.
type e2eStack struct {
	bridge     *Bridge
	server     *Server
	sdkHandler a2asrv.RequestHandler
	route      RouteInfo
}

func newE2EStack(t *testing.T) *e2eStack {
	t.Helper()

	dir := t.TempDir()
	store, err := state.New(filepath.Join(dir, "e2e.db"))
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	cfg := &Config{
		Bridge:   BridgeConfig{ExternalURL: "https://e2e.test"},
		Hub:      HubConfig{Endpoint: "http://hub.invalid", User: e2eHubUser},
		Auth:     AuthConfig{Scheme: "apiKey", APIKey: e2eAPIKey},
		Projects: []ProjectConfig{{Slug: e2eProject, ExposedAgents: []string{e2eAgentSlug}}},
		Timeouts: TimeoutConfig{SendMessage: 10 * time.Second},
	}

	stack := &e2eStack{route: RouteInfo{ProjectSlug: e2eProject, AgentSlug: e2eAgentSlug}}

	// Fake Scion agent: on Hub send, reply asynchronously over the broker path.
	agents := &mockAgentService{
		listFn: func(ctx context.Context, opts *hubclient.ListAgentsOptions) (*hubclient.ListAgentsResponse, error) {
			return &hubclient.ListAgentsResponse{
				Agents: []hubclient.Agent{{ID: e2eAgentID, Slug: e2eAgentSlug, ProjectID: e2eProject}},
			}, nil
		},
		sendFn: func(ctx context.Context, agentID string, msg *messages.StructuredMessage, interrupt, notify, wake bool) (*hubclient.MessageResponse, error) {
			reply := &messages.StructuredMessage{
				Type:      msg.Type,
				Sender:    "agent:" + e2eAgentSlug,
				Recipient: msg.Sender,
				Msg:       "echo: " + msg.Msg,
				Metadata:  map[string]string{"a2aTaskId": msg.Metadata["a2aTaskId"]},
			}
			go func() {
				// Small delay so the reply arrives after the waiter is armed.
				time.Sleep(20 * time.Millisecond)
				topic := projectcompat.UserTopic(e2eProject, e2eHubUser)
				if err := stack.bridge.HandleBrokerMessage(context.Background(), topic, reply); err != nil {
					t.Logf("fake agent: HandleBrokerMessage: %v", err)
				}
			}()
			return &hubclient.MessageResponse{}, nil
		},
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	b := New(store, &mockHubClient{agents: agents}, nil, cfg, nil, log)
	t.Cleanup(func() { b.Shutdown() })
	stack.bridge = b

	executor := NewScionExecutor(b, log)
	scopedStore := NewScopedTaskStore(taskstore.NewInMemory(&taskstore.InMemoryStoreConfig{
		Authenticator: RouteKeyAuthenticator(),
	}))
	sdkRequestHandler := a2asrv.NewHandler(
		executor,
		a2asrv.WithLogger(log),
		a2asrv.WithCapabilityChecks(&a2a.AgentCapabilities{Streaming: true, PushNotifications: false}),
		a2asrv.WithAgentInactivityTimeout(cfg.Timeouts.SendMessage),
		a2asrv.WithTaskStore(scopedStore),
	)
	b.SetSDKRequestHandler(sdkRequestHandler)
	stack.sdkHandler = sdkRequestHandler

	stack.server = NewServer(b, cfg, nil, log, a2asrv.NewJSONRPCHandler(sdkRequestHandler))
	return stack
}

// startGRPC starts the gRPC transport exactly as cmd/scion-a2a-bridge does.
func (s *e2eStack) startGRPC(t *testing.T) string {
	t.Helper()
	grpcServer := grpc.NewServer(
		grpc.MaxRecvMsgSize(1<<20),
		grpc.MaxConcurrentStreams(100),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             30 * time.Second,
			PermitWithoutStream: true,
		}),
		grpc.ChainUnaryInterceptor(
			s.server.AuthUnaryInterceptor(),
			RouteInfoUnaryInterceptor(s.route),
		),
		grpc.ChainStreamInterceptor(
			s.server.AuthStreamInterceptor(),
			RouteInfoStreamInterceptor(s.route),
		),
	)
	a2agrpc.NewHandler(s.sdkHandler).RegisterWith(grpcServer)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = grpcServer.Serve(lis) }()
	t.Cleanup(grpcServer.Stop)
	return lis.Addr().String()
}

// startREST starts the REST transport with the same middleware chain as
// cmd/scion-a2a-bridge.
func (s *e2eStack) startREST(t *testing.T) string {
	t.Helper()
	handler := s.server.AuthHTTPMiddleware(MaxBytesReaderMiddleware(1<<20,
		RouteInfoMiddleware(s.route,
			SSEWriteDeadlineMiddleware(
				a2asrv.NewRESTHandler(s.sdkHandler, a2asrv.WithTransportKeepAlive(15*time.Second)),
			),
		),
	))
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts.URL
}

// apiKeyRoundTripper injects the bridge API key into every REST request.
type apiKeyRoundTripper struct {
	key  string
	base http.RoundTripper
}

func (rt *apiKeyRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("X-API-Key", rt.key)
	base := rt.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(r)
}

// grpcAPIKeyCreds injects the bridge API key into gRPC metadata.
type grpcAPIKeyCreds struct{ key string }

func (c grpcAPIKeyCreds) GetRequestMetadata(ctx context.Context, uri ...string) (map[string]string, error) {
	return map[string]string{"x-api-key": c.key}, nil
}
func (c grpcAPIKeyCreds) RequireTransportSecurity() bool { return false }

// textOf extracts the concatenated text of a task's final status message.
func textOf(t *testing.T, task *a2a.Task) string {
	t.Helper()
	if task.Status.Message == nil {
		return ""
	}
	var sb strings.Builder
	for _, p := range task.Status.Message.Parts {
		sb.WriteString(p.Text())
	}
	return sb.String()
}

// TestE2E_GRPCTransport_RoundTrip drives the gRPC transport with the real
// a2a-go gRPC client over a real TCP socket and asserts a complete
// request/response round-trip through the executor and back.
func TestE2E_GRPCTransport_RoundTrip(t *testing.T) {
	stack := newE2EStack(t)
	addr := stack.startGRPC(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := a2aclient.NewFromEndpoints(ctx,
		[]*a2a.AgentInterface{{
			URL:             addr,
			ProtocolBinding: a2a.TransportProtocolGRPC,
			ProtocolVersion: a2a.ProtocolVersion("0.3"),
		}},
		a2aclient.WithDefaultsDisabled(),
		a2agrpc.WithGRPCTransport(
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithPerRPCCredentials(grpcAPIKeyCreds{key: e2eAPIKey}),
		),
	)
	if err != nil {
		t.Fatalf("a2aclient (gRPC): %v", err)
	}
	defer client.Destroy()

	result, err := client.SendMessage(ctx, &a2a.SendMessageRequest{
		Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hello over grpc")),
	})
	if err != nil {
		t.Fatalf("SendMessage over gRPC: %v", err)
	}

	task, ok := result.(*a2a.Task)
	if !ok {
		t.Fatalf("result type = %T, want *a2a.Task", result)
	}
	if task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("task state = %q, want completed (message: %q)", task.Status.State, textOf(t, task))
	}
	if got := textOf(t, task); got != "echo: hello over grpc" {
		t.Errorf("agent reply = %q, want %q", got, "echo: hello over grpc")
	}
	t.Logf("gRPC round-trip OK: task=%s state=%s reply=%q", task.ID, task.Status.State, textOf(t, task))
}

// TestE2E_GRPCTransport_Streaming exercises the streaming (server-side stream)
// path over gRPC, which also proves the stream interceptors propagate both the
// auth context and the route info.
func TestE2E_GRPCTransport_Streaming(t *testing.T) {
	stack := newE2EStack(t)
	addr := stack.startGRPC(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := a2aclient.NewFromEndpoints(ctx,
		[]*a2a.AgentInterface{{
			URL:             addr,
			ProtocolBinding: a2a.TransportProtocolGRPC,
			ProtocolVersion: a2a.ProtocolVersion("0.3"),
		}},
		a2aclient.WithDefaultsDisabled(),
		a2agrpc.WithGRPCTransport(
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithPerRPCCredentials(grpcAPIKeyCreds{key: e2eAPIKey}),
		),
	)
	if err != nil {
		t.Fatalf("a2aclient (gRPC): %v", err)
	}
	defer client.Destroy()

	var states []a2a.TaskState
	var final string
	for event, err := range client.SendStreamingMessage(ctx, &a2a.SendMessageRequest{
		Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("stream over grpc")),
	}) {
		if err != nil {
			t.Fatalf("streaming event error: %v", err)
		}
		switch e := event.(type) {
		case *a2a.Task:
			states = append(states, e.Status.State)
		case *a2a.TaskStatusUpdateEvent:
			states = append(states, e.Status.State)
			if e.Status.Message != nil {
				var sb strings.Builder
				for _, p := range e.Status.Message.Parts {
					sb.WriteString(p.Text())
				}
				if sb.Len() > 0 {
					final = sb.String()
				}
			}
		}
	}

	if len(states) == 0 || states[len(states)-1] != a2a.TaskStateCompleted {
		t.Fatalf("streaming states = %v, want to end in completed", states)
	}
	if final != "echo: stream over grpc" {
		t.Errorf("streamed reply = %q, want %q", final, "echo: stream over grpc")
	}
	t.Logf("gRPC streaming OK: states=%v reply=%q", states, final)
}

// TestE2E_GRPCTransport_RejectsBadCredentials proves auth is actually enforced
// on the gRPC transport end to end.
func TestE2E_GRPCTransport_RejectsBadCredentials(t *testing.T) {
	stack := newE2EStack(t)
	addr := stack.startGRPC(t)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	client, err := a2aclient.NewFromEndpoints(ctx,
		[]*a2a.AgentInterface{{
			URL:             addr,
			ProtocolBinding: a2a.TransportProtocolGRPC,
			ProtocolVersion: a2a.ProtocolVersion("0.3"),
		}},
		a2aclient.WithDefaultsDisabled(),
		a2agrpc.WithGRPCTransport(
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithPerRPCCredentials(grpcAPIKeyCreds{key: "wrong-key"}),
		),
	)
	if err != nil {
		t.Fatalf("a2aclient (gRPC): %v", err)
	}
	defer client.Destroy()

	if _, err := client.SendMessage(ctx, &a2a.SendMessageRequest{
		Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("nope")),
	}); err == nil {
		t.Fatal("expected SendMessage with bad credentials to fail")
	} else {
		t.Logf("gRPC auth rejection OK: %v", err)
	}
}

// TestE2E_RESTTransport_RoundTrip drives the REST transport with the real
// a2a-go REST client over a real HTTP socket.
func TestE2E_RESTTransport_RoundTrip(t *testing.T) {
	stack := newE2EStack(t)
	baseURL := stack.startREST(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	httpClient := &http.Client{
		Timeout:   30 * time.Second,
		Transport: &apiKeyRoundTripper{key: e2eAPIKey},
	}

	client, err := a2aclient.NewFromEndpoints(ctx,
		[]*a2a.AgentInterface{{
			URL:             baseURL,
			ProtocolBinding: a2a.TransportProtocolHTTPJSON,
			ProtocolVersion: a2a.ProtocolVersion("1.0"),
		}},
		a2aclient.WithDefaultsDisabled(),
		a2aclient.WithRESTTransport(httpClient),
	)
	if err != nil {
		t.Fatalf("a2aclient (REST): %v", err)
	}
	defer client.Destroy()

	result, err := client.SendMessage(ctx, &a2a.SendMessageRequest{
		Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hello over rest")),
	})
	if err != nil {
		t.Fatalf("SendMessage over REST: %v", err)
	}

	task, ok := result.(*a2a.Task)
	if !ok {
		t.Fatalf("result type = %T, want *a2a.Task", result)
	}
	if task.Status.State != a2a.TaskStateCompleted {
		t.Fatalf("task state = %q, want completed (message: %q)", task.Status.State, textOf(t, task))
	}
	if got := textOf(t, task); got != "echo: hello over rest" {
		t.Errorf("agent reply = %q, want %q", got, "echo: hello over rest")
	}
	t.Logf("REST round-trip OK: task=%s state=%s reply=%q", task.ID, task.Status.State, textOf(t, task))
}

// TestE2E_RESTTransport_Streaming exercises the REST SSE streaming path, which
// also covers SSEWriteDeadlineMiddleware in the live chain.
func TestE2E_RESTTransport_Streaming(t *testing.T) {
	stack := newE2EStack(t)
	baseURL := stack.startREST(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	httpClient := &http.Client{Transport: &apiKeyRoundTripper{key: e2eAPIKey}}
	client, err := a2aclient.NewFromEndpoints(ctx,
		[]*a2a.AgentInterface{{
			URL:             baseURL,
			ProtocolBinding: a2a.TransportProtocolHTTPJSON,
			ProtocolVersion: a2a.ProtocolVersion("1.0"),
		}},
		a2aclient.WithDefaultsDisabled(),
		a2aclient.WithRESTTransport(httpClient),
	)
	if err != nil {
		t.Fatalf("a2aclient (REST): %v", err)
	}
	defer client.Destroy()

	var states []a2a.TaskState
	var final string
	for event, err := range client.SendStreamingMessage(ctx, &a2a.SendMessageRequest{
		Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("stream over rest")),
	}) {
		if err != nil {
			t.Fatalf("streaming event error: %v", err)
		}
		switch e := event.(type) {
		case *a2a.Task:
			states = append(states, e.Status.State)
		case *a2a.TaskStatusUpdateEvent:
			states = append(states, e.Status.State)
			if e.Status.Message != nil {
				var sb strings.Builder
				for _, p := range e.Status.Message.Parts {
					sb.WriteString(p.Text())
				}
				if sb.Len() > 0 {
					final = sb.String()
				}
			}
		}
	}

	if len(states) == 0 || states[len(states)-1] != a2a.TaskStateCompleted {
		t.Fatalf("streaming states = %v, want to end in completed", states)
	}
	if final != "echo: stream over rest" {
		t.Errorf("streamed reply = %q, want %q", final, "echo: stream over rest")
	}
	t.Logf("REST streaming OK: states=%v reply=%q", states, final)
}

// TestE2E_RESTTransport_RejectsBadCredentials proves auth is enforced on REST.
func TestE2E_RESTTransport_RejectsBadCredentials(t *testing.T) {
	stack := newE2EStack(t)
	baseURL := stack.startREST(t)

	u, err := url.Parse(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	transport := a2aclient.NewRESTTransport(u, &http.Client{
		Transport: &apiKeyRoundTripper{key: "wrong-key"},
	})
	defer transport.Destroy()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if _, err := transport.SendMessage(ctx, a2aclient.ServiceParams{}, &a2a.SendMessageRequest{
		Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("nope")),
	}); err == nil {
		t.Fatal("expected REST SendMessage with bad credentials to fail")
	} else {
		t.Logf("REST auth rejection OK: %v", err)
	}
}
