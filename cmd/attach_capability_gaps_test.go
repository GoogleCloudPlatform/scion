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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newBrokerGetHub serves GET /api/v1/runtime-brokers/{brokerID} with the
// given raw JSON body and status.
func newBrokerGetHub(t *testing.T, brokerID string, status int, body string) *HubContext {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/runtime-brokers/"+brokerID {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	return &HubContext{Client: client, Endpoint: srv.URL}
}

// TestAttachSupportedByBroker_Branches covers the attachSupportedByBroker
// branches not covered elsewhere in this package. Every body is raw JSON
// (the wire shape a real Hub returns), so the *bool decode is exercised
// too.
func TestAttachSupportedByBroker_Branches(t *testing.T) {
	const id = "b1"
	tests := []struct {
		name           string
		status         int
		body           string
		profile        string
		want           bool
		wantUnreadable bool
	}{
		{
			// A live broker instance always resolves its own default-type
			// profile's Attach explicitly (pkg/runtimebroker's resolver), so
			// the profile's own answer wins over the broker-wide fallback
			// when both are present.
			name:    "profile attach true wins over broker-wide false",
			status:  http.StatusOK,
			body:    `{"id":"b1","profiles":[{"name":"p","type":"docker","attach":true}],"capabilities":{"attach":false}}`,
			profile: "p",
			want:    true,
		},
		{
			name:    "profile not in list falls back to broker-wide false",
			status:  http.StatusOK,
			body:    `{"id":"b1","profiles":[{"name":"other","type":"docker","attach":true}],"capabilities":{"attach":false}}`,
			profile: "missing",
			want:    false,
		},
		{
			name:    "no profile falls back to broker-wide true",
			status:  http.StatusOK,
			body:    `{"id":"b1","capabilities":{"attach":true}}`,
			profile: "",
			want:    true,
		},
		{
			name:    "no profile match and no capabilities defaults true",
			status:  http.StatusOK,
			body:    `{"id":"b1","profiles":[{"name":"other","type":"docker","attach":false}]}`,
			profile: "missing",
			want:    true,
		},
		{
			// nil profile Attach means "this profile said nothing" and
			// falls through to the broker-wide answer, which is final: an
			// explicit broker-wide false is real information (e.g. the
			// broker's registration producer had no live instance to ask
			// for this specific profile, but its default runtime, reported
			// via Capabilities.Attach, does not support attach), so it is
			// not overridden back to "supported" just because this one
			// profile had nothing to say for itself.
			name:    "profile present with attach key absent falls through to broker-wide false",
			status:  http.StatusOK,
			body:    `{"id":"b1","profiles":[{"name":"p","type":"docker"}],"capabilities":{"attach":false}}`,
			profile: "p",
			want:    false,
		},
		{
			// The point-GET fails and this fixture serves no LIST endpoint at
			// all (a 404 default), so the LIST fallback also can't produce
			// the record: this is the "record unreadable" case — supported
			// comes back false but unreadable=true is the signal callers
			// must check first, since attachUnsupportedErr treats an
			// unreadable record as unknown and proceeds to dial rather than
			// refusing on this false. The LIST-succeeds-as-fallback and
			// LIST-also-fails cases against a live attachViaHub call are
			// covered in attach_test.go's TestAttachViaHub_PointGETForbidden_*
			// tests.
			name:           "hub error reading broker and list fallback reports unreadable",
			status:         http.StatusForbidden,
			body:           `{"error":{"code":"forbidden","message":"no"}}`,
			profile:        "p",
			want:           false,
			wantUnreadable: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hubCtx := newBrokerGetHub(t, id, tc.status, tc.body)
			got, unreadable := attachSupportedByBroker(context.Background(), hubCtx, id, tc.profile)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.wantUnreadable, unreadable)
		})
	}
}

