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

	gcplog "cloud.google.com/go/logging"
	"github.com/GoogleCloudPlatform/scion/pkg/util/cloudrunenv"
)

// ArtifactLogID is the Cloud Logging log ID of the dedicated artifact
// decision log (ptone/scion#5120).
const ArtifactLogID = "scion-artifacts"

// Attribute keys of artifact log entries that are promoted to Cloud
// Logging labels: the cheap, low-cardinality-enough fields an operator
// filters on. pkg/artifacts writes the entries with the same keys (it does
// not import this package; a test pins that the keys agree).
const (
	AttrArtifactID     = "artifact_id"
	AttrArtifactAction = "action"
	AttrArtifactResult = "outcome"
	AttrArtifactReason = "reason"
	AttrPrincipalKind  = "principal_kind"
	AttrPrincipalID    = "principal_id"
)

// ArtifactLoggerConfig configures the dedicated artifact logger.
type ArtifactLoggerConfig struct {
	CloudClient *gcplog.Client // Shared GCP client (nil if not enabled)
	CircuitOpen func() bool    // Returns true when circuit breaker is open (nil = never open)
	Component   string         // "scion-server", "scion-hub"
	HubName     string         // Logical hub identity for log labels
	HubID       string         // Stable unique hub instance ID for log labels
	UseGCP      bool           // Format output as GCP-compatible JSON
	Level       slog.Leveler   // Floor, consulted per record (nil = info)
}

// NewArtifactLogger creates the dedicated logger for artifact decisions.
// When Cloud Logging is enabled, entries go to the separate
// "scion-artifacts" log with the artifact id, project, action, outcome,
// reason and principal promoted to labels, and also to stdout except on
// Cloud Run (whose runtime forwards stdout). Without Cloud Logging they go
// to stdout only, as the message log does. An entry logged with a
// request's context carries that request's id, so it joins to the request
// log.
// Returns the logger, a cleanup function, and any error.
func NewArtifactLogger(cfg ArtifactLoggerConfig) (*slog.Logger, func(), error) {
	var handlers []slog.Handler
	var cleanups []func()

	opts := &slog.HandlerOptions{Level: cfg.Level}

	if cfg.CloudClient != nil {
		ch := newArtifactCloudHandler(cfg.CloudClient, ArtifactLogID, cfg.Component, cfg.HubName, cfg.HubID, cfg.Level)
		var cloudHandler slog.Handler = ch
		if cfg.CircuitOpen != nil {
			cloudHandler = &circuitGatedHandler{inner: ch, circuitOpen: cfg.CircuitOpen}
		}
		handlers = append(handlers, cloudHandler)
		cleanups = append(cleanups, func() {
			if err := ch.logger.Flush(); err != nil {
				CloudWriter().RecordFailure(CloudReasonFlushError)
			}
		})
	}

	// Stdout handler for local visibility, skipped on Cloud Run when the
	// cloud handler is active (the runtime forwards stdout to Cloud
	// Logging, so it would duplicate every entry).
	if !cloudrunenv.IsService() || cfg.CloudClient == nil {
		handlers = append(handlers, newStdoutHandler(logStdout, opts, cfg.Component, cfg.HubName, cfg.UseGCP, true))
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

	return slog.New(NewRequestIDHandler(handler)), cleanup, nil
}

// NewRequestIDHandler wraps inner so every record logged with a request
// context carries that request's id (RequestIDFromContext) under
// AttrRequestID, unless it already has one.
func NewRequestIDHandler(inner slog.Handler) slog.Handler {
	return &requestIDHandler{inner: inner}
}

// requestIDHandler adds the request id of the record's context (see
// RequestIDFromContext) to every record that does not carry one.
type requestIDHandler struct {
	inner slog.Handler
}

func (h *requestIDHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *requestIDHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := RequestIDFromContext(ctx); id != "" {
		has := false
		r.Attrs(func(a slog.Attr) bool {
			has = a.Key == AttrRequestID
			return !has
		})
		if !has {
			r = r.Clone()
			r.AddAttrs(slog.String(AttrRequestID, id))
		}
	}
	return h.inner.Handle(ctx, r)
}

func (h *requestIDHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &requestIDHandler{inner: h.inner.WithAttrs(attrs)}
}

func (h *requestIDHandler) WithGroup(name string) slog.Handler {
	return &requestIDHandler{inner: h.inner.WithGroup(name)}
}

// artifactCloudHandler extends CloudHandler to promote the artifact log's
// fields to GCP labels.
type artifactCloudHandler struct {
	CloudHandler
}

func newArtifactCloudHandler(client *gcplog.Client, logID, component, hubName, hubID string, level slog.Leveler) *artifactCloudHandler {
	base := NewCloudHandlerFromClient(client, logID, component, hubName, hubID, level)
	return &artifactCloudHandler{CloudHandler: *base}
}

// Handle implements slog.Handler.
func (h *artifactCloudHandler) Handle(_ context.Context, r slog.Record) error {
	labels := map[string]string{
		"component": h.component,
	}
	addHubLabel(labels, h.hubName)
	if h.hubID != "" {
		labels["hub_id"] = h.hubID
	}
	if h.hostname != "" {
		labels["node"] = h.hostname
	}

	payload := make(map[string]any)
	payload["message"] = r.Message
	payload["component"] = h.component
	target := payload
	for _, group := range h.groups {
		sub := make(map[string]any)
		target[group] = sub
		target = sub
	}
	add := func(a slog.Attr) bool {
		addAttrToMap(target, a)
		promoteAttrToLabels(labels, a)
		promoteArtifactAttrToLabels(labels, a)
		return true
	}
	for _, a := range h.attrs {
		add(a)
	}
	r.Attrs(add)

	entry := gcplog.Entry{
		Severity:  slogLevelToSeverity(r.Level),
		Payload:   payload,
		Labels:    labels,
		Timestamp: r.Time,
	}
	if h.logHook != nil {
		h.logHook(entry)
		return nil
	}
	h.logger.Log(entry)
	return nil
}

// WithAttrs implements slog.Handler.
func (h *artifactCloudHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	base := h.CloudHandler.WithAttrs(attrs).(*CloudHandler)
	return &artifactCloudHandler{CloudHandler: *base}
}

// WithGroup implements slog.Handler.
func (h *artifactCloudHandler) WithGroup(name string) slog.Handler {
	base := h.CloudHandler.WithGroup(name).(*CloudHandler)
	return &artifactCloudHandler{CloudHandler: *base}
}

// promoteArtifactAttrToLabels promotes the artifact log's label fields.
func promoteArtifactAttrToLabels(labels map[string]string, a slog.Attr) {
	switch a.Key {
	case AttrArtifactID, AttrArtifactAction, AttrArtifactResult, AttrArtifactReason,
		AttrPrincipalKind, AttrPrincipalID:
		if v := a.Value.String(); v != "" {
			labels[a.Key] = v
		}
	}
}
