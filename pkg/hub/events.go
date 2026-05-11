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

package hub

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// EventPublisher defines the interface for publishing Hub events.
// Implementations fan out events to subscribers by subject pattern.
type EventPublisher interface {
	PublishAgentStatus(ctx context.Context, agent *store.Agent)
	PublishAgentCreated(ctx context.Context, agent *store.Agent)
	PublishAgentDeleted(ctx context.Context, agentID, projectID string)
	PublishProjectCreated(ctx context.Context, project *store.Project)
	PublishProjectUpdated(ctx context.Context, project *store.Project)
	PublishProjectDeleted(ctx context.Context, projectID string)
	PublishBrokerConnected(ctx context.Context, brokerID, brokerName string, projectIDs []string)
	PublishBrokerDisconnected(ctx context.Context, brokerID string, projectIDs []string)
	PublishBrokerStatus(ctx context.Context, brokerID, status string)
	PublishNotification(ctx context.Context, notif *store.Notification)
	PublishUserMessage(ctx context.Context, msg *store.Message)
	PublishAllowListChanged(ctx context.Context, action string, email string)
	PublishInviteChanged(ctx context.Context, action string, inviteID string, codePrefix string)
	Close()
}

// noopEventPublisher is a zero-value EventPublisher where all methods are no-ops.
// The Server initializes events to this so handlers never need nil checks.
type noopEventPublisher struct{}

func (noopEventPublisher) PublishAgentStatus(_ context.Context, _ *store.Agent)              {}
func (noopEventPublisher) PublishAgentCreated(_ context.Context, _ *store.Agent)             {}
func (noopEventPublisher) PublishAgentDeleted(_ context.Context, _, _ string)                {}
func (noopEventPublisher) PublishProjectCreated(_ context.Context, _ *store.Project)             {}
func (noopEventPublisher) PublishProjectUpdated(_ context.Context, _ *store.Project)             {}
func (noopEventPublisher) PublishProjectDeleted(_ context.Context, _ string)                   {}
func (noopEventPublisher) PublishBrokerConnected(_ context.Context, _, _ string, _ []string) {}
func (noopEventPublisher) PublishBrokerDisconnected(_ context.Context, _ string, _ []string) {}
func (noopEventPublisher) PublishBrokerStatus(_ context.Context, _, _ string)                {}
func (noopEventPublisher) PublishNotification(_ context.Context, _ *store.Notification)      {}
func (noopEventPublisher) PublishUserMessage(_ context.Context, _ *store.Message)            {}
func (noopEventPublisher) PublishAllowListChanged(_ context.Context, _, _ string)             {}
func (noopEventPublisher) PublishInviteChanged(_ context.Context, _, _, _ string)             {}
func (noopEventPublisher) Close()                                                            {}

// Event is a published event with a subject and JSON-encoded data.
type Event struct {
	Subject string
	Data    []byte
}

// AgentDetail provides freeform context about the current activity in SSE events.
type AgentDetail struct {
	ToolName    string `json:"toolName,omitempty"`
	Message     string `json:"message,omitempty"`
	TaskSummary string `json:"taskSummary,omitempty"`

	// Limits tracking — included so the frontend can update counters in real-time.
	CurrentTurns      int    `json:"currentTurns,omitempty"`
	CurrentModelCalls int    `json:"currentModelCalls,omitempty"`
	StartedAt         string `json:"startedAt,omitempty"`
}

// AgentStatusEvent is published when an agent's status changes.
type AgentStatusEvent struct {
	AgentID         string       `json:"agentId"`
	ProjectID       string       `json:"projectId"`
	GroveID         string       `json:"groveId"`
	Phase           string       `json:"phase,omitempty"`
	Activity        string       `json:"activity,omitempty"`
	Detail          *AgentDetail `json:"detail,omitempty"`
	ContainerStatus string       `json:"containerStatus,omitempty"`
}

