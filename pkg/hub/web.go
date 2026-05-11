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
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"github.com/GoogleCloudPlatform/scion/pkg/version"
	"github.com/GoogleCloudPlatform/scion/web"
	"github.com/gorilla/sessions"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// HealthProvider is a function that returns the health info for a component.
// The returned value is serialized as a JSON sub-object in the composite
// health response. The concrete type is intentionally left as interface{}
// so that the web server does not import hub or runtimebroker types directly.
type HealthProvider func(ctx context.Context) interface{}

// WebHealthInfo holds the web server's own health information.
type WebHealthInfo struct {
	Status         string `json:"status"`
	AssetsDir      string `json:"assetsDir"`
	AssetsEmbedded bool   `json:"assetsEmbedded"`
}

// CompositeHealthResponse is the top-level health response returned by the
// web server's /healthz endpoint. It includes backward-compatible top-level
// fields (status, version, scionVersion, uptime) plus per-component sub-objects.
type CompositeHealthResponse struct {
	Status       string      `json:"status"`
	Version      string      `json:"version"`
	ScionVersion string      `json:"scionVersion"`
	Uptime       string      `json:"uptime,omitempty"`
	Web          interface{} `json:"web"`
	Hub          interface{} `json:"hub,omitempty"`
	Broker       interface{} `json:"broker,omitempty"`
}

// shoelaceVersion is the Shoelace CDN version used by the SPA shell.
const shoelaceVersion = "2.19.0"

// webSessionName is the cookie name for web sessions.
const webSessionName = "scion_sess"

// Session key constants for storing values in the gorilla session map.
const (
	sessKeyUserID          = "uid"
	sessKeyUserEmail       = "email"
	sessKeyUserName        = "name"
	sessKeyUserAvatar      = "avatar"
	sessKeyUserRole        = "role"
	sessKeyReturnTo        = "returnTo"
	sessKeyOAuthState      = "oauthState"
	sessKeyHubAccessToken  = "hubAccessToken"
	sessKeyHubRefreshToken = "hubRefreshToken"
	sessKeyHubTokenExpiry  = "hubTokenExpiry"
)

// webUserContextKey is the key for storing the web session user in the request context.
type webUserContextKey struct{}

// webSessionUser represents an authenticated user from the web session.
type webSessionUser struct {
	UserID    string `json:"id"`
	Email     string `json:"email"`
	Name      string `json:"displayName"`
	AvatarURL string `json:"avatarUrl,omitempty"`
	Role      string `json:"role,omitempty"`
}

// getWebSessionUser retrieves the web session user from the request context.
func getWebSessionUser(ctx context.Context) *webSessionUser {
	if u, ok := ctx.Value(webUserContextKey{}).(*webSessionUser); ok {
		return u
	}
	return nil
}

// WebServerConfig holds configuration for the web frontend server.
type WebServerConfig struct {
	// Port is the HTTP port to listen on (default 8080).
	Port int
	// Host is the address to bind to (e.g., "0.0.0.0").
	Host string
	// AssetsDir overrides embedded assets with a filesystem directory.
	// When set, static files are served from this path instead of the embedded FS.
	AssetsDir string
	// Debug enables verbose debug logging.
	Debug bool
	// SessionSecret is the HMAC key for signing session cookies.
	SessionSecret string
	// BaseURL is the public URL for OAuth redirects (e.g., "https://scion.example.com").
	BaseURL string
	// DevAuthToken is the dev token for auto-login (empty = disabled).
	DevAuthToken string
	// AuthorizedDomains is the list of allowed email domains (empty = all allowed).
	AuthorizedDomains []string
	// AdminEmails is the list of bootstrap admin emails (bypass domain check).
	AdminEmails []string
	// UserAccessMode controls login-time access evaluation ("open", "domain_restricted", "invite_only").
	UserAccessMode string
	// AdminMode restricts access to admin users only (maintenance mode).
	AdminMode bool
	// MaintenanceMessage is the custom message shown during admin mode.
	MaintenanceMessage string
}

// WebServer serves the web frontend SPA shell and static assets.
type WebServer struct {
	config       WebServerConfig
	httpServer   *http.Server
	mux          *http.ServeMux
	assets       fs.FS  // embedded or nil
	assetsDisk   string // filesystem override path, or ""
	shellTmpl    *template.Template
	sessionStore *sessions.FilesystemStore
	oauthService *OAuthService
	store        store.Store
	userTokenSvc *UserTokenService
	events       *ChannelEventPublisher      // nil when no publisher configured
	hubHandler   http.Handler                // mounted Hub API handler, or nil
	hubShutdown  func(context.Context) error // Hub resource cleanup, or nil
	maintenance  *MaintenanceState           // runtime maintenance mode state (shared with Hub)
	startTime    time.Time

	// Dedicated request logger (nil = disabled)
	requestLogger *slog.Logger

	// Health providers for composite health response (combo mode)
	hubHealthProvider    HealthProvider
	brokerHealthProvider HealthProvider
}

// safeJSONForHTML escapes sequences in serialized JSON that could be
// interpreted as HTML when embedded in a <script> tag. Specifically:
//   - "</  -> "<\/" prevents closing a parent <script> element.
//   - "<!--" -> "<\!--" prevents opening an HTML comment.
func safeJSONForHTML(raw string) string {
	s := strings.ReplaceAll(raw, "</", `<\/`)
	s = strings.ReplaceAll(s, "<!--", `<\!--`)
	return s
}

