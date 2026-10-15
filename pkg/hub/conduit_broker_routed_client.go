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
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
)

// The routed branch of HybridBrokerClient's transient operations (see
// conduit_broker_routed.go). Each helper reports done=false when nothing
// was sent over conduit, and the caller then runs its existing path, which
// is the fallback only.

// SetConduitRouting routes the transient broker operations through t,
// with the existing control channel and HTTP paths as the fallback. The
// requests are built as on the control channel; t signs each attempt.
func (c *HybridBrokerClient) SetConduitRouting(t *routedBrokerTunnel) {
	if t == nil {
		c.routed = nil
		return
	}
	// The tunnel signs each attempt for its session; the client does not.
	c.routed = &ControlChannelBrokerClient{manager: t, debug: c.debug}
}

// routedFor reports whether a request to brokerID may go through the
// Router: routing is configured and enabled, and the broker is not a
// stateless co-located broker (always served by its local endpoint).
func (c *HybridBrokerClient) routedFor(brokerID string) bool {
	return c.routed != nil && !c.isStatelessLocalBroker(brokerID) && c.routed.manager.IsConnected(brokerID)
}

func (c *HybridBrokerClient) routedGetAgentLogs(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string, tail int) (string, bool, error) {
	if !c.routedFor(brokerID) {
		return "", false, nil
	}
	out, err := c.routed.GetAgentLogs(withBrokerRPC(ctx, conduit.RPCAgentLogs, true), brokerID, brokerEndpoint, agentID, projectID, tail)
	if routedBrokerFallback(err) {
		return "", false, nil
	}
	return out, true, err
}

func (c *HybridBrokerClient) routedExecAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string, command []string, timeout int) (string, int, bool, error) {
	if !c.routedFor(brokerID) {
		return "", 0, false, nil
	}
	out, code, err := c.routed.ExecAgent(withBrokerRPC(ctx, conduit.RPCAgentExec, false), brokerID, brokerEndpoint, agentID, projectID, command, timeout)
	if routedBrokerFallback(err) {
		return "", 0, false, nil
	}
	return out, code, true, err
}

// routedCleanupProject returns a broker failure (non-2xx) as an error; a
// 404 means there is nothing left to clean up.
func (c *HybridBrokerClient) routedCleanupProject(ctx context.Context, brokerID, brokerEndpoint, projectSlug, projectID string) (bool, error) {
	if !c.routedFor(brokerID) {
		return false, nil
	}
	err := c.routed.CleanupProject(withBrokerRPC(ctx, conduit.RPCProjectCleanup, true), brokerID, brokerEndpoint, projectSlug, projectID)
	if routedBrokerFallback(err) {
		return false, nil
	}
	if isBrokerStatus(err, http.StatusNotFound) {
		return true, nil
	}
	return true, err
}

func (c *HybridBrokerClient) routedImageStatus(ctx context.Context, brokerID, shortImage, longImage string) (*BrokerImageStatusResponse, bool, error) {
	if !c.routedFor(brokerID) {
		return nil, false, nil
	}
	resp, err := c.routed.ImageStatus(withBrokerRPC(ctx, conduit.RPCImageStatus, true), brokerID, shortImage, longImage)
	if routedBrokerFallback(err) {
		return nil, false, nil
	}
	return resp, true, err
}

func (c *HybridBrokerClient) routedPullImage(ctx context.Context, brokerID, image string) (bool, error) {
	if !c.routedFor(brokerID) {
		return false, nil
	}
	err := c.routed.PullImage(withBrokerRPC(ctx, conduit.RPCImagePull, true), brokerID, image)
	if routedBrokerFallback(err) {
		return false, nil
	}
	return true, err
}

func (c *HybridBrokerClient) routedDeleteImage(ctx context.Context, brokerID, image string) (bool, error) {
	if !c.routedFor(brokerID) {
		return false, nil
	}
	err := c.routed.DeleteImage(withBrokerRPC(ctx, conduit.RPCImageDelete, false), brokerID, image)
	if routedBrokerFallback(err) {
		return false, nil
	}
	return true, err
}
