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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/GoogleCloudPlatform/scion/pkg/testutil"
)

// printEnvVar makes the test binary print its environment and exit, so a
// test can inspect the environment a child process of the test inherits.
const printEnvVar = "DELEGATION_TEST_PRINT_ENV"

func TestMain(m *testing.M) {
	if os.Getenv(printEnvVar) == "1" {
		fmt.Print(strings.Join(os.Environ(), "\n"))
		os.Exit(0)
	}
	os.Exit(m.Run())
}

var testStart = time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)

// harness runs a Helper against a fakeHub, with a fake clock driving the
// helper's schedule and the hub's credential lifetimes.
type harness struct {
	hub    *fakeHub
	clk    *clock.Fake
	client *hub.Client
	helper *Helper
	sock   string
	home   string
	http   *http.Client
}

// maxSocketPath is the longest unix socket path a client can connect to on
// Linux (sun_path is 108 bytes, including the terminating NUL).
const maxSocketPath = 107

// shortTempDir returns a new temporary directory whose path stays short:
// t.TempDir embeds the test name, and a socket path under it can exceed
// maxSocketPath when TMPDIR is itself long.
func shortTempDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "dg")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

// requireDialablePath fails the test at once when path is too long for a
// client to connect to, instead of failing later with "invalid argument".
func requireDialablePath(t *testing.T, path string) {
	t.Helper()
	require.LessOrEqual(t, len(path), maxSocketPath, "socket path too long to dial: %s", path)
}

func newHarness(t *testing.T, configure ...func(f *fakeHub, cfg *Config)) *harness {
	t.Helper()
	testutil.ScrubHubEnv(t)
	home := shortTempDir(t)
	t.Cleanup(hub.SetTokenHome(home))

	clk := clock.NewFake(testStart)
	fh := newFakeHub(t, clk)
	client := hub.NewClientWithConfig(fh.srv.URL, fh.currentJWT(), testAgentID)
	sock := filepath.Join(home, ".scion", SocketName)
	requireDialablePath(t, sock)
	cfg := Config{Client: client, SocketPath: sock, Clock: clk}
	for _, fn := range configure {
		fn(fh, &cfg)
	}
	h := New(cfg)
	require.NoError(t, h.Start())
	t.Cleanup(func() { _ = h.Close() })

	hc := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
		},
	}
	t.Cleanup(hc.CloseIdleConnections)
	return &harness{hub: fh, clk: clk, client: client, helper: h, sock: sock, home: home, http: hc}
}

type proxyResult struct {
	status int
	header http.Header
	body   []byte
	code   string
}