// spaShellTemplate is the Go html/template for the SPA shell page.
// It mirrors the structure from web/src/server/ssr/templates.ts but renders
// a client-only shell (no SSR content).
var spaShellTemplate = `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Scion</title>

    <!-- Preconnect to CDNs for faster loading -->
    <link rel="preconnect" href="https://cdn.jsdelivr.net">
    <link rel="preconnect" href="https://fonts.googleapis.com">
    <link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>

    <!-- Fonts -->
    <link href="https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;700&display=swap" rel="stylesheet">
    <link href="https://fonts.googleapis.com/css2?family=JetBrains+Mono:wght@400;500&display=swap" rel="stylesheet">

    <!-- Shoelace Component Library -->
    <link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/@shoelace-style/shoelace@{{.ShoelaceVersion}}/cdn/themes/light.css">
    <link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/@shoelace-style/shoelace@{{.ShoelaceVersion}}/cdn/themes/dark.css">
    <script type="module" src="https://cdn.jsdelivr.net/npm/@shoelace-style/shoelace@{{.ShoelaceVersion}}/cdn/shoelace-autoloader.js"></script>

    <!-- Initial state for hydration -->
    <script id="__SCION_DATA__" type="application/json">{{.InitialData}}</script>

    <style>
        /* Critical CSS - Core layout to prevent FOUC */

        /* Color Palette - Light Mode (inlined for fast first paint) */
        :root {
            /* Primary */
            --scion-primary-50: #eff6ff;
            --scion-primary-500: #3b82f6;
            --scion-primary-600: #2563eb;
            --scion-primary-700: #1d4ed8;

            /* Neutral */
            --scion-neutral-50: #f8fafc;
            --scion-neutral-100: #f1f5f9;
            --scion-neutral-200: #e2e8f0;
            --scion-neutral-500: #64748b;
            --scion-neutral-600: #475569;
            --scion-neutral-700: #334155;
            --scion-neutral-800: #1e293b;
            --scion-neutral-900: #0f172a;

            /* Semantic */
            --scion-primary: var(--scion-primary-500);
            --scion-primary-hover: var(--scion-primary-600);
            --scion-bg: var(--scion-neutral-50);
            --scion-bg-subtle: var(--scion-neutral-100);
            --scion-surface: #ffffff;
            --scion-text: var(--scion-neutral-800);
            --scion-text-muted: var(--scion-neutral-500);
            --scion-border: var(--scion-neutral-200);

            /* Layout */
            --scion-sidebar-width: 260px;
            --scion-header-height: 60px;

            /* Typography */
            --scion-font-sans: 'Inter', ui-sans-serif, system-ui, -apple-system, sans-serif;
            --scion-font-mono: 'JetBrains Mono', ui-monospace, monospace;
        }

        /* Dark mode support */
        @media (prefers-color-scheme: dark) {
            :root:not([data-theme="light"]) {
                --scion-primary: #60a5fa;
                --scion-primary-hover: #93c5fd;
                --scion-bg: var(--scion-neutral-900);
                --scion-bg-subtle: var(--scion-neutral-800);
                --scion-surface: var(--scion-neutral-800);
                --scion-text: #f1f5f9;
                --scion-text-muted: #94a3b8;
                --scion-border: var(--scion-neutral-700);
            }
        }

        [data-theme="dark"] {
            --scion-primary: #60a5fa;
            --scion-primary-hover: #93c5fd;
            --scion-bg: var(--scion-neutral-900);
            --scion-bg-subtle: var(--scion-neutral-800);
            --scion-surface: var(--scion-neutral-800);
            --scion-text: #f1f5f9;
            --scion-text-muted: #94a3b8;
            --scion-border: var(--scion-neutral-700);
        }

        * {
            box-sizing: border-box;
            margin: 0;
            padding: 0;
        }

        html, body {
            height: 100%;
            font-family: var(--scion-font-sans);
            background: var(--scion-bg);
            color: var(--scion-text);
            -webkit-font-smoothing: antialiased;
            -moz-osx-font-smoothing: grayscale;
        }

        #app {
            min-height: 100%;
        }

        /* Prevent FOUC for custom elements */
        scion-app:not(:defined),
        scion-login-page:not(:defined),
        scion-nav:not(:defined),
        scion-header:not(:defined),
        scion-breadcrumb:not(:defined),
        scion-status-badge:not(:defined),
        scion-page-home:not(:defined),
        scion-page-projects:not(:defined),
        scion-page-agents:not(:defined),
        scion-page-404:not(:defined) {
            display: block;
            opacity: 0.5;
        }

        /* Shoelace component loading state */
        sl-button:not(:defined),
        sl-icon:not(:defined),
        sl-badge:not(:defined),
        sl-drawer:not(:defined),
        sl-dropdown:not(:defined),
        sl-menu:not(:defined),
        sl-menu-item:not(:defined),
        sl-breadcrumb:not(:defined),
        sl-breadcrumb-item:not(:defined),
        sl-tooltip:not(:defined),
        sl-avatar:not(:defined) {
            visibility: hidden;
        }
    </style>

    <!-- Theme detection script (runs before paint) -->
    <script>
        (function() {
            var saved = localStorage.getItem('scion-theme');
            if (saved === 'dark' || (!saved && window.matchMedia('(prefers-color-scheme: dark)').matches)) {
                document.documentElement.setAttribute('data-theme', 'dark');
                document.documentElement.classList.add('sl-theme-dark');
            } else {
                document.documentElement.setAttribute('data-theme', 'light');
            }
        })();
    </script>
</head>
<body>
    <div id="app">{{if .IsLoginPage}}<scion-login-page></scion-login-page>{{else if .IsInvitePage}}<scion-page-invite></scion-page-invite>{{else}}<scion-app></scion-app>{{end}}</div>

    <!-- Client entry point -->
    <script type="module" src="/assets/main.js"></script>
</body>
</html>`

// noAssetsPage is a self-contained HTML page served when the binary was built
// without embedded web assets (e.g. via `go install`) and no --web-assets-dir
// was provided. It requires no external resources so it renders correctly even
// when the static asset routes return 404.
var noAssetsPage = `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Scion – Web UI Not Available</title>
    <style>
        * { box-sizing: border-box; margin: 0; padding: 0; }
        body {
            font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
            background: #0f172a; color: #e2e8f0;
            display: flex; align-items: center; justify-content: center;
            min-height: 100vh; padding: 2rem;
        }
        .container {
            max-width: 540px; text-align: center;
        }
        h1 { font-size: 1.5rem; margin-bottom: 1rem; color: #f1f5f9; }
        p  { line-height: 1.6; margin-bottom: 1rem; color: #94a3b8; }
        code {
            background: #1e293b; padding: 0.15em 0.4em; border-radius: 4px;
            font-size: 0.9em; color: #60a5fa;
        }
        .hint {
            margin-top: 1.5rem; padding: 1rem; background: #1e293b;
            border-radius: 8px; text-align: left; font-size: 0.9rem;
        }
        .hint p { margin-bottom: 0.5rem; }
        .hint p:last-child { margin-bottom: 0; }
    </style>
</head>
<body>
    <div class="container">
        <h1>Web UI Not Available</h1>
        <p>This Scion binary was not built from source with web assets included.
           The Hub API is still fully operational.</p>
        <div class="hint">
            <p>To use the web UI, either:</p>
            <p>1. Build from source: <code>make build</code></p>
            <p>2. Point to pre-built assets: <code>--web-assets-dir /path/to/dist/client</code></p>
        </div>
    </div>
</body>
</html>`

// spaShellData holds the template data for the SPA shell.
type spaShellData struct {
	ShoelaceVersion string
	IsLoginPage     bool
	IsInvitePage    bool
	// InitialData is safe-for-HTML JSON embedded in the __SCION_DATA__ script tag.
	// It is typed as template.JS so html/template does not escape it further.
	InitialData template.JS
}

