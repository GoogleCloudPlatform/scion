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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"

	pb "github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/a2apb"
	"github.com/GoogleCloudPlatform/scion/extras/scion-a2a-bridge/internal/state"
)

// GRPCServer adapts the Bridge to the generated A2AServiceServer interface.
// Each method translates protobuf types to the bridge's internal types,
// delegates to the existing Bridge methods, and translates back.
type GRPCServer struct {
	pb.UnimplementedA2AServiceServer

	bridge *Bridge
	config *Config
	log    *slog.Logger
}

// NewGRPCServer creates a new gRPC server adapter wrapping the given Bridge.
func NewGRPCServer(bridge *Bridge, config *Config, log *slog.Logger) *GRPCServer {
	return &GRPCServer{
		bridge: bridge,
		config: config,
		log:    log,
	}
}

// Register registers the A2A gRPC service on the given gRPC server.
func (s *GRPCServer) Register(srv *grpc.Server) {
	pb.RegisterA2AServiceServer(srv, s)
}

// resolveProject extracts the project and agent slugs from the tenant field.
// Supported formats:
//   - "projects/{projectSlug}/agents/{agentSlug}"
//   - "{projectSlug}/{agentSlug}"
//
// If no tenant is provided we fall back to the first configured project.
func (s *GRPCServer) resolveProject(tenant string) (projectSlug, agentSlug string, err error) {
	if tenant == "" {
		// Use first configured project if available, but no agent slug.
		if len(s.config.Projects) > 0 {
			return s.config.Projects[0].Slug, "", fmt.Errorf("tenant is required to identify the agent")
		}
		return "", "", fmt.Errorf("tenant is required")
	}

	parts := strings.Split(tenant, "/")

	// "projects/{project}/agents/{agent}" → 4 segments
	if len(parts) == 4 && parts[0] == "projects" && parts[2] == "agents" {
		if parts[1] == "" || parts[3] == "" {
			return "", "", fmt.Errorf("invalid tenant format: %s", tenant)
		}
		return parts[1], parts[3], nil
	}

	// "{project}/{agent}" → 2 segments
	if len(parts) == 2 {
		if parts[0] == "" || parts[1] == "" {
			return "", "", fmt.Errorf("invalid tenant format: %s", tenant)
		}
		return parts[0], parts[1], nil
	}

	// Single segment → project only, missing agent slug.
	if len(parts) == 1 {
		return parts[0], "", fmt.Errorf("tenant missing agent slug: %s", tenant)
	}

	return "", "", fmt.Errorf("invalid tenant format: %s", tenant)
}

// SendMessage handles the SendMessage RPC.
func (s *GRPCServer) SendMessage(ctx context.Context, req *pb.SendMessageRequest) (*pb.SendMessageResponse, error) {
	projectSlug, agentSlug, err := s.resolveProject(req.GetTenant())
	if err != nil {
		return nil, grpcstatus.Errorf(codes.InvalidArgument, "invalid tenant: %v", err)
	}

	if err := s.bridge.AuthorizeExposed(projectSlug, agentSlug); err != nil {
		return nil, grpcstatus.Errorf(codes.NotFound, "agent not found")
	}

	if req.GetMessage() == nil || len(req.GetMessage().GetParts()) == 0 {
		return nil, grpcstatus.Errorf(codes.InvalidArgument, "message with parts is required")
	}

	parts := pbPartsToInternal(req.GetMessage().GetParts())
	contextID := req.GetMessage().GetContextId()

	blocking := true
	if req.GetConfiguration() != nil && req.GetConfiguration().GetReturnImmediately() {
		blocking = false
	}

	result, err := s.bridge.SendMessage(ctx, projectSlug, agentSlug, contextID, parts, blocking)
	if err != nil {
		return nil, bridgeErrToGRPC(err)
	}

	return &pb.SendMessageResponse{
		Payload: &pb.SendMessageResponse_Task{
			Task: taskResultToProto(result),
		},
	}, nil
}

