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

package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	scionruntime "github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The --format json document (ptone/scion#5593): generation context,
// Platform Health state and reason codes, and the verbatim summary.

var doctorTestNow = time.Date(2026, 10, 22, 10, 15, 4, 0, time.UTC)

// runDoctorJSON runs the Hub and Platform Health sections against hubEP
// with client and returns stdout and the decoded top-level object.
func runDoctorJSON(t *testing.T, hubEP string, client hubclient.Client, authSource string) ([]byte, map[string]json.RawMessage) {
	t.Helper()
	var buf bytes.Buffer
	r := &doctorReport{stdout: &buf, json: true, now: func() time.Time { return doctorTestNow }, authSource: authSource}
	r.platformHealthSection(r.hubChecks(hubEP, client, ""))
	require.NoError(t, r.finish())
	dec := json.NewDecoder(bytes.NewReader(buf.Bytes()))
	var doc map[string]json.RawMessage
	require.NoError(t, dec.Decode(&doc), "stdout: %s", buf.String())
	var extra json.RawMessage
	require.ErrorIs(t, dec.Decode(&extra), io.EOF, "nothing may follow the JSON object")
	return buf.Bytes(), doc
}

func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func closedServerURL() string {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	return url
}

func TestDoctorJSON_TopLevelShape(t *testing.T) {
	f := &fakeDoctorHub{summaryBody: summaryJSON(t, degradedSummary())}
	srv := f.serve(t)

	_, doc := runDoctorJSON(t, srv.URL, newDoctorTestClient(t, srv.URL), hubAuthSourceEnvHubToken)
	assert.Equal(t, []string{"formatVersion", "general", "generatedAt", "healthSummary", "hub", "hubHealth", "platformHealth", "runtime", "status"}, sortedKeys(doc))
	assert.JSONEq(t, `2`, string(doc["formatVersion"]))
	assert.JSONEq(t, `"2026-10-22T10:15:04Z"`, string(doc["generatedAt"]))
	for _, key := range []string{"general", "hubHealth", "runtime"} {
		var sec map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(doc[key], &sec), key)
		assert.True(t, strings.HasPrefix(string(sec["checks"]), "["), "%s.checks is an array", key)
	}
}