// NewWebServer creates a new web frontend server.
func NewWebServer(cfg WebServerConfig) *WebServer {
	if cfg.Port == 0 {
		cfg.Port = 8080
	}
	if cfg.Host == "" {
		cfg.Host = "0.0.0.0"
	}

	ws := &WebServer{
		config:    cfg,
		mux:       http.NewServeMux(),
		startTime: time.Now(),
	}

	// Initialize session store
	sessionKey := cfg.SessionSecret
	if sessionKey == "" {
		// Generate a random key for development/testing
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			slog.Error("Failed to generate session secret", "error", err)
		}
		sessionKey = hex.EncodeToString(b)
		slog.Warn("No session secret configured, using random key (sessions will not persist across restarts)")
	}

	// Use a filesystem-backed session store so that only a small session ID
	// is sent as a cookie. This avoids the 4 KB cookie size limit that can
	// be exceeded when JWT tokens are stored in the session.
	sessionDir := filepath.Join(os.TempDir(), "scion-sessions")
	if err := os.MkdirAll(sessionDir, 0700); err != nil {
		slog.Error("Failed to create session directory", "dir", sessionDir, "error", err)
	}
	fsStore := sessions.NewFilesystemStore(sessionDir, []byte(sessionKey))
	// Remove the default 4096-byte securecookie encoding limit. The
	// FilesystemStore writes session data to disk (not cookies), so the
	// browser cookie-size cap is irrelevant. JWT tokens stored in the
	// session regularly exceed 4096 bytes after gob+base64 encoding,
	// which causes Save() to fail and tokens to be silently dropped.
	fsStore.MaxLength(0)
	fsStore.Options = &sessions.Options{
		Path:     "/",
		MaxAge:   86400, // 24 hours
		HttpOnly: true,
		Secure:   strings.HasPrefix(cfg.BaseURL, "https://"),
		SameSite: http.SameSiteLaxMode,
	}
	ws.sessionStore = fsStore

	// Resolve asset source
	if cfg.AssetsDir != "" {
		ws.assetsDisk = cfg.AssetsDir
		slog.Info("Web server using filesystem assets", "dir", cfg.AssetsDir)
	} else if web.AssetsEmbedded {
		sub, err := fs.Sub(web.ClientAssets, "dist/client")
		if err != nil {
			slog.Error("Failed to create sub-filesystem from embedded assets", "error", err)
		} else {
			ws.assets = sub
		}
		slog.Info("Web server using embedded assets")
	} else {
		slog.Warn("No web assets available: build with embedded assets or use --web-assets-dir")
	}

	// Parse SPA shell template
	tmpl, err := template.New("spa-shell").Parse(spaShellTemplate)
	if err != nil {
		slog.Error("Failed to parse SPA shell template", "error", err)
	}
	ws.shellTmpl = tmpl

	ws.registerRoutes()

	return ws
}

// SetMaintenanceState sets the shared runtime maintenance state.
func (ws *WebServer) SetMaintenanceState(ms *MaintenanceState) {
	ws.maintenance = ms
}

// SetOAuthService sets the OAuth service for web OAuth flows.
func (ws *WebServer) SetOAuthService(svc *OAuthService) {
	ws.oauthService = svc
}

// SetStore sets the data store for user lookup/creation.
func (ws *WebServer) SetStore(s store.Store) {
	ws.store = s
}

// SetUserTokenService sets the user token service for Hub JWT generation.
func (ws *WebServer) SetUserTokenService(svc *UserTokenService) {
	ws.userTokenSvc = svc
}

// SetEventPublisher sets the event publisher for real-time SSE streaming.
func (ws *WebServer) SetEventPublisher(pub *ChannelEventPublisher) {
	ws.events = pub
}

// SetRequestLogger sets the dedicated request logger.
func (ws *WebServer) SetRequestLogger(l *slog.Logger) {
	ws.requestLogger = l
}

// SetHubHealthProvider registers a health provider for the Hub component.
// When set, the web server's /healthz endpoint includes Hub health in the
// composite response.
func (ws *WebServer) SetHubHealthProvider(p HealthProvider) {
	ws.hubHealthProvider = p
}

// SetBrokerHealthProvider registers a health provider for the Runtime Broker
// component. When set, the web server's /healthz endpoint includes broker
// health in the composite response.
func (ws *WebServer) SetBrokerHealthProvider(p HealthProvider) {
	ws.brokerHealthProvider = p
}

// MountHubAPI mounts the Hub API handler on the web server so both are
// served on a single port. hubShutdown is called during graceful shutdown
// to clean up Hub resources (control channels, broker auth, etc.).
func (ws *WebServer) MountHubAPI(hubHandler http.Handler, hubShutdown func(context.Context) error) {
	ws.hubHandler = hubHandler
	ws.hubShutdown = hubShutdown
	// Register the Hub API handler on the mux. Go's ServeMux uses
	// longest-prefix matching, so /api/v1/ takes priority over /
	// (the SPA catch-all).
	ws.mux.Handle("/api/v1/", ws.sessionToBearerMiddleware(hubHandler))
}

// sessionToBearerMiddleware bridges cookie-based web sessions to the
// Bearer-token authentication expected by the Hub API. If the request
// already carries an Authorization header it is passed through unchanged.
func (ws *WebServer) sessionToBearerMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// If the caller already supplied an Authorization header, pass through.
		if r.Header.Get("Authorization") != "" {
			next.ServeHTTP(w, r)
			return
		}

		session, err := ws.sessionStore.Get(r, webSessionName)
		if err != nil {
			// No valid session — let the Hub's own auth return 401.
			next.ServeHTTP(w, r)
			return
		}

		accessToken, _ := session.Values[sessKeyHubAccessToken].(string)
		if accessToken == "" {
			next.ServeHTTP(w, r)
			return
		}

		// Validate the access token. If the signing key was rotated,
		// the token will fail signature verification and we must
		// force the user to re-authenticate.
		tokenValid := false
		if ws.userTokenSvc != nil {
			if _, err := ws.userTokenSvc.ValidateUserToken(accessToken); err == nil {
				tokenValid = true
			}
		} else {
			// No token service — pass through and let Hub decide.
			tokenValid = true
		}

		// If token is invalid (expired or bad signature), try to refresh.
		if !tokenValid {
			refreshToken, _ := session.Values[sessKeyHubRefreshToken].(string)
			if refreshToken != "" && ws.userTokenSvc != nil {
				newAccess, newRefresh, expiresIn, err := ws.userTokenSvc.RefreshTokens(refreshToken)
				if err == nil {
					accessToken = newAccess
					tokenValid = true
					session.Values[sessKeyHubAccessToken] = newAccess
					session.Values[sessKeyHubRefreshToken] = newRefresh
					session.Values[sessKeyHubTokenExpiry] = time.Now().Add(time.Duration(expiresIn) * time.Second).UnixMilli()
					if err := session.Save(r, w); err != nil {
						slog.Warn("Failed to persist refreshed Hub token to session", "error", err)
					}
				} else {
					slog.Debug("Failed to refresh Hub token", "error", err)
				}
			}
		}

		// If token is still invalid after refresh attempt, the signing
		// key was likely rotated. Clear the session and force re-login.
		if !tokenValid {
			slog.Info("Hub token irrecoverably invalid, clearing session",
				"user", session.Values[sessKeyUserEmail])
			for key := range session.Values {
				delete(session.Values, key)
			}
			session.Options.MaxAge = -1
			if err := session.Save(r, w); err != nil {
				slog.Warn("Failed to clear invalid session", "error", err)
			}

			if isBrowserRequest(r) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				json.NewEncoder(w).Encode(map[string]interface{}{
					"error": map[string]string{
						"code":    "session_expired",
						"message": "Your session has expired. Please sign in again.",
					},
				})
				return
			}
			// Non-browser: let Hub return 401.
			next.ServeHTTP(w, r)
			return
		}

		// Check token expiry and proactively refresh if needed.
		if expiryMs, ok := session.Values[sessKeyHubTokenExpiry].(int64); ok {
			if time.Now().UnixMilli() >= expiryMs {
				refreshToken, _ := session.Values[sessKeyHubRefreshToken].(string)
				if refreshToken != "" && ws.userTokenSvc != nil {
					newAccess, newRefresh, expiresIn, err := ws.userTokenSvc.RefreshTokens(refreshToken)
					if err == nil {
						accessToken = newAccess
						session.Values[sessKeyHubAccessToken] = newAccess
						session.Values[sessKeyHubRefreshToken] = newRefresh
						session.Values[sessKeyHubTokenExpiry] = time.Now().Add(time.Duration(expiresIn) * time.Second).UnixMilli()
						if err := session.Save(r, w); err != nil {
							slog.Warn("Failed to persist refreshed Hub token to session", "error", err)
						}
					}
				}
			}
		}

		// Clone the request and inject the Authorization header.
		r2 := r.Clone(r.Context())
		r2.Header.Set("Authorization", "Bearer "+accessToken)
		next.ServeHTTP(w, r2)
	})
}

