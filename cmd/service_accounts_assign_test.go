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
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// ASSIGN column on the service account lists (ptone/scion#3329 phase 4c).

const saAssignBody = `{"items":[
 {"id":"sa-mapped","scope":"project","scopeId":"scion-proj-1","email":"mapped@x.iam.gserviceaccount.com","projectId":"gcp","verified":true,
  "assignStatus":{"state":"mapped","message":"m","brokerId":"b1","brokerName":"broker-a","profile":"gke","namespace":"agents"}},
 {"id":"sa-unmapped","scope":"project","scopeId":"scion-proj-1","email":"unmapped@x.iam.gserviceaccount.com","projectId":"gcp","verified":true,
  "assignStatus":{"state":"not_mapped","message":"m","brokerId":"b1","brokerName":"broker-a","profile":"gke"}},
 {"id":"sa-unknown","scope":"hub","scopeId":"hub","email":"shared@x.iam.gserviceaccount.com","projectId":"gcp","verified":true,
  "assignStatus":{"state":"unknown","reason":"report_stale","message":"m","brokerId":"b1","brokerName":"broker-a","profile":"gke"}}
]}`

func withSAAssignFlags(t *testing.T, profile, broker string) {
	t.Helper()
	oldP, oldB := saListProfile, saListBroker
	saListProfile, saListBroker = profile, broker
	t.Cleanup(func() { saListProfile, saListBroker = oldP, oldB })
}

func TestSAScopedList_AssignColumn(t *testing.T) {
	orig := saveSACLIState()
	defer orig.restore()

	seen := saCLIHub(t, saAssignBody)
	globalMode = false
	saOutputJSON = false
	saGlobalListAssignable = true
	withSAAssignFlags(t, "gke", "broker-a")

	out := captureStdout(t, func() { require.NoError(t, runSAScopedList(nil, nil)) })

	require.Equal(t, "true", seen.Get("assignStatus"))
	require.Equal(t, "gke", seen.Get("profile"))
	require.Equal(t, "broker-a", seen.Get("broker"))

	assert.Contains(t, out, "ASSIGN")
	lines := strings.Split(out, "\n")
	row := func(id string) string {
		for _, l := range lines {
			if strings.HasPrefix(l, id+" ") {
				return l
			}
		}
		t.Fatalf("no row for %s in:\n%s", id, out)
		return ""
	}
	assert.True(t, strings.HasSuffix(row("sa-mapped"), "  mapped"), row("sa-mapped"))
	assert.True(t, strings.HasSuffix(row("sa-unmapped"), "  not mapped"), row("sa-unmapped"))
	assert.True(t, strings.HasSuffix(row("sa-unknown"), "  unknown (report_stale)"), "unknown is shown, not hidden: %s", row("sa-unknown"))
	assert.Contains(t, out, "broker broker-a, profile gke")
	assert.Contains(t, out, "does not mean ready")
}

func TestProjectSAList_AssignColumn(t *testing.T) {
	orig := saveSACLIState()
	defer orig.restore()

	seen := saCLIHub(t, saAssignBody)
	globalMode = false
	saOutputJSON = false
	withSAAssignFlags(t, "gke", "")

	out := captureStdout(t, func() { require.NoError(t, runSAList(nil, nil)) })

	require.Equal(t, "gke", seen.Get("profile"))
	require.NotContains(t, *seen, "broker", "no --broker leaves the choice to the Hub")
	assert.Contains(t, out, "MAPPED    ASSIGN")
	assert.Contains(t, out, "unknown (report_stale)")
	assert.Contains(t, out, "not mapped")
}

// Without the flags nothing extra is asked for and the table is unchanged.
func TestSALists_NoAssignFlagsUnchanged(t *testing.T) {
	orig := saveSACLIState()
	defer orig.restore()
	withSAAssignFlags(t, "", "")

	seen := saCLIHub(t, saAssignBody)
	globalMode = false
	saOutputJSON = false
	saGlobalListAssignable = true
	out := captureStdout(t, func() { require.NoError(t, runSAScopedList(nil, nil)) })
	for _, k := range []string{"assignStatus", "profile", "broker"} {
		require.NotContains(t, *seen, k)
	}
	assert.NotContains(t, out, "ASSIGN")
	assert.Contains(t, out, "GCP PROJECT           VERIFIED\n")

	seen = saCLIHub(t, saAssignBody)
	out = captureStdout(t, func() { require.NoError(t, runSAList(nil, nil)) })
	require.NotContains(t, *seen, "profile")
	assert.NotContains(t, out, "ASSIGN")
	assert.Contains(t, out, "VERIFIED  MAPPED\n")
}

func TestSAScopedList_AssignFlagsWithGlobalRefused(t *testing.T) {
	orig := saveSACLIState()
	defer orig.restore()
	withSAAssignFlags(t, "gke", "")
	globalMode = true
	saGlobalListAssignable = false
	err := runSAScopedList(nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--global")
}

func TestSAAssignColumn(t *testing.T) {
	for _, tc := range []struct {
		st   *hubclient.GCPServiceAccountAssignStatus
		want string
	}{
		{nil, "-"},
		{&hubclient.GCPServiceAccountAssignStatus{State: "mapped"}, "mapped"},
		{&hubclient.GCPServiceAccountAssignStatus{State: "not_mapped"}, "not mapped"},
		{&hubclient.GCPServiceAccountAssignStatus{State: "not_required"}, "not required"},
		{&hubclient.GCPServiceAccountAssignStatus{State: "unknown", Reason: "no_broker"}, "unknown (no_broker)"},
		{&hubclient.GCPServiceAccountAssignStatus{State: "unknown"}, "unknown"},
	} {
		assert.Equal(t, tc.want, saAssignColumn(tc.st))
	}
}

// --broker without --profile is a usage error on both lists, before any
// request.
func TestSALists_BrokerWithoutProfileRefused(t *testing.T) {
	orig := saveSACLIState()
	defer orig.restore()
	withSAAssignFlags(t, "", "broker-a")

	seen := saCLIHub(t, saAssignBody)
	globalMode = false
	saGlobalListAssignable = true
	err := runSAScopedList(nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--profile")
	err = runSAList(nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--profile")
	assert.Empty(t, *seen, "no request was sent")
}
