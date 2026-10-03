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

// Package target holds the target-side grant check for StreamOpen (design
// §3.5, contracts §3): the target (agent or broker) verifies the grant in
// every StreamOpen against what it knows independently, namely its own
// identity and incarnation, the session id and epoch from its current
// Welcome, its project, and the StreamOpen kind and params, and only then
// performs the side effect. Keys come from Welcome.grant_keys and the
// token-refresh response.
package target

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// ErrAgentMismatch refuses a broker-target grant whose signed agent_id is
// absent or differs from the agent the broker is actually acting on.
var ErrAgentMismatch = errors.New("conduit target: grant agent_id does not match the agent acted on")

// ErrNoKeys means no Welcome has delivered grant keys yet.
var ErrNoKeys = errors.New("conduit target: no grant keys")

// KeysFromProto converts Welcome.grant_keys to grant public keys.
func KeysFromProto(keys []*conduitv1.GrantKey) []grant.PublicKey {
	out := make([]grant.PublicKey, 0, len(keys))
	for _, k := range keys {
		pk := grant.PublicKey{KeyID: k.GetKid(), Key: append([]byte(nil), k.GetPublicKey()...)}
		if n := k.GetNotAfterUnix(); n != 0 {
			pk.NotAfter = time.Unix(n, 0)
		}
		out = append(out, pk)
	}
	return out
}

// KeysToProto converts grant public keys to Welcome.grant_keys.
func KeysToProto(keys []grant.PublicKey) []*conduitv1.GrantKey {
	out := make([]*conduitv1.GrantKey, 0, len(keys))
	for _, k := range keys {
		gk := &conduitv1.GrantKey{Kid: k.KeyID, PublicKey: append([]byte(nil), k.Key...)}
		if !k.NotAfter.IsZero() {
			gk.NotAfterUnix = k.NotAfter.Unix()
		}
		out = append(out, gk)
	}
	return out
}

// KeyHolder is the target's current KeySet, replaced atomically on every
// Welcome and token refresh (never merged: a key the hub stopped
// publishing is no longer trusted).
type KeyHolder struct{ p atomic.Pointer[grant.KeySet] }

// Set replaces the key set. An invalid set is refused and the previous one
// kept.
func (h *KeyHolder) Set(keys []grant.PublicKey) error {
	ks, err := grant.NewKeySet(keys...)
	if err != nil {
		return err
	}
	h.p.Store(&ks)
	return nil
}

// SetFromWelcome replaces the key set from a Welcome.
func (h *KeyHolder) SetFromWelcome(w *conduitv1.Welcome) error {
	return h.Set(KeysFromProto(w.GetGrantKeys()))
}

// Lookup implements grant.KeySet. Without keys every kid is unknown, so
// Verify fails closed.
func (h *KeyHolder) Lookup(kid string) (grant.PublicKey, bool) {
	ks := h.p.Load()
	if ks == nil {
		return grant.PublicKey{}, false
	}
	return (*ks).Lookup(kid)
}

// Identity is what the target knows about itself.
type Identity struct {
	Kind        string // grant.TargetKindAgent | grant.TargetKindBroker
	ID          string
	ProjectID   string // agents: the agent's project; brokers: unused (see Acting)
	Incarnation string // the endpoint_incarnation the target presented
}

// Acting names the agent a broker target is about to act on, as the broker
// itself resolved it (not as the StreamOpen claims). Agent targets pass
// the zero value.
type Acting struct {
	AgentID   string
	ProjectID string
}

// Binding is the session the StreamOpen arrived on, from its Welcome.
type Binding struct {
	SessionID       string
	ConnectionEpoch int64
}

// BindingFromWelcome extracts the binding.
func BindingFromWelcome(w *conduitv1.Welcome) Binding {
	return Binding{SessionID: w.GetSessionId(), ConnectionEpoch: w.GetConnectionEpoch()}
}

// Verifier verifies StreamOpen grants for one target.
type Verifier struct {
	Identity  Identity
	Keys      *KeyHolder
	Replay    grant.ReplayCache
	Issuer    string        // optional: required iss
	ClockSkew time.Duration // capped by grant.MaxClockSkew
	Now       func() time.Time
}

// VerifyStreamOpen verifies open.grant for a stream about to be accepted on
// the session b. actingAgentID is the agent the target will act on:
//
//   - broker targets must pass it (the agent whose container the stream
//     enters, as the broker resolved it) and the grant's signed
//     agent_id param must equal it; an empty value or a mismatch is
//     refused with ErrAgentMismatch.
//   - agent targets act only on themselves; actingAgentID is ignored, and
//     a signed agent_id param, if present, must equal the target's own id.
//
// The jti is consumed only when every check passes. Perform the side effect
// only after a nil error.
func (v *Verifier) VerifyStreamOpen(ctx context.Context, b Binding, open *conduitv1.StreamOpen, acting Acting) (*grant.Claims, error) {
	if v.Keys == nil {
		return nil, ErrNoKeys
	}
	kind, err := conduit.StreamKindFromProto(open.GetKind())
	if err != nil {
		return nil, fmt.Errorf("%w: %w", grant.ErrStream, err)
	}
	// The agent_id binding is checked on the StreamOpen params before
	// Verify: Verify then proves the params equal the signed ones, so a
	// refusal here never burns the jti.
	params := open.GetParams()
	projectID := v.Identity.ProjectID
	switch v.Identity.Kind {
	case grant.TargetKindBroker:
		if acting.AgentID == "" || params[grant.ParamAgentID] != acting.AgentID {
			return nil, ErrAgentMismatch
		}
		projectID = acting.ProjectID
	case grant.TargetKindAgent:
		if a, ok := params[grant.ParamAgentID]; ok && a != v.Identity.ID {
			return nil, ErrAgentMismatch
		}
	default:
		return nil, fmt.Errorf("%w: target kind %q", grant.ErrExpectation, v.Identity.Kind)
	}
	now := time.Now
	if v.Now != nil {
		now = v.Now
	}
	expect := grant.Expectation{
		Target: grant.Target{
			Kind:                v.Identity.Kind,
			ID:                  v.Identity.ID,
			EndpointIncarnation: v.Identity.Incarnation,
			SessionID:           b.SessionID,
			ConnectionEpoch:     b.ConnectionEpoch,
		},
		Header:    grant.StreamHeader{Kind: string(kind), Params: params},
		ProjectID: projectID,
		Issuer:    v.Issuer,
		ClockSkew: v.ClockSkew,
	}
	return grant.Verify(ctx, open.GetGrant(), v.Keys, expect, v.Replay, now())
}