// registerRoutes sets up the web server routes.
func (ws *WebServer) registerRoutes() {
	ws.mux.HandleFunc("/healthz", ws.handleHealthz)
	ws.mux.Handle("/assets/", ws.staticHandler())
	ws.mux.Handle("/shoelace/", ws.staticHandler())
	// Auth routes (no session auth required)
	ws.mux.HandleFunc("/auth/login/", ws.handleOAuthLogin)
	ws.mux.HandleFunc("/auth/callback/", ws.handleOAuthCallback)
	ws.mux.HandleFunc("/auth/logout", ws.handleLogout)
	ws.mux.HandleFunc("/auth/me", ws.handleAuthMe)
	ws.mux.HandleFunc("/auth/providers", ws.handleAuthProviders)
	ws.mux.HandleFunc("/auth/debug", ws.handleAuthDebug)
	// SSE event stream (protected by session auth middleware)
	ws.mux.HandleFunc("/events", ws.handleSSE)
	// SPA catch-all (protected by session auth middleware)
	ws.mux.HandleFunc("/", ws.spaHandler())
}

// handleHealthz returns the web server health status.
// In standalone mode, only the web sub-object is included.
// In combo mode (with hub/broker health providers), a composite response
// is returned with backward-compatible top-level fields.
func (ws *WebServer) handleHealthz(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	webHealth := &WebHealthInfo{
		Status:         "ok",
		AssetsDir:      ws.config.AssetsDir,
		AssetsEmbedded: web.AssetsEmbedded,
	}

	resp := CompositeHealthResponse{
		Status:       "healthy",
		Version:      "0.1.0",
		ScionVersion: version.Short(),
		Web:          webHealth,
	}

	// Include Hub health if a provider is registered.
	if ws.hubHealthProvider != nil {
		hubHealth := ws.hubHealthProvider(ctx)
		resp.Hub = hubHealth

		// Inherit top-level version/uptime from hub health if available.
		if h, ok := hubHealth.(interface{ HealthStatus() string }); ok {
			if h.HealthStatus() != "healthy" {
				resp.Status = "degraded"
			}
		}
		// Use hub's uptime as the authoritative uptime.
		if h, ok := hubHealth.(*HealthResponse); ok {
			resp.Uptime = h.Uptime
			resp.Version = h.Version
			resp.ScionVersion = h.ScionVersion
		}
	} else {
		// No hub provider — use the web server's own uptime.
		resp.Uptime = time.Since(ws.startTime).Round(time.Second).String()
	}

	// Include Broker health if a provider is registered.
	if ws.brokerHealthProvider != nil {
		brokerHealth := ws.brokerHealthProvider(ctx)
		resp.Broker = brokerHealth

		if h, ok := brokerHealth.(interface{ HealthStatus() string }); ok {
			if h.HealthStatus() != "healthy" {
				resp.Status = "degraded"
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(resp)
}

// staticHandler returns an http.Handler that serves static assets.
// The handler checks ws.assets/ws.assetsDisk at serve time (not registration
// time) so that tests can override these fields after construction.
func (ws *WebServer) staticHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws.serveStaticAsset(w, r)
	})
}

func (ws *WebServer) serveStaticAsset(w http.ResponseWriter, r *http.Request) {
	if ws.assetsDisk == "" && ws.assets == nil {
		http.Error(w, "no assets available", http.StatusNotFound)
		return
	}

	var fileServer http.Handler
	if ws.assetsDisk != "" {
		fileServer = http.FileServer(http.Dir(ws.assetsDisk))
	} else {
		fileServer = http.FileServer(http.FS(ws.assets))
	}

	// Set cache headers based on whether the filename contains a hash.
	// Vite hashed assets (e.g., chunk-abc123.js) get long-lived caching.
	// Non-hashed entry points (e.g., main.js) get revalidation.
	if isHashedAsset(r.URL.Path) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	fileServer.ServeHTTP(w, r)
}

