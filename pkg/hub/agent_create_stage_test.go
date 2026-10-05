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
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// signErrStorage fails every signed URL request.
type signErrStorage struct {
	*mockStorage
}

func (signErrStorage) GenerateSignedURL(context.Context, string, storage.SignedURLOptions) (*storage.SignedURL, error) {
	return nil, errors.New("injected signed URL fault")
}

// runIntentErrStore fails every run-intent write.
type runIntentErrStore struct {
	store.Store
}

func (runIntentErrStore) SwapRunIntent(context.Context, string, store.RunIntent) (store.RunIntent, time.Time, error) {
	return "", time.Time{}, errors.New("injected run intent fault")
}

// useFailingManagedBackend swaps in a managed-agent backend whose create
// fails, for the rest of the test.
func useFailingManagedBackend(t *testing.T) {
	t.Helper()
	managedBackendMu.Lock()
	prev := managedBackendInst
	managedBackendInst = failingManagedAgentBackend{}
	managedBackendMu.Unlock()
	t.Cleanup(func() {
		managedBackendMu.Lock()
		managedBackendInst = prev
		managedBackendMu.Unlock()
	})
}

// Each create failure site records its own stage in the rollback audit
// record.
//
// Must not run in parallel: the managed case swaps the package-level
// managed-agent backend.
func TestCreateRollbackStageAtEachSite(t *testing.T) {
	workspaceFiles := []transfer.FileInfo{{Path: "main.go", Size: 100, Hash: "sha256:abc123"}}
	cases := []struct {
		name      string
		disp      AgentDispatcher
		setup     func(t *testing.T, srv *Server)
		req       CreateAgentRequest
		wantStage string
	}{
		{
			name:      "storage",
			disp:      &createAgentDispatcher{},
			req:       CreateAgentRequest{WorkspaceFiles: workspaceFiles},
			wantStage: createStageStorage,
		},
		{
			name: "upload URL",
			disp: &createAgentDispatcher{},
			setup: func(t *testing.T, srv *Server) {
				srv.SetStorage(signErrStorage{newMockStorage("test-bucket")})
			},
			req:       CreateAgentRequest{WorkspaceFiles: workspaceFiles},
			wantStage: createStageUploadURL,
		},
		{
			name:      "managed",
			disp:      &createAgentDispatcher{},
			setup:     func(t *testing.T, _ *Server) { useFailingManagedBackend(t) },
			req:       CreateAgentRequest{Profile: ManagedAgentsProfile},
			wantStage: createStageManaged,
		},
		{
			name: "run intent",
			disp: &createAgentDispatcher{},
			setup: func(t *testing.T, srv *Server) {
				srv.store = runIntentErrStore{srv.store}
			},
			wantStage: createStageRunIntent,
		},
		{
			name:      "dispatch with env gather",
			disp:      &failingCreateDispatcher{createErr: errors.New("broker unavailable")},
			req:       CreateAgentRequest{GatherEnv: true},
			wantStage: createStageDispatchEnvGather,
		},
		{
			name:      "dispatch",
			disp:      &failingCreateDispatcher{createErr: errors.New("broker unavailable")},
			wantStage: createStageDispatch,
		},
		{
			name:      "missing env",
			disp:      &createAgentDispatcher{envReqs: &RemoteEnvRequirementsResponse{Needs: []string{"SOME_REQUIRED_KEY"}}},
			wantStage: createStageMissingEnv,
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, project := setupCreateAgentServer(t, tc.disp)
			if tc.setup != nil {
				tc.setup(t, srv)
			}
			req := tc.req
			req.Name = "stage-" + tidSlugSafe(tc.name)
			req.ProjectID = project.ID
			req.Task = "do something"

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", req)
			require.GreaterOrEqual(t, rec.Code, 400, "case %d: %s", i, rec.Body.String())

			failed, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{TargetType: "agent", MutationType: mutationTypeAgentCreateDispatchFailed})
			require.NoError(t, err)
			require.Len(t, failed, 1, "the rolled-back create is recorded once")
			var sum compensationSummary
			require.NoError(t, json.Unmarshal([]byte(failed[0].AfterSummary), &sum))
			assert.Equal(t, tc.wantStage, sum.Stage)
		})
	}
}
