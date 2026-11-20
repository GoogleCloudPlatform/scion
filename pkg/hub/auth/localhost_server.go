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

// Package auth provides authentication utilities for CLI-to-Hub communication.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// CallbackPort is the port for the localhost callback server.
	CallbackPort = 18271
	// CallbackPath is the path for the OAuth callback.
	CallbackPath = "/callback"
	// DefaultTimeout is the default timeout for waiting for authentication.
	DefaultTimeout = 5 * time.Minute

	defaultCallbackBrandName  = "Scion"
	callbackAttributionShown  = "shown"
	callbackAttributionHidden = "hidden"
	maxCallbackBrandNameRunes = 64
	maxCallbackLogoURLLen     = 2048
)

// CallbackColors holds validated #RRGGBB hex tokens for one theme mode of the
// localhost OAuth callback HTML page.
type CallbackColors struct {
	Bg        string
	Surface   string
	Text      string
	TextMuted string
	Border    string
	Accent    string
}

// CallbackBranding holds optional Hub branding applied to the localhost OAuth
// callback HTML pages. Every color is validated as #RRGGBB before rendering;
// invalid or empty fields fall back to stock Scion defaults.
type CallbackBranding struct {
	Name        string
	Attribution string
	LogoLight   string
	LogoDark    string
	Light       CallbackColors
	Dark        CallbackColors
}

func defaultCallbackLightColors() CallbackColors {
	return CallbackColors{
		Bg:        "#f8fafc",
		Surface:   "#ffffff",
		Text:      "#1e293b",
		TextMuted: "#64748b",
		Border:    "#e2e8f0",
		Accent:    "#3b82f6",
	}
}

func defaultCallbackDarkColors() CallbackColors {
	return CallbackColors{
		Bg:        "#0f172a",
		Surface:   "#1e293b",
		Text:      "#f1f5f9",
		TextMuted: "#94a3b8",
		Border:    "#334155",
		Accent:    "#60a5fa",
	}
}

func isValidHexColor(s string) bool {
	if len(s) != 7 || s[0] != '#' {
		return false
	}
	for i := 1; i < 7; i++ {
		c := s[i]
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !isHex {
			return false
		}
	}
	return true
}

func pickHexOrDefault(val, fallback string) string {
	if isValidHexColor(val) {
		return strings.ToLower(val)
	}
	return fallback
}

func sanitizeCallbackColors(in, def CallbackColors) CallbackColors {
	return CallbackColors{
		Bg:        pickHexOrDefault(in.Bg, def.Bg),
		Surface:   pickHexOrDefault(in.Surface, def.Surface),
		Text:      pickHexOrDefault(in.Text, def.Text),
		TextMuted: pickHexOrDefault(in.TextMuted, def.TextMuted),
		Border:    pickHexOrDefault(in.Border, def.Border),
		Accent:    pickHexOrDefault(in.Accent, def.Accent),
	}
}

func sanitizeCallbackName(raw string) string {
	if !utf8.ValidString(raw) {
		return defaultCallbackBrandName
	}
	for _, r := range raw {
		if unicode.IsControl(r) || r == 0x061C || r == 0x200E || r == 0x200F ||
			(r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069) {
			return defaultCallbackBrandName
		}
	}
	trimmed := strings.TrimSpace(raw)
	n := utf8.RuneCountInString(trimmed)
	if n == 0 || n > maxCallbackBrandNameRunes {
		return defaultCallbackBrandName
	}
	return trimmed
}

func sanitizeCallbackLogoURL(raw string) string {
	if raw == "" || len(raw) > maxCallbackLogoURLLen || !utf8.ValidString(raw) {
		return ""
	}
	for _, r := range raw {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return ""
		}
	}
	if strings.ContainsAny(raw, "\"'`()<>\\") {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return ""
	}
	if strings.EqualFold(u.Scheme, "https") {
		return raw
	}
	if strings.EqualFold(u.Scheme, "http") {
		host := u.Hostname()
		if host == "localhost" || host == "127.0.0.1" || host == "::1" {
			return raw
		}
	}
	return ""
}

