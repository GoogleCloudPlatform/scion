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
	"testing"
)

// A failed Teams token request carries the token endpoint's body in its
// error only as upstreamText renders it. The channel registry logs that
// error once per failed delivery (ChannelRegistry.Dispatch).
func TestTeamsTokenEndpointErrorBodyRendered(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return httpJSONResponse(http.StatusUnauthorized, upstreamMarkerBody), nil
	})}
	tp := newTeamsTokenProvider("app-id", "app-secret", "tenant-id", client)

	_, err := tp.getToken(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	assertUpstreamTextRendered(t, err.Error())
}
