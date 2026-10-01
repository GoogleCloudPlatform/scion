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

// Package hub provides the Scion Hub API server.
package hub

import (
	"context"
)

// BrokerIdentity represents an authenticated Runtime Broker.
type BrokerIdentity interface {
	Identity
	BrokerID() string
}

// brokerIdentityImpl implements BrokerIdentity.
//
// INTENTIONAL DESIGN: brokerIdentityImpl must not implement UserIdentity
// or AgentIdentity. See TestBrokerIdentityImpl_MustNotSatisfyUserIdentity
// and TestBrokerIdentityImpl_MustNotSatisfyAgentIdentity (DEF-58).
//
// Broker-relayed messages carry a SenderID that names the upstream
// principal, NOT the broker itself. The SenderID == "" guard in
// messagebroker.go (deliverToAgent, fanOutToProject, fanOutGlobal)
// intentionally skips DM-conversation resolution and self-skip logic
// when SenderID is empty, because an empty SenderID means the upstream
// sender is unknown or not a locally-authenticated principal. Without
// that guard, empty-SenderID messages would either derive a DM key
// from a zero-value participant (creating ghost conversations) or
// fail to self-skip (delivering the sender its own broadcast).
// This comparison is tested by TestEmptySenderID_DeliverToUser_SkipsDMResolution
// and TestEmptySenderID_DeliverToAgent_SkipsDMResolution in messagebroker_test.go.
type brokerIdentityImpl struct {
	brokerID string
}

// ID returns the broker ID.
func (h *brokerIdentityImpl) ID() string { return h.brokerID }

// Type returns the identity type ("broker").
func (h *brokerIdentityImpl) Type() string { return "broker" }

// BrokerID returns the broker ID.
func (h *brokerIdentityImpl) BrokerID() string { return h.brokerID }

// NewBrokerIdentity creates a new BrokerIdentity.
func NewBrokerIdentity(brokerID string) BrokerIdentity {
	return &brokerIdentityImpl{brokerID: brokerID}
}

// brokerIdentityContextKey is the context key for BrokerIdentity.
type brokerIdentityContextKey struct{}

// GetBrokerIdentityFromContext returns the BrokerIdentity from the context, if
// present. The type assertions below report ok=true for a stored value whose
// concrete type implements BrokerIdentity even when that concrete pointer is
// nil (a typed nil), because a type assertion checks the dynamic type, not
// nilness. BrokerIdentity embeds Identity, so isNilIdentity applies directly:
// each branch treats that case the same as nothing being stored, so callers
// comparing the result with == nil see a real nil interface instead of a
// non-nil interface wrapping a nil pointer.
func GetBrokerIdentityFromContext(ctx context.Context) BrokerIdentity {
	if identity, ok := ctx.Value(brokerIdentityContextKey{}).(BrokerIdentity); ok && !isNilIdentity(identity) {
		return identity
	}
	// Also check the generic identity key
	if identity, ok := ctx.Value(identityContextKey{}).(BrokerIdentity); ok && !isNilIdentity(identity) {
		return identity
	}
	return nil
}

// contextWithBrokerIdentity returns a new context with the BrokerIdentity set.
func contextWithBrokerIdentity(ctx context.Context, broker BrokerIdentity) context.Context {
	return context.WithValue(ctx, brokerIdentityContextKey{}, broker)
}
