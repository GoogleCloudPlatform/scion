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

package hubclient

import (
	"context"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// ExecContainerAgent holds the agent metadata returned by
// GET/PUT /api/v1/users/me/exec-container when the container is available.
type ExecContainerAgent struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	ProjectID   string `json:"projectId"`
	ProjectName string `json:"projectName"`
	Phase       string `json:"phase"`
	Activity    string `json:"activity"`
}

// ExecContainerStatus is the response shape of
// GET/PUT /api/v1/users/me/exec-container.
type ExecContainerStatus struct {
	Configured bool                `json:"configured"`
	State      string              `json:"state,omitempty"`
	Agent      *ExecContainerAgent `json:"agent,omitempty"`
	Reason     string              `json:"reason,omitempty"`
}

type setExecContainerRequest struct {
	AgentID string `json:"agentId"`
}

// GetExecContainer returns the caller's registered execution container status.
func (s *userService) GetExecContainer(ctx context.Context) (*ExecContainerStatus, error) {
	resp, err := s.c.get(ctx, "/api/v1/users/me/exec-container", nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[ExecContainerStatus](resp)
}

// SetExecContainer registers agentID as the caller's default execution container.
func (s *userService) SetExecContainer(ctx context.Context, agentID string) (*ExecContainerStatus, error) {
	resp, err := s.c.put(ctx, "/api/v1/users/me/exec-container", setExecContainerRequest{AgentID: agentID}, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[ExecContainerStatus](resp)
}

// ClearExecContainer clears the caller's execution container registration.
func (s *userService) ClearExecContainer(ctx context.Context) error {
	resp, err := s.c.delete(ctx, "/api/v1/users/me/exec-container", nil)
	if err != nil {
		return err
	}
	return apiclient.CheckResponse(resp)
}
