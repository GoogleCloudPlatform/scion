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

package chatapp

import (
	"context"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// deleteResultAgentService reports a 202 or 204 through DeleteWithResult.
type deleteResultAgentService struct {
	*stubAgentService
	accepted bool
}

func (s *deleteResultAgentService) DeleteWithResult(ctx context.Context, id string, opts *hubclient.DeleteAgentOptions) (hubclient.DeleteResult, error) {
	return hubclient.DeleteResult{Accepted: s.accepted, AgentID: id}, nil
}

type deleteResultClient struct {
	*stubClient
	svc hubclient.AgentService
}

func (c *deleteResultClient) ProjectAgents(string) hubclient.AgentService { return c.svc }

func TestExecuteDelete_Wording202And204(t *testing.T) {
	for _, tc := range []struct {
		accepted bool
		want     string
	}{
		{accepted: false, want: "Agent `my-agent` deleted."},
		{accepted: true, want: "Deleting agent `my-agent`"},
	} {
		base := newStubClient()
		router, _, _ := newTestRouterWithHub(t, base)
		router.testClient = &deleteResultClient{stubClient: base, svc: &deleteResultAgentService{stubAgentService: base.agents, accepted: tc.accepted}}

		resp, err := router.executeDelete(context.Background(), testEvent(), "agent-123", "my-agent")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp == nil || resp.UpdateMessage == nil || !strings.Contains(resp.UpdateMessage.Text, tc.want) {
			t.Errorf("accepted=%v: want %q in %+v", tc.accepted, tc.want, resp)
		}
	}
}