// SendStreamingMessage handles the SendStreamingMessage server-streaming RPC.
func (s *GRPCServer) SendStreamingMessage(req *pb.SendMessageRequest, stream grpc.ServerStreamingServer[pb.StreamResponse]) error {
	ctx := stream.Context()

	projectSlug, agentSlug, err := s.resolveProject(req.GetTenant())
	if err != nil {
		return grpcstatus.Errorf(codes.InvalidArgument, "invalid tenant: %v", err)
	}

	if err := s.bridge.AuthorizeExposed(projectSlug, agentSlug); err != nil {
		return grpcstatus.Errorf(codes.NotFound, "agent not found")
	}

	if req.GetMessage() == nil || len(req.GetMessage().GetParts()) == 0 {
		return grpcstatus.Errorf(codes.InvalidArgument, "message with parts is required")
	}

	parts := pbPartsToInternal(req.GetMessage().GetParts())
	contextID := req.GetMessage().GetContextId()

	taskID, events, cleanup, err := s.bridge.SendStreamingMessage(ctx, projectSlug, agentSlug, contextID, parts)
	if err != nil {
		return bridgeErrToGRPC(err)
	}
	defer cleanup()

	return s.streamEvents(ctx, stream, taskID, events)
}

// GetTask handles the GetTask RPC.
func (s *GRPCServer) GetTask(ctx context.Context, req *pb.GetTaskRequest) (*pb.Task, error) {
	if req.GetId() == "" {
		return nil, grpcstatus.Errorf(codes.InvalidArgument, "id is required")
	}

	result, err := s.bridge.GetTask(ctx, req.GetId())
	if err != nil {
		return nil, grpcstatus.Errorf(codes.Internal, "get task: %v", err)
	}
	if result == nil {
		return nil, grpcstatus.Errorf(codes.NotFound, "task not found")
	}

	return taskResultToProto(result), nil
}

// ListTasks handles the ListTasks RPC.
func (s *GRPCServer) ListTasks(ctx context.Context, req *pb.ListTasksRequest) (*pb.ListTasksResponse, error) {
	if req.GetContextId() == "" {
		return nil, grpcstatus.Errorf(codes.InvalidArgument, "context_id is required")
	}

	results, err := s.bridge.ListTasks(ctx, req.GetContextId())
	if err != nil {
		return nil, grpcstatus.Errorf(codes.Internal, "list tasks: %v", err)
	}

	tasks := make([]*pb.Task, len(results))
	for i, r := range results {
		tasks[i] = taskResultToProto(&r)
	}

	return &pb.ListTasksResponse{
		Tasks:    tasks,
		PageSize: int32(len(tasks)),
	}, nil
}

// CancelTask handles the CancelTask RPC.
func (s *GRPCServer) CancelTask(ctx context.Context, req *pb.CancelTaskRequest) (*pb.Task, error) {
	if req.GetId() == "" {
		return nil, grpcstatus.Errorf(codes.InvalidArgument, "id is required")
	}

	result, err := s.bridge.CancelTask(ctx, req.GetId())
	if err != nil {
		return nil, grpcstatus.Errorf(codes.Internal, "cancel task: %v", err)
	}
	if result == nil {
		return nil, grpcstatus.Errorf(codes.NotFound, "task not found")
	}

	return taskResultToProto(result), nil
}

// SubscribeToTask handles the SubscribeToTask server-streaming RPC.
func (s *GRPCServer) SubscribeToTask(req *pb.SubscribeToTaskRequest, stream grpc.ServerStreamingServer[pb.StreamResponse]) error {
	ctx := stream.Context()

	if req.GetId() == "" {
		return grpcstatus.Errorf(codes.InvalidArgument, "id is required")
	}

	events, cleanup, err := s.bridge.SubscribeToTask(ctx, req.GetId())
	if err != nil {
		return grpcstatus.Errorf(codes.Internal, "subscribe to task: %v", err)
	}
	defer cleanup()

	return s.streamEvents(ctx, stream, req.GetId(), events)
}

// CreateTaskPushNotificationConfig handles the CreateTaskPushNotificationConfig RPC.
func (s *GRPCServer) CreateTaskPushNotificationConfig(ctx context.Context, req *pb.TaskPushNotificationConfig) (*pb.TaskPushNotificationConfig, error) {
	if req.GetTaskId() == "" {
		return nil, grpcstatus.Errorf(codes.InvalidArgument, "task_id is required")
	}
	if req.GetUrl() == "" {
		return nil, grpcstatus.Errorf(codes.InvalidArgument, "url is required")
	}

	var authScheme, authCredentials string
	if req.GetAuthentication() != nil {
		authScheme = req.GetAuthentication().GetScheme()
		authCredentials = req.GetAuthentication().GetCredentials()
	}

	cfg, err := s.bridge.SetPushNotificationConfig(ctx, req.GetTaskId(), req.GetUrl(), req.GetToken(), authScheme, authCredentials)
	if err != nil {
		return nil, grpcstatus.Errorf(codes.Internal, "set push config: %v", err)
	}

	return pushConfigToProto(cfg), nil
}

