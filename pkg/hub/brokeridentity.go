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

// GetBrokerIdentityFromContext returns the BrokerIdentity from the context, if present.
func GetBrokerIdentityFromContext(ctx context.Context) BrokerIdentity {
	if identity, ok := ctx.Value(brokerIdentityContextKey{}).(BrokerIdentity); ok {
		return identity
	}
	// Also check the generic identity key
	if identity, ok := ctx.Value(identityContextKey{}).(BrokerIdentity); ok {
		return identity
	}
	return nil
}

// contextWithBrokerIdentity returns a new context with the BrokerIdentity set.
func contextWithBrokerIdentity(ctx context.Context, broker BrokerIdentity) context.Context {
	return context.WithValue(ctx, brokerIdentityContextKey{}, broker)
}