// AgentCreatedEvent is published when an agent is created.
// Unlike status deltas this carries the full agent snapshot so that
// subscribers can render a complete row without an extra REST fetch.
type AgentCreatedEvent struct {
	AgentID         string `json:"agentId"`
	ProjectID       string `json:"projectId"`
	GroveID         string `json:"groveId"`
	Name            string `json:"name"`
	Slug            string `json:"slug"`
	Template        string `json:"template,omitempty"`
	Phase           string `json:"phase,omitempty"`
	Activity        string `json:"activity,omitempty"`
	ContainerStatus string `json:"containerStatus,omitempty"`
	Image           string `json:"image,omitempty"`
	Runtime         string `json:"runtime,omitempty"`
	RuntimeBrokerID string `json:"runtimeBrokerId,omitempty"`
	CreatedBy       string `json:"createdBy,omitempty"`
	Visibility      string `json:"visibility,omitempty"`
	TaskSummary     string `json:"taskSummary,omitempty"`
	Created         string `json:"created,omitempty"`
}

// AgentDeletedEvent is published when an agent is deleted.
type AgentDeletedEvent struct {
	AgentID   string `json:"agentId"`
	ProjectID string `json:"projectId"`
	GroveID   string `json:"groveId"`
}

// ProjectCreatedEvent is published when a project is created.
type ProjectCreatedEvent struct {
	ProjectID string `json:"projectId"`
	GroveID   string `json:"groveId"`
	Name      string `json:"name"`
	Slug      string `json:"slug"`
}

// ProjectUpdatedEvent is published when a project is updated.
type ProjectUpdatedEvent struct {
	ProjectID string `json:"projectId"`
	GroveID   string `json:"groveId"`
	Name      string `json:"name"`
}

// ProjectDeletedEvent is published when a project is deleted.
type ProjectDeletedEvent struct {
	ProjectID string `json:"projectId"`
	GroveID   string `json:"groveId"`
}

// BrokerProjectEvent is published when a broker connects or disconnects,
// with one event per project the broker serves.
type BrokerProjectEvent struct {
	BrokerID   string `json:"brokerId"`
	BrokerName string `json:"brokerName,omitempty"`
	ProjectID  string `json:"projectId"`
	GroveID    string `json:"groveId"`
	Status     string `json:"status"` // "online" or "offline"
}

// BrokerStatusEvent is published for general broker status changes.
type BrokerStatusEvent struct {
	BrokerID string `json:"brokerId"`
	Status   string `json:"status"`
}

// UserMessageEvent is published when a message involving a human user is
// persisted — either an agent→user reply or a user→agent instruction.
type UserMessageEvent struct {
	ID          string `json:"id"`
	ProjectID   string `json:"projectId"`
	GroveID     string `json:"groveId"`
	Sender      string `json:"sender"`
	SenderID    string `json:"senderId"`
	Recipient   string `json:"recipient"`
	RecipientID string `json:"recipientId"`
	Msg         string `json:"msg"`
	Type        string `json:"type"`
	Urgent      bool   `json:"urgent,omitempty"`
	Broadcasted bool   `json:"broadcasted,omitempty"`
	AgentID     string `json:"agentId"`
	CreatedAt   string `json:"createdAt"`
}

// NotificationCreatedEvent is published when a user notification is created.
type NotificationCreatedEvent struct {
	ID        string `json:"id"`
	AgentID   string `json:"agentId"`
	ProjectID string `json:"projectId"`
	GroveID   string `json:"groveId"`
	Status    string `json:"status"`
	Message   string `json:"message"`
	CreatedAt string `json:"createdAt"`
}

// AllowListChangedEvent is published when the allow list is modified.
type AllowListChangedEvent struct {
	Action string `json:"action"` // "added", "removed", "bulk_added"
	Email  string `json:"email,omitempty"`
}

// InviteChangedEvent is published when an invite code is created, redeemed, revoked, or deleted.
type InviteChangedEvent struct {
	Action     string `json:"action"` // "created", "redeemed", "revoked", "deleted"
	InviteID   string `json:"inviteId"`
	CodePrefix string `json:"codePrefix,omitempty"`
}

// ChannelEventPublisher is an in-process event publisher that fans out events
// to Go channel subscribers using NATS-style subject matching.
type ChannelEventPublisher struct {
	mu          sync.RWMutex
	subscribers map[string][]chan Event
	closed      bool
}