func TestDoctorJSON_PlatformHealthStates(t *testing.T) {
	type want struct {
		state, status, reason, message string
		httpStatus                     int
		summary                        bool
	}
	tests := []struct {
		name string
		// hub returns the endpoint and client; nil client means none.
		hub  func(t *testing.T) (string, hubclient.Client)
		want want
	}{
		{
			name: "ok",
			hub: func(t *testing.T) (string, hubclient.Client) {
				srv := (&fakeDoctorHub{summaryBody: summaryJSON(t, degradedSummary())}).serve(t)
				return srv.URL, newDoctorTestClient(t, srv.URL)
			},
			want: want{state: doctorPHStateOK, status: "warn", summary: true},
		},
		{
			name: "permission denied",
			hub: func(t *testing.T) (string, hubclient.Client) {
				srv := (&fakeDoctorHub{summaryStatus: http.StatusForbidden, summaryBody: `{"error":{"code":"forbidden","message":"non-user identity"}}`, brokersBody: `{"brokers":[]}`, agentsBody: `{"agents":[]}`}).serve(t)
				return srv.URL, newDoctorTestClient(t, srv.URL)
			},
			want: want{state: doctorPHStateSkipped, reason: doctorPHReasonPermissionDenied, message: doctorHealthSummaryForbidden, httpStatus: 403},
		},
		{
			name: "older hub without the endpoint",
			hub: func(t *testing.T) (string, hubclient.Client) {
				srv := (&fakeDoctorHub{summaryStatus: http.StatusNotFound, summaryBody: `{"error":{"code":"not_found","message":"not found"}}`, brokersBody: `{"brokers":[]}`, agentsBody: `{"agents":[]}`}).serve(t)
				return srv.URL, newDoctorTestClient(t, srv.URL)
			},
			want: want{state: doctorPHStateSkipped, reason: doctorPHReasonNotSupported, message: "This Hub does not provide a health summary (older Hub version)", httpStatus: 404},
		},
		{
			name: "hub unavailable",
			hub: func(t *testing.T) (string, hubclient.Client) {
				srv := (&fakeDoctorHub{summaryStatus: http.StatusServiceUnavailable, summaryBody: `<html>down</html>`, brokersBody: `{"brokers":[]}`, agentsBody: `{"agents":[]}`}).serve(t)
				return srv.URL, newDoctorTestClient(t, srv.URL)
			},
			want: want{state: doctorPHStateUnavailable, reason: doctorPHReasonHubUnavailable, message: doctorHealthSummaryUnavailable, httpStatus: 503},
		},
		{
			name: "other HTTP error",
			hub: func(t *testing.T) (string, hubclient.Client) {
				srv := (&fakeDoctorHub{summaryStatus: http.StatusInternalServerError, summaryBody: `{"error":{"code":"internal","message":"boom"}}`, brokersBody: `{"brokers":[]}`, agentsBody: `{"agents":[]}`}).serve(t)
				return srv.URL, newDoctorTestClient(t, srv.URL)
			},
			want: want{state: doctorPHStateUnavailable, reason: doctorPHReasonError, httpStatus: 500},
		},
		{
			name: "unreadable body",
			hub: func(t *testing.T) (string, hubclient.Client) {
				srv := (&fakeDoctorHub{summaryBody: `{"status":`, brokersBody: `{"brokers":[]}`, agentsBody: `{"agents":[]}`}).serve(t)
				return srv.URL, newDoctorTestClient(t, srv.URL)
			},
			want: want{state: doctorPHStateUnavailable, reason: doctorPHReasonError},
		},
		{
			name: "hub unreachable",
			hub: func(t *testing.T) (string, hubclient.Client) {
				url := closedServerURL()
				return url, newDoctorTestClient(t, url)
			},
			want: want{state: doctorPHStateSkipped, reason: doctorPHReasonHubUnreachable, message: "Skipped (Hub connectivity check failed)"},
		},
		{
			name: "no hub client",
			hub: func(t *testing.T) (string, hubclient.Client) {
				srv := (&fakeDoctorHub{}).serve(t)
				return srv.URL, nil
			},
			want: want{state: doctorPHStateSkipped, reason: doctorPHReasonClientUnavailable, message: "Skipped (Hub client not available)"},
		},
		{
			name: "no hub endpoint",
			hub:  func(t *testing.T) (string, hubclient.Client) { return "", nil },
			want: want{state: doctorPHStateSkipped, reason: doctorPHReasonNoHubEndpoint, message: "No Hub endpoint configured"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ep, client := tt.hub(t)
			_, doc := runDoctorJSON(t, ep, client, "")

			var ph map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(doc["platformHealth"], &ph))
			var got doctorJSONPlatformHealth
			require.NoError(t, json.Unmarshal(doc["platformHealth"], &got))

			assert.Equal(t, tt.want.state, got.State)
			assert.Equal(t, tt.want.status, got.Status)
			assert.Equal(t, tt.want.reason, got.Reason)
			assert.Equal(t, tt.want.httpStatus, got.HTTPStatus)
			if tt.want.message != "" {
				assert.Equal(t, tt.want.message, got.Message)
			}
			require.NotEmpty(t, got.Checks)
			assert.Equal(t, doctorHealthSummaryCheck, got.Checks[0].Name)

			if tt.want.state == doctorPHStateOK {
				assert.Equal(t, []string{"checks", "state", "status"}, sortedKeys(ph), "ok carries no reason")
			} else {
				assert.NotContains(t, ph, "status", "no verdict when the summary was not read")
				assert.NotEmpty(t, got.Message)
				assert.NotContains(t, got.Message, "\n")
				if tt.want.httpStatus == 0 {
					assert.NotContains(t, ph, "httpStatus")
				}
			}
			if tt.want.summary {
				assert.Contains(t, doc, "healthSummary")
			} else {
				assert.NotContains(t, doc, "healthSummary")
			}
		})
	}
}

