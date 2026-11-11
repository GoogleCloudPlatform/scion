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
	"strconv"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/knadh/koanf/v2"
	"github.com/stretchr/testify/require"
)

// enableOffload sets messaging.offload_threshold_runes (and, when
// envelopeOn, conversation_envelope_switch) via OperationalSettings, mirroring
// enableCPM's pattern (cross_project_messaging_test.go).
func enableOffload(t *testing.T, srv *Server, threshold int, envelopeOn bool) {
	t.Helper()
	fakeStore := newFakeHubSettingStore()
	fileK := koanf.New(".")
	envK := koanf.New(".")
	ops := NewOperationalSettings(fakeStore, fileK, envK)
	doc := []byte(`{"conversation_envelope_switch":` + boolStr(envelopeOn) +
		`,"offload_threshold_runes":` + strconv.Itoa(threshold) + `}`)
	rev, err := ops.Update(context.Background(), "messaging", doc, "test", 0, "managed")
	require.NoError(t, err, "failed to seed messaging opsettings")
	require.Greater(t, rev, int64(0))
	require.Equal(t, threshold, ops.OffloadThresholdRunes())
	srv.SetOperationalSettings(ops)
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// predicateSetup creates two projects, each with one agent, and a hub owner.
// Hub-level cross-project messaging starts disabled (compiled default).
func predicateSetup(t *testing.T) (srv *Server, s store.Store, projA, projB string, agentA, agentB *store.Agent) {
	t.Helper()
	srv, s = testServer(t)
	ctx := context.Background()

	owner := &store.User{
		ID:      tid("pred-owner"),
		Email:   "pred-owner@test.example",
		Role:    store.UserRoleMember,
		Status:  "active",
		Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, owner))
	ensureHubMembership(ctx, s, owner.ID)

	projA = tid("pred-project-a")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projA, Name: "pred-a", Slug: "pred-a", OwnerID: owner.ID, CreatedBy: owner.ID,
	}))
	projB = tid("pred-project-b")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projB, Name: "pred-b", Slug: "pred-b", OwnerID: owner.ID, CreatedBy: owner.ID,
	}))

	agentA = &store.Agent{
		ID: tid("pred-agent-a"), Name: "agent-a", Slug: "agent-a",
		ProjectID: projA, Phase: "running", Ancestry: []string{owner.ID},
	}
	require.NoError(t, s.CreateAgent(ctx, agentA))
	agentB = &store.Agent{
		ID: tid("pred-agent-b"), Name: "agent-b", Slug: "agent-b",
		ProjectID: projB, Phase: "running", Ancestry: []string{owner.ID},
	}
	require.NoError(t, s.CreateAgent(ctx, agentB))

	return srv, s, projA, projB, agentA, agentB
}
