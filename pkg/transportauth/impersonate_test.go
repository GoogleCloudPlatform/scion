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

package transportauth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for assign-mode ID tokens through IAM generateIdToken
// (ptone/scion#4240, X3). The fakes stand in for the scion metadata
// emulator (the assigned service account's access token) and the IAM
// Credentials API.

func fakeJWT(claims map[string]any) string {
	h := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	b, _ := json.Marshal(claims)
	return h + "." + base64.RawURLEncoding.EncodeToString(b) + ".c2lnbmF0dXJl"
}

type fakeEmulator struct {
	srv      *httptest.Server
	calls    atomic.Int32
	status   int
	body     string
	tokenSeq atomic.Int32
}

func newFakeEmulator(t *testing.T, expiresIn int) *fakeEmulator {
	t.Helper()
	e := &fakeEmulator{status: http.StatusOK}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.calls.Add(1)
		if r.URL.Path != "/computeMetadata/v1/instance/service-accounts/default/token" || r.Header.Get("Metadata-Flavor") != "Google" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if e.status != http.StatusOK {
			http.Error(w, e.body, e.status)
			return
		}
		if e.body != "" {
			_, _ = w.Write([]byte(e.body))
			return
		}
		n := e.tokenSeq.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": fmt.Sprintf("ACCESS-TOKEN-%d", n), "expires_in": expiresIn, "token_type": "Bearer"})
	}))
	t.Cleanup(e.srv.Close)
	return e
}

type iamCall struct {
	sa, auth, audience string
	includeEmail       bool
}

type fakeIAM struct {
	srv    *httptest.Server
	mu     sync.Mutex
	calls  []iamCall
	status int
	// mint returns the token for a call; default: aud and email as asked.
	mint func(sa, aud string, n int) string
	now  func() time.Time
}

