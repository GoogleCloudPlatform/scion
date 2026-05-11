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
	"encoding/json"
	"net/url"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// NotificationService handles notification operations.
type NotificationService interface {
	// List returns notifications for the current user.
	List(ctx context.Context, opts *ListNotificationsOptions) ([]Notification, error)

	// Acknowledge marks a single notification as acknowledged.
	Acknowledge(ctx context.Context, id string) error

	// AcknowledgeAll marks all unacknowledged notifications as acknowledged.
	AcknowledgeAll(ctx context.Context) error
}

// notificationService is the implementation of NotificationService.
type notificationService struct {
	c *client
}

// ListNotificationsOptions configures notification listing.
type ListNotificationsOptions struct {
	OnlyUnacknowledged bool
}

// Notification represents a notification from the Hub API.
type Notification struct {
	ID             string    `json:"id"`
	SubscriptionID string    `json:"subscriptionId"`
	AgentID        string    `json:"agentId"`
	ProjectID      string    `json:"projectId"`
	SubscriberType string    `json:"subscriberType"`
	SubscriberID   string    `json:"subscriberId"`
	Status         string    `json:"status"`
	Message        string    `json:"message"`
	Dispatched     bool      `json:"dispatched"`
	Acknowledged   bool      `json:"acknowledged"`
	CreatedAt      time.Time `json:"createdAt"`
}

// UnmarshalJSON implements custom unmarshaling to support legacy groveId field.
func (n *Notification) UnmarshalJSON(data []byte) error {
	type Alias Notification
	aux := &struct {
		GroveID string `json:"groveId"`
		*Alias
	}{
		Alias: (*Alias)(n),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if n.ProjectID == "" && aux.GroveID != "" {
		n.ProjectID = aux.GroveID
	}
	return nil
}

// MarshalJSON implements custom marshaling to support legacy groveId field.
func (n Notification) MarshalJSON() ([]byte, error) {
	type Alias Notification
	return json.Marshal(&struct {
		Alias
		GroveID string `json:"groveId,omitempty"`
	}{
		Alias:   Alias(n),
		GroveID: n.ProjectID,
	})
}

// List returns notifications for the current user.
func (s *notificationService) List(ctx context.Context, opts *ListNotificationsOptions) ([]Notification, error) {
	query := url.Values{}
	if opts != nil && !opts.OnlyUnacknowledged {
		query.Set("acknowledged", "true")
	}

	resp, err := s.c.getWithQuery(ctx, "/api/v1/notifications", query, nil)
	if err != nil {
		return nil, err
	}
	result, err := apiclient.DecodeResponse[[]Notification](resp)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return []Notification{}, nil
	}
	return *result, nil
}

