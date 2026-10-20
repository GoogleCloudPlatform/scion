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

package hubprofile

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hubprofile/hubprofiletest"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
)

// The own-hub check by hub identity (ptone/scion#4240, X5). The target and
// the agent's own hub are two local test servers on different ports, so
// the name check (CheckNotOwnHub) passes and only the hub ids decide.

// debugLog collects a session's debug output.
type debugLog struct {
	mu    sync.Mutex
	lines []string
}

func (d *debugLog) f(format string, args ...any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lines = append(d.lines, fmt.Sprintf(format, args...))
}

func (d *debugLog) joined() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return strings.Join(d.lines, "\n")
}

// openWithOwn opens a session on h's target with own as the agent's own
// hub (SCION_HUB_ENDPOINT), capturing debug output.
func (h *harness) openWithOwn(t *testing.T, own *hubprofiletest.HealthHub, alias, run string) (*Session, *debugLog) {
	t.Helper()
	return h.openWithOwnURL(t, own.Srv.URL, alias, run)
}

func (h *harness) openWithOwnURL(t *testing.T, ownURL, alias, run string) (*Session, *debugLog) {
	t.Helper()
	h.env["SCION_HUB_ENDPOINT"] = ownURL
	d := testDeps(h.gcp, h.clk, h.env)
	log := &debugLog{}
	d.Debugf = log.f
	s, err := Open(Options{Home: h.home, Profile: testProfile(h.target, TransportIAP), Alias: alias, Run: run}, d)
	require.NoError(t, err, "the names differ, so Open passes")
	return s, log
}

func listAsIssuer(s *Session) error {
	client, err := s.Client()
	if err != nil {
		return err
	}
	_, err = client.TestIdentities().List(context.Background(), nil)
	return err
}

// assertNoBearerReachedTarget checks that no request the target received
// carried a profile bearer or delegation.
func assertNoBearerReachedTarget(t *testing.T, target *hubprofiletest.Target) {
	t.Helper()
	for _, r := range target.Recorded() {
		assert.Empty(t, r.Header.Get("Authorization"), r.Path)
		assert.Empty(t, r.Header.Get(HeaderIssuerDelegation), r.Path)
	}
}

// D1: an alias name of the agent's own hub (another endpoint, the same hub
// id) is refused before any profile credential is sent.
func TestOwnHubByID_SameIDUnderAnotherNameRefused(t *testing.T) {
	h := newHarness(t, true)
	h.target.SetHubID("hub-own")
	own := hubprofiletest.NewHealthHub(t, "hub-own")
	s, _ := h.openWithOwn(t, own, "", "r1")

	err := listAsIssuer(s)
	require.ErrorIs(t, err, ErrOwnHub)
	assert.Contains(t, err.Error(), `hub profile "t1"`)
	assertNoBearerReachedTarget(t, h.target)
	for _, r := range h.target.Recorded() {
		assert.Equal(t, testInfraStatusPath, r.Path, "only the transport-only issuer audience read reached the target")
	}

	// The raw HTTP client is refused the same way, and the result is kept.
	req, _ := http.NewRequest(http.MethodGet, s.Endpoint()+"/api/v1/test-identities", nil)
	_, err = s.HTTPClient().Do(req)
	require.ErrorIs(t, err, ErrOwnHub)
	assert.Len(t, h.target.HealthProbes(), 1, "the id lookup runs once per session")
	assertNoBearerReachedTarget(t, h.target)
}

// D1: the same refusal for a session acting as a cached test identity.
func TestOwnHubByID_AliasSessionRefused(t *testing.T) {
	h := newHarness(t, true)
	s := h.mustOpen(testProfile(h.target, TransportIAP), "", "r1")
	_, err := IssueToCache(context.Background(), s, IssueRequest{Alias: "owner"})
	require.NoError(t, err, "no own hub endpoint: no id comparison")
	before := len(h.target.Recorded())

	h.target.SetHubID("hub-own")
	own := hubprofiletest.NewHealthHub(t, "hub-own")
	as, _ := h.openWithOwn(t, own, "owner", "r1")
	client, err := as.Client()
	require.NoError(t, err)
	_, err = client.Auth().Me(context.Background())
	require.ErrorIs(t, err, ErrOwnHub)
	assert.Len(t, h.target.Recorded(), before, "the fixture token was never sent")
}

