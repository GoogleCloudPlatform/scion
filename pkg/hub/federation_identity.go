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

// FederatedAgentIdentity represents an agent authenticated via OIDC from a
// trusted external issuer. It implements the AgentIdentity interface.
type FederatedAgentIdentity struct {
	issuerURL string
	agentID   string
	projectID string
	agentName string
	ancestry  []string
	rootUser  string
	scopes    []AgentTokenScope
}

// NewFederatedAgentIdentity constructs a new FederatedAgentIdentity.
func NewFederatedAgentIdentity(issuerURL, agentID, projectID, agentName, rootUser string,
	ancestry []string, scopes []AgentTokenScope) *FederatedAgentIdentity {
	return &FederatedAgentIdentity{
		issuerURL: issuerURL,
		agentID:   agentID,
		projectID: projectID,
		agentName: agentName,
		ancestry:  ancestry,
		rootUser:  rootUser,
		scopes:    scopes,
	}
}

// ID returns the unique identifier for this federated agent, combining the
// issuer URL and remote agent ID.
func (f *FederatedAgentIdentity) ID() string { return f.issuerURL + ":" + f.agentID }

// Type returns the identity type ("federated_agent").
func (f *FederatedAgentIdentity) Type() string { return "federated_agent" }

// ProjectID returns an empty string. Federated agents are not bound to a local
// project; use RemoteProjectID() to get the project on the originating hub.
func (f *FederatedAgentIdentity) ProjectID() string { return "" }

// Scopes returns the scopes granted to this federated agent.
func (f *FederatedAgentIdentity) Scopes() []AgentTokenScope { return f.scopes }

// HasScope reports whether the federated agent has been granted the given scope.
func (f *FederatedAgentIdentity) HasScope(scope AgentTokenScope) bool {
	for _, s := range f.scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// Ancestry returns the ordered ancestor chain from the OIDC token claims.
func (f *FederatedAgentIdentity) Ancestry() []string { return f.ancestry }

// OriginUserID returns the root user who originated the agent chain on the
// remote hub.
func (f *FederatedAgentIdentity) OriginUserID() string { return f.rootUser }

// TokenID returns an empty string for federated agents (they use OIDC tokens,
// not locally-issued JWTs).
func (f *FederatedAgentIdentity) TokenID() string { return "" }

// IssuerURL returns the OIDC issuer URL of the trusted external issuer.
func (f *FederatedAgentIdentity) IssuerURL() string { return f.issuerURL }

// RemoteAgentID returns the agent ID on the originating hub.
func (f *FederatedAgentIdentity) RemoteAgentID() string { return f.agentID }

// RemoteProjectID returns the project ID on the originating hub.
func (f *FederatedAgentIdentity) RemoteProjectID() string { return f.projectID }

// AgentName returns the human-readable name of the agent on the originating hub.
func (f *FederatedAgentIdentity) AgentName() string { return f.agentName }
