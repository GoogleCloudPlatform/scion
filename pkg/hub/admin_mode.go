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
			if user := GetUserIdentityFromContext(r.Context()); user != nil && user.Role() == "admin" {
				next.ServeHTTP(w, r)
				return
			}

			// Block everyone else with 503.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{
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
		if strings.HasPrefix(path, "/assets/") || strings.HasPrefix(path, "/shoelace/") || path == "/favicon.ico" {
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
		fmt.Fprint(w, maintenancePageHTML(ws.maintenance.Message()))
	})
}

// handleAdminMaintenance handles GET and PUT /api/v1/admin/maintenance.
// GET returns the current maintenance state; PUT updates it.
// Both require admin role.
func (s *Server) handleAdminMaintenance(w http.ResponseWriter, r *http.Request) {
	// Require admin user.
	user := GetUserIdentityFromContext(r.Context())
	if user == nil || user.Role() != "admin" {
		Forbidden(w)
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
		MethodNotAllowed(w)
	}
}

// handleAdminScheduler handles GET /api/v1/admin/scheduler.
// Returns the scheduler's current status including recurring handlers,
// event handlers, and active one-shot timer count. Requires admin role.
func (s *Server) handleAdminScheduler(w http.ResponseWriter, r *http.Request) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil || user.Role() != "admin" {
		Forbidden(w)
		return
	}

	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
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

// maintenancePageHTML returns a self-contained HTML maintenance page.
// It uses inline styles (no external dependencies) with dark mode support.
func maintenancePageHTML(message string) string {
	// Escape the message for safe HTML embedding.
	escaped := strings.ReplaceAll(message, "&", "&amp;")
	escaped = strings.ReplaceAll(escaped, "<", "&lt;")
	escaped = strings.ReplaceAll(escaped, ">", "&gt;")
	escaped = strings.ReplaceAll(escaped, "\"", "&quot;")

	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Scion - Maintenance</title>
    <style>
        :root {
            --bg: #f8fafc;
            --surface: #ffffff;
            --text: #1e293b;
            --text-muted: #64748b;
            --border: #e2e8f0;
            --accent: #3b82f6;
        }

        @media (prefers-color-scheme: dark) {
            :root {
                --bg: #0f172a;
                --surface: #1e293b;
                --text: #f1f5f9;
                --text-muted: #94a3b8;
                --border: #334155;
                --accent: #60a5fa;
            }
        }

        * {
            box-sizing: border-box;
            margin: 0;
            padding: 0;
        }

        html, body {
            height: 100%%;
            font-family: 'Inter', ui-sans-serif, system-ui, -apple-system, sans-serif;
            background: var(--bg);
            color: var(--text);
            -webkit-font-smoothing: antialiased;
        }

        body {
            display: flex;
            align-items: center;
            justify-content: center;
        }

        .container {
            text-align: center;
            padding: 2rem;
            max-width: 480px;
        }

        .icon {
            font-size: 3rem;
            margin-bottom: 1.5rem;
            display: block;
        }

        h1 {
            font-size: 1.5rem;
            font-weight: 600;
            margin-bottom: 0.75rem;
        }

        .message {
            color: var(--text-muted);
            font-size: 1rem;
            line-height: 1.6;
        }

        .badge {
            display: inline-block;
            margin-top: 1.5rem;
            padding: 0.25rem 0.75rem;
            font-size: 0.75rem;
            font-weight: 500;
            color: var(--accent);
            border: 1px solid var(--border);
            border-radius: 9999px;
            background: var(--surface);
        }
    </style>
</head>
<body>
    <div class="container">
        <span class="icon" role="img" aria-label="maintenance">&#128295;</span>
        <h1>Under Maintenance</h1>
        <p class="message">%s</p>
        <span class="badge">scion</span>
    </div>
</body>
</html>`, escaped)
}
