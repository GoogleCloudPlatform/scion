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
	"path/filepath"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	grpcstatus "google.golang.org/grpc/status"

	pb "github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/a2apb"
	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/state"
)

// --- Type translation tests ---

func TestPbPartsToInternal(t *testing.T) {
	tests := []struct {
		name     string
		pbParts  []*pb.Part
		wantLen  int
		wantText string
		wantURL  string
	}{
		{
			name: "text part",
			pbParts: []*pb.Part{
				{Content: &pb.Part_Text{Text: "hello"}, MediaType: "text/plain"},
			},
			wantLen:  1,
			wantText: "hello",
		},
		{
			name: "url part",
			pbParts: []*pb.Part{
				{Content: &pb.Part_Url{Url: "https://example.com/file.txt"}},
			},
			wantLen: 1,
			wantURL: "https://example.com/file.txt",
		},
		{
			name:    "empty parts",
			pbParts: []*pb.Part{},
			wantLen: 0,
		},
		{
			name: "mixed parts",
			pbParts: []*pb.Part{
				{Content: &pb.Part_Text{Text: "text1"}, MediaType: "text/plain"},
				{Content: &pb.Part_Url{Url: "https://example.com"}},
			},
			wantLen: 2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			parts := pbPartsToInternal(tc.pbParts)
			if len(parts) != tc.wantLen {
				t.Fatalf("got %d parts, want %d", len(parts), tc.wantLen)
			}
			if tc.wantText != "" && parts[0].Text != tc.wantText {
				t.Errorf("text = %q, want %q", parts[0].Text, tc.wantText)
			}
			if tc.wantURL != "" && parts[0].URL != tc.wantURL {
				t.Errorf("url = %q, want %q", parts[0].URL, tc.wantURL)
			}
		})
	}
}

func TestInternalPartToProto(t *testing.T) {
	tests := []struct {
		name      string
		part      Part
		wantText  string
		wantURL   string
		wantMedia string
	}{
		{
			name:      "text part",
			part:      Part{Text: "hello", MediaType: "text/plain"},
			wantText:  "hello",
			wantMedia: "text/plain",
		},
		{
			name:    "url part",
			part:    Part{URL: "https://example.com/file"},
			wantURL: "https://example.com/file",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pbPart := internalPartToProto(tc.part)
			if tc.wantText != "" {
				if text, ok := pbPart.Content.(*pb.Part_Text); !ok || text.Text != tc.wantText {
					t.Errorf("text = %v, want %q", pbPart.Content, tc.wantText)
				}
			}
			if tc.wantURL != "" {
				if url, ok := pbPart.Content.(*pb.Part_Url); !ok || url.Url != tc.wantURL {
					t.Errorf("url = %v, want %q", pbPart.Content, tc.wantURL)
				}
			}
			if pbPart.MediaType != tc.wantMedia {
				t.Errorf("media_type = %q, want %q", pbPart.MediaType, tc.wantMedia)
			}
		})
	}
}

func TestInternalPartToProto_StructuredData(t *testing.T) {
	tests := []struct {
		name string
		data interface{}
	}{
		{
			name: "map data",
			data: map[string]interface{}{
				"key":    "value",
				"nested": map[string]interface{}{"a": float64(1)},
			},
		},
		{
			name: "slice data",
			data: []interface{}{"a", "b", "c"},
		},
		{
			name: "string data",
			data: "just-a-string",
		},
		{
			name: "numeric data",
			data: float64(42),
		},
		{
			name: "boolean data",
			data: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			part := Part{Data: tc.data, MediaType: "application/json"}
			pbPart := internalPartToProto(part)

			dataPart, ok := pbPart.Content.(*pb.Part_Data)
			if !ok {
				t.Fatalf("expected Part_Data, got %T", pbPart.Content)
			}
			if dataPart.Data == nil {
				t.Fatal("data value is nil")
			}
		})
	}
}

func TestInternalPartToProto_NilDataIgnored(t *testing.T) {
	part := Part{MediaType: "text/plain"}
	pbPart := internalPartToProto(part)
	if pbPart.Content != nil {
		t.Errorf("expected nil content for empty part, got %T", pbPart.Content)
	}
}

