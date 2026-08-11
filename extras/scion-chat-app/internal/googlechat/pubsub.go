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

package googlechat

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"cloud.google.com/go/pubsub"
	"google.golang.org/api/option"

	"github.com/GoogleCloudPlatform/scion/extras/scion-chat-app/internal/chatapp"
)

// PubSubIngress subscribes to a Cloud Pub/Sub subscription and feeds events
// to the same EventHandler used by HTTP mode. All responses are async — the
// handler's EventResponse is converted to Chat API calls rather than being
// returned in an HTTP response body.
type PubSubIngress struct {
	subscription *pubsub.Subscription
	client       *pubsub.Client
	adapter      *Adapter
	log          *slog.Logger
}

// NewPubSubIngress creates a PubSubIngress from a subscription resource name
// (e.g. "projects/my-proj/subscriptions/chat-events"). The resource name is
// parsed to extract the project ID and subscription ID.
func NewPubSubIngress(subscriptionResource string, adapter *Adapter, credentialsFile string, log *slog.Logger) (*PubSubIngress, error) {
	projectID, subID, err := parseSubscriptionResource(subscriptionResource)
	if err != nil {
		return nil, err
	}

	ctx := context.Background()

	var opts []option.ClientOption
	if credentialsFile != "" {
		opts = append(opts, option.WithCredentialsFile(credentialsFile))
	} else if f := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"); f != "" {
		opts = append(opts, option.WithCredentialsFile(f))
	}

	client, err := pubsub.NewClient(ctx, projectID, opts...)
	if err != nil {
		return nil, fmt.Errorf("creating pubsub client for project %s: %w", projectID, err)
	}

	sub := client.Subscription(subID)

	return &PubSubIngress{
		subscription: sub,
		client:       client,
		adapter:      adapter,
		log:          log.With("component", "pubsub-ingress"),
	}, nil
}

// Start begins pulling messages from the Pub/Sub subscription. Each message
// is unmarshalled, normalized via the adapter's normalizeEvent, passed to the
// event handler, and the response is dispatched asynchronously via Chat API
// calls. Messages are acked only after successful processing to preserve
// Pub/Sub's at-least-once delivery guarantees.
func (p *PubSubIngress) Start(ctx context.Context) error {
	p.log.Info("pubsub ingress starting", "subscription", p.subscription.String())
	defer p.client.Close()

	return p.subscription.Receive(ctx, func(ctx context.Context, msg *pubsub.Message) {
		p.log.Debug("pubsub message received",
			"message_id", msg.ID,
			"publish_time", msg.PublishTime,
			"data_len", len(msg.Data),
		)

		// 1. Unmarshal message data as JSON (same format as HTTP body).
		var raw rawEvent
		if err := json.Unmarshal(msg.Data, &raw); err != nil {
			p.log.Error("failed to unmarshal pubsub message", "error", err, "message_id", msg.ID)
			msg.Ack() // Ack malformed messages to prevent redelivery loops.
			return
		}

		// 2. Normalize via adapter.
		event := p.adapter.normalizeEvent(&raw)
		if event == nil {
			p.log.Debug("pubsub event normalized to nil, ignoring", "message_id", msg.ID)
			msg.Ack()
			return
		}

		p.log.Info("pubsub event received",
			"type", event.Type,
			"space", event.SpaceID,
			"user", event.UserID,
			"command", event.Command,
			"message_id", msg.ID,
		)

		// 3. Pass to event handler.
		resp, err := p.adapter.eventHandler(ctx, event)
		if err != nil {
			p.log.Error("event handler error",
				"type", event.Type,
				"error", err,
				"message_id", msg.ID,
			)
			msg.Nack()
			return
		}

		// 4. Convert EventResponse to async API calls.
		if resp != nil {
			if err := p.handleAsyncResponse(ctx, event, resp); err != nil {
				p.log.Error("async response error",
					"type", event.Type,
					"error", err,
					"message_id", msg.ID,
				)
				msg.Nack()
				return
			}
		}

		// 5. Ack after successful processing.
		msg.Ack()
	})
}

