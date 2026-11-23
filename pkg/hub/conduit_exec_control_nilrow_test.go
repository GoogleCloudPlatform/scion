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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// execNilAgentRowStore returns a nil agent row with no error, breaking the store
// contract (ErrNotFound for a missing agent).
type execNilAgentRowStore struct {
	store.Store
}

func (execNilAgentRowStore) GetAgent(context.Context, string) (*store.Agent, error) { return nil, nil }

// TestCallAgentExec_NilAgentRowNotSent: a nil agent row is refused like a
// missing agent (ErrNotFound) before anything is routed or sent.
func TestCallAgentExec_NilAgentRowNotSent(t *testing.T) {
	s := &Server{store: execNilAgentRowStore{}}
	_, _, err := s.callAgentExec(context.Background(), "agent-1", "launch-1", agentcontrol.RouteExecStatus, []byte(`{}`), "")
	require.ErrorIs(t, err, store.ErrNotFound)
}