func TestTaskStateToProto(t *testing.T) {
	tests := []struct {
		state string
		want  pb.TaskState
	}{
		{TaskStateSubmitted, pb.TaskState_TASK_STATE_SUBMITTED},
		{TaskStateWorking, pb.TaskState_TASK_STATE_WORKING},
		{TaskStateCompleted, pb.TaskState_TASK_STATE_COMPLETED},
		{TaskStateFailed, pb.TaskState_TASK_STATE_FAILED},
		{TaskStateCanceled, pb.TaskState_TASK_STATE_CANCELED},
		{TaskStateInputRequired, pb.TaskState_TASK_STATE_INPUT_REQUIRED},
		{TaskStateRejected, pb.TaskState_TASK_STATE_REJECTED},
		{"unknown", pb.TaskState_TASK_STATE_UNSPECIFIED},
	}

	for _, tc := range tests {
		t.Run(tc.state, func(t *testing.T) {
			got := taskStateToProto(tc.state)
			if got != tc.want {
				t.Errorf("taskStateToProto(%q) = %v, want %v", tc.state, got, tc.want)
			}
		})
	}
}

func TestProtoTaskStateToInternal(t *testing.T) {
	tests := []struct {
		state pb.TaskState
		want  string
	}{
		{pb.TaskState_TASK_STATE_SUBMITTED, TaskStateSubmitted},
		{pb.TaskState_TASK_STATE_WORKING, TaskStateWorking},
		{pb.TaskState_TASK_STATE_COMPLETED, TaskStateCompleted},
		{pb.TaskState_TASK_STATE_FAILED, TaskStateFailed},
		{pb.TaskState_TASK_STATE_CANCELED, TaskStateCanceled},
		{pb.TaskState_TASK_STATE_INPUT_REQUIRED, TaskStateInputRequired},
		{pb.TaskState_TASK_STATE_REJECTED, TaskStateRejected},
		{pb.TaskState_TASK_STATE_UNSPECIFIED, ""},
	}

	for _, tc := range tests {
		t.Run(tc.state.String(), func(t *testing.T) {
			got := protoTaskStateToInternal(tc.state)
			if got != tc.want {
				t.Errorf("protoTaskStateToInternal(%v) = %q, want %q", tc.state, got, tc.want)
			}
		})
	}
}

func TestTaskResultToProto(t *testing.T) {
	msg := &Message{MessageID: "msg-1", Role: RoleAgent, Parts: []Part{{Text: "response", MediaType: "text/plain"}}}
	result := &TaskResult{
		ID:        "task-123",
		ContextID: "ctx-456",
		Status:    TaskStatus{State: TaskStateCompleted, Message: msg},
		Artifacts: []Artifact{
			{ArtifactID: "art-1", Name: "output", Parts: []Part{{Text: "data"}}},
		},
	}

	pbTask := taskResultToProto(result)

	if pbTask.Id != "task-123" {
		t.Errorf("id = %q, want %q", pbTask.Id, "task-123")
	}
	if pbTask.ContextId != "ctx-456" {
		t.Errorf("context_id = %q, want %q", pbTask.ContextId, "ctx-456")
	}
	if pbTask.Status.State != pb.TaskState_TASK_STATE_COMPLETED {
		t.Errorf("state = %v, want COMPLETED", pbTask.Status.State)
	}
	if pbTask.Status.Message == nil {
		t.Fatal("status.message is nil")
	}
	if pbTask.Status.Message.MessageId != "msg-1" {
		t.Errorf("message_id = %q, want %q", pbTask.Status.Message.MessageId, "msg-1")
	}
	if len(pbTask.Artifacts) != 1 {
		t.Fatalf("artifacts count = %d, want 1", len(pbTask.Artifacts))
	}
	if pbTask.Artifacts[0].ArtifactId != "art-1" {
		t.Errorf("artifact_id = %q, want %q", pbTask.Artifacts[0].ArtifactId, "art-1")
	}
}

func TestRoleToProto(t *testing.T) {
	if roleToProto(RoleUser) != pb.Role_ROLE_USER {
		t.Errorf("RoleUser mapping failed")
	}
	if roleToProto(RoleAgent) != pb.Role_ROLE_AGENT {
		t.Errorf("RoleAgent mapping failed")
	}
	if roleToProto("unknown") != pb.Role_ROLE_UNSPECIFIED {
		t.Errorf("unknown role mapping failed")
	}
}

