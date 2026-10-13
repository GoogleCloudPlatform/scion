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
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// withSkipped moves the applied keys the apply step skipped into Skipped,
// with a reason that names no configured value; pending_restart is
// unchanged (ptone/scion#4466).
func TestSavedKeysReport_WithSkipped(t *testing.T) {
	base := savedKeysReport{
		Applied: []string{
			"server.federation.enabled", "server.federation.trusted_issuers",
			"server.hub.hub_name", notificationChannelsKey,
		},
		PendingRestart: []string{"server.hub.public_url"},
		Skipped:        []skippedKey{},
	}

	r := base.withSkipped(layer1ApplySkips{})
	assert.Equal(t, base.Applied, r.Applied, "nothing skipped, nothing moved")
	assert.NotNil(t, r.Skipped)
	assert.Empty(t, r.Skipped)

	r = base.withSkipped(layer1ApplySkips{ChannelCount: 3, SkippedChannels: []int{1}})
	assert.Equal(t, []string{"server.federation.enabled", "server.federation.trusted_issuers", "server.hub.hub_name"}, r.Applied)
	require.Len(t, r.Skipped, 1)
	assert.Equal(t, notificationChannelsKey, r.Skipped[0].Key)
	assert.Equal(t, "Notification channel 2 of 3 failed validation and is not in use. The other channels are in use.", r.Skipped[0].Reason)
	assert.Equal(t, []string{"server.hub.public_url"}, r.PendingRestart)

	r = base.withSkipped(layer1ApplySkips{ChannelCount: 3, SkippedChannels: []int{0, 2}, FederationReason: federationSkipBuildFailed})
	assert.Equal(t, []string{"server.hub.hub_name"}, r.Applied)
	assert.Equal(t, []skippedKey{
		{Key: "server.federation.enabled", Reason: federationSkipBuildFailed},
		{Key: "server.federation.trusted_issuers", Reason: federationSkipBuildFailed},
		{Key: notificationChannelsKey, Reason: "Notification channels 1, 3 of 3 failed validation and are not in use. The other channels are in use."},
	}, r.Skipped)
	assert.Equal(t, []string{"server.hub.public_url"}, r.PendingRestart)

	r = base.withSkipped(layer1ApplySkips{ChannelCount: 2, SkippedChannels: []int{0, 1}})
	require.Len(t, r.Skipped, 1)
	assert.Equal(t, "Notification channels 1, 2 of 2 failed validation and are not in use. No notification channel is in use.", r.Skipped[0].Reason)

	// A skip recorded for a key this save did not change is not reported.
	r = savedKeysReport{Applied: []string{"server.hub.hub_name"}, PendingRestart: []string{}, Skipped: []skippedKey{}}.
		withSkipped(layer1ApplySkips{ChannelCount: 1, SkippedChannels: []int{0}, FederationReason: federationSkipInvalid})
	assert.Equal(t, []string{"server.hub.hub_name"}, r.Applied)
	assert.Empty(t, r.Skipped)
}

type skippedSaveReport struct {
	AppliedKeys    []string     `json:"applied_keys"`
	PendingRestart []string     `json:"pending_restart"`
	Skipped        []skippedKey `json:"skipped"`
}

func skippedReport(t *testing.T, body []byte) skippedSaveReport {
	t.Helper()
	var resp struct {
		Reload skippedSaveReport `json:"reload"`
	}
	require.NoError(t, json.Unmarshal(body, &resp))
	require.NotNil(t, resp.Reload.Skipped, "skipped is always a list")
	return resp.Reload
}

