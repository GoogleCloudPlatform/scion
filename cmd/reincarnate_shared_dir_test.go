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
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSharedDirBackendFlags(t *testing.T) {
	got, err := parseSharedDirBackendFlags([]string{"notes=nfs", "build-cache=nfs"}, true)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"notes": "nfs", "build-cache": "nfs"}, got)

	got, err = parseSharedDirBackendFlags(nil, false)
	require.NoError(t, err)
	assert.Nil(t, got)

	for _, tc := range []struct {
		values     []string
		allowEmpty bool
		want       string
	}{
		{nil, true, "--allow-empty-shared-dir needs --shared-dir-backend"},
		{[]string{"notes"}, false, "want NAME=nfs"},
		{[]string{"=nfs"}, false, "want NAME=nfs"},
		{[]string{"notes=local"}, false, "only nfs is supported"},
		{[]string{"Notes=nfs"}, false, "invalid shared dir name"},
		{[]string{"notes=nfs", "notes=nfs"}, false, "more than once"},
	} {
		_, err := parseSharedDirBackendFlags(tc.values, tc.allowEmpty)
		require.Error(t, err, "%v", tc.values)
		assert.Contains(t, err.Error(), tc.want)
	}
}

func TestReincarnateCmd_SharedDirFlagsRegistered(t *testing.T) {
	f := reincarnateCmd.Flags().Lookup("shared-dir-backend")
	require.NotNil(t, f)
	assert.Equal(t, "stringArray", f.Value.Type())
	require.NotNil(t, reincarnateCmd.Flags().Lookup("allow-empty-shared-dir"))
}

