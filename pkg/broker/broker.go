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

// Package broker provides the message broker abstraction for Scion's messaging system.
// The broker routes structured messages between agents, users, and system components
// using topic-based publish/subscribe with NATS-style subject matching.
//
// Topic hierarchy:
//
//	scion.grove.<grove-id>.agent.<agent-slug>.messages   - direct messages to an agent
//	scion.grove.<grove-id>.broadcast                      - grove-wide broadcasts
//	scion.global.broadcast                                - global broadcasts
package broker

import (
	"context"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// MessageBroker abstracts message routing and delivery.
// Implementations range from in-process (Go channels) to external systems (NATS, Redis).
type MessageBroker interface {
	// Publish sends a structured message to a topic.
	Publish(ctx context.Context, topic string, msg *messages.StructuredMessage) error

	// Subscribe registers a handler for messages matching a topic pattern.
	// Patterns use NATS-style wildcards: * matches a single token, > matches the remainder.
	// Returns a Subscription that can be used to unsubscribe.
	Subscribe(pattern string, handler MessageHandler) (Subscription, error)

	// Close shuts down the broker and releases resources.
	Close() error
}

// MessageHandler is a callback function invoked when a message is received on a subscribed topic.
type MessageHandler func(ctx context.Context, topic string, msg *messages.StructuredMessage)

// Subscription represents an active subscription that can be cancelled.
type Subscription interface {
	Unsubscribe() error
}

// Topic helper functions for constructing well-known topic strings.

// TopicAgentMessages returns the topic for direct messages to an agent.
func TopicAgentMessages(groveID, agentSlug string) string {
	return "scion.grove." + groveID + ".agent." + agentSlug + ".messages"
}

// TopicProjectBroadcast returns the topic for project-wide broadcast messages.
func TopicProjectBroadcast(projectID string) string {
	return "scion.grove." + projectID + ".broadcast"
}

// TopicGlobalBroadcast returns the topic for global broadcast messages.
func TopicGlobalBroadcast() string {
	return "scion.global.broadcast"
}

// TopicAllAgentMessages returns a wildcard pattern matching all agent message
// topics in a grove.
func TopicAllAgentMessages(groveID string) string {
	return "scion.grove." + groveID + ".agent.*.messages"
}

// TopicUserMessages returns the topic for messages directed at a specific user in a grove.
func TopicUserMessages(groveID, userID string) string {
	return "scion.grove." + groveID + ".user." + userID + ".messages"
}

// TopicAllUserMessages returns a wildcard pattern matching all user message
// topics in a grove.
func TopicAllUserMessages(groveID string) string {
	return "scion.grove." + groveID + ".user.*.messages"
}
