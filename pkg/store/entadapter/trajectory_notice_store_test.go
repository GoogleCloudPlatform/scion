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

package entadapter

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

func TestRunAttribution_NoticeVersionAndCapturePolicyStamping(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)

	var (
		captureEnabled = false
		allowOptOut    = true
	)
	cs.SetTrajectoryCaptureState(func() (bool, bool) {
		return captureEnabled, allowOptOut
	})
	cs.SetRunAttributionConfig(&store.RunAttributionConfig{HubID: "hub-notice"})

	newProjectWithPolicy := func(name, policy string) string {
		t.Helper()
		pid := uuid.NewString()
		proj := &store.Project{
			ID:   pid,
			Name: name,
			Slug: name + "-" + pid[:8],
		}
		if policy != "" {
			proj.Annotations = map[string]string{
				telemetrycontract.TrajectoryCaptureProjectAnnotation: policy,
			}
		}
		require.NoError(t, cs.CreateProject(ctx, proj))
		return pid
	}

	newAgent := func(projectID, name string) *store.Agent {
		t.Helper()
		ag := &store.Agent{
			ID:        uuid.NewString(),
			ProjectID: projectID,
			Name:      name,
			Slug:      name + "-" + uuid.NewString()[:6],
			Template:  "claude",
			Phase:     "created",
		}
		require.NoError(t, cs.CreateAgent(ctx, ag))
		return ag
	}

	// 1. Experiment OFF: both capture_policy and notice_version remain empty.
	projDefault := newProjectWithPolicy("proj-default", "")
	agOffExp := newAgent(projDefault, "ag-off-exp")
	_, err := cs.SetAgentRunID(ctx, agOffExp.ID, "run-exp-off", nil)
	require.NoError(t, err)
	attr, err := cs.GetRunAttribution(ctx, "run-exp-off")
	require.NoError(t, err)
	assert.Empty(t, attr.CapturePolicy)
	assert.Empty(t, attr.NoticeVersion)

	// 2. Experiment ON, default policy (full): stamps capture_policy="full", notice_version="v1".
	captureEnabled = true
	allowOptOut = true
	agFull := newAgent(projDefault, "ag-full")
	_, err = cs.SetAgentRunID(ctx, agFull.ID, "run-full", nil)
	require.NoError(t, err)
	attr, err = cs.GetRunAttribution(ctx, "run-full")
	require.NoError(t, err)
	assert.Equal(t, telemetrycontract.TrajectoryCapturePolicyFull, attr.CapturePolicy)
	assert.Equal(t, telemetrycontract.TrajectoryNoticeVersion, attr.NoticeVersion)

	// 3. Experiment ON, project metadata_only: stamps capture_policy="metadata_only", notice_version="v1".
	projMeta := newProjectWithPolicy("proj-meta", telemetrycontract.TrajectoryCapturePolicyMetadataOnly)
	agMeta := newAgent(projMeta, "ag-meta")
	_, err = cs.SetAgentRunID(ctx, agMeta.ID, "run-meta", nil)
	require.NoError(t, err)
	attr, err = cs.GetRunAttribution(ctx, "run-meta")
	require.NoError(t, err)
	assert.Equal(t, telemetrycontract.TrajectoryCapturePolicyMetadataOnly, attr.CapturePolicy)
	assert.Equal(t, telemetrycontract.TrajectoryNoticeVersion, attr.NoticeVersion)

	// 4. Experiment ON, project off, opt-out allowed: stamps capture_policy="off", notice_version="".
	projOff := newProjectWithPolicy("proj-off", telemetrycontract.TrajectoryCapturePolicyOff)
	agOff := newAgent(projOff, "ag-off")
	_, err = cs.SetAgentRunID(ctx, agOff.ID, "run-policy-off", nil)
	require.NoError(t, err)
	attr, err = cs.GetRunAttribution(ctx, "run-policy-off")
	require.NoError(t, err)
	assert.Equal(t, telemetrycontract.TrajectoryCapturePolicyOff, attr.CapturePolicy)
	assert.Empty(t, attr.NoticeVersion)

	// 5. Experiment ON, project off, Hub opt-out lock enforced: stamps capture_policy="full", notice_version="v1".
	allowOptOut = false
	agEnforced := newAgent(projOff, "ag-enforced")
	_, err = cs.SetAgentRunID(ctx, agEnforced.ID, "run-enforced", nil)
	require.NoError(t, err)
	attr, err = cs.GetRunAttribution(ctx, "run-enforced")
	require.NoError(t, err)
	assert.Equal(t, telemetrycontract.TrajectoryCapturePolicyFull, attr.CapturePolicy)
	assert.Equal(t, telemetrycontract.TrajectoryNoticeVersion, attr.NoticeVersion)

	// 6. Immutability (fill once): flipping experiment OFF and calling EnsureRunAttributionForAgent
	// does NOT overwrite the already-stamped capture_policy or notice_version.
	captureEnabled = false
	existing, err := cs.EnsureRunAttributionForAgent(ctx, agFull.ID, "run-full", "hub-notice")
	require.NoError(t, err)
	assert.Equal(t, telemetrycontract.TrajectoryCapturePolicyFull, existing.CapturePolicy)
	assert.Equal(t, telemetrycontract.TrajectoryNoticeVersion, existing.NoticeVersion)

	// 7. Lazy EnsureRunAttributionForAgent when usage_ledger atomic hook is OFF
	// still stamps capture_policy and notice_version when trajectory capture is ON.
	cs.SetRunAttributionConfig(nil)
	captureEnabled = true
	allowOptOut = true
	agLazy := newAgent(projMeta, "ag-lazy")
	_, err = cs.SetAgentRunID(ctx, agLazy.ID, "run-lazy", nil)
	require.NoError(t, err)
	_, err = cs.GetRunAttribution(ctx, "run-lazy")
	require.ErrorIs(t, err, store.ErrNotFound)

	lazyAttr, err := cs.EnsureRunAttributionForAgent(ctx, agLazy.ID, "run-lazy", "hub-notice")
	require.NoError(t, err)
	assert.Equal(t, telemetrycontract.TrajectoryCapturePolicyMetadataOnly, lazyAttr.CapturePolicy)
	assert.Equal(t, telemetrycontract.TrajectoryNoticeVersion, lazyAttr.NoticeVersion)

	// 8. BackfillActiveRunAttributions for a run already active before the ledger
	// was enabled must NOT stamp notice_version (or live capture_policy).
	agBackfill := newAgent(projDefault, "ag-backfill")
	_, err = cs.SetAgentRunID(ctx, agBackfill.ID, "run-backfill-active", nil)
	require.NoError(t, err)
	_, err = cs.GetRunAttribution(ctx, "run-backfill-active")
	require.ErrorIs(t, err, store.ErrNotFound)

	inserted, err := cs.BackfillActiveRunAttributions(ctx, "hub-notice")
	require.NoError(t, err)
	assert.GreaterOrEqual(t, inserted, 1)
	bfAttr, err := cs.GetRunAttribution(ctx, "run-backfill-active")
	require.NoError(t, err)
	assert.Empty(t, bfAttr.NoticeVersion, "backfilled run_attribution must not stamp notice_version")
	assert.Empty(t, bfAttr.CapturePolicy, "backfilled run_attribution must not retroactively stamp capture_policy")
}

