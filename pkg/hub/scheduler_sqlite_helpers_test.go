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

package hub

import (
	"context"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

func fireScheduledDispatchAsOwner(t *testing.T, f *bypassAgentsFixture, agentName string) error {
	t.Helper()
	ctx := context.Background()
	f.srv.seedProjectCreatorMembership(ctx, f.proj)
	require.NoError(t, f.srv.createProjectOwnerRoleBinding(ctx, f.proj.ID, f.owner.ID))
	return f.srv.dispatchAgentEventHandler()(ctx, withSessionRevision(store.ScheduledEvent{
		ID:        "evt-" + agentName,
		ProjectID: f.proj.ID,
		EventType: "dispatch_agent",
		Payload:   `{"agentName":"` + agentName + `","task":"scheduled work"}`,
		CreatedBy: f.owner.ID,
	}, f.owner.ID))
}

func setProjectDefaultSAAnnotations(t *testing.T, f *bypassAgentsFixture, saID string) {
	t.Helper()
	ctx := context.Background()
	proj, err := f.store.GetProject(ctx, f.proj.ID)
	require.NoError(t, err)
	if proj.Annotations == nil {
		proj.Annotations = map[string]string{}
	}
	proj.Annotations[projectSettingDefaultGCPIdentityMode] = store.GCPMetadataModeAssign
	proj.Annotations[projectSettingDefaultGCPIdentitySAID] = saID
	require.NoError(t, f.store.UpdateProject(ctx, proj))
}