// NewChannelEventPublisher creates a new ChannelEventPublisher.
func NewChannelEventPublisher() *ChannelEventPublisher {
	return &ChannelEventPublisher{
		subscribers: make(map[string][]chan Event),
	}
}

// Subscribe returns a channel that receives events matching the given patterns,
// and an unsubscribe function. The channel is buffered with capacity 64.
// Patterns use NATS-style wildcards: * matches a single token, > matches the remainder.
func (p *ChannelEventPublisher) Subscribe(patterns ...string) (<-chan Event, func()) {
	ch := make(chan Event, 64)

	p.mu.Lock()
	for _, pattern := range patterns {
		p.subscribers[pattern] = append(p.subscribers[pattern], ch)
	}
	p.mu.Unlock()

	unsubscribe := func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		for _, pattern := range patterns {
			subs := p.subscribers[pattern]
			for i, s := range subs {
				if s == ch {
					p.subscribers[pattern] = append(subs[:i], subs[i+1:]...)
					break
				}
			}
		}
	}

	return ch, unsubscribe
}

// publish marshals the event to JSON and fans out to matching subscribers.
// Sends are non-blocking: events are dropped if a subscriber's buffer is full.
func (p *ChannelEventPublisher) publish(subject string, event interface{}) {
	data, err := json.Marshal(event)
	if err != nil {
		slog.Error("Failed to marshal event", "subject", subject, "error", err)
		return
	}

	evt := Event{Subject: subject, Data: data}

	p.mu.RLock()
	defer p.mu.RUnlock()

	if p.closed {
		return
	}

	for pattern, subs := range p.subscribers {
		if subjectMatchesPattern(pattern, subject) {
			for _, ch := range subs {
				select {
				case ch <- evt:
				default:
					// Drop event on full buffer (backpressure)
				}
			}
		}
	}
}

// Close marks the publisher as closed and closes all subscriber channels.
func (p *ChannelEventPublisher) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return
	}
	p.closed = true

	// Close all unique subscriber channels
	seen := make(map[chan Event]bool)
	for _, subs := range p.subscribers {
		for _, ch := range subs {
			if !seen[ch] {
				close(ch)
				seen[ch] = true
			}
		}
	}
}

// PublishAgentStatus publishes an agent status event to both agent-specific
// and project-scoped subjects (dual-publish pattern).
func (p *ChannelEventPublisher) PublishAgentStatus(_ context.Context, agent *store.Agent) {
	evt := AgentStatusEvent{
		AgentID:         agent.ID,
		ProjectID:       agent.ProjectID,
		GroveID:         agent.ProjectID,
		Phase:           agent.Phase,
		Activity:        agent.Activity,
		ContainerStatus: agent.ContainerStatus,
	}

	detail := AgentDetail{
		ToolName:          agent.ToolName,
		Message:           agent.Message,
		TaskSummary:       agent.TaskSummary,
		CurrentTurns:      agent.CurrentTurns,
		CurrentModelCalls: agent.CurrentModelCalls,
	}
	if !agent.StartedAt.IsZero() {
		detail.StartedAt = agent.StartedAt.Format("2006-01-02T15:04:05Z07:00")
	}
	if detail != (AgentDetail{}) {
		evt.Detail = &detail
	}
	p.publish("agent."+agent.ID+".status", evt)
	if agent.ProjectID != "" {
		p.publish("project."+agent.ProjectID+".agent.status", evt)
		p.publish("grove."+agent.ProjectID+".agent.status", evt)
	}
}

