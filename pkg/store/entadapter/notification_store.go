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

package entadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/notification"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/notificationsubscription"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/subscriptiontemplate"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// entDesc returns the Ent ordering option for descending order.
func entDesc() entsql.OrderTermOption { return entsql.OrderDesc() }

// NotificationEventType enumerates the kinds of notification changes that are
// published to a real-time channel (e.g. Postgres LISTEN/NOTIFY).
type NotificationEventType string

const (
	// NotificationEventCreated is published when a notification record is created.
	NotificationEventCreated NotificationEventType = "created"
	// NotificationEventDispatched is published when a notification is claimed for
	// dispatch by exactly one replica (see MarkNotificationDispatched).
	NotificationEventDispatched NotificationEventType = "dispatched"
)

// NotificationEvent describes a change worth broadcasting to other hub replicas
// so they can react in real time (deliver to a connected agent, wake a poller,
// etc.) instead of busy-polling the database.
type NotificationEvent struct {
	Type           NotificationEventType `json:"type"`
	NotificationID string                `json:"notificationId"`
	SubscriberType string                `json:"subscriberType"`
	SubscriberID   string                `json:"subscriberId"`
	ProjectID      string                `json:"projectId"`
}

// NotificationPublisher publishes notification events to a real-time channel.
//
// In a multi-replica Postgres deployment this is implemented on top of
// LISTEN/NOTIFY so a notification created or dispatched on one replica is seen
// immediately by the others. The SQLite / single-process path can leave the
// publisher nil, in which case publishing is a no-op. Publishing is best-effort:
// a publish failure never fails the underlying write.
type NotificationPublisher interface {
	PublishNotification(ctx context.Context, evt NotificationEvent) error
}

// NotificationStore implements store.NotificationStore using Ent ORM.
type NotificationStore struct {
	client    *ent.Client
	publisher NotificationPublisher
}

// NewNotificationStore creates a new Ent-backed NotificationStore. No publisher
// is attached; use WithPublisher to wire LISTEN/NOTIFY for multi-replica
// deployments.
func NewNotificationStore(client *ent.Client) *NotificationStore {
	return &NotificationStore{client: client}
}

// WithPublisher returns a copy of the store that publishes notification events
// through p. Passing nil disables publishing.
func (s *NotificationStore) WithPublisher(p NotificationPublisher) *NotificationStore {
	clone := *s
	clone.publisher = p
	return &clone
}

// publish emits an event best-effort. Errors are intentionally swallowed so the
// real-time fan-out never breaks the durable write path.
func (s *NotificationStore) publish(ctx context.Context, evt NotificationEvent) {
	if s.publisher == nil {
		return
	}
	_ = s.publisher.PublishNotification(ctx, evt)
}

// ----------------------------------------------------------------------------
// Conversions
// ----------------------------------------------------------------------------

// entSubToStore converts an Ent NotificationSubscription to the store model.
func entSubToStore(e *ent.NotificationSubscription) *store.NotificationSubscription {
	sub := &store.NotificationSubscription{
		ID:             e.ID.String(),
		Scope:          e.Scope,
		SubscriberType: e.SubscriberType,
		SubscriberID:   e.SubscriberID,
		ProjectID:      e.ProjectID.String(),
		CreatedAt:      e.Created,
		CreatedBy:      e.CreatedBy,
	}
	if e.AgentID != nil {
		sub.AgentID = e.AgentID.String()
	}
	if e.TriggerActivities != "" {
		_ = json.Unmarshal([]byte(e.TriggerActivities), &sub.TriggerActivities)
	}
	return sub
}

// entNotifToStore converts an Ent Notification to the store model.
func entNotifToStore(e *ent.Notification) *store.Notification {
	return &store.Notification{
		ID:             e.ID.String(),
		SubscriptionID: e.SubscriptionID.String(),
		AgentID:        e.AgentID.String(),
		ProjectID:      e.ProjectID.String(),
		SubscriberType: e.SubscriberType,
		SubscriberID:   e.SubscriberID,
		Status:         e.Status,
		Message:        e.Message,
		Dispatched:     e.Dispatched,
		Acknowledged:   e.Acknowledged,
		CreatedAt:      e.Created,
	}
}