// isHashedAsset checks if a path looks like it contains a content hash.
// Vite produces filenames like "chunk-abc12345.js" or "style-abc12345.css".
func isHashedAsset(path string) bool {
	// Look for the pattern: name-<hash>.ext where hash is hex chars
	lastDot := strings.LastIndex(path, ".")
	if lastDot <= 0 {
		return false
	}
	name := path[:lastDot]
	lastDash := strings.LastIndex(name, "-")
	if lastDash <= 0 || lastDash >= len(name)-1 {
		return false
	}
	hash := name[lastDash+1:]
	if len(hash) < 6 {
		return false
	}
	for _, c := range hash {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// resolveAPIPath maps a browser URL path to the Hub API endpoint that should
// be prefetched for SSR hydration. Returns "" for paths with no prefetch.
func resolveAPIPath(urlPath string) string {
	// Trim trailing slash for consistent matching
	p := strings.TrimRight(urlPath, "/")

	switch {
	case p == "/agents":
		return "/api/v1/agents"
	case p == "/projects", p == "/groves":
		return "/api/v1/projects"
	case strings.HasPrefix(p, "/agents/") && strings.Count(p, "/") == 2:
		// /agents/{id} -> /api/v1/agents/{id}
		return "/api/v1" + p
	case strings.HasPrefix(p, "/projects/") && strings.Count(p, "/") == 2:
		// /projects/{id} -> /api/v1/projects/{id}
		return "/api/v1" + p
	default:
		return ""
	}
}

// prefetchPageData builds the initial page data JSON for the __SCION_DATA__
// script tag. It always includes user info from the session, and optionally
// prefetches page data from the Hub API via an in-process call.
func (ws *WebServer) prefetchPageData(r *http.Request) template.JS {
	// Build user info from session context (set by auth middleware).
	type pageUser struct {
		ID        string `json:"id"`
		Email     string `json:"email"`
		Name      string `json:"name"`
		AvatarURL string `json:"avatarUrl,omitempty"`
		Role      string `json:"role,omitempty"`
	}

	type pageDataEnvelope struct {
		Path  string      `json:"path"`
		Title string      `json:"title"`
		User  *pageUser   `json:"user,omitempty"`
		Data  interface{} `json:"data,omitempty"`
	}

	envelope := pageDataEnvelope{
		Path:  r.URL.Path,
		Title: "Scion",
	}

	// Populate user from session context.
	if u := getWebSessionUser(r.Context()); u != nil {
		envelope.User = &pageUser{
			ID:        u.UserID,
			Email:     u.Email,
			Name:      u.Name,
			AvatarURL: u.AvatarURL,
			Role:      u.Role,
		}
	}

	// Prefetch API data if Hub is mounted and the route maps to an API path.
	apiPath := resolveAPIPath(r.URL.Path)
	if apiPath != "" && ws.hubHandler != nil {
		// Read access token from session for the synthetic request.
		var accessToken string
		session, err := ws.sessionStore.Get(r, webSessionName)
		if err == nil {
			accessToken, _ = session.Values[sessKeyHubAccessToken].(string)
		}

		// Create a synthetic request with a 2-second deadline.
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		apiReq, err := http.NewRequestWithContext(ctx, "GET", apiPath, nil)
		if err == nil {
			if accessToken != "" {
				apiReq.Header.Set("Authorization", "Bearer "+accessToken)
			}
			rec := httptest.NewRecorder()
			ws.hubHandler.ServeHTTP(rec, apiReq)

			if rec.Code == http.StatusOK {
				var apiData interface{}
				if err := json.Unmarshal(rec.Body.Bytes(), &apiData); err == nil {
					envelope.Data = apiData
				}
			}
		}
	}

	raw, err := json.Marshal(envelope)
	if err != nil {
		slog.Error("Failed to marshal SSR page data", "error", err)
		return template.JS("{}")
	}

	return template.JS(safeJSONForHTML(string(raw)))
}

// hasWebAssets reports whether the server has web assets available to serve,
// either from an embedded FS or a filesystem directory.
func (ws *WebServer) hasWebAssets() bool {
	return ws.assets != nil || ws.assetsDisk != ""
}

// spaHandler returns the SPA shell HTML for any route not matched by other handlers.
func (ws *WebServer) spaHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Check if the request matches a real static file (e.g. root-level
		// public assets like notification icons) before falling through to
		// the SPA shell. Without this, files in web/public/ that don't live
		// under /assets/ or /shoelace/ would get the HTML shell instead.
		if r.URL.Path != "/" && ws.tryServeStaticFile(w, r) {
			return
		}

		// When no web assets are available (e.g. binary built via `go install`
		// without embedded assets), serve a self-contained error page instead
		// of the SPA shell which would render as a blank page.
		if !ws.hasWebAssets() {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, noAssetsPage)
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)

		if ws.shellTmpl == nil {
			http.Error(w, "SPA shell template not available", http.StatusInternalServerError)
			return
		}

		data := spaShellData{
			ShoelaceVersion: shoelaceVersion,
			IsLoginPage:     r.URL.Path == "/login",
			IsInvitePage:    r.URL.Path == "/invite",
			InitialData:     ws.prefetchPageData(r),
		}
		if err := ws.shellTmpl.Execute(w, data); err != nil {
			slog.Error("Failed to render SPA shell", "error", err)
		}
	}
}

// tryServeStaticFile attempts to serve the request as a static file from the
// asset filesystem. Returns true if the file was found and served, false if
// the path does not correspond to a real file.
func (ws *WebServer) tryServeStaticFile(w http.ResponseWriter, r *http.Request) bool {
	// Clean the path and strip the leading slash for fs.Open.
	name := strings.TrimPrefix(filepath.ToSlash(r.URL.Path), "/")
	if name == "" {
		return false
	}

	if ws.assetsDisk != "" {
		p := filepath.Join(ws.assetsDisk, filepath.FromSlash(name))
		info, err := os.Stat(p)
		if err != nil || info.IsDir() {
			return false
		}
	} else if ws.assets != nil {
		f, err := ws.assets.Open(name)
		if err != nil {
			return false
		}
		f.Close()
	} else {
		return false
	}

	ws.serveStaticAsset(w, r)
	return true
}

