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
)

// envGatherMockBrokerClient extends mockRuntimeBrokerClient with env-gather methods.
type envGatherMockBrokerClient struct {
	mockRuntimeBrokerClient

	// Env-gather fields
	createWithGatherCalled bool
	gatherReturnEnvReqs    *RemoteEnvRequirementsResponse
}

func (m *envGatherMockBrokerClient) CreateAgentWithGather(ctx context.Context, brokerID, brokerEndpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
	m.createWithGatherCalled = true
	m.lastBrokerID = brokerID
	m.lastEndpoint = brokerEndpoint
	m.lastCreateReq = req
	if m.returnErr != nil {
		return nil, nil, m.returnErr
	}
	if m.gatherReturnEnvReqs != nil {
		return nil, m.gatherReturnEnvReqs, nil
	}
	// All env satisfied
	return &RemoteAgentResponse{
		Agent: &RemoteAgentInfo{
			ID:     req.ID,
			Slug:   req.Slug,
			Name:   req.Name,
			Status: "running",
		},
		Created: true,
	}, nil, nil
}