// entTemplateToStore converts an Ent SubscriptionTemplate to the store model.
func entTemplateToStore(e *ent.SubscriptionTemplate) *store.SubscriptionTemplate {
	tmpl := &store.SubscriptionTemplate{
		ID:        e.ID.String(),
		Name:      e.Name,
		Scope:     e.Scope,
		CreatedBy: e.CreatedBy,
	}
	if e.ProjectID != nil {
		tmpl.ProjectID = e.ProjectID.String()
	}
	if e.TriggerActivities != "" {
		_ = json.Unmarshal([]byte(e.TriggerActivities), &tmpl.TriggerActivities)
	}
	return tmpl
}

// marshalTriggers serializes trigger activities to the JSON string stored in the
// dialect-neutral trigger_activities column.
func marshalTriggers(triggers []string) string {
	if triggers == nil {
		triggers = []string{}
	}
	b, _ := json.Marshal(triggers)
	return string(b)
}

// ----------------------------------------------------------------------------
// Notification Subscription Operations
// ----------------------------------------------------------------------------

// CreateNotificationSubscription creates a new notification subscription.
func (s *NotificationStore) CreateNotificationSubscription(ctx context.Context, sub *store.NotificationSubscription) error {
	if sub.ID == "" || sub.SubscriberID == "" || sub.ProjectID == "" {
		return store.ErrInvalidInput
	}

	// Default scope to agent for backward compatibility.
	if sub.Scope == "" {
		sub.Scope = store.SubscriptionScopeAgent
	}

	// Validate scope-specific constraints.
	switch sub.Scope {
	case store.SubscriptionScopeAgent:
		if sub.AgentID == "" {
			return store.ErrInvalidInput
		}
	case store.SubscriptionScopeProject:
		sub.AgentID = "" // Ensure no agent_id for project-scoped subscriptions.
	default:
		return fmt.Errorf("invalid scope %q: %w", sub.Scope, store.ErrInvalidInput)
	}

	subscriberType := sub.SubscriberType
	if subscriberType == "" {
		subscriberType = "agent"
	}

	id, err := parseUUID(sub.ID)
	if err != nil {
		return err
	}
	projectUID, err := parseUUID(sub.ProjectID)
	if err != nil {
		return err
	}

	if sub.CreatedAt.IsZero() {
		sub.CreatedAt = time.Now()
	}

	create := s.client.NotificationSubscription.Create().
		SetID(id).
		SetScope(sub.Scope).
		SetSubscriberType(subscriberType).
		SetSubscriberID(sub.SubscriberID).
		SetProjectID(projectUID).
		SetTriggerActivities(marshalTriggers(sub.TriggerActivities)).
		SetCreatedBy(sub.CreatedBy).
		SetCreated(sub.CreatedAt)

	if sub.AgentID != "" {
		agentUID, err := parseUUID(sub.AgentID)
		if err != nil {
			return err
		}
		create.SetAgentID(agentUID)
	}

	if _, err := create.Save(ctx); err != nil {
		return mapError(err)
	}
	return nil
}

// GetNotificationSubscription returns a single subscription by ID.
func (s *NotificationStore) GetNotificationSubscription(ctx context.Context, id string) (*store.NotificationSubscription, error) {
	uid, err := parseGetID(id)
	if err != nil {
		return nil, err
	}
	e, err := s.client.NotificationSubscription.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	return entSubToStore(e), nil
}

// GetNotificationSubscriptions returns all agent-scoped subscriptions for a watched agent.
func (s *NotificationStore) GetNotificationSubscriptions(ctx context.Context, agentID string) ([]store.NotificationSubscription, error) {
	uid, err := parseUUID(agentID)
	if err != nil {
		return nil, err
	}
	rows, err := s.client.NotificationSubscription.Query().
		Where(notificationsubscription.AgentIDEQ(uid)).
		Order(notificationsubscription.ByCreated()).
		All(ctx)
	if err != nil {
		return nil, err
	}
	return subsToStore(rows), nil
}

// GetNotificationSubscriptionsByProject returns all subscriptions within a project (any scope).
func (s *NotificationStore) GetNotificationSubscriptionsByProject(ctx context.Context, projectID string) ([]store.NotificationSubscription, error) {
	uid, err := parseUUID(projectID)
	if err != nil {
		return nil, err
	}
	rows, err := s.client.NotificationSubscription.Query().
		Where(notificationsubscription.ProjectIDEQ(uid)).
		Order(notificationsubscription.ByCreated()).
		All(ctx)
	if err != nil {
		return nil, err
	}
	return subsToStore(rows), nil
}

