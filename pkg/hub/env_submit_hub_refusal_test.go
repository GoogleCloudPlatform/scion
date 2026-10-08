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

//go:build !no_sqlite

package hub

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The env submit step answers a refusal the hub raised while finalizing the
// create with the hub's own classification, not a 502 (ptone/scion#3452).

func TestSubmitAgentEnv_RelaysHubRefusal(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{
			name:   "token issue chain outcome",
			err:    &agentTokenIssueError{Site: mintSiteCreate, Cause: DenyCauseCeilingOrphaned, Err: errors.New("no delegation record")},
			status: http.StatusForbidden,
			code:   ErrCodeForbidden,
		},
		{
			name:   "token issue chain outcome, source not accepted",
			err:    &agentTokenIssueError{Site: mintSiteCreate, Cause: DenyCauseCeilingSourceNotAllowed, Err: errors.New("source")},
			status: http.StatusForbidden,
			code:   ErrCodeForbidden,
		},
		{
			name:   "token issue lookup fault",
			err:    &agentTokenIssueError{Site: mintSiteCreate, Lookup: true, Err: errors.New("store down")},
			status: http.StatusServiceUnavailable,
			code:   ErrCodeUnavailable,
		},
		{
			name:   "broker lacks empty-per-agent capability",
			err:    &brokerLacksEmptyPerAgentError{broker: "b1"},
			status: http.StatusPreconditionFailed,
			code:   ErrCodeUnsupportedCapability,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// What the hub's own classifier writes for this error.
			want := httptest.NewRecorder()
			require.True(t, writeAgentTokenIssueError(want, tc.err) || writeEmptyPerAgentCapabilityError(want, tc.err))
			require.Equal(t, tc.status, want.Code)

			rec := submitEnvWithFinalizeErr(t, "env-hub-refusal", tc.err)

			require.Equal(t, tc.status, rec.Code, rec.Body.String())
			assert.JSONEq(t, want.Body.String(), rec.Body.String(), "relayed exactly as the hub classifies it")
			assert.Contains(t, rec.Body.String(), `"code":"`+tc.code+`"`)
		})
	}
}

// TestSubmitAgentEnv_UnclassifiedTokenErrorStays502 keeps other failures
// unchanged: a token service error the hub does not classify as a refusal
// still answers 502.
func TestSubmitAgentEnv_UnclassifiedTokenErrorStays502(t *testing.T) {
	rec := submitEnvWithFinalizeErr(t, "env-token-svc", &agentTokenIssueError{Site: mintSiteCreate, Err: errors.New("token service")})
	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
}