func newFakeIAM(t *testing.T, now func() time.Time) *fakeIAM {
	t.Helper()
	f := &fakeIAM{status: http.StatusOK, now: now}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "/v1/projects/-/serviceAccounts/"
		if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, prefix) || !strings.HasSuffix(r.URL.Path, ":generateIdToken") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		sa := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, prefix), ":generateIdToken")
		var body struct {
			Audience     string `json:"audience"`
			IncludeEmail bool   `json:"includeEmail"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.calls = append(f.calls, iamCall{sa: sa, auth: r.Header.Get("Authorization"), audience: body.Audience, includeEmail: body.IncludeEmail})
		n := len(f.calls)
		f.mu.Unlock()
		if f.status != http.StatusOK {
			w.WriteHeader(f.status)
			_, _ = w.Write([]byte(`{"error":{"code":403,"status":"PERMISSION_DENIED","message":"Permission 'iam.serviceAccounts.getOpenIdToken' denied"}}`))
			return
		}
		var tok string
		if f.mint != nil {
			tok = f.mint(sa, body.Audience, n)
		} else {
			tok = fakeJWT(map[string]any{"aud": body.Audience, "email": sa, "sub": "1234", "iss": "https://accounts.google.com", "exp": f.now().Add(time.Hour).Unix(), "iat": f.now().Unix(), "n": n})
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"token": tok})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIAM) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestAssignModeMetadataBaseURL(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	cases := []struct {
		name    string
		env     map[string]string
		want    string
		wantErr string
	}{
		{"assign localhost", map[string]string{EnvMetadataMode: "assign", EnvGCEMetadataHost: "localhost:18380"}, "http://localhost:18380", ""},
		{"assign loopback ip", map[string]string{EnvMetadataMode: "assign", EnvGCEMetadataHost: "127.0.0.1:18380"}, "http://127.0.0.1:18380", ""},
		{"unset mode", map[string]string{EnvGCEMetadataHost: "localhost:18380"}, "", "not \"assign\""},
		{"passthrough", map[string]string{EnvMetadataMode: "passthrough", EnvGCEMetadataHost: "localhost:18380"}, "", "not \"assign\""},
		{"block", map[string]string{EnvMetadataMode: "block", EnvGCEMetadataHost: "localhost:18380"}, "", "not \"assign\""},
		{"no host", map[string]string{EnvMetadataMode: "assign"}, "", "GCE_METADATA_HOST is not set"},
		{"remote host", map[string]string{EnvMetadataMode: "assign", EnvGCEMetadataHost: "metadata.example.com:80"}, "", "loopback"},
		{"link-local", map[string]string{EnvMetadataMode: "assign", EnvGCEMetadataHost: "169.254.169.254"}, "", "loopback"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := AssignModeMetadataBaseURL(env(tc.env))
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestMetadataAccessTokenSource_CachesAndRefreshesBeforeExpiry(t *testing.T) {
	emu := newFakeEmulator(t, 3600)
	clk := &testClock{t: time.Unix(1_800_000_000, 0)}
	src := NewMetadataAccessTokenSource(emu.srv.URL)
	src.now = clk.now

	tok, err := src.AccessToken()
	require.NoError(t, err)
	assert.Equal(t, "ACCESS-TOKEN-1", tok)
	tok, err = src.AccessToken()
	require.NoError(t, err)
	assert.Equal(t, "ACCESS-TOKEN-1", tok, "cached")
	assert.EqualValues(t, 1, emu.calls.Load())

	// Just outside the refresh margin: still cached.
	clk.advance(time.Hour - RefreshMargin - time.Second)
	tok, _ = src.AccessToken()
	assert.Equal(t, "ACCESS-TOKEN-1", tok)
	// Inside the margin: refreshed before expiry.
	clk.advance(2 * time.Second)
	tok, err = src.AccessToken()
	require.NoError(t, err)
	assert.Equal(t, "ACCESS-TOKEN-2", tok)
	assert.EqualValues(t, 2, emu.calls.Load())
}

func TestMetadataAccessTokenSource_FailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"emulator error", http.StatusBadGateway, "token generation failed", "returned 502"},
		{"malformed", http.StatusOK, "not json", "malformed"},
		{"no token", http.StatusOK, `{"expires_in":3600}`, "malformed"},
		{"no expiry", http.StatusOK, `{"access_token":"ACCESS-NOEXP"}`, "no expiry"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			emu := newFakeEmulator(t, 3600)
			emu.status, emu.body = tc.status, tc.body
			_, err := NewMetadataAccessTokenSource(emu.srv.URL).AccessToken()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			assert.NotContains(t, err.Error(), "ACCESS-NOEXP")
		})
	}
	t.Run("unreachable", func(t *testing.T) {
		emu := newFakeEmulator(t, 3600)
		emu.srv.Close()
		_, err := NewMetadataAccessTokenSource(emu.srv.URL).AccessToken()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unreachable")
	})
}

func newIAMSource(t *testing.T, sa, aud string, emu *fakeEmulator, iam *fakeIAM, clk *testClock) *IAMIDTokenSource {
	t.Helper()
	acc := NewMetadataAccessTokenSource(emu.srv.URL)
	acc.now = clk.now
	src, err := NewIAMIDTokenSource(sa, aud, acc, WithIAMCredentialsURL(iam.srv.URL), WithClock(clk.now))
	require.NoError(t, err)
	return src
}

func TestIAMIDTokenSource_MintsAsTargetWithAssignedAccessToken(t *testing.T) {
	clk := &testClock{t: time.Unix(1_800_000_000, 0)}
	emu := newFakeEmulator(t, 3600)
	iam := newFakeIAM(t, clk.now)
	const sa = "t1-issuer@proj.iam.gserviceaccount.com"
	src := newIAMSource(t, sa, "scion-test-issuer:hub-1", emu, iam, clk)

	tok, err := src.Token()
	require.NoError(t, err)
	c, err := DecodeIDTokenClaims(tok)
	require.NoError(t, err)
	assert.Equal(t, sa, c.Email)
	assert.True(t, c.HasAudience("scion-test-issuer:hub-1"))

	require.Equal(t, 1, iam.callCount())
	call := iam.calls[0]
	assert.Equal(t, sa, call.sa, "generateIdToken runs AS the target service account")
	assert.Equal(t, "Bearer ACCESS-TOKEN-1", call.auth, "authenticated with the assigned account's access token")
	assert.Equal(t, "scion-test-issuer:hub-1", call.audience)
	assert.True(t, call.includeEmail)
	assert.Equal(t, sa, src.TargetServiceAccount())
	assert.Equal(t, time.Unix(1_800_000_000, 0).Add(time.Hour), src.Expiry())
}

func TestIAMIDTokenSource_RefreshesBeforeExpiry(t *testing.T) {
	clk := &testClock{t: time.Unix(1_800_000_000, 0)}
	emu := newFakeEmulator(t, 7200)
	iam := newFakeIAM(t, clk.now)
	src := newIAMSource(t, "t1-uat@proj.iam.gserviceaccount.com", "iap-client", emu, iam, clk)

	first, err := src.Token()
	require.NoError(t, err)
	again, _ := src.Token()
	assert.Equal(t, first, again, "cached while outside the refresh margin")
	assert.Equal(t, 1, iam.callCount())

	clk.advance(time.Hour - RefreshMargin - time.Second)
	again, _ = src.Token()
	assert.Equal(t, first, again)
	assert.Equal(t, 1, iam.callCount())

	clk.advance(2 * time.Second) // now within RefreshMargin of the ID token's exp
	second, err := src.Token()
	require.NoError(t, err)
	assert.NotEqual(t, first, second, "re-minted before expiry")
	assert.Equal(t, 2, iam.callCount())
}

func TestIAMIDTokenSource_FailsClosed(t *testing.T) {
	clk := &testClock{t: time.Unix(1_800_000_000, 0)}
	const sa = "t1-issuer@proj.iam.gserviceaccount.com"

	t.Run("emulator error: IAM never called", func(t *testing.T) {
		emu := newFakeEmulator(t, 3600)
		emu.status = http.StatusForbidden
		iam := newFakeIAM(t, clk.now)
		_, err := newIAMSource(t, sa, "aud", emu, iam, clk).Token()
		require.Error(t, err)
		assert.Equal(t, 0, iam.callCount())
	})
	t.Run("not assign mode", func(t *testing.T) {
		iam := newFakeIAM(t, clk.now)
		acc := providerFunc(func() (string, error) { return "", ErrNotAssignMode })
		src, err := NewIAMIDTokenSource(sa, "aud", acc, WithIAMCredentialsURL(iam.srv.URL))
		require.NoError(t, err)
		_, err = src.Token()
		require.ErrorIs(t, err, ErrNotAssignMode)
		assert.Equal(t, 0, iam.callCount())
	})
	t.Run("IAM permission denied", func(t *testing.T) {
		emu := newFakeEmulator(t, 3600)
		iam := newFakeIAM(t, clk.now)
		iam.status = http.StatusForbidden
		_, err := newIAMSource(t, sa, "aud", emu, iam, clk).Token()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "returned 403")
		assert.Contains(t, err.Error(), "PERMISSION_DENIED")
		assert.NotContains(t, err.Error(), "ACCESS-TOKEN")
	})
	t.Run("IAM unreachable", func(t *testing.T) {
		emu := newFakeEmulator(t, 3600)
		iam := newFakeIAM(t, clk.now)
		iam.srv.Close()
		_, err := newIAMSource(t, sa, "aud", emu, iam, clk).Token()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unreachable")
	})
	for _, tc := range []struct {
		name string
		mint func(sa, aud string, n int) string
		want string
	}{
		{"wrong audience", func(sa, _ string, _ int) string {
			return fakeJWT(map[string]any{"aud": "other", "email": sa, "exp": clk.now().Add(time.Hour).Unix()})
		}, "audience does not match"},
		{"wrong email", func(_, aud string, _ int) string {
			return fakeJWT(map[string]any{"aud": aud, "email": "someone@proj.iam.gserviceaccount.com", "exp": clk.now().Add(time.Hour).Unix()})
		}, "email does not match"},
		{"expired", func(sa, aud string, _ int) string {
			return fakeJWT(map[string]any{"aud": aud, "email": sa, "exp": clk.now().Add(-time.Minute).Unix()})
		}, "no future expiry"},
		{"no expiry", func(sa, aud string, _ int) string {
			return fakeJWT(map[string]any{"aud": aud, "email": sa})
		}, "no future expiry"},
		{"not a JWT", func(_, _ string, _ int) string { return "SECRET-OPAQUE" }, "not a JWT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			emu := newFakeEmulator(t, 3600)
			iam := newFakeIAM(t, clk.now)
			iam.mint = tc.mint
			src := newIAMSource(t, sa, "aud", emu, iam, clk)
			_, err := src.Token()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			assert.NotContains(t, err.Error(), "SECRET-OPAQUE")
			assert.True(t, src.Expiry().IsZero(), "a refused token is not cached")
		})
	}
	t.Run("refresh failure is not hidden by a stale token", func(t *testing.T) {
		emu := newFakeEmulator(t, 7200)
		iam := newFakeIAM(t, clk.now)
		src := newIAMSource(t, sa, "aud", emu, iam, clk)
		_, err := src.Token()
		require.NoError(t, err)
		clk.advance(time.Hour - time.Minute) // inside the refresh margin
		iam.status = http.StatusForbidden
		_, err = src.Token()
		require.Error(t, err, "a failed refresh fails closed")
		clk.advance(-(time.Hour - time.Minute))
	})
}

func TestNewIAMIDTokenSource_Validates(t *testing.T) {
	acc := providerFunc(func() (string, error) { return "x", nil })
	for _, sa := range []string{"", "no-at-sign", "a@b/c", "a@b?x", "a@b c"} {
		_, err := NewIAMIDTokenSource(sa, "aud", acc)
		assert.Error(t, err, "service account %q", sa)
	}
	_, err := NewIAMIDTokenSource("a@p.iam.gserviceaccount.com", "", acc)
	assert.Error(t, err)
	_, err = NewIAMIDTokenSource("a@p.iam.gserviceaccount.com", "aud", nil)
	assert.Error(t, err)
}

func TestFromEnv_AssignModeStillUsesNoMetadataSource(t *testing.T) {
	// The new assign-mode source is explicit only: FromEnv keeps refusing
	// the redirected metadata server, so no existing caller changes.
	orig := IsOnGCEFunc
	IsOnGCEFunc = func() bool { return true }
	defer func() { IsOnGCEFunc = orig }()
	t.Setenv(EnvMetadataMode, "assign")
	t.Setenv(EnvTransportAudience, "aud")
	t.Setenv(EnvTransportToken, "")
	t.Setenv("SCION_TRANSPORT_TOKEN_FILE", "")
	t.Setenv(EnvTransportMode, "")
	src, err := FromEnv()
	require.NoError(t, err)
	assert.Nil(t, src)
}

type providerFunc func() (string, error)

func (f providerFunc) AccessToken() (string, error) { return f() }