// PublishAgentCreated publishes an agent created event to both agent-specific
// and project-scoped subjects (dual-publish pattern).
func (p *ChannelEventPublisher) PublishAgentCreated(_ context.Context, agent *store.Agent) {
	evt := AgentCreatedEvent{
		AgentID:         agent.ID,
		ProjectID:       agent.ProjectID,
		GroveID:         agent.ProjectID,
		Name:            agent.Name,
		Slug:            agent.Slug,
		Template:        agent.Template,
		Phase:           agent.Phase,
		Activity:        agent.Activity,
		ContainerStatus: agent.ContainerStatus,
		Image:           agent.Image,
		Runtime:         agent.Runtime,
		RuntimeBrokerID: agent.RuntimeBrokerID,
		CreatedBy:       agent.CreatedBy,
		Visibility:      agent.Visibility,
		TaskSummary:     agent.TaskSummary,
	}
	if !agent.Created.IsZero() {
		evt.Created = agent.Created.Format("2006-01-02T15:04:05Z07:00")
	}
	p.publish("agent."+agent.ID+".created", evt)
	if agent.ProjectID != "" {
		p.publish("project."+agent.ProjectID+".agent.created", evt)
		p.publish("grove."+agent.ProjectID+".agent.created", evt)
	}
}

// PublishAgentDeleted publishes an agent deleted event to both agent-specific
// and project-scoped subjects (dual-publish pattern).
func (p *ChannelEventPublisher) PublishAgentDeleted(_ context.Context, agentID, projectID string) {
	evt := AgentDeletedEvent{
		AgentID:   agentID,
		ProjectID: projectID,
		GroveID:   projectID,
	}
	p.publish("agent."+agentID+".deleted", evt)
	if projectID != "" {
		p.publish("project."+projectID+".agent.deleted", evt)
		p.publish("grove."+projectID+".agent.deleted", evt)
	}
}

// PublishProjectCreated publishes a project created event.
func (p *ChannelEventPublisher) PublishProjectCreated(_ context.Context, project *store.Project) {
	evt := ProjectCreatedEvent{
		ProjectID: project.ID,
		GroveID:   project.ID,
		Name:      project.Name,
		Slug:      project.Slug,
	}
	p.publish("project."+project.ID+".created", evt)
	p.publish("grove."+project.ID+".created", evt)
}

// PublishProjectUpdated publishes a project updated event.
func (p *ChannelEventPublisher) PublishProjectUpdated(_ context.Context, project *store.Project) {
	evt := ProjectUpdatedEvent{
		ProjectID: project.ID,
		GroveID:   project.ID,
		Name:      project.Name,
	}
	p.publish("project."+project.ID+".updated", evt)
	p.publish("grove."+project.ID+".updated", evt)
}

// PublishProjectDeleted publishes a project deleted event.
func (p *ChannelEventPublisher) PublishProjectDeleted(_ context.Context, projectID string) {
	evt := ProjectDeletedEvent{
		ProjectID: projectID,
		GroveID:   projectID,
	}
	p.publish("project."+projectID+".deleted", evt)
	p.publish("grove."+projectID+".deleted", evt)
}

// PublishBrokerConnected publishes broker connection events, one per project the broker serves.
func (p *ChannelEventPublisher) PublishBrokerConnected(_ context.Context, brokerID, brokerName string, projectIDs []string) {
	for _, gid := range projectIDs {
		evt := BrokerProjectEvent{
			BrokerID:   brokerID,
			BrokerName: brokerName,
			ProjectID:  gid,
			GroveID:    gid,
			Status:     "online",
		}
		p.publish("project."+gid+".broker.status", evt)
		p.publish("grove."+gid+".broker.status", evt)
	}
}

// PublishBrokerDisconnected publishes broker disconnection events, one per project the broker serves.
func (p *ChannelEventPublisher) PublishBrokerDisconnected(_ context.Context, brokerID string, projectIDs []string) {
	for _, gid := range projectIDs {
		evt := BrokerProjectEvent{
			BrokerID:  brokerID,
			ProjectID: gid,
			GroveID:   gid,
			Status:    "offline",
		}
		p.publish("project."+gid+".broker.status", evt)
		p.publish("grove."+gid+".broker.status", evt)
	}
}

// PublishBrokerStatus publishes a general broker status event.
func (p *ChannelEventPublisher) PublishBrokerStatus(_ context.Context, brokerID, status string) {
	evt := BrokerStatusEvent{
		BrokerID: brokerID,
		Status:   status,
	}
	p.publish("broker."+brokerID+".status", evt)
}