// GetNotificationSubscriptionsByProjectScope returns project-scoped subscriptions
// (scope='project') for a given project.
func (s *NotificationStore) GetNotificationSubscriptionsByProjectScope(ctx context.Context, projectID string) ([]store.NotificationSubscription, error) {
	uid, err := parseUUID(projectID)
	if err != nil {
		return nil, err
	}
	rows, err := s.client.NotificationSubscription.Query().
		Where(
			notificationsubscription.ProjectIDEQ(uid),
			notificationsubscription.ScopeEQ(store.SubscriptionScopeProject),
		).
		Order(notificationsubscription.ByCreated()).
		All(ctx)
	if err != nil {
		return nil, err
	}
	return subsToStore(rows), nil
}

// GetSubscriptionsForSubscriber returns all subscriptions owned by a subscriber.
func (s *NotificationStore) GetSubscriptionsForSubscriber(ctx context.Context, subscriberType, subscriberID string) ([]store.NotificationSubscription, error) {
	rows, err := s.client.NotificationSubscription.Query().
		Where(
			notificationsubscription.SubscriberTypeEQ(subscriberType),
			notificationsubscription.SubscriberIDEQ(subscriberID),
		).
		Order(notificationsubscription.ByCreated()).
		All(ctx)
	if err != nil {
		return nil, err
	}
	return subsToStore(rows), nil
}

// UpdateNotificationSubscriptionTriggers updates the trigger activities of a subscription.
func (s *NotificationStore) UpdateNotificationSubscriptionTriggers(ctx context.Context, id string, triggerActivities []string) error {
	if id == "" || len(triggerActivities) == 0 {
		return store.ErrInvalidInput
	}
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	_, err = s.client.NotificationSubscription.UpdateOneID(uid).
		SetTriggerActivities(marshalTriggers(triggerActivities)).
		Save(ctx)
	if err != nil {
		return mapError(err)
	}
	return nil
}

// DeleteNotificationSubscription deletes a subscription by ID.
func (s *NotificationStore) DeleteNotificationSubscription(ctx context.Context, id string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	if err := s.client.NotificationSubscription.DeleteOneID(uid).Exec(ctx); err != nil {
		return mapError(err)
	}
	return nil
}

// DeleteNotificationSubscriptionsForAgent deletes all subscriptions for a watched agent.
// No error on zero rows affected.
func (s *NotificationStore) DeleteNotificationSubscriptionsForAgent(ctx context.Context, agentID string) error {
	uid, err := parseUUID(agentID)
	if err != nil {
		return err
	}
	_, err = s.client.NotificationSubscription.Delete().
		Where(notificationsubscription.AgentIDEQ(uid)).
		Exec(ctx)
	return err
}

// subsToStore converts a slice of Ent subscriptions to store models.
func subsToStore(rows []*ent.NotificationSubscription) []store.NotificationSubscription {
	out := make([]store.NotificationSubscription, 0, len(rows))
	for _, e := range rows {
		out = append(out, *entSubToStore(e))
	}
	return out
}

// ----------------------------------------------------------------------------
// Notification Operations
// ----------------------------------------------------------------------------

// CreateNotification creates a new notification record.
func (s *NotificationStore) CreateNotification(ctx context.Context, notif *store.Notification) error {
	if notif.ID == "" || notif.SubscriptionID == "" || notif.AgentID == "" {
		return store.ErrInvalidInput
	}

	id, err := parseUUID(notif.ID)
	if err != nil {
		return err
	}
	subUID, err := parseUUID(notif.SubscriptionID)
	if err != nil {
		return err
	}
	agentUID, err := parseUUID(notif.AgentID)
	if err != nil {
		return err
	}
	projectUID, err := parseUUID(notif.ProjectID)
	if err != nil {
		return err
	}

	if notif.CreatedAt.IsZero() {
		notif.CreatedAt = time.Now()
	}

	_, err = s.client.Notification.Create().
		SetID(id).
		SetSubscriptionID(subUID).
		SetAgentID(agentUID).
		SetProjectID(projectUID).
		SetSubscriberType(notif.SubscriberType).
		SetSubscriberID(notif.SubscriberID).
		SetStatus(notif.Status).
		SetMessage(notif.Message).
		SetDispatched(notif.Dispatched).
		SetAcknowledged(notif.Acknowledged).
		SetCreated(notif.CreatedAt).
		Save(ctx)
	if err != nil {
		return mapError(err)
	}

	// Broadcast so other replicas can pick up delivery in real time.
	s.publish(ctx, NotificationEvent{
		Type:           NotificationEventCreated,
		NotificationID: notif.ID,
		SubscriberType: notif.SubscriberType,
		SubscriberID:   notif.SubscriberID,
		ProjectID:      notif.ProjectID,
	})
	return nil
}