// do sends a request over the delegation socket.
func (hs *harness) do(t *testing.T, method, path, grantID, body string, header map[string]string) proxyResult {
	t.Helper()
	req, err := http.NewRequest(method, "http://delegation"+path, strings.NewReader(body))
	require.NoError(t, err)
	if grantID != "" {
		req.Header.Set(GrantHeader, grantID)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := hs.http.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	res := proxyResult{status: resp.StatusCode, header: resp.Header, body: raw}
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil {
		res.code = e.Error.Code
	}
	return res
}

func (hs *harness) get(t *testing.T, grantID string) proxyResult {
	t.Helper()
	return hs.do(t, http.MethodGet, "/api/v1/agents/agent-2", grantID, "", nil)
}

func eventually(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	require.Eventually(t, cond, 5*time.Second, 5*time.Millisecond, msg)
}

func (hs *harness) waitExchanges(t *testing.T, n int) []exchangeRecord {
	t.Helper()
	eventually(t, func() bool { return len(hs.hub.exchangeLog()) >= n }, fmt.Sprintf("waiting for %d exchanges", n))
	return hs.hub.exchangeLog()
}

func lastBearer(t *testing.T, fh *fakeHub) string {
	t.Helper()
	uses := fh.useLog()
	require.NotEmpty(t, uses)
	return strings.TrimPrefix(uses[len(uses)-1].authorization, "Bearer ")
}

func TestProxy_ForwardsWithDelegatedCredentialOnly(t *testing.T) {
	hs := newHarness(t)

	res := hs.do(t, http.MethodPost, "/api/v1/agents/agent-2/messages?wait=1", "grant-1", `{"text":"hello"}`, map[string]string{
		"Content-Type":        "application/json",
		"Authorization":       "Bearer caller-supplied",
		"X-Scion-Agent-Token": "caller-agent-token",
		"Cookie":              "session=caller",
	})
	require.Equal(t, http.StatusOK, res.status, string(res.body))
	assert.Equal(t, "yes", res.header.Get("X-Fake-Hub"))
	assert.Empty(t, res.header.Get("Set-Cookie"))

	ex := hs.hub.exchangeLog()
	require.Len(t, ex, 1)
	assert.Equal(t, "grant-1", ex[0].grantID)
	assert.Equal(t, testAudience, ex[0].audience)
	assert.Equal(t, hs.hub.currentJWT(), ex[0].jwt)

	uses := hs.hub.useLog()
	require.Len(t, uses, 1)
	assert.Equal(t, "Bearer "+ex[0].token, uses[0].authorization)
	assert.Empty(t, uses[0].agentToken, "the agent credential must not accompany a delegated request")
	assert.Empty(t, uses[0].cookie)
	assert.Equal(t, http.MethodPost, uses[0].method)
	assert.Equal(t, "/api/v1/agents/agent-2/messages?wait=1", uses[0].path)
	assert.Equal(t, `{"text":"hello"}`, uses[0].body)

	// A second request reuses the held credential.
	res = hs.get(t, "grant-1")
	require.Equal(t, http.StatusOK, res.status)
	assert.Len(t, hs.hub.exchangeLog(), 1)
	assert.Equal(t, ex[0].token, lastBearer(t, hs.hub))
}

func TestProxy_RefusesRequestsItDoesNotForward(t *testing.T) {
	hs := newHarness(t)
	cases := []struct {
		name, method, path, grant, body string
		status                          int
		code                            string
	}{
		{"no grant", http.MethodGet, "/api/v1/agents/agent-2", "", "", http.StatusBadRequest, codeGrantRequired},
		{"malformed grant", http.MethodGet, "/api/v1/agents/agent-2", "../grant", "", http.StatusBadRequest, codeGrantInvalid},
		{"grant with space", http.MethodGet, "/api/v1/agents/agent-2", "grant 1", "", http.StatusBadRequest, codeGrantInvalid},
		{"not an API path", http.MethodGet, "/healthz", "grant-1", "", http.StatusBadRequest, codePathNotAllowed},
		{"other API version", http.MethodGet, "/api/v2/agents", "grant-1", "", http.StatusBadRequest, codePathNotAllowed},
		{"dot segment", http.MethodGet, "/api/v1/../healthz", "grant-1", "", http.StatusBadRequest, codePathNotAllowed},
		{"encoded dot segment", http.MethodGet, "/api/v1/%2e%2e/healthz", "grant-1", "", http.StatusBadRequest, codePathNotAllowed},
		{"mixed encoded dot segment", http.MethodGet, "/api/v1/.%2e/healthz", "grant-1", "", http.StatusBadRequest, codePathNotAllowed},
		{"mixed encoded dot segment reversed", http.MethodGet, "/api/v1/%2e./x", "grant-1", "", http.StatusBadRequest, codePathNotAllowed},
		{"encoded single dot", http.MethodGet, "/api/v1/%2e/x", "grant-1", "", http.StatusBadRequest, codePathNotAllowed},
		{"upper-case encoded dots", http.MethodGet, "/api/v1/%2E%2E/healthz", "grant-1", "", http.StatusBadRequest, codePathNotAllowed},
		{"body too large", http.MethodPost, "/api/v1/agents/agent-2/messages", "grant-1", strings.Repeat("x", maxRequestBody+1), http.StatusRequestEntityTooLarge, codeRequestTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := hs.do(t, tc.method, tc.path, tc.grant, tc.body, nil)
			assert.Equal(t, tc.status, res.status)
			assert.Equal(t, tc.code, res.code)
		})
	}
	assert.Empty(t, hs.hub.exchangeLog(), "a refused request must not exchange")
	assert.Empty(t, hs.hub.useLog())
}