// sharedDirTestHub is a fake hub that records reincarnate request bodies.
// plan returns the plan JSON for a request body; nil echoes the request's
// sharedDirBackends and allowEmptySharedDir, as a current hub does.
func sharedDirTestHub(t *testing.T, plan func(body map[string]interface{}) string) (*HubContext, *[]map[string]interface{}) {
	t.Helper()
	if plan == nil {
		plan = echoSharedDirPlan
	}
	var bodies []map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/reincarnate") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		data, _ := io.ReadAll(r.Body)
		var body map[string]interface{}
		_ = json.Unmarshal(data, &body)
		bodies = append(bodies, body)
		state := "pending"
		if dry, _ := body["dryRun"].(bool); dry {
			state = "planned"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"agentId":"agent-1","generation":2,"state":"` + state + `","plan":` + plan(body) + `}`))
	}))
	t.Cleanup(srv.Close)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	return &HubContext{Client: client, Endpoint: srv.URL, ProjectID: "proj-1"}, &bodies
}

// echoSharedDirPlan is the plan of a hub that supports shared dir backend
// changes: it echoes them.
func echoSharedDirPlan(body map[string]interface{}) string {
	plan := map[string]interface{}{}
	if v, ok := body["sharedDirBackends"]; ok {
		plan["sharedDirBackends"] = v
	}
	if v, ok := body["allowEmptySharedDir"]; ok {
		plan["allowEmptySharedDir"] = v
	}
	data, _ := json.Marshal(plan)
	return string(data)
}

// oldHubPlan is the plan of a hub that predates shared dir backend changes.
func oldHubPlan(map[string]interface{}) string { return `{}` }

func setSharedDirFlags(t *testing.T, values []string, allowEmpty, dryRun bool) {
	t.Helper()
	prevSD, prevAllow, prevDry, prevBroker := reincarnateSharedDirs, reincarnateAllowEmptySD, reincarnateDryRun, reincarnateBroker
	t.Cleanup(func() {
		reincarnateSharedDirs, reincarnateAllowEmptySD, reincarnateDryRun, reincarnateBroker = prevSD, prevAllow, prevDry, prevBroker
	})
	reincarnateSharedDirs, reincarnateAllowEmptySD, reincarnateDryRun, reincarnateBroker = values, allowEmpty, dryRun, ""
}

// The flags reach the hub as sharedDirBackends and allowEmptySharedDir.
func TestReincarnateAgentViaHub_SendsSharedDirBackends(t *testing.T) {
	hubCtx, bodies := sharedDirTestHub(t, nil)
	setSharedDirFlags(t, []string{"notes=nfs"}, true, true)
	require.NoError(t, reincarnateAgentViaHub(hubCtx, "agent-1", "", false))
	require.Len(t, *bodies, 1)
	body := (*bodies)[0]
	assert.Equal(t, map[string]interface{}{"notes": "nfs"}, body["sharedDirBackends"])
	assert.Equal(t, true, body["allowEmptySharedDir"])
	assert.Equal(t, true, body["dryRun"])
}

// Without the flags the request carries neither field.
func TestReincarnateAgentViaHub_NoSharedDirBackendsByDefault(t *testing.T) {
	hubCtx, bodies := sharedDirTestHub(t, nil)
	setSharedDirFlags(t, nil, false, true)
	require.NoError(t, reincarnateAgentViaHub(hubCtx, "agent-1", "", false))
	require.Len(t, *bodies, 1)
	_, has := (*bodies)[0]["sharedDirBackends"]
	assert.False(t, has)
	_, has = (*bodies)[0]["allowEmptySharedDir"]
	assert.False(t, has)
}

// An agent changing its own shared dir backend, or a malformed flag, is
// refused before any hub request.
func TestReincarnateAgentViaHub_SharedDirRefusalsBeforeHub(t *testing.T) {
	hubCtx, bodies := sharedDirTestHub(t, nil)

	setSharedDirFlags(t, []string{"notes=nfs"}, false, false)
	err := reincarnateAgentViaHub(hubCtx, "agent-1", "handoff", true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot change its own shared dir backend")

	setSharedDirFlags(t, []string{"notes=local"}, false, true)
	err = reincarnateAgentViaHub(hubCtx, "agent-1", "", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "only nfs is supported")

	setSharedDirFlags(t, nil, true, true)
	err = reincarnateAgentViaHub(hubCtx, "agent-1", "", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--allow-empty-shared-dir needs --shared-dir-backend")

	assert.Empty(t, *bodies, "the hub must receive no request")

	// A plain self-reincarnation without the flags still reaches the hub.
	setSharedDirFlags(t, nil, false, true)
	require.NoError(t, reincarnateAgentViaHub(hubCtx, "agent-1", "", true))
	assert.Len(t, *bodies, 1)
}

// A real request with a shared dir change is preceded by a dry-run probe;
// the hub echoes the change, so the real request follows.
func TestReincarnateAgentViaHub_SharedDirProbeEchoed(t *testing.T) {
	hubCtx, bodies := sharedDirTestHub(t, nil)
	setSharedDirFlags(t, []string{"notes=nfs"}, false, false)
	require.NoError(t, reincarnateAgentViaHub(hubCtx, "agent-1", "handoff", false))
	require.Len(t, *bodies, 2, "probe, then the real request")
	assert.Equal(t, true, (*bodies)[0]["dryRun"])
	assert.Equal(t, map[string]interface{}{"notes": "nfs"}, (*bodies)[0]["sharedDirBackends"])
	_, dry := (*bodies)[1]["dryRun"]
	assert.False(t, dry, "the second request is the real one")
	assert.Equal(t, map[string]interface{}{"notes": "nfs"}, (*bodies)[1]["sharedDirBackends"])
}

// A hub that does not echo the change stops the CLI after the probe, so no
// plain reincarnation runs.
func TestReincarnateAgentViaHub_SharedDirProbeNotEchoed(t *testing.T) {
	hubCtx, bodies := sharedDirTestHub(t, oldHubPlan)
	setSharedDirFlags(t, []string{"notes=nfs"}, false, false)
	err := reincarnateAgentViaHub(hubCtx, "agent-1", "handoff", false)
	require.Error(t, err)
	assert.Equal(t, "this hub does not support --shared-dir-backend; upgrade the hub", err.Error())
	require.Len(t, *bodies, 1, "only the dry-run probe reaches the hub")
	assert.Equal(t, true, (*bodies)[0]["dryRun"])
}

// A dry run against a hub that does not echo the change fails instead of
// printing a plan without it.
func TestReincarnateAgentViaHub_SharedDirDryRunNotEchoed(t *testing.T) {
	hubCtx, bodies := sharedDirTestHub(t, oldHubPlan)
	setSharedDirFlags(t, []string{"notes=nfs"}, true, true)
	err := reincarnateAgentViaHub(hubCtx, "agent-1", "", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not support --shared-dir-backend")
	assert.Len(t, *bodies, 1)
}

// An echo that differs (here allowEmptySharedDir dropped) is not accepted.
func TestReincarnateAgentViaHub_SharedDirPartialEchoRejected(t *testing.T) {
	hubCtx, _ := sharedDirTestHub(t, func(map[string]interface{}) string {
		return `{"sharedDirBackends":{"notes":"nfs"}}`
	})
	setSharedDirFlags(t, []string{"notes=nfs"}, true, true)
	err := reincarnateAgentViaHub(hubCtx, "agent-1", "", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not support --shared-dir-backend")
}

// The probe passes but the hub that takes the real request ignores the
// change: the error says the reincarnation started without it.
func TestReincarnateAgentViaHub_SharedDirRealRequestNotEchoed(t *testing.T) {
	hubCtx, bodies := sharedDirTestHub(t, func(body map[string]interface{}) string {
		if dry, _ := body["dryRun"].(bool); dry {
			return echoSharedDirPlan(body)
		}
		return `{}`
	})
	setSharedDirFlags(t, []string{"notes=nfs"}, false, false)
	err := reincarnateAgentViaHub(hubCtx, "agent-1", "handoff", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "started without the shared dir backend change")
	assert.Len(t, *bodies, 2)
}