// handleSSE serves the Server-Sent Events endpoint. It subscribes to the
// in-process ChannelEventPublisher and streams matching events to the browser.
// Route: GET /events?sub=<pattern>&sub=<pattern>...
func (ws *WebServer) handleSSE(w http.ResponseWriter, r *http.Request) {
	if ws.events == nil {
		http.Error(w, "event streaming not configured", http.StatusServiceUnavailable)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	subjects := r.URL.Query()["sub"]
	if len(subjects) == 0 {
		http.Error(w, "at least one sub parameter required", http.StatusBadRequest)
		return
	}

	if errMsg := validateSSESubjects(subjects); errMsg != "" {
		http.Error(w, errMsg, http.StatusBadRequest)
		return
	}

	// Disable the server's WriteTimeout for this long-lived SSE connection.
	// Without this, the global WriteTimeout (e.g. 60s) kills the stream,
	// causing reconnection churn and wasted connection-pool slots.
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		slog.Debug("Failed to clear write deadline for SSE", "error", err)
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher.Flush()

	ch, unsubscribe := ws.events.Subscribe(subjects...)
	defer unsubscribe()

	eventID := 0
	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case evt, ok := <-ch:
			if !ok {
				// Publisher closed
				return
			}
			eventID++
			// Wrap subject + data into the shape the client expects:
			//   event: update
			//   data: {"subject":"project.xxx.agent.created","data":{...}}
			// The client's SSEClient listens for event type "update" and
			// the StateManager parses the subject to route the event.
			fmt.Fprintf(w, "id: %d\nevent: update\ndata: {\"subject\":%q,\"data\":%s}\n\n",
				eventID, evt.Subject, evt.Data)
			flusher.Flush()
		case <-heartbeat.C:
			fmt.Fprintf(w, ":heartbeat %d\n\n", time.Now().UnixMilli())
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// validateSSESubjects validates the subject patterns for SSE subscriptions.
// Returns an error message if invalid, or empty string if all subjects are valid.
func validateSSESubjects(subjects []string) string {
	for _, sub := range subjects {
		if sub == "" {
			return "subject pattern must not be empty"
		}
		if len(sub) > 256 {
			return fmt.Sprintf("subject pattern too long: %d characters (max 256)", len(sub))
		}
		tokens := strings.Split(sub, ".")
		for i, token := range tokens {
			if token == "" {
				return fmt.Sprintf("invalid subject %q: empty token", sub)
			}
			// '>' must be the last token
			if token == ">" && i != len(tokens)-1 {
				return fmt.Sprintf("invalid subject %q: '>' must be the last token", sub)
			}
			// '*' must be a complete token (not mixed like "foo*bar")
			if strings.Contains(token, "*") && token != "*" {
				return fmt.Sprintf("invalid subject %q: '*' must be a complete token", sub)
			}
			// Check for allowed characters
			for _, c := range token {
				if !isAllowedSubjectChar(c) {
					return fmt.Sprintf("invalid subject %q: invalid character %q", sub, string(c))
				}
			}
		}
	}
	return ""
}

// isAllowedSubjectChar returns true if the character is valid in a subject token.
func isAllowedSubjectChar(c rune) bool {
	return (c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') ||
		c == '-' || c == '_' ||
		c == '*' || c == '>'
}

// isPublicRoute returns true for routes that do not require authentication.
func isPublicRoute(path string) bool {
	switch {
	case path == "/healthz":
		return true
	case strings.HasPrefix(path, "/assets/"):
		return true
	case strings.HasPrefix(path, "/shoelace/"):
		return true
	case strings.HasPrefix(path, "/auth/"):
		return true
	case strings.HasPrefix(path, "/api/v1/"):
		// Hub API routes have their own auth (UnifiedAuth middleware).
		// Let them pass through the Web session auth layer untouched.
		return true
	case path == "/login":
		return true
	case path == "/invite":
		return true
	case isRootLevelStaticFile(path): // e.g. /favicon.ico, /scion-notification-icon.png
		return true
	default:
		return false
	}
}

// isRootLevelStaticFile returns true for root-level paths that look like
// static file requests (e.g. /favicon.ico, /scion-notification-icon.png,
// /robots.txt). These are files placed in web/public/ and served at the
// root by Vite. They have a file extension and no sub-path segments.
func isRootLevelStaticFile(path string) bool {
	// Must start with / and have no further slashes (root-level only).
	if len(path) < 2 || strings.Count(path, "/") != 1 {
		return false
	}
	return filepath.Ext(path) != ""
}

// isBrowserRequest returns true if the request appears to come from a browser.
func isBrowserRequest(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	return strings.Contains(accept, "text/html")
}

// devAuthMiddleware auto-populates the session with the dev user identity
// when a dev token is configured and no user is already in the session.
func (ws *WebServer) devAuthMiddleware(next http.Handler) http.Handler {
	if ws.config.DevAuthToken == "" {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session, err := ws.sessionStore.Get(r, webSessionName)
		if err != nil {
			// Session decode error — create fresh session
			session, _ = ws.sessionStore.New(r, webSessionName)
		}

		// If user already in session, load into context and continue
		if uid, ok := session.Values[sessKeyUserID].(string); ok && uid != "" {
			user := &webSessionUser{
				UserID:    uid,
				Email:     sessionString(session, sessKeyUserEmail),
				Name:      sessionString(session, sessKeyUserName),
				AvatarURL: sessionString(session, sessKeyUserAvatar),
				Role:      sessionString(session, sessKeyUserRole),
			}
			ctx := context.WithValue(r.Context(), webUserContextKey{}, user)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		// No user — auto-login with dev identity
		devUser := &webSessionUser{
			UserID:    DevUserID,
			Email:     "dev@localhost",
			Name:      "Development User",
			AvatarURL: "",
			Role:      "admin",
		}

		session.Values[sessKeyUserID] = devUser.UserID
		session.Values[sessKeyUserEmail] = devUser.Email
		session.Values[sessKeyUserName] = devUser.Name
		session.Values[sessKeyUserAvatar] = devUser.AvatarURL
		session.Values[sessKeyUserRole] = devUser.Role

		// Generate Hub JWTs so the session-to-bearer middleware can
		// authenticate API requests, mirroring handleOAuthCallback.
		if ws.userTokenSvc != nil {
			if _, ok := session.Values[sessKeyHubAccessToken].(string); !ok {
				accessToken, refreshToken, expiresIn, err := ws.userTokenSvc.GenerateTokenPair(
					devUser.UserID, devUser.Email, devUser.Name, "admin", ClientTypeWeb,
				)
				if err == nil {
					session.Values[sessKeyHubAccessToken] = accessToken
					session.Values[sessKeyHubRefreshToken] = refreshToken
					session.Values[sessKeyHubTokenExpiry] = time.Now().Add(time.Duration(expiresIn) * time.Second).UnixMilli()
				}
			}
		}

		if err := session.Save(r, w); err != nil {
			slog.Error("Failed to save dev-auth session", "error", err)
		}

		ctx := context.WithValue(r.Context(), webUserContextKey{}, devUser)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// sessionAuthMiddleware protects web routes by requiring an authenticated session.
func (ws *WebServer) sessionAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip auth for public routes
		if isPublicRoute(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		// If user already in context (e.g., set by devAuthMiddleware), proceed
		if getWebSessionUser(r.Context()) != nil {
			next.ServeHTTP(w, r)
			return
		}

		// Load session
		session, err := ws.sessionStore.Get(r, webSessionName)
		if err != nil {
			session, _ = ws.sessionStore.New(r, webSessionName)
		}

		// Check for user in session
		if uid, ok := session.Values[sessKeyUserID].(string); ok && uid != "" {
			user := &webSessionUser{
				UserID:    uid,
				Email:     sessionString(session, sessKeyUserEmail),
				Name:      sessionString(session, sessKeyUserName),
				AvatarURL: sessionString(session, sessKeyUserAvatar),
				Role:      sessionString(session, sessKeyUserRole),
			}
			ctx := context.WithValue(r.Context(), webUserContextKey{}, user)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		// No authenticated user
		if isBrowserRequest(r) {
			// Store returnTo for post-login redirect
			session.Values[sessKeyReturnTo] = r.URL.Path
			_ = session.Save(r, w)
			http.Redirect(w, r, "/auth/login", http.StatusFound)
			return
		}

		// Non-browser request: return 401 JSON
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "authentication required",
		})
	})
}

// handleOAuthLogin initiates the OAuth flow for a given provider.
// Route: GET /auth/login/{provider}
// Also handles GET /auth/login (redirects to /login page).
func (ws *WebServer) handleOAuthLogin(w http.ResponseWriter, r *http.Request) {
	// Extract provider from path: /auth/login/{provider}
	provider := strings.TrimPrefix(r.URL.Path, "/auth/login/")
	provider = strings.TrimSuffix(provider, "/")

	// If no provider specified, redirect to SPA login page
	if provider == "" {
		session, err := ws.sessionStore.Get(r, webSessionName)
		if err != nil {
			session, _ = ws.sessionStore.New(r, webSessionName)
		}
		if returnTo := r.URL.Query().Get("returnTo"); returnTo != "" {
			session.Values[sessKeyReturnTo] = returnTo
			_ = session.Save(r, w)
		}
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	// Validate provider
	if provider != "google" && provider != "github" {
		http.Error(w, "unsupported OAuth provider", http.StatusBadRequest)
		return
	}

	// Check that OAuth service is available
	if ws.oauthService == nil {
		http.Error(w, "OAuth not configured", http.StatusServiceUnavailable)
		return
	}

	if !ws.oauthService.IsProviderConfiguredForClient(OAuthClientTypeWeb, provider) {
		http.Error(w, fmt.Sprintf("OAuth provider %s is not configured", provider), http.StatusBadRequest)
		return
	}

	// Generate state for CSRF protection
	stateBytes := make([]byte, 32)
	if _, err := rand.Read(stateBytes); err != nil {
		http.Error(w, "failed to generate state", http.StatusInternalServerError)
		return
	}
	state := hex.EncodeToString(stateBytes)

	// Store state in session
	session, err := ws.sessionStore.Get(r, webSessionName)
	if err != nil {
		session, _ = ws.sessionStore.New(r, webSessionName)
	}
	session.Values[sessKeyOAuthState] = state
	if err := session.Save(r, w); err != nil {
		slog.Error("Failed to save OAuth state to session", "error", err)
		http.Error(w, "session error", http.StatusInternalServerError)
		return
	}

	// Build redirect URI
	redirectURI := ws.config.BaseURL + "/auth/callback/" + provider

	// Get authorization URL
	authURL, err := ws.oauthService.GetAuthorizationURLForClient(OAuthClientTypeWeb, provider, redirectURI, state)
	if err != nil {
		slog.Error("Failed to generate OAuth URL", "provider", provider, "error", err)
		http.Error(w, "failed to generate auth URL", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, authURL, http.StatusFound)
}

// handleOAuthCallback handles the OAuth provider callback.
// Route: GET /auth/callback/{provider}
func (ws *WebServer) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	// Extract provider
	provider := strings.TrimPrefix(r.URL.Path, "/auth/callback/")
	provider = strings.TrimSuffix(provider, "/")

	if provider != "google" && provider != "github" {
		http.Error(w, "unsupported OAuth provider", http.StatusBadRequest)
		return
	}

	if ws.oauthService == nil || ws.store == nil {
		http.Error(w, "OAuth not configured", http.StatusServiceUnavailable)
		return
	}

	// Load session
	session, err := ws.sessionStore.Get(r, webSessionName)
	if err != nil {
		slog.Error("Failed to load session for callback", "error", err)
		http.Redirect(w, r, "/login?error=session_error", http.StatusFound)
		return
	}

	// Validate state (CSRF protection)
	expectedState, _ := session.Values[sessKeyOAuthState].(string)
	actualState := r.URL.Query().Get("state")
	if expectedState == "" || !apiclient.ValidateDevToken(actualState, expectedState) {
		slog.Warn("OAuth state mismatch", "provider", provider)
		http.Redirect(w, r, "/login?error=state_mismatch", http.StatusFound)
		return
	}

	// Clear state from session
	delete(session.Values, sessKeyOAuthState)

	// Check for error from provider
	if errParam := r.URL.Query().Get("error"); errParam != "" {
		slog.Warn("OAuth provider returned error", "provider", provider, "error", errParam)
		http.Redirect(w, r, "/login?error="+errParam, http.StatusFound)
		return
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		http.Redirect(w, r, "/login?error=no_code", http.StatusFound)
		return
	}

	// Build redirect URI (must match the one used in the login request)
	redirectURI := ws.config.BaseURL + "/auth/callback/" + provider

	// Exchange code for user info (direct function call, no HTTP)
	ctx := r.Context()
	userInfo, err := ws.oauthService.ExchangeCodeForClient(ctx, OAuthClientTypeWeb, provider, code, redirectURI)
	if err != nil {
		slog.Error("OAuth code exchange failed", "provider", provider, "error", err)
		http.Redirect(w, r, "/login?error=exchange_failed", http.StatusFound)
		return
	}

	// Check if user is authorized (admin bypass, domain check, access mode)
	if !checkUserAuthorized(ctx, userInfo.Email, ws.config.AuthorizedDomains, ws.config.AdminEmails, ws.config.UserAccessMode, ws.store) {
		slog.Warn("Unauthorized user", "email", userInfo.Email)
		http.Redirect(w, r, "/login?error=unauthorized_domain", http.StatusFound)
		return
	}

	// Find or create user
	user, err := ws.store.GetUserByEmail(ctx, userInfo.Email)
	if err != nil {
		// Create new user
		role := determineUserRole(userInfo.Email, ws.config.AdminEmails)
		user = &store.User{
			ID:          generateID(),
			Email:       userInfo.Email,
			DisplayName: userInfo.DisplayName,
			AvatarURL:   userInfo.AvatarURL,
			Role:        role,
			Status:      "active",
			Created:     time.Now(),
			LastLogin:   time.Now(),
		}
		if err := ws.store.CreateUser(ctx, user); err != nil {
			slog.Error("Failed to create user", "email", userInfo.Email, "error", err)
			http.Redirect(w, r, "/login?error=user_create_failed", http.StatusFound)
			return
		}
	} else {
		// Update last login
		user.LastLogin = time.Now()
		if userInfo.AvatarURL != "" && user.AvatarURL == "" {
			user.AvatarURL = userInfo.AvatarURL
		}
		if userInfo.DisplayName != "" && user.DisplayName == "" {
			user.DisplayName = userInfo.DisplayName
		}
		if err := ws.store.UpdateUser(ctx, user); err != nil {
			slog.Warn("Failed to update user on login", "email", userInfo.Email, "error", err)
		}
	}

	// Ensure user is a member of the hub-members group
	ensureHubMembership(ctx, ws.store, user.ID)

	// Generate Hub tokens if token service is available
	if ws.userTokenSvc != nil {
		accessToken, refreshToken, expiresIn, err := ws.userTokenSvc.GenerateTokenPair(
			user.ID, user.Email, user.DisplayName, user.Role, ClientTypeWeb,
		)
		if err != nil {
			slog.Warn("Failed to generate Hub tokens", "error", err)
		} else {
			session.Values[sessKeyHubAccessToken] = accessToken
			session.Values[sessKeyHubRefreshToken] = refreshToken
			session.Values[sessKeyHubTokenExpiry] = time.Now().Add(time.Duration(expiresIn) * time.Second).UnixMilli()
		}
	}

	// Store user info in session
	session.Values[sessKeyUserID] = user.ID
	session.Values[sessKeyUserEmail] = user.Email
	session.Values[sessKeyUserName] = user.DisplayName
	session.Values[sessKeyUserAvatar] = user.AvatarURL
	session.Values[sessKeyUserRole] = user.Role

	// Get returnTo and clear it
	returnTo, _ := session.Values[sessKeyReturnTo].(string)
	delete(session.Values, sessKeyReturnTo)

	if err := session.Save(r, w); err != nil {
		slog.Error("Failed to save session after OAuth callback", "error", err)
		http.Redirect(w, r, "/login?error=session_error", http.StatusFound)
		return
	}

	if returnTo == "" {
		returnTo = "/"
	}
	http.Redirect(w, r, returnTo, http.StatusFound)
}

// handleLogout clears the session and redirects to login (or returns JSON for API).
// Route: GET /auth/logout, POST /auth/logout
func (ws *WebServer) handleLogout(w http.ResponseWriter, r *http.Request) {
	session, err := ws.sessionStore.Get(r, webSessionName)
	if err != nil {
		session, _ = ws.sessionStore.New(r, webSessionName)
	}

	// Clear all session values
	for key := range session.Values {
		delete(session.Values, key)
	}
	session.Options.MaxAge = -1 // Delete cookie
	if err := session.Save(r, w); err != nil {
		slog.Error("Failed to clear session on logout", "error", err)
	}

	if isBrowserRequest(r) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// handleAuthMe returns the current user from the session as JSON.
// Route: GET /auth/me
func (ws *WebServer) handleAuthMe(w http.ResponseWriter, r *http.Request) {
	// Check context first (set by devAuthMiddleware or sessionAuthMiddleware)
	if user := getWebSessionUser(r.Context()); user != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(user)
		return
	}

	// Fall back to loading from session directly
	session, err := ws.sessionStore.Get(r, webSessionName)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "authentication required"})
		return
	}

	uid, ok := session.Values[sessKeyUserID].(string)
	if !ok || uid == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "authentication required"})
		return
	}

	user := &webSessionUser{
		UserID:    uid,
		Email:     sessionString(session, sessKeyUserEmail),
		Name:      sessionString(session, sessKeyUserName),
		AvatarURL: sessionString(session, sessKeyUserAvatar),
		Role:      sessionString(session, sessKeyUserRole),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(user)
}