// Flow 31 (.design/agent-delegation.md §17.2): after exchange, the token
// file, the child process environment and the sciontool logs carry no
// delegated credential. (Socket mode and owner: socket_linux_test.go.)
func TestFlow31_DelegatedCredentialStaysInMemory(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "agent.log")
	log.SetLogPath(logPath)
	log.SetDebug(true)
	log.SetQuiet(true)
	t.Cleanup(func() {
		log.SetQuiet(false)
		log.SetDebug(false)
		log.SetLogPath(filepath.Join(os.TempDir(), "agent.log"))
	})

	hs := newHarness(t)
	require.NoError(t, hub.WriteTokenFile(hs.hub.currentJWT(), 0, 0))

	// Exchange, use, refresh the agent credential (which rewrites the
	// token file and re-exchanges), use again, and re-exchange before
	// expiry.
	require.Equal(t, http.StatusOK, hs.get(t, "grant-1").status)
	_, _, err := hs.client.RefreshToken(context.Background())
	require.NoError(t, err)
	hs.waitExchanges(t, 2)
	require.Equal(t, http.StatusOK, hs.get(t, "grant-1").status)
	require.True(t, hs.clk.WaitFor(5*time.Second, func(n int) bool { return n >= 1 }))
	hs.clk.Advance(14 * time.Minute)
	hs.waitExchanges(t, 3)
	require.Equal(t, http.StatusOK, hs.get(t, "grant-1").status)

	issued := hs.hub.issuedTokens()
	require.Len(t, issued, 3)
	contains := func(s string) string {
		if strings.Contains(s, hub.DelegatedTokenPrefix) {
			return hub.DelegatedTokenPrefix
		}
		for _, tok := range issued {
			if strings.Contains(s, tok) {
				return tok
			}
		}
		return ""
	}

	// The token file holds the agent credential and nothing else.
	assert.Equal(t, hs.hub.currentJWT(), hub.ReadTokenFile())
	tokenFile, err := os.ReadFile(hub.TokenFilePath())
	require.NoError(t, err)
	assert.Empty(t, contains(string(tokenFile)), "token file")

	// No file under the agent home carries one.
	var files int
	require.NoError(t, filepath.WalkDir(hs.home, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		files++
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		assert.Empty(t, contains(string(data)), "file %s", path)
		return nil
	}))
	assert.Positive(t, files, "the walk must see the token file")

	// Neither sciontool's environment nor a child's carries one.
	for _, kv := range os.Environ() {
		assert.Empty(t, contains(kv), "environment entry")
	}
	child := exec.Command(os.Args[0], "-test.run=^$")
	child.Env = append(os.Environ(), printEnvVar+"=1")
	out, err := child.Output()
	require.NoError(t, err)
	require.Contains(t, string(out), printEnvVar+"=1", "the child must print its environment")
	assert.Empty(t, contains(string(out)), "child environment")

	// The logs describe the exchanges without the credential.
	logData, err := os.ReadFile(logPath)
	require.NoError(t, err)
	assert.Contains(t, string(logData), "Delegation helper: exchanged grant grant-1")
	assert.Contains(t, string(logData), "agent credential changed")
	assert.Empty(t, contains(string(logData)), "agent.log")
}

func TestReexchange_BeforeExpiry(t *testing.T) {
	hs := newHarness(t)
	require.Equal(t, http.StatusOK, hs.get(t, "grant-1").status)
	ex := hs.hub.exchangeLog()
	require.Len(t, ex, 1)
	first := ex[0].token

	// A 15-minute credential is re-exchanged 2 minutes before it expires,
	// with the same agent credential.
	due := testStart.Add(13 * time.Minute)
	require.True(t, hs.clk.WaitForTimer(5*time.Second, due), "re-exchange timer armed at expiry minus margin")
	hs.clk.Advance(13*time.Minute - time.Second)
	assert.Len(t, hs.hub.exchangeLog(), 1, "no re-exchange before the margin")
	hs.clk.Advance(time.Second)
	ex = hs.waitExchanges(t, 2)
	assert.Equal(t, ex[0].jwt, ex[1].jwt)
	assert.NotEqual(t, first, ex[1].token)

	require.Equal(t, http.StatusOK, hs.get(t, "grant-1").status)
	assert.Equal(t, ex[1].token, lastBearer(t, hs.hub))
	assert.Len(t, hs.hub.exchangeLog(), 2)
	// The earlier credential stays valid until it expires (at most 2).
	assert.Equal(t, "active", hs.hub.credState(first))
}

