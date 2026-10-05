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

package target

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

var t0 = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

type fixture struct {
	signer *grant.Signer
	keys   *KeyHolder
	other  *KeyHolder // a holder with an unrelated key
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ring := func() grant.KeyRing {
		k, err := grant.NewRingKey(t0.Add(-time.Hour), t0.Add(-time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		return grant.KeyRing{Keys: []grant.RingKey{k}}
	}
	r1, r2 := ring(), ring()
	s, err := r1.Signer(t0)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{signer: s, keys: &KeyHolder{}, other: &KeyHolder{}}
	// Round-trip through the Welcome encoding, as a target receives them.
	if err := f.keys.SetFromWelcome(&conduitv1.Welcome{GrantKeys: KeysToProto(r1.PublicKeys(t0))}); err != nil {
		t.Fatal(err)
	}
	if err := f.other.Set(r2.PublicKeys(t0)); err != nil {
		t.Fatal(err)
	}
	return f
}

const (
	agentID  = "agent-1"
	agentInc = "launch-1"
	project  = "proj-1"
	broker   = "broker-1"
	brokInc  = "bproc-1"
)

func agentClaims(epoch int64) grant.Claims {
	return grant.Claims{
		Issuer: "scion-hub", Subject: "user-1", ProjectID: project,
		Target:    grant.Target{Kind: grant.TargetKindAgent, ID: agentID, EndpointIncarnation: agentInc, SessionID: "sess-a", ConnectionEpoch: epoch},
		Stream:    grant.StreamHeader{Kind: grant.StreamKindTCP, Params: map[string]string{grant.ParamHost: "127.0.0.1", grant.ParamPort: "8080"}},
		NotBefore: t0, Expiry: t0.Add(30 * time.Second),
	}
}

func brokerClaims(agent string) grant.Claims {
	return grant.Claims{
		Issuer: "scion-hub", Subject: "user-1", ProjectID: project,
		Target:    grant.Target{Kind: grant.TargetKindBroker, ID: broker, EndpointIncarnation: brokInc, SessionID: "sess-b", ConnectionEpoch: 3},
		Stream:    grant.StreamHeader{Kind: grant.StreamKindPTY, Params: map[string]string{grant.ParamAgentID: agent}},
		NotBefore: t0, Expiry: t0.Add(30 * time.Second),
	}
}

func (f *fixture) open(t *testing.T, c grant.Claims, kind conduitv1.StreamKind, params map[string]string) *conduitv1.StreamOpen {
	t.Helper()
	tok, err := grant.Mint(f.signer, c)
	if err != nil {
		t.Fatal(err)
	}
	return &conduitv1.StreamOpen{Kind: kind, Params: params, Grant: tok}
}

func TestVerifyStreamOpen(t *testing.T) {
	agentIdent := Identity{Kind: grant.TargetKindAgent, ID: agentID, ProjectID: project, Incarnation: agentInc}
	brokerIdent := Identity{Kind: grant.TargetKindBroker, ID: broker, Incarnation: brokInc}
	tcp := map[string]string{grant.ParamPort: "8080", grant.ParamHost: "127.0.0.1"}
	for _, tc := range []struct {
		name    string
		ident   Identity
		claims  grant.Claims
		kind    conduitv1.StreamKind
		params  map[string]string
		binding Binding
		acting  Acting
		other   bool // verify with an unrelated key set
		want    error
	}{
		{name: "agent ok", ident: agentIdent, claims: agentClaims(5), kind: conduitv1.StreamKind_STREAM_KIND_TCP, params: tcp, binding: Binding{"sess-a", 5}},
		{name: "agent wrong epoch", ident: agentIdent, claims: agentClaims(4), kind: conduitv1.StreamKind_STREAM_KIND_TCP, params: tcp, binding: Binding{"sess-a", 5}, want: grant.ErrTarget},
		{name: "agent wrong session", ident: agentIdent, claims: agentClaims(5), kind: conduitv1.StreamKind_STREAM_KIND_TCP, params: tcp, binding: Binding{"sess-x", 5}, want: grant.ErrTarget},
		{name: "agent old incarnation", ident: Identity{Kind: grant.TargetKindAgent, ID: agentID, ProjectID: project, Incarnation: "launch-2"}, claims: agentClaims(5), kind: conduitv1.StreamKind_STREAM_KIND_TCP, params: tcp, binding: Binding{"sess-a", 5}, want: grant.ErrTarget},
		{name: "agent unknown kid", ident: agentIdent, claims: agentClaims(5), kind: conduitv1.StreamKind_STREAM_KIND_TCP, params: tcp, binding: Binding{"sess-a", 5}, other: true, want: grant.ErrUnknownKey},
		{name: "agent params differ", ident: agentIdent, claims: agentClaims(5), kind: conduitv1.StreamKind_STREAM_KIND_TCP, params: map[string]string{grant.ParamHost: "127.0.0.1", grant.ParamPort: "22"}, binding: Binding{"sess-a", 5}, want: grant.ErrStream},
		{name: "agent kind differs", ident: agentIdent, claims: agentClaims(5), kind: conduitv1.StreamKind_STREAM_KIND_PTY, params: tcp, binding: Binding{"sess-a", 5}, want: grant.ErrStream},
		{name: "agent foreign agent_id param", ident: agentIdent, claims: agentClaims(5), kind: conduitv1.StreamKind_STREAM_KIND_TCP, params: map[string]string{grant.ParamAgentID: "agent-2"}, binding: Binding{"sess-a", 5}, want: ErrAgentMismatch},
		{name: "broker ok", ident: brokerIdent, claims: brokerClaims(agentID), kind: conduitv1.StreamKind_STREAM_KIND_PTY, params: map[string]string{grant.ParamAgentID: agentID}, binding: Binding{"sess-b", 3}, acting: Acting{agentID, project}},
		{name: "broker acting on another agent", ident: brokerIdent, claims: brokerClaims(agentID), kind: conduitv1.StreamKind_STREAM_KIND_PTY, params: map[string]string{grant.ParamAgentID: agentID}, binding: Binding{"sess-b", 3}, acting: Acting{"agent-2", project}, want: ErrAgentMismatch},
		{name: "broker open re-aimed at another agent", ident: brokerIdent, claims: brokerClaims(agentID), kind: conduitv1.StreamKind_STREAM_KIND_PTY, params: map[string]string{grant.ParamAgentID: "agent-2"}, binding: Binding{"sess-b", 3}, acting: Acting{"agent-2", project}, want: grant.ErrStream},
		{name: "broker without acting agent", ident: brokerIdent, claims: brokerClaims(agentID), kind: conduitv1.StreamKind_STREAM_KIND_PTY, params: map[string]string{grant.ParamAgentID: agentID}, binding: Binding{"sess-b", 3}, want: ErrAgentMismatch},
		{name: "broker grant without agent_id", ident: brokerIdent, claims: brokerClaims(""), kind: conduitv1.StreamKind_STREAM_KIND_PTY, params: map[string]string{grant.ParamAgentID: ""}, binding: Binding{"sess-b", 3}, acting: Acting{agentID, project}, want: ErrAgentMismatch},
		{name: "broker wrong project", ident: brokerIdent, claims: brokerClaims(agentID), kind: conduitv1.StreamKind_STREAM_KIND_PTY, params: map[string]string{grant.ParamAgentID: agentID}, binding: Binding{"sess-b", 3}, acting: Acting{agentID, "proj-2"}, want: grant.ErrProject},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			keys := f.keys
			if tc.other {
				keys = f.other
			}
			replay := grant.NewMemoryReplayCache(func() time.Time { return t0 }, 0)
			v := &Verifier{Identity: tc.ident, Keys: keys, Replay: replay, Issuer: "scion-hub", Now: func() time.Time { return t0 }}
			open := f.open(t, tc.claims, tc.kind, tc.params)
			_, err := v.VerifyStreamOpen(context.Background(), tc.binding, open, tc.acting)
			if !errors.Is(err, tc.want) || (tc.want == nil) != (err == nil) {
				t.Fatalf("VerifyStreamOpen = %v, want %v", err, tc.want)
			}
			if tc.want != nil && replay.Len() != 0 {
				t.Fatalf("a refused grant consumed its jti (replay cache len %d)", replay.Len())
			}
			if tc.want == nil {
				if _, err := v.VerifyStreamOpen(context.Background(), tc.binding, open, tc.acting); !errors.Is(err, grant.ErrReplay) {
					t.Fatalf("second use = %v, want ErrReplay", err)
				}
			}
		})
	}
}