func TestPushConfigToProto(t *testing.T) {
	cfg := &state.PushNotificationConfig{
		ID:              "push-1",
		TaskID:          "task-1",
		URL:             "https://example.com/webhook",
		Token:           "tok-123",
		AuthScheme:      "Bearer",
		AuthCredentials: "secret",
	}

	pbCfg := pushConfigToProto(cfg)

	if pbCfg.Id != "push-1" {
		t.Errorf("id = %q, want %q", pbCfg.Id, "push-1")
	}
	if pbCfg.TaskId != "task-1" {
		t.Errorf("task_id = %q, want %q", pbCfg.TaskId, "task-1")
	}
	if pbCfg.Url != "https://example.com/webhook" {
		t.Errorf("url = %q, want %q", pbCfg.Url, "https://example.com/webhook")
	}
	if pbCfg.Token != "tok-123" {
		t.Errorf("token = %q, want %q", pbCfg.Token, "tok-123")
	}
	if pbCfg.Authentication == nil {
		t.Fatal("authentication is nil")
	}
	if pbCfg.Authentication.Scheme != "Bearer" {
		t.Errorf("auth scheme = %q, want %q", pbCfg.Authentication.Scheme, "Bearer")
	}
	if pbCfg.Authentication.Credentials != "secret" {
		t.Errorf("auth credentials = %q, want %q", pbCfg.Authentication.Credentials, "secret")
	}
}

func TestPushConfigToProtoNoAuth(t *testing.T) {
	cfg := &state.PushNotificationConfig{
		ID:     "push-2",
		TaskID: "task-2",
		URL:    "https://example.com/hook",
	}

	pbCfg := pushConfigToProto(cfg)
	if pbCfg.Authentication != nil {
		t.Errorf("expected nil authentication, got %v", pbCfg.Authentication)
	}
}

func TestStreamEventToProto(t *testing.T) {
	t.Run("task event", func(t *testing.T) {
		event := StreamEvent{
			Task: &TaskResult{
				ID:        "t-1",
				ContextID: "c-1",
				Status:    TaskStatus{State: TaskStateWorking},
			},
		}
		resp := streamEventToProto("t-1", event)
		if resp == nil {
			t.Fatal("nil response")
		}
		taskResp, ok := resp.Payload.(*pb.StreamResponse_Task)
		if !ok {
			t.Fatalf("expected task payload, got %T", resp.Payload)
		}
		if taskResp.Task.Id != "t-1" {
			t.Errorf("task id = %q, want %q", taskResp.Task.Id, "t-1")
		}
	})

	t.Run("status update event", func(t *testing.T) {
		event := StreamEvent{
			StatusUpdate: &TaskStatusUpdate{
				TaskID: "t-2",
				Status: TaskStatus{State: TaskStateCompleted},
				Final:  true,
			},
		}
		resp := streamEventToProto("t-2", event)
		if resp == nil {
			t.Fatal("nil response")
		}
		statusResp, ok := resp.Payload.(*pb.StreamResponse_StatusUpdate)
		if !ok {
			t.Fatalf("expected status_update payload, got %T", resp.Payload)
		}
		if statusResp.StatusUpdate.TaskId != "t-2" {
			t.Errorf("task id = %q, want %q", statusResp.StatusUpdate.TaskId, "t-2")
		}
		if statusResp.StatusUpdate.Status.State != pb.TaskState_TASK_STATE_COMPLETED {
			t.Errorf("state = %v, want COMPLETED", statusResp.StatusUpdate.Status.State)
		}
	})

	t.Run("status update with message", func(t *testing.T) {
		event := StreamEvent{
			StatusUpdate: &TaskStatusUpdate{
				TaskID: "t-2b",
				Status: TaskStatus{
					State: TaskStateFailed,
					Message: &Message{
						MessageID: "err-1",
						Role:      RoleAgent,
						Parts:     []Part{{Text: "something went wrong"}},
					},
				},
				Final: true,
			},
		}
		resp := streamEventToProto("t-2b", event)
		if resp == nil {
			t.Fatal("nil response")
		}
		statusResp, ok := resp.Payload.(*pb.StreamResponse_StatusUpdate)
		if !ok {
			t.Fatalf("expected status_update payload, got %T", resp.Payload)
		}
		if statusResp.StatusUpdate.Status.Message == nil {
			t.Fatal("status message should not be nil when set on the event")
		}
		if statusResp.StatusUpdate.Status.Message.MessageId != "err-1" {
			t.Errorf("message_id = %q, want %q", statusResp.StatusUpdate.Status.Message.MessageId, "err-1")
		}
		if len(statusResp.StatusUpdate.Status.Message.Parts) != 1 {
			t.Fatalf("parts count = %d, want 1", len(statusResp.StatusUpdate.Status.Message.Parts))
		}
	})

	t.Run("artifact update event", func(t *testing.T) {
		event := StreamEvent{
			ArtifactUpdate: &TaskArtifactUpdate{
				TaskID: "t-3",
				Artifact: Artifact{
					ArtifactID: "art-1",
					Parts:      []Part{{Text: "result"}},
				},
			},
		}
		resp := streamEventToProto("t-3", event)
		if resp == nil {
			t.Fatal("nil response")
		}
		artResp, ok := resp.Payload.(*pb.StreamResponse_ArtifactUpdate)
		if !ok {
			t.Fatalf("expected artifact_update payload, got %T", resp.Payload)
		}
		if artResp.ArtifactUpdate.TaskId != "t-3" {
			t.Errorf("task id = %q, want %q", artResp.ArtifactUpdate.TaskId, "t-3")
		}
	})

	t.Run("artifact update with last_chunk", func(t *testing.T) {
		event := StreamEvent{
			ArtifactUpdate: &TaskArtifactUpdate{
				TaskID: "t-3b",
				Artifact: Artifact{
					ArtifactID: "art-final",
					Parts:      []Part{{Text: "done"}},
					LastChunk:  true,
				},
			},
		}
		resp := streamEventToProto("t-3b", event)
		if resp == nil {
			t.Fatal("nil response")
		}
		artResp, ok := resp.Payload.(*pb.StreamResponse_ArtifactUpdate)
		if !ok {
			t.Fatalf("expected artifact_update payload, got %T", resp.Payload)
		}
		if !artResp.ArtifactUpdate.LastChunk {
			t.Error("expected LastChunk=true, got false")
		}
	})

	t.Run("empty event", func(t *testing.T) {
		resp := streamEventToProto("t-4", StreamEvent{})
		if resp != nil {
			t.Errorf("expected nil for empty event, got %v", resp)
		}
	})
}

