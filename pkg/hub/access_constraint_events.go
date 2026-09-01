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
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Invalidation event types (B6)
// ---------------------------------------------------------------------------

// InvalidationEvent is a lightweight signal that cached state may be stale.
// Events must NOT include sensitive principal lists — only entity type,
// entity ID, and change type.
type InvalidationEvent struct {
	// Type identifies the change. One of:
	//   boundary.created, boundary.updated, boundary.deleted,
	//   rolebinding.created, rolebinding.updated, rolebinding.deleted,
	//   membership.changed, principal.status_changed,
	//   permission_registry.changed, project.lifecycle_changed,
	//   recovery.started, recovery.completed
	Type string `json:"type"`

	// EntityID is the ID of the entity that changed.
	EntityID string `json:"entityId"`

	// Timestamp is when the event was generated.
	Timestamp time.Time `json:"timestamp"`
}

// Invalidation event type constants.
const (
	EventBoundaryCreated           = "boundary.created"
	EventBoundaryUpdated           = "boundary.updated"
	EventBoundaryDeleted           = "boundary.deleted"
	EventRoleBindingCreated        = "rolebinding.created"
	EventRoleBindingUpdated        = "rolebinding.updated"
	EventRoleBindingDeleted        = "rolebinding.deleted"
	EventMembershipChanged         = "membership.changed"
	EventPrincipalStatusChanged    = "principal.status_changed"
	EventPermissionRegistryChanged = "permission_registry.changed"
	EventProjectLifecycleChanged   = "project.lifecycle_changed"
	EventRecoveryStarted           = "recovery.started"
	EventRecoveryCompleted         = "recovery.completed"
)

// ---------------------------------------------------------------------------
// InvalidationEventBus — in-process pub/sub with authorization
// ---------------------------------------------------------------------------

// InvalidationHandler is a callback that receives invalidation events.
type InvalidationHandler func(event InvalidationEvent)

// EventSubscription represents a registered event handler.
type EventSubscription struct {
	// ID is a unique identifier for this subscription.
	ID string

	// Handler is the callback function.
	Handler InvalidationHandler

	// EventTypes limits delivery to these event types. Empty means all events.
	EventTypes []string

	// PrincipalID is the subscriber's principal ID for authorization.
	PrincipalID string

	// Authorized indicates whether the subscription is authorized.
	// Unauthorized subscriptions do not receive events.
	Authorized bool
}

// InvalidationEventBus is a simple in-process event bus for invalidation
// events. Subscribers receive events after successful commits (not inside
// the transaction). Authorization of subscriptions is enforced: only
// authorized subscribers receive events.
type InvalidationEventBus struct {
	mu            sync.RWMutex
	subscriptions map[string]*EventSubscription
	logger        *slog.Logger
	nextID        int
}

// NewInvalidationEventBus creates a new event bus.
func NewInvalidationEventBus(logger *slog.Logger) *InvalidationEventBus {
	return &InvalidationEventBus{
		subscriptions: make(map[string]*EventSubscription),
		logger:        logger,
	}
}

// Subscribe registers an event handler. Returns a subscription ID that can
// be used to unsubscribe. The subscription is only authorized if authorized
// is true.
func (bus *InvalidationEventBus) Subscribe(principalID string, eventTypes []string, handler InvalidationHandler, authorized bool) string {
	bus.mu.Lock()
	defer bus.mu.Unlock()

	bus.nextID++
	id := fmt.Sprintf("sub_%d", bus.nextID)

	bus.subscriptions[id] = &EventSubscription{
		ID:          id,
		Handler:     handler,
		EventTypes:  eventTypes,
		PrincipalID: principalID,
		Authorized:  authorized,
	}

	bus.logger.Debug("event subscription registered",
		"subscription_id", id,
		"principal_id", principalID,
		"event_types", eventTypes,
		"authorized", authorized,
	)

	return id
}

// Unsubscribe removes a subscription by ID.
func (bus *InvalidationEventBus) Unsubscribe(id string) {
	bus.mu.Lock()
	defer bus.mu.Unlock()
	delete(bus.subscriptions, id)
}

// Publish sends an event to all authorized subscribers whose event type
// filter matches. Events are delivered synchronously in the calling
// goroutine. Panics in handlers are recovered and logged.
func (bus *InvalidationEventBus) Publish(event InvalidationEvent) {
	bus.mu.RLock()
	// Copy subscriptions to release the lock during delivery.
	subs := make([]*EventSubscription, 0, len(bus.subscriptions))
	for _, sub := range bus.subscriptions {
		subs = append(subs, sub)
	}
	bus.mu.RUnlock()

	for _, sub := range subs {
		// Authorization check: unauthorized subscribers do not receive events.
		if !sub.Authorized {
			bus.logger.Debug("skipping unauthorized subscription",
				"subscription_id", sub.ID,
				"principal_id", sub.PrincipalID,
			)
			continue
		}

		// Event type filter: if the subscriber has a type filter, check it.
		if len(sub.EventTypes) > 0 && !eventTypeMatches(event.Type, sub.EventTypes) {
			continue
		}

		// Deliver the event, recovering from panics.
		func() {
			defer func() {
				if r := recover(); r != nil {
					bus.logger.Error("panic in event handler",
						"subscription_id", sub.ID,
						"event_type", event.Type,
						"panic", r,
					)
				}
			}()
			sub.Handler(event)
		}()
	}
}

// SubscriptionCount returns the number of active subscriptions.
func (bus *InvalidationEventBus) SubscriptionCount() int {
	bus.mu.RLock()
	defer bus.mu.RUnlock()
	return len(bus.subscriptions)
}

// eventTypeMatches checks if an event type matches any of the filter types.
func eventTypeMatches(eventType string, filterTypes []string) bool {
	for _, ft := range filterTypes {
		if ft == eventType {
			return true
		}
	}
	return false
}
