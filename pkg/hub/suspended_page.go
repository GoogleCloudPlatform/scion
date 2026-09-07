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
	"errors"
	"fmt"
	"html"
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// suspendedPageHTML returns a self-contained HTML page for suspended users.
// It uses inline styles (no external dependencies) with dark mode support,
// consistent with the maintenance and error page patterns. The page includes
// a working sign-out action and displays the signed-in email.
//
// The page deliberately omits the SPA entry script and __SCION_DATA__ to
// prevent any protected API or SSE bootstrap fan-out.
func suspendedPageHTML(email string) string {
	var emailLine string
	if email != "" {
		emailLine = fmt.Sprintf(
			`<p class="email">Signed in as %s</p>`,
			html.EscapeString(email),
		)
	}

	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Scion - Account Suspended</title>
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

        .email {
            font-size: 0.875rem;
            color: var(--text-muted);
            margin-top: 1rem;
            word-break: break-all;
        }

        .sign-out {
            display: inline-block;
            margin-top: 1.5rem;
            padding: 0.5rem 1.25rem;
            font-size: 0.875rem;
            font-weight: 500;
            color: var(--accent);
            border: 1px solid var(--border);
            border-radius: 6px;
            background: var(--surface);
            cursor: pointer;
            text-decoration: none;
            transition: background-color 0.15s;
        }

        .sign-out:hover {
            background: var(--bg);
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
        <span class="icon" role="img" aria-label="suspended">&#128683;</span>
        <h1>Account Suspended</h1>
        <p class="message">Your account has been suspended and access to Scion is currently unavailable. Please contact your administrator for assistance.</p>
        %s
        <div><a href="/auth/logout" class="sign-out">Sign Out</a></div>
        <span class="badge">scion</span>
    </div>
</body>
</html>`, emailLine)
}

// serveSuspendedPage writes the self-contained suspended-account HTML page
// to the response. It sets appropriate cache-control headers and returns
// HTTP 403.
func (ws *WebServer) serveSuspendedPage(w http.ResponseWriter, email string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.WriteHeader(http.StatusForbidden)
	_, _ = fmt.Fprint(w, suspendedPageHTML(email))
}

// serveSuspendedResponse is the single entry point for all suspension denial
// responses. Browser navigations receive the self-contained HTML page; all
// other request types (SSE, programmatic fetch, HEAD, API-style) receive the
// canonical structured JSON user_suspended denial so callers that expect
// machine-readable responses are never surprised by HTML.
func (ws *WebServer) serveSuspendedResponse(w http.ResponseWriter, r *http.Request, email string) {
	if isBrowserRequest(r) {
		ws.serveSuspendedPage(w, email)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]string{
			"code":    "user_suspended",
			"message": "Your account has been suspended.",
		},
	})
}

// serveInternalError is the single entry point for fail-closed internal error
// responses on protected routes. Browser navigations receive a minimal HTML
// error page; all other request types receive structured JSON. This prevents
// stale cookie authority from being trusted when the store is unavailable.
func (ws *WebServer) serveInternalError(w http.ResponseWriter, r *http.Request) {
	if isBrowserRequest(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprint(w, `<!DOCTYPE html><html lang="en"><head><meta charset="UTF-8"><title>Service Error</title></head><body><h1>Service Unavailable</h1><p>An internal error occurred. Please try again later.</p></body></html>`)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]string{
			"code":    "internal_error",
			"message": "An internal error occurred. Please try again later.",
		},
	})
}

// suspendedUserMiddleware re-verifies the authenticated user's status against
// the authoritative store on every protected browser navigation. This catches
// mid-session suspensions that the cookie-based session auth middleware cannot
// detect (the session cookie lives for 24h, so without this check a suspended
// user would keep full web access until cookie expiry).
//
// Placement: runs after session/proxy/dev auth middleware has loaded the user
// into context, and before adminModeWebMiddleware and the SPA handler.
//
// API routes are skipped — they have their own structured JSON denial via
// the Hub's UnifiedAuth middleware.
func (ws *WebServer) suspendedUserMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip public routes (login, logout, assets, health, etc.)
		if isPublicRoute(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		// Get user from context (set by dev/proxy/session auth middleware)
		user := getWebSessionUser(r.Context())
		if user == nil {
			// No authenticated user — downstream middleware will handle
			// (sessionAuthMiddleware redirects to login).
			next.ServeHTTP(w, r)
			return
		}

		// Need a store for authoritative lookup. Fail closed if the store
		// is not configured — do not trust stale cookie authority.
		if ws.store == nil {
			ws.logger().Error("Suspended user check: store not configured, failing closed",
				"user_id", user.UserID)
			ws.serveInternalError(w, r)
			return
		}

		// Authoritative store lookup by user ID (not the cookie role/status).
		dbUser, err := ws.store.GetUser(r.Context(), user.UserID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				// User deleted — fail closed. Clear the stale session and
				// redirect to login so the cookie cannot carry stale admin
				// authority.
				ws.logger().Warn("Suspended user check: user no longer exists",
					"user_id", user.UserID, "email", user.Email)
				ws.clearStaleSession(w, r)
				return
			}
			// Transient store error — fail closed to prevent stale authority.
			ws.logger().Error("Suspended user check: store lookup failed",
				"user_id", user.UserID, "error", err)
			ws.serveInternalError(w, r)
			return
		}

		if dbUser.Status == store.UserStatusSuspended {
			ws.logger().Info("Suspended user check: access blocked",
				"user_id", user.UserID, "email", user.Email)
			ws.serveSuspendedResponse(w, r, user.Email)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// clearStaleSession clears the session cookie and returns a fail-closed
// response. For browser requests it redirects to the login page; for
// non-browser requests it returns a JSON 401 so programmatic callers can
// handle re-authentication. This avoids redirect loops because /login is
// a public route that does not trigger the session auth middleware.
func (ws *WebServer) clearStaleSession(w http.ResponseWriter, r *http.Request) {
	session, err := ws.sessionStore.Get(r, webSessionName)
	if err != nil {
		session, _ = ws.sessionStore.New(r, webSessionName)
	}
	for key := range session.Values {
		delete(session.Values, key)
	}
	session.Options.MaxAge = -1
	if err := session.Save(r, w); err != nil {
		ws.logger().Warn("Failed to clear stale session", "error", err)
	}

	if isBrowserRequest(r) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]string{
			"code":    "session_invalid",
			"message": "Your session is no longer valid. Please sign in again.",
		},
	})
}