// handleAuthProviders returns which OAuth providers are enabled for web login.
// Route: GET /auth/providers
func (ws *WebServer) handleAuthProviders(w http.ResponseWriter, r *http.Request) {
	resp := map[string]bool{
		"google": false,
		"github": false,
	}
	if ws.oauthService != nil {
		resp["google"] = ws.oauthService.IsProviderConfiguredForClient(OAuthClientTypeWeb, "google")
		resp["github"] = ws.oauthService.IsProviderConfiguredForClient(OAuthClientTypeWeb, "github")
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleAuthDebug returns session debug info (debug mode only).
// Route: GET /auth/debug
func (ws *WebServer) handleAuthDebug(w http.ResponseWriter, r *http.Request) {
	if !ws.config.Debug {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	session, err := ws.sessionStore.Get(r, webSessionName)
	if err != nil {
		session, _ = ws.sessionStore.New(r, webSessionName)
	}

	debug := map[string]interface{}{
		"sessionIsNew":   session.IsNew,
		"hasUser":        session.Values[sessKeyUserID] != nil,
		"hasAccessToken": session.Values[sessKeyHubAccessToken] != nil,
		"config": map[string]interface{}{
			"baseURL":         ws.config.BaseURL,
			"devAuthEnabled":  ws.config.DevAuthToken != "",
			"oauthConfigured": ws.oauthService != nil,
			"storeConfigured": ws.store != nil,
		},
	}

	if uid, ok := session.Values[sessKeyUserID].(string); ok {
		debug["user"] = map[string]string{
			"id":    uid,
			"email": sessionString(session, sessKeyUserEmail),
			"name":  sessionString(session, sessKeyUserName),
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(debug)
}

// sessionString is a helper to safely extract a string from session values.
func sessionString(session *sessions.Session, key string) string {
	if v, ok := session.Values[key].(string); ok {
		return v
	}
	return ""
}

// securityHeadersMiddleware adds security headers to all responses.
func (ws *WebServer) securityHeadersMiddleware(next http.Handler) http.Handler {
	// Build CSP matching the Koa server's policy (web/src/server/config.ts:154-162)
	csp := strings.Join([]string{
		"default-src 'self'",
		"script-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net https://cdn.webawesome.com",
		"style-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net https://cdn.webawesome.com https://fonts.googleapis.com",
		"font-src 'self' https://fonts.gstatic.com https://cdn.jsdelivr.net https://cdn.webawesome.com",
		"img-src 'self' data: https:",
		"connect-src 'self' data: ws: wss: http://localhost:* http://127.0.0.1:*",
	}, "; ")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", csp)
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-XSS-Protection", "1; mode=block")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Permissions-Policy", "accelerometer=(), camera=(), geolocation=(), gyroscope=(), magnetometer=(), microphone=(), payment=(), usb=()")
		next.ServeHTTP(w, r)
	})
}

// loggingMiddleware logs incoming requests.
func (ws *WebServer) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		wrapped := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		next.ServeHTTP(wrapped, r)

		if ws.config.Debug || wrapped.statusCode >= 400 {
			slog.Info("Web request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", wrapped.statusCode),
				slog.Duration("duration", time.Since(start)),
			)
		}
	})
}

