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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testDelegatedToken = "scion_adt_dGVzdC1kZWxlZ2F0ZWQtY3JlZGVudGlhbA"

func TestExchangeDelegation_SendsAgentCredentialAndAudience(t *testing.T) {
	scrubHubEnv(t)
	expires := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/v1/agents/agent-1/delegations/grant-1/exchange", r.URL.Path)
		assert.Equal(t, "agent-jwt", r.Header.Get("X-Scion-Agent-Token"))
		assert.Empty(t, r.Header.Get("Authorization"))
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		raw, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var body map[string]any
		require.NoError(t, json.Unmarshal(raw, &body))
		assert.Equal(t, map[string]any{"audience": "scion-hub:hub-1"}, body)

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":        testDelegatedToken,
			"expiresAt":    expires,
			"grantId":      "grant-1",
			"credentialId": "cred-1",
			"audience":     "scion-hub:hub-1",
			"permissions":  []string{"agent.read"},
		})
	}))
	defer server.Close()

	client := NewClientWithConfig(server.URL, "agent-jwt", "agent-1")
	cred, err := client.ExchangeDelegation(context.Background(), "grant-1", "scion-hub:hub-1")
	require.NoError(t, err)
	assert.Equal(t, testDelegatedToken, cred.Token)
	assert.True(t, cred.ExpiresAt.Equal(expires))
	assert.Equal(t, "grant-1", cred.GrantID)
	assert.Equal(t, "cred-1", cred.CredentialID)
	assert.Equal(t, "scion-hub:hub-1", cred.Audience)
	assert.Equal(t, []string{"agent.read"}, cred.Permissions)
}

func TestExchangeDelegation_RefusalsAreClassified(t *testing.T) {
	scrubHubEnv(t)
	cases := []struct {
		name           string
		status         int
		code           string
		reauthenticate bool
		retryable      bool
	}{
		{"revoked agent credential", http.StatusUnauthorized, "unauthorized", true, false},
		{"missing or expired agent credential row", http.StatusUnauthorized, "agent_credential_invalid", true, false},
		{"credential status lookup fault", http.StatusServiceUnavailable, "unavailable", false, true},
		{"audit write failed", http.StatusInternalServerError, "audit_failed", false, true},
		{"grant inactive", http.StatusForbidden, "grant_inactive", false, false},
		{"grant not found", http.StatusNotFound, "grant_not_found", false, false},
		{"wrong audience", http.StatusBadRequest, "invalid_audience", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprintf(w, `{"error":{"code":%q,"message":"refused"}}`, tc.code)
			}))
			defer server.Close()

			client := NewClientWithConfig(server.URL, "agent-jwt", "agent-1")
			cred, err := client.ExchangeDelegation(context.Background(), "grant-1", "scion-hub")
			require.Error(t, err)
			assert.Nil(t, cred)
			var xe *ExchangeError
			require.True(t, errors.As(err, &xe), "error is %T", err)
			assert.Equal(t, tc.status, xe.StatusCode)
			assert.Equal(t, tc.code, xe.Code)
			assert.Equal(t, tc.reauthenticate, xe.Reauthenticate())
			assert.Equal(t, tc.retryable, xe.Retryable())
		})
	}
}

func TestExchangeDelegation_RejectsResponseWithoutDelegatedCredential(t *testing.T) {
	scrubHubEnv(t)
	cases := map[string]string{
		"no prefix":     `{"token":"opaque","expiresAt":"2026-10-24T12:00:00Z","grantId":"grant-1"}`,
		"bare prefix":   `{"token":"scion_adt_","expiresAt":"2026-10-24T12:00:00Z","grantId":"grant-1"}`,
		"other grant":   `{"token":"` + testDelegatedToken + `","expiresAt":"2026-10-24T12:00:00Z","grantId":"grant-2"}`,
		"no expiry":     `{"token":"` + testDelegatedToken + `","grantId":"grant-1"}`,
		"not JSON body": `token=` + testDelegatedToken,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, body)
			}))
			defer server.Close()

			client := NewClientWithConfig(server.URL, "agent-jwt", "agent-1")
			cred, err := client.ExchangeDelegation(context.Background(), "grant-1", "scion-hub")
			require.Error(t, err)
			assert.Nil(t, cred)
			assert.NotContains(t, err.Error(), DelegatedTokenPrefix)
		})
	}
}

func TestExchangeDelegation_RequiresGrantAndAudience(t *testing.T) {
	scrubHubEnv(t)
	client := NewClientWithConfig("http://127.0.0.1:1", "agent-jwt", "agent-1")
	_, err := client.ExchangeDelegation(context.Background(), "", "scion-hub")
	assert.Error(t, err)
	_, err = client.ExchangeDelegation(context.Background(), "grant-1", "")
	assert.Error(t, err)
	var unconfigured *Client
	_, err = unconfigured.ExchangeDelegation(context.Background(), "grant-1", "scion-hub")
	assert.Error(t, err)
}

func TestDelegatedCredential_FormattingOmitsToken(t *testing.T) {
	cred := DelegatedCredential{
		Token:        testDelegatedToken,
		ExpiresAt:    time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC),
		GrantID:      "grant-1",
		CredentialID: "cred-1",
	}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		for _, v := range []any{cred, &cred} {
			out := fmt.Sprintf(verb, v)
			assert.NotContains(t, out, DelegatedTokenPrefix, "verb %s on %T", verb, v)
			assert.True(t, strings.Contains(out, "cred-1"), "verb %s on %T: %s", verb, v, out)
		}
	}
}

func TestTokenHomeIsolated_OnlyAfterSetTokenHomeUnderTest(t *testing.T) {
	before := TokenHomeIsolated()
	assert.Equal(t, tokenHomeOverridden, before, "under go test, isolated means SetTokenHome is in effect")
	restore := SetTokenHome(t.TempDir())
	assert.True(t, TokenHomeIsolated())
	restore()
	assert.Equal(t, before, TokenHomeIsolated())
}
