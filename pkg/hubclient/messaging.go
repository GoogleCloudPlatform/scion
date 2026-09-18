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

package hubclient

import (
	"context"
	"net/url"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// MessagingService provides operations for cross-project messaging.
type MessagingService interface {
	// Capabilities returns the hub's messaging capabilities and feature flags.
	Capabilities(ctx context.Context) (*MessagingCapabilities, error)

	// ResolveTarget resolves a cross-project messaging target by project and agent reference.
	// Privacy-preserving: returns 404 for both nonexistent and unauthorized targets.
	ResolveTarget(ctx context.Context, projectRef, agentRef string) (*TargetResolveResult, error)

	// ResolveConversation resolves a conversation reference without creating rows.
	// Returns exists=false when the peer is valid but no conversation exists yet.
	ResolveConversation(ctx context.Context, reference string, projectID string) (*ConversationResolveResult, error)

	// SendMessage sends a message into an existing conversation.
	SendMessage(ctx context.Context, conversationID string, req *ConversationSendRequest) (*ConversationSendResult, error)

	// GetHubMessagingSettings returns the hub-level messaging admin settings.
	GetHubMessagingSettings(ctx context.Context) (*HubMessagingSettings, error)

	// UpdateHubMessagingSettings updates the hub-level messaging admin settings.
	// CAS is enforced via ExpectedRevision on security-critical flags.
	UpdateHubMessagingSettings(ctx context.Context, req *UpdateHubMessagingRequest) (*HubMessagingSettings, error)

	// GetProjectMessagingPolicy returns a project's cross-project inbound policy.
	GetProjectMessagingPolicy(ctx context.Context, projectID string) (*ProjectMessagingPolicy, error)

	// UpdateProjectMessagingPolicy updates a project's cross-project inbound policy.
	// Requires project ownership or hub admin. CAS via ExpectedRevision.
	UpdateProjectMessagingPolicy(ctx context.Context, projectID string, req *UpdateProjectMessagingPolicyRequest) (*ProjectMessagingPolicy, error)
}

// messagingService implements MessagingService.
type messagingService struct {
	c *client
}

// MessagingCapabilities describes the hub's messaging feature state.
type MessagingCapabilities struct {
	HubEnabled                    bool     `json:"hubEnabled"`
	CrossProjectConversationKinds []string `json:"crossProjectConversationKinds"`
	SupportedModes                []string `json:"supportedModes"`
}

// TargetAgentInfo contains minimal identity for a resolved messaging target.
type TargetAgentInfo struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	ProjectID   string `json:"projectId"`
	ProjectSlug string `json:"projectSlug"`
}

// MessageabilityInfo contains directional reachability for a resolved target.
type MessageabilityInfo struct {
	CanMessage     bool   `json:"canMessage"`
	CanReachViewer bool   `json:"canReachViewer"`
	ReplyReason    string `json:"replyReason,omitempty"`
}

// TargetResolveResult is the response from resolving a messaging target.
type TargetResolveResult struct {
	Agent          *TargetAgentInfo    `json:"agent"`
	Messageability *MessageabilityInfo `json:"messageability"`
}

// ConversationResolveResult is the response from resolving a conversation reference.
type ConversationResolveResult struct {
	Exists       bool               `json:"exists"`
	Conversation *ConversationDetail `json:"conversation,omitempty"`
	PeerAgent    *TargetAgentInfo    `json:"peerAgent,omitempty"`
}

// ConversationSendRequest is the request body for sending a message into a conversation.
type ConversationSendRequest struct {
	Msg       string `json:"msg"`
	Type      string `json:"type,omitempty"`
	Urgent    bool   `json:"urgent,omitempty"`
	Interrupt bool   `json:"interrupt,omitempty"`
}

// ConversationSendResult is the response from sending a message.
type ConversationSendResult struct {
	MessageID string `json:"messageId"`
	Status    string `json:"status"`
}

