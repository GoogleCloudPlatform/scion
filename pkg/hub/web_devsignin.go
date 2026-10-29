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
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/gorilla/sessions"
)

// Dev sign-in (dev auth only). A web session for the dev user is created
// only from a sign-in link: the holder of the dev token (the CLI that
// started the server) asks for a code at devLaunchPath, then opens
// devSignInPath?code=... in the browser. The code is random, single use,
// short lived, kept only as a hash in this process's memory and never
// logged. A request with no session and no code gets no dev session: it
// takes the normal login path.
const (
	// devLaunchPath mints a sign-in code: POST, Authorization: Bearer
	// <dev token>.
	devLaunchPath = "/auth/dev-launch"
	// devSignInPath exchanges a code for the session cookie: GET
	// ?code=...&next=/path.
	devSignInPath = "/auth/dev-signin"

	// devSignInCodeTTL is how long a code can be used after it is minted.
	devSignInCodeTTL = 2 * time.Minute
	// devSignInMaxPending bounds the codes waiting to be used.
	devSignInMaxPending = 32
	// devSignInCodeBytes is the code's random length.
	devSignInCodeBytes = 32
)

// devSignInCodes holds the pending sign-in codes by SHA-256 hash.
type devSignInCodes struct {
	mu      sync.Mutex
	pending map[[sha256.Size]byte]time.Time // hash -> expiry
	now     func() time.Time
}

func newDevSignInCodes() *devSignInCodes {
	return &devSignInCodes{pending: make(map[[sha256.Size]byte]time.Time), now: time.Now}
}

// issue returns a new code and its expiry. ok is false when too many codes
// are pending or no random bytes could be read.
func (c *devSignInCodes) issue() (code string, expiry time.Time, ok bool) {
	b := make([]byte, devSignInCodeBytes)
	if _, err := rand.Read(b); err != nil {
		return "", time.Time{}, false
	}
	code = base64.RawURLEncoding.EncodeToString(b)

	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for h, exp := range c.pending {
		if !now.Before(exp) {
			delete(c.pending, h)
		}
	}
	if len(c.pending) >= devSignInMaxPending {
		return "", time.Time{}, false
	}
	expiry = now.Add(devSignInCodeTTL)
	c.pending[sha256.Sum256([]byte(code))] = expiry
	return code, expiry, true
}

// consume reports whether code is pending and unexpired. A known code is
// removed whatever the answer, so it never works twice.
func (c *devSignInCodes) consume(code string) bool {
	if code == "" {
		return false
	}
	h := sha256.Sum256([]byte(code))
	c.mu.Lock()
	defer c.mu.Unlock()
	exp, found := c.pending[h]
	if !found {
		return false
	}
	delete(c.pending, h)
	return c.now().Before(exp)
}

// devLaunchResponse is the body devLaunchPath answers with.
type devLaunchResponse struct {
	// Code is the single-use sign-in code.
	Code string `json:"code"`
	// SignInPath is the path to open in the browser, code included.
	SignInPath string `json:"signInPath"`
	// ExpiresAt is when the code stops working.
	ExpiresAt time.Time `json:"expiresAt"`
}

// handleDevLaunch mints a sign-in code for the holder of the dev token.
// Route: POST /auth/dev-launch
func (ws *WebServer) handleDevLaunch(w http.ResponseWriter, r *http.Request) {
	if ws.config.DevAuthToken == "" || ws.devSignIn == nil {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	token := extractBearerToken(r)
	if token == "" || !apiclient.ValidateDevToken(token, ws.config.DevAuthToken) {
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "a valid development token is required", nil)
		return
	}
	code, expiry, ok := ws.devSignIn.issue()
	if !ok {
		w.Header().Set("Retry-After", "10")
		writeError(w, http.StatusTooManyRequests, ErrCodeRateLimited, "too many sign-in links are pending; try again shortly", nil)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(devLaunchResponse{
		Code:       code,
		SignInPath: devSignInPath + "?" + url.Values{"code": {code}}.Encode(),
		ExpiresAt:  expiry.UTC(),
	})
}

// devSignInRefusedText is the body of a refused sign-in link.
const devSignInRefusedText = "This sign-in link is not valid: it was already used or it has expired.\n" +
	"Run 'scion server open' in a terminal to open a new one.\n"

// handleDevSignIn exchanges a sign-in code for a fresh dev web session and
// redirects to next (a local path, "/" by default).
// Route: GET /auth/dev-signin?code=...&next=/path
func (ws *WebServer) handleDevSignIn(w http.ResponseWriter, r *http.Request) {
	if ws.config.DevAuthToken == "" || ws.devSignIn == nil {
		http.NotFound(w, r)
		return
	}
	// GET only: a HEAD (a link prefetcher or unfurler) must not use up the
	// code.
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	// The link carries the code: keep it out of the Referer of any
	// request the next page makes.
	w.Header().Set("Referrer-Policy", "no-referrer")

	q := r.URL.Query()
	if !ws.devSignIn.consume(q.Get("code")) {
		ws.logger().Info("Dev sign-in link refused")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(devSignInRefusedText))
		return
	}

	// A fresh session: nothing from a cookie the browser already had.
	session, _ := ws.sessionStore.New(r, webSessionName)
	session.Values = make(map[interface{}]interface{})
	session.IsNew = true
	ws.populateDevSession(r.Context(), session)
	// A test identity marker cookie, if any, goes with the old session (its
	// presence only; the classifier is the one reader of its contents).
	if c, err := r.Cookie(testIdentitySessionCookie); err == nil && c.Value != "" {
		ws.expireTestIdentityMarker(w)
	}
	if err := session.Save(r, w); err != nil {
		ws.logger().Error("Failed to save dev sign-in session", "error", err)
		ws.serveInternalError(w, r)
		return
	}
	ws.logger().Info("Dev sign-in link used")
	http.Redirect(w, r, localRedirectPath(q.Get("next")), http.StatusSeeOther)
}

