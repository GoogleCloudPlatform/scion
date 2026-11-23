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

package commands

import (
	"context"
	"errors"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"
)

// newAnalyticsHubIDResolver resolves hub_id for usage reporting through the
// credentialed hub client (GET /api/v1/agent/hub), falling back to the public
// /healthz endpoint only when the hub responds with 404 (an older hub that
// does not have the agent endpoint yet).
func newAnalyticsHubIDResolver(newClient func() *hub.Client) telemetry.HubIDResolver {
	return func(ctx context.Context) (string, error) {
		c := newClient()
		if c == nil || !c.IsConfigured() {
			return "", errors.New("hub client not configured")
		}
		id, err := c.GetHubID(ctx)
		if err == nil {
			return id, nil
		}
		if !errors.Is(err, hub.ErrHubInfoUnsupported) {
			return "", err
		}
		return telemetry.NewHealthzHubIDResolver(c.HubURL(), c.HTTPClient())(ctx)
	}
}