// GetTaskPushNotificationConfig handles the GetTaskPushNotificationConfig RPC.
func (s *GRPCServer) GetTaskPushNotificationConfig(ctx context.Context, req *pb.GetTaskPushNotificationConfigRequest) (*pb.TaskPushNotificationConfig, error) {
	if req.GetTaskId() == "" {
		return nil, grpcstatus.Errorf(codes.InvalidArgument, "task_id is required")
	}
	if req.GetId() == "" {
		return nil, grpcstatus.Errorf(codes.InvalidArgument, "id is required")
	}

	configs, err := s.bridge.GetPushNotificationConfig(ctx, req.GetTaskId())
	if err != nil {
		return nil, grpcstatus.Errorf(codes.Internal, "get push config: %v", err)
	}

	// Return the specific config by ID if provided.
	for _, c := range configs {
		if c.ID == req.GetId() {
			return pushConfigToProto(&c), nil
		}
	}

	return nil, grpcstatus.Errorf(codes.NotFound, "push notification config not found")
}

// ListTaskPushNotificationConfigs handles the ListTaskPushNotificationConfigs RPC.
func (s *GRPCServer) ListTaskPushNotificationConfigs(ctx context.Context, req *pb.ListTaskPushNotificationConfigsRequest) (*pb.ListTaskPushNotificationConfigsResponse, error) {
	if req.GetTaskId() == "" {
		return nil, grpcstatus.Errorf(codes.InvalidArgument, "task_id is required")
	}

	configs, err := s.bridge.GetPushNotificationConfig(ctx, req.GetTaskId())
	if err != nil {
		return nil, grpcstatus.Errorf(codes.Internal, "list push configs: %v", err)
	}

	pbConfigs := make([]*pb.TaskPushNotificationConfig, len(configs))
	for i, c := range configs {
		pbConfigs[i] = pushConfigToProto(&c)
	}

	return &pb.ListTaskPushNotificationConfigsResponse{
		Configs: pbConfigs,
	}, nil
}

// GetExtendedAgentCard handles the GetExtendedAgentCard RPC.
func (s *GRPCServer) GetExtendedAgentCard(ctx context.Context, req *pb.GetExtendedAgentCardRequest) (*pb.AgentCard, error) {
	projectSlug, agentSlug, err := s.resolveProject(req.GetTenant())
	if err != nil {
		return nil, grpcstatus.Errorf(codes.InvalidArgument, "invalid tenant: %v", err)
	}

	card := s.bridge.GenerateAgentCard(ctx, projectSlug, agentSlug)
	return agentCardMapToProto(card), nil
}

// DeleteTaskPushNotificationConfig handles the DeleteTaskPushNotificationConfig RPC.
func (s *GRPCServer) DeleteTaskPushNotificationConfig(ctx context.Context, req *pb.DeleteTaskPushNotificationConfigRequest) (*emptypb.Empty, error) {
	if req.GetTaskId() == "" {
		return nil, grpcstatus.Errorf(codes.InvalidArgument, "task_id is required")
	}
	if req.GetId() == "" {
		return nil, grpcstatus.Errorf(codes.InvalidArgument, "id is required")
	}

	if err := s.bridge.DeletePushNotificationConfig(ctx, req.GetTaskId(), req.GetId()); err != nil {
		return nil, grpcstatus.Errorf(codes.Internal, "delete push config: %v", err)
	}

	return &emptypb.Empty{}, nil
}

