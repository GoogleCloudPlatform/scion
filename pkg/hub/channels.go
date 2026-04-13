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
	"fmt"
	"log/slog"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// NotificationChannel delivers notifications to external systems.
type NotificationChannel interface {
	// Name returns the channel identifier (e.g., "webhook", "slack").
	Name() string

	// Deliver sends a notification via this channel.
	Deliver(ctx context.Context, msg *messages.StructuredMessage) error

	// Validate checks that the channel configuration is valid.
	Validate() error
}

// ChannelConfig holds channel-specific configuration loaded from settings.
type ChannelConfig struct {
	Type             string            `json:"type" yaml:"type" koanf:"type"`
	Params           map[string]string `json:"params,omitempty" yaml:"params,omitempty" koanf:"params"`
	FilterTypes      []string          `json:"filter_types,omitempty" yaml:"filter_types,omitempty" koanf:"filter_types"`
	FilterUrgentOnly bool              `json:"filter_urgent_only,omitempty" yaml:"filter_urgent_only,omitempty" koanf:"filter_urgent_only"`
}

// ChannelRegistry holds configured notification channels and dispatches
// notifications to all matching channels. Channel dispatch is fire-and-forget;
// delivery failures are logged but do not block the notification pipeline.
type ChannelRegistry struct {
	channels []NotificationChannel
	configs  []ChannelConfig
	log      *slog.Logger
}

// NewChannelRegistry creates a ChannelRegistry from a list of channel configurations.
// Each config is used to instantiate the appropriate channel implementation.
// Invalid or unknown channel types are logged and skipped.
func NewChannelRegistry(configs []ChannelConfig, log *slog.Logger) *ChannelRegistry {
	r := &ChannelRegistry{
		configs: configs,
		log:     log,
	}

	for i, cfg := range configs {
		ch, err := newChannelFromConfig(cfg)
		if err != nil {
			log.Warn("Skipping invalid notification channel",
				"index", i, "type", cfg.Type, "error", err)
			continue
		}
		if err := ch.Validate(); err != nil {
			log.Warn("Skipping notification channel with invalid config",
				"index", i, "type", cfg.Type, "error", err)
			continue
		}
		r.channels = append(r.channels, ch)
		log.Info("Notification channel registered", "type", cfg.Type)
	}

	return r
}

// Dispatch sends a structured message to all registered channels whose
// filters match the message. Delivery is fire-and-forget with logging.
func (r *ChannelRegistry) Dispatch(ctx context.Context, msg *messages.StructuredMessage) {
	for i, ch := range r.channels {
		cfg := r.configs[i]
		if !r.matchesFilter(cfg, msg) {
			continue
		}
		if err := ch.Deliver(ctx, msg); err != nil {
			r.log.Error("Notification channel delivery failed",
				"channel", ch.Name(), "error", err,
				"sender", msg.Sender, "recipient", msg.Recipient, "msg_type", msg.Type)
		} else {
			r.log.Debug("Notification channel delivery succeeded",
				"channel", ch.Name(), "sender", msg.Sender, "msg_type", msg.Type)
		}
	}
}

// Len returns the number of registered channels.
func (r *ChannelRegistry) Len() int {
	return len(r.channels)
}

// matchesFilter checks whether a message passes the channel's filter criteria.
func (r *ChannelRegistry) matchesFilter(cfg ChannelConfig, msg *messages.StructuredMessage) bool {
	if cfg.FilterUrgentOnly && !msg.Urgent {
		return false
	}
	if len(cfg.FilterTypes) > 0 {
		matched := false
		for _, ft := range cfg.FilterTypes {
			if strings.EqualFold(ft, msg.Type) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

// newChannelFromConfig creates a NotificationChannel from a ChannelConfig.
func newChannelFromConfig(cfg ChannelConfig) (NotificationChannel, error) {
	switch cfg.Type {
	case "discord":
		return NewDiscordChannel(cfg.Params), nil
	case "email":
		return NewEmailChannel(cfg.Params), nil
	case "slack":
		return NewSlackChannel(cfg.Params), nil
	case "webhook":
		return NewWebhookChannel(cfg.Params), nil
	default:
		return nil, fmt.Errorf("unknown notification channel type: %q", cfg.Type)
	}
}
