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

package mcpserver_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/mcpserver"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type headerRoundTripper struct {
	base    http.RoundTripper
	headers map[string]string
}

func (h *headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	for k, v := range h.headers {
		clone.Header.Set(k, v)
	}
	return h.base.RoundTrip(clone)
}

type noopMessageDispatcher struct {
	hub.AgentDispatcher
}

func (*noopMessageDispatcher) DispatchAgentMessage(_ context.Context, _ *store.Agent, _ string, _ bool, _ *messages.StructuredMessage) error {
	return nil
}

func newIntegrationHub(t *testing.T) (*hub.Server, store.Store) {
	t.Helper()
	ctx := context.Background()

	dbPath := filepath.Join(t.TempDir(), "hub.db")
	entClient, err := entc.OpenSQLite(dbPath, entc.PoolConfig{MaxOpenConns: 1})
	if err != nil {
		t.Fatalf("entc.OpenSQLite: %v", err)
	}
	if err := entc.AutoMigrate(ctx, entClient); err != nil {
		t.Fatalf("entc.AutoMigrate: %v", err)
	}
	st := entadapter.NewCompositeStore(entClient)
	t.Cleanup(func() { _ = st.Close() })

	srv, err := hub.New(hub.ServerConfig{
		Host: "127.0.0.1",
		Port: 0,
	}, st)
	if err != nil {
		t.Fatalf("hub.New: %v", err)
	}
	srv.SetDispatcher(&noopMessageDispatcher{})
	return srv, st
}