func renderCallbackPage(b *CallbackBranding, pageTitle, iconClass, iconEntity, heading, message string) string {
	name := defaultCallbackBrandName
	attribution := callbackAttributionShown
	var logoLight, logoDark string
	light := defaultCallbackLightColors()
	dark := defaultCallbackDarkColors()

	if b != nil {
		name = sanitizeCallbackName(b.Name)
		if b.Attribution == callbackAttributionHidden {
			attribution = callbackAttributionHidden
		}
		logoLight = sanitizeCallbackLogoURL(b.LogoLight)
		logoDark = sanitizeCallbackLogoURL(b.LogoDark)
		if logoLight == "" && logoDark != "" {
			logoLight = logoDark
		}
		light = sanitizeCallbackColors(b.Light, light)
		dark = sanitizeCallbackColors(b.Dark, dark)
	}

	fullTitle := pageTitle
	if name != defaultCallbackBrandName {
		fullTitle = name + " - " + pageTitle
	}

	var logoHTML string
	if logoLight != "" {
		alt := html.EscapeString(name)
		if logoDark != "" && logoDark != logoLight {
			logoHTML = fmt.Sprintf(
				`<picture class="brand-logo-wrap"><source media="(prefers-color-scheme: dark)" srcset="%s"><img class="brand-logo" src="%s" alt="%s"></picture>`,
				html.EscapeString(logoDark), html.EscapeString(logoLight), alt,
			)
		} else {
			logoHTML = fmt.Sprintf(`<img class="brand-logo" src="%s" alt="%s">`, html.EscapeString(logoLight), alt)
		}
	}

	badge := "scion"
	if name != defaultCallbackBrandName || logoLight != "" {
		if attribution == callbackAttributionHidden {
			badge = name
		} else {
			badge = name + " · Powered by Scion"
		}
	}

	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>%s</title>
    <style>
        :root {
            --bg: %s;
            --surface: %s;
            --text: %s;
            --text-muted: %s;
            --border: %s;
            --accent: %s;
            --error-accent: #dc2626;
        }

        @media (prefers-color-scheme: dark) {
            :root {
                --bg: %s;
                --surface: %s;
                --text: %s;
                --text-muted: %s;
                --border: %s;
                --accent: %s;
                --error-accent: #f87171;
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
            background: var(--surface);
            border: 1px solid var(--border);
            padding: 2.5rem 3rem;
            border-radius: 12px;
            max-width: 480px;
            margin: 1rem;
        }

        .brand-logo-wrap {
            display: block;
        }

        .brand-logo {
            max-height: 36px;
            max-width: 180px;
            width: auto;
            height: auto;
            object-fit: contain;
            display: block;
            margin: 0 auto 1.25rem;
        }

        .checkmark {
            font-size: 3rem;
            color: var(--accent);
            margin-bottom: 1rem;
        }

        .error-icon {
            font-size: 3rem;
            color: var(--error-accent);
            margin-bottom: 1rem;
        }

        h1 {
            font-size: 1.5rem;
            font-weight: 600;
            margin-bottom: 0.75rem;
        }

        p {
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
            background: var(--bg);
        }
    </style>
</head>
<body>
    <div class="container">
        %s<div class="%s">%s</div>
        <h1>%s</h1>
        <p>%s</p>
        <span class="badge">%s</span>
    </div>
</body>
</html>`,
		html.EscapeString(fullTitle),
		light.Bg, light.Surface, light.Text, light.TextMuted, light.Border, light.Accent,
		dark.Bg, dark.Surface, dark.Text, dark.TextMuted, dark.Border, dark.Accent,
		logoHTML,
		iconClass,
		iconEntity,
		html.EscapeString(heading),
		html.EscapeString(message),
		html.EscapeString(badge),
	)
}

// LocalhostAuthServer handles the OAuth callback on localhost.
type LocalhostAuthServer struct {
	server   *http.Server
	listener net.Listener
	codeChan chan string
	errChan  chan error
	state    string
	port     int
	branding *CallbackBranding
	mu       sync.Mutex
	started  bool
}

// NewLocalhostAuthServer creates a new localhost authentication server.
func NewLocalhostAuthServer() *LocalhostAuthServer {
	return &LocalhostAuthServer{
		codeChan: make(chan string, 1),
		errChan:  make(chan error, 1),
		port:     CallbackPort,
	}
}

// WithPort sets a custom port for the server.
func (s *LocalhostAuthServer) WithPort(port int) *LocalhostAuthServer {
	s.port = port
	return s
}

// WithBranding sets optional Hub branding for the callback HTML pages.
func (s *LocalhostAuthServer) WithBranding(b *CallbackBranding) *LocalhostAuthServer {
	s.branding = b
	return s
}

// Start starts the localhost callback server and returns the callback URL.
func (s *LocalhostAuthServer) Start(ctx context.Context) (callbackURL string, state string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.started {
		return "", "", fmt.Errorf("server already started")
	}

	// Generate random state for CSRF protection
	s.state, err = generateRandomState()
	if err != nil {
		return "", "", fmt.Errorf("failed to generate state: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc(CallbackPath, s.handleCallback)

	addr := fmt.Sprintf("127.0.0.1:%d", s.port)
	s.listener, err = net.Listen("tcp", addr)
	if err != nil {
		return "", "", fmt.Errorf("failed to listen on %s: %w", addr, err)
	}

	s.server = &http.Server{
		Handler: mux,
	}

	s.started = true

	// Start server in background
	go func() {
		if err := s.server.Serve(s.listener); err != nil && err != http.ErrServerClosed {
			select {
			case s.errChan <- err:
			default:
			}
		}
	}()

	callbackURL = fmt.Sprintf("http://127.0.0.1:%d%s", s.port, CallbackPath)
	return callbackURL, s.state, nil
}

func (s *LocalhostAuthServer) writeCallbackHTML(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// handleCallback handles the OAuth callback request.
func (s *LocalhostAuthServer) handleCallback(w http.ResponseWriter, r *http.Request) {
	// Verify state matches (CSRF protection)
	state := r.URL.Query().Get("state")
	if state != s.state {
		s.sendError(fmt.Errorf("state mismatch"))
		s.writeCallbackHTML(w, http.StatusBadRequest,
			renderCallbackPage(s.branding, "Authentication Failed", "error-icon", "&#10008;", "Authentication Failed", "State mismatch - possible CSRF attack"))
		return
	}

	// Check for error response
	if errCode := r.URL.Query().Get("error"); errCode != "" {
		errDesc := r.URL.Query().Get("error_description")
		if errDesc == "" {
			errDesc = errCode
		}
		s.sendError(fmt.Errorf("auth failed: %s", errDesc))
		s.writeCallbackHTML(w, http.StatusBadRequest,
			renderCallbackPage(s.branding, "Authentication Failed", "error-icon", "&#10008;", "Authentication Failed", errDesc))
		return
	}

	// Get authorization code
	code := r.URL.Query().Get("code")
	if code == "" {
		s.sendError(fmt.Errorf("no authorization code received"))
		s.writeCallbackHTML(w, http.StatusBadRequest,
			renderCallbackPage(s.branding, "Authentication Failed", "error-icon", "&#10008;", "Authentication Failed", "No authorization code received"))
		return
	}

	// Send success page to browser
	s.writeCallbackHTML(w, http.StatusOK,
		renderCallbackPage(s.branding, "Authentication Successful", "checkmark", "&#10004;", "Authentication Successful", "You can close this window and return to the terminal."))

	// Send code through channel
	select {
	case s.codeChan <- code:
	default:
		// Channel full, ignore duplicate callback
	}
}

// sendError sends an error through the error channel.
func (s *LocalhostAuthServer) sendError(err error) {
	select {
	case s.errChan <- err:
	default:
		// Channel full, ignore duplicate error
	}
}

// WaitForCode waits for the authorization code or timeout.
func (s *LocalhostAuthServer) WaitForCode(ctx context.Context) (string, error) {
	select {
	case code := <-s.codeChan:
		return code, nil
	case err := <-s.errChan:
		return "", err
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(DefaultTimeout):
		return "", fmt.Errorf("authentication timeout after %v", DefaultTimeout)
	}
}

// Shutdown gracefully shuts down the server.
func (s *LocalhostAuthServer) Shutdown() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.started || s.server == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	s.started = false
	return s.server.Shutdown(ctx)
}

// GetState returns the state parameter for this session.
func (s *LocalhostAuthServer) GetState() string {
	return s.state
}

// generateRandomState generates a cryptographically random state string.
func generateRandomState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(b), nil
}
