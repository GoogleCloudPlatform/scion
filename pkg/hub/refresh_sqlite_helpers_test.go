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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

type staticTokenGenerator struct{ token string }

func (g staticTokenGenerator) GenerateAgentToken(string, string, []string, AgentRole, []AgentTokenScope) (string, error) {
	return g.token, nil
}

func (g staticTokenGenerator) AuthorizeAgentToken(_ context.Context, agent *store.Agent) (AgentTokenGrant, error) {
	return AgentTokenGrant{AgentID: agent.ID, ProjectID: agent.ProjectID}, nil
}

func (g staticTokenGenerator) SignAgentToken(grant AgentTokenGrant, runID string) (string, *store.AgentCredential, error) {
	return g.token, &store.AgentCredential{AgentID: grant.AgentID, ProjectID: grant.ProjectID, TokenJTIHash: hashJTI(uuid.NewString()), RunID: runID}, nil
}