func TestReexchange_AfterAgentCredentialRefresh(t *testing.T) {
	hs := newHarness(t)
	jwt1 := hs.hub.currentJWT()
	require.Equal(t, http.StatusOK, hs.get(t, "grant-1").status)

	_, _, err := hs.client.RefreshToken(context.Background())
	require.NoError(t, err)
	jwt2 := hs.hub.currentJWT()
	require.NotEqual(t, jwt1, jwt2)

	// The refresh alone triggers the re-exchange, with the new agent
	// credential.
	ex := hs.waitExchanges(t, 2)
	assert.Equal(t, jwt1, ex[0].jwt)
	assert.Equal(t, jwt2, ex[1].jwt)

	// The next request uses the new credential; the old one no longer
	// works at the hub.
	res := hs.get(t, "grant-1")
	require.Equal(t, http.StatusOK, res.status)
	assert.Equal(t, ex[1].token, lastBearer(t, hs.hub))
	for _, u := range hs.hub.useLog() {
		assert.Equal(t, http.StatusOK, u.status)
	}
	assert.Len(t, hs.hub.exchangeLog(), 2)
}

func TestExchange_BothUnauthorizedAnswersWaitForNewAgentCredential(t *testing.T) {
	for _, code := range []string{"unauthorized", "agent_credential_invalid"} {
		t.Run(code, func(t *testing.T) {
			hs := newHarness(t)
			jwt1 := hs.hub.currentJWT()
			hs.hub.set(func(f *fakeHub) { f.refuse401[jwt1] = code })

			res := hs.get(t, "grant-1")
			assert.Equal(t, http.StatusServiceUnavailable, res.status)
			assert.Equal(t, codeRefreshPending, res.code)
			require.Len(t, hs.hub.exchangeLog(), 1)

			// The same agent credential is not tried again, at once or
			// after the backoff.
			res = hs.get(t, "grant-1")
			assert.Equal(t, codeRefreshPending, res.code)
			hs.clk.Advance(retryMaxDelay + time.Minute)
			res = hs.get(t, "grant-1")
			assert.Equal(t, http.StatusServiceUnavailable, res.status)
			assert.Equal(t, codeRefreshPending, res.code)
			assert.Len(t, hs.hub.exchangeLog(), 1)

			// A new agent credential is used at once.
			var jwt2 string
			hs.hub.set(func(f *fakeHub) { jwt2 = f.mintJWTLocked(); f.current = jwt2 })
			hs.client.SetToken(jwt2)
			ex := hs.waitExchanges(t, 2)
			assert.Equal(t, jwt2, ex[1].jwt)
			require.Equal(t, http.StatusOK, hs.get(t, "grant-1").status)
			assert.Equal(t, ex[1].token, lastBearer(t, hs.hub))
			assert.Len(t, hs.hub.exchangeLog(), 2)
		})
	}
}

func TestExchange_RefusedAgentCredentialRetriedAfterChangeAndBackoff(t *testing.T) {
	hs := newHarness(t)
	jwt1 := hs.hub.currentJWT()
	hs.hub.set(func(f *fakeHub) { f.refuse401[jwt1] = "agent_credential_invalid" })
	require.Equal(t, codeRefreshPending, hs.get(t, "grant-1").code)

	// The agent credential changes before the backoff ends: the exchange
	// waits for the backoff.
	var jwt2 string
	hs.hub.set(func(f *fakeHub) { jwt2 = f.mintJWTLocked(); f.current = jwt2 })
	hs.client.SetToken(jwt2)
	require.True(t, hs.clk.WaitFor(5*time.Second, func(n int) bool { return n >= 1 }), "retry armed for the end of the backoff")
	assert.Len(t, hs.hub.exchangeLog(), 1)

	hs.clk.Advance(retryBaseDelay)
	ex := hs.waitExchanges(t, 2)
	assert.Equal(t, jwt2, ex[1].jwt)
	require.Equal(t, http.StatusOK, hs.get(t, "grant-1").status)
}

