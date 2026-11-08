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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// profileUnresolvedBody is a broker runtime_unavailable body for a
// stored-profile miss (ptone/scion#4955).
func profileUnresolvedBody(profileJSON string) string {
	return `{"error":{"code":"runtime_unavailable","message":"requested runtime profile cannot be resolved: agent \"a\": profile \"gone\" not found in the hub's stored profiles (available profiles: x)","details":{"reason":"profile_unresolved","profile":` + profileJSON + `}}}`
}

func decodeHubError(t *testing.T, rec *httptest.ResponseRecorder) (code, message string) {
	t.Helper()
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	return body.Error.Code, body.Error.Message
}

// A profile_unresolved refusal keeps the code and the 503, names the
// profile in the hub's own text, drops the retry-later wording and the
// Retry-After, and does not relay the broker's message.
func TestWriteBrokerRuntimeUnavailable_ProfileUnresolvedNamesProfile(t *testing.T) {
	rec := httptest.NewRecorder()
	err := &brokerStatusError{StatusCode: http.StatusServiceUnavailable, Body: profileUnresolvedBody(`"gone"`), RetryAfter: "30"}
	require.True(t, writeBrokerRuntimeUnavailable(rec, err, "docker"))

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Empty(t, rec.Header().Get("Retry-After"), "a stored-profile miss is not transient")
	code, msg := decodeHubError(t, rec)
	require.Equal(t, brokerCodeRuntimeUnavailable, code)
	require.Contains(t, msg, `Runtime profile "gone" cannot be resolved`)
	require.NotContains(t, strings.ToLower(msg), "retry later")
	require.NotContains(t, msg, "available profiles: x", "the broker's message must not be relayed")
}

// An unset active_profile (empty profile) gets the default-profile text.
func TestWriteBrokerRuntimeUnavailable_ProfileUnresolvedDefaultProfile(t *testing.T) {
	rec := httptest.NewRecorder()
	err := &brokerStatusError{StatusCode: http.StatusServiceUnavailable, Body: profileUnresolvedBody(`""`), RetryAfter: "30"}
	require.True(t, writeBrokerRuntimeUnavailable(rec, err, "docker"))

	require.Empty(t, rec.Header().Get("Retry-After"))
	_, msg := decodeHubError(t, rec)
	require.Contains(t, msg, "active_profile is unset or not one of the hub's stored profiles")
}

// A profile detail that is not a string or is too long is not echoed.
func TestWriteBrokerRuntimeUnavailable_ProfileUnresolvedBadProfileDetail(t *testing.T) {
	for _, profileJSON := range []string{`42`, `null`, `"` + strings.Repeat("p", maxRelayedProfileNameLen+1) + `"`} {
		rec := httptest.NewRecorder()
		err := &brokerStatusError{StatusCode: http.StatusServiceUnavailable, Body: profileUnresolvedBody(profileJSON)}
		require.True(t, writeBrokerRuntimeUnavailable(rec, err, "docker"))
		_, msg := decodeHubError(t, rec)
		require.Contains(t, msg, "active_profile is unset", "profile detail %s", profileJSON)
		require.NotContains(t, msg, "ppp")
	}
}

// Without the reason, or with another reason, the generic retryable relay
// is unchanged.
func TestWriteBrokerRuntimeUnavailable_OtherReasonKeepsGenericRelay(t *testing.T) {
	for _, body := range []string{
		`{"error":{"code":"runtime_unavailable","message":"runtime \"kubernetes\" is not available on this broker"}}`,
		`{"error":{"code":"runtime_unavailable","message":"x","details":{"reason":"something_else","profile":"gone"}}}`,
	} {
		rec := httptest.NewRecorder()
		err := &brokerStatusError{StatusCode: http.StatusServiceUnavailable, Body: body, RetryAfter: "12"}
		require.True(t, writeBrokerRuntimeUnavailable(rec, err, "kubernetes"))
		require.Equal(t, "12", rec.Header().Get("Retry-After"))
		_, msg := decodeHubError(t, rec)
		require.Equal(t, brokerRuntimeUnavailableMessage("kubernetes"), msg)
	}
}

// The reason does not change the hub's classification of the answer as
// the broker's runtime_unavailable.
func TestBrokerProfileUnresolved_StillRuntimeUnavailable(t *testing.T) {
	err := &brokerStatusError{StatusCode: http.StatusServiceUnavailable, Body: profileUnresolvedBody(`"gone"`)}
	require.True(t, isBrokerRuntimeUnavailable(err))
	require.True(t, startRefusedBeforeRun(err))
	profile, ok := brokerProfileUnresolved(err)
	require.True(t, ok)
	require.Equal(t, "gone", profile)
}
