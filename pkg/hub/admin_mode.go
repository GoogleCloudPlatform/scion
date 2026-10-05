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
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const defaultMaintenanceMessage = "System offline for maintenance"

// MaintenanceState holds runtime maintenance mode state shared between
// the Hub API server and the Web frontend server. It is safe for
// concurrent access.
type MaintenanceState struct {
	mu      sync.RWMutex
	enabled bool
	message string
}

// NewMaintenanceState creates a MaintenanceState with the given initial values.
func NewMaintenanceState(enabled bool, message string) *MaintenanceState {
	return &MaintenanceState{
		enabled: enabled,
		message: message,
	}
}

// IsEnabled returns whether maintenance mode is currently active.
func (ms *MaintenanceState) IsEnabled() bool {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	return ms.enabled
}

// Message returns the current maintenance message, falling back to the
// default if none is set.
func (ms *MaintenanceState) Message() string {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	if ms.message == "" {
		return defaultMaintenanceMessage
	}
	return ms.message
}

// SetEnabled enables or disables maintenance mode.
func (ms *MaintenanceState) SetEnabled(v bool) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	ms.enabled = v
}

// SetMessage updates the maintenance message.
func (ms *MaintenanceState) SetMessage(msg string) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	ms.message = msg
}

// Set updates both enabled and message atomically.
func (ms *MaintenanceState) Set(enabled bool, message string) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	ms.enabled = enabled
	ms.message = message
}

// adminModeMiddleware restricts Hub API access to admin users only when
// maintenance mode is enabled. Non-admin users receive a 503 JSON response.
// Agents and brokers are allowed through so that system operations continue
// uninterrupted. The middleware checks the runtime MaintenanceState on every
// request, so toggling maintenance mode takes effect immediately.
func adminModeMiddleware(state *MaintenanceState) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !state.IsEnabled() {
				next.ServeHTTP(w, r)
				return
			}

			// Allow agents through — system operations must continue.
			if agent := GetAgentIdentityFromContext(r.Context()); agent != nil {
				next.ServeHTTP(w, r)
				return
			}

			// Allow brokers through — system operations must continue.
			if broker := GetBrokerIdentityFromContext(r.Context()); broker != nil {
				next.ServeHTTP(w, r)
				return
			}

			// Allow admin users through.
			if user := GetUserIdentityFromContext(r.Context()); IsUnscopedLocalPlatformAdmin(user) {
				next.ServeHTTP(w, r)
				return
			}

			// Block everyone else with 503.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error":   "system_maintenance",
				"message": state.Message(),
			})
		})
	}
}

// adminModeWebMiddleware restricts web frontend access to admin users only
// when maintenance mode is enabled. Auth routes and health checks are always
// allowed through so admins can log in. Non-admin users see a self-contained
// HTML maintenance page.
func (ws *WebServer) adminModeWebMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ws.maintenance == nil || !ws.maintenance.IsEnabled() {
			next.ServeHTTP(w, r)
			return
		}

		path := r.URL.Path

		// Allow auth routes so admins can log in.
		if strings.HasPrefix(path, "/auth/") || path == "/login" {
			next.ServeHTTP(w, r)
			return
		}

		// Allow health checks.
		if path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}

		// Allow static assets (required for login page).
		if strings.HasPrefix(path, "/assets/") || strings.HasPrefix(path, "/shoelace/") || isAppIconPath(path) {
			next.ServeHTTP(w, r)
			return
		}

		// Allow Hub API routes through — they have their own admin mode
		// middleware via the Hub's applyMiddleware chain.
		if strings.HasPrefix(path, "/api/v1/") {
			next.ServeHTTP(w, r)
			return
		}

		// Allow admin users through.
		if user := getWebSessionUser(r.Context()); user != nil && user.Role == "admin" {
			next.ServeHTTP(w, r)
			return
		}

		// Block everyone else with a maintenance page.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprint(w, maintenancePageHTML(ws.maintenance.Message()))
	})
}

