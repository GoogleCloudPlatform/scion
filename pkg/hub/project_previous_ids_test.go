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
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	prevIDsRemote   = "github.com/acme/widgets"
	prevIDsLegacyID = "5fd0688b-4db9-5dc0-a2eb-bc4b5cc804b8" // legacy ID of prevIDsRemote
	prevIDsCurrent  = "0b9d3a52-6c1e-4f07-9a51-2f4c8d7e6a10"
	prevIDsOther    = "7e2c41aa-93b5-4d8e-b1f0-5a6c9d2e8b34"
)

// previousIDsStore answers only the two lookups previousProjectIDs makes.
type previousIDsStore struct {
	store.Store
	sameRemote    []*store.Project
	sameRemoteErr error
	byID          map[string]*store.Project
	getErr        error
}

func (s *previousIDsStore) GetProjectsByGitRemote(_ context.Context, _ string) ([]*store.Project, error) {
	return s.sameRemote, s.sameRemoteErr
}

func (s *previousIDsStore) GetProject(_ context.Context, id string) (*store.Project, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	if p, ok := s.byID[id]; ok {
		return p, nil
	}
	return nil, store.ErrNotFound
}

func TestPreviousProjectIDs(t *testing.T) {
	current := &store.Project{ID: prevIDsCurrent, GitRemote: prevIDsRemote}
	sharedInfo := projectDispatchInfo{sharedWorkspace: true, projectID: prevIDsCurrent, gitRemote: prevIDsRemote}
	for _, tc := range []struct {
		name  string
		info  projectDispatchInfo
		store *previousIDsStore
		want  []string
	}{
		{
			name:  "all conditions hold",
			info:  sharedInfo,
			store: &previousIDsStore{sameRemote: []*store.Project{current}},
			want:  []string{prevIDsLegacyID},
		},
		{
			name:  "not a shared-workspace project (e.g. worktree-per-agent)",
			info:  projectDispatchInfo{projectID: prevIDsCurrent, gitRemote: prevIDsRemote, workspaceMode: store.WorkspaceModeWorktreePerAgent},
			store: &previousIDsStore{sameRemote: []*store.Project{current}},
		},
		{
			name:  "no project",
			info:  projectDispatchInfo{sharedWorkspace: true, gitRemote: prevIDsRemote},
			store: &previousIDsStore{sameRemote: []*store.Project{current}},
		},
		{
			name:  "remote not in canonical form",
			info:  projectDispatchInfo{sharedWorkspace: true, projectID: prevIDsCurrent, gitRemote: "https://github.com/acme/widgets.git"},
			store: &previousIDsStore{sameRemote: []*store.Project{current}},
		},
		{
			name:  "malformed remote",
			info:  projectDispatchInfo{sharedWorkspace: true, projectID: prevIDsCurrent, gitRemote: "github.com/acme"},
			store: &previousIDsStore{sameRemote: []*store.Project{current}},
		},
		{
			name:  "derived ID is the project's own ID",
			info:  projectDispatchInfo{sharedWorkspace: true, projectID: prevIDsLegacyID, gitRemote: prevIDsRemote},
			store: &previousIDsStore{sameRemote: []*store.Project{{ID: prevIDsLegacyID, GitRemote: prevIDsRemote}}},
		},
		{
			name:  "another live project shares the remote",
			info:  sharedInfo,
			store: &previousIDsStore{sameRemote: []*store.Project{current, {ID: prevIDsOther, GitRemote: prevIDsRemote}}},
		},
		{
			name:  "the only project on the remote is another one",
			info:  sharedInfo,
			store: &previousIDsStore{sameRemote: []*store.Project{{ID: prevIDsOther, GitRemote: prevIDsRemote}}},
		},
		{
			name:  "no project on the remote",
			info:  sharedInfo,
			store: &previousIDsStore{},
		},
		{
			name:  "remote lookup fails",
			info:  sharedInfo,
			store: &previousIDsStore{sameRemoteErr: errors.New("db down")},
		},
		{
			name: "a live project holds the derived ID",
			info: sharedInfo,
			store: &previousIDsStore{
				sameRemote: []*store.Project{current},
				byID:       map[string]*store.Project{prevIDsLegacyID: {ID: prevIDsLegacyID}},
			},
		},
		{
			name:  "ID lookup fails",
			info:  sharedInfo,
			store: &previousIDsStore{sameRemote: []*store.Project{current}, getErr: errors.New("db down")},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := NewHTTPAgentDispatcher(tc.store, false, slog.Default())
			assert.Equal(t, tc.want, d.previousProjectIDs(context.Background(), tc.info))
		})
	}
}

func TestApplyStartExtras_PreviousProjectIDs(t *testing.T) {
	payload := map[string]interface{}{}
	applyStartExtras(payload, StartExtras{})
	_, present := payload["previousProjectIds"]
	assert.False(t, present, "an empty list sends nothing")

	payload = map[string]interface{}{}
	applyStartExtras(payload, StartExtras{PreviousProjectIDs: []string{prevIDsLegacyID}})
	assert.Equal(t, []string{prevIDsLegacyID}, payload["previousProjectIds"])
}

func TestRemoteCreateAgentRequest_PreviousProjectIDsWire(t *testing.T) {
	data, err := json.Marshal(RemoteCreateAgentRequest{Name: "a"})
	require.NoError(t, err)
	assert.NotContains(t, string(data), "previousProjectIds", "an empty list sends nothing (omitempty)")

	data, err = json.Marshal(RemoteCreateAgentRequest{Name: "a", PreviousProjectIDs: []string{prevIDsLegacyID}})
	require.NoError(t, err)
	assert.Contains(t, string(data), `"previousProjectIds":["`+prevIDsLegacyID+`"]`)
}
