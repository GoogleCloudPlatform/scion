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
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

const brokerRuntimeUnavailableBody = `{"error":{"code":"runtime_unavailable","message":"runtime \"kubernetes\" is not available on this broker; retry later or check the broker's runtime configuration"}}`

func runtimeUnavailableErr() error {
	return &brokerStatusError{StatusCode: http.StatusServiceUnavailable, Body: brokerRuntimeUnavailableBody, RetryAfter: "30"}
}

// startErrClient is mockRuntimeBrokerClient with StartAgent failing and
// every other call succeeding.
type startErrClient struct {
	*mockRuntimeBrokerClient
	startErr error
}

func (c startErrClient) StartAgent(context.Context, string, string, string, string, string, string, string, string, string, string, map[string]string, []ResolvedSecret, *api.ScionConfig, []api.SharedDir, bool, bool, StartExtras) (*RemoteAgentResponse, error) {
	c.startCalled = true
	return nil, c.startErr
}
