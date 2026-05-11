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

package logging

import (
	"context"
	"log/slog"
	"os"

	gcplog "cloud.google.com/go/logging"
)

// Cloud Logging log ID for the dedicated message log.
const MessageLogID = "scion-messages"

// Standard attribute keys for message log entries.
const (
	AttrSender      = "sender"
	AttrSenderID    = "sender_id"
	AttrRecipient   = "recipient"
	AttrRecipientID = "recipient_id"
	AttrMsgType     = "msg_type"
	AttrMsgProjectID  = "grove_id"
)

// MessageLoggerConfig configures the dedicated message logger.
type MessageLoggerConfig struct {
	CloudClient *gcplog.Client // Shared GCP client (nil if not enabled)
	Component   string         // "scion-server", "scion-hub", "scion-broker"
	UseGCP      bool           // Format output as GCP-compatible JSON
	Level       slog.Level
}

// NewMessageLogger creates a dedicated logger for message audit logging.
// When Cloud Logging is enabled, messages are written to a separate
// "scion-messages" log with sender, recipient, and type promoted to GCP labels.
// Returns the logger, a cleanup function, and any error.
func NewMessageLogger(cfg MessageLoggerConfig) (*slog.Logger, func(), error) {
	var handlers []slog.Handler
	var cleanups []func()

	opts := &slog.HandlerOptions{Level: cfg.Level}

	// Cloud handler with dedicated log ID and message-aware label promotion
	if cfg.CloudClient != nil {
		ch := newMessageCloudHandler(cfg.CloudClient, MessageLogID, cfg.Component, cfg.Level)
		handlers = append(handlers, ch)
		cleanups = append(cleanups, func() {
			ch.logger.Flush()
		})
	}

	// Stdout handler for local visibility (always enabled for message log)
	if cfg.UseGCP {
		handlers = append(handlers, NewGCPHandler(os.Stdout, opts, cfg.Component))
	} else {
		handlers = append(handlers, slog.NewJSONHandler(os.Stdout, opts).WithAttrs([]slog.Attr{
			slog.String(AttrComponent, cfg.Component),
		}))
	}

	if len(handlers) == 0 {
		return slog.Default(), nil, nil
	}

	var handler slog.Handler
	if len(handlers) == 1 {
		handler = handlers[0]
	} else {
		handler = newMultiHandler(handlers...)
	}

	cleanup := func() {
		for _, fn := range cleanups {
			fn()
		}
	}

	return slog.New(handler), cleanup, nil
}

// messageCloudHandler extends CloudHandler to promote message-specific
// attributes (sender, recipient, msg_type) to GCP labels.
type messageCloudHandler struct {
	CloudHandler
}

// newMessageCloudHandler creates a CloudHandler for the message log that
// promotes sender, recipient, and msg_type to GCP labels.
func newMessageCloudHandler(client *gcplog.Client, logID, component string, level slog.Level) *messageCloudHandler {
	base := NewCloudHandlerFromClient(client, logID, component, level)
	return &messageCloudHandler{CloudHandler: *base}
}

// Handle overrides CloudHandler.Handle to promote message-specific attributes.
func (h *messageCloudHandler) Handle(ctx context.Context, r slog.Record) error {
	// Build labels with message-specific promotions
	labels := map[string]string{
		"component": h.component,
	}
	if h.hostname != "" {
		labels["hub"] = h.hostname
	}
	for _, a := range h.attrs {
		promoteAttrToLabels(labels, a)
		promoteMessageAttrToLabels(labels, a)
	}
	r.Attrs(func(a slog.Attr) bool {
		promoteAttrToLabels(labels, a)
		promoteMessageAttrToLabels(labels, a)
		return true
	})

	// Use the base CloudHandler logic but with our enriched labels.
	// We rebuild the entry here to include the extra labels.
	return h.handleWithLabels(ctx, r, labels)
}

// handleWithLabels is a copy of CloudHandler.Handle that accepts pre-built labels.
func (h *messageCloudHandler) handleWithLabels(ctx context.Context, r slog.Record, labels map[string]string) error {
	payload := make(map[string]any)
	payload["message"] = r.Message
	payload["component"] = h.component

	target := payload
	for _, group := range h.groups {
		sub := make(map[string]any)
		target[group] = sub
		target = sub
	}
	for _, a := range h.attrs {
		addAttrToMap(target, a)
	}
	r.Attrs(func(a slog.Attr) bool {
		addAttrToMap(target, a)
		return true
	})

	severity := slogLevelToSeverity(r.Level)

	entry := gcplog.Entry{
		Severity:  severity,
		Payload:   payload,
		Labels:    labels,
		Timestamp: r.Time,
	}

	h.logger.Log(entry)
	return nil
}

// WithAttrs implements slog.Handler for messageCloudHandler.
func (h *messageCloudHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	newAttrs := make([]slog.Attr, len(h.attrs)+len(attrs))
	copy(newAttrs, h.attrs)
	copy(newAttrs[len(h.attrs):], attrs)
	return &messageCloudHandler{
		CloudHandler: CloudHandler{
			logger:    h.logger,
			client:    h.client,
			level:     h.level,
			component: h.component,
			hostname:  h.hostname,
			attrs:     newAttrs,
			groups:    h.groups,
		},
	}
}

// WithGroup implements slog.Handler for messageCloudHandler.
func (h *messageCloudHandler) WithGroup(name string) slog.Handler {
	newGroups := make([]string, len(h.groups)+1)
	copy(newGroups, h.groups)
	newGroups[len(h.groups)] = name
	return &messageCloudHandler{
		CloudHandler: CloudHandler{
			logger:    h.logger,
			client:    h.client,
			level:     h.level,
			component: h.component,
			hostname:  h.hostname,
			attrs:     h.attrs,
			groups:    newGroups,
		},
	}
}

// promoteMessageAttrToLabels promotes message-specific attributes to GCP labels.
func promoteMessageAttrToLabels(labels map[string]string, a slog.Attr) {
	switch a.Key {
	case AttrSender, AttrSenderID, AttrRecipient, AttrRecipientID, AttrMsgType, AttrMsgProjectID:
		if v := a.Value.String(); v != "" {
			labels[a.Key] = v
		}
	}
}