// A saved notification channel that fails validation is skipped by the
// apply step; the save reports server.notification_channels as skipped,
// not applied (ptone/scion#4466).
func TestPutServerConfigDB_InvalidChannelReportedSkipped(t *testing.T) {
	srv, _, ops := newTestDBServer(t)
	ops.server = srv // the write self-applies, as on a running hub

	rr := putHubNameServerConfigDB(t, srv, ops, `{"server":{
		"hub":{"hub_name":"skip-hub"},
		"notification_channels":[
			{"type":"slack","params":{"webhook_url":"https://hooks.slack.com/services/T0/B0/okay"}},
			{"type":"slack","params":{"webhook_url":"https://not-slack.example.com/token-4466"}}
		]}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	rep := skippedReport(t, rr.Body.Bytes())
	assert.Contains(t, rep.AppliedKeys, "server.hub.hub_name")
	assert.NotContains(t, rep.AppliedKeys, notificationChannelsKey, "a skipped channel is not reported as applied")
	require.Len(t, rep.Skipped, 1)
	assert.Equal(t, notificationChannelsKey, rep.Skipped[0].Key)
	assert.Contains(t, rep.Skipped[0].Reason, "Notification channel 2 of 2")
	assert.NotContains(t, rep.Skipped[0].Reason, "not-slack.example.com")
	assert.NotContains(t, rep.Skipped[0].Reason, "token-4466")
	assert.Empty(t, rep.PendingRestart)
	reg := srv.currentChannelRegistry()
	require.NotNil(t, reg)
	assert.Equal(t, 1, reg.Len(), "only the valid channel is in use")

	// Fixing the channel list applies it.
	rr = putHubNameServerConfigDB(t, srv, ops, `{"server":{"notification_channels":[
		{"type":"slack","params":{"webhook_url":"https://hooks.slack.com/services/T0/B0/okay"}}
	]}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	rep = skippedReport(t, rr.Body.Bytes())
	assert.Equal(t, []string{notificationChannelsKey}, rep.AppliedKeys)
	assert.Empty(t, rep.Skipped)
}

// A saved federation configuration whose authenticator cannot be built is
// not installed; the save reports its changed keys as skipped, not
// applied, and the previous authenticator stays in use (ptone/scion#4466).
func TestPutServerConfigDB_FederationRebuildErrorReportedSkipped(t *testing.T) {
	srv, _, ops := newTestDBServer(t)
	ops.server = srv // the write self-applies, as on a running hub
	srv.federationClient = &http.Client{Timeout: time.Second}
	previous := &FederationAuthenticator{}
	srv.federationAuth.Store(previous)

	// Valid as saved, but a hosted hub refuses an http issuer when it builds
	// the authenticator. Federation only: another section's self-apply sees no
	// federation row and clears the authenticator stored by hand above.
	rr := putHubNameServerConfigDB(t, srv, ops, `{
		"federation":{"enabled":true,"trusted_issuers":[{"issuer_url":"http://issuer-4466.example.com"}]}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	rep := skippedReport(t, rr.Body.Bytes())
	assert.Empty(t, rep.AppliedKeys, "no federation key is reported as applied")
	assert.Equal(t, []skippedKey{
		{Key: "server.federation.enabled", Reason: federationSkipBuildFailed},
		{Key: "server.federation.trusted_issuers", Reason: federationSkipBuildFailed},
	}, rep.Skipped)
	for _, sk := range rep.Skipped {
		assert.NotContains(t, sk.Reason, "issuer-4466")
	}
	assert.Same(t, previous, srv.federationAuth.Load(), "the previous authenticator stays in use")

	// A buildable configuration is installed and its changed key is
	// applied again. A hub issuer with an https URL and an expected
	// audience builds without network I/O.
	rr = putHubNameServerConfigDB(t, srv, ops, `{"federation":{"enabled":true,"trusted_issuers":[
		{"issuer_url":"https://issuer-4466.example.com","expected_audience":"https://hub.example.com"}]}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	rep = skippedReport(t, rr.Body.Bytes())
	assert.Equal(t, []string{"server.federation.trusted_issuers"}, rep.AppliedKeys)
	assert.Empty(t, rep.Skipped)
	installed := srv.federationAuth.Load()
	require.NotNil(t, installed)
	assert.NotSame(t, previous, installed, "the new authenticator is in use")

	// Turning federation off applies.
	rr = putHubNameServerConfigDB(t, srv, ops, `{"federation":{"enabled":false,"trusted_issuers":[
		{"issuer_url":"https://issuer-4466.example.com","expected_audience":"https://hub.example.com"}]}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	rep = skippedReport(t, rr.Body.Bytes())
	assert.Equal(t, []string{"server.federation.enabled"}, rep.AppliedKeys)
	assert.Empty(t, rep.Skipped)
	assert.Nil(t, srv.federationAuth.Load())
}

// ApplySnapshot records why it did not install a federation
// configuration, and clears the reason once federation is off.
func TestApplySnapshot_RecordsFederationSkipReason(t *testing.T) {
	srv := &Server{}
	srv.config.Mode = "dev"
	srv.config.Workstation = true

	ApplySnapshot(srv, Layer1Snapshot{FederationConfig: &config.FederationConfig{Enabled: true}})
	assert.Equal(t, federationSkipInvalid, srv.layer1ApplySkips().FederationReason)

	ApplySnapshot(srv, Layer1Snapshot{FederationConfig: &config.FederationConfig{Enabled: false}})
	assert.Empty(t, srv.layer1ApplySkips().FederationReason)

	// While the test admin tier is on, a federation configuration is
	// never installed, and one already installed is removed.
	tier := &Server{testAdminGate: testAdminGate{hubAdmin: true}}
	tier.federationAuth.Store(&FederationAuthenticator{})
	ApplySnapshot(tier, Layer1Snapshot{FederationConfig: &config.FederationConfig{Enabled: true}})
	assert.Equal(t, federationSkipTestAdminTier, tier.layer1ApplySkips().FederationReason)
	assert.Nil(t, tier.federationAuth.Load())
}

// newChannelRegistry returns the indexes of the channels it skipped, both
// for an unknown type and for a channel that fails validation.
func TestNewChannelRegistry_ReturnsSkippedIndexes(t *testing.T) {
	reg, skipped := newChannelRegistry([]ChannelConfig{
		{Type: "carrier-pigeon"},
		{Type: "slack", Params: map[string]string{"webhook_url": "https://hooks.slack.com/services/T0/B0/okay"}},
		{Type: "slack", Params: map[string]string{"webhook_url": "https://not-slack.example.com/"}},
	}, slog.New(slog.DiscardHandler))
	assert.Equal(t, []int{0, 2}, skipped)
	assert.Equal(t, 1, reg.Len())
}