func TestExchange_UnavailableRetriedWithSameAgentCredential(t *testing.T) {
	hs := newHarness(t)
	jwt1 := hs.hub.currentJWT()
	hs.hub.set(func(f *fakeHub) { f.nextStatus = []scriptedAnswer{{http.StatusServiceUnavailable, "unavailable"}} })

	res := hs.get(t, "grant-1")
	assert.Equal(t, http.StatusServiceUnavailable, res.status)
	assert.Equal(t, codeUnavailable, res.code)
	require.Len(t, hs.hub.exchangeLog(), 1)

	// Inside the backoff the helper answers itself, with Retry-After.
	res = hs.get(t, "grant-1")
	assert.Equal(t, http.StatusServiceUnavailable, res.status)
	assert.Equal(t, codeUnavailable, res.code)
	assert.Equal(t, "1", res.header.Get("Retry-After"))
	assert.Len(t, hs.hub.exchangeLog(), 1)

	// When the backoff ends the helper retries in the background, with
	// the same agent credential.
	require.True(t, hs.clk.WaitFor(5*time.Second, func(n int) bool { return n >= 1 }))
	hs.clk.Advance(retryBaseDelay)
	ex := hs.waitExchanges(t, 2)
	assert.Equal(t, jwt1, ex[0].jwt)
	assert.Equal(t, jwt1, ex[1].jwt)
	assert.NotEmpty(t, ex[1].token)

	require.Equal(t, http.StatusOK, hs.get(t, "grant-1").status)
	assert.Equal(t, ex[1].token, lastBearer(t, hs.hub))
	assert.Len(t, hs.hub.exchangeLog(), 2)
}

func TestSupersede_ClientUsesNewestAndRecoversWhenSuperseded(t *testing.T) {
	hs := newHarness(t)
	require.Equal(t, http.StatusOK, hs.get(t, "grant-1").status)
	held := hs.hub.exchangeLog()[0].token

	// Two exchanges elsewhere leave the helper's credential the oldest of
	// three, so the hub supersedes it.
	other1 := hs.hub.externalExchange("grant-1")
	other2 := hs.hub.externalExchange("grant-1")
	require.Equal(t, "superseded", hs.hub.credState(held))

	// The hub refuses the superseded credential; the helper exchanges
	// again and sends the request once more.
	res := hs.get(t, "grant-1")
	require.Equal(t, http.StatusOK, res.status)
	ex := hs.hub.exchangeLog()
	require.Len(t, ex, 2)
	uses := hs.hub.useLog()
	require.Len(t, uses, 3)
	assert.Equal(t, http.StatusUnauthorized, uses[1].status)
	assert.Equal(t, "Bearer "+held, uses[1].authorization)
	assert.Equal(t, http.StatusOK, uses[2].status)
	assert.Equal(t, "Bearer "+ex[1].token, uses[2].authorization)
	// The helper's exchange in turn superseded the oldest remaining one.
	assert.Equal(t, "superseded", hs.hub.credState(other1))
	assert.Equal(t, "active", hs.hub.credState(other2))
	assert.Equal(t, "active", hs.hub.credState(ex[1].token))

	// A re-exchange before expiry supersedes the oldest active credential
	// and leaves the helper's previous one valid; the helper presents the
	// newest.
	require.True(t, hs.clk.WaitForTimer(5*time.Second, testStart.Add(13*time.Minute)))
	hs.clk.Advance(13 * time.Minute)
	ex = hs.waitExchanges(t, 3)
	assert.Equal(t, "superseded", hs.hub.credState(other2))
	assert.Equal(t, "active", hs.hub.credState(ex[1].token))
	require.Equal(t, http.StatusOK, hs.get(t, "grant-1").status)
	assert.Equal(t, ex[2].token, lastBearer(t, hs.hub))
}

func TestProxy_RefusedTwiceIsReturnedToCaller(t *testing.T) {
	hs := newHarness(t)
	require.Equal(t, http.StatusOK, hs.get(t, "grant-1").status)
	// Every credential exchanged with this agent credential stops working
	// at the hub, but exchange still succeeds.
	hs.hub.set(func(f *fakeHub) {
		for _, c := range f.creds {
			c.revoked = "revoked"
		}
		f.ttl = 0
	})
	res := hs.get(t, "grant-1")
	assert.Equal(t, http.StatusUnauthorized, res.status)
	// One re-exchange and one retry, not a loop.
	assert.Len(t, hs.hub.exchangeLog(), 2)
	assert.Len(t, hs.hub.useLog(), 3)
	// A credential with no lifetime left gets no background re-exchange
	// (a zero-delay timer would re-exchange back to back on the real clock).
	assert.Zero(t, hs.clk.Pending())
}

func (hs *harness) tracked(id string) bool {
	hs.helper.mu.Lock()
	defer hs.helper.mu.Unlock()
	_, ok := hs.helper.grants[id]
	return ok
}

