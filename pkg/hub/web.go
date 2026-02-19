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
	"strings"
	"time"

	"github.com/gorilla/sessions"
	"github.com/ptone/scion-agent/pkg/apiclient"
	"github.com/ptone/scion-agent/pkg/store"
	"github.com/ptone/scion-agent/web"
)

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
}

// WebServer serves the web frontend SPA shell and static assets.
type WebServer struct {
	config       WebServerConfig
	httpServer   *http.Server
	mux          *http.ServeMux
	assets       fs.FS              // embedded or nil
	assetsDisk   string             // filesystem override path, or ""
	shellTmpl    *template.Template
	sessionStore *sessions.CookieStore
	oauthService *OAuthService
	store        store.Store
	userTokenSvc *UserTokenService
}

// spaShellTemplate is the Go html/template for the SPA shell page.
// It mirrors the structure from web/src/server/ssr/templates.ts but renders
// a client-only shell (no SSR content).
var spaShellTemplate = `<!DOCTYPE html>
<html lang="en" data-theme="light">
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
    <link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/@shoelace-style/shoelace@{{.ShoelaceVersion}}/cdn/themes/dark.css" media="(prefers-color-scheme: dark)">
    <script type="module" src="https://cdn.jsdelivr.net/npm/@shoelace-style/shoelace@{{.ShoelaceVersion}}/cdn/shoelace-autoloader.js"></script>

    <!-- Initial state for hydration -->
    <script id="__SCION_DATA__" type="application/json">{}</script>

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
        scion-nav:not(:defined),
        scion-header:not(:defined),
        scion-breadcrumb:not(:defined),
        scion-status-badge:not(:defined),
        scion-page-home:not(:defined),
        scion-page-groves:not(:defined),
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
            }
        })();
    </script>
</head>
<body>
    <div id="app"><scion-app></scion-app></div>

    <!-- Client entry point -->
    <script type="module" src="/assets/main.js"></script>
</body>
</html>`

// spaShellData holds the template data for the SPA shell.
type spaShellData struct {
	ShoelaceVersion string
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
		config: cfg,
		mux:    http.NewServeMux(),
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

	cookieStore := sessions.NewCookieStore([]byte(sessionKey))
	cookieStore.Options = &sessions.Options{
		Path:     "/",
		MaxAge:   86400, // 24 hours
		HttpOnly: true,
		Secure:   strings.HasPrefix(cfg.BaseURL, "https://"),
		SameSite: http.SameSiteLaxMode,
	}
	ws.sessionStore = cookieStore

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

// registerRoutes sets up the web server routes.
func (ws *WebServer) registerRoutes() {
	ws.mux.HandleFunc("/healthz", ws.handleHealthz)
	ws.mux.Handle("/assets/", ws.staticHandler())
	// Auth routes (no session auth required)
	ws.mux.HandleFunc("/auth/login/", ws.handleOAuthLogin)
	ws.mux.HandleFunc("/auth/callback/", ws.handleOAuthCallback)
	ws.mux.HandleFunc("/auth/logout", ws.handleLogout)
	ws.mux.HandleFunc("/auth/me", ws.handleAuthMe)
	ws.mux.HandleFunc("/auth/debug", ws.handleAuthDebug)
	// SPA catch-all (protected by session auth middleware)
	ws.mux.HandleFunc("/", ws.spaHandler())
}

// handleHealthz returns the web server health status.
func (ws *WebServer) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"status":"ok","component":"web","assetsDir":"%s","assetsEmbedded":%t}`,
		ws.config.AssetsDir, web.AssetsEmbedded)
}

// staticHandler returns an http.Handler that serves static assets.
func (ws *WebServer) staticHandler() http.Handler {
	if ws.assetsDisk == "" && ws.assets == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "no assets available", http.StatusNotFound)
		})
	}

	var fileServer http.Handler
	if ws.assetsDisk != "" {
		fileServer = http.FileServer(http.Dir(ws.assetsDisk))
	} else {
		fileServer = http.FileServer(http.FS(ws.assets))
	}

	return http.StripPrefix("/assets/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Set cache headers based on whether the filename contains a hash.
		// Vite hashed assets (e.g., chunk-abc123.js) get long-lived caching.
		// Non-hashed entry points (e.g., main.js) get revalidation.
		if isHashedAsset(r.URL.Path) {
			w.Header().Set("Cache-Control", "public, max-age=86400")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		fileServer.ServeHTTP(w, r)
	}))
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

// spaHandler returns the SPA shell HTML for any route not matched by other handlers.
func (ws *WebServer) spaHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)

		if ws.shellTmpl == nil {
			http.Error(w, "SPA shell template not available", http.StatusInternalServerError)
			return
		}

		data := spaShellData{
			ShoelaceVersion: shoelaceVersion,
		}
		if err := ws.shellTmpl.Execute(w, data); err != nil {
			slog.Error("Failed to render SPA shell", "error", err)
		}
	}
}

// isPublicRoute returns true for routes that do not require authentication.
func isPublicRoute(path string) bool {
	switch {
	case path == "/healthz":
		return true
	case strings.HasPrefix(path, "/assets/"):
		return true
	case strings.HasPrefix(path, "/auth/"):
		return true
	case path == "/login":
		return true
	case path == "/favicon.ico":
		return true
	default:
		return false
	}
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
			}
			ctx := context.WithValue(r.Context(), webUserContextKey{}, user)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		// No user — auto-login with dev identity
		devUser := &webSessionUser{
			UserID:    "dev-user",
			Email:     "dev@localhost",
			Name:      "Development User",
			AvatarURL: "",
		}

		session.Values[sessKeyUserID] = devUser.UserID
		session.Values[sessKeyUserEmail] = devUser.Email
		session.Values[sessKeyUserName] = devUser.Name
		session.Values[sessKeyUserAvatar] = devUser.AvatarURL
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

	// Check email authorization
	if !isEmailAuthorized(userInfo.Email, ws.config.AuthorizedDomains, ws.config.AdminEmails) {
		slog.Warn("Unauthorized email domain", "email", userInfo.Email)
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
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(user)
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
		"sessionIsNew": session.IsNew,
		"hasUser":      session.Values[sessKeyUserID] != nil,
		"config": map[string]interface{}{
			"baseURL":        ws.config.BaseURL,
			"devAuthEnabled": ws.config.DevAuthToken != "",
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
		"connect-src 'self' ws: wss: http://localhost:* http://127.0.0.1:*",
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

	// Session auth middleware (innermost, checks session for protected routes)
	handler = ws.sessionAuthMiddleware(handler)

	// Dev-auth middleware (auto-populates session when dev token configured)
	handler = ws.devAuthMiddleware(handler)

	// Security headers
	handler = ws.securityHeadersMiddleware(handler)

	// Request logging (outermost)
	handler = ws.loggingMiddleware(handler)

	return handler
}

// Start starts the web frontend HTTP server.
func (ws *WebServer) Start(ctx context.Context) error {
	handler := ws.buildHandler()

	ws.httpServer = &http.Server{
		Addr:         fmt.Sprintf("%s:%d", ws.config.Host, ws.config.Port),
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
	}

	slog.Info("Web frontend server starting", "host", ws.config.Host, "port", ws.config.Port)

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

// Shutdown gracefully shuts down the web server.
func (ws *WebServer) Shutdown(ctx context.Context) error {
	if ws.httpServer == nil {
		return nil
	}

	slog.Info("Web frontend server shutting down...")

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	return ws.httpServer.Shutdown(ctx)
}

// Handler returns the HTTP handler for testing without starting a listener.
func (ws *WebServer) Handler() http.Handler {
	return ws.buildHandler()
}