func TestBridgeErrToGRPC(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode codes.Code
	}{
		{"agent not found", ErrAgentNotFound, codes.NotFound},
		{"context unknown", ErrContextUnknown, codes.InvalidArgument},
		{"generic error", io.ErrUnexpectedEOF, codes.Internal},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			grpcErr := bridgeErrToGRPC(tc.err)
			st, ok := grpcstatus.FromError(grpcErr)
			if !ok {
				t.Fatal("expected gRPC status error")
			}
			if st.Code() != tc.wantCode {
				t.Errorf("code = %v, want %v", st.Code(), tc.wantCode)
			}
		})
	}
}

func TestResolveProject(t *testing.T) {
	cfg := &Config{
		Projects: []ProjectConfig{
			{Slug: "default-project"},
		},
	}
	s := &GRPCServer{config: cfg}

	tests := []struct {
		name        string
		tenant      string
		wantProject string
		wantAgent   string
		wantErr     bool
	}{
		{
			name:        "full resource name",
			tenant:      "projects/my-project/agents/my-agent",
			wantProject: "my-project",
			wantAgent:   "my-agent",
		},
		{
			name:        "simple two-part format",
			tenant:      "my-project/my-agent",
			wantProject: "my-project",
			wantAgent:   "my-agent",
		},
		{
			name:        "full resource name with hyphens and numbers",
			tenant:      "projects/proj-123/agents/agent-456",
			wantProject: "proj-123",
			wantAgent:   "agent-456",
		},
		{
			name:    "empty tenant falls back with error",
			tenant:  "",
			wantErr: true,
		},
		{
			name:    "single segment is missing agent",
			tenant:  "just-project",
			wantErr: true,
		},
		{
			name:    "three segments is invalid",
			tenant:  "a/b/c",
			wantErr: true,
		},
		{
			name:    "empty project in full format",
			tenant:  "projects//agents/my-agent",
			wantErr: true,
		},
		{
			name:    "empty agent in full format",
			tenant:  "projects/my-project/agents/",
			wantErr: true,
		},
		{
			name:    "empty parts in simple format",
			tenant:  "/my-agent",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			project, agent, err := s.resolveProject(tc.tenant)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error, got project=%q agent=%q", project, agent)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if project != tc.wantProject {
				t.Errorf("project = %q, want %q", project, tc.wantProject)
			}
			if agent != tc.wantAgent {
				t.Errorf("agent = %q, want %q", agent, tc.wantAgent)
			}
		})
	}
}

// --- gRPC server integration tests ---