func TestDoctorJSON_PlatformHealthStatusFollowsSummary(t *testing.T) {
	for status, want := range map[string]string{healthSummaryHealthy: "pass", healthSummaryDegraded: "warn", healthSummaryUnhealthy: "fail"} {
		t.Run(status, func(t *testing.T) {
			s := degradedSummary()
			s.Status = status
			srv := (&fakeDoctorHub{summaryBody: summaryJSON(t, s)}).serve(t)
			_, doc := runDoctorJSON(t, srv.URL, newDoctorTestClient(t, srv.URL), "")
			var got doctorJSONPlatformHealth
			require.NoError(t, json.Unmarshal(doc["platformHealth"], &got))
			assert.Equal(t, want, got.Status)
		})
	}
}

func hubObject(t *testing.T, doc map[string]json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	require.Contains(t, doc, "hub")
	var h map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(doc["hub"], &h))
	return h
}

func TestDoctorJSON_GenerationContext(t *testing.T) {
	t.Run("from the summary", func(t *testing.T) {
		srv := (&fakeDoctorHub{
			healthzBody: `{"status":"healthy","scionVersion":"9.9.9"}`,
			summaryBody: summaryJSON(t, degradedSummary()),
		}).serve(t)
		_, doc := runDoctorJSON(t, srv.URL, newDoctorTestClient(t, srv.URL), hubAuthSourceStoredLogin)
		assert.JSONEq(t, `{
			"endpoint": "`+srv.URL+`",
			"source": "healthSummary",
			"version": "1.4.0",
			"instanceId": "i1",
			"summaryGeneratedAt": "2026-10-12T10:00:00Z",
			"auth": {"source": "stored_login"}
		}`, string(doc["hub"]), "the summary wins over /healthz")
	})

	t.Run("from healthz when the summary is not readable", func(t *testing.T) {
		srv := (&fakeDoctorHub{
			healthzBody:   `{"status":"healthy","version":"0.1.0","scionVersion":"1.5.2","hub_id":"hub-identity"}`,
			summaryStatus: http.StatusForbidden,
			summaryBody:   `{"error":{"code":"forbidden","message":"forbidden"}}`,
			brokersBody:   `{"brokers":[]}`,
			agentsBody:    `{"agents":[]}`,
		}).serve(t)
		_, doc := runDoctorJSON(t, srv.URL, newDoctorTestClient(t, srv.URL), hubAuthSourceEnvHubToken)
		assert.JSONEq(t, `{
			"endpoint": "`+srv.URL+`",
			"source": "healthz",
			"version": "1.5.2",
			"auth": {"source": "env_hub_token"}
		}`, string(doc["hub"]), "no instance id or summary time without the summary; hub_id is not an instance id")
	})

	t.Run("absent when the hub gives nothing", func(t *testing.T) {
		srv := (&fakeDoctorHub{
			summaryStatus: http.StatusForbidden,
			summaryBody:   `{"error":{"code":"forbidden","message":"forbidden"}}`,
			brokersBody:   `{"brokers":[]}`,
			agentsBody:    `{"agents":[]}`,
		}).serve(t)
		_, doc := runDoctorJSON(t, srv.URL, newDoctorTestClient(t, srv.URL), "")
		assert.Equal(t, []string{"endpoint"}, sortedKeys(hubObject(t, doc)))
	})

	t.Run("summary without context fields", func(t *testing.T) {
		s := degradedSummary()
		s.GeneratedAt = time.Time{}
		s.Hub.Version = ""
		s.Hub.InstanceID = ""
		srv := (&fakeDoctorHub{healthzBody: `{"status":"healthy","scionVersion":"1.5.2"}`, summaryBody: summaryJSON(t, s)}).serve(t)
		_, doc := runDoctorJSON(t, srv.URL, newDoctorTestClient(t, srv.URL), "")
		assert.Equal(t, []string{"endpoint", "source"}, sortedKeys(hubObject(t, doc)), "empty values are omitted, not filled in from /healthz")
	})

	t.Run("hub unreachable", func(t *testing.T) {
		url := closedServerURL()
		_, doc := runDoctorJSON(t, url, newDoctorTestClient(t, url), hubAuthSourceNone)
		assert.JSONEq(t, `{"endpoint": "`+url+`", "auth": {"source": "none"}}`, string(doc["hub"]))
	})

	t.Run("no hub configured", func(t *testing.T) {
		_, doc := runDoctorJSON(t, "", nil, "")
		assert.NotContains(t, doc, "hub")
	})
}

