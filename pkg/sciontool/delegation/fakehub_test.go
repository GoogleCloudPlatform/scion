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

package delegation

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
)

const (
	testAgentID  = "agent-1"
	testHubID    = "hub-1"
	testAudience = "scion-hub:" + testHubID
)

// fakeHub models the hub's side of exchange, refresh and use for one
// agent: agent credentials (JWTs) that a refresh rotates and revokes,
// delegated credentials bound to the agent credential they were exchanged
// with, the per-grant cap of 2 active credentials (the oldest is
// superseded), and a use endpoint that admits only an active delegated
// credential.
type fakeHub struct {
	t   *testing.T
	clk clock.Clock
	srv *httptest.Server

	mu sync.Mutex
	// jwts maps each agent credential to whether it is still live.
	jwts    map[string]bool
	jwtSeq  int
	current string
	// refuse401 answers exchanges made with these agent credentials with
	// 401 and the given code.
	refuse401 map[string]string
	// nextStatus answers the next exchanges with these statuses and codes
	// instead of issuing a credential.
	nextStatus []scriptedAnswer
	ttl        time.Duration
	// grantExpires, when set, is the grant's expiry: exchange caps the
	// credential lifetime at it and refuses once it has passed (§8.2 steps
	// 8 and 14).
	grantExpires time.Time
	hubID        string
	healthzRaw   string // overrides the /healthz body when set

	creds     []*fakeCred
	exchanges []exchangeRecord
	uses      []useRecord
}

type scriptedAnswer struct {
	status int
	code   string
}

type fakeCred struct {
	token     string
	grantID   string
	jwt       string
	expiresAt time.Time
	revoked   string // revoke reason, "" while active
}

type exchangeRecord struct {
	grantID  string
	jwt      string
	audience string
	token    string // issued token, "" when refused
}

type useRecord struct {
	path          string
	method        string
	authorization string
	agentToken    string
	cookie        string
	body          string
	status        int
}

func newFakeHub(t *testing.T, clk clock.Clock) *fakeHub {
	t.Helper()
	f := &fakeHub{
		t:         t,
		clk:       clk,
		jwts:      map[string]bool{},
		refuse401: map[string]string{},
		ttl:       15 * time.Minute,
		hubID:     testHubID,
	}
	f.current = f.mintJWTLocked()
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeHub) mintJWTLocked() string {
	f.jwtSeq++
	jwt := fmt.Sprintf("agent-jwt-%d", f.jwtSeq)
	f.jwts[jwt] = true
	return jwt
}

func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return "scion_adt_" + base64.RawURLEncoding.EncodeToString(b)
}

func writeHubError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"error":{"code":%q,"message":"fake hub refused"}}`, code)
}

func (f *fakeHub) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/healthz":
		f.mu.Lock()
		raw, id := f.healthzRaw, f.hubID
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if raw != "" {
			_, _ = io.WriteString(w, raw)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "healthy", "hub_id": id})
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/agents/"+testAgentID+"/token/refresh":
		f.serveRefresh(w, r)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/v1/agents/"+testAgentID+"/delegations/") &&
		strings.HasSuffix(r.URL.Path, "/exchange"):
		grantID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/agents/"+testAgentID+"/delegations/"), "/exchange")
		f.serveExchange(w, r, grantID)
	default:
		f.serveUse(w, r)
	}
}

// serveRefresh rotates the agent credential: the presented one is
// revoked, and so is every delegated credential exchanged with it (§9.3).
func (f *fakeHub) serveRefresh(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	old := r.Header.Get("X-Scion-Agent-Token")
	if !f.jwts[old] {
		writeHubError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	f.jwts[old] = false
	f.current = f.mintJWTLocked()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"token":      f.current,
		"expires_at": time.Now().Add(10 * time.Hour).UTC().Format(time.RFC3339),
	})
}

func (f *fakeHub) serveExchange(w http.ResponseWriter, r *http.Request, grantID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	jwt := r.Header.Get("X-Scion-Agent-Token")
	var body struct {
		Audience string `json:"audience"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	rec := exchangeRecord{grantID: grantID, jwt: jwt, audience: body.Audience}
	defer func() { f.exchanges = append(f.exchanges, rec) }()

	if code, ok := f.refuse401[jwt]; ok {
		writeHubError(w, http.StatusUnauthorized, code)
		return
	}
	if !f.jwts[jwt] {
		// The middleware refuses a revoked agent credential.
		writeHubError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if len(f.nextStatus) > 0 {
		a := f.nextStatus[0]
		f.nextStatus = f.nextStatus[1:]
		writeHubError(w, a.status, a.code)
		return
	}
	want := "scion-hub"
	if f.hubID != "" {
		want = "scion-hub:" + f.hubID
	}
	if body.Audience != want {
		writeHubError(w, http.StatusBadRequest, "invalid_audience")
		return
	}

	now := f.clk.Now()
	expires := now.Add(f.ttl)
	if !f.grantExpires.IsZero() {
		if !now.Before(f.grantExpires) {
			writeHubError(w, http.StatusForbidden, "grant_inactive")
			return
		}
		if f.grantExpires.Before(expires) {
			expires = f.grantExpires
		}
	}
	cred := &fakeCred{token: randomToken(), grantID: grantID, jwt: jwt, expiresAt: expires}
	f.creds = append(f.creds, cred)
	f.supersedeLocked(grantID, now)
	rec.token = cred.token
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"token":        cred.token,
		"expiresAt":    cred.expiresAt,
		"grantId":      grantID,
		"credentialId": fmt.Sprintf("cred-%d", len(f.creds)),
		"audience":     body.Audience,
		"permissions":  []string{"agent.read"},
	})
}

