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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// `scion project service-accounts show` and the MAPPED list column
// (ptone/scion#4018). The Hub is an httptest server.

const saStatusCLIBody = `{
  "account":{"id":"sa-1","displayName":"Worker","scope":"project","email":"worker@example.com"},
  "verification":{"status":"verified","verified":true,"verifiedAt":"2026-10-10T10:00:00Z"},
  "mappings":[
    {"brokerId":"b1","brokerName":"b","profile":"gke","state":"mapped"},
    {"brokerId":"b1","brokerName":"b","profile":"gke-2","state":"not_mapped"},
    {"brokerId":"b1","brokerName":"b","profile":"gke-3","state":"not_reported"}
  ],
  "workloadIdentityBinding":{"state":"unknown","reason":"not checked"},
  "defaultFor":[{"kind":"project"},{"kind":"profile","profile":"gke-2"},{"kind":"hub"}],
  "agents":{"count":3,"names":["a1","a2"]},
  "nextStep":{"code":"not_mapped","brokerName":"b","profile":"gke-2","message":"No Kubernetes service account mapping on profile gke-2 of broker b."}
}`

// saPathHub is saCLIHub that records the request path instead of the query.
func saPathHub(t *testing.T, body string) *string {
	t.Helper()
	seen := new(string)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	tmpHome := t.TempDir()
	_ = os.Setenv("HOME", tmpHome)
	_ = os.Unsetenv("SCION_HUB_ENDPOINT")
	noHub = false
	projectDir := filepath.Join(tmpHome, "project", ".scion")
	require.NoError(t, os.MkdirAll(projectDir, 0755))
	data, err := json.Marshal(map[string]interface{}{
		"project_id": "proj-local",
		"hub":        map[string]interface{}{"enabled": true, "endpoint": srv.URL, "projectId": "scion-proj-1"},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "settings.json"), data, 0644))
	projectPath = projectDir
	return seen
}

func TestPrintSAStatus_AllSections(t *testing.T) {
	restore := clitime.SetNow(func() time.Time { return time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC) })
	defer restore()

	var st hubclient.GCPServiceAccountStatus
	require.NoError(t, json.Unmarshal([]byte(saStatusCLIBody), &st))
	var buf bytes.Buffer
	printSAStatus(&buf, &st)
	out := buf.String()

	for _, want := range []string{
		"Service account: Worker",
		"Email:  worker@example.com",
		"Scope:  project",
		"Status: verified (checked 2h ago)",
		"b/gke                           mapped",
		"b/gke-2                         not mapped",
		"b/gke-3                         not reported",
		"Workload Identity binding:\n  unknown (not checked)",
		"  project default\n  profile gke-2\n  hub default",
		"Agents using it (3):\n  a1\n  a2\n  ... and 1 more",
		"Next step:\n  No Kubernetes service account mapping on profile gke-2 of broker b.",
	} {
		assert.Contains(t, out, want)
	}
}

func TestPrintSAStatus_EmptySections(t *testing.T) {
	st := &hubclient.GCPServiceAccountStatus{}
	st.Account.Email = "worker@example.com"
	st.Verification.Status = "failed"
	st.Verification.Error = "permission denied"
	st.WorkloadIdentityBinding.State = "unknown"
	st.NextStep.Message = "Not verified."
	var buf bytes.Buffer
	printSAStatus(&buf, st)
	out := buf.String()

	assert.Contains(t, out, "Service account: worker@example.com", "falls back to the email without a name")
	assert.Contains(t, out, "Status: failed\n  Error:  permission denied")
	assert.Contains(t, out, "no Kubernetes broker profiles in this project")
	assert.Contains(t, out, "Default for:\n  none")
	assert.Contains(t, out, "Agents using it (0):\n  none")
	assert.NotContains(t, out, "bound (", "never claims a binding")
}

func TestProjectSAShow_AsksForTheStatusOfTheReference(t *testing.T) {
	orig := saveSACLIState()
	defer orig.restore()

	seen := saPathHub(t, saStatusCLIBody)
	saOutputJSON = false

	out := captureStdout(t, func() {
		require.NoError(t, runSAShow(nil, []string{"Worker SA"}))
	})
	assert.Equal(t, "/api/v1/projects/scion-proj-1/gcp-service-accounts/Worker%20SA/status", *seen,
		"the reference is resolved by the Hub in the linked Scion project")
	assert.Contains(t, out, "Next step:")
}

func TestProjectSAList_MappedColumn(t *testing.T) {
	orig := saveSACLIState()
	defer orig.restore()

	saCLIHub(t, `{"items":[
	  {"id":"sa-1","scope":"project","email":"one@example.com","verified":true,
	   "mapping":{"mappedProfiles":1,"reportedProfiles":2,"unreportedProfiles":0}},
	  {"id":"sa-2","scope":"project","email":"two@example.com","verified":false,
	   "mapping":{"mappedProfiles":0,"reportedProfiles":0,"unreportedProfiles":1}},
	  {"id":"sa-3","scope":"project","email":"three@example.com","verified":false}
	]}`)
	saOutputJSON = false

	out := captureStdout(t, func() {
		require.NoError(t, runSAList(nil, nil))
	})
	lines := strings.Split(out, "\n")
	require.GreaterOrEqual(t, len(lines), 5, out)
	assert.True(t, strings.HasSuffix(strings.TrimSpace(lines[1]), "MAPPED"), lines[1])
	assert.True(t, strings.HasSuffix(strings.TrimSpace(lines[3]), "yes       1/2"), lines[3])
	assert.True(t, strings.HasSuffix(strings.TrimSpace(lines[4]), "no        -"), lines[4])
	assert.True(t, strings.HasSuffix(strings.TrimSpace(lines[5]), "no        -"), lines[5])
}
