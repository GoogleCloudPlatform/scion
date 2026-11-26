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
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTriggerWebhookCreate_ProviderModeAndRoutingHints(t *testing.T) {
	origName := triggerWebhookName
	origSig := triggerWebhookSignatureMode
	origType := triggerWebhookTypePath
	origSub := triggerWebhookSubjectPath
	origDedup := triggerWebhookDedupPath
	defer func() {
		triggerWebhookName = origName
		triggerWebhookSignatureMode = origSig
		triggerWebhookTypePath = origType
		triggerWebhookSubjectPath = origSub
		triggerWebhookDedupPath = origDedup
	}()

	triggerWebhookName = "gh-repo"
	triggerWebhookSignatureMode = "github_v1"
	triggerWebhookTypePath = ""
	triggerWebhookSubjectPath = "/repository/full_name"
	triggerWebhookDedupPath = ""

	req, err := buildCreateWebhookEndpointRequest()
	require.NoError(t, err)
	assert.Equal(t, "github_v1", req.SignatureMode)
	require.NotNil(t, req.RoutingHints)
	assert.Equal(t, "/repository/full_name", req.RoutingHints.SubjectPath)
	assert.Empty(t, req.RoutingHints.TypePath)

	// Invalid signature mode fails with usage error.
	triggerWebhookSignatureMode = "bogus_v1"
	_, err = buildCreateWebhookEndpointRequest()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--signature-mode")

	// Help text includes sender examples for all three modes.
	assert.Contains(t, triggerWebhookCreateCmd.Long, "scion_v1")
	assert.Contains(t, triggerWebhookCreateCmd.Long, "github_v1")
	assert.Contains(t, triggerWebhookCreateCmd.Long, "X-Hub-Signature-256")
	assert.Contains(t, triggerWebhookCreateCmd.Long, "stripe_v1")
	assert.Contains(t, triggerWebhookCreateCmd.Long, "Stripe-Signature")
}

func TestTriggerWebhookUpdate_ProviderModeAndRoutingHints(t *testing.T) {
	origSig := triggerWebhookUpdateSignatureMode
	origType := triggerWebhookUpdateTypePath
	origSub := triggerWebhookUpdateSubjectPath
	origDedup := triggerWebhookUpdateDedupPath
	defer func() {
		triggerWebhookUpdateSignatureMode = origSig
		triggerWebhookUpdateTypePath = origType
		triggerWebhookUpdateSubjectPath = origSub
		triggerWebhookUpdateDedupPath = origDedup
	}()

	cmd := &cobra.Command{Use: "update"}
	bindTriggerWebhookUpdateFlags(cmd)
	require.NoError(t, cmd.ParseFlags([]string{
		"--signature-mode", "stripe_v1",
		"--type-path", "/custom/type",
		"--dedup-path", "",
	}))

	req, err := buildUpdateWebhookEndpointRequest(cmd)
	require.NoError(t, err)
	require.NotNil(t, req.SignatureMode)
	assert.Equal(t, "stripe_v1", *req.SignatureMode)
	require.NotNil(t, req.RoutingHints)
	require.NotNil(t, req.RoutingHints.TypePath)
	assert.Equal(t, "/custom/type", *req.RoutingHints.TypePath)
	assert.Nil(t, req.RoutingHints.SubjectPath, "unset flag must remain nil for partial PATCH")
	require.NotNil(t, req.RoutingHints.DedupPath)
	assert.Equal(t, "", *req.RoutingHints.DedupPath, "explicit empty string clears path")
}
