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

package apiclient

import "net/http"

// Authenticator provides authentication credentials for API requests.
type Authenticator interface {
	// ApplyAuth adds authentication to the request (header, query param, etc.)
	ApplyAuth(req *http.Request) error

	// Refresh refreshes expired credentials if supported.
	// Returns false if refresh is not supported.
	Refresh() (bool, error)
}

// BearerAuth implements Bearer token authentication.
type BearerAuth struct {
	Token string
}

// ApplyAuth adds the Bearer token to the Authorization header.
func (a *BearerAuth) ApplyAuth(req *http.Request) error {
	if a.Token != "" {
		req.Header.Set("Authorization", "Bearer "+a.Token)
	}
	return nil
}

// Refresh indicates that refresh is not supported for static tokens.
func (a *BearerAuth) Refresh() (bool, error) { return false, nil }

// BrokerTokenAuth implements Runtime Broker token authentication.
type BrokerTokenAuth struct {
	Token string
}

// ApplyAuth adds the broker token to the X-Scion-Broker-Token header.
func (a *BrokerTokenAuth) ApplyAuth(req *http.Request) error {
	if a.Token != "" {
		req.Header.Set("X-Scion-Broker-Token", a.Token)
	}
	return nil
}

// Refresh indicates that refresh is not supported for broker tokens.
func (a *BrokerTokenAuth) Refresh() (bool, error) { return false, nil }

// AgentTokenAuth implements Agent token authentication.
type AgentTokenAuth struct {
	Token string
	// RunID is the agent's run id, sent as X-Scion-Run-Id when set.
	RunID string
}

// ApplyAuth adds the agent token to the X-Scion-Agent-Token header, and
// the run id to X-Scion-Run-Id.
func (a *AgentTokenAuth) ApplyAuth(req *http.Request) error {
	if a.Token != "" {
		req.Header.Set("X-Scion-Agent-Token", a.Token)
		if a.RunID != "" {
			req.Header.Set("X-Scion-Run-Id", a.RunID)
		}
	}
	return nil
}

// Refresh indicates that refresh is not supported for agent tokens.
func (a *AgentTokenAuth) Refresh() (bool, error) { return false, nil }
