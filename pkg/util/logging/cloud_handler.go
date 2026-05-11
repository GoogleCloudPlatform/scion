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
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"time"

	gcplog "cloud.google.com/go/logging"
	logpb "cloud.google.com/go/logging/apiv2/loggingpb"
)

// Environment variable names for Cloud Logging configuration.
const (
	EnvCloudLogging       = "SCION_CLOUD_LOGGING"
	EnvCloudLoggingLogID  = "SCION_CLOUD_LOGGING_LOG_ID"
	EnvGCPProjectID       = "SCION_GCP_PROJECT_ID"
	EnvGoogleCloudProject = "GOOGLE_CLOUD_PROJECT"
)

// CloudLoggingConfig holds configuration for direct Cloud Logging.
type CloudLoggingConfig struct {
	// ProjectID is the GCP project ID.
	ProjectID string
	// LogID is the log name within Cloud Logging.
	LogID string
	// Component is the server component name (e.g., "scion-hub").
	Component string
}

// CloudHandler is a slog.Handler that sends log entries directly to
// Google Cloud Logging using the client library.
type CloudHandler struct {
	logger    *gcplog.Logger
	client    *gcplog.Client
	level     slog.Level
	component string
	hostname  string
	projectID string
	attrs     []slog.Attr
	groups    []string
}

// NewCloudHandler creates a new CloudHandler that sends logs to Cloud Logging.
// Returns the handler, a cleanup function to flush and close the client, and any error.
func NewCloudHandler(ctx context.Context, config CloudLoggingConfig, level slog.Level) (*CloudHandler, func(), error) {
	projectID := config.ProjectID
	if projectID == "" {
		projectID = resolveProjectID()
	}
	if projectID == "" {
		return nil, nil, fmt.Errorf("GCP project ID is required: set SCION_GCP_PROJECT_ID or GOOGLE_CLOUD_PROJECT")
	}

	logID := config.LogID
	if logID == "" {
		logID = resolveLogID()
	}

	client, err := gcplog.NewClient(ctx, projectID)
	if err != nil {
		return nil, nil, fmt.Errorf("creating Cloud Logging client: %w", err)
	}

	logger := client.Logger(logID)

	hostname, _ := os.Hostname()

	handler := &CloudHandler{
		logger:    logger,
		client:    client,
		level:     level,
		component: config.Component,
		hostname:  hostname,
		projectID: projectID,
	}

	cleanup := func() {
		if err := logger.Flush(); err != nil {
			fmt.Fprintf(os.Stderr, "error flushing Cloud Logging: %v\n", err)
		}
		if err := client.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "error closing Cloud Logging client: %v\n", err)
		}
	}

	return handler, cleanup, nil
}