// newTestGRPCServer sets up a gRPC server with a real Bridge backed by an in-memory
// SQLite database (no hub client), returning a connected client and cleanup function.
func newTestGRPCServer(t *testing.T) (pb.A2AServiceClient, *state.Store) {
	t.Helper()

	dir := t.TempDir()
	store, err := state.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	cfg := &Config{
		Bridge: BridgeConfig{
			ExternalURL: "https://a2a.test.example.com",
			Provider: ProviderConfig{
				Organization: "Test Org",
				URL:          "https://test.example.com",
			},
		},
		Projects: []ProjectConfig{
			{
				Slug:          "test-project",
				ExposedAgents: []string{"test-agent"},
			},
		},
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	b := New(store, nil, nil, cfg, nil, log)
	t.Cleanup(b.Shutdown)

	grpcSrv := grpc.NewServer()
	adapter := NewGRPCServer(b, cfg, log)
	adapter.Register(grpcSrv)

	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	go func() {
		if err := grpcSrv.Serve(lis); err != nil {
			// Server stopped, expected during cleanup.
		}
	}()
	t.Cleanup(grpcSrv.GracefulStop)

	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	return pb.NewA2AServiceClient(conn), store
}

func TestGRPCGetTask_NotFound(t *testing.T) {
	client, _ := newTestGRPCServer(t)

	_, err := client.GetTask(context.Background(), &pb.GetTaskRequest{Id: "nonexistent-task"})
	if err == nil {
		t.Fatal("expected error for nonexistent task")
	}
	st, ok := grpcstatus.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status, got %v", err)
	}
	if st.Code() != codes.NotFound {
		t.Errorf("code = %v, want NotFound", st.Code())
	}
}

func TestGRPCGetTask_MissingID(t *testing.T) {
	client, _ := newTestGRPCServer(t)

	_, err := client.GetTask(context.Background(), &pb.GetTaskRequest{})
	if err == nil {
		t.Fatal("expected error for missing ID")
	}
	st, _ := grpcstatus.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", st.Code())
	}
}

func TestGRPCGetTask_Exists(t *testing.T) {
	client, store := newTestGRPCServer(t)

	// Create a task directly in the store.
	task := &state.Task{
		ID:        "task-grpc-1",
		ContextID: "ctx-1",
		ProjectID: "test-project",
		AgentSlug: "test-agent",
		State:     TaskStateWorking,
		Metadata:  "{}",
	}
	if err := store.CreateTask(task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	resp, err := client.GetTask(context.Background(), &pb.GetTaskRequest{Id: "task-grpc-1"})
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if resp.Id != "task-grpc-1" {
		t.Errorf("id = %q, want %q", resp.Id, "task-grpc-1")
	}
	if resp.Status.State != pb.TaskState_TASK_STATE_WORKING {
		t.Errorf("state = %v, want WORKING", resp.Status.State)
	}
}

func TestGRPCListTasks_Empty(t *testing.T) {
	client, _ := newTestGRPCServer(t)

	resp, err := client.ListTasks(context.Background(), &pb.ListTasksRequest{ContextId: "no-such-context"})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(resp.Tasks) != 0 {
		t.Errorf("expected 0 tasks, got %d", len(resp.Tasks))
	}
}

func TestGRPCListTasks_MissingContextID(t *testing.T) {
	client, _ := newTestGRPCServer(t)

	_, err := client.ListTasks(context.Background(), &pb.ListTasksRequest{})
	if err == nil {
		t.Fatal("expected error for missing context_id")
	}
	st, _ := grpcstatus.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", st.Code())
	}
}

func TestGRPCCancelTask_NotFound(t *testing.T) {
	client, _ := newTestGRPCServer(t)

	_, err := client.CancelTask(context.Background(), &pb.CancelTaskRequest{Id: "nonexistent"})
	if err == nil {
		t.Fatal("expected error for nonexistent task")
	}
	st, _ := grpcstatus.FromError(err)
	if st.Code() != codes.NotFound {
		t.Errorf("code = %v, want NotFound", st.Code())
	}
}

func TestGRPCSendMessage_MissingParts(t *testing.T) {
	client, _ := newTestGRPCServer(t)

	_, err := client.SendMessage(context.Background(), &pb.SendMessageRequest{
		Tenant:  "test-project/test-agent",
		Message: &pb.Message{},
	})
	if err == nil {
		t.Fatal("expected error for empty parts")
	}
	st, _ := grpcstatus.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", st.Code())
	}
}