// buildHandler constructs the full middleware chain and returns the HTTP handler.
func (ws *WebServer) buildHandler() http.Handler {
	var handler http.Handler = ws.mux

	// Admin mode middleware (innermost — runs after session user is loaded).
	// Always applied — checks runtime MaintenanceState on each request.
	handler = ws.adminModeWebMiddleware(handler)

	// Session auth middleware (loads session user into context, redirects to login)
	handler = ws.sessionAuthMiddleware(handler)

	// Dev-auth middleware (auto-populates session when dev token configured)
	handler = ws.devAuthMiddleware(handler)

	// Security headers
	handler = ws.securityHeadersMiddleware(handler)

	// Request logging (outermost)
	if ws.requestLogger != nil {
		handler = logging.RequestLogMiddleware(ws.requestLogger, "web", nil)(handler)
	} else {
		handler = ws.loggingMiddleware(handler)
	}

	return handler
}

// Start starts the web frontend HTTP server.
// The server uses h2c (HTTP/2 cleartext) so browsers can multiplex all
// requests over a single TCP connection without TLS. This prevents the
// HTTP/1.1 six-connection-per-origin limit from blocking requests behind
// long-lived SSE streams. When deployed behind a TLS-terminating reverse
// proxy (e.g. Caddy), the proxy negotiates HTTP/2 with the browser natively.
func (ws *WebServer) Start(ctx context.Context) error {
	handler := ws.buildHandler()

	// Wrap the handler with h2c to support HTTP/2 cleartext upgrades.
	h2s := &http2.Server{}
	h2cHandler := h2c.NewHandler(handler, h2s)

	ws.httpServer = &http.Server{
		Addr:         fmt.Sprintf("%s:%d", ws.config.Host, ws.config.Port),
		Handler:      h2cHandler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
	}

	slog.Info("Web frontend server starting", "host", ws.config.Host, "port", ws.config.Port, "h2c", true)

	errCh := make(chan error, 1)
	go func() {
		if err := ws.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		return ws.Shutdown(context.Background())
	}
}

// Shutdown gracefully shuts down the web server and any mounted Hub resources.
func (ws *WebServer) Shutdown(ctx context.Context) error {
	if ws.httpServer == nil {
		return nil
	}

	slog.Info("Web frontend server shutting down...")

	// Clean up mounted Hub resources (control channels, broker auth, event publisher)
	if ws.hubShutdown != nil {
		if err := ws.hubShutdown(ctx); err != nil {
			slog.Error("Failed to clean up Hub resources during web server shutdown", "error", err)
		}
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	return ws.httpServer.Shutdown(ctx)
}

// Handler returns the HTTP handler for testing without starting a listener.
func (ws *WebServer) Handler() http.Handler {
	return ws.buildHandler()
}
