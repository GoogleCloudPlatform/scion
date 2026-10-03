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

package substrate

import (
	"context"
	"io"
	"net/http"
	"strings"
)

// TargetActorHeader is the header Substrate's inbound atenet-router uses to
// address a specific actor (substrate-runtime.md §1).
const TargetActorHeader = "ate-target-actor"

// RouterClient sends HTTP requests to an actor's in-actor control server
// (sciontool substrate-serve) through Substrate's inbound atenet-router.
// An inbound request through the router auto-resumes a SUSPENDED actor —
// callers that must not do that (e.g. a health probe against an actor the
// hub believes is suspended) should check the actor's state via the ateapi
// Control client first rather than call through RouterClient.
type RouterClient struct {
	// Endpoint is the router's base URL, e.g.
	// "http://atenet-router.ate-system.svc:80".
	Endpoint string
	// HTTPClient performs the requests. Defaults to a client with no
	// blanket timeout when nil (set by NewRouterClient): callers set an
	// appropriate deadline on the context passed to Do instead, because a
	// single fixed timeout can't fit every call this client makes — an
	// exec's timeout_s is caller-chosen and can legitimately exceed a
	// short healthz/bootstrap deadline (see pkg/runtime's doExec, which
	// derives its context deadline from timeout_s; a flat 30s client
	// timeout would cut off exec calls whose timeout_s was 60s).
	HTTPClient *http.Client
}

// NewRouterClient builds a RouterClient targeting endpoint.
func NewRouterClient(endpoint string) *RouterClient {
	return &RouterClient{
		Endpoint:   strings.TrimRight(endpoint, "/"),
		HTTPClient: &http.Client{},
	}
}

// Do sends method+path to the actor identified by atespace/actor, through
// the router, with the given body and extra headers (may be nil). The
// caller is responsible for closing the returned response body.
func (c *RouterClient) Do(ctx context.Context, atespace, actor, method, path string, body io.Reader, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.Endpoint+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set(TargetActorHeader, atespace+"/"+actor)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	return client.Do(req)
}