func TestGRPCSendMessage_MissingTenant(t *testing.T) {
	client, _ := newTestGRPCServer(t)

	_, err := client.SendMessage(context.Background(), &pb.SendMessageRequest{
		Message: &pb.Message{
			MessageId: "m1",
			Role:      pb.Role_ROLE_USER,
			Parts:     []*pb.Part{{Content: &pb.Part_Text{Text: "hello"}}},
		},
	})
	if err == nil {
		t.Fatal("expected error for missing tenant")
	}
}

func TestGRPCDeletePushNotification_MissingTaskID(t *testing.T) {
	client, _ := newTestGRPCServer(t)

	_, err := client.DeleteTaskPushNotificationConfig(context.Background(), &pb.DeleteTaskPushNotificationConfigRequest{})
	if err == nil {
		t.Fatal("expected error for missing task_id")
	}
	st, _ := grpcstatus.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", st.Code())
	}
}

func TestGRPCDeletePushNotification_MissingID(t *testing.T) {
	client, _ := newTestGRPCServer(t)

	_, err := client.DeleteTaskPushNotificationConfig(context.Background(), &pb.DeleteTaskPushNotificationConfigRequest{
		TaskId: "some-task",
	})
	if err == nil {
		t.Fatal("expected error for missing id")
	}
	st, _ := grpcstatus.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", st.Code())
	}
}

func TestGRPCGetPushNotification_MissingID(t *testing.T) {
	client, _ := newTestGRPCServer(t)

	_, err := client.GetTaskPushNotificationConfig(context.Background(), &pb.GetTaskPushNotificationConfigRequest{
		TaskId: "some-task",
	})
	if err == nil {
		t.Fatal("expected error for missing id")
	}
	st, _ := grpcstatus.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", st.Code())
	}
}

func TestGRPCGetPushNotification_MissingTaskID(t *testing.T) {
	client, _ := newTestGRPCServer(t)

	_, err := client.GetTaskPushNotificationConfig(context.Background(), &pb.GetTaskPushNotificationConfigRequest{})
	if err == nil {
		t.Fatal("expected error for missing task_id")
	}
	st, _ := grpcstatus.FromError(err)
	if st.Code() != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", st.Code())
	}
}

func TestGRPCListPushNotificationConfigs(t *testing.T) {
	client, _ := newTestGRPCServer(t)

	resp, err := client.ListTaskPushNotificationConfigs(context.Background(), &pb.ListTaskPushNotificationConfigsRequest{
		TaskId: "some-task",
	})
	if err != nil {
		t.Fatalf("ListTaskPushNotificationConfigs: %v", err)
	}
	if len(resp.Configs) != 0 {
		t.Errorf("expected 0 configs, got %d", len(resp.Configs))
	}
}

func TestGRPCAgentCardMapToProto(t *testing.T) {
	card := map[string]interface{}{
		"name":        "Test Agent",
		"description": "A test agent",
		"url":         "https://example.com/agent",
		"version":     "1.0.0",
		"skills": []map[string]interface{}{
			{"id": "skill-1", "name": "Skill One", "description": "Does stuff"},
		},
		"provider": map[string]string{
			"organization": "Acme Corp",
			"url":          "https://acme.example.com",
		},
		"defaultInputModes":  []string{"text/plain"},
		"defaultOutputModes": []string{"text/plain", "application/json"},
	}

	pbCard := agentCardMapToProto(card)

	if pbCard.Name != "Test Agent" {
		t.Errorf("name = %q, want %q", pbCard.Name, "Test Agent")
	}
	if pbCard.Description != "A test agent" {
		t.Errorf("description = %q, want %q", pbCard.Description, "A test agent")
	}
	if pbCard.Version != "1.0.0" {
		t.Errorf("version = %q, want %q", pbCard.Version, "1.0.0")
	}
	if len(pbCard.SupportedInterfaces) != 1 {
		t.Fatalf("interfaces count = %d, want 1", len(pbCard.SupportedInterfaces))
	}
	if pbCard.SupportedInterfaces[0].Url != "https://example.com/agent" {
		t.Errorf("interface url = %q", pbCard.SupportedInterfaces[0].Url)
	}
	if len(pbCard.Skills) != 1 {
		t.Fatalf("skills count = %d, want 1", len(pbCard.Skills))
	}
	if pbCard.Skills[0].Id != "skill-1" {
		t.Errorf("skill id = %q, want %q", pbCard.Skills[0].Id, "skill-1")
	}
	if pbCard.Provider == nil {
		t.Fatal("provider is nil")
	}
	if pbCard.Provider.Organization != "Acme Corp" {
		t.Errorf("provider org = %q, want %q", pbCard.Provider.Organization, "Acme Corp")
	}
}
