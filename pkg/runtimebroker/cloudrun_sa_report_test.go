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

package runtimebroker

import (
	"encoding/json"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	crReportA     = "a@example-project.iam.gserviceaccount.com"
	crReportB     = "b@example-project.iam.gserviceaccount.com"
	crReportBlock = "zero@example-project.iam.gserviceaccount.com"
)

// cloudRunReportSettings has a Cloud Run profile on the runtimes.<entry>.cloudrun
// form with an allow-list and a block account, a cloudrun-instances profile,
// a docker profile and a Kubernetes profile.
func cloudRunReportSettings(list []string, block string) *config.VersionedSettings {
	return &config.VersionedSettings{
		Profiles: map[string]config.V1ProfileConfig{
			"cr":       {Runtime: "cr-entry"},
			"cr-inst":  {Runtime: "cr-inst-entry"},
			"local":    {Runtime: "docker"},
			"gke":      {Runtime: "gke-entry"},
			"cr-bare":  {Runtime: "cloudrun"},
			"no-entry": {},
		},
		Runtimes: map[string]config.V1RuntimeConfig{
			"cr-entry": {Type: "cloudrun", CloudRun: &config.CloudRunConfig{
				AssignableServiceAccounts: list,
				BlockServiceAccount:       block,
			}},
			"cr-inst-entry": {Type: "cloudrun-instances", CloudRunInstances: &config.V1CloudRunInstancesConfig{}},
			"docker":        {Type: "docker"},
			"gke-entry":     {Type: "kubernetes", Namespace: "agents"},
			"cloudrun":      {},
		},
	}
}

func cloudRunReportServer(vs *config.VersionedSettings, forced string) *Server {
	srv := &Server{loadMappingSettings: func() (*config.VersionedSettings, error) { return vs, nil }}
	srv.config.ForceRuntime = forced
	srv.saDiscoveryCache = newSADiscoveryCache(func(string) (kubernetes.Interface, error) { return fake.NewClientset(), nil }, discardLogger())
	return srv
}

func reportByName(t *testing.T, states []hubclient.ProfileSAMappingsState, name string) (hubclient.ProfileSAMappingsState, bool) {
	t.Helper()
	for _, s := range states {
		if s.Name == name {
			return s, true
		}
	}
	return hubclient.ProfileSAMappingsState{}, false
}

// A Cloud Run profile on the runtimes.<entry>.cloudrun form reports its
// allow-list (lower-cased, trimmed, de-duplicated, sorted; GSA only, Source
// cloudrun_allowlist), complete, at the current report version, with the
// native identity capability and the block account as a boolean only.
func TestServer_HeartbeatProfileSAMappings_CloudRunAllowList(t *testing.T) {
	srv := cloudRunReportServer(cloudRunReportSettings(
		[]string{" B@Example-Project.iam.gserviceaccount.com ", crReportA, "", crReportB}, crReportBlock), "")
	got := srv.heartbeatProfileSAMappings()
	srv.saDiscoveryCache.wait()

	cr, ok := reportByName(t, got, "cr")
	require.True(t, ok, "the Cloud Run profile is reported")
	assert.Equal(t, hubclient.ProfileSAMappingsState{
		Name: "cr",
		ServiceAccountMappings: []hubclient.BrokerProfileSAMapping{
			{GSA: crReportA, Source: api.BrokerSASourceCloudRunAllowlist},
			{GSA: crReportB, Source: api.BrokerSASourceCloudRunAllowlist},
		},
		Complete:               true,
		ReportVersion:          api.BrokerSAReportVersion,
		CloudRunNativeIdentity: true,
		CloudRunBlockAccount:   true,
	}, cr)
	assert.Equal(t, 3, api.BrokerSAReportVersion)

	// The block account itself never leaves the broker.
	b, err := json.Marshal(got)
	require.NoError(t, err)
	assert.False(t, strings.Contains(string(b), crReportBlock), "the block account is not in the report")

	_, ok = reportByName(t, got, "local")
	assert.False(t, ok, "a docker profile has no report")
	_, ok = reportByName(t, got, "no-entry")
	assert.False(t, ok, "a profile without a runtime entry has no report")
	_, ok = reportByName(t, got, "gke")
	assert.True(t, ok, "Kubernetes profiles are still reported")
}