// Stop is a no-op — the Pub/Sub client is closed via defer in Start after
// Receive returns, ensuring all in-flight message handlers complete first.
func (p *PubSubIngress) Stop() error {
	return nil
}

// handleAsyncResponse converts a synchronous EventResponse to async Chat API
// calls. In Pub/Sub mode there is no HTTP response body to write, so all
// responses must go through the Chat API.
func (p *PubSubIngress) handleAsyncResponse(ctx context.Context, event *chatapp.ChatEvent, resp *chatapp.EventResponse) error {
	if resp.Message != nil {
		req := *resp.Message
		if req.SpaceID == "" {
			req.SpaceID = event.SpaceID
		}
		if req.ThreadID == "" {
			req.ThreadID = event.ThreadID
		}
		_, err := p.adapter.SendMessage(ctx, req)
		if err != nil {
			return fmt.Errorf("sending async message: %w", err)
		}
		return nil
	}

	if resp.UpdateMessage != nil {
		if event.MessageName == "" {
			p.log.Warn("update message requested but no message name available",
				"space", event.SpaceID,
				"type", event.Type,
			)
			// Fall back to sending a new message instead.
			req := *resp.UpdateMessage
			if req.SpaceID == "" {
				req.SpaceID = event.SpaceID
			}
			if req.ThreadID == "" {
				req.ThreadID = event.ThreadID
			}
			_, err := p.adapter.SendMessage(ctx, req)
			return err
		}
		return p.adapter.UpdateMessage(ctx, event.MessageName, *resp.UpdateMessage)
	}

	if resp.Dialog != nil {
		// Dialogs are not supported in Pub/Sub mode — Phase 6 migrated all
		// dialogs to cards. Log a warning and send the dialog fields as a card.
		p.log.Warn("dialog response in pubsub mode -- dialogs are not supported, sending as card",
			"title", resp.Dialog.Title,
			"space", event.SpaceID,
		)
		card := dialogToCard(resp.Dialog)
		_, err := p.adapter.SendMessage(ctx, chatapp.SendMessageRequest{
			SpaceID:  event.SpaceID,
			ThreadID: event.ThreadID,
			Card:     &card,
		})
		return err
	}

	if resp.CloseDialog {
		// Nothing to do — no dialog to close in Pub/Sub mode.
		p.log.Debug("close dialog response ignored in pubsub mode")
	}

	return nil
}

// dialogToCard converts a Dialog to a Card for use when a dialog response
// arrives in Pub/Sub mode (which does not support synchronous dialogs).
func dialogToCard(d *chatapp.Dialog) chatapp.Card {
	card := chatapp.Card{
		Header: chatapp.CardHeader{
			Title: d.Title,
		},
	}

	var widgets []chatapp.Widget
	for _, f := range d.Fields {
		switch f.Type {
		case "text", "textarea":
			widgets = append(widgets, chatapp.Widget{
				Type:     chatapp.WidgetInput,
				Label:    f.Label,
				ActionID: f.ID,
			})
		case "select", "checkbox":
			widgets = append(widgets, chatapp.Widget{
				Type:     chatapp.WidgetCheckbox,
				Label:    f.Label,
				ActionID: f.ID,
				Options:  f.Options,
			})
		default:
			widgets = append(widgets, chatapp.Widget{
				Type:    chatapp.WidgetText,
				Label:   f.Label,
				Content: f.Placeholder,
			})
		}
	}

	if len(widgets) > 0 {
		card.Sections = []chatapp.CardSection{
			{Widgets: widgets},
		}
	}

	return card
}

// parseSubscriptionResource extracts the project ID and subscription ID from
// a full resource name like "projects/my-proj/subscriptions/chat-events".
func parseSubscriptionResource(resource string) (projectID, subscriptionID string, err error) {
	parts := strings.Split(resource, "/")
	if len(parts) != 4 || parts[0] != "projects" || parts[2] != "subscriptions" {
		return "", "", fmt.Errorf("invalid subscription resource name %q: expected projects/{project}/subscriptions/{subscription}", resource)
	}
	return parts[1], parts[3], nil
}
