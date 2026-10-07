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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// issueAgentTokenForTest issues a token for the stored agent record the way
// a dispatcher mint site does: authorize, sign for the agent's current run,
// record the credential.
func (s *Server) issueAgentTokenForTest(ctx context.Context, agent *store.Agent) (string, error) {
	grant, err := s.AuthorizeAgentToken(ctx, agent)
	if err != nil {
		return "", err
	}
	return signAndRecordAgentToken(ctx, s, s.store, grant, agent.RunID)
}