// Capabilities returns the hub's messaging capabilities.
func (s *messagingService) Capabilities(ctx context.Context) (*MessagingCapabilities, error) {
	resp, err := s.c.get(ctx, "/api/v1/messaging/capabilities", nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[MessagingCapabilities](resp)
}

// ResolveTarget resolves a cross-project messaging target.
func (s *messagingService) ResolveTarget(ctx context.Context, projectRef, agentRef string) (*TargetResolveResult, error) {
	query := url.Values{}
	query.Set("project", projectRef)
	query.Set("agent", agentRef)

	resp, err := s.c.getWithQuery(ctx, "/api/v1/messaging/targets/resolve", query, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[TargetResolveResult](resp)
}

// ResolveConversation resolves a conversation reference without creating rows.
func (s *messagingService) ResolveConversation(ctx context.Context, reference string, projectID string) (*ConversationResolveResult, error) {
	query := url.Values{}
	query.Set("reference", reference)
	if projectID != "" {
		query.Set("project_id", projectID)
	}

	resp, err := s.c.getWithQuery(ctx, "/api/v1/conversations/resolve", query, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[ConversationResolveResult](resp)
}

// SendMessage sends a message into an existing conversation.
func (s *messagingService) SendMessage(ctx context.Context, conversationID string, req *ConversationSendRequest) (*ConversationSendResult, error) {
	resp, err := s.c.post(ctx, "/api/v1/conversations/"+url.PathEscape(conversationID)+"/messages", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[ConversationSendResult](resp)
}

// ---------------------------------------------------------------------------
// Hub messaging admin settings
// ---------------------------------------------------------------------------

// HubMessagingSettings is the hub-level messaging configuration.
type HubMessagingSettings struct {
	ConversationEnvelopeSwitch   *bool `json:"conversation_envelope_switch"`
	CrossProjectMessagingEnabled *bool `json:"cross_project_messaging_enabled"`
	Revision                     int64 `json:"revision"`
}

// UpdateHubMessagingRequest is the request to update hub messaging settings.
type UpdateHubMessagingRequest struct {
	ConversationEnvelopeSwitch   *bool  `json:"conversation_envelope_switch,omitempty"`
	CrossProjectMessagingEnabled *bool  `json:"cross_project_messaging_enabled,omitempty"`
	ExpectedRevision             *int64 `json:"expected_revision,omitempty"`
}

// GetHubMessagingSettings returns the hub-level messaging admin settings.
func (s *messagingService) GetHubMessagingSettings(ctx context.Context) (*HubMessagingSettings, error) {
	resp, err := s.c.get(ctx, "/api/v1/admin/messaging", nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[HubMessagingSettings](resp)
}

// UpdateHubMessagingSettings updates the hub-level messaging admin settings.
func (s *messagingService) UpdateHubMessagingSettings(ctx context.Context, req *UpdateHubMessagingRequest) (*HubMessagingSettings, error) {
	resp, err := s.c.put(ctx, "/api/v1/admin/messaging", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[HubMessagingSettings](resp)
}

// ---------------------------------------------------------------------------
// Project messaging policy
// ---------------------------------------------------------------------------

// ProjectMessagingPolicy is a project's cross-project inbound policy.
type ProjectMessagingPolicy struct {
	CrossProjectInbound          string `json:"crossProjectInbound"`
	Revision                     int64  `json:"revision"`
	EffectiveCrossProjectInbound string `json:"effectiveCrossProjectInbound"`
	HubCrossProjectEnabled       bool   `json:"hubCrossProjectEnabled"`
}

// UpdateProjectMessagingPolicyRequest is the request to update a project's policy.
type UpdateProjectMessagingPolicyRequest struct {
	CrossProjectInbound string `json:"crossProjectInbound"`
	ExpectedRevision    int64  `json:"expectedRevision"`
}

// GetProjectMessagingPolicy returns a project's cross-project inbound policy.
func (s *messagingService) GetProjectMessagingPolicy(ctx context.Context, projectID string) (*ProjectMessagingPolicy, error) {
	resp, err := s.c.get(ctx, "/api/v1/projects/"+url.PathEscape(projectID)+"/messaging-policy", nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[ProjectMessagingPolicy](resp)
}

// UpdateProjectMessagingPolicy updates a project's cross-project inbound policy.
func (s *messagingService) UpdateProjectMessagingPolicy(ctx context.Context, projectID string, req *UpdateProjectMessagingPolicyRequest) (*ProjectMessagingPolicy, error) {
	resp, err := s.c.put(ctx, "/api/v1/projects/"+url.PathEscape(projectID)+"/messaging-policy", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[ProjectMessagingPolicy](resp)
}