func TestDoctorJSON_HealthSummaryIsTheHubBodyVerbatim(t *testing.T) {
	s := degradedSummary()
	s.Attention[0].Message = `Runtime broker <gpu-1> & "east" is offline`
	compact := summaryJSON(t, s)
	var indented bytes.Buffer
	require.NoError(t, json.Indent(&indented, []byte(compact), "", "\t  "))
	// Literal <, > and & where the encoder would escape them, a \u escape
	// and a non-ASCII character: none of these may be re-encoded.
	value := strings.NewReplacer(`\u003c`, "<", `\u003e`, ">", `\u0026`, "&").Replace(indented.String())
	value = strings.Replace(value, `"status": "degraded"`, `"status"  :   "degraded", "note": "café \u00e9"`, 1)
	require.True(t, json.Valid([]byte(value)))
	body := " \n" + value + "\n\n"

	srv := (&fakeDoctorHub{summaryBody: body}).serve(t)
	out, doc := runDoctorJSON(t, srv.URL, newDoctorTestClient(t, srv.URL), "")

	assert.Equal(t, value, string(doc["healthSummary"]), "healthSummary must be the hub's bytes")
	assert.Contains(t, string(out), `"healthSummary": `+value+"\n}")
}

func TestSpliceHealthSummary(t *testing.T) {
	got, err := spliceHealthSummary([]byte("{\n  \"a\": 1\n}"), json.RawMessage(" {\"x\":  [1,2]}\n"))
	require.NoError(t, err)
	assert.Equal(t, "{\n  \"a\": 1,\n  \"healthSummary\": {\"x\":  [1,2]}\n}", string(got))

	got, err = spliceHealthSummary([]byte("{\n  \"a\": 1\n}"), nil)
	require.NoError(t, err)
	assert.Equal(t, "{\n  \"a\": 1\n}", string(got))

	_, err = spliceHealthSummary([]byte("{\n  \"a\": 1\n}"), json.RawMessage(`{"x":`))
	assert.Error(t, err)
}

func TestDoctorJSON_MessagesAreOneLine(t *testing.T) {
	var buf bytes.Buffer
	r := &doctorReport{stdout: &buf, json: true}
	r.add(&r.General, scionruntime.CheckResult{Name: "multi", Status: "warn", Message: "line one\nline two", Remediation: "do\r\nthis"})
	require.NoError(t, r.finish())
	var doc struct {
		General doctorJSONSection `json:"general"`
	}
	require.NoError(t, json.Unmarshal(buf.Bytes(), &doc))
	require.Len(t, doc.General.Checks, 1)
	assert.Equal(t, "line one line two", doc.General.Checks[0].Message)
	assert.Equal(t, "do  this", doc.General.Checks[0].Remediation)
}

func TestDoctorDisplayEndpoint(t *testing.T) {
	tests := map[string]string{
		"https://hub.example.com":                                    "https://hub.example.com",
		"https://hub.example.com:8443/scion/":                        "https://hub.example.com:8443/scion/",
		"https://alice:pa55word@hub.example.com/p?token=s3cr3t#frag": "https://hub.example.com/p",
		"https://alice@hub.example.com?access_token=s3cr3t":          "https://hub.example.com",
	}
	for in, want := range tests {
		assert.Equal(t, want, doctorDisplayEndpoint(in), in)
	}
}