// PublishNotification publishes a user notification event.
func (p *ChannelEventPublisher) PublishNotification(_ context.Context, notif *store.Notification) {
	evt := NotificationCreatedEvent{
		ID:        notif.ID,
		AgentID:   notif.AgentID,
		ProjectID: notif.ProjectID,
		GroveID:   notif.ProjectID,
		Status:    notif.Status,
		Message:   notif.Message,
		CreatedAt: notif.CreatedAt.Format("2006-01-02T15:04:05.000Z"),
	}
	p.publish("notification.created", evt)
	if notif.ProjectID != "" {
		p.publish("project."+notif.ProjectID+".notification", evt)
		p.publish("grove."+notif.ProjectID+".notification", evt)
	}
}

// PublishAllowListChanged publishes an allow list change event.
// Email is intentionally omitted from the event to avoid PII leak via SSE.
func (p *ChannelEventPublisher) PublishAllowListChanged(_ context.Context, action, _ string) {
	evt := AllowListChangedEvent{
		Action: action,
	}
	p.publish("admin.allowlist.changed", evt)
}

// PublishInviteChanged publishes an invite code change event.
func (p *ChannelEventPublisher) PublishInviteChanged(_ context.Context, action, inviteID, codePrefix string) {
	evt := InviteChangedEvent{
		Action:     action,
		InviteID:   inviteID,
		CodePrefix: codePrefix,
	}
	p.publish("admin.invite.changed", evt)
}

// PublishUserMessage publishes a user.message event when a message involving
// a human user is persisted — either an agent→user reply (from
// handleAgentOutboundMessage) or a user→agent instruction (from
// handleAgentMessage). The event is fanned out to several subjects so
// different consumers can subscribe at the granularity they need:
//
//   - user.<recipientID>.message — inbox-tray for the message's addressee
//     (only when the recipient is a user, not an agent)
//   - project.<projectID>.user.message — project-level user-message feeds
//     (only when the recipient is a user)
//   - agent.<agentID>.message — per-agent conversation streams (both
//     directions; subscribers filter by user participation themselves)
func (p *ChannelEventPublisher) PublishUserMessage(_ context.Context, msg *store.Message) {
	evt := UserMessageEvent{
		ID:          msg.ID,
		ProjectID:   msg.ProjectID,
		GroveID:     msg.ProjectID,
		Sender:      msg.Sender,
		SenderID:    msg.SenderID,
		Recipient:   msg.Recipient,
		RecipientID: msg.RecipientID,
		Msg:         msg.Msg,
		Type:        msg.Type,
		Urgent:      msg.Urgent,
		Broadcasted: msg.Broadcasted,
		AgentID:     msg.AgentID,
		CreatedAt:   msg.CreatedAt.Format("2006-01-02T15:04:05.000Z"),
	}
	// Only fan out to user-inbox and project-level subjects when the
	// recipient is actually a human user. For user→agent messages the
	// RecipientID is the agent UUID, so publishing to user.<agentID>
	// would be a no-op (no subscriber) and project feeds would double-
	// count by mixing user→agent prompts with agent→user replies.
	recipientIsUser := strings.HasPrefix(msg.Recipient, "user:")
	if recipientIsUser && msg.RecipientID != "" {
		p.publish("user."+msg.RecipientID+".message", evt)
	}
	if recipientIsUser && msg.ProjectID != "" {
		p.publish("project."+msg.ProjectID+".user.message", evt)
		p.publish("grove."+msg.ProjectID+".user.message", evt)
	}
	if msg.AgentID != "" {
		p.publish("agent."+msg.AgentID+".message", evt)
	}
}

// subjectMatchesPattern checks if a subject matches a NATS-style pattern.
// '*' matches exactly one token, '>' matches one or more remaining tokens.
// Tokens are dot-separated.
func subjectMatchesPattern(pattern, subject string) bool {
	patternParts := strings.Split(pattern, ".")
	subjectParts := strings.Split(subject, ".")

	for i, pp := range patternParts {
		if pp == ">" {
			// '>' matches one or more remaining tokens
			return i < len(subjectParts)
		}
		if i >= len(subjectParts) {
			return false
		}
		if pp == "*" {
			continue
		}
		if pp != subjectParts[i] {
			return false
		}
	}

	return len(patternParts) == len(subjectParts)
}