func TestIntegration_SendAndReadMessagesWithRealHub(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	hubSrv, st := newIntegrationHub(t)

	// 1. Seed a user in the real Hub store and grant hub-member so they can create a project.
	user := &store.User{
		ID:          uuid.NewString(),
		Email:       "alice@example.com",
		DisplayName: "Alice",
		Role:        store.UserRoleMember,
		Status:      "active",
	}
	if err := st.CreateUser(ctx, user); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	hubMemberRole, err := st.GetRoleDefinitionByName(ctx, "hub-member", store.RoleScopeSystem)
	if err != nil {
		t.Fatalf("GetRoleDefinitionByName(hub-member): %v", err)
	}
	if _, err := st.CreateRoleBinding(ctx, &store.RoleBinding{
		ID:               uuid.NewString(),
		PrincipalType:    "user",
		PrincipalID:      user.ID,
		RoleDefinitionID: hubMemberRole.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        user.ID,
	}); err != nil {
		t.Fatalf("CreateRoleBinding(hub-member): %v", err)
	}

	inProcClient := func(opt hubclient.Option) hubclient.Client {
		hc := &http.Client{
			Transport: &mcpserver.InProcessTransport{Handler: hubSrv.Handler()},
		}
		cl, err := hubclient.New("http://in-process",
			hubclient.WithHTTPClient(hc),
			opt,
			hubclient.WithTransportAuth(nil, 0),
		)
		if err != nil {
			t.Fatalf("hubclient.New: %v", err)
		}
		return cl
	}

	// Mint a user JWT to create the project and UATs through the real Hub REST API.
	userJWT, _, err := hubSrv.GetUserTokenService().MintAccessToken(hub.UserTokenSubject{
		UserID:      user.ID,
		Email:       user.Email,
		DisplayName: user.DisplayName,
		Role:        user.Role,
		ClientType:  hub.ClientTypeCLI,
	})
	if err != nil {
		t.Fatalf("MintAccessToken: %v", err)
	}
	userHubClient := inProcClient(hubclient.WithBearerToken(userJWT))

	proj, err := userHubClient.Projects().Create(ctx, &hubclient.CreateProjectRequest{
		Name: "MCP Test Project",
		Slug: "mcp-test",
	})
	if err != nil {
		t.Fatalf("Projects.Create: %v", err)
	}

	agent := &store.Agent{
		ID:              uuid.NewString(),
		Slug:            "echo-t",
		Name:            "echo-t",
		ProjectID:       proj.ID,
		OwnerID:         user.ID,
		CreatedBy:       user.ID,
		RuntimeBrokerID: uuid.NewString(),
		Phase:           "running",
		MessageMode:     "project",
		RunID:           "run-1",
	}
	if err := st.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	// Mint an agent JWT for echo-t so the test can write replies through the Hub API.
	agentToken, agentCred, err := hubSrv.SignAgentToken(hub.AgentTokenGrant{
		AgentID:   agent.ID,
		ProjectID: proj.ID,
		Ancestry:  []string{user.ID},
		Scopes:    []hub.AgentTokenScope{hub.ScopeAgentStatusUpdate, hub.ScopeProjectRead},
	}, "run-1")
	if err != nil {
		t.Fatalf("SignAgentToken: %v", err)
	}
	if err := st.CreateAgentCredential(ctx, agentCred); err != nil {
		t.Fatalf("CreateAgentCredential: %v", err)
	}
	fullUAT, err := userHubClient.Tokens().Create(ctx, &hubclient.CreateTokenRequest{
		Name:      "mcp-p1-full",
		ProjectID: proj.ID,
		Scopes: []string{
			"project:read",
			"agent:read",
			"agent:message",
			"inbox:read",
			"inbox:write",
		},
	})
	if err != nil {
		t.Fatalf("create full UAT: %v", err)
	}

	// Also mint a UAT lacking agent:message to verify Hub authz reason propagation.
	noMsgUAT, err := userHubClient.Tokens().Create(ctx, &hubclient.CreateTokenRequest{
		Name:      "mcp-p1-no-msg",
		ProjectID: proj.ID,
		Scopes: []string{
			"project:read",
			"agent:read",
			"inbox:read",
		},
	})
	if err != nil {
		t.Fatalf("create no-msg UAT: %v", err)
	}

	// 2. Start the embedded MCP listener on 127.0.0.1:0 with an in-process RoundTripper.
	var expEnabled atomic.Bool
	expEnabled.Store(false) // starts off to test 404 gate

	backend := mcpserver.NewBackend(mcpserver.BackendOptions{
		HubBaseURL: "http://in-process",
		Transport:  &mcpserver.InProcessTransport{Handler: hubSrv.Handler()},
		Version:    "integration-test",
		ExperimentChecker: func(_ context.Context, _ mcpserver.Credential, name string) (bool, error) {
			if name == experiments.MCPServer {
				return expEnabled.Load(), nil
			}
			return hubSrv.ExperimentEnabled(name), nil
		},
		DisableTransportAuth: true,
	})

	mcpSrv, err := mcpserver.New(mcpserver.Options{
		Version: "integration-test",
		Backend: backend,
	})
	if err != nil {
		t.Fatalf("mcpserver.New: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	srvCtx, srvCancel := context.WithCancel(ctx)
	srvDone := make(chan error, 1)
	go func() {
		srvDone <- mcpSrv.Serve(srvCtx, ln)
	}()
	defer func() {
		srvCancel()
		<-srvDone
	}()

	endpoint := "http://" + ln.Addr().String() + "/mcp"

	// 3. With hub.mcp_server off, /mcp answers 404.
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	req.Header.Set("Authorization", "Bearer "+fullUAT.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /mcp (exp off): %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("experiment off: got status %d, want 404", resp.StatusCode)
	}

	// Flip hub.mcp_server on.
	expEnabled.Store(true)

	// 4. Connect a go-sdk StreamableClientTransport with the static UAT header.
	connectClient := func(token string) *mcp.ClientSession {
		cl := mcp.NewClient(&mcp.Implementation{Name: "mcp-integration-client", Version: "1.0"}, nil)
		hc := &http.Client{
			Transport: &headerRoundTripper{
				base:    &http.Transport{DisableKeepAlives: true},
				headers: map[string]string{"Authorization": "Bearer " + token},
			},
		}
		sess, err := cl.Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint:   endpoint,
			HTTPClient: hc,
		}, nil)
		if err != nil {
			t.Fatalf("mcp Connect: %v", err)
		}
		t.Cleanup(func() { _ = sess.Close() })
		return sess
	}

	session := connectClient(fullUAT.Token)

	// 5. tools/list returns exactly the two P1 tools with schemas and annotations.
	listRes, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(listRes.Tools) != 2 {
		t.Fatalf("ListTools returned %d tools, want 2", len(listRes.Tools))
	}
	toolByName := make(map[string]*mcp.Tool, len(listRes.Tools))
	for _, tl := range listRes.Tools {
		toolByName[tl.Name] = tl
	}
	sendTool := toolByName["send_message"]
	readTool := toolByName["read_messages"]
	if sendTool == nil || readTool == nil {
		t.Fatalf("expected send_message and read_messages, got %v", toolByName)
	}
	if sendTool.InputSchema == nil || readTool.InputSchema == nil {
		t.Fatal("expected non-nil JSON input schemas on both tools")
	}
	if sendTool.Annotations == nil || sendTool.Annotations.OpenWorldHint == nil || !*sendTool.Annotations.OpenWorldHint {
		t.Fatalf("send_message annotations = %+v, want OpenWorldHint=true", sendTool.Annotations)
	}
	if readTool.Annotations == nil || !readTool.Annotations.ReadOnlyHint {
		t.Fatalf("read_messages annotations = %+v, want ReadOnlyHint=true", readTool.Annotations)
	}
	if !strings.Contains(strings.ToLower(readTool.Description), "untrusted") {
		t.Errorf("read_messages description should warn that message text is untrusted: %q", readTool.Description)
	}

	// 6. Client calls send_message to echo-t in proj.ID.
	sendRes, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "send_message",
		Arguments: map[string]any{
			"project": proj.ID,
			"agent":   agent.Slug,
			"text":    "ping-p1-nonce",
		},
	})
	if err != nil {
		t.Fatalf("CallTool(send_message): %v", err)
	}
	if sendRes.IsError {
		t.Fatalf("send_message returned tool error: %s", sendRes.Content[0].(*mcp.TextContent).Text)
	}
	var sendOut mcpserver.SendResult
	if err := json.Unmarshal([]byte(sendRes.Content[0].(*mcp.TextContent).Text), &sendOut); err != nil {
		t.Fatalf("unmarshal send_message output: %v", err)
	}
	if sendOut.MessageID == "" || sendOut.ConversationID == "" || sendOut.Cursor == "" {
		t.Fatalf("send_message missing fields: %+v", sendOut)
	}

	// 7. Agent reply is written through the Hub API using the agent's JWT.
	agentHubClient := inProcClient(hubclient.WithAgentToken(agentToken))
	replyOut, err := agentHubClient.Agents().SendOutboundMessage(ctx, agent.ID, &hubclient.OutboundMessageRequest{
		Recipient:      "user:" + user.Email,
		ConversationID: sendOut.ConversationID,
		Msg:            "PONG-p1-nonce",
		Type:           messages.TypeReply,
	})
	if err != nil {
		t.Fatalf("agent SendOutboundMessage: %v", err)
	}
	if replyOut == nil || replyOut.MessageID == "" {
		t.Fatalf("unexpected empty agent reply result: %+v", replyOut)
	}

	// 8. Client calls read_messages from the send cursor -> returns exactly the agent's reply.
	readRes, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "read_messages",
		Arguments: map[string]any{
			"conversation_id": sendOut.ConversationID,
			"cursor":          sendOut.Cursor,
		},
	})
	if err != nil {
		t.Fatalf("CallTool(read_messages): %v", err)
	}
	if readRes.IsError {
		t.Fatalf("read_messages returned tool error: %+v", readRes.Content)
	}
	var readOut mcpserver.ReadMessagesOutput
	if err := json.Unmarshal([]byte(readRes.Content[0].(*mcp.TextContent).Text), &readOut); err != nil {
		t.Fatalf("unmarshal read_messages output: %v", err)
	}
	if len(readOut.Messages) != 1 {
		t.Fatalf("read_messages returned %d messages, want 1: %+v", len(readOut.Messages), readOut.Messages)
	}
	gotMsg := readOut.Messages[0]
	if gotMsg.Text != "PONG-p1-nonce" {
		t.Fatalf("reply text = %q, want %q", gotMsg.Text, "PONG-p1-nonce")
	}
	if gotMsg.Sender.Kind != "agent" {
		t.Fatalf("reply sender.kind = %q, want %q", gotMsg.Sender.Kind, "agent")
	}
	if gotMsg.ConversationID != sendOut.ConversationID {
		t.Fatalf("reply conversation_id = %q, want %q", gotMsg.ConversationID, sendOut.ConversationID)
	}
	if readOut.NextCursor == "" || readOut.NextCursor == sendOut.Cursor {
		t.Fatalf("expected next_cursor to advance from %q, got %q", sendOut.Cursor, readOut.NextCursor)
	}

	// 9. A UAT lacking agent:message gets a tool error carrying the Hub's reason.
	noMsgSession := connectClient(noMsgUAT.Token)
	deniedRes, err := noMsgSession.CallTool(ctx, &mcp.CallToolParams{
		Name: "send_message",
		Arguments: map[string]any{
			"project": proj.ID,
			"agent":   agent.Slug,
			"text":    "should fail",
		},
	})
	if err != nil {
		t.Fatalf("CallTool(send_message denied) protocol error: %v", err)
	}
	if !deniedRes.IsError {
		t.Fatalf("expected tool error for UAT lacking agent:message, got success: %+v", deniedRes.Content)
	}
	errText := deniedRes.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(errText, "403") && !strings.Contains(strings.ToLower(errText), "scope") && !strings.Contains(strings.ToLower(errText), "agent:message") {
		t.Fatalf("expected Hub authz reason in tool error, got: %q", errText)
	}
}
