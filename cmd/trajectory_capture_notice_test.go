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
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
	"github.com/GoogleCloudPlatform/scion/pkg/wsclient"
)

func captureNoticeStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stderr = w
	defer func() { os.Stderr = orig }()

	fn()

	require.NoError(t, w.Close())
	var buf bytes.Buffer
	_, err = io.Copy(&buf, r)
	require.NoError(t, err)
	require.NoError(t, r.Close())
	return buf.String()
}

func TestAttachViaHub_PrintsTrajectoryCaptureNoticeBeforeAttach(t *testing.T) {
	cases := []struct {
		name       string
		status     *telemetrycontract.TrajectoryCapturePolicyStatus
		wantNotice string
	}{
		{
			name:       "experiment off omits notice",
			status:     nil,
			wantNotice: "",
		},
		{
			name: "full policy",
			status: &telemetrycontract.TrajectoryCapturePolicyStatus{
				Policy:        telemetrycontract.TrajectoryCapturePolicyFull,
				EnforcedByHub: false,
			},
			wantNotice: telemetrycontract.TrajectoryCaptureNotice(telemetrycontract.TrajectoryCapturePolicyStatus{
				Policy:        telemetrycontract.TrajectoryCapturePolicyFull,
				EnforcedByHub: false,
			}),
		},
		{
			name: "full policy enforced by hub",
			status: &telemetrycontract.TrajectoryCapturePolicyStatus{
				Policy:        telemetrycontract.TrajectoryCapturePolicyFull,
				EnforcedByHub: true,
			},
			wantNotice: telemetrycontract.TrajectoryCaptureNotice(telemetrycontract.TrajectoryCapturePolicyStatus{
				Policy:        telemetrycontract.TrajectoryCapturePolicyFull,
				EnforcedByHub: true,
			}),
		},
		{
			name: "metadata_only policy",
			status: &telemetrycontract.TrajectoryCapturePolicyStatus{
				Policy:        telemetrycontract.TrajectoryCapturePolicyMetadataOnly,
				EnforcedByHub: false,
			},
			wantNotice: telemetrycontract.TrajectoryCaptureNotice(telemetrycontract.TrajectoryCapturePolicyStatus{
				Policy:        telemetrycontract.TrajectoryCapturePolicyMetadataOnly,
				EnforcedByHub: false,
			}),
		},
		{
			name: "off policy",
			status: &telemetrycontract.TrajectoryCapturePolicyStatus{
				Policy:        telemetrycontract.TrajectoryCapturePolicyOff,
				EnforcedByHub: false,
			},
			wantNotice: telemetrycontract.TrajectoryCaptureNotice(telemetrycontract.TrajectoryCapturePolicyStatus{
				Policy:        telemetrycontract.TrajectoryCapturePolicyOff,
				EnforcedByHub: false,
			}),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			origAttachFn := attachToAgentFn
			origTransportFn := resolveAttachTransportFn
			t.Cleanup(func() {
				attachToAgentFn = origAttachFn
				resolveAttachTransportFn = origTransportFn
			})

			resolveAttachTransportFn = func() (transportauth.TokenSource, transportauth.HeaderMode, error) {
				return &fakeTransportSource{token: "test-transport-token"}, transportauth.HeaderAuthorization, nil
			}
			attachCalled := false
			attachToAgentFn = func(_ context.Context, _, _, _ string, _ ...wsclient.AttachOption) error {
				attachCalled = true
				return nil
			}

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(hubclient.Agent{
					ID:                "agent-id-1",
					Name:              "a1",
					Slug:              "a1",
					Phase:             "running",
					Status:            "running",
					RuntimeBrokerID:   "broker-1",
					TrajectoryCapture: tc.status,
				})
			}))
			defer srv.Close()

			client, err := hubclient.New(srv.URL)
			require.NoError(t, err)
			hubCtx := &HubContext{
				Client:    client,
				Endpoint:  srv.URL,
				ProjectID: "proj-1",
			}

			var runErr error
			stderr := captureNoticeStderr(t, func() {
				runErr = attachViaHub(hubCtx, "a1")
			})
			require.NoError(t, runErr)
			assert.True(t, attachCalled, "attachToAgentFn should be called")
			if tc.wantNotice == "" {
				assert.NotContains(t, stderr, "Notice:")
			} else {
				assert.Contains(t, stderr, "Notice: "+tc.wantNotice)
			}
		})
	}
}

func TestFinishHubStart_PrintsTrajectoryCaptureNotice(t *testing.T) {
	origAttach := attach
	origNoWait := startNoWait
	origWaitReady := startWaitReady
	origOutputFormat := outputFormat
	origAttachFn := attachToAgentFn
	origTransportFn := resolveAttachTransportFn
	t.Cleanup(func() {
		attach = origAttach
		startNoWait = origNoWait
		startWaitReady = origWaitReady
		outputFormat = origOutputFormat
		attachToAgentFn = origAttachFn
		resolveAttachTransportFn = origTransportFn
	})

	startNoWait = false
	startWaitReady = false
	outputFormat = ""
	resolveAttachTransportFn = func() (transportauth.TokenSource, transportauth.HeaderMode, error) {
		return &fakeTransportSource{token: "test-transport-token"}, transportauth.HeaderAuthorization, nil
	}

	status := &telemetrycontract.TrajectoryCapturePolicyStatus{
		Policy:        telemetrycontract.TrajectoryCapturePolicyFull,
		EnforcedByHub: true,
	}
	wantNotice := telemetrycontract.TrajectoryCaptureNotice(*status)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/projects/proj-1/agents/a1" {
			_ = json.NewEncoder(w).Encode(hubclient.Agent{
				ID:                "agent-id-1",
				Name:              "a1",
				Slug:              "a1",
				Phase:             "running",
				Status:            "running",
				RuntimeBrokerID:   "broker-1",
				TrajectoryCapture: status,
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  srv.URL,
		ProjectID: "proj-1",
	}

	// Verify notice is printed before attachToAgentFn runs when --attach is set.
	attach = true
	attached := false
	attachToAgentFn = func(_ context.Context, _, _, _ string, _ ...wsclient.AttachOption) error {
		attached = true
		return nil
	}

	var runErr error
	stderr := captureNoticeStderr(t, func() {
		runErr = finishHubStart(hubCtx, "proj-1", "a1", false, false, &hubclient.CreateAgentResponse{
			Agent: &hubclient.Agent{
				ID:     "agent-id-1",
				Name:   "a1",
				Slug:   "a1",
				Phase:  "running",
				Status: "running",
			},
		}, nil, false)
	})
	require.NoError(t, runErr)
	assert.True(t, attached)
	assert.Contains(t, stderr, "Notice: "+wantNotice)
}