// TestAttachViaHub_AgentProfileAttachFalse_ReturnsExplicitError pins the
// end-to-end per-profile wiring: the agent record's AppliedConfig.Profile
// (agentProfileName) must select the broker profile whose Attach=false,
// even though the broker-wide capability says attach is supported.
func TestAttachViaHub_AgentProfileAttachFalse_ReturnsExplicitError(t *testing.T) {
	clearAppTokenSources(t)
	const (
		projectID = "proj-prof"
		agentName = "prof-agent"
		brokerID  = "broker-prof"
	)
	falseVal := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/projects/" + projectID + "/agents/" + agentName:
			_ = json.NewEncoder(w).Encode(hubclient.Agent{
				ID: "a1", Name: agentName, Phase: "running", Runtime: "optout",
				RuntimeBrokerID: brokerID,
				AppliedConfig:   &hubclient.AgentConfig{Profile: "optout-prof"},
			})
		case "/api/v1/runtime-brokers/" + brokerID:
			_ = json.NewEncoder(w).Encode(hubclient.RuntimeBroker{
				ID:           brokerID,
				Profiles:     []hubclient.BrokerProfile{{Name: "optout-prof", Type: "optout", Attach: &falseVal}},
				Capabilities: &hubclient.BrokerCapabilities{Attach: true},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	err = attachViaHub(&HubContext{Client: client, Endpoint: srv.URL, ProjectID: projectID}, agentName)
	require.Error(t, err)
	assert.Equal(t, "attach is not supported for agents on the optout runtime", err.Error())
}

// TestAttachViaHub_EmptyAgentRuntime_ProfileAttachFalse_ReturnsRuntimeAgnosticMessage
// covers an agent record whose Runtime field is empty (a real value along
// the scion start -a / scion resume -a polling path before the Hub has
// reported one — see attachUnsupportedErr's own comment). With nothing to
// name, the rejection message must stand on its own rather than templating
// an empty runtime name into the wording used when one is known.
func TestAttachViaHub_EmptyAgentRuntime_ProfileAttachFalse_ReturnsRuntimeAgnosticMessage(t *testing.T) {
	clearAppTokenSources(t)
	const (
		projectID = "proj-empty-runtime"
		agentName = "empty-runtime-agent"
		brokerID  = "broker-empty-runtime"
	)
	falseVal := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/projects/" + projectID + "/agents/" + agentName:
			_ = json.NewEncoder(w).Encode(hubclient.Agent{
				ID: "a1", Name: agentName, Phase: "running", Runtime: "",
				RuntimeBrokerID: brokerID,
				AppliedConfig:   &hubclient.AgentConfig{Profile: "empty-runtime-prof"},
			})
		case "/api/v1/runtime-brokers/" + brokerID:
			_ = json.NewEncoder(w).Encode(hubclient.RuntimeBroker{
				ID:           brokerID,
				Profiles:     []hubclient.BrokerProfile{{Name: "empty-runtime-prof", Type: "optout", Attach: &falseVal}},
				Capabilities: &hubclient.BrokerCapabilities{Attach: true},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	err = attachViaHub(&HubContext{Client: client, Endpoint: srv.URL, ProjectID: projectID}, agentName)
	require.Error(t, err)
	assert.Equal(t, "attach is not supported for this agent's runtime", err.Error())
}

// TestAttachViaHub_StandaloneBrokerNilProfileAttach_BrokerWideFalse_Refuses
// covers a standalone broker: a remote broker registered via `scion broker
// register` (buildBrokerProfiles, cmd/broker.go) has no live runtime
// instance to ask at registration time, so every profile it reports carries
// Attach=nil — proven here by actually calling buildBrokerProfiles, not by
// hand-building a profile literal. Once that broker is running, its
// heartbeat reports the real broker-wide Capabilities.Attach from its own
// default runtime (HeartbeatService.buildHeartbeat). A profile match with no
// Attach of its own must fall through to that broker-wide answer rather than
// defaulting to supported — otherwise a broker whose default runtime has no
// attach primitive would still let a caller dial in on the one deployment
// shape (a remote/standalone broker) with no live instance to ask
// directly: a profile silent on attach refuses, never assumes supported.
func TestAttachViaHub_StandaloneBrokerNilProfileAttach_BrokerWideFalse_Refuses(t *testing.T) {
	clearAppTokenSources(t)
	const (
		projectID = "proj-standalone"
		agentName = "noattach-agent"
		brokerID  = "standalone-broker"
		profile   = "noattach"
	)

	settings := &config.Settings{
		Profiles: map[string]config.ProfileConfig{
			profile: {Runtime: "noattach"},
		},
	}
	profiles := buildBrokerProfiles(settings)
	require.Len(t, profiles, 1)
	require.Nil(t, profiles[0].Attach, "a standalone broker's registration producer has no live instance to set this")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/projects/" + projectID + "/agents/" + agentName:
			_ = json.NewEncoder(w).Encode(hubclient.Agent{
				ID: "a1", Name: agentName, Phase: "running", Runtime: "noattach",
				RuntimeBrokerID: brokerID,
				AppliedConfig:   &hubclient.AgentConfig{Profile: profile},
			})
		case "/api/v1/runtime-brokers/" + brokerID:
			_ = json.NewEncoder(w).Encode(hubclient.RuntimeBroker{
				ID:       brokerID,
				Profiles: profiles,
				// What a real heartbeat reports once this broker is
				// running with a no-attach runtime as its default (see
				// HeartbeatService.buildHeartbeat).
				Capabilities: &hubclient.BrokerCapabilities{Attach: false},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	err = attachViaHub(&HubContext{Client: client, Endpoint: srv.URL, ProjectID: projectID}, agentName)
	require.Error(t, err)
	assert.Equal(t, "attach is not supported for agents on the noattach runtime", err.Error())
}