// GetNotifications returns notifications for a subscriber, newest first.
func (s *NotificationStore) GetNotifications(ctx context.Context, subscriberType, subscriberID string, onlyUnacknowledged bool) ([]store.Notification, error) {
	query := s.client.Notification.Query().
		Where(
			notification.SubscriberTypeEQ(subscriberType),
			notification.SubscriberIDEQ(subscriberID),
		)
	if onlyUnacknowledged {
		query = query.Where(notification.AcknowledgedEQ(false))
	}
	rows, err := query.Order(notification.ByCreated(entDesc())).All(ctx)
	if err != nil {
		return nil, err
	}
	return notifsToStore(rows), nil
}

// GetNotificationsByAgent returns notifications for a subscriber filtered by agent ID, newest first.
func (s *NotificationStore) GetNotificationsByAgent(ctx context.Context, agentID, subscriberType, subscriberID string, onlyUnacknowledged bool) ([]store.Notification, error) {
	uid, err := parseUUID(agentID)
	if err != nil {
		return nil, err
	}
	query := s.client.Notification.Query().
		Where(
			notification.AgentIDEQ(uid),
			notification.SubscriberTypeEQ(subscriberType),
			notification.SubscriberIDEQ(subscriberID),
		)
	if onlyUnacknowledged {
		query = query.Where(notification.AcknowledgedEQ(false))
	}
	rows, err := query.Order(notification.ByCreated(entDesc())).All(ctx)
	if err != nil {
		return nil, err
	}
	return notifsToStore(rows), nil
}

// AcknowledgeNotification marks a notification as acknowledged.
func (s *NotificationStore) AcknowledgeNotification(ctx context.Context, id string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	_, err = s.client.Notification.UpdateOneID(uid).SetAcknowledged(true).Save(ctx)
	if err != nil {
		return mapError(err)
	}
	return nil
}

// AcknowledgeAllNotifications marks all notifications for a subscriber as acknowledged.
// No error on zero rows affected.
func (s *NotificationStore) AcknowledgeAllNotifications(ctx context.Context, subscriberType, subscriberID string) error {
	_, err := s.client.Notification.Update().
		Where(
			notification.SubscriberTypeEQ(subscriberType),
			notification.SubscriberIDEQ(subscriberID),
		).
		SetAcknowledged(true).
		Save(ctx)
	return err
}

// MarkNotificationDispatched atomically claims a notification for dispatch.
//
// The conditional update (dispatched = false guard) is the multi-replica
// concurrency primitive: in a Postgres deployment several hub replicas may race
// to dispatch the same notification, but the UPDATE ... WHERE dispatched = false
// is atomic, so exactly one replica observes affected == 1 and "wins" the claim.
// That winner is the one that publishes the dispatch event / drives the side
// effect; losers (affected == 0 on an existing row) treat it as an idempotent
// no-op. A missing row is still reported as ErrNotFound to preserve the
// interface contract.
func (s *NotificationStore) MarkNotificationDispatched(ctx context.Context, id string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}

	affected, err := s.client.Notification.Update().
		Where(
			notification.IDEQ(uid),
			notification.DispatchedEQ(false),
		).
		SetDispatched(true).
		Save(ctx)
	if err != nil {
		return mapError(err)
	}

	if affected == 0 {
		// Either the notification doesn't exist or another replica already
		// claimed the dispatch. Disambiguate to preserve ErrNotFound semantics.
		e, err := s.client.Notification.Query().Where(notification.IDEQ(uid)).Only(ctx)
		if err != nil {
			return mapError(err)
		}
		// Already dispatched by some replica — idempotent success.
		_ = e
		return nil
	}

	// We won the claim; only this replica broadcasts the dispatch.
	if e, err := s.client.Notification.Get(ctx, uid); err == nil {
		s.publish(ctx, NotificationEvent{
			Type:           NotificationEventDispatched,
			NotificationID: id,
			SubscriberType: e.SubscriberType,
			SubscriberID:   e.SubscriberID,
			ProjectID:      e.ProjectID.String(),
		})
	}
	return nil
}

