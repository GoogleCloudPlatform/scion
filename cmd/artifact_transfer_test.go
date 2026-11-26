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

//go:build !no_sqlite

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type cliTransferHost struct {
	oneAgentHost
	featureOn   bool
	callerKind  string
	callerRef   string
	callerScope string
	ownerLive   bool
	targets     []artifacts.TransferCandidate
}

func (h *cliTransferHost) Principal(context.Context) (string, string, string, bool) {
	return h.callerKind, h.callerRef, h.callerScope, true
}
func (h *cliTransferHost) Permits(context.Context, string, string) bool   { return true }
func (h *cliTransferHost) Authorize(context.Context, string, string) bool { return true }
func (h *cliTransferHost) MemberScopes(context.Context) ([]string, error) {
	return []string{"project-1"}, nil
}
func (h *cliTransferHost) FeatureEnabled(_ context.Context, name string) bool {
	return h.featureOn && name == artifacts.FeatureTransfer
}
func (h *cliTransferHost) SessionCredentialAllowed(context.Context) bool   { return true }
func (h *cliTransferHost) OwnerLive(context.Context, string) (bool, error) { return h.ownerLive, nil }
func (h *cliTransferHost) ControlsAgent(context.Context, string) bool      { return true }
func (h *cliTransferHost) StewardClause(context.Context, string, string, string, bool) string {
	return artifacts.StewardClauseDelegator
}
func (h *cliTransferHost) TransferTarget(_ context.Context, id, _ string) (string, bool, string, error) {
	for _, c := range h.targets {
		if c.ID == id {
			return "user-1", true, "", nil
		}
	}
	return "", false, artifacts.TargetReasonNotFound, nil
}
func (h *cliTransferHost) TransferTargets(context.Context, string) ([]artifacts.TransferCandidate, bool, error) {
	return h.targets, false, nil
}

func TestArtifactTransferCLI(t *testing.T) {
	oldAgentID := uuid.NewString()
	newAgentID := uuid.NewString()
	host := &cliTransferHost{
		featureOn:   true,
		callerKind:  artifacts.PrincipalKindAgent,
		callerRef:   oldAgentID,
		callerScope: "project-1",
		ownerLive:   false,
		targets: []artifacts.TransferCandidate{
			{ID: newAgentID, Slug: "worker-two", Name: "Worker Two", Phase: "running"},
		},
	}
	svc, _ := shareHub(t, host)
	ctx := context.Background()

	// Agent publishes artifact.
	body := "# handoff"
	pub, err := svc.Publish(ctx, &hubclient.PublishArtifactRequest{
		Name: "handoff.md", Title: "Handoff", Scope: "project-1",
		Content: strings.NewReader(body), Size: int64(len(body)),
	})
	require.NoError(t, err)
	ref := pub.Artifact.Ref

	// Switch to human steward caller.
	host.callerKind = artifacts.PrincipalKindUser
	host.callerRef = "user-1"
	host.callerScope = ""

	// 1. Feature off -> clear error message.
	host.featureOn = false
	var out bytes.Buffer
	err = transferArtifact(ctx, svc, &out, ref, transferOptions{To: "worker-two", Yes: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not enabled")
	host.featureOn = true

	// 2. Non-interactive without --yes -> refuses without transferring.
	err = transferArtifact(ctx, svc, &out, ref, transferOptions{To: "worker-two", Yes: false, Interactive: false})
	require.ErrorIs(t, err, errTransferNeedsConfirmation)

	// 3. Unknown target -> refuses with eligible candidate list.
	err = transferArtifact(ctx, svc, &out, ref, transferOptions{To: "ghost-agent", Yes: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "worker-two")

	// 4. Interactive declined -> no transfer, no error.
	out.Reset()
	err = transferArtifact(ctx, svc, &out, ref, transferOptions{
		To: "worker-two", Yes: false, Interactive: true, Ask: func(string) bool { return false },
	})
	require.NoError(t, err)
	assert.Contains(t, out.String(), "Not transferred.")

	// 5. Transfer with --yes and --format json succeeds.
	out.Reset()
	err = transferArtifact(ctx, svc, &out, ref, transferOptions{
		To: "worker-two", Yes: true, AsJSON: true,
	})
	require.NoError(t, err)
	var tr hubclient.ArtifactTransferResponse
	require.NoError(t, json.Unmarshal(out.Bytes(), &tr))
	assert.Equal(t, newAgentID, tr.Artifact.Artifact.OwnerRef)
	assert.Equal(t, oldAgentID, tr.Artifact.Artifact.FormerOwnerRef)
	assert.NotNil(t, tr.Artifact.Artifact.TransferredAt)
}
