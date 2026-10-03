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
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReincarnateMove_VerdictFromRefusal round-trips a hub move refusal
// through the hub client: the request carries targetBroker, and the CLI
// recovers and prints the verdict from the error details.
func TestReincarnateMove_VerdictFromRefusal(t *testing.T) {
	var gotReq hubclient.ReincarnateAgentRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/reincarnate") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPreconditionFailed)
		_, _ = w.Write([]byte(`{"error":{"code":"unsupported_capability","message":"broker b2 does not support agent move; upgrade it",
			"details":{"verdict":{"eligible":false,
				"sourceBroker":{"id":"id-1","name":"b1"},"targetBroker":{"id":"id-2","name":"b2"},
				"profile":"k8s","runtimeType":"kubernetes",
				"checks":[{"name":"workspace_mode","result":"passed"},
					{"name":"capability","result":"failed","message":"broker b2 does not support agent move; upgrade it"},
					{"name":"capacity","result":"not_evaluated"}]}}}}`))
	}))
	t.Cleanup(srv.Close)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	_, err = client.ProjectAgents("proj-1").Reincarnate(context.Background(), "agent-1",
		&hubclient.ReincarnateAgentRequest{DryRun: true, TargetBroker: "b2"})
	require.Error(t, err)
	assert.Equal(t, "b2", gotReq.TargetBroker)
	assert.True(t, gotReq.DryRun)

	v := moveVerdictFromError(err)
	require.NotNil(t, v)
	assert.Equal(t, "id-2", v.TargetBroker.ID)
	require.Len(t, v.Checks, 3)

	var out bytes.Buffer
	printMoveVerdict(&out, v)
	text := out.String()
	assert.Contains(t, text, "Move eligibility: b1 (id-1) -> b2 (id-2)")
	assert.Contains(t, text, "k8s (kubernetes)")
	assert.Contains(t, text, "capability:")
	assert.Contains(t, text, "failed - broker b2 does not support agent move")
	assert.Contains(t, text, "not_evaluated")
}

func TestMoveVerdictFromError_NoVerdict(t *testing.T) {
	assert.Nil(t, moveVerdictFromError(errors.New("plain")))
	assert.Nil(t, moveVerdictFromError(nil))
}