// appIconPaths are the root-level app icon and web app manifest files
// from web/public/. Browsers fetch them without credentials (the
// manifest is a CORS request with no cookies), so they must load on the
// maintenance-mode login page too. Keep this list exact rather than
// allowing every root-level file.
var appIconPaths = map[string]bool{
	"/favicon.ico":           true,
	"/favicon.svg":           true,
	"/apple-touch-icon.png":  true,
	"/icon-192.png":          true,
	"/icon-512.png":          true,
	"/icon-maskable-512.png": true,
	"/manifest.webmanifest":  true,
}

// isAppIconPath reports whether path is one of appIconPaths.
func isAppIconPath(path string) bool {
	return appIconPaths[path]
}

// handleAdminMaintenance handles GET and PUT /api/v1/admin/maintenance.
// GET returns the current maintenance state; PUT updates it.
// Authorization: enforced by routeGuard via hub.admin_mode.update permission.
func (s *Server) handleAdminMaintenance(w http.ResponseWriter, r *http.Request) {
	// Whenever OperationalSettings is wired (any DB driver, SQLite included),
	// delegate to DB-backed handlers: maintenance is durable (persisted in
	// hub_settings) and, on postgres, cluster-wide (LISTEN/NOTIFY). An
	// in-memory-only write on a DB-backed hub would be reverted by the next
	// ops.Update re-applying the maintenance row (#1091). Only a hub with no
	// OperationalSettings keeps the in-memory state.
	if ops := s.GetOperationalSettings(); ops != nil {
		switch r.Method {
		case http.MethodGet:
			s.handleGetMaintenanceDB(w, ops)
		case http.MethodPut:
			s.handlePutMaintenanceDB(w, r, ops)
		default:
			MethodNotAllowed(w, http.MethodGet, http.MethodPut)
		}
		return
	}

	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"enabled": s.maintenance.IsEnabled(),
			"message": s.maintenance.Message(),
		})

	case http.MethodPut:
		var body struct {
			Enabled *bool  `json:"enabled"`
			Message string `json:"message"`
		}
		if err := readJSON(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "Invalid request body", nil)
			return
		}
		if body.Enabled != nil {
			s.maintenance.SetEnabled(*body.Enabled)
		}
		if body.Message != "" {
			s.maintenance.SetMessage(body.Message)
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"enabled": s.maintenance.IsEnabled(),
			"message": s.maintenance.Message(),
		})

	default:
		MethodNotAllowed(w, http.MethodGet, http.MethodPut)
	}
}

// handleAdminScheduler handles GET /api/v1/admin/scheduler.
// Returns the scheduler's current status including recurring handlers,
// event handlers, and active one-shot timer count.
// Authorization: enforced by routeGuard via hub.scheduler.read permission.
func (s *Server) handleAdminScheduler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}

	if s.scheduler == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"status": "not_initialized",
		})
		return
	}

	status := s.scheduler.Status()

	// Fetch recent scheduled events across all projects.
	var events []store.ScheduledEvent
	if s.store != nil {
		result, err := s.store.ListScheduledEvents(r.Context(), store.ScheduledEventFilter{}, store.ListOptions{Limit: 50})
		if err == nil && result != nil {
			events = result.Items
		}
	}

	// Fetch recurring schedules across all projects.
	var schedules []store.Schedule
	if s.store != nil {
		result, err := s.store.ListSchedules(r.Context(), store.ScheduleFilter{}, store.ListOptions{Limit: 100})
		if err == nil && result != nil {
			schedules = result.Items
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"scheduler":          status,
		"scheduledEvents":    events,
		"recurringSchedules": schedules,
		"serverTime":         time.Now().UTC(),
	})
}

// maintenancePageHTML returns a self-contained HTML maintenance page,
// using the shared renderErrorPage template.
func maintenancePageHTML(message string) string {
	return renderErrorPage("Scion - Maintenance", "&#128295;", "maintenance", "Under Maintenance", message)
}