// Acknowledge marks a single notification as acknowledged.
func (s *notificationService) Acknowledge(ctx context.Context, id string) error {
	resp, err := s.c.post(ctx, "/api/v1/notifications/"+url.PathEscape(id)+"/ack", nil, nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}

// AcknowledgeAll marks all unacknowledged notifications as acknowledged.
func (s *notificationService) AcknowledgeAll(ctx context.Context) error {
	resp, err := s.c.post(ctx, "/api/v1/notifications/ack-all", nil, nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}

// SubscriptionService handles notification subscription operations.
type SubscriptionService interface {
	// Create creates a new notification subscription.
	Create(ctx context.Context, req *CreateSubscriptionRequest) (*Subscription, error)

	// List returns subscriptions for the current user.
	List(ctx context.Context, opts *ListSubscriptionsOptions) ([]Subscription, error)

	// Update modifies the trigger activities of a subscription.
	Update(ctx context.Context, id string, req *UpdateSubscriptionRequest) (*Subscription, error)

	// Delete removes a subscription by ID.
	Delete(ctx context.Context, id string) error

	// BulkCreate creates multiple subscriptions in a single request.
	BulkCreate(ctx context.Context, reqs []CreateSubscriptionRequest) ([]Subscription, error)

	// BulkDelete removes multiple subscriptions by ID in a single request.
	BulkDelete(ctx context.Context, ids []string) (int, error)
}

// subscriptionService is the implementation of SubscriptionService.
type subscriptionService struct {
	c *client
}

// CreateSubscriptionRequest is the request body for creating a subscription.
type CreateSubscriptionRequest struct {
	Scope             string   `json:"scope"`
	AgentID           string   `json:"agentId,omitempty"`
	ProjectID         string   `json:"projectId"`
	TriggerActivities []string `json:"triggerActivities"`
}

// MarshalJSON implements custom marshaling to support legacy groveId field.
func (r CreateSubscriptionRequest) MarshalJSON() ([]byte, error) {
	type Alias CreateSubscriptionRequest
	return json.Marshal(&struct {
		Alias
		GroveID string `json:"groveId,omitempty"`
	}{
		Alias:   Alias(r),
		GroveID: r.ProjectID,
	})
}

// UnmarshalJSON implements custom unmarshaling to support legacy groveId field.
func (r *CreateSubscriptionRequest) UnmarshalJSON(data []byte) error {
	type Alias CreateSubscriptionRequest
	aux := &struct {
		GroveID string `json:"groveId"`
		*Alias
	}{
		Alias: (*Alias)(r),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if r.ProjectID == "" && aux.GroveID != "" {
		r.ProjectID = aux.GroveID
	}
	return nil
}

// UpdateSubscriptionRequest is the request body for updating a subscription.
type UpdateSubscriptionRequest struct {
	TriggerActivities []string `json:"triggerActivities"`
}

// ListSubscriptionsOptions configures subscription listing.
type ListSubscriptionsOptions struct {
	ProjectID string
	AgentID   string
	Scope     string
}

// Subscription represents a notification subscription from the Hub API.
type Subscription struct {
	ID                string    `json:"id"`
	Scope             string    `json:"scope"`
	AgentID           string    `json:"agentId,omitempty"`
	SubscriberType    string    `json:"subscriberType"`
	SubscriberID      string    `json:"subscriberId"`
	ProjectID         string    `json:"projectId"`
	TriggerActivities []string  `json:"triggerActivities"`
	CreatedAt         time.Time `json:"createdAt"`
	CreatedBy         string    `json:"createdBy"`
}

// UnmarshalJSON implements custom unmarshaling to support legacy groveId field.
func (s *Subscription) UnmarshalJSON(data []byte) error {
	type Alias Subscription
	aux := &struct {
		GroveID string `json:"groveId"`
		*Alias
	}{
		Alias: (*Alias)(s),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if s.ProjectID == "" && aux.GroveID != "" {
		s.ProjectID = aux.GroveID
	}
	return nil
}

// MarshalJSON implements custom marshaling to support legacy groveId field.
func (s Subscription) MarshalJSON() ([]byte, error) {
	type Alias Subscription
	return json.Marshal(&struct {
		Alias
		GroveID string `json:"groveId,omitempty"`
	}{
		Alias:   Alias(s),
		GroveID: s.ProjectID,
	})
}

// Create creates a new notification subscription.
func (s *subscriptionService) Create(ctx context.Context, req *CreateSubscriptionRequest) (*Subscription, error) {
	resp, err := s.c.post(ctx, "/api/v1/notifications/subscriptions", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[Subscription](resp)
}

// List returns subscriptions for the current user.
func (s *subscriptionService) List(ctx context.Context, opts *ListSubscriptionsOptions) ([]Subscription, error) {
	query := url.Values{}
	if opts != nil {
		if opts.ProjectID != "" {
			query.Set("projectId", opts.ProjectID)
			query.Set("groveId", opts.ProjectID)
		}
		if opts.AgentID != "" {
			query.Set("agentId", opts.AgentID)
		}
		if opts.Scope != "" {
			query.Set("scope", opts.Scope)
		}
	}

	resp, err := s.c.getWithQuery(ctx, "/api/v1/notifications/subscriptions", query, nil)
	if err != nil {
		return nil, err
	}
	result, err := apiclient.DecodeResponse[[]Subscription](resp)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return []Subscription{}, nil
	}
	return *result, nil
}

// Update modifies the trigger activities of a subscription.
func (s *subscriptionService) Update(ctx context.Context, id string, req *UpdateSubscriptionRequest) (*Subscription, error) {
	resp, err := s.c.patch(ctx, "/api/v1/notifications/subscriptions/"+url.PathEscape(id), req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[Subscription](resp)
}

// Delete removes a subscription by ID.
func (s *subscriptionService) Delete(ctx context.Context, id string) error {
	resp, err := s.c.delete(ctx, "/api/v1/notifications/subscriptions/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}

// BulkCreate creates multiple subscriptions in a single request.
func (s *subscriptionService) BulkCreate(ctx context.Context, reqs []CreateSubscriptionRequest) ([]Subscription, error) {
	resp, err := s.c.post(ctx, "/api/v1/notifications/subscriptions/bulk", reqs, nil)
	if err != nil {
		return nil, err
	}
	result, err := apiclient.DecodeResponse[[]Subscription](resp)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return []Subscription{}, nil
	}
	return *result, nil
}

// SubscriptionTemplateService handles subscription template operations.
type SubscriptionTemplateService interface {
	// Create creates a new subscription template.
	Create(ctx context.Context, req *CreateSubscriptionTemplateRequest) (*SubscriptionTemplate, error)

	// List returns subscription templates, optionally filtered by project.
	List(ctx context.Context, projectID string) ([]SubscriptionTemplate, error)

	// Delete removes a template by ID.
	Delete(ctx context.Context, id string) error
}

// subscriptionTemplateService is the implementation of SubscriptionTemplateService.
type subscriptionTemplateService struct {
	c *client
}

// CreateSubscriptionTemplateRequest is the request body for creating a subscription template.
type CreateSubscriptionTemplateRequest struct {
	Name              string   `json:"name"`
	Scope             string   `json:"scope"`
	TriggerActivities []string `json:"triggerActivities"`
	ProjectID         string   `json:"groveId"`
}

// SubscriptionTemplate represents a subscription template from the Hub API.
type SubscriptionTemplate struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	Scope             string   `json:"scope"`
	TriggerActivities []string `json:"triggerActivities"`
	ProjectID         string   `json:"groveId"`
	CreatedBy         string   `json:"createdBy"`
}

// Create creates a new subscription template.
func (s *subscriptionTemplateService) Create(ctx context.Context, req *CreateSubscriptionTemplateRequest) (*SubscriptionTemplate, error) {
	resp, err := s.c.post(ctx, "/api/v1/notifications/templates", req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeResponse[SubscriptionTemplate](resp)
}

// List returns subscription templates, optionally filtered by project.
func (s *subscriptionTemplateService) List(ctx context.Context, projectID string) ([]SubscriptionTemplate, error) {
	query := url.Values{}
	if projectID != "" {
		query.Set("projectId", projectID)
		query.Set("groveId", projectID)
	}
	resp, err := s.c.getWithQuery(ctx, "/api/v1/notifications/templates", query, nil)
	if err != nil {
		return nil, err
	}
	result, err := apiclient.DecodeResponse[[]SubscriptionTemplate](resp)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return []SubscriptionTemplate{}, nil
	}
	return *result, nil
}

// Delete removes a template by ID.
func (s *subscriptionTemplateService) Delete(ctx context.Context, id string) error {
	resp, err := s.c.delete(ctx, "/api/v1/notifications/templates/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}

// BulkDelete removes multiple subscriptions by ID in a single request.
func (s *subscriptionService) BulkDelete(ctx context.Context, ids []string) (int, error) {
	body := struct {
		IDs []string `json:"ids"`
	}{IDs: ids}
	resp, err := s.c.post(ctx, "/api/v1/notifications/subscriptions/bulk-delete", body, nil)
	if err != nil {
		return 0, err
	}
	result, err := apiclient.DecodeResponse[struct {
		Deleted int `json:"deleted"`
	}](resp)
	if err != nil {
		return 0, err
	}
	return result.Deleted, nil
}