// credentialedEndpoint returns base with user info, a path, a token
// query parameter and a fragment.
func credentialedEndpoint(base string) string {
	return strings.Replace(base, "http://", "http://alice:pa55word@", 1) + "/base?token=s3cr3t-tok#frag"
}

func assertNoEndpointCredentials(t *testing.T, out string) {
	t.Helper()
	for _, secret := range []string{"alice", "pa55word", "s3cr3t-tok", "token=", "#frag", "frag/"} {
		assert.NotContains(t, out, secret)
	}
}

func TestDoctorHubEndpointCredentialsNeverShown(t *testing.T) {
	t.Run("reachable", func(t *testing.T) {
		srv := (&fakeDoctorHub{summaryBody: summaryJSON(t, degradedSummary())}).serve(t)
		ep := credentialedEndpoint(srv.URL)
		// The client talks to the fake hub; doctor is given the
		// credentialed endpoint to display.
		client := newDoctorTestClient(t, srv.URL)

		var text bytes.Buffer
		r := &doctorReport{stdout: &text, authSource: hubAuthSourceEnvHubToken}
		r.platformHealthSection(r.hubChecks(ep, client, ""))
		assert.Contains(t, text.String(), "Hub at "+srv.URL+"/base is healthy")
		assertNoEndpointCredentials(t, text.String())

		out, doc := runDoctorJSON(t, ep, client, hubAuthSourceEnvHubToken)
		assertNoEndpointCredentials(t, string(out))
		assert.JSONEq(t, `"`+srv.URL+`/base"`, string(hubObject(t, doc)["endpoint"]))
	})

	t.Run("unreachable", func(t *testing.T) {
		// The client's error quotes the request URL built from the
		// credentialed endpoint.
		ep := credentialedEndpoint(closedServerURL())
		client := newDoctorTestClient(t, ep)

		var text bytes.Buffer
		r := &doctorReport{stdout: &text}
		r.platformHealthSection(r.hubChecks(ep, client, ""))
		assert.Contains(t, text.String(), "Cannot reach Hub at ")
		assertNoEndpointCredentials(t, text.String())

		out, _ := runDoctorJSON(t, ep, client, "")
		assertNoEndpointCredentials(t, string(out))
	})
}

func TestScrubDoctorHubURLs(t *testing.T) {
	ep := "http://alice:pw@hub.example.com:8080/base?token=t#f"
	in := `request failed: Get "http://alice:xxxxx@hub.example.com:8080/base?token=xxxxx#f/healthz": refused; see https://docs.example.com/x?y=1#z.`
	assert.Equal(t, `request failed: Get "http://hub.example.com:8080/base": refused; see https://docs.example.com/x?y=1#z.`, scrubDoctorHubURLs(in, ep))
	assert.Equal(t, in, scrubDoctorHubURLs(in, ""))
}

func TestDoctorHubHealth_CredentialLine(t *testing.T) {
	srv := (&fakeDoctorHub{summaryBody: summaryJSON(t, degradedSummary())}).serve(t)
	var text bytes.Buffer
	r := &doctorReport{stdout: &text, authSource: hubAuthSourceAgentTokenFile}
	r.platformHealthSection(r.hubChecks(srv.URL, newDoctorTestClient(t, srv.URL), ""))
	c := findCheck(t, r.HubChecks, "hub-credential")
	assert.Equal(t, "info", c.Status)
	assert.Contains(t, text.String(), "i hub-credential: Using the agent token file; it takes precedence over SCION_HUB_TOKEN")
	// It follows the authentication check.
	names := checkNames(r.HubChecks)
	require.GreaterOrEqual(t, len(names), 3)
	assert.Equal(t, []string{"hub-connectivity", "hub-auth", "hub-credential"}, names[:3])

	// No line without a Hub.
	r2 := &doctorReport{stdout: io.Discard, authSource: hubAuthSourceDev}
	r2.platformHealthSection(r2.hubChecks("", nil, ""))
	assert.NotContains(t, checkNames(r2.HubChecks), "hub-credential")
}