// streamEvents reads from the bridge's event channel and sends protobuf StreamResponse
// messages to the gRPC stream until the channel closes or the context is cancelled.
func (s *GRPCServer) streamEvents(ctx context.Context, stream grpc.ServerStreamingServer[pb.StreamResponse], taskID string, events <-chan StreamEvent) error {
	for {
		select {
		case event, ok := <-events:
			if !ok {
				return nil
			}

			resp := streamEventToProto(taskID, event)
			if resp == nil {
				continue
			}
			if err := stream.Send(resp); err != nil {
				return err
			}

			if event.StatusUpdate != nil && event.StatusUpdate.Final {
				return nil
			}

		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// --- Type translation functions ---

// pbPartsToInternal converts protobuf Part messages to internal Part types.
func pbPartsToInternal(pbParts []*pb.Part) []Part {
	parts := make([]Part, 0, len(pbParts))
	for _, p := range pbParts {
		part := Part{
			MediaType: p.GetMediaType(),
		}
		switch c := p.GetContent().(type) {
		case *pb.Part_Text:
			part.Text = c.Text
		case *pb.Part_Url:
			part.URL = c.Url
		case *pb.Part_Data:
			if c.Data != nil {
				part.Data = c.Data.AsInterface()
			}
		}
		parts = append(parts, part)
	}
	return parts
}

// internalPartToProto converts an internal Part to a protobuf Part.
func internalPartToProto(p Part) *pb.Part {
	pbPart := &pb.Part{
		MediaType: p.MediaType,
	}
	switch {
	case p.Text != "":
		pbPart.Content = &pb.Part_Text{Text: p.Text}
	case p.URL != "":
		pbPart.Content = &pb.Part_Url{Url: p.URL}
	case p.Data != nil:
		// Convert structured data to a protobuf Value.
		// First try direct conversion; fall back to JSON round-trip for
		// complex types (e.g. maps with non-string keys).
		val, err := structpb.NewValue(p.Data)
		if err != nil {
			// structpb.NewValue doesn't handle all Go types directly.
			// Round-trip through JSON to normalize to JSON-compatible types.
			b, jerr := json.Marshal(p.Data)
			if jerr != nil {
				// Last resort: emit as text so data is not silently lost.
				pbPart.Content = &pb.Part_Text{Text: fmt.Sprintf("%v", p.Data)}
				break
			}
			var raw interface{}
			if err2 := json.Unmarshal(b, &raw); err2 != nil {
				pbPart.Content = &pb.Part_Text{Text: string(b)}
				break
			}
			val, err = structpb.NewValue(raw)
			if err != nil {
				pbPart.Content = &pb.Part_Text{Text: string(b)}
				break
			}
		}
		pbPart.Content = &pb.Part_Data{Data: val}
	}
	return pbPart
}

// taskStateToProto maps an internal task state string to a protobuf TaskState enum.
func taskStateToProto(state string) pb.TaskState {
	switch state {
	case TaskStateSubmitted:
		return pb.TaskState_TASK_STATE_SUBMITTED
	case TaskStateWorking:
		return pb.TaskState_TASK_STATE_WORKING
	case TaskStateCompleted:
		return pb.TaskState_TASK_STATE_COMPLETED
	case TaskStateFailed:
		return pb.TaskState_TASK_STATE_FAILED
	case TaskStateCanceled:
		return pb.TaskState_TASK_STATE_CANCELED
	case TaskStateInputRequired:
		return pb.TaskState_TASK_STATE_INPUT_REQUIRED
	case TaskStateRejected:
		return pb.TaskState_TASK_STATE_REJECTED
	default:
		return pb.TaskState_TASK_STATE_UNSPECIFIED
	}
}

// protoTaskStateToInternal maps a protobuf TaskState enum to an internal state string.
func protoTaskStateToInternal(state pb.TaskState) string {
	switch state {
	case pb.TaskState_TASK_STATE_SUBMITTED:
		return TaskStateSubmitted
	case pb.TaskState_TASK_STATE_WORKING:
		return TaskStateWorking
	case pb.TaskState_TASK_STATE_COMPLETED:
		return TaskStateCompleted
	case pb.TaskState_TASK_STATE_FAILED:
		return TaskStateFailed
	case pb.TaskState_TASK_STATE_CANCELED:
		return TaskStateCanceled
	case pb.TaskState_TASK_STATE_INPUT_REQUIRED:
		return TaskStateInputRequired
	case pb.TaskState_TASK_STATE_REJECTED:
		return TaskStateRejected
	default:
		return ""
	}
}

// taskResultToProto converts an internal TaskResult to a protobuf Task.
func taskResultToProto(r *TaskResult) *pb.Task {
	task := &pb.Task{
		Id:        r.ID,
		ContextId: r.ContextID,
		Status: &pb.TaskStatus{
			State: taskStateToProto(r.Status.State),
		},
	}

	if r.Status.Message != nil {
		task.Status.Message = messageToProto(r.Status.Message)
	}

	for _, a := range r.Artifacts {
		task.Artifacts = append(task.Artifacts, artifactToProto(&a))
	}

	return task
}

// messageToProto converts an internal Message to a protobuf Message.
func messageToProto(m *Message) *pb.Message {
	pbMsg := &pb.Message{
		MessageId: m.MessageID,
		Role:      roleToProto(m.Role),
	}
	for _, p := range m.Parts {
		pbMsg.Parts = append(pbMsg.Parts, internalPartToProto(p))
	}
	return pbMsg
}

// roleToProto maps an internal role string to a protobuf Role enum.
func roleToProto(role string) pb.Role {
	switch role {
	case RoleUser:
		return pb.Role_ROLE_USER
	case RoleAgent:
		return pb.Role_ROLE_AGENT
	default:
		return pb.Role_ROLE_UNSPECIFIED
	}
}

// artifactToProto converts an internal Artifact to a protobuf Artifact.
func artifactToProto(a *Artifact) *pb.Artifact {
	pbArt := &pb.Artifact{
		ArtifactId: a.ArtifactID,
		Name:       a.Name,
	}
	for _, p := range a.Parts {
		pbArt.Parts = append(pbArt.Parts, internalPartToProto(p))
	}
	return pbArt
}

// pushConfigToProto converts a state.PushNotificationConfig to protobuf.
func pushConfigToProto(c *state.PushNotificationConfig) *pb.TaskPushNotificationConfig {
	cfg := &pb.TaskPushNotificationConfig{
		Id:     c.ID,
		TaskId: c.TaskID,
		Url:    c.URL,
		Token:  c.Token,
	}
	if c.AuthScheme != "" {
		cfg.Authentication = &pb.AuthenticationInfo{
			Scheme:      c.AuthScheme,
			Credentials: c.AuthCredentials,
		}
	}
	return cfg
}

// agentCardMapToProto converts the bridge's map-based agent card to a protobuf AgentCard.
// This is a best-effort conversion since the bridge returns a generic map.
func agentCardMapToProto(card map[string]interface{}) *pb.AgentCard {
	pbCard := &pb.AgentCard{
		Name:        getStringField(card, "name"),
		Description: getStringField(card, "description"),
		Version:     getStringField(card, "version"),
	}

	if url, ok := card["url"].(string); ok {
		pbCard.SupportedInterfaces = []*pb.AgentInterface{
			{
				Url:             url,
				ProtocolBinding: "JSONRPC",
				ProtocolVersion: "0.3",
			},
		}
	}

	if inputModes, ok := card["defaultInputModes"].([]string); ok {
		pbCard.DefaultInputModes = inputModes
	}
	if outputModes, ok := card["defaultOutputModes"].([]string); ok {
		pbCard.DefaultOutputModes = outputModes
	}

	if skills, ok := card["skills"].([]map[string]interface{}); ok {
		for _, s := range skills {
			pbCard.Skills = append(pbCard.Skills, &pb.AgentSkill{
				Id:          getStringField(s, "id"),
				Name:        getStringField(s, "name"),
				Description: getStringField(s, "description"),
			})
		}
	}

	if provider, ok := card["provider"].(map[string]string); ok {
		pbCard.Provider = &pb.AgentProvider{
			Organization: provider["organization"],
			Url:          provider["url"],
		}
	}

	return pbCard
}

// streamEventToProto converts an internal StreamEvent to a protobuf StreamResponse.
func streamEventToProto(taskID string, event StreamEvent) *pb.StreamResponse {
	switch {
	case event.Task != nil:
		return &pb.StreamResponse{
			Payload: &pb.StreamResponse_Task{
				Task: taskResultToProto(event.Task),
			},
		}

	case event.StatusUpdate != nil:
		pbStatus := &pb.TaskStatus{
			State: taskStateToProto(event.StatusUpdate.Status.State),
		}
		if event.StatusUpdate.Status.Message != nil {
			pbStatus.Message = messageToProto(event.StatusUpdate.Status.Message)
		}
		return &pb.StreamResponse{
			Payload: &pb.StreamResponse_StatusUpdate{
				StatusUpdate: &pb.TaskStatusUpdateEvent{
					TaskId: event.StatusUpdate.TaskID,
					Status: pbStatus,
				},
			},
		}

	case event.ArtifactUpdate != nil:
		return &pb.StreamResponse{
			Payload: &pb.StreamResponse_ArtifactUpdate{
				ArtifactUpdate: &pb.TaskArtifactUpdateEvent{
					TaskId:    event.ArtifactUpdate.TaskID,
					Artifact:  artifactToProto(&event.ArtifactUpdate.Artifact),
					LastChunk: event.ArtifactUpdate.Artifact.LastChunk,
				},
			},
		}
	}

	return nil
}

// bridgeErrToGRPC translates bridge errors to appropriate gRPC status codes.
func bridgeErrToGRPC(err error) error {
	switch {
	case errors.Is(err, ErrAgentNotFound):
		return grpcstatus.Errorf(codes.NotFound, "agent not found")
	case errors.Is(err, ErrContextUnknown):
		return grpcstatus.Errorf(codes.InvalidArgument, "unknown context ID")
	default:
		return grpcstatus.Errorf(codes.Internal, "internal error: %v", err)
	}
}

// getStringField safely extracts a string field from a map.
func getStringField(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}