// A cloudrun-instances entry reports the capability as not supported, with
// no list; a cloudrun entry with no cloudrun settings block is supported
// with an empty list and no block account.
func TestServer_HeartbeatProfileSAMappings_CloudRunInstancesNoList(t *testing.T) {
	srv := cloudRunReportServer(cloudRunReportSettings([]string{crReportA}, ""), "")
	got := srv.heartbeatProfileSAMappings()
	srv.saDiscoveryCache.wait()

	inst, ok := reportByName(t, got, "cr-inst")
	require.True(t, ok)
	assert.Equal(t, hubclient.ProfileSAMappingsState{
		Name:                   "cr-inst",
		ServiceAccountMappings: []hubclient.BrokerProfileSAMapping{},
		Complete:               true,
		ReportVersion:          api.BrokerSAReportVersion,
	}, inst)

	bare, ok := reportByName(t, got, "cr-bare")
	require.True(t, ok, "an entry keyed cloudrun with no type is a Cloud Run entry")
	assert.Equal(t, hubclient.ProfileSAMappingsState{
		Name:                   "cr-bare",
		ServiceAccountMappings: []hubclient.BrokerProfileSAMapping{},
		Complete:               true,
		ReportVersion:          api.BrokerSAReportVersion,
		CloudRunNativeIdentity: true,
	}, bare)

	cr, ok := reportByName(t, got, "cr")
	require.True(t, ok)
	assert.False(t, cr.CloudRunBlockAccount, "no block account configured")
	assert.True(t, cr.CloudRunNativeIdentity)
}

// The report hash changes when the allow-list or the block account
// presence changes, so the Hub asks for and stores the new report.
func TestServer_HeartbeatProfileSAMappings_CloudRunHashChanges(t *testing.T) {
	hashOf := func(list []string, block string) string {
		srv := cloudRunReportServer(cloudRunReportSettings(list, block), "")
		got := srv.heartbeatProfileSAMappings()
		srv.saDiscoveryCache.wait()
		cr, ok := reportByName(t, got, "cr")
		require.True(t, ok)
		return profileSAMappingsHash(cr)
	}
	base := hashOf([]string{crReportA}, crReportBlock)
	assert.Equal(t, base, hashOf([]string{" " + strings.ToUpper(crReportA)}, crReportBlock), "an equal list hashes equally")
	assert.NotEqual(t, base, hashOf([]string{crReportA, crReportB}, crReportBlock), "an added account changes the hash")
	assert.NotEqual(t, base, hashOf(nil, crReportBlock), "an emptied list changes the hash")
	assert.NotEqual(t, base, hashOf([]string{crReportA}, ""), "removing the block account changes the hash")
	assert.Equal(t, base, hashOf([]string{crReportA}, "other@example-project.iam.gserviceaccount.com"),
		"which block account is configured is not reported, only that one is")
}

// Under ForceRuntime a Cloud Run profile is reported incomplete, with no
// list and no capability.
func TestServer_HeartbeatProfileSAMappings_CloudRunForceRuntime(t *testing.T) {
	srv := cloudRunReportServer(cloudRunReportSettings([]string{crReportA}, crReportBlock), "cloudrun")
	got := srv.heartbeatProfileSAMappings()
	srv.saDiscoveryCache.wait()
	cr, ok := reportByName(t, got, "cr")
	require.True(t, ok)
	assert.Equal(t, hubclient.ProfileSAMappingsState{
		Name:                   "cr",
		ServiceAccountMappings: []hubclient.BrokerProfileSAMapping{},
		IncompleteReason:       api.BrokerSAReportForceRuntime,
		ReportVersion:          api.BrokerSAReportVersion,
	}, cr)
}

// A flat Runtime Broker instance sends no profile report at all, so a
// Cloud Run report never reaches the Hub from one.
func TestFlatHeartbeat_NoCloudRunReport(t *testing.T) {
	svc, client := profileScopedHeartbeat(true)
	svc.profileSAMappings = func() []hubclient.ProfileSAMappingsState {
		return []hubclient.ProfileSAMappingsState{{
			Name:                   "cr",
			ServiceAccountMappings: []hubclient.BrokerProfileSAMapping{{GSA: crReportA, Source: api.BrokerSASourceCloudRunAllowlist}},
			Complete:               true,
			ReportVersion:          api.BrokerSAReportVersion,
			CloudRunNativeIdentity: true,
		}}
	}
	hb := lastHeartbeat(t, svc, client)
	assert.Empty(t, hb.ProfileSAMappings)
	assert.Empty(t, hb.ProfileSAMappingsHashes)
}
