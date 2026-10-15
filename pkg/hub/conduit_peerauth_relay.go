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

import "github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"

// The relay-peer authenticator lives in pkg/conduit/relay, so that a
// separate relay process builds it without this package. These names keep
// the hub's callers unchanged.

// DefaultConduitPeerAudience is relay.DefaultConduitPeerAudience.
const DefaultConduitPeerAudience = relay.DefaultConduitPeerAudience

// ConduitPeerAuthOptions is relay.ConduitPeerAuthOptions.
type ConduitPeerAuthOptions = relay.ConduitPeerAuthOptions

// ResolveConduitPeerAuthMode is relay.ResolveConduitPeerAuthMode.
func ResolveConduitPeerAuthMode(mode string, onGCP bool) (string, error) {
	return relay.ResolveConduitPeerAuthMode(mode, onGCP)
}

// NewConduitPeerAuth is relay.NewConduitPeerAuth.
func NewConduitPeerAuth(o ConduitPeerAuthOptions) (relay.PeerAuth, string, error) {
	return relay.NewConduitPeerAuth(o)
}
