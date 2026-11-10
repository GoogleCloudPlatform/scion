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

package store

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/stretchr/testify/assert"
)

// TestLaunchErrorCodesMatchAPI pins the store's launch error codes to the
// shared pkg/api codes and to their wire strings, which are stored and
// shown to clients and so must never change (design wave 3 E-a,
// ptone/scion#4670).
func TestLaunchErrorCodesMatchAPI(t *testing.T) {
	for _, tc := range []struct {
		store, api, wire string
	}{
		{LaunchErrorLaunchTimeout, api.LaunchCodeLaunchTimeout, "launch_timeout"},
		{LaunchErrorBrokerLost, api.LaunchCodeBrokerLost, "broker_lost"},
		{LaunchErrorLaunchStopped, api.LaunchCodeLaunchStopped, "launch_stopped"},
		{LaunchErrorAgentError, api.LaunchCodeAgentError, "agent_error"},
		{LaunchErrorHubUnreachable, api.LaunchCodeHubUnreachable, "hub_unreachable"},
		{LaunchErrorBrokerRestarted, api.LaunchCodeBrokerRestarted, "broker_restarted"},
		{LaunchErrorDispatchFailed, api.LaunchCodeDispatchFailed, "dispatch_failed"},
	} {
		assert.Equal(t, tc.api, tc.store, "store code must equal the api code")
		assert.Equal(t, tc.wire, tc.store, "store code must keep its wire string")
	}
}