// populateDevSession writes the dev user's identity into session, with a
// hub token pair when a token service is set, and sets its cookie options
// (applyDevSessionCookieOptions). It returns the user.
func (ws *WebServer) populateDevSession(ctx context.Context, session *sessions.Session) *webSessionUser {
	// Read from the store so any identity set via the onboarding wizard
	// is reflected.
	devEmail := "dev@localhost"
	devName := "Development User"
	if ws.store != nil {
		if dbUser, err := ws.store.GetUser(ctx, DevUserID); err == nil {
			if dbUser.Email != "" {
				devEmail = dbUser.Email
			}
			if dbUser.DisplayName != "" {
				devName = dbUser.DisplayName
			}
			// The session generation, as the OAuth sign-in records it, so
			// a revocation (generation bump) ends this session and a new
			// sign-in after one is accepted.
			session.Values[sessKeySessionGeneration] = dbUser.SessionGeneration
			if err := ws.store.UpdateUserLastLogin(ctx, dbUser.ID, time.Now()); err != nil {
				ws.logger().Warn("Failed to update dev-auth last login", "error", err)
			}
		}
	}
	devUser := &webSessionUser{
		UserID: DevUserID,
		Email:  devEmail,
		Name:   devName,
		Role:   "admin",
	}
	session.Values[sessKeyUserID] = devUser.UserID
	session.Values[sessKeyUserEmail] = devUser.Email
	session.Values[sessKeyUserName] = devUser.Name
	session.Values[sessKeyUserAvatar] = devUser.AvatarURL
	session.Values[sessKeyUserRole] = devUser.Role

	// Hub JWTs so the session-to-bearer middleware can authenticate API
	// requests, mirroring handleOAuthCallback.
	if ws.userTokenSvc != nil {
		accessToken, refreshToken, expiresIn, err := ws.userTokenSvc.GenerateTokenPair(
			devUser.UserID, devUser.Email, devUser.Name, devUser.Role, ClientTypeWeb,
		)
		if err == nil {
			session.Values[sessKeyHubAccessToken] = accessToken
			session.Values[sessKeyHubRefreshToken] = refreshToken
			session.Values[sessKeyHubTokenExpiry] = time.Now().Add(time.Duration(expiresIn) * time.Second).UnixMilli()
		} else {
			ws.logger().Warn("Failed to generate hub tokens for the dev session", "error", err)
		}
	}
	ws.applyDevSessionCookieOptions(session)
	return devUser
}

// applyDevSessionCookieOptions gives a dev user's session cookie
// SameSite=Strict (and HttpOnly, as every session cookie), so that every
// save of it keeps those attributes. Other sessions are left as they are.
func (ws *WebServer) applyDevSessionCookieOptions(session *sessions.Session) {
	if ws.config.DevAuthToken == "" || session == nil || session.Options == nil {
		return
	}
	if uid, _ := session.Values[sessKeyUserID].(string); uid != DevUserID {
		return
	}
	opts := *session.Options
	opts.HttpOnly = true
	opts.SameSite = http.SameSiteStrictMode
	session.Options = &opts
}

// keepsNoAnonymousSession reports whether an unauthenticated redirect must
// not save a session for r: on a dev sign-in server, a request that carries
// no session cookie. The dev session cookie is SameSite=Strict, so a
// cross-site navigation arrives without it; saving an anonymous session
// there would replace (sign out) the browser's dev session.
func (ws *WebServer) keepsNoAnonymousSession(r *http.Request) bool {
	if ws.devSignIn == nil {
		return false
	}
	_, err := r.Cookie(webSessionName)
	return err != nil
}

// noSessionLoginLocation is the login redirect for a browser request
// without a session cookie on a dev sign-in server (see
// keepsNoAnonymousSession). With web OAuth providers configured, it is the
// sign-in page with the requested path as returnTo, limited to a path on
// this server: the page passes returnTo on when a provider is picked, and
// nothing is saved in a cookie that would replace a dev session. Otherwise
// it is /auth/login.
func (ws *WebServer) noSessionLoginLocation(requested string) string {
	if !ws.webOAuthConfigured() {
		return "/auth/login"
	}
	next := localRedirectPath(requested)
	if next == "/" {
		return "/login"
	}
	return "/login?" + url.Values{"returnTo": {next}}.Encode()
}

// webOAuthConfigured reports whether at least one OAuth provider is
// configured for web sign-in.
func (ws *WebServer) webOAuthConfigured() bool {
	if ws.oauthService == nil || ws.config.AuthMode == "proxy" {
		return false
	}
	for _, pid := range hubclient.OAuthProviderOrder() {
		if ws.oauthService.IsProviderConfiguredForClient(OAuthClientTypeWeb, pid) {
			return true
		}
	}
	return false
}

// localRedirectPath returns next when it is a path on this server, and "/"
// otherwise (empty, absolute or scheme-relative URLs, backslashes, or the
// sign-in path itself).
func localRedirectPath(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.ContainsAny(next, "\\\r\n") {
		return "/"
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil {
		return "/"
	}
	if u.Path == devSignInPath || u.Path == devLaunchPath {
		return "/"
	}
	return next
}