// Enabled implements slog.Handler.
func (h *CloudHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

// Handle implements slog.Handler.
func (h *CloudHandler) Handle(_ context.Context, r slog.Record) error {
	// Build the payload map
	payload := make(map[string]any)
	payload["message"] = r.Message
	payload["component"] = h.component

	// Add pre-configured attrs
	target := payload
	for _, group := range h.groups {
		sub := make(map[string]any)
		target[group] = sub
		target = sub
	}
	for _, a := range h.attrs {
		addAttrToMap(target, a)
	}

	// Add record attrs
	r.Attrs(func(a slog.Attr) bool {
		addAttrToMap(target, a)
		return true
	})

	// Build source location
	var sourceLocation *logpb.LogEntrySourceLocation
	if r.PC != 0 {
		fs := runtime.CallersFrames([]uintptr{r.PC})
		f, _ := fs.Next()
		sourceLocation = &logpb.LogEntrySourceLocation{
			File:     f.File,
			Line:     int64(f.Line),
			Function: f.Function,
		}
	}

	// Map slog level to Cloud Logging severity
	severity := slogLevelToSeverity(r.Level)

	// Build labels, promoting agent_id/grove_id from attrs
	labels := map[string]string{
		"component": h.component,
	}
	if h.hostname != "" {
		labels["hub"] = h.hostname
	}
	for _, a := range h.attrs {
		promoteAttrToLabels(labels, a)
	}
	r.Attrs(func(a slog.Attr) bool {
		promoteAttrToLabels(labels, a)
		return true
	})

	entry := gcplog.Entry{
		Severity:       severity,
		Payload:        payload,
		SourceLocation: sourceLocation,
		Labels:         labels,
		Timestamp:      r.Time,
	}
	if traceID, ok := payload[AttrTraceID].(string); ok {
		if normalized := NormalizeTraceID(traceID); normalized != "" {
			payload[AttrTraceID] = normalized
			entry.Trace = FormatCloudTraceResource(h.projectID, normalized)
			entry.Labels[gcpTraceIDLabelKey] = normalized
		}
	}

	// Promote httpRequest to top-level entry field if present.
	if reqMap, ok := payload["httpRequest"].(map[string]any); ok {
		entry.HTTPRequest = mapToCloudHTTPRequest(reqMap)
		delete(payload, "httpRequest")
		delete(payload, "message")
	}

	h.logger.Log(entry)
	return nil
}

// WithAttrs implements slog.Handler.
func (h *CloudHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	newAttrs := make([]slog.Attr, len(h.attrs)+len(attrs))
	copy(newAttrs, h.attrs)
	copy(newAttrs[len(h.attrs):], attrs)
	return &CloudHandler{
		logger:    h.logger,
		client:    h.client,
		level:     h.level,
		component: h.component,
		hostname:  h.hostname,
		projectID: h.projectID,
		attrs:     newAttrs,
		groups:    h.groups,
	}
}

// WithGroup implements slog.Handler.
func (h *CloudHandler) WithGroup(name string) slog.Handler {
	newGroups := make([]string, len(h.groups)+1)
	copy(newGroups, h.groups)
	newGroups[len(h.groups)] = name
	return &CloudHandler{
		logger:    h.logger,
		client:    h.client,
		level:     h.level,
		component: h.component,
		hostname:  h.hostname,
		projectID: h.projectID,
		attrs:     h.attrs,
		groups:    newGroups,
	}
}

// Client returns the underlying Cloud Logging client.
// This allows reuse of the same client connection for multiple loggers.
func (h *CloudHandler) Client() *gcplog.Client {
	return h.client
}

// NewCloudHandlerFromClient creates a CloudHandler from an existing client.
// This avoids opening a second connection for the request log stream.
func NewCloudHandlerFromClient(client *gcplog.Client, logID, component string, level slog.Level) *CloudHandler {
	logger := client.Logger(logID)
	hostname, _ := os.Hostname()
	return &CloudHandler{
		logger:    logger,
		client:    client,
		level:     level,
		component: component,
		hostname:  hostname,
		projectID: resolveProjectID(),
	}
}

// slogLevelToSeverity maps slog.Level to Cloud Logging severity.
func slogLevelToSeverity(level slog.Level) gcplog.Severity {
	switch {
	case level >= slog.LevelError:
		return gcplog.Error
	case level >= slog.LevelWarn:
		return gcplog.Warning
	case level >= slog.LevelInfo:
		return gcplog.Info
	default:
		return gcplog.Debug
	}
}

// addAttrToMap adds a slog.Attr to a map, handling nested groups.
func addAttrToMap(m map[string]any, a slog.Attr) {
	val := a.Value.Resolve()
	if val.Kind() == slog.KindGroup {
		sub := make(map[string]any)
		for _, ga := range val.Group() {
			addAttrToMap(sub, ga)
		}
		if a.Key == "" {
			// Inline group (no key)
			for k, v := range sub {
				m[k] = v
			}
		} else {
			m[a.Key] = sub
		}
		return
	}
	m[a.Key] = val.Any()
}

// promoteAttrToLabels checks if a slog.Attr should be promoted to a GCP label.
func promoteAttrToLabels(labels map[string]string, a slog.Attr) {
	switch a.Key {
	case AttrAgentID, AttrProjectID, AttrBrokerID:
		if v := a.Value.String(); v != "" {
			labels[a.Key] = v
		}
	}
}

// resolveProjectID returns the GCP project ID from environment variables.
// Priority: SCION_GCP_PROJECT_ID > GOOGLE_CLOUD_PROJECT
func resolveProjectID() string {
	if v := os.Getenv(EnvGCPProjectID); v != "" {
		return v
	}
	return os.Getenv(EnvGoogleCloudProject)
}

// resolveLogID returns the Cloud Logging log ID from environment variables.
// Defaults to "scion-server" if not set.
func resolveLogID() string {
	if v := os.Getenv(EnvCloudLoggingLogID); v != "" {
		return v
	}
	return "scion-server"
}

// isCloudLoggingEnabled checks if direct Cloud Logging is enabled via env var.
func isCloudLoggingEnabled() bool {
	val := os.Getenv(EnvCloudLogging)
	return val == "true" || val == "1" || val == "yes"
}

// IsCloudLoggingEnabled is the exported version for use in cmd/.
func IsCloudLoggingEnabled() bool {
	return isCloudLoggingEnabled()
}

// ResolveProjectID returns the GCP project ID from environment variables.
// Returns empty string if no project is configured.
func ResolveProjectID() string {
	return resolveProjectID()
}

// ResolveLogLevel returns the slog.Level based on the debug flag and env var.
func ResolveLogLevel(debug bool) slog.Level {
	if debug || os.Getenv("SCION_LOG_LEVEL") == "debug" {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}

// FormatLogID returns the configured log ID (for display purposes).
func FormatLogID() string {
	return resolveLogID()
}

// FormatProjectID returns the resolved project ID (for display purposes).
func FormatProjectID() string {
	id := resolveProjectID()
	if id == "" {
		return "(auto-detect)"
	}
	// Redact to show only partial for security
	if len(id) > 8 {
		return id[:4] + "..." + strconv.Itoa(len(id)) + " chars"
	}
	return id
}

// mapToCloudHTTPRequest converts a map (from slog.Group attrs) to a
// gcplog.HTTPRequest for promotion to a top-level LogEntry field.
func mapToCloudHTTPRequest(m map[string]any) *gcplog.HTTPRequest {
	getString := func(key string) string {
		if v, ok := m[key].(string); ok {
			return v
		}
		return ""
	}
	getInt := func(key string) int {
		switch v := m[key].(type) {
		case int:
			return v
		case int64:
			return int(v)
		case float64:
			return int(v)
		}
		return 0
	}
	getInt64 := func(key string) int64 {
		switch v := m[key].(type) {
		case int64:
			return v
		case int:
			return int64(v)
		case float64:
			return int64(v)
		}
		return 0
	}

	method := getString("requestMethod")
	rawURL := getString("requestUrl")
	userAgent := getString("userAgent")
	referer := getString("referer")
	protocol := getString("protocol")
	latencyStr := getString("latency")

	// Build a minimal *http.Request for the Cloud Logging client.
	parsedURL, _ := url.Parse(rawURL)
	if parsedURL == nil {
		parsedURL = &url.URL{}
	}
	req := &http.Request{
		Method: method,
		URL:    parsedURL,
		Proto:  protocol,
		Header: http.Header{},
	}
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}
	if referer != "" {
		req.Header.Set("Referer", referer)
	}

	var latency time.Duration
	if latencyStr != "" {
		latency, _ = time.ParseDuration(latencyStr)
	}

	return &gcplog.HTTPRequest{
		Request:      req,
		RequestSize:  getInt64("requestSize"),
		Status:       getInt("status"),
		ResponseSize: getInt64("responseSize"),
		Latency:      latency,
		RemoteIP:     getString("remoteIp"),
	}
}
