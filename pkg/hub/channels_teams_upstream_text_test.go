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
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/stretchr/testify/assert"
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

// Every failed Teams delivery reaches the server log through
// ChannelRegistry.Dispatch exactly once, with the upstream text rendered:
// the token endpoint body, the Bot Connector API error body, and a transport
// error (ptone/scion#7498).
//
// Must not run in parallel: it swaps the default logger.
func TestTeamsDeliveryFailure_LoggedOnceRendered(t *testing.T) {
	tests := []struct {
		name  string
		setup func(ch *TeamsChannel)
	}{
		{name: "token endpoint error", setup: func(ch *TeamsChannel) {
			ch.tokenProvider.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return httpJSONResponse(http.StatusUnauthorized, upstreamMarkerBody), nil
			})}
		}},
		{name: "API error body", setup: func(ch *TeamsChannel) {
			ch.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return httpJSONResponse(http.StatusForbidden, upstreamMarkerBody), nil
			})}
		}},
		{name: "transport error", setup: func(ch *TeamsChannel) {
			ch.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, errors.New(upstreamMarkerBody)
			})}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ch := newTestTeamsChannel(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusCreated)
			}))
			tc.setup(ch)
			logs := captureUpstreamLog(t)
			reg := &ChannelRegistry{
				entries: []channelEntry{{channel: ch, config: ChannelConfig{Type: "teams"}}},
				log:     slog.Default(),
			}

			reg.Dispatch(context.Background(), &messages.StructuredMessage{
				Version: messages.Version,
				Sender:  "test",
				Msg:     "test message",
				Type:    messages.TypeStateChange,
			})

			assertLoggedOnceRendered(t, logs, "Notification channel delivery failed")
			assert.Equal(t, 1, strings.Count(logs.String(), upstreamMarker), "the marker reaches exactly one log record")
		})
	}
}