// D1: different hub ids are allowed.
func TestOwnHubByID_DifferentIDsAllowed(t *testing.T) {
	h := newHarness(t, true)
	h.target.SetHubID("hub-t1")
	own := hubprofiletest.NewHealthHub(t, "hub-own")
	s, log := h.openWithOwn(t, own, "", "r1")

	require.NoError(t, listAsIssuer(s))
	require.NoError(t, listAsIssuer(s))
	assert.Len(t, h.target.HealthProbes(), 1)
	assert.Len(t, own.Recorded(), 1)
	assert.Contains(t, log.joined(), "own-hub check by hub id passed")
}

// D1: the combined web server reports the hub id under "hub".
func TestOwnHubByID_CombinedHealthShape(t *testing.T) {
	h := newHarness(t, true)
	h.target.SetHubID("hub-own")
	own := hubprofiletest.NewHealthHub(t, "")
	own.SetResponse(http.StatusOK, map[string]any{"status": "healthy", "web": map[string]any{"status": "ok"}, "hub": map[string]any{"status": "healthy", "hub_id": "hub-own"}})
	s, _ := h.openWithOwn(t, own, "", "r1")
	require.ErrorIs(t, listAsIssuer(s), ErrOwnHub)
}

// D1/D2: when either id is unavailable the name check (already passed) is
// the only check; this is said in debug output only.
func TestOwnHubByID_UnavailableFallsBackToNames(t *testing.T) {
	targetFails := func(h http.HandlerFunc) func(*hubprofiletest.Target, *hubprofiletest.HealthHub) {
		return func(tg *hubprofiletest.Target, _ *hubprofiletest.HealthHub) { tg.SetHealthHandler(h) }
	}
	cases := []struct {
		name     string
		targetID string
		own      func(*hubprofiletest.HealthHub)
		want     string
		target   func(*hubprofiletest.Target, *hubprofiletest.HealthHub)
	}{
		{"target reports no id", "", nil, "the target hub's id is unavailable", nil},
		{"target 503", "hub-own", nil, "returned 503", targetFails(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"hub_id":"hub-own"}`))
		})},
		{"target not JSON", "hub-own", nil, "not JSON", targetFails(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("<html>hub-own</html>"))
		})},
		// Following the redirect would read the own hub's id and refuse.
		{"target redirect", "hub-own", nil, "returned 302", func(tg *hubprofiletest.Target, own *hubprofiletest.HealthHub) {
			tg.SetHealthHandler(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, own.Srv.URL+"/healthz", http.StatusFound)
			})
		}},
		{"own reports no id", "hub-own", func(o *hubprofiletest.HealthHub) {
			o.SetResponse(http.StatusOK, map[string]any{"status": "healthy"})
		}, "this agent's own hub id is unavailable", nil},
		{"own fails", "hub-own", func(o *hubprofiletest.HealthHub) {
			o.SetResponse(http.StatusServiceUnavailable, map[string]any{"hub_id": "hub-own"})
		}, "this agent's own hub id is unavailable", nil},
		{"own id malformed", "hub-own", func(o *hubprofiletest.HealthHub) {
			o.SetResponse(http.StatusOK, map[string]any{"hub_id": "hub own"})
		}, "this agent's own hub id is unavailable", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, true)
			h.target.SetHubID(tc.targetID)
			own := hubprofiletest.NewHealthHub(t, "hub-own")
			if tc.own != nil {
				tc.own(own)
			}
			if tc.target != nil {
				tc.target(h.target, own)
			}
			s, log := h.openWithOwn(t, own, "", "r1")
			require.NoError(t, listAsIssuer(s))
			assert.Contains(t, log.joined(), tc.want)
			assert.Contains(t, log.joined(), "name check only")
			if tc.target != nil {
				assert.Empty(t, own.Recorded(), "the own hub is not probed when the target id is unavailable")
			}
			outcome, err := s.OwnHubCheck(context.Background())
			require.NoError(t, err)
			assert.True(t, strings.HasPrefix(outcome, "by name only: "), outcome)
		})
	}

	t.Run("no own hub endpoint: no lookup at all", func(t *testing.T) {
		h := newHarness(t, true)
		h.target.SetHubID("hub-t1")
		d := testDeps(h.gcp, h.clk, h.env)
		log := &debugLog{}
		d.Debugf = log.f
		s, err := Open(Options{Home: h.home, Profile: testProfile(h.target, TransportIAP), Run: "r1"}, d)
		require.NoError(t, err)
		require.NoError(t, listAsIssuer(s))
		assert.Empty(t, h.target.HealthProbes())
		assert.Contains(t, log.joined(), "name check only")
	})
}

// D1/D2: a name match is refused at Open whatever the hub ids say, and no
// lookup is made: a failing or differing lookup never allows it.
func TestOwnHubByID_NameMatchStillRefused(t *testing.T) {
	h := newHarness(t, true)
	h.target.SetHubID("hub-t1")
	h.env["SCION_HUB_ENDPOINT"] = h.target.Srv.URL
	_, err := h.open(testProfile(h.target, TransportIAP), "", "r1")
	require.ErrorIs(t, err, ErrOwnHub)
	assert.Empty(t, h.target.HealthProbes())
	assert.Empty(t, h.target.Recorded())
}

// D2: the target lookup carries only the profile's transport token, and
// the own-hub lookup carries no credential, although ambient tokens are in
// the session's environment (SCION_AUTH_TOKEN and SCION_HUB_TOKEN from
// the fakes' environment, plus SCION_DEV_TOKEN here).
func TestOwnHubByID_LookupsSendNoExtraCredential(t *testing.T) {
	h := newHarness(t, true)
	h.env["SCION_DEV_TOKEN"] = "AMBIENT-SCION-DEV-TOKEN"
	env := testDeps(h.gcp, h.clk, h.env).Getenv
	for _, k := range []string{"SCION_AUTH_TOKEN", "SCION_HUB_TOKEN", "SCION_DEV_TOKEN"} {
		require.Contains(t, env(k), "AMBIENT", k)
	}
	h.target.SetHubID("hub-t1")
	own := hubprofiletest.NewHealthHub(t, "hub-own")
	s, _ := h.openWithOwn(t, own, "", "r1")
	require.NoError(t, listAsIssuer(s))

	probes := h.target.HealthProbes()
	require.Len(t, probes, 1)
	assert.Equal(t, http.MethodGet, probes[0].Method)
	assert.Equal(t, []string{"Proxy-Authorization"}, credentialHeaderNames(probes[0].Header), "the transport token only")

	ownReqs := own.Recorded()
	require.Len(t, ownReqs, 1)
	assert.Equal(t, "/healthz", ownReqs[0].Path)
	assert.Equal(t, http.MethodGet, ownReqs[0].Method)
	assert.Empty(t, credentialHeaderNames(ownReqs[0].Header), "no credential to the agent's own hub")
	for _, r := range append(probes, ownReqs...) {
		for k, vs := range r.Header {
			for _, v := range vs {
				assert.NotContains(t, v, "AMBIENT", "header %s", k)
			}
		}
	}
}

// D2: a redirect from the own hub is not followed (no request to the
// redirect target) and counts as an unavailable id.
func TestOwnHubByID_RedirectNotFollowed(t *testing.T) {
	h := newHarness(t, true)
	h.target.SetHubID("hub-t1")
	elsewhere := hubprofiletest.NewHealthHub(t, "hub-t1")
	own := httptest.NewServer(http.RedirectHandler(elsewhere.Srv.URL+"/healthz", http.StatusFound))
	t.Cleanup(own.Close)
	s, log := h.openWithOwnURL(t, own.URL, "", "r1")
	require.NoError(t, listAsIssuer(s))
	assert.Empty(t, elsewhere.Recorded())
	assert.Contains(t, log.joined(), "returned 302")
}

// The status probe reports the refusal as a failed check and sends no
// profile credential.
func TestOwnHubByID_StatusReportsRefusal(t *testing.T) {
	h := newHarness(t, true)
	h.target.SetHubID("hub-own")
	own := hubprofiletest.NewHealthHub(t, "hub-own")
	s, _ := h.openWithOwn(t, own, "", "r1")
	r := s.Status(context.Background())
	assert.False(t, r.OK)
	require.NotEmpty(t, r.Checks)
	assert.Equal(t, "not this agent's own hub", r.Checks[0].Name)
	assert.False(t, r.Checks[0].OK)
	assert.Empty(t, h.target.Recorded())
}

func TestHealthHubID(t *testing.T) {
	cases := []struct {
		body, want string
		err        bool
	}{
		{`{"status":"healthy","hub_id":"abc123"}`, "abc123", false},
		{`{"status":"healthy","hub":{"hub_id":"abc123"}}`, "abc123", false},
		{`{"hub_id":"top","hub":{"hub_id":"nested"}}`, "top", false},
		{`{"status":"healthy"}`, "", true},
		{`not json`, "", true},
		{`{"hub_id":"a\u0000b"}`, "", true},
		{`{"hub_id":"` + strings.Repeat("a", maxHubIDLen+1) + `"}`, "", true},
	}
	for _, tc := range cases {
		got, err := healthHubID(strings.NewReader(tc.body))
		if tc.err {
			assert.Error(t, err, tc.body)
			continue
		}
		require.NoError(t, err, tc.body)
		assert.Equal(t, tc.want, got)
	}
}

// hub status reports the outcome of the id check when it passes or falls
// back to the name check, not only when it refuses.
func TestOwnHubByID_StatusReportsEveryOutcome(t *testing.T) {
	cases := []struct{ name, targetID, ownID, want string }{
		{"passed", "hub-t1", "hub-own", OwnHubByIDPassed},
		{"target id unavailable", "", "hub-own", OwnHubByIDNoTargetID},
		{"own id unavailable", "hub-t1", "", OwnHubByIDNoOwnID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, true)
			h.target.SetHubID(tc.targetID)
			own := hubprofiletest.NewHealthHub(t, tc.ownID)
			s, _ := h.openWithOwn(t, own, "", "r1")
			r := s.Status(context.Background())
			assert.True(t, r.OK, "%+v", r.Checks)
			require.NotEmpty(t, r.Checks)
			assert.Equal(t, CheckNotOwnHubName, r.Checks[0].Name)
			assert.True(t, r.Checks[0].OK)
			assert.Equal(t, tc.want, r.Checks[0].Detail)
		})
	}
	t.Run("no own hub endpoint", func(t *testing.T) {
		h := newHarness(t, true)
		r := h.mustOpen(testProfile(h.target, TransportIAP), "", "r1").Status(context.Background())
		require.NotEmpty(t, r.Checks)
		assert.Equal(t, OwnHubByIDNoOwnEndpoint, r.Checks[0].Detail)
	})
}

// A refused session reads no issuer audience and mints no issuer token:
// the id check runs before the bearer.
func TestOwnHubByID_RefusedBeforeAnyBearerMint(t *testing.T) {
	h := newHarness(t, true)
	h.target.SetHubID("hub-own")
	own := hubprofiletest.NewHealthHub(t, "hub-own")
	s, _ := h.openWithOwn(t, own, "", "r1")
	require.ErrorIs(t, listAsIssuer(s), ErrOwnHub)
	assert.Empty(t, h.target.Recorded(), "no issuer audience read and no request")
	for _, tok := range h.gcp.MintedTokens() {
		c, err := transportauth.DecodeIDTokenClaims(tok)
		if err != nil {
			continue
		}
		assert.NotEqual(t, testIssuerSA, c.Email, "no issuer ID token was minted")
	}
}

// Concurrent requests on a same-id session are all refused, with one
// lookup on each side and no bearer on the wire.
func TestOwnHubByID_ConcurrentRequestsAllRefused(t *testing.T) {
	h := newHarness(t, true)
	h.target.SetHubID("hub-own")
	own := hubprofiletest.NewHealthHub(t, "hub-own")
	s, _ := h.openWithOwn(t, own, "", "r1")
	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = listAsIssuer(s)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		assert.ErrorIs(t, err, ErrOwnHub, "request %d", i)
	}
	assert.Len(t, h.target.HealthProbes(), 1)
	assert.Len(t, own.Recorded(), 1)
	assertNoBearerReachedTarget(t, h.target)
	assert.Empty(t, h.target.Recorded())
}

// A slow own hub falls back to the name check within the lookup bound.
func TestOwnHubByID_SlowOwnHubFallsBack(t *testing.T) {
	prev := hubIDLookupTimeout
	hubIDLookupTimeout = 200 * time.Millisecond
	t.Cleanup(func() { hubIDLookupTimeout = prev })
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(slow.Close)
	t.Cleanup(func() { close(release) })
	h := newHarness(t, true)
	h.target.SetHubID("hub-own")
	s, log := h.openWithOwnURL(t, slow.URL, "", "r1")
	start := time.Now()
	require.NoError(t, listAsIssuer(s))
	assert.Less(t, time.Since(start), 5*time.Second)
	assert.Contains(t, log.joined(), "this agent's own hub id is unavailable")
}