func TestResolveRunAttributionCaptureFields_MatchesTelemetryContract(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)

	annotations := []string{"", "invalid", "full", "metadata_only", "off"}
	for _, allowOptOut := range []bool{true, false} {
		for _, ann := range annotations {
			pid := uuid.New()
			proj := &store.Project{
				ID:   pid.String(),
				Name: "p-" + pid.String()[:8],
				Slug: "p-" + pid.String()[:8],
			}
			if ann != "" {
				proj.Annotations = map[string]string{
					telemetrycontract.TrajectoryCaptureProjectAnnotation: ann,
				}
			}
			require.NoError(t, cs.CreateProject(ctx, proj))

			wantStatus := telemetrycontract.ResolveTrajectoryCapturePolicy(allowOptOut, ann)
			wantNotice := ""
			if telemetrycontract.IsTrajectoryCaptureEffective(wantStatus.Policy) {
				wantNotice = telemetrycontract.TrajectoryNoticeVersion
			}

			gotPolicy, gotNotice := resolveRunAttributionCaptureFields(ctx, client, pid, func() (bool, bool) {
				return true, allowOptOut
			})
			assert.Equal(t, wantStatus.Policy, gotPolicy, "allowOptOut=%v ann=%q", allowOptOut, ann)
			assert.Equal(t, wantNotice, gotNotice, "allowOptOut=%v ann=%q", allowOptOut, ann)
		}
	}
}