// supersedeLocked keeps at most 2 active credentials for the grant,
// revoking the oldest beyond that (§8.2 step 13).
func (f *fakeHub) supersedeLocked(grantID string, now time.Time) {
	var active []*fakeCred
	for _, c := range f.creds {
		if c.grantID == grantID && c.revoked == "" && now.Before(c.expiresAt) {
			active = append(active, c)
		}
	}
	for len(active) > 2 {
		active[0].revoked = "superseded"
		active = active[1:]
	}
}

// activeLocked reports whether token is an active delegated credential.
func (f *fakeHub) activeLocked(token string) bool {
	for _, c := range f.creds {
		if c.token == token {
			return c.revoked == "" && f.jwts[c.jwt] && f.clk.Now().Before(c.expiresAt)
		}
	}
	return false
}

func (f *fakeHub) serveUse(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	rec := useRecord{
		path:          r.URL.RequestURI(),
		method:        r.Method,
		authorization: r.Header.Get("Authorization"),
		agentToken:    r.Header.Get("X-Scion-Agent-Token"),
		cookie:        r.Header.Get("Cookie"),
		body:          string(body),
	}
	tok := strings.TrimPrefix(rec.authorization, "Bearer ")
	if rec.agentToken != "" || !f.activeLocked(tok) {
		rec.status = http.StatusUnauthorized
		f.uses = append(f.uses, rec)
		writeHubError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	rec.status = http.StatusOK
	f.uses = append(f.uses, rec)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Set-Cookie", "session=should-not-pass")
	w.Header().Set("X-Fake-Hub", "yes")
	_ = json.NewEncoder(w).Encode(map[string]string{"path": r.URL.Path, "method": r.Method})
}

// externalExchange exchanges grantID as another holder of the agent
// credential would, outside the helper.
func (f *fakeHub) externalExchange(grantID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.clk.Now()
	cred := &fakeCred{token: randomToken(), grantID: grantID, jwt: f.current, expiresAt: now.Add(f.ttl)}
	f.creds = append(f.creds, cred)
	f.supersedeLocked(grantID, now)
	return cred.token
}

func (f *fakeHub) exchangeLog() []exchangeRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]exchangeRecord(nil), f.exchanges...)
}

func (f *fakeHub) useLog() []useRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]useRecord(nil), f.uses...)
}

func (f *fakeHub) currentJWT() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.current
}

// issuedTokens returns every delegated credential the fake hub issued.
func (f *fakeHub) issuedTokens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.creds))
	for _, c := range f.creds {
		out = append(out, c.token)
	}
	return out
}

func (f *fakeHub) credState(token string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.creds {
		if c.token == token {
			if c.revoked != "" {
				return c.revoked
			}
			return "active"
		}
	}
	return "unknown"
}

func (f *fakeHub) set(fn func(f *fakeHub)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeHub) credExpiry(token string) time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.creds {
		if c.token == token {
			return c.expiresAt
		}
	}
	return time.Time{}
}