// TestReexchange_StopsWhenGrantExpiryCapsTheLifetime: near the grant's
// expiry the hub caps each credential at it, so a re-exchange can no longer
// extend the credential. The helper stops re-exchanging in the background
// instead of exchanging back to back as the lifetime shrinks.
func TestReexchange_StopsWhenGrantExpiryCapsTheLifetime(t *testing.T) {
	grantEnd := testStart.Add(20 * time.Minute)
	hs := newHarness(t, func(f *fakeHub, _ *Config) { f.grantExpires = grantEnd })
	require.Equal(t, http.StatusOK, hs.get(t, "grant-1").status)

	// 15-minute credential, re-exchanged at 13 min: the hub caps the new one
	// at the grant expiry (7 min left), which still extends it.
	require.True(t, hs.clk.WaitForTimer(5*time.Second, testStart.Add(13*time.Minute)))
	hs.clk.Advance(13 * time.Minute)
	ex := hs.waitExchanges(t, 2)
	assert.True(t, hs.hub.credExpiry(ex[1].token).Equal(grantEnd))

	// Its re-exchange (7 min less a fifth of it) returns the same expiry, so
	// nothing further is scheduled.
	next := testStart.Add(13*time.Minute + 7*time.Minute - 84*time.Second)
	require.True(t, hs.clk.WaitForTimer(5*time.Second, next))
	hs.clk.Advance(next.Sub(hs.clk.Now()))
	hs.waitExchanges(t, 3)
	eventually(t, func() bool { return hs.clk.Pending() == 0 }, "no re-exchange armed once the expiry cannot move")

	hs.clk.Advance(30 * time.Minute)
	assert.Len(t, hs.hub.exchangeLog(), 3, "no background exchange past the cap")
	// After the grant expires, a request gets the hub's denial.
	res := hs.get(t, "grant-1")
	assert.Equal(t, http.StatusForbidden, res.status)
	assert.Equal(t, "grant_inactive", res.code)
}

func TestIdleGrant_IsForgottenAndNotReexchanged(t *testing.T) {
	hs := newHarness(t)
	require.Equal(t, http.StatusOK, hs.get(t, "grant-1").status)

	// Re-exchanged every 13 minutes while used within the last hour; the
	// first timer after an hour without a request forgets the grant.
	for i := 0; i < 10 && hs.tracked("grant-1"); i++ {
		require.True(t, hs.clk.WaitFor(5*time.Second, func(n int) bool { return n >= 1 }))
		n := len(hs.hub.exchangeLog())
		hs.clk.Advance(13 * time.Minute)
		eventually(t, func() bool { return len(hs.hub.exchangeLog()) > n || !hs.tracked("grant-1") }, "re-exchange or forget")
	}
	require.False(t, hs.tracked("grant-1"))
	assert.Len(t, hs.hub.exchangeLog(), 5, "exchanges at 0, 13, 26, 39 and 52 minutes")
	assert.Zero(t, hs.clk.Pending())

	// An agent credential change no longer re-exchanges it.
	var jwt2 string
	hs.hub.set(func(f *fakeHub) { jwt2 = f.mintJWTLocked(); f.current = jwt2 })
	hs.client.SetToken(jwt2)
	require.Never(t, func() bool { return len(hs.hub.exchangeLog()) > 5 }, 200*time.Millisecond, 10*time.Millisecond)

	// A new request exchanges it again.
	require.Equal(t, http.StatusOK, hs.get(t, "grant-1").status)
	assert.Len(t, hs.hub.exchangeLog(), 6)
}

func TestIdleGrant_FailingGrantIsForgotten(t *testing.T) {
	hs := newHarness(t, func(f *fakeHub, _ *Config) {
		for i := 0; i < 100; i++ {
			f.nextStatus = append(f.nextStatus, scriptedAnswer{http.StatusServiceUnavailable, "unavailable"})
		}
	})
	require.Equal(t, codeUnavailable, hs.get(t, "grant-1").code)

	// Background retries back off to 5 minutes and stop once the grant has
	// been idle for an hour.
	for i := 0; i < 40 && hs.tracked("grant-1"); i++ {
		require.True(t, hs.clk.WaitFor(5*time.Second, func(n int) bool { return n >= 1 }))
		n := len(hs.hub.exchangeLog())
		hs.clk.Advance(retryMaxDelay)
		eventually(t, func() bool { return len(hs.hub.exchangeLog()) > n || !hs.tracked("grant-1") }, "retry or forget")
	}
	require.False(t, hs.tracked("grant-1"))
	assert.Zero(t, hs.clk.Pending())
	assert.False(t, hs.clk.Now().Before(testStart.Add(idleAfter)), "kept until an hour without requests")
}