// TestVerifyStreamOpenAgentIDMismatchKeepsJTI: the broker-side agent_id
// refusal happens before Verify, so the same grant still works for the
// agent it names.
func TestVerifyStreamOpenAgentIDMismatchKeepsJTI(t *testing.T) {
	f := newFixture(t)
	replay := grant.NewMemoryReplayCache(func() time.Time { return t0 }, 0)
	v := &Verifier{Identity: Identity{Kind: grant.TargetKindBroker, ID: broker, Incarnation: brokInc}, Keys: f.keys, Replay: replay, Now: func() time.Time { return t0 }}
	open := f.open(t, brokerClaims(agentID), conduitv1.StreamKind_STREAM_KIND_PTY, map[string]string{grant.ParamAgentID: agentID})
	b := Binding{"sess-b", 3}
	if _, err := v.VerifyStreamOpen(context.Background(), b, open, Acting{"agent-2", project}); !errors.Is(err, ErrAgentMismatch) {
		t.Fatalf("mismatch = %v", err)
	}
	if _, err := v.VerifyStreamOpen(context.Background(), b, open, Acting{agentID, project}); err != nil {
		t.Fatalf("matching agent after refusal = %v", err)
	}
}

func TestKeyHolder(t *testing.T) {
	f := newFixture(t)
	var h KeyHolder
	if _, ok := h.Lookup(f.signer.KeyID); ok {
		t.Fatal("empty holder resolved a kid")
	}
	v := &Verifier{Identity: Identity{Kind: grant.TargetKindAgent, ID: agentID, ProjectID: project, Incarnation: agentInc}, Keys: &h,
		Replay: grant.NewMemoryReplayCache(func() time.Time { return t0 }, 0), Now: func() time.Time { return t0 }}
	open := f.open(t, agentClaims(5), conduitv1.StreamKind_STREAM_KIND_TCP, map[string]string{grant.ParamHost: "127.0.0.1", grant.ParamPort: "8080"})
	if _, err := v.VerifyStreamOpen(context.Background(), Binding{"sess-a", 5}, open, Acting{}); !errors.Is(err, grant.ErrUnknownKey) {
		t.Fatalf("no keys = %v, want ErrUnknownKey (fail closed)", err)
	}
	// An invalid replacement is refused and the previous set kept.
	k, _ := f.keys.Lookup(f.signer.KeyID)
	if err := h.Set([]grant.PublicKey{k}); err != nil {
		t.Fatal(err)
	}
	if err := h.Set([]grant.PublicKey{{KeyID: "bad", Key: []byte{1}}}); err == nil {
		t.Fatal("invalid key accepted")
	}
	if _, ok := h.Lookup(f.signer.KeyID); !ok {
		t.Fatal("previous key set lost after a refused update")
	}
	// Replacing drops keys the hub no longer publishes.
	if err := h.Set(nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.Lookup(f.signer.KeyID); ok {
		t.Fatal("key survived replacement")
	}
}

// TestFallbackAdmittedTargetVerifiesGenGrant: a target that presented no
// launch id is admitted as "gen-<N>" and its grants are minted against
// that value. Its identity must come from the Welcome (the admitted
// value); the presented "" is an incomplete expectation and refuses every
// grant.
func TestFallbackAdmittedTargetVerifiesGenGrant(t *testing.T) {
	welcome := &conduitv1.Welcome{SessionId: "sess-a", ConnectionEpoch: 5, EndpointIncarnation: "gen-7"}
	claims := agentClaims(5)
	claims.Target.EndpointIncarnation = "gen-7"
	tcp := map[string]string{grant.ParamPort: "8080", grant.ParamHost: "127.0.0.1"}
	for _, tc := range []struct {
		name  string
		ident Identity
		want  error
	}{
		{name: "identity from welcome", ident: IdentityFromWelcome(grant.TargetKindAgent, agentID, project, welcome)},
		{name: "presented value (empty)", ident: Identity{Kind: grant.TargetKindAgent, ID: agentID, ProjectID: project, Incarnation: ""}, want: grant.ErrExpectation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			replay := grant.NewMemoryReplayCache(func() time.Time { return t0 }, 0)
			v := &Verifier{Identity: tc.ident, Keys: f.keys, Replay: replay, Issuer: "scion-hub", Now: func() time.Time { return t0 }}
			_, err := v.VerifyStreamOpen(context.Background(), BindingFromWelcome(welcome), f.open(t, claims, conduitv1.StreamKind_STREAM_KIND_TCP, tcp), Acting{})
			if !errors.Is(err, tc.want) || (tc.want == nil) != (err == nil) {
				t.Fatalf("VerifyStreamOpen = %v, want %v", err, tc.want)
			}
		})
	}
}