// GetLastNotificationStatus returns the status of the most recent notification
// for a given subscription. Returns ("", nil) if no notifications exist.
func (s *NotificationStore) GetLastNotificationStatus(ctx context.Context, subscriptionID string) (string, error) {
	uid, err := parseUUID(subscriptionID)
	if err != nil {
		return "", err
	}
	e, err := s.client.Notification.Query().
		Where(notification.SubscriptionIDEQ(uid)).
		Order(notification.ByCreated(entDesc())).
		First(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return "", nil
		}
		return "", err
	}
	return e.Status, nil
}

// notifsToStore converts a slice of Ent notifications to store models.
func notifsToStore(rows []*ent.Notification) []store.Notification {
	out := make([]store.Notification, 0, len(rows))
	for _, e := range rows {
		out = append(out, *entNotifToStore(e))
	}
	return out
}

// ----------------------------------------------------------------------------
// Subscription Template Operations
// ----------------------------------------------------------------------------

// CreateSubscriptionTemplate creates a new subscription template.
func (s *NotificationStore) CreateSubscriptionTemplate(ctx context.Context, tmpl *store.SubscriptionTemplate) error {
	if tmpl.ID == "" || tmpl.Name == "" || len(tmpl.TriggerActivities) == 0 {
		return store.ErrInvalidInput
	}

	id, err := parseUUID(tmpl.ID)
	if err != nil {
		return err
	}

	scope := tmpl.Scope
	if scope == "" {
		scope = "project"
	}

	create := s.client.SubscriptionTemplate.Create().
		SetID(id).
		SetName(tmpl.Name).
		SetScope(scope).
		SetTriggerActivities(marshalTriggers(tmpl.TriggerActivities)).
		SetCreatedBy(tmpl.CreatedBy)

	// An empty ProjectID denotes a global template (NULL project_id).
	if tmpl.ProjectID != "" {
		projectUID, err := parseUUID(tmpl.ProjectID)
		if err != nil {
			return err
		}
		create.SetProjectID(projectUID)
	}

	if _, err := create.Save(ctx); err != nil {
		return mapError(err)
	}
	return nil
}

// GetSubscriptionTemplate returns a template by ID.
func (s *NotificationStore) GetSubscriptionTemplate(ctx context.Context, id string) (*store.SubscriptionTemplate, error) {
	uid, err := parseGetID(id)
	if err != nil {
		return nil, err
	}
	e, err := s.client.SubscriptionTemplate.Get(ctx, uid)
	if err != nil {
		return nil, mapError(err)
	}
	return entTemplateToStore(e), nil
}

// ListSubscriptionTemplates returns all templates. If projectID is non-empty,
// returns both global templates and project-specific templates.
func (s *NotificationStore) ListSubscriptionTemplates(ctx context.Context, projectID string) ([]store.SubscriptionTemplate, error) {
	query := s.client.SubscriptionTemplate.Query()

	if projectID != "" {
		uid, err := parseUUID(projectID)
		if err != nil {
			return nil, err
		}
		// Global templates (NULL project_id) plus those owned by this project.
		query = query.Where(subscriptiontemplate.Or(
			subscriptiontemplate.ProjectIDIsNil(),
			subscriptiontemplate.ProjectIDEQ(uid),
		))
	} else {
		query = query.Where(subscriptiontemplate.ProjectIDIsNil())
	}

	rows, err := query.Order(subscriptiontemplate.ByName()).All(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]store.SubscriptionTemplate, 0, len(rows))
	for _, e := range rows {
		out = append(out, *entTemplateToStore(e))
	}
	return out, nil
}

// DeleteSubscriptionTemplate deletes a template by ID.
func (s *NotificationStore) DeleteSubscriptionTemplate(ctx context.Context, id string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	if err := s.client.SubscriptionTemplate.DeleteOneID(uid).Exec(ctx); err != nil {
		return mapError(err)
	}
	return nil
}

// Ensure NotificationStore satisfies the store interface.
var _ store.NotificationStore = (*NotificationStore)(nil)