func TestGrantTable_IdleGrantsMakeRoom(t *testing.T) {
	hs := newHarness(t)
	for i := 0; i < maxGrants; i++ {
		_, err := hs.helper.lookup(fmt.Sprintf("grant-%d", i))
		require.NoError(t, err)
	}
	res := hs.get(t, "grant-new")
	assert.Equal(t, http.StatusServiceUnavailable, res.status)
	assert.Equal(t, codeTooManyGrants, res.code)
	assert.Empty(t, hs.hub.exchangeLog())

	hs.clk.Advance(idleAfter)
	require.Equal(t, http.StatusOK, hs.get(t, "grant-new").status)
	assert.True(t, hs.tracked("grant-new"))
	assert.False(t, hs.tracked("grant-0"), "idle grants were forgotten to make room")
}

func TestExchange_DenialIsPassedThroughAndGrantForgotten(t *testing.T) {
	hs := newHarness(t)
	hs.hub.set(func(f *fakeHub) { f.nextStatus = []scriptedAnswer{{http.StatusForbidden, "grant_inactive"}} })

	res := hs.get(t, "grant-1")
	assert.Equal(t, http.StatusForbidden, res.status)
	assert.Equal(t, "grant_inactive", res.code)
	hs.helper.mu.Lock()
	_, tracked := hs.helper.grants["grant-1"]
	hs.helper.mu.Unlock()
	assert.False(t, tracked, "a denied grant is not tracked")
	assert.Zero(t, hs.clk.Pending(), "nothing is scheduled for a denied grant")

	// A later request exchanges afresh.
	require.Equal(t, http.StatusOK, hs.get(t, "grant-1").status)
	assert.Len(t, hs.hub.exchangeLog(), 2)
}

func TestAudience_ResolvedFromHubHealth(t *testing.T) {
	cases := []struct {
		name    string
		hubID   string
		healthz string
		want    string
	}{
		{"hub with an ID", "hub-9", "", "scion-hub:hub-9"},
		{"hub without an ID", "", `{"status":"healthy"}`, "scion-hub"},
		{"combined server", "hub-1", `{"status":"healthy","hub":{"hub_id":"hub-1"}}`, "scion-hub:hub-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hs := newHarness(t, func(f *fakeHub, _ *Config) {
				f.hubID = tc.hubID
				f.healthzRaw = tc.healthz
			})
			require.Equal(t, http.StatusOK, hs.get(t, "grant-1").status)
			assert.Equal(t, tc.want, hs.hub.exchangeLog()[0].audience)
		})
	}

	t.Run("not a hub health answer", func(t *testing.T) {
		hs := newHarness(t, func(f *fakeHub, _ *Config) { f.healthzRaw = `{"error":"from a proxy"}` })
		res := hs.get(t, "grant-1")
		assert.Equal(t, http.StatusServiceUnavailable, res.status)
		assert.Empty(t, hs.hub.exchangeLog())
	})

	t.Run("unreadable health answer wraps the parse error", func(t *testing.T) {
		hs := newHarness(t, func(f *fakeHub, _ *Config) { f.healthzRaw = "not json" })
		_, err := hs.helper.resolveAudience(context.Background())
		require.Error(t, err)
		var syntaxErr *json.SyntaxError
		assert.True(t, errors.As(err, &syntaxErr), "the JSON error is wrapped: %v", err)
		assert.Contains(t, err.Error(), "/healthz did not answer JSON")
	})

	t.Run("configured audience", func(t *testing.T) {
		hs := newHarness(t, func(f *fakeHub, cfg *Config) {
			f.healthzRaw = "not json"
			cfg.Audience = testAudience
		})
		require.Equal(t, http.StatusOK, hs.get(t, "grant-1").status)
		assert.Equal(t, testAudience, hs.hub.exchangeLog()[0].audience)
	})
}

func TestRefreshMargin(t *testing.T) {
	assert.Equal(t, 2*time.Minute, refreshMargin(time.Hour))
	assert.Equal(t, 2*time.Minute, refreshMargin(15*time.Minute))
	assert.Equal(t, time.Minute, refreshMargin(5*time.Minute))
	assert.Equal(t, 5*time.Second, refreshMargin(10*time.Second))
}
